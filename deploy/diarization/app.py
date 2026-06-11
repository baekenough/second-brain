"""
Speaker diarization microservice wrapping pyannote.audio.

Privacy contract: ALL audio processing is local. No audio data leaves this
process. The ONLY allowed network egress is the one-time HuggingFace model
download (gated models pyannote/speaker-diarization-3.1 +
pyannote/segmentation-3.0), controlled by HF_TOKEN env var.
"""

from __future__ import annotations

import logging
import os
import tempfile
from contextlib import asynccontextmanager
from typing import Optional

import anyio
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse
from pydantic import BaseModel

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
)
logger = logging.getLogger("diarization")

# ---------------------------------------------------------------------------
# Global model state (loaded once at startup)
# ---------------------------------------------------------------------------

_pipeline = None  # pyannote.audio Pipeline instance
_model_loaded: bool = False
_model_error: Optional[str] = None


def _load_pipeline() -> None:
    """Load pyannote speaker-diarization pipeline.

    Called once in a background thread at startup so the event loop is not
    blocked. Sets module-level _pipeline / _model_loaded / _model_error.
    """
    global _pipeline, _model_loaded, _model_error

    hf_token = os.environ.get("HF_TOKEN")
    if not hf_token:
        _model_error = (
            "HF_TOKEN env var not set — model not loaded. "
            "Set HF_TOKEN and restart to enable /diarize."
        )
        logger.warning(_model_error)
        return

    device = os.environ.get("DIARIZATION_DEVICE", "cpu")

    try:
        # Deferred import so the service can start even without pyannote
        # installed (useful for smoke-testing the HTTP surface).
        from pyannote.audio import Pipeline  # type: ignore
        import torch  # type: ignore

        logger.info("Loading pyannote/speaker-diarization-3.1 …")
        pipeline = Pipeline.from_pretrained(
            "pyannote/speaker-diarization-3.1",
            use_auth_token=hf_token,
        )

        # Move to target device
        torch_device = torch.device(device)
        pipeline.to(torch_device)
        logger.info("Pipeline loaded on device=%s", device)

        _pipeline = pipeline
        _model_loaded = True
    except Exception as exc:  # noqa: BLE001
        _model_error = f"Failed to load pipeline: {exc}"
        logger.exception("Pipeline load failed")


# ---------------------------------------------------------------------------
# Lifespan: load model in a thread at startup
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Run model loading in a threadpool so startup is non-blocking."""
    logger.info("Service starting — loading diarization model …")
    await anyio.to_thread.run_sync(_load_pipeline)
    if _model_loaded:
        logger.info("Model ready — service is healthy.")
    else:
        logger.warning("Model NOT loaded: %s", _model_error)
    yield
    logger.info("Service shutting down.")


# ---------------------------------------------------------------------------
# FastAPI application
# ---------------------------------------------------------------------------

app = FastAPI(
    title="Speaker Diarization Service",
    description=(
        "Wraps pyannote.audio to provide speaker segments for mono audio. "
        "Local-only processing — no audio data is sent to external services."
    ),
    version="1.0.0",
    lifespan=lifespan,
)

# ---------------------------------------------------------------------------
# Response schemas
# ---------------------------------------------------------------------------


class HealthResponse(BaseModel):
    status: str
    model_loaded: bool


class Segment(BaseModel):
    start: float
    end: float
    speaker: str


class DiarizeResponse(BaseModel):
    segments: list[Segment]


# ---------------------------------------------------------------------------
# Routes
# ---------------------------------------------------------------------------


@app.get(
    "/health",
    response_model=HealthResponse,
    summary="Health check",
    tags=["ops"],
)
async def health() -> HealthResponse:
    """Return service health and model-loaded status."""
    return HealthResponse(status="ok", model_loaded=_model_loaded)


def _run_diarization(audio_bytes: bytes, num_speakers: Optional[int]) -> list[dict]:
    """CPU-bound diarization — executed in a threadpool by the caller.

    Writes audio to a temp file, runs pyannote pipeline, converts annotation
    to a sorted list of segment dicts, and deletes the temp file.

    Args:
        audio_bytes: Raw audio bytes (any format ffmpeg can decode).
        num_speakers: Exact speaker count hint (None = auto-detect).

    Returns:
        List of dicts with keys start, end, speaker — sorted by start time.

    Raises:
        RuntimeError: If the pipeline fails (bad audio, model error, etc.).
    """
    tmp_path: Optional[str] = None
    try:
        # Write to a named temp file so pyannote (and ffmpeg under the hood)
        # can open it by path.
        with tempfile.NamedTemporaryFile(
            suffix=".audio", delete=False
        ) as tmp:
            tmp.write(audio_bytes)
            tmp_path = tmp.name

        logger.info(
            "Running diarization on %d bytes (num_speakers=%s)",
            len(audio_bytes),
            num_speakers,
        )

        # Build kwargs — only pass num_speakers when explicitly provided.
        kwargs: dict = {}
        if num_speakers is not None:
            kwargs["num_speakers"] = num_speakers

        diarization = _pipeline(tmp_path, **kwargs)

        segments: list[dict] = []
        for turn, _, speaker in diarization.itertracks(yield_label=True):
            segments.append(
                {
                    "start": round(turn.start, 3),
                    "end": round(turn.end, 3),
                    "speaker": speaker,
                }
            )

        # Sort by start time (pyannote usually returns sorted output, but
        # make this explicit for contract stability).
        segments.sort(key=lambda s: s["start"])

        logger.info("Diarization complete — %d segments", len(segments))
        return segments

    except Exception as exc:
        logger.exception("Diarization failed")
        raise RuntimeError(str(exc)) from exc
    finally:
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except OSError:
                logger.warning("Could not delete tmp file: %s", tmp_path)


@app.post(
    "/diarize",
    response_model=DiarizeResponse,
    responses={
        400: {"description": "Unreadable or missing audio"},
        503: {"description": "Model not loaded yet"},
    },
    summary="Diarize an audio file",
    tags=["diarization"],
)
async def diarize(
    file: UploadFile = File(..., description="Audio file (m4a/wav/mp3/…)"),
    num_speakers: Optional[int] = Form(
        default=None,
        description=(
            "Exact number of speakers. Pass 2 for phone calls. "
            "Omit to let pyannote auto-detect."
        ),
        ge=1,
        le=20,
    ),
) -> DiarizeResponse:
    """Perform speaker diarization on the uploaded audio.

    - Accepts any format decodable by ffmpeg/soundfile.
    - When num_speakers is provided it is forwarded verbatim to pyannote
      (improves accuracy for phone calls where the count is known).
    - Processing runs in a threadpool so the event loop is not blocked.
    """
    if not _model_loaded:
        raise HTTPException(
            status_code=503,
            detail=_model_error or "Model not loaded — check HF_TOKEN and service logs.",
        )

    audio_bytes = await file.read()
    if not audio_bytes:
        raise HTTPException(status_code=400, detail="Uploaded file is empty.")

    try:
        segments = await anyio.to_thread.run_sync(
            lambda: _run_diarization(audio_bytes, num_speakers)
        )
    except RuntimeError as exc:
        raise HTTPException(
            status_code=400,
            detail=f"Audio processing failed: {exc}",
        ) from exc

    return DiarizeResponse(
        segments=[Segment(**s) for s in segments]
    )

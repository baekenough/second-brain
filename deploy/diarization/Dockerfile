# Speaker Diarization Service
#
# PREREQUISITES (before building or running):
#   1. Accept gated model licenses on HuggingFace:
#      https://huggingface.co/pyannote/speaker-diarization-3.1
#      https://huggingface.co/pyannote/segmentation-3.0
#   2. Create a HF read-token at https://huggingface.co/settings/tokens
#   3. Pass it at runtime:  docker run -e HF_TOKEN=hf_xxx ...
#      (The token is used ONLY for the one-time model download on first start;
#       subsequent starts use the cached model in /home/app/.cache)
#
# PRIVACY: all audio processing is local. No audio data leaves the container.

FROM python:3.11-slim AS base

# ── System dependencies ──────────────────────────────────────────────────────
# ffmpeg  — audio decoding/resampling for pyannote
# libsndfile1 — soundfile backend used by torchaudio / pyannote
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        libsndfile1 \
    && rm -rf /var/lib/apt/lists/*

# ── Non-root user ────────────────────────────────────────────────────────────
RUN useradd --create-home --shell /bin/bash app
USER app
WORKDIR /home/app

# Ensure user-local bin (pip install --user) is on PATH
ENV PATH="/home/app/.local/bin:${PATH}"

# ── Python dependencies ──────────────────────────────────────────────────────
# Install torch/torchaudio from the CPU-only index first to avoid pulling
# the multi-GB GPU wheel, then install the rest from PyPI.
COPY --chown=app:app requirements.txt .

RUN pip install --no-cache-dir --user \
        torch==2.5.1 \
        torchaudio==2.5.1 \
        --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir --user \
        fastapi==0.115.6 \
        "uvicorn[standard]==0.32.1" \
        python-multipart==0.0.20 \
        anyio==4.7.0 \
        "pyannote.audio==3.3.2"

# ── Application ──────────────────────────────────────────────────────────────
COPY --chown=app:app app.py .

EXPOSE 8001

# Model download cache directory (optional volume mount for persistence)
ENV HF_HOME="/home/app/.cache/huggingface"
ENV DIARIZATION_DEVICE="cpu"

# Health check — wait up to 5 minutes for the model to finish downloading
HEALTHCHECK --interval=15s --timeout=5s --start-period=300s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:8001/health')"

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8001", "--workers", "1"]

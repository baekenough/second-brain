# Speaker Diarization Service

Wraps [pyannote.audio 3.1](https://github.com/pyannote/pyannote-audio) to
provide speaker-segment output for mono call/voice recordings. The Go
transcription pipeline calls this service to get *who-spoke-when* timestamps,
then aligns them with Whisper word-level output.

Addresses: [#111](https://github.com/baekenough/second-brain/issues/111)

---

## Prerequisites

### 1. Accept gated model licenses

pyannote/speaker-diarization-3.1 is a HuggingFace gated model. You must
accept the license for **both** underlying models:

- <https://huggingface.co/pyannote/speaker-diarization-3.1>
- <https://huggingface.co/pyannote/segmentation-3.0>

Click **"Agree and access repository"** on each page while logged in with
your HuggingFace account.

### 2. Create a HuggingFace read-token

<https://huggingface.co/settings/tokens> → *New token* → type: **Read**

The token is used **only once** to download the model weights on first start.
After that the weights are cached locally and the token is no longer needed
for inference.

---

## Running

```bash
# Build
docker build -t second-brain-diarization .

# Run (model downloads on first start — may take several minutes)
docker run -d \
  --name diarization \
  -p 8001:8001 \
  -e HF_TOKEN=hf_xxxxxxxxxxxxxxxx \
  -v diarization-cache:/home/app/.cache/huggingface \
  second-brain-diarization
```

Mount the `diarization-cache` volume to persist model weights across
container restarts and avoid re-downloading.

---

## API

### GET /health

```json
{"status": "ok", "model_loaded": true}
```

Returns `model_loaded: false` (and 200) if the model is still loading or
`HF_TOKEN` was not set. `/diarize` returns 503 until `model_loaded` is true.

### POST /diarize

`multipart/form-data` fields:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | bytes | yes | Audio file — any format ffmpeg can decode (m4a, wav, mp3, …) |
| `num_speakers` | int | no | Exact speaker count. Pass `2` for phone calls; omit for auto-detect |

#### Success response — 200

```json
{
  "segments": [
    {"start": 0.0,   "end": 3.42,  "speaker": "SPEAKER_00"},
    {"start": 3.85,  "end": 7.10,  "speaker": "SPEAKER_01"},
    {"start": 7.50,  "end": 12.00, "speaker": "SPEAKER_00"}
  ]
}
```

Segments are sorted by `start`. The caller aligns these with Whisper
word-timestamps to produce labelled transcript lines.

#### Error responses

| Code | Meaning |
|------|---------|
| 400  | Empty or unreadable audio |
| 503  | Model not loaded yet (check `HF_TOKEN`, wait for download) |

---

## Privacy

**All audio processing is local.** No audio data is sent to any external
service. The only network egress permitted is the one-time HuggingFace model
download (`hf.co`) gated by `HF_TOKEN`. After the weights are cached, the
service operates fully offline.

---

## Configuration

| Env var | Default | Description |
|---------|---------|-------------|
| `HF_TOKEN` | *(required)* | HuggingFace read token for model download |
| `DIARIZATION_DEVICE` | `cpu` | Torch device (`cpu` or `cuda:0`) |
| `HF_HOME` | `/home/app/.cache/huggingface` | Model weight cache directory |

---

## CPU performance note

pyannote on CPU is slow (~1–3× real-time for short clips). For a 5-minute
call expect 5–15 minutes processing time. Consider GPU (`DIARIZATION_DEVICE=cuda:0`)
or scheduling diarization as a background/nightly batch job as discussed in
issue #111.

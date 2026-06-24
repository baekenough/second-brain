# Whisper Deployment Guide

Two deployment modes are available. The collector switches between them via
`WHISPER_API_URL` and `WHISPER_CONCURRENCY` in `.env.local`.

> **Placeholder convention:** This guide uses `<node1-tailscale-ip>`,
> `<node2-tailscale-ip>`, and `<node3-tailscale-ip>` instead of real addresses.
> Replace each `<nodeN-tailscale-ip>` with that node's own Tailscale IP — real
> values are kept only in `.env.local` and on the servers, never committed.
>
> Node roles: **node1** = collector host (runs the local whisper backend + the
> Caddy LB), **node2** = secondary backend, **node3** = the Mac.

---

## Mode A — Non-distributed / Local (default)

Whisper runs as a service inside `docker-compose.local.yml` on the Mac (node3).
The collector reaches it via Docker internal DNS.

**When to use:** Single-machine setup; no home-server infrastructure needed.

### Configuration

```env
# .env.local
WHISPER_API_URL=http://whisper:8000/v1
WHISPER_CONCURRENCY=1
```

### Start

```bash
docker compose -f docker-compose.local.yml up -d
```

The `whisper` service starts automatically via `depends_on`.

---

## Mode B — Distributed 3-node LB (active setup)

Always-on home servers (node1 / node2, reachable via Tailscale) each run a
`faster-whisper` backend. A Caddy round-robin load balancer on node1 fronts the
backends. The Mac (node3) can also join the pool as a third backend.

> Replace each `<nodeN-tailscale-ip>` below with that node's own Tailscale IP.

```
collector (node3 / Mac)
    │  WHISPER_API_URL=http://<node1-tailscale-ip>:8770/v1
    ▼
Caddy LB  <node1-tailscale-ip>:8770   (node1)
    ├──► whisper:8000                  local backend on node1 (compose service DNS)
    ├──► <node2-tailscale-ip>:8771     remote backend on node2 (Tailscale)
    └──► <node3-tailscale-ip>:8771     remote backend on node3 / Mac (Tailscale)
```

When node3 (the Mac) also serves as a backend, it must set `WHISPER_HOST_BIND`
in `.env.local` to its own Tailscale IP so the whisper port binds to the
Tailscale interface (consumed by `docker-compose.local.yml`). The default is
`127.0.0.1` (local-only mode), in which case the published port is unused.

### Node specs

| Node  | Tailscale IP            | Role                | Cores | RAM free |
|-------|-------------------------|---------------------|-------|----------|
| node1 | `<node1-tailscale-ip>`  | backend + LB        | 12    | 28 GB    |
| node2 | `<node2-tailscale-ip>`  | backend (secondary) | 12    | 11 GB    |
| node3 | `<node3-tailscale-ip>`  | backend (Mac)       | —     | —        |

### Ports

| Port | Node  | Purpose                                   |
|------|-------|-------------------------------------------|
| 8770 | node1 | Caddy LB frontend (collector points here) |
| 8771 | node1 | whisper backend                           |
| 8771 | node2 | whisper backend                           |
| 8771 | node3 | whisper backend (Mac, optional)           |

All ports are bound to Tailscale IPs only — LAN and public interfaces are never
exposed (issue #100: call audio is private data).

### Configuration

```env
# .env.local
WHISPER_API_URL=http://<node1-tailscale-ip>:8770/v1
WHISPER_CONCURRENCY=2
# Only on node3 (the Mac) when it also serves as a backend:
WHISPER_HOST_BIND=<node3-tailscale-ip>
```

### Hairpin avoidance

Caddy runs as a container on node1. Docker containers cannot reach the host's
own Tailscale IP (`<node1-tailscale-ip>`) from inside — the NAT hairpin path is
blocked. The local backend is therefore addressed via the compose network
service name (`whisper:8000`), while the remote backends use their Tailscale IPs
(`<node2-tailscale-ip>:8771`, `<node3-tailscale-ip>:8771`). The published port
`<node1-tailscale-ip>:8771` remains for direct access from outside the host
(e.g. health checks from other nodes).

### Deploy

```bash
# node1 — backend + LB
scp deploy/whisper-lb/ubuntu1/docker-compose.yml node1:~/second-brain-whisper/docker-compose.yml
scp deploy/whisper-lb/ubuntu1/Caddyfile          node1:~/second-brain-whisper/Caddyfile
ssh node1 'cd ~/second-brain-whisper && docker compose up -d'

# node2 — backend only
scp deploy/whisper-lb/ubuntu2/docker-compose.yml node2:~/second-brain-whisper/docker-compose.yml
ssh node2 'cd ~/second-brain-whisper && docker compose up -d'
```

> The `ubuntu1` / `ubuntu2` directory names under `deploy/whisper-lb/` are
> historical; `node1` / `node2` above are ssh host aliases you configure in your
> `~/.ssh/config` for the corresponding hosts.

### Model pre-warming

The medium model (~1.5 GB) downloads on first use and is cached in the named
volume `hf-cache`. Pre-warm immediately after deploy:

```bash
# Generate a 1-second silent WAV
ffmpeg -f lavfi -i anullsrc=r=16000:cl=mono -t 1 /tmp/sample.wav

# Warm each backend (downloads model on first call — may take ~30s each)
curl -s http://<node1-tailscale-ip>:8771/v1/audio/transcriptions \
     -F file=@/tmp/sample.wav -F model=Systran/faster-whisper-medium
curl -s http://<node2-tailscale-ip>:8771/v1/audio/transcriptions \
     -F file=@/tmp/sample.wav -F model=Systran/faster-whisper-medium
```

### Verify

```bash
# Each backend must return 200 with a model list
curl -sS http://<node1-tailscale-ip>:8771/v1/models
curl -sS http://<node2-tailscale-ip>:8771/v1/models
curl -sS http://<node1-tailscale-ip>:8770/v1/models   # via LB

# Verify the Mac (node3) collector container can reach the LB
docker exec second-brain-local-collector-1 \
    curl -sS http://<node1-tailscale-ip>:8770/v1/models
```

### Known limitations

- **LB single point of failure:** Caddy and the LB endpoint live on node1.
  If node1 goes down, the LB is unavailable — even though node2's backend
  is still running. Rollback to local mode (see below) while node1 recovers.
- **node2 load:** node2 is marked as secondary (RAM 11 GB free, busier
  workload). With `lb_policy round_robin`, it receives an even share of traffic.
  Monitor CPU/memory during long transcription backlog runs; switch to a
  weighted policy if needed.
- **Long-running requests:** Caddy `read_timeout` / `write_timeout` are set to
  3 hours and `flush_interval -1` to prevent disconnection during multi-hour
  transcription jobs. If requests exceed 3 h, increase the timeouts in
  `Caddyfile` and reload (`docker exec second-brain-whisper-lb caddy reload
  --config /etc/caddy/Caddyfile`).

---

## Rollback: Mode B → Mode A

If the distributed setup is unavailable:

1. Set `.env.local`:
   ```env
   WHISPER_API_URL=http://whisper:8000/v1
   WHISPER_CONCURRENCY=1
   ```
2. Start the local whisper service and recreate the collector:
   ```bash
   docker compose -f docker-compose.local.yml up -d whisper
   docker compose -f docker-compose.local.yml up -d --no-deps collector
   ```
3. The Mac whisper container picks up any backlog; no data loss occurs because
   the `transcription_ledger` table tracks processed files.

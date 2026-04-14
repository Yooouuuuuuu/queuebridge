# FlowDispatch

A Go broker that accepts requests from clients, queues them with priority, and dispatches them across persistent connection pools to backend AI services.

## Overview

FlowDispatch sits between clients and a set of backend services. Each job carries a service type, priority, and optional target pool. The broker routes jobs through two layers:

1. **Service queue** — jobs are grouped by service type (STT, TTS, …) and ordered FIFO with optional priority
2. **Pool dispatch** — N persistent backend connections per pool; the broker keeps each connection continuously busy, cycling through queued jobs one at a time

```
                                 ┌──────────────────────────────────────────────┐
                                 │                 FlowDispatch                 │
                                 │                                              │
                                 │  ┌─ STT queue ─┐   ┌── Pool A ───────────┐  │
Clients        Gateway           │  │  (FIFO +    │──►│ conn 1 ──► ws       │  │
───────        ───────           │  │  priority)  │   │ conn N ──► ws       │  │
WebSocket ──┐                    │  │             │   └─────────────────────┘  │
HTTP      ──┼──► route ──► queue─┤  │             │   ┌── Pool B ───────────┐  │
(gRPC     ──┘    + tag           │  │             │──►│ conn 1 ──► ws       │  │
 planned)                        │  │             │   │ conn N ──► ws       │  │
                                 │  └─────────────┘   └─────────────────────┘  │
                                 │                                              │
                                 │  ┌─ TTS queue ─┐   ┌── Pool C ───────────┐  │
                                 │  │  (FIFO +    │──►│ conn 1 ──► grpc     │  │
                                 │  │  priority)  │   │ conn N ──► grpc     │  │
                                 │  │             │   └─────────────────────┘  │
                                 │  │             │   ┌── Pool D ───────────┐  │
                                 │  │             │──►│ conn 1 ──► grpc     │  │
                                 │  │             │   │ conn N ──► grpc     │  │
                                 │  └─────────────┘   └─────────────────────┘  │
                                 │                                              │
                                 │  ┌─ ??? queue ─┐   ┌── Pool E ───────────┐  │
                                 │  │  (FIFO +    │──►│ conn 1 ──► http     │  │
                                 │  │  priority)  │   │ conn N ──► http     │  │
                                 │  │             │   └─────────────────────┘  │
                                 │  │             │   ┌── Pool F ───────────┐  │
                                 │  │             │──►│ conn 1 ──► http     │  │
                                 │  │             │   │ conn N ──► http     │  │
                                 │  └─────────────┘   └─────────────────────┘  │
                                 └──────────────────────────────────────────────┘
```

### Connection philosophy

**Backend connections (outbound) — always persistent.**
Connections are established at startup and kept alive for the process lifetime. Each connection processes one job at a time; the number of connections per pool is bounded by what the backend service allows.

**Client connections (inbound) — short-lived today, session-oriented planned.**
Currently each WS connection handles one job: `start` → `ready` → audio → `stop` → results → `done`. Session-oriented types (e.g. `customer_service`) with persistent connections and pool affinity are on the roadmap.

### WS job protocol

```
client                    gateway                    broker / STT
  │                          │                            │
  │── {"type":"start"} ─────►│                            │
  │                          │── Submit(job) ────────────►│
  │                          │                     [queue wait]
  │                          │◄── close(ReadyCh) ─────────│  session dequeued
  │◄── {"type":"ready"} ─────│                            │
  │                          │                            │
  │── [audio chunks] ───────►│── SendAudioChunk ─────────►│
  │── {"type":"stop"} ──────►│── close(audioCh) ─────────►│
  │                          │                            │
  │◄── {"type":"result"} ────│◄── ResultCh ───────────────│  partial / final
  │◄── {"type":"done"} ──────│◄── close(ResultCh) ────────│  job complete
```

The `ready` signal is the key backpressure point: the client does not stream audio until the broker has assigned a live backend session to the job. This prevents audio from buffering during queue wait and ensures the STT session is active before the first byte arrives.

## API Reference

All endpoints are under `/v1/`. Calling an unconfigured service (e.g. `/v1/llm`) returns a JSON error immediately.

---

### POST /v1/tts — Text to Speech

Returns raw WAV audio. The caller is responsible for saving it.

**Request** (`application/json`):

| Field | Type | Required | Description |
|---|---|---|---|
| `text` | string | yes | Text to synthesize |
| `pool` | string | no | Target pool name; falls back to least-loaded if missing or congested |
| `priority` | int | no | 0–9, default 0 |
| `speaker` | string | no | Override config default |
| `language` | string | no | Override config default |
| `speed` | float | no | Override config default (0 = use default) |
| `gain` | float | no | Override config default (0 = use default) |
| `out_format` | string | no | `wav` / `mp3` / `pcm` (override config default) |

**Response**: `audio/wav` binary. Headers `X-Pool-Used` and `X-Warning` (if fallback occurred).

```bash
# Save to a specific path
curl -X POST http://localhost:8080/v1/tts \
  -H "Content-Type: application/json" \
  -d '{"text":"今天天氣真好"}' \
  -o /path/to/output.wav

# Save to current directory
curl -X POST http://localhost:8080/v1/tts \
  -H "Content-Type: application/json" \
  -d '{"text":"今天天氣真好"}' \
  -o output.wav

# With voice overrides
curl -X POST http://localhost:8080/v1/tts \
  -H "Content-Type: application/json" \
  -d '{"text":"今天天氣真好","speaker":"Sharon","speed":1.2}' \
  -o output.wav

# Show response headers (pool used, any routing warning)
curl -X POST http://localhost:8080/v1/tts \
  -H "Content-Type: application/json" \
  -d '{"text":"今天天氣真好"}' \
  -o output.wav -D -
```

---

### POST /v1/stt — Speech to Text (file upload)

Accepts a complete WAV file and returns the full transcript as JSON. Use this when you have a pre-recorded file and don't need streaming results.

**Request** (`multipart/form-data`):

| Field | Type | Required | Description |
|---|---|---|---|
| `audio` | file | yes | WAV audio file (16 kHz mono 16-bit recommended) |
| `pool` | string | no | Target pool name; falls back to least-loaded if missing or congested |
| `priority` | int | no | 0–9, default 0 |

**Response** (`application/json`):
```json
{
  "transcript": "很快就沒事了。",
  "pool_used": "stt-default",
  "warning": ""
}
```

```bash
curl -X POST http://localhost:8080/v1/stt \
  -F "audio=@/path/to/audio.wav" | jq .

# With pool targeting
curl -X POST http://localhost:8080/v1/stt \
  -F "audio=@audio.wav" \
  -F "pool=stt-primary" | jq .
```

---

### WS /v1/stt/stream — Speech to Text (streaming)

Use this for real-time transcription where results should arrive as the audio is being spoken.

**Message flow:**
```
client                         server
  │── {"type":"start"}  ──────►│  begin job; optional "pool" and "priority" fields
  │◄── {"type":"warning"} ─────│  if pool fallback occurred (optional)
  │◄── {"type":"ready"}  ──────│  session assigned; start streaming audio now
  │── [binary audio chunks] ──►│
  │── {"type":"stop"}   ───────►│  no more audio
  │◄── {"type":"result",...} ──│  partial results (final: false)
  │◄── {"type":"result",...} ──│  final result (final: true)
  │◄── {"type":"done"}  ───────│  job complete
```

**Client → server messages:**

```json
{"type": "start", "pool": "stt-primary", "priority": 0}
{"type": "stop"}
```

**Server → client messages:**

```json
{"type": "connected"}
{"type": "warning", "msg": "pool \"stt-primary\" not found, routed to \"stt-default\""}
{"type": "ready"}
{"type": "result", "text": "今天天氣", "final": false}
{"type": "result", "text": "今天天氣真好。", "final": true}
{"type": "error", "code": "upstream_failed", "msg": "..."}
{"type": "done"}
```

---

### GET /health

```bash
curl http://localhost:8080/health
# ok
```

---

### Error responses

All errors return JSON:
```json
{"error": "service \"llm\" not configured", "code": "service_unavailable"}
```

| Code | Meaning |
|---|---|
| `service_unavailable` | No pool configured for this service |
| `bad_request` | Invalid request format |
| `upstream_failed` | Backend STT/TTS service returned an error |
| `timeout` | Job did not complete within the deadline |
| `shutting_down` | Server is draining, not accepting new jobs |

---

## Current State

| Service | Protocol | Connections | Status |
|---------|----------|-------------|--------|
| STT (Speech-to-Text) | WebSocket | configurable | working |
| TTS (Text-to-Speech) | gRPC | configurable | working |

## Configuration

FlowDispatch is configured with a YAML file. Copy `flowdispatch.example.yaml` to e.g. `dev.yaml`, fill in your endpoints and tokens, then pass it at startup:

```bash
flowdispatch serve --config dev.yaml
```

You can maintain separate files per environment (`dev.yaml`, `prod.yaml`, …) and select one at startup. `--addr` is the only CLI override — it sets the listen address without touching the config file:

```bash
flowdispatch serve --config prod.yaml --addr :9090
```

**Tokens belong in the config file, not in environment variables.** The 12-factor convention of one env var per secret works fine for a single service, but FlowDispatch connects to multiple backends — each pool can point to a different host with its own token. Managing a separate env var per pool (`STT_TOKEN_A`, `STT_TOKEN_B`, …) does not scale. The config file is the right place: each service entry carries its token next to its endpoint, the file is gitignored, and access is controlled by filesystem permissions. This is the same approach Prometheus uses for scrape credentials.

Tokens and endpoints are never hardcoded in source. `config/config.go` only contains universal defaults (listen address, TTS voice settings, etc.).

Environment variables are available as a convenience override for single-service setups or CI/CD pipelines where managing a file is impractical:

| Variable | Field |
|---|---|
| `LISTEN_ADDR` | `listen` |
| `STT_ENDPOINT` | `stt.endpoint` |
| `STT_TOKEN` | `stt.token` |
| `STT_UID` | `stt.uid` |
| `STT_DOMAIN` | `stt.domain` |
| `TTS_ENDPOINT` | `tts.endpoint` |
| `TTS_TOKEN` | `tts.token` |
| `TTS_UID` | `tts.uid` |
| `TTS_SPEAKER` | `tts.speaker` |
| `TTS_LANGUAGE` | `tts.language` |

Precedence: **`--addr` flag > env vars > config file > built-in defaults**

## Quick Start

```bash
# Normal usage
go run ./cmd/flowdispatch serve --config dev.yaml

# Single requests (uses env vars or built-in defaults for connection)
go run ./cmd/playground stt testdata/stt/input/example.wav
go run ./cmd/playground tts "今天天氣真的很好"

# Batch with N concurrent clients
go run ./cmd/playground stt-batch -workers 20
go run ./cmd/playground tts-batch
```

## Project Structure

```
flowdispatch/
├── cmd/
│   ├── flowdispatch/main.go   # serve subcommand; --pool / --stt / --tts flags
│   ├── playground/main.go    # test CLI: stt, stt-batch, tts, tts-batch
│   └── sttdebug/main.go      # direct STT backend debug tool (bypasses broker)
├── internal/
│   ├── broker/broker.go      # pool registry, priority queue, worker dispatch
│   ├── gateway/gateway.go    # inbound WS and HTTP handlers
│   ├── stt/client.go         # WebSocket STT client with ListeningCh lifecycle
│   └── tts/client.go         # gRPC TTS client
├── config/config.go          # Config struct, LoadFile (YAML), env overrides
├── proto/                    # TTS gRPC protobuf definitions
└── testdata/
    ├── stt/input/            # WAV files for STT testing
    └── tts/input/            # sentence list for TTS batch testing
```

## Tech Stack

- **Language:** Go 1.24
- **Inbound:** HTTP, WebSocket (gRPC planned)
- **Outbound:** WebSocket (STT), gRPC (TTS)
- **Queue:** In-memory priority queue (`container/heap` + `sync.Cond`)

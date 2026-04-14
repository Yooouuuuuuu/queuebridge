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

Two endpoints. The URL identifies the transport; the `service` field in the payload identifies what to do.

---

### POST /v1/http — HTTP (request/response)

**TTS** (`Content-Type: application/json`):

| Field | Type | Required | Description |
|---|---|---|---|
| `service` | string | yes | `"tts"` |
| `text` | string | yes | Text to synthesize |
| `pool` | string | no | Target pool; falls back to least-loaded if missing or congested |
| `priority` | int | no | 0–9, default 0 |
| `speaker` | string | no | Override config default |
| `language` | string | no | Override config default |
| `speed` | float | no | Override config default (0 = use default) |
| `gain` | float | no | Override config default (0 = use default) |
| `out_format` | string | no | `wav` / `mp3` / `pcm` |

Response: `audio/wav` binary. Headers `X-Pool-Used` and `X-Warning` (if fallback occurred).

```bash
# Save to current directory
curl -X POST http://localhost:8080/v1/http \
  -H "Content-Type: application/json" \
  -d '{"service":"tts","text":"今天天氣真好"}' \
  -o output.wav

# With voice overrides
curl -X POST http://localhost:8080/v1/http \
  -H "Content-Type: application/json" \
  -d '{"service":"tts","text":"今天天氣真好","speaker":"Sharon","speed":1.2}' \
  -o output.wav

# Show response headers
curl -X POST http://localhost:8080/v1/http \
  -H "Content-Type: application/json" \
  -d '{"service":"tts","text":"今天天氣真好"}' \
  -o output.wav -D -
```

---

**STT** (`Content-Type: multipart/form-data`):

| Field | Type | Required | Description |
|---|---|---|---|
| `service` | string | yes | `"stt"` |
| `audio` | file | yes | WAV audio file (16 kHz mono 16-bit recommended) |
| `pool` | string | no | Target pool; falls back to least-loaded if missing or congested |
| `priority` | int | no | 0–9, default 0 |

Response (`application/json`):
```json
{
  "transcript": "很快就沒事了。",
  "pool_used": "stt-default",
  "warning": ""
}
```

```bash
curl -X POST http://localhost:8080/v1/http \
  -F "service=stt" \
  -F "audio=@/path/to/audio.wav" | jq .

# With pool targeting
curl -X POST http://localhost:8080/v1/http \
  -F "service=stt" \
  -F "audio=@audio.wav" \
  -F "pool=stt-primary" | jq .
```

---

### WS /v1/ws — WebSocket (streaming)

All fields go in the `start` message. The URL is transport-only.

**STT message flow:**
```
client                         server
  │── {"type":"start","service":"stt",...} ►│
  │◄── {"type":"warning"} ─────────────────│  if pool fallback (optional)
  │◄── {"type":"ready"}  ──────────────────│  session assigned; stream audio now
  │── [binary audio chunks] ──────────────►│
  │── {"type":"stop"}   ───────────────────►│
  │◄── {"type":"result", "text":"...", "final":false} ─│  partial
  │◄── {"type":"result", "text":"...", "final":true}  ─│  final
  │◄── {"type":"done"}  ───────────────────│  job complete
```

**TTS message flow:**
```
client                         server
  │── {"type":"start","service":"tts","text":"..."} ►│
  │◄── {"type":"warning"} ─────────────────│  if pool fallback (optional)
  │◄── [binary audio data] ────────────────│
  │◄── {"type":"done"}  ───────────────────│
```

**Client → server messages:**

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | yes | `"start"` or `"stop"` |
| `service` | string | yes | `"stt"` or `"tts"` |
| `session_type` | string | no | Non-empty → persistent session (heartbeat + pool affinity) |
| `pool` | string | no | Target pool |
| `priority` | int | no | 0–9, default 0 |
| `text` | string | TTS only | Text to synthesize |
| `speaker`, `language`, `speed`, `gain`, `out_format` | — | TTS only | Voice overrides |

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

**Session behaviour:**

- `session_type` empty (default): server closes the connection after the first `done`.
- `session_type` non-empty (e.g. `"customer_service"`): connection stays open after `done`. The first job's pool becomes the sticky pool for all subsequent jobs on that connection (soft affinity — falls back to least-loaded if the sticky pool is congested). Server sends WebSocket pings every 30 s; unresponsive connections are cleaned up.

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
| `service_unavailable` | Service not configured |
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

# Single requests
go run ./cmd/playground stt testdata/stt/input/example.wav
go run ./cmd/playground tts "今天天氣真的很好"

# Batch with N concurrent clients
go run ./cmd/playground stt-batch -workers 5
go run ./cmd/playground tts-batch -workers 5
```

**Manual WS testing** (requires [wscat](https://github.com/websockets/wscat): `npm install -g wscat`):

```bash
# Short-lived STT session (server closes after done)
wscat -c ws://localhost:8080/v1/ws
> {"type":"start","service":"stt"}
# stream audio separately, then:
> {"type":"stop"}

# Session-oriented (connection stays open after done, pool affinity applied)
wscat -c ws://localhost:8080/v1/ws
> {"type":"start","service":"stt","session_type":"customer_service"}
# after done, send another job on the same connection:
> {"type":"start","service":"stt","session_type":"customer_service"}

# TTS over WS
wscat -c ws://localhost:8080/v1/ws
> {"type":"start","service":"tts","text":"今天天氣真好"}
# server sends binary audio then done
```

## Project Structure

```
flowdispatch/
├── cmd/
│   ├── flowdispatch/main.go   # serve subcommand
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

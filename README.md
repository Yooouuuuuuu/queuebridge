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

## Current State

| Service | Protocol | Connections | Status |
|---------|----------|-------------|--------|
| STT (Speech-to-Text) | WebSocket | configurable | working |
| TTS (Text-to-Speech) | gRPC | configurable | working |

## Quick Start

```bash
# Start with 2 STT connections and 1 TTS connection (shorthand flags)
go run ./cmd/queuebridge serve --stt 2 --tts 1

# Or define pools explicitly (repeatable; name:service:protocol:conns)
go run ./cmd/queuebridge serve --pool stt-a:stt:ws:2 --pool tts-a:tts:grpc:1

# Single requests
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
│   ├── queuebridge/main.go   # serve subcommand; --pool / --stt / --tts flags
│   ├── playground/main.go    # test CLI: stt, stt-batch, tts, tts-batch
│   └── sttdebug/main.go      # direct STT backend debug tool (bypasses broker)
├── internal/
│   ├── broker/broker.go      # pool registry, priority queue, worker dispatch
│   ├── gateway/gateway.go    # inbound WS and HTTP handlers
│   ├── stt/client.go         # WebSocket STT client with ListeningCh lifecycle
│   └── tts/client.go         # gRPC TTS client
├── config/config.go          # env-based configuration
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

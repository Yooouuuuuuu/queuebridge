# FlowDispatch

A Go-based job routing broker that accepts requests from clients, queues them with priority, and dispatches them across persistent connection pools to multiple backend services.

## Overview

QueueBridge sits between clients and a set of backend services. Each job carries a tag describing its service type, priority, and optionally a target pool. The broker routes jobs through two layers:

1. **Service queue** — jobs are grouped by service type (STT, TTS, …) and ordered mostly FIFO, with priority for jobs that carry a higher-priority tag
2. **Pool selection** — within each service, there may be multiple backend pools; who decides which pool handles a job (client tag, load-balancing, priority rule) is configurable and still being designed

```
                                    ┌─────────────────────────────────────────────┐
                                    │              QueueBridge                    │
                                    │                                             │
                                    │  ┌─ STT queue ─┐   ┌── Pool A ──┐          │
Clients          Gateway            │  │  (FIFO +    │──►│ N workers  ├──► ws    │
───────          ───────            │  │  priority)  │   └────────────┘          │
HTTP    ──┐                         │  │             │──►┌── Pool B ──┐          │
WebSocket─┼──► normalize ──► route ─┤  │             │   │ N workers  ├──► ws    │
gRPC    ──┘      + tag              │  └─────────────┘   └────────────┘          │
                                    │                       ↑ who decides?        │
                                    │                  (client tag / algo / rule) │
                                    │                                             │
                                    │  ┌─ TTS queue ─┐   ┌── Pool D ──┐          │
                                    │  │  (FIFO +    │──►│ N workers  ├──► grpc  │
                                    │  │  priority)  │   └────────────┘          │
                                    │  │             │──►┌── Pool E ──┐          │
                                    │  │             │   │ N workers  ├──► grpc  │
                                    │  └─────────────┘   └────────────┘          │
                                    │                                             │
                                    │  ┌─ ??? queue ─┐   ┌── Pool F ──┐          │
                                    │  │             │──►│ N workers  ├──► http  │
                                    │  └─────────────┘   └────────────┘          │
                                    └─────────────────────────────────────────────┘
```

Most jobs are FIFO within their service queue. Jobs can carry a priority tag to move ahead. Some jobs may specify a pool directly; others let the system decide based on load or rules.

### Connection philosophy

**Backend connections (outbound) — always persistent.**
For WebSocket and gRPC backends, connections are established at startup and kept alive for the lifetime of the process. The number of connections per pool is bounded by what the backend service authorizes or can handle — the service itself may have its own internal queue or concurrency limit (e.g. returning a "server busy" signal when saturated), so opening more connections than the service allows adds no throughput. HTTP backends are stateless and connect per-request.

**Client connections (inbound) — determined by the client's type tag.**
The client declares its type when connecting. The gateway uses this to decide how to treat the connection:

- `customer_service` (and similar session-oriented types) — connection is kept alive for the duration of the conversation. Session lifetime is unknown and may vary from minutes to hours; no idle timeout is applied. A heartbeat is used to detect dead connections. The gateway stores a session registry (`session_id → assigned pool`) to enforce pool affinity: all jobs from the same conversation are routed to the same backend pool, ensuring consistency (e.g. same STT model state across turns).
- Other types — short-lived; the connection closes when the job is done.

This means a single conversation maps to one persistent inbound WS, and the gateway pins it to one backend pool for its lifetime.

## Current State

The prototype is running with two active backend services:

| Service | Protocol | Status |
|---------|----------|--------|
| STT (Speech-to-Text) | WebSocket | working |
| TTS (Text-to-Speech) | gRPC | working |

The broker maintains persistent connections to both backends. Connection counts are configured at startup via CLI flags.

```bash
go run ./cmd/queuebridge serve --stt 2 --tts 1
```

## Project Structure

```
queuebridge/
├── cmd/
│   ├── queuebridge/main.go   # serve, test-stt, test-tts subcommands
│   ├── playground/main.go    # manual test CLI (stt, tts, stt-batch, tts-batch)
│   └── sttdebug/main.go      # direct STT backend debug tool
├── internal/
│   ├── broker/broker.go      # persistent pools, job queue, worker dispatch
│   ├── gateway/gateway.go    # inbound HTTP and WebSocket handlers
│   ├── stt/client.go         # WebSocket STT client
│   └── tts/client.go         # gRPC TTS client
├── config/config.go          # env-based configuration
├── proto/                    # TTS gRPC protobuf definitions
└── testdata/
    ├── stt/input/            # WAV files for STT testing
    └── tts/input/            # sentence list for TTS batch testing
```

## Quick Start

```bash
# Start the gateway (with 2 STT workers and 1 TTS connection)
go run ./cmd/queuebridge serve --stt 2 --tts 1

# Test single requests
go run ./cmd/playground tts "今天天氣真的很好"
go run ./cmd/playground stt testdata/stt/input/example.wav

# Run batch with concurrent workers
go run ./cmd/playground stt-batch -workers 20
go run ./cmd/playground tts-batch
```

## Tech Stack

- **Language:** Go 1.24
- **Inbound:** HTTP, WebSocket (gRPC planned)
- **Outbound:** WebSocket, gRPC
- **Queue:** In-memory priority queue (planned)

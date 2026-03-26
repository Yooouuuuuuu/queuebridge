# QueueBridge

A Go-based gateway that queues incoming requests and distributes them across a pool of outbound WebSocket connections to maximize backend service utilization.

## Overview

QueueBridge sits between your clients and a backend service, accepting traffic via HTTP, WebSocket, and gRPC, then intelligently queuing and routing requests through a managed connection pool.

```
Clients                  QueueBridge                  Backend Service
───────                  ───────────                  ───────────────
HTTP    ──┐              ┌──────────┐
WebSocket ──┼──► Gateway ──► Queue ──► Pool ──────────► WebSocket (WS)
gRPC    ──┘              └──────────┘                   gRPC (planned)
```

## Project Structure

```
queuebridge/
├── cmd/
│   └── queuebridge/
│       └── main.go          # Entry point
├── internal/
│   ├── gateway/
│   │   └── gateway.go       # Inbound HTTP, WebSocket, and gRPC handlers
│   ├── queue/
│   │   └── queue.go         # Request queue logic
│   ├── pool/
│   │   └── pool.go          # Outbound WebSocket connection pool
│   └── broker/
│       └── broker.go        # Wires gateway, queue, and pool together
├── config/
│   └── config.go            # Configuration structs and loading
└── go.mod
```

## Components

**Gateway** — Accepts inbound connections from clients over HTTP, WebSocket, and gRPC. Parses and normalizes requests before handing them to the broker.

**Queue** — Holds pending requests and dispatches them in order. Designed to support priority queuing and backpressure strategies.

**Pool** — Manages a pool of outbound WebSocket connections to the backend service. Distributes load across connections to maximize throughput.

**Broker** — The central coordinator. Receives requests from the gateway, enqueues them, and assigns them to available pool connections.

**Config** — Loads and validates configuration from environment variables or a config file.

## Planned Features

- [x] Project structure
- [ ] HTTP inbound gateway
- [ ] WebSocket inbound gateway
- [ ] Request queue with backpressure
- [ ] Outbound WebSocket connection pool
- [ ] Broker wiring all components
- [ ] gRPC inbound gateway
- [ ] gRPC outbound connection support
- [ ] Metrics and observability
- [ ] Docker support

## Tech Stack

- **Language:** Go 1.22
- **Inbound:** HTTP, WebSocket, gRPC
- **Outbound:** WebSocket (gRPC planned)
- **Queue:** In-memory (Redis-backed planned)

## Getting Started

```bash
# Clone the repo
git clone https://github.com/Yooouuuuuuu/queuebridge.git
cd queuebridge

# Run
go run ./cmd/queuebridge
```

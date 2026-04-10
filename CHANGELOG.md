# Changelog

## [Unreleased] — Broker Refactor

### Planned
- Generic `Job` type with service tag, priority, and source metadata
- Per-service pool registry: multiple pools per service type (e.g. STT pool A
  and pool B on different backends); routing can be client-driven (tag
  specifies pool) or system-driven (load-balancing algorithm)
- Priority-aware queue: high-priority jobs skip ahead or pin to a specific pool
- Blocking submission — no rejection; queue until a worker is free
- README rewrite to reflect the general-purpose broker architecture

---

## [0.2.0] — 2026-04-10 — Persistent Broker & Parallel Batch

### Added
- `internal/broker/`: persistent STT WebSocket and TTS gRPC connection pools
  at startup; `--stt N` / `--tts N` flags control pool sizes
- STT workers share one `jobCh`; Go channel fan-out balances load naturally
- TTS pool uses atomic round-robin across M gRPC connections
- Automatic STT reconnection with backoff on drop
- Broker status log every 10 s: active sessions, queue depth, pool sizes
- `stt-batch -workers N`: concurrent batch with semaphore; per-item timing log
- Single `stt` saves transcript to a timestamped `.txt` file
- Retry with backoff (3 attempts) on empty or rejected STT sessions
- Gateway closes connection immediately on queue-full for fast client retry

### Changed
- Gateway routes through broker instead of opening a new backend conn per request
- `gateway.New` takes `*broker.Broker` instead of `config.Config`
- `serve` uses `flag.FlagSet` for proper subcommand flag parsing

### Fixed
- Empty transcript silently counted as "ok" — now correctly fails
- STT 1006 close logged as error — reclassified as normal client disconnect

---

## [Unreleased] - 2026-04-09

### Added
- `internal/gateway`: inbound HTTP `/tts` and WebSocket `/ws` handlers
- `cmd/playground`: `stt`, `stt-batch`, `tts`, `tts-batch` test commands
- `cmd/sttdebug`: direct STT backend debug tool (bypasses gateway)
- `cmd/wstest`: quick WebSocket smoke test
- `testdata/stt/input/`: directory for WAV input files
- `testdata/tts/input/sentences.txt`: 514 Traditional Chinese sentences for TTS batch testing

### Changed
- `internal/stt/client.go`: fixed protocol — added `action` and `platform` fields to start payload, changed stop payload to `{"action":"stop"}`
- `config/config.go`: added STT default token
- `cmd/queuebridge/main.go`: added `serve` command, wires config into gateway

---

## 2026-04-01 — connect to service apis

### Added
- `internal/stt/client.go`: WebSocket STT client (connect, start/stop recognition, send audio, read messages)
- `internal/tts/client.go`: gRPC TTS client (connect, synthesize, stream, per-request options)
- `proto/tts.proto`, `tts.pb.go`, `tts_grpc.pb.go`: TTS gRPC protobuf definitions
- `config/config.go`: config loading from env vars with hardcoded defaults
- `cmd/queuebridge/main.go`: `test-stt`, `test-tts`, `test-both` test subcommands
- `.gitignore`
- `go.mod` / `go.sum`: gorilla/websocket, grpc, protobuf dependencies

---

## 2026-03-26 — init: project structure

### Added
- `README.md`: project overview and planned architecture
- Scaffold files for `cmd/queuebridge`, `config`, `internal/gateway`, `internal/queue`, `internal/pool`, `internal/broker`

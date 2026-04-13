# Changelog

## [0.4.0] — 2026-04-13 — Session Reliability & Backpressure

### Added
- **Backpressure via `ReadyCh`**: broker closes `job.ReadyCh` when a session dequeues
  the job; the gateway sends `{"type":"ready"}` to the client at that moment. Clients
  wait for `ready` before streaming audio, so no audio is buffered during queue wait.
- **`{"type":"done"}` from gateway**: sent when `ResultCh` closes (job fully complete).
  Clients exit immediately instead of waiting for a read deadline — eliminated ~5 s of
  dead time per file when workers were the throughput bottleneck.
- **`ListeningCh()` on STT client**: returns a channel that is closed when the server
  sends `{"state":"listening"}` after a session ends. Broker waits on this before each
  `StartRecognition` to eliminate the stop/start race that caused silent rejections.
- **`idle atomic.Int32` on pool**: tracks sessions waiting for a job; status ticker now
  logs `conns / active / idle / queued`.

### Fixed
- **Silent STT session rejections under load**: `StartRecognition` was sent before the
  server finished resetting the previous session (the server sends `{"state":"listening"}`
  asynchronously after `isFinal`). Caused consistent failures for specific files across
  all retry attempts. Fixed by `ListeningCh` wait before each `StartRecognition`.
- **`ListeningCh` captured immediately after `StartRecognition`** (not at dequeue time):
  the server now has the entire job-processing window to send `listening`; the wait is
  near-instant on a busy queue instead of blocking for the full server reset latency.

### Changed
- Removed `time.Sleep(chunkDuration)` from playground audio streaming — audio is now
  sent at full speed. Batch throughput: 5 000 files with 2 connections in 5 m 29 s
  (vs. minutes per hundred files before).
- `runArmedSession` comment and log messages updated to reflect the simplified lifecycle
  (no pre-arming, no re-arm after job).

---

## [0.3.0] — 2026-04-10 — Broker Refactor

### Added
- Generic `Job` / `STTPayload` / `TTSPayload` / `Result` types replace `STTJob` / `STTResult`
- Named pool registry: `--pool name:service:protocol:conns` (repeatable flag)
- Priority queue per pool (`container/heap` + `sync.Cond`): 0–9 priority, higher dispatched
  first; FIFO tiebreaker within same priority
- Blocking submit — `Submit()` always enqueues, never rejects
- Least-loaded auto-routing when `job.Pool` is empty
- Per-pool status log every 10s: workers / active / queued
- `--stt N` / `--tts N` kept as convenience shortcuts

### Fixed
- Panic: `send on closed channel` in `OnResult` callback — added `<-readDone` to wait for
  `ReadMessages` goroutine to stop before closing `ResultCh`
- `proto/tts.pb.go` regenerated (corrupted by module rename sed)

### Changed
- `broker.Synthesize` / `broker.SubmitSTT` replaced by unified `broker.Submit(job Job)`
- Gateway uses `broker.Result` instead of `broker.STTResult`

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

## [0.1.0] — 2026-04-09 — Inbound Gateway & Playground

### Added
- `internal/gateway`: inbound HTTP `/tts` and WebSocket `/ws` handlers
- `cmd/playground`: `stt`, `stt-batch`, `tts`, `tts-batch` test commands
- `cmd/sttdebug`: direct STT backend debug tool (bypasses gateway)
- `cmd/wstest`: quick WebSocket smoke test
- `testdata/stt/input/`: directory for WAV input files
- `testdata/tts/input/sentences.txt`: 514 Traditional Chinese sentences for TTS batch testing

### Changed
- `internal/stt/client.go`: fixed protocol — added `action` and `platform` fields to
  start payload, changed stop payload to `{"action":"stop"}`
- `config/config.go`: added STT default token
- `cmd/queuebridge/main.go`: added `serve` command, wires config into gateway

---

## [0.0.2] — 2026-04-01 — Service API Clients

### Added
- `internal/stt/client.go`: WebSocket STT client (connect, start/stop recognition, send audio, read messages)
- `internal/tts/client.go`: gRPC TTS client (connect, synthesize, stream, per-request options)
- `proto/tts.proto`, `tts.pb.go`, `tts_grpc.pb.go`: TTS gRPC protobuf definitions
- `config/config.go`: config loading from env vars with hardcoded defaults
- `cmd/queuebridge/main.go`: `test-stt`, `test-tts`, `test-both` test subcommands
- `.gitignore`
- `go.mod` / `go.sum`: gorilla/websocket, grpc, protobuf dependencies

---

## [0.0.1] — 2026-03-26 — Project Init

### Added
- `README.md`: project overview and planned architecture
- Scaffold: `cmd/queuebridge`, `config`, `internal/gateway`, `internal/queue`, `internal/pool`, `internal/broker`

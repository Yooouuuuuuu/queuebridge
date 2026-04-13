# TODO

Items are ordered by priority. Config and shutdown unblock everything else; session
management is the core product feature; observability and gRPC follow from there.

---

## 1 · Config file

- [ ] Pool definitions from a config file (YAML or TOML): name, service, protocol, endpoint, connections
- [ ] Load from env or config file; CLI flags remain as overrides

---

## 2 · Graceful shutdown

- [ ] Stop accepting new jobs on SIGTERM / SIGINT
- [ ] Drain the queue: wait for all in-flight jobs to finish before exiting
- [ ] Signal in-progress workers to complete (not abort) their current job

---

## 3 · Inbound session management

- [ ] Client sends type tag on connect (e.g. `type: customer_service`)
- [ ] Gateway checks type tag to decide connection lifetime:
  - session-oriented types → persistent connection, heartbeat liveness
  - other types → short-lived, close after job completes (current behaviour)
- [ ] Session registry in gateway: `session_id → { pool_id, last_seen, metadata }`
- [ ] Session affinity: first job picks a pool; all subsequent jobs on the same connection go to the same pool
- [ ] Heartbeat: ping/pong to detect dead connections; no idle timeout
- [ ] Session cleanup on disconnect or heartbeat failure

---

## 4 · Observability

- [ ] Per-pool metrics: active, idle, queue depth, throughput, error rate
- [ ] `/metrics` endpoint (Prometheus format)

---

## 5 · gRPC inbound gateway

- [ ] Accept jobs over gRPC in addition to HTTP/WS
- [ ] Job metadata (priority, target pool) in request headers or message fields

---

## 6 · Minor / polish

- [ ] Optional per-job timeout: job cancelled if not picked up within N seconds
- [ ] High-priority jobs with explicit pool tag bypassing normal queue ordering (not yet tested under load)
- [ ] Docker support

---

## Done

### Broker
- [x] Generic `Job` struct: `Service`, `Pool`, `Priority`, `Payload`, `ResultCh`
- [x] Unified `Submit(job Job)` entry point; payload typed per service
- [x] `ReadyCh chan struct{}` — broker closes it when a session dequeues the job; client only starts streaming once a backend session is live (backpressure)
- [x] `active` + `idle` counters per pool; status ticker logs `conns / active / idle / queued`
- [x] Pool registry: `service → []*pool`; N persistent connections per pool
- [x] Client tag selects target pool directly, or empty for least-loaded auto-routing
- [x] Priority queue (`container/heap` + mutex + `sync.Cond`); FIFO within same priority
- [x] Blocking submit — no rejection; always enqueues

### STT session lifecycle
- [x] `StartRecognition` called per-job (not pre-armed)
- [x] `ListeningCh()` on STT client: channel closed when server sends `{"state":"listening"}` — eliminates the start/stop race that caused silent session rejections
- [x] `ListeningCh` captured right after `StartRecognition` so the server's reset window overlaps with the current job's audio processing — wait is near-instant on a busy queue

### Gateway
- [x] WS inbound protocol: `start` → `ready` (backpressure) → audio chunks → `stop` → results → `done`
- [x] `done` message sent when `ResultCh` closes — client exits immediately, no read-deadline dead time
- [x] HTTP POST inbound for TTS

# TODO

Items are ordered by priority.

---

## 1 · TTS reconnection

STT workers reconnect automatically on connection drop; TTS workers have no equivalent.
If the outbound gRPC connection drops, the worker keeps failing jobs silently.

- [ ] Reconnect loop in `runTTSWorker` mirroring the STT reconnect pattern

---

## 2 · Per-job queue timeout

Jobs currently wait in the queue forever if all workers are busy.

- [ ] Optional `timeout_ms` field in job payload; broker cancels the job and returns an
      error to the client if no worker dequeues it within the deadline

---

## 3 · Pool routing tags

Currently all named-pool requests use soft routing: warn + fallback to least-loaded.
Two additional behaviours are needed:

- [ ] `strict` — no fallback; return 503 immediately if the named pool is missing or congested
- [ ] `priority-privilege` — skip the congestion check; always use the named pool even when busy

Tag would be a field in the `start` message (WS / gRPC Stream) or JSON body (HTTP).

---

## 4 · Inbound auth

No authentication on inbound requests; any client that can reach the port can submit jobs.

- [ ] API key check on all inbound transports (HTTP header, WS `start` message, gRPC metadata)
- [ ] Keys defined in config file; unknown key → 401 / `unauthenticated`

---

## 5 · Docker

- [ ] `Dockerfile` (multi-stage: build + minimal runtime image)
- [ ] `docker-compose.yml` for local dev (mounts `dev.yaml`, exposes `:8080` and `:9090`)

---

## Done

### Observability
- [x] Per-pool metrics: active, idle, queue depth, throughput, error rate
- [x] `/metrics` endpoint (Prometheus format)

### gRPC inbound gateway
- [x] `proto/flowdispatch.proto`: `Submit` (unary) and `Stream` (bidirectional streaming) RPCs
- [x] Message fields match existing payload design: `service`, `session_type`, `pool`, `priority`, plus service-specific fields
- [x] `internal/grpcgateway` started alongside HTTP/WS in `cmd/flowdispatch/main.go`
- [x] Session management (session_type, sticky pool affinity) — same logic as WS; gRPC keepalive replaces manual heartbeat
- [x] `grpc_listen` field in YAML config (default `:9090`)

### Inbound session management
- [x] Client sends `session_type` in `start` message (e.g. `"session_type":"customer_service"`)
- [x] Gateway checks `session_type` to decide connection lifetime:
  - non-empty → persistent connection, heartbeat liveness
  - empty → short-lived, server closes after first job completes
- [x] Session registry in gateway: `session_id → { client_type, sticky_pool, last_seen }`
- [x] Session affinity (soft): first job picks a pool; all subsequent jobs on the same connection prefer that pool; falls back to least-loaded if congested
- [x] Heartbeat: ping every 30s, cancel connection if ping fails; no idle timeout
- [x] Session cleanup on disconnect or heartbeat failure

### Config & operations
- [x] YAML config file (`--config`): pools, listen address, STT/TTS settings
- [x] Per-pool endpoint override; tokens and endpoints removed from source defaults
- [x] Graceful shutdown: drain flag, `sync.WaitGroup` per worker, 30s timeout
- [x] Two-phase context split: signal stops gateway; broker workers finish in-flight jobs

### Broker
- [x] Generic `Job` struct: `Service`, `Pool`, `Priority`, `Payload`, `ResultCh`
- [x] Unified `Submit(job Job)` entry point; payload typed per service
- [x] `ReadyCh chan struct{}` — broker closes it when a session dequeues the job; client only starts streaming once a backend session is live (backpressure)
- [x] `active` + `idle` counters per pool
- [x] Pool registry: `service → []*pool`; N persistent connections per pool
- [x] Client tag selects target pool directly, or empty for least-loaded auto-routing
- [x] Priority queue (`container/heap` + mutex + `sync.Cond`); FIFO within same priority
- [x] Blocking submit — no rejection; always enqueues

### STT session lifecycle
- [x] `StartRecognition` called per-job (not pre-armed)
- [x] `ListeningCh()` on STT client: channel closed when server sends `{"state":"listening"}` — eliminates the start/stop race that caused silent session rejections
- [x] `ListeningCh` captured right after `StartRecognition` so the server's reset window overlaps with the current job's audio processing — wait is near-instant on a busy queue

### Gateway & API
- [x] Transport-unified API: URL = transport only (`POST /v1/http`, `WS /v1/ws`); `service` field in payload routes the job
- [x] WS inbound protocol: `start` → `ready` (backpressure) → audio chunks → `stop` → results → `done`
- [x] `done` message sent when `ResultCh` closes — client exits immediately, no read-deadline dead time
- [x] HTTP POST inbound for TTS and STT (multipart WAV upload)
- [x] Per-request TTS voice overrides (speaker, language, speed, gain, out_format)
- [x] Smart pool routing: warn + fallback to least-loaded when named pool is missing or congested
- [x] Consistent JSON error format with string error codes across all endpoints

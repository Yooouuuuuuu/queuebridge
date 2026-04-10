# TODO

## Inbound Session Management

- [ ] Client sends type tag on connect (e.g. `type: customer_service`)
- [ ] Gateway checks type tag to decide connection lifetime:
  - session-oriented types → persistent connection, heartbeat liveness
  - other types → short-lived, close after job completes
- [ ] Session registry in gateway: `session_id → { pool_id, last_seen, metadata }`
- [ ] Session affinity: first job picks a pool, all subsequent jobs on the same connection go to the same pool
- [ ] Heartbeat: ping/pong to detect dead connections; no idle timeout
- [ ] Session cleanup on disconnect or heartbeat failure

---

## Broker Refactor

### Job model
- [x] Define generic `Job` struct with: `Service`, `Pool`, `Priority`, `Payload`, `ResultCh`
- [x] Replace `STTJob` / TTS direct call with unified `Submit(job Job)` entry point
- [x] Job payload typed per service (STT audio stream, TTS text, etc.)

### Pool routing
- [x] Pool registry: map of service name → list of pools (e.g. `stt → [poolA, poolB]`)
- [x] Each pool has a name, protocol (ws/grpc/http), and N persistent workers
- [x] Client tag selects target pool directly, or leaves it empty for auto-routing
- [x] Auto-routing: pick least-loaded pool for the target service

### Priority queue
- [x] Replace `chan Job` with a priority queue (`container/heap` + mutex + `sync.Cond`)
- [x] Higher priority jobs dispatched first within the same pool
- [ ] High-priority jobs with explicit pool tag bypass normal queue ordering (not yet tested)

### Submission
- [x] Blocking submit — no rejection; always enqueues
- [ ] Optional per-job timeout: job cancelled if not picked up within N seconds

### Bug to fix
- [ ] Jobs get stuck retrying forever under heavy load (100/100 and 10/10 pool/workers
      both reproduced) — suspect `ReadMessages` blocking on `<-readDone`, or audio drain
      goroutine keeping `audioCh` open past session end

### Config
- [ ] Pool definitions in config (name, service, protocol, endpoint, connections)
- [ ] Load from env or config file

---

## Gateway

- [ ] gRPC inbound gateway (accept jobs over gRPC, not just HTTP/WS)
- [ ] Job metadata (priority, target pool) passed in request headers or message fields

---

## Observability

- [ ] Per-pool metrics: active workers, queue depth, throughput, error rate
- [ ] Expose `/metrics` endpoint (Prometheus format)

---

## Ops

- [ ] Docker support
- [ ] Graceful shutdown: drain queue before exit

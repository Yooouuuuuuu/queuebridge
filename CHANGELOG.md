# Changelog

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

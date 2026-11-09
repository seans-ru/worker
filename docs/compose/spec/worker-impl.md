---
feature: worker-impl
status: delivered
updated: 2026-09-20
branch: feat/worker-impl
commits: 5c32eed..4247ddb
---

# Worker Implementation

## Report

**What was built** — A Go 1.23 worker for the Seans media service that pulls tasks from a backend API, downloads torrents via qBittorrent Web API v2, and uploads files to S3 via rclone subprocess. The worker implements a full state machine (preparing→downloading→uploading→done) with stall detection, metadata timeouts, quota checks, file selection with suffix fallback, and graceful shutdown with drain support.

**Verification** — `go build ./...` compiles clean. `go vet ./...` passes. `go test ./... -v -count=1` runs 27 tests across 3 packages (backend, qbit, engine) — all PASS.

**Journey log**:
- `syscall.Statfs` is Linux-only; split into build-tagged files (`disk.go` + `disk_windows.go`)
- qBittorrent test helper initially used custom logger type instead of `*slog.Logger`; fixed to use real slog with io.Discard
- Task runner uses concrete types (not interfaces) for simplicity

## [S1] Problem

Seans needs a worker component that pulls tasks from the backend API, downloads torrents via qBittorrent, uploads files to S3 via rclone, and reports progress. The worker must be resilient (graceful shutdown, lease management, retry logic) and testable.

## [S2] Design

### Architecture
- Go 1.23+ stdlib only (net/http, encoding/json, log/slog, os/signal, context)
- rclone as subprocess for S3 upload (generates INI config at startup)
- qBittorrent Web API v2 for torrent management
- Pull model: worker polls backend for tasks, sends heartbeats

### API Contract (v1)
Worker calls these backend endpoints:
- `POST /v1/workers/register` — register with shared secret, get worker_id + worker_token
- `POST /v1/workers/{worker_id}/heartbeat` — report all active tasks progress
- `POST /v1/workers/{worker_id}/claim` — claim next task
- `POST /v1/tasks/{task_id}/complete` — report task success with file list
- `POST /v1/tasks/{task_id}/fail` — report task failure (permanent or temporary)

Auth: `Authorization: Bearer {worker_token}` (register uses `WORKER_REGISTER_TOKEN`)

### Task State Machine
`preparing → downloading → uploading → done`

#### Preparing
1. Clean staging dir `/staging/{task_id}/`
2. Remove leftover torrents from qBittorrent (same category+savepath)
3. Add torrent (magnet or base64 .torrent file)
4. Wait for torrent metadata (5 min timeout for magnets)
5. Read file list, check total size vs max_bytes
6. Set file priorities (selected=1, unselected=0)

#### Downloading
1. Poll qBittorrent every 5s for progress
2. Stall detection: no progress for 30 min → fail(permanent)
3. Task timeout: 12 hours → fail(temporary)
4. Complete when progress >= 1.0 and not actively downloading

#### Uploading
1. Compute s3_key = dest_prefix + relative_path
2. rclone move with retry (3 attempts)
3. Clean staging dir
4. Remove torrent from qBittorrent
5. POST complete to backend

### Config
All from environment variables. See .env.example for full list.



## [S3] Out of Scope
- Backend implementation
- Multiple concurrent task support in v1 (MAX_CONCURRENT_TASKS defaults to 1)
- TLS/certificate management
- Web UI for worker monitoring

## Tasks
- [x] T1: Go module + config — acceptance: `go build ./cmd/agent` compiles, config parses from env (covers: S2)
- [x] T2: Backend HTTP client — acceptance: register/claim/heartbeat/complete/fail with auth, 401 re-register, retry logic (covers: S2)
- [x] T3: qBittorrent client — acceptance: login, add torrent, poll progress, set priorities, delete torrent (covers: S2)
- [x] T4: rclone uploader — acceptance: generate rclone.conf, execute rclone move with retry (covers: S2)
- [x] T5: Task state machine — acceptance: preparing→downloading→uploading→done with all error paths (covers: S2)
- [x] T6: Engine (claim loop, heartbeat, graceful shutdown) — acceptance: claim loop, heartbeat goroutine, SIGTERM handling (covers: S2)
- [x] T7: Agent entrypoint — acceptance: `make build` produces working binary, logs version (covers: S2)
- [x] T9: Docker (Dockerfile + compose) — acceptance: `docker compose up` starts qbit + agent (covers: S2)
- [x] T10: Tests — acceptance: qbit/client_test, engine/task_test, backend/client_test all pass (covers: S2)
- [x] T11: .env.example + Makefile + README — acceptance: all env vars documented, README has 3-command deploy guide (covers: S2)

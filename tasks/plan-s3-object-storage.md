# Implementation Plan: S3 Object Storage And Request Interval

## Overview

Add two features to Pixiegrabber:

1. S3-compatible object storage mode. When the user enables it, Pixiegrabber stores References and manifests in a bucket instead of local disk.
2. A request interval flag. It throttles all Pixieset API calls to avoid flooding the Pixieset servers.

The user confirmed these decisions:

- Manifests upload to the bucket in S3 mode.
- The interval applies to all Pixieset API calls.
- The target is a generic S3-compatible endpoint.

## Architecture Decisions

- Use `github.com/minio/minio-go/v7` for S3-compatible storage. It supports custom endpoints, path-style addressing, and automatic multipart upload.
- Read S3 secrets from environment variables. Never read them from flags or logs.
- Introduce a `Store` interface in a new `internal/store` package. `LocalStore` wraps `outputfs.FS`. `S3Store` wraps minio-go.
- Refactor `internal/app`, `internal/archive`, `internal/download`, and `internal/manifest` to use the `Store` interface.
- Reuse the `internal/paths` package to build S3 keys. Keys mirror the local layout.
- Use `golang.org/x/time/rate` for the request interval. Add jitter and retry with backoff on 429 and 5xx responses.
- Inject one shared limiter into the Pixieset API client and the downloader client.
- Store the SHA-256 checksum as object metadata and in the manifest.
- Lock the bucket with a lock object. Acquire with a conditional PUT (`If-None-Match: *`). Detect stale locks by `LastModified` age and overwrite them. Release by deleting the lock object.
- The `--interval` flag defaults to `0`, which disables throttling. Users opt in with a value such as `--interval 2s`.

## Dependency Graph

```text
Pixieset API client ─┐
Browser cookies ─────┼─> planner ─> Store ─> CLI
Paths and manifest ──┘              ├─> LocalStore (outputfs.FS)
                                   └─> S3Store (minio-go)
Request interval ──> API client and downloader
```

## Phases

### Phase 1: Storage Abstraction

- Task 1: Define the `Store` interface in `internal/store`.
- Task 2: Implement `LocalStore` over `outputfs.FS`.
- Task 3: Refactor `app`, `archive`, `download`, and `manifest` to use `Store`.
- Task 4: Keep local behavior identical.

Checkpoint:

- `go test ./...` passes with the local store.
- No behavior change for existing users.

### Phase 2: S3 Store

- Task 5: Add S3 flags and environment variables.
- Task 6: Implement `S3Store` with minio-go.
- Task 7: Build S3 keys from the `paths` package.
- Task 8: Add bucket locking with a lock object. Cover conditional acquire, stale detection, and release on exit.
- Task 9: Store SHA-256 as object metadata.

Checkpoint:

- `go test ./internal/store` passes against a fake S3 server.

### Phase 3: Request Interval

- Task 10: Add the `--interval` flag.
- Task 11: Implement the throttle package with jitter.
- Task 12: Inject the limiter into the API client and the downloader.
- Task 13: Add retry with backoff on 429 and 5xx. Honor `Retry-After`.

Checkpoint:

- `go test ./internal/throttle` passes.

### Phase 4: Integration And Docs

- Task 14: Wire S3 mode into the app flow. Cover manifest, verify, video stop, and prompt.
- Task 15: Add end-to-end tests for S3 mode.
- Task 16: Update README, spec, and ADR.

Checkpoint:

- `go test ./...`
- `go vet ./...`
- `go build ./cmd/pixiegrabber`
- End-to-end S3 run against local fixture servers.

## Evidence Path

- Claim: local behavior does not change.
  Evidence: existing tests pass unchanged after the Store refactor.
- Claim: S3 mode stores References and manifests in the bucket.
  Evidence: end-to-end test with a fake S3 server verifies objects and manifest.
- Claim: the interval throttles all Pixieset calls.
  Evidence: throttle tests measure request spacing and jitter.
- Claim: 429 responses trigger backoff and honor Retry-After.
  Evidence: HTTP tests with injected 429 responses.
- Claim: credentials never appear in logs or manifests.
  Evidence: redaction tests and env-var-only credential loading.

## Risks And Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Store refactor changes local behavior | High | Keep LocalStore identical. Run existing tests. |
| S3 has no atomic replace or flock | Medium | Use per-key atomic PUT and a lock object. |
| minio-go conditional-put support is unclear | Medium | Verify early. Fall back to check-then-create. |
| Interval slows long runs | Medium | User controls the interval. Document the trade-off. |
| Credentials leak | High | Env vars only. Never log secrets. |
| S3 endpoint differences | Medium | Configurable endpoint, region, path-style, and secure flags. |

## Deferred Work

- S3 multipart tuning and resumable uploads.
- Server-side encryption and lifecycle policies.
- Presigned URLs and direct-to-bucket uploads.

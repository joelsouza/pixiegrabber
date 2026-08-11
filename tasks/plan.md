# Implementation Plan: Pixiegrabber Image CLI

## Overview

Build the approved image-only Go CLI in small tested slices. Keep the undocumented Pixieset contract, browser cookie access, local state, and download execution behind separate internal packages. Use local HTTP servers and synthetic browser profiles for repeatable verification.

## Architecture Decisions

- Pin `github.com/browserutils/kooky` at `v0.2.10`. Hide its unstable API inside `internal/browsercookies`.
- Convert browser cookies into a standard `net/http/cookiejar` and retain no reusable cookie export.
- Allow only HTTPS requests to `galleries.pixieset.com` and `images.pixieset.com`. Reject redirects to other hosts.
- Use separate API and media clients. The media client receives no cookies, authorization, CSRF, Origin, or Referer headers and rejects every redirect.
- Decode third-party responses into private wire types, validate IDs and relationships, then map them to domain values.
- Persist one versioned manifest per Collection with atomic file replacement.
- Lock each output root and reject symbolic-link or reparse-point write targets.
- Treat image URLs as transient. Do not write them to manifests or logs.
- Stop before image downloads when a non-empty video response appears. Write only a sanitized diagnostic schema.

## Dependency Graph

```text
Pixieset API client ─┐
Browser cookies ─────┼─> run planner ─> image archive ─> CLI
Paths and manifest ──┘         └──────> video diagnostic stop
```

## Phases

### Phase 1: External Boundaries

- Task 1: Implement and test Pixieset listing, Set, and photo response mapping.
- Task 2: Implement and test browser selector parsing, store selection, cookie filtering, and cookie-jar creation.

### Checkpoint: Boundaries

- `go test ./internal/pixieset ./internal/browsercookies`
- No test reads a real browser profile or external network.

### Phase 2: Local State

- Task 3: Implement and test safe paths, manifest schema, loading, and atomic writes.
- Task 4: Implement and test Collection classification, missing state, and sanitized video diagnostics.

### Checkpoint: State

- `go test ./internal/paths ./internal/manifest ./internal/archive`
- Fixtures contain no user account data, cookies, signed URLs, or source names.

### Phase 3: Executable Flow

- Task 5: Implement and test bounded image downloads, SHA-256 checksums, retries, and atomic replacement.
- Task 6: Integrate flags, planning, confirmation, new and incomplete runs, synchronization, and verification.

### Checkpoint: Complete

- `go test ./...`
- `go vet ./...`
- `go build ./cmd/pixiegrabber`
- Cross-build the CLI for macOS, Linux, and Windows.
- Run the CLI against a local end-to-end fixture server.

## Evidence Path

- Claim: pagination, mapping, and validation match the captured image contracts.
  Evidence: table-driven HTTP tests with sanitized multi-page and malformed fixtures.
- Claim: credentials cannot leak to media hosts or diagnostics.
  Evidence: host-allowlist redirect tests, domain-filtered cookie tests, and recursive diagnostic redaction tests.
- Claim: partial runs resume without corrupting files.
  Evidence: injected download failures, atomic-file tests, and a second successful run against the same temporary output.
- Claim: video data stops the run before image downloads.
  Evidence: an end-to-end fixture with one video object and an image endpoint that fails the test if called.
- Claim: the source builds for all target operating systems.
  Evidence: cross-builds. Real browser decryption remains a release-time platform test outside this machine.

## Risks And Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Pixieset changes undocumented responses | High | Isolate wire types and fail with endpoint and missing-field context. |
| Browser cookie formats or encryption change | High | Pin `kooky`, isolate it, and keep profile fixtures and a release matrix. |
| Cookies leak through redirects or logs | High | Use host allowlists, a cookie jar, redacted errors, and redirect rejection tests. |
| A run is interrupted during a write | High | Write to same-directory temporary files, sync, close, and rename atomically. |
| Two processes or links redirect writes | High | Lock the output root and reject link-based write targets. |
| First run downloads excessive data | Medium | Build a complete plan and require confirmation unless `--yes` is present. |
| Current machine cannot verify every target browser and OS | Medium | Cross-build and fixture-test now; document the remaining real-profile verification gap. |

## Deferred Work

- Video response mapping and download support.
- Original image download endpoints beyond the listed `xxlarge` variant.
- Collection filtering, multiple workspaces, and scheduled runs.

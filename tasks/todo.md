# Pixiegrabber Tasks

## Task 1: Pixieset API Boundary

**Acceptance criteria:**

- [ ] Discover every Collection page and validate pagination.
- [ ] Load Sets and image records through the verified endpoints.
- [ ] Normalize protocol-relative URLs and sentinel dates without exposing wire types.

**Verify:** `go test ./internal/pixieset`

**Files:** `go.mod`, `internal/pixieset/model.go`, `internal/pixieset/client.go`, `internal/pixieset/client_test.go`

## Task 2: Browser Session Boundary

**Acceptance criteria:**

- [ ] Parse `BROWSER[:PROFILE][::CONTAINER]` and select one explicit or default store.
- [ ] Read valid Pixieset cookies only and close every discovered store.
- [ ] Create an in-memory cookie jar without printing or persisting values.

**Verify:** `go test ./internal/browsercookies`

**Files:** `internal/browsercookies/selector.go`, `internal/browsercookies/source.go`, `internal/browsercookies/source_test.go`

## Task 3: Paths And Manifest

**Acceptance criteria:**

- [ ] Create readable cross-platform path components with stable IDs.
- [ ] Load and validate schema-versioned Collection manifests.
- [ ] Replace manifests atomically and keep valid previous state after failure.

**Verify:** `go test ./internal/paths ./internal/manifest`

**Files:** `internal/paths/paths.go`, `internal/paths/paths_test.go`, `internal/manifest/manifest.go`, `internal/manifest/manifest_test.go`

## Task 4: Planning And Video Stop

**Acceptance criteria:**

- [ ] Classify new, incomplete, healthy, missing-file, and source-missing Collections.
- [ ] Build one Reference with multiple Set Placements by media ID.
- [ ] Write a useful sanitized video schema and stop before downloads.

**Verify:** `go test ./internal/archive`

**Files:** `internal/archive/plan.go`, `internal/archive/diagnostic.go`, `internal/archive/plan_test.go`, `internal/archive/diagnostic_test.go`

## Task 5: Image Download

**Acceptance criteria:**

- [ ] Download with bounded concurrency, retries, and approved-host redirects only.
- [ ] Compute SHA-256 while writing and replace destinations atomically.
- [ ] Preserve successful files and return specific failed Reference results.

**Verify:** `go test ./internal/download`

**Files:** `internal/download/downloader.go`, `internal/download/downloader_test.go`

## Task 6: CLI Flow

**Acceptance criteria:**

- [ ] Validate all approved flags and detect or override the selected browser User-Agent.
- [ ] Run plan, confirmation, download, resume, synchronization, and verification flows.
- [ ] Exit nonzero with actionable messages for authentication, video, and partial failures.

**Verify:** `go test ./... && go vet ./... && go build ./cmd/pixiegrabber`

**Files:** `cmd/pixiegrabber/main.go`, `internal/app/app.go`, `internal/app/app_test.go`, `README.md`

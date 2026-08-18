# Spec: Pixiegrabber CLI

## Objective

Pixiegrabber is a command-line tool that preserves authorized Pixieset Client Gallery Collections. It discovers Collections in the active Pixieset workspace, downloads each image Reference, and writes one normalized JSON manifest for each Collection. It stores output in a local directory or in an S3-compatible bucket.

The first version processes new and incomplete Collections by default. It can safely synchronize completed Collections without deleting local files.

## Scope

- Use the active workspace from an imported browser session.
- Store output in a local directory or in an S3-compatible bucket.
- Discover all pages from Pixieset's dashboard listings endpoint.
- Ignore Pixieset dashboard folders.
- Process all Sets and image References in each selected Collection.
- Download the largest listed image variant, starting with `path_xxlarge` and falling back through smaller named variants.
- Use one Collection-scoped Reference per Pixieset media ID.
- Create one Placement and one local file for each Set that contains a Reference.
- Detect non-empty video responses, write one sanitized diagnostic sample, and stop before image downloads.
- Use Pixieset's internal JSON endpoints directly, as recorded in [ADR 0001](./adr/0001-use-pixieset-internal-api.md).
- Never change remote Pixieset data.

The first version does not accept Collection URLs, filter Collections, download videos, retain raw API responses, or support more than the active workspace.

## Runtime

- Implementation language: Go.
- Supported operating systems: macOS and Linux.
- Supported browsers where available: Brave, Chrome, Chromium, Edge, Firefox, and Safari.
- Distribution: one Go executable.
- Browser cookie dependency: [`github.com/browserutils/kooky` `v0.2.10`](https://github.com/browserutils/kooky/releases/tag/v0.2.10), pinned behind `internal/browsercookies` because its API is not stable.
- Output locking dependency: [`github.com/gofrs/flock` `v0.13.0`](https://github.com/gofrs/flock/releases/tag/v0.13.0).
- Extended image decoder dependency: [`golang.org/x/image` `v0.44.0`](https://pkg.go.dev/golang.org/x/image@v0.44.0).
- S3-compatible storage dependency: [`github.com/minio/minio-go/v7`](https://github.com/minio/minio-go).
- Rate limiting dependency: [`golang.org/x/time`](https://pkg.go.dev/golang.org/x/time).
- Use popular, actively maintained Go libraries for common functions when they make implementation simpler or faster. Prefer them over custom code, and write custom infrastructure only when no suitable library meets the required behavior.
- The selected browser can remain open while Pixiegrabber reads cookies from a safe temporary copy of its profile data.

## Command

```sh
pixiegrabber \
  --cookies-from-browser 'firefox[:PROFILE][::CONTAINER]' \
  --output ./references
```

S3 mode:

```sh
export PIXIEGRABBER_S3_ACCESS_KEY=...
export PIXIEGRABBER_S3_SECRET_KEY=...
pixiegrabber \
  --cookies-from-browser 'firefox[:PROFILE][::CONTAINER]' \
  --s3 \
  --s3-endpoint localhost:9000 \
  --s3-bucket references
```

Required flags:

- `--cookies-from-browser BROWSER[:PROFILE][::CONTAINER]`: Import the active Pixieset session. A browser name alone selects its default profile.
- `--output DIR`: Select the local output root. Required unless `--s3` is set.

Optional flags:

- `--sync-existing`: Refresh completed Collections and represent remote removals without deleting local files.
- `--verify`: Check every local Placement against its saved SHA-256 checksum and restore missing or changed files.
- `--yes`: Accept the download plan without an interactive prompt.
- `--concurrency N`: Set concurrent Reference downloads. The default is `4`.
- `--user-agent VALUE`: Override the User-Agent detected from the selected browser and its installed version.
- `--interval DURATION`: Set the minimum interval between Pixieset API calls. The default is `0`, which disables throttling. Use a value such as `2s` to avoid flooding the Pixieset servers.
- `--s3`: Store output in an S3-compatible bucket instead of a local directory.
- `--s3-endpoint HOST[:PORT]`: The S3-compatible endpoint without a scheme.
- `--s3-bucket NAME`: The bucket name. The bucket must already exist.
- `--s3-region REGION`: The region. The default is `us-east-1`.
- `--s3-path-style`: Use path-style addressing. The default is `true`.
- `--s3-secure`: Use HTTPS for the S3 endpoint. The default is `true`.

Build and verification commands:

```sh
go build ./cmd/pixiegrabber
go test ./...
go vet ./...
```

## Verified API Contracts

Pixiegrabber maps Pixieset's API term `gallery` to the domain term **Set**.

- `GET /api/v1/dashboard_listings?page={page}` returns Collection summaries and pagination metadata.
- `GET /api/v1/collections/{collection_id}/galleries` returns the Sets in one Collection. Each Set includes its ID, Collection ID, name, description, image count, and video count.
- `GET /api/v1/galleries/{set_id}?expand=photos.starred%2Cvideos` returns one Set with `photos` and `videos` arrays.
- An image record includes its media ID, Collection ID, Set ID, name, MIME type, extension, byte size, dimensions, rank, capture date, and named media variants through `path_xxlarge`.

The API origin is exactly `https://galleries.pixieset.com` on the default HTTPS port. The media origin is exactly `https://images.pixieset.com` on the default HTTPS port. API and media clients reject redirects. The media client has no cookie jar and sends no authorization, CSRF, Origin, or Referer header.

Media URLs can be protocol-relative. Pixiegrabber normalizes them to HTTPS before validation. Pixieset can return invalid sentinel dates, including zero dates and negative-year timestamps. Pixiegrabber normalizes these values to JSON `null` instead of failing the Collection.

Only documented normalized fields cross from `internal/pixieset` into the manifest model. Unknown API fields are ignored. Missing IDs or invalid Collection and Set relationships are errors. Responses, page counts, and decoded diagnostic structures have explicit size limits.

## Run Behavior

1. Import cookies from the selected browser without changing its profile.
2. Detect a matching desktop User-Agent unless the user supplied one.
3. Discover every dashboard-listing page and deduplicate Collections by Pixieset ID.
4. Classify Collections as new, incomplete, complete, or missing locally expected files.
5. Load every Set in every selected Collection to build a complete plan before starting any image worker.
6. If a non-empty `videos` array appears, write one sanitized sample to `<output>/pixiegrabber-unsupported-video.json` and exit nonzero before image downloads.
7. Show planned Collection, image Reference, Placement file, and source-byte counts. Require confirmation unless `--yes` is present.
8. Process new and incomplete Collections. Restore missing local files. Skip other completed Collections unless `--sync-existing` is present.
9. Download with four workers by default. Use bounded retries with backoff and honor `Retry-After`. When `--interval` is set, wait at least that long between every Pixieset API call.
10. Persist resumable manifest state with atomic writes.
11. Exit nonzero when authentication, discovery, or any Reference fails.

An empty Collection gets a manifest and a completed state.

If a Reference download fails, Pixiegrabber keeps successful files, records the failure, and resumes it on the next normal run.

## Synchronization

`--sync-existing` applies these rules:

- Match Collections, Sets, and References by their Pixieset IDs, not their names.
- Rename local paths when a source name changes.
- When a Reference moves, mark its old Placement missing and create a new present Placement in the destination Set. Retain the old file.
- Add a Placement when one Reference appears in another Set.
- Retain and mark a Placement missing when it disappears from a Set.
- Retain and mark a Reference missing when it has no source Placements.
- Retain and mark a Collection missing when it no longer appears in dashboard discovery.
- Download and hash every present Reference during synchronization. Replace changed content atomically and retain unchanged files.
- Never delete local Collection, Set, Reference, or Placement data automatically.

A normal run checks that expected Placement files exist. It does not hash every file unless `--verify` is present. If an incomplete Placement disappears before resumption, mark it missing and continue instead of retrying it forever.

Variant fallback occurs when a higher variant field is empty or its media request returns `404` or `410`. Authentication errors, malformed media, and other request failures do not select a lower variant.

## Local Layout

```text
<output>/
  <collection-name>--<collection-id>/
    collection.json
    <set-name>--<set-id>/
      <reference-name>--<media-id>.<extension>
```

Pixiegrabber permits only one process to use an output root at a time. Reliable locking and replacement require a local filesystem. Existing symbolic-link and reparse-point components are rejected, and Go's rooted filesystem API prevents path races from escaping the opened output root.

On macOS and Linux, output directories use mode `0700` and output files use mode `0600`.

Names remain readable. Path generation normalizes Unicode and replaces characters that are unsafe on macOS or Linux. Stable Pixieset IDs prevent collisions and preserve identity after renames.

Each Collection is self-contained. If the same media appears in two Collections, each Collection stores its own Reference files. If one Reference appears in two Sets of one Collection, each Set stores its own Placement file.

## S3 Mode

S3 mode mirrors the local layout as object keys:

```text
<collection-name>--<collection-id>/
  collection.json
  <set-name>--<set-id>/
    <reference-name>--<media-id>.<extension>
```

- Read the access key and secret key from `PIXIEGRABBER_S3_ACCESS_KEY` and `PIXIEGRABBER_S3_SECRET_KEY`. Never pass them as flags or log them.
- The bucket must already exist. Pixiegrabber does not create it.
- Each object stores its SHA-256 checksum as metadata and in the manifest.
- A lock object `.pixiegrabber.lock` prevents two processes from using the bucket at once. A stale lock older than 10 minutes is replaced.
- `--verify` uses the stored checksum metadata when it matches the manifest, and re-downloads otherwise.
- The video-stop diagnostic is written to the bucket root in S3 mode.

## Manifest

Each Collection has one human-readable `collection.json` file with a schema version.

The manifest stores:

- Collection ID, name, description, source dates, presence state, and run state.
- Set IDs, names, descriptions, source order, dates, and presence states.
- Reference media IDs, names, descriptions, source order, dates, media type, original filename, dimensions, duration, MIME type, selected quality, and SHA-256 checksum when available.
- Placement Set IDs, relative local paths, presence states, download states, and installed SHA-256 checksums.
- Last discovery, attempt, success, and verification times in UTC.
- Sanitized failure information needed for resumption.

The manifest does not store raw source payloads, source selection state, source links, passwords, download PINs, cookies, CSRF values, authorization headers, or other secrets.

Manifest updates use a same-directory temporary file and replacement that is atomic against process interruption on supported local filesystems. A terminated process must leave either the previous valid manifest or the next valid manifest. Power-loss durability is not guaranteed.

## Unsupported Video Diagnostic

- Do not download videos in the first version.
- Stop at the first non-empty `videos` response before image downloads begin.
- Write one sanitized value-shaped JSON sample to `<output>/pixiegrabber-unsupported-video.json`.
- Preserve field names, JSON value types, and allowlisted media facts. Replace all other scalar values with type-compatible placeholders. Remove URL queries, fragments, user information, and personal path segments.
- Create the diagnostic with owner-only permissions where the operating system supports them.
- Print the diagnostic path and exit nonzero so a later implementation session can add the verified video contract.
- Never log or persist the raw video object.

## Project Structure

```text
cmd/pixiegrabber/        CLI entry point
internal/browsercookies Browser profile discovery, cookie reading, and User-Agent detection
internal/pixieset/       Isolated internal API client and response mapping
internal/archive/        Collection classification and synchronization
internal/download/       Image download execution
internal/manifest/       Versioned manifest model and atomic persistence
internal/paths/          Cross-platform readable path generation
internal/store/          Storage abstraction: local and S3-compatible backends
internal/throttle/       Polite rate limiting for Pixieset API calls
internal/testdata/       Sanitized API and media fixtures
docs/                    Domain, specification, and architecture documents
```

## Code Style

Use small internal packages and typed domain values where they prevent ID mix-ups. Keep API response types inside `internal/pixieset`; do not expose undocumented response shapes to archive logic.

```go
type Reference struct {
	ID         string      `json:"id"`
	MediaType  MediaType   `json:"media_type"`
	SHA256     string      `json:"sha256,omitempty"`
	Placements []Placement `json:"placements"`
}
```

Format code with `gofmt`. Return errors with operation and subject context. Do not log secret-bearing request data.

## Testing Strategy

- Unit tests cover pagination, cookie selector parsing, path safety, manifest migration, Collection classification, Placement identity, rename handling, missing-source state, and partial-failure resumption.
- HTTP integration tests use `httptest.Server` and sanitized fixtures for multiple listing pages, images, video detection, authentication failures, malformed responses, retries, and rate limits.
- Diagnostic tests prove that video payloads are structurally useful and do not retain secrets, personal strings, or signed URL queries.
- End-to-end tests run the CLI against a local fixture server and verify the complete output tree and manifests.
- Platform tests build for macOS and Linux. Browser-cookie tests use synthetic profile databases and platform-specific decryption seams; they never use a developer's real profile in automated tests.
- A manual isolated-browser check validates the current Pixieset endpoint contracts before release.

## Boundaries

Always:

- Validate flags, output paths, IDs, HTTP status, content type, response shape, and downloaded files.
- Keep cookies scoped to verified Pixieset hosts through an HTTP cookie jar.
- Use temporary files and atomic replacement for media and manifests.
- Lock the output root and reject symbolic-link or reparse-point write targets.
- Preserve successful work after partial failure.
- Sanitize fixtures, manifests, errors, and logs.

Ask first:

- Change the manifest in a backward-incompatible way.
- Add an API request that changes remote state.
- Add Collection filters, multiple-workspace support, or scheduled operation.

Never:

- Commit or persist cookies, tokens, passwords, PINs, authorization headers, or unsanitized API payloads.
- Delete local files because Pixieset no longer lists them.
- Send session credentials to an unverified host.
- Modify browser profile data.
- Infer undocumented endpoint contracts without a captured and sanitized example.

## Success Criteria

- The CLI discovers all pages of Collections in the active workspace with an authorized browser session.
- A first run shows a plan and downloads all confirmed new Collections.
- Images use the largest listed quality and preserve every Set Placement.
- The first video response produces a sanitized diagnostic and stops the run before image downloads.
- Output follows the documented hierarchy and each Collection has one valid normalized manifest.
- S3 mode stores References, manifests, and the video-stop diagnostic in the bucket with the documented key layout.
- `--interval` throttles every Pixieset API call and honors `Retry-After` on 429 responses.
- A failed Reference does not discard successful work and resumes on the next normal run.
- A normal run skips healthy completed Collections and restores missing Placement files.
- `--sync-existing` handles additions, renames, moves, repeated Placements, changed content, and source removals without local deletion.
- `--verify` detects and restores missing or checksum-mismatched files.
- Browser cookie import works while supported browsers are open on macOS and Linux.
- Automated tests contain no account data or secrets and `go test ./...` and `go vet ./...` pass.

## Open Questions

- Capture a sanitized non-empty `videos` response in a later implementation session.

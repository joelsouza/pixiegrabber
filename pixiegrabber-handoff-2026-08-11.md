# Pixiegrabber Handoff

## Context

Continue work in `/Users/joelsouza/Code/craftAM/pixiegrabber`.

The user wants a secure, cross-platform Go CLI that downloads authorized Pixieset image References. The user explicitly requires popular, maintained libraries when they make implementation simpler or faster. Do not add custom infrastructure when the standard library or a suitable package meets the required behavior.

Read these project artifacts instead of reconstructing requirements:

- `CONTEXT.md`: required domain language.
- `docs/spec.md`: current approved behavior and dependency policy.
- `docs/adr/0001-use-pixieset-internal-api.md`: API decision.
- `tasks/plan.md`: implementation plan.
- `tasks/todo.md`: acceptance criteria and task state.

All repository files are currently untracked. `git diff` does not show their contents. Use file reads and `git status --short`. Do not commit unless the user asks.

## Current Decisions

- Go version: `1.25.0`.
- Use Go 1.25 `os.Root` for output-root containment.
- Use `github.com/gofrs/flock v0.13.0` for the real process lock.
- Use `golang.org/x/image v0.44.0` for WebP and TIFF decoding.
- Keep `golang.org/x/sys v0.47.0` for the Windows ACL implementation.
- Keep `github.com/browserutils/kooky v0.2.10` and `github.com/ncruces/go-sqlite3 v0.35.3`.
- Do not use `github.com/google/renameio/v2`. Source review found that its write API is Unix-only and cannot operate through `os.Root`. It was removed from `go.mod` and `go.sum`.
- Keep custom Pixieset retry and image-variant fallback logic because generic retry packages do not match the required policy.
- Output locking and replacement require a local filesystem.
- Replacement guarantees old-or-complete-new content against process interruption. It does not promise power-loss durability.
- On Windows, the output root has a protected current-user OICI DACL. New descendants inherit current-user-only access.

## Implemented And Verified

Existing completed packages include:

- `internal/pixieset`: API client, pagination, response limits, mapping, retries, and media URL validation.
- `internal/paths`: stable-ID, cross-platform path generation.
- `internal/manifest`: model, validation, migration, and current string-path persistence.
- `internal/browsercookies`: browser selection, safe profile snapshots, cookies, and User-Agent detection.
- `internal/archive`: accepted planner and sanitized unsupported-video diagnostic.
- `internal/download`: initial image downloader. It still has required review fixes listed below.
- `internal/privatefs`: owner-only Unix creation and protected current-user Windows DACL creation/restriction.
- `internal/outputfs`: new locked and rooted output filesystem boundary.

Recent security work:

- `privatefs` now rejects Windows UNC paths, device namespaces, alternate data streams, unsafe leaf names, and unsafe temporary patterns.
- Unix `privatefs.Restrict` now uses no-follow handles, identity checks, and handle-based chmod.
- Browser cookie snapshots now use `privatefs.MkdirTemp`, `privatefs.OpenNew`, and `privatefs.Restrict`.
- `outputfs.Open` creates/restricts the output root, opens `os.Root`, and acquires a persistent `.pixiegrabber.lock` with `flock`.
- `outputfs` provides `DisplayPath`, `MkdirAll`, `Inspect`, `OpenRegular`, `TempFile`, `AtomicReplace`, and `Remove`.
- `outputfs` explicitly rejects existing symlink/reparse components and pins intermediate directories with `Root.OpenRoot` plus identity checks.
- Corrective tests cover volume-root rejection before permission mutation, lock path types, missing parent inspection, temporary cleanup identity, callback errors, and link rejection.

Last reported verification passed:

```sh
env -u GOROOT go test -race -count=1 ./internal/privatefs
env -u GOROOT go test -race -count=1 ./internal/browsercookies
env -u GOROOT go test -race -count=1 ./internal/outputfs
env -u GOROOT go vet ./internal/privatefs ./internal/browsercookies ./internal/outputfs
env -u GOROOT go test ./...
env -u GOROOT go mod verify
```

Linux builds and Windows amd64 test compilation passed. Real Windows ACL behavior is not verified yet.

## Work Stopped Here

No production package uses `outputfs` yet. The next migration was prepared but not started.

The final read-only Oracle audit of `internal/outputfs` completed after work stopped. It made no file changes. Fix these findings before production integration:

1. Reserve `.pixiegrabber.lock` from all public mutations. `AtomicReplace` and `Remove` currently accept it, which can let two processes lock different inodes.
2. Add operation lifecycle synchronization. Public operations must hold a shared guard. `Close` must wait with an exclusive guard, close rooted handles, and release the flock last. The current `Close` can unlock while an `AtomicReplace` callback still runs.
3. Use a fixed short atomic temporary prefix such as `.pixiegrabber-tmp-`. The current prefix includes the full target leaf and fails for valid 255-byte names.
4. Set exact `0700` permissions on newly created descendant directories before opening them. A restrictive umask can currently create an unusable mode `0000` directory.
5. Replace `sync.Once` close caching with guarded state that permits another `Close` attempt when `flock.Close` fails and retains its lock handle.

Add tests for lock-name mutation rejection, concurrent `Close` waiting for a blocked operation, maximum-length targets, restrictive-umask nested directory creation, retried flock unlock failure, and recovered callback panic cleanup.

Optional simplification from the audit:

- Consolidate `ensureDirectories` and `pinParent` into one create-or-open traversal.
- Remove duplicate validation and traversal in `TempFile` and `AtomicReplace`.
- Use deferred temporary cleanup so a recovered callback panic leaves no file or descriptor.
- Consider ignoring close errors from read-only pinned child roots on supported Go 1.25 platforms. Do not remove component pinning, identity comparisons, final `Lstat` checks, absolute-component checks, or identity-aware cleanup.

The current `internal/outputfs/outputfs.go` is about 747 lines. Do not simplify it by removing the explicit link/reparse and identity checks. Look only for behavior-preserving consolidation or concrete unnecessary helpers.

## Next Steps

1. Fix the five required `outputfs` audit findings and add the listed regression tests. Apply the optional traversal consolidation only if it clearly reduces code while preserving all identity and link checks. Run focused race, vet, Windows compile, and full tests.
2. Migrate unsupported-video diagnostics:
   - Change `archive.CheckVideos` to accept `*outputfs.FS` instead of an output-root string.
   - Use `outputfs.AtomicReplace` for `pixiegrabber-unsupported-video.json`.
   - Use `DisplayPath` only for the returned user-facing diagnostic path.
   - Delete duplicated root canonicalization, directory creation, link checks, permission helpers, and replacement code from `internal/archive/diagnostic.go`.
   - Delete `internal/archive/diagnostic_replace_unix.go` and `internal/archive/diagnostic_replace_windows.go` after tests pass.
3. Migrate manifests:
   - Change `manifest.Load` and `manifest.Write` to accept `*outputfs.FS` plus a slash-separated relative manifest path.
   - Use `OpenRegular` and `AtomicReplace`.
   - Preserve `ErrNotFound`, size limits, strict JSON decoding, validation, and readable JSON formatting.
   - Delete `internal/manifest/replace_unix.go` and `internal/manifest/replace_windows.go` after tests pass.
4. Convert archive planning records from absolute output paths to root-relative slash-separated paths. Remove duplicate path containment and Windows reparse reflection from `internal/archive/plan.go`.
5. Fix and migrate the downloader:
   - Planner destinations include Collection, Set, and Reference components. Current downloader validation omits the Collection component.
   - Bound every media response by both the source byte hint and a hard absolute cap.
   - Validate full image data with `image.Decode`, not only header/prefix checks.
   - Register stdlib GIF/JPEG/PNG and blank-import `x/image/webp` and `x/image/tiff`.
   - Enforce encoded bytes, width, height, pixel count, and decoded-memory limits before accepting content.
   - AVIF is not supported by stdlib or `x/image v0.44.0`; do not add an immature decoder without user direction.
   - Use `outputfs.TempFile` for staging and `outputfs.AtomicReplace` for each Placement.
   - Delete downloader replacement and absolute-path helper files only after migration tests pass.
6. Implement `internal/app` and `cmd/pixiegrabber`. Open and lock `outputfs` before API discovery. Call `CheckVideos` before creating image work or starting workers.
7. Add real Windows CI for ACL shape, inherited child access, junctions, alternate data streams, lock behavior, and replacement sharing violations.

## Important Constraints

- Never persist or log cookies, tokens, signed URL queries, raw API payloads, passwords, or PINs.
- Signed media URLs can exist only in memory for requests.
- The first non-empty video response must write one sanitized diagnostic and stop before image downloads.
- Do not delete local References or Placements because Pixieset no longer lists them.
- Reject output writes through existing symbolic links and Windows reparse points.
- Use `DisplayPath` for messages only. Do not perform I/O through its returned string.
- Use slash-separated relative paths at the `outputfs` boundary.
- Do not use a lock-file existence check as a lock. The persistent file is normal; `flock` owns lock state.
- Do not remove a stale-looking lock file.
- Existing `GOROOT=/Users/joelsouza/.asdf/installs/golang/1.25.0/go` is stale. Prefix Go commands with `env -u GOROOT`.
- Use `apply_patch` for manual file edits.

## Residual Risks

- Windows ACL tests only cross-compile on this Mac. Run them on real Windows.
- Reliable lock and rename semantics are not guaranteed on network filesystems.
- Public Go APIs cannot prove that no internal link was ever traversed during every race. `os.Root` prevents escape from the opened root, and `outputfs` rejects observed link/reparse components.
- Browser keychain and live profile behavior still need manual platform checks.
- Future video support still needs a sanitized real video response sample.

## Suggested Skills

- `using-agent-skills`: select applicable workflows at session start.
- `incremental-implementation`: migrate one package at a time and keep the suite green.
- `test-driven-development`: preserve RED evidence for behavior changes.
- `verification-planning`: define focused checks before each migration slice.
- `security-and-hardening`: review path, file, cookie, and HTTP trust boundaries.
- `api-and-interface-design`: keep the `outputfs`, manifest, archive, and downloader contracts hard to misuse.
- `simplify`: remove old duplicate filesystem code only after the replacement integration passes.
- `code-review-and-quality`: run a final multi-axis review before CLI completion or merge.

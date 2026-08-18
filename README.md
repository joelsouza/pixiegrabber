# Pixiegrabber

Pixiegrabber preserves authorized Pixieset Client Gallery Collections locally or in an S3-compatible bucket. It discovers the Collections in the active Pixieset workspace, downloads each image Reference, and writes one normalized JSON manifest for each Collection.

## Domain language

- **Collection**: a top-level Pixieset client gallery that contains zero or more Sets.
- **Set**: a named group of References within a Collection.
- **Reference**: a Pixieset gallery image preserved locally together with the metadata that identifies and describes its source.
- **Placement**: the membership of a Reference in a Set. Each Placement has its own local file path, so one Reference can appear in multiple Set folders.

## Requirements

- Go 1.25 or newer.
- An active Pixieset session in a supported browser (Brave, Chrome, Chromium, Edge, Firefox, or Safari).
- An S3-compatible bucket and credentials when using S3 mode.

## Usage

Local mode:

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

### Required flags

- `--cookies-from-browser BROWSER[:PROFILE][::CONTAINER]`: import the active Pixieset session. A browser name alone selects its default profile.
- `--output DIR`: select the local output root. Required unless `--s3` is set.

### Optional flags

- `--sync-existing`: refresh completed Collections and represent remote removals without deleting local files.
- `--verify`: check every Placement against its saved SHA-256 checksum and restore missing or changed files.
- `--yes`: accept the download plan without an interactive prompt.
- `--concurrency N`: set concurrent Reference downloads. The default is `4`.
- `--user-agent VALUE`: override the User-Agent detected from the selected browser and its installed version.
- `--interval DURATION`: set the minimum interval between Pixieset API calls. The default is `0`, which disables throttling. Use a value such as `2s` to avoid flooding the Pixieset servers.
- `--s3`: store output in an S3-compatible bucket instead of a local directory.
- `--s3-endpoint HOST[:PORT]`: the S3-compatible endpoint without a scheme.
- `--s3-bucket NAME`: the bucket name. The bucket must already exist.
- `--s3-region REGION`: the region. The default is `us-east-1`.
- `--s3-path-style`: use path-style addressing. The default is `true`.
- `--s3-secure`: use HTTPS for the S3 endpoint. The default is `true`.

## Local layout

```text
<output>/
  <collection-name>--<collection-id>/
    collection.json
    <set-name>--<set-id>/
      <reference-name>--<media-id>.<extension>
```

Only one process may use an output root at a time. The output root is locked, and symbolic-link and reparse-point components are rejected.

## S3 layout

S3 mode mirrors the local layout as object keys:

```text
<collection-name>--<collection-id>/
  collection.json
  <set-name>--<set-id>/
    <reference-name>--<media-id>.<extension>
```

- Read the access key and secret key from `PIXIEGRABBER_S3_ACCESS_KEY` and `PIXIEGRABBER_S3_SECRET_KEY`. Never pass them as flags.
- The bucket must already exist. Pixiegrabber does not create it.
- Each object stores its SHA-256 checksum as metadata and in the manifest.
- A lock object `.pixiegrabber.lock` prevents two processes from using the bucket at once. A stale lock older than 10 minutes is replaced.
- `--verify` uses the stored checksum metadata when it matches the manifest, and re-downloads otherwise.

## Video stop

Pixiegrabber does not download videos in this version. When a Set contains a non-empty `videos` array, it writes one sanitized value-shaped sample to `<output>/pixiegrabber-unsupported-video.json` (or the bucket root in S3 mode), prints the diagnostic path, and exits nonzero before any image download begins.

## Build and test

```sh
go build ./cmd/pixiegrabber
go test ./...
go vet ./...
```

## Security

Pixiegrabber never logs or persists cookies, tokens, signed URL queries, raw API payloads, passwords, PINs, or S3 credentials. It never deletes local Collection, Set, Reference, or Placement data automatically.
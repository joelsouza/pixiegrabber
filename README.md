# Pixiegrabber

Pixiegrabber preserves authorized Pixieset Client Gallery Collections locally. It
discovers the Collections in the active Pixieset workspace, downloads each image
Reference, and writes one normalized JSON manifest for each Collection.

## Domain language

- **Collection**: a top-level Pixieset client gallery that contains zero or more
  Sets.
- **Set**: a named group of References within a Collection.
- **Reference**: a Pixieset gallery image preserved locally together with the
  metadata that identifies and describes its source.
- **Placement**: the membership of a Reference in a Set. Each Placement has its
  own local file path, so one Reference can appear in multiple Set folders.

## Requirements

- Go 1.25 or newer.
- An active Pixieset session in a supported browser (Brave, Chrome, Chromium,
  Edge, Firefox, or Safari).

## Usage

```sh
pixiegrabber \
  --cookies-from-browser 'firefox[:PROFILE][::CONTAINER]' \
  --output ./references
```

### Required flags

- `--cookies-from-browser BROWSER[:PROFILE][::CONTAINER]`: import the active
  Pixieset session. A browser name alone selects its default profile.
- `--output DIR`: select the local output root. Pixiegrabber has no implicit
  output directory.

### Optional flags

- `--sync-existing`: refresh completed Collections and represent remote removals
  without deleting local files.
- `--verify`: check every local Placement against its saved SHA-256 checksum and
  restore missing or changed files.
- `--yes`: accept the download plan without an interactive prompt.
- `--concurrency N`: set concurrent Reference downloads. The default is `4`.
- `--user-agent VALUE`: override the User-Agent detected from the selected
  browser and its installed version.

## Local layout

```text
<output>/
  <collection-name>--<collection-id>/
    collection.json
    <set-name>--<set-id>/
      <reference-name>--<media-id>.<extension>
```

Only one process may use an output root at a time. The output root is locked, and
symbolic-link and reparse-point components are rejected.

## Video stop

Pixiegrabber does not download videos in this version. When a Set contains a
non-empty `videos` array, it writes one sanitized value-shaped sample to
`<output>/pixiegrabber-unsupported-video.json`, prints the diagnostic path, and
exits nonzero before any image download begins.

## Build and test

```sh
go build ./cmd/pixiegrabber
go test ./...
go vet ./...
```

## Security

Pixiegrabber never logs or persists cookies, tokens, signed URL queries, raw API
payloads, passwords, or PINs. It never deletes local Collection, Set, Reference,
or Placement data automatically.

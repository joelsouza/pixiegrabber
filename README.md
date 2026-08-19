# Pixiegrabber

Pixiegrabber downloads images and videos from Pixieset Client Gallery Collections that you can access. It stores the images on your computer or in an S3-compatible bucket. It also writes a JSON manifest for each Collection.

## Terms

This README uses the following terms:

- Collection: one Pixieset client gallery. A Collection can contain Sets.
- Set: a named group of References in a Collection.
- Reference: an image or video and the metadata that describes its source.
- Placement: the location of a Reference in a Set. One Reference can have a Placement in more than one Set.

## Requirements

You need:

- Go 1.25 or later.
- A signed-in Pixieset session in Brave, Chrome, Chromium, Edge, Firefox, or Safari.
- An S3-compatible bucket and its credentials if you use S3 storage.

## Usage

To save files on your computer:

```sh
pixiegrabber --output ./references
```

To save files in an S3-compatible bucket:

```sh
export PIXIEGRABBER_S3_ACCESS_KEY=...
export PIXIEGRABBER_S3_SECRET_KEY=...
pixiegrabber \
  --s3 \
  --s3-endpoint localhost:9000 \
  --s3-bucket references
```

Pixiegrabber finds your Pixieset session for you. It looks in each supported browser, and it selects the profile and the Firefox container that hold the session. You do not have to close the browser first.

Pixiegrabber prints its choice, and the value that selects it again:

```
Using chrome profile "Personal" (5 session cookies, 19 Pixieset cookies).
Override with --cookies-from-browser 'chrome:Personal'.
```

If it finds no session, it lists each profile that it examined:

```
no valid Pixieset cookies found; sign in to Pixieset and retry
  chrome:Personal — 0 Pixieset cookies
  firefox:default-release — 3 Pixieset cookies, no session cookie
```

Copy one of those names into `--cookies-from-browser` to select that profile.

### Required flags

- `--output DIR` sets the output directory. You must use this flag unless you use `--s3`.

### Optional flags

- `--cookies-from-browser [BROWSER[:PROFILE][::CONTAINER]]` limits the search for your Pixieset session. Use `chrome` to search one browser. Use `chrome:Profile 1` to select one profile, by name or by directory path. Use `firefox:default-release::Work` to select one Firefox container. Use `::none` for cookies that belong to no container.
- `--sync-existing` checks completed Collections again. It records Collections, Sets, References, and Placements that are no longer in Pixieset. It does not delete saved files.
- `--verify` checks each Placement against its saved SHA-256 checksum. Pixiegrabber downloads a file again if the file is missing or has changed.
- `--videos` downloads the videos in each Collection. Videos download at up to 1080p. Without this flag, Pixiegrabber stops at the first video it cannot plan. A video that Pixieset cannot serve becomes a missing Reference, and the run still completes.
- `--yes` accepts the download plan without a prompt.
- `--quiet` hides the progress lines. Pixiegrabber still writes the run log.
- `--concurrency N` sets the number of References that Pixiegrabber can download at the same time. The default is `4`.
- `--user-agent VALUE` sets the User-Agent header. If you do not use this flag, Pixiegrabber gets the value from the selected browser.
- `--interval DURATION` sets the minimum time between Pixieset API and image requests. The default is `0`, which turns this delay off. Use a value such as `2s` to reduce the request rate.
- `--s3` stores the output in an S3-compatible bucket.
- `--s3-endpoint HOST[:PORT]` sets the S3-compatible endpoint without a scheme.
- `--s3-bucket NAME` sets the bucket name. The bucket must exist before you run Pixiegrabber.
- `--s3-region REGION` sets the region. The default is `us-east-1`.
- `--s3-path-style` uses path-style S3 addresses. The default is `true`.
- `--s3-secure` uses HTTPS for the S3 endpoint. The default is `true`.

## Local file layout

Pixiegrabber uses this directory structure:

```text
<output>/
  pixiegrabber-run.log
  <collection-name>--<collection-id>/
    collection.json
    <set-name>--<set-id>/
      <reference-name>--<media-id>.<extension>
```

Pixiegrabber locks the output directory while it runs. A second process cannot use the same directory at the same time. Pixiegrabber rejects symbolic links and reparse points in the output path.

## The run log

A large account takes a long time to read. Pixiegrabber shows what it does while it works:

```
Discovering collections: page 3/3, 60 found.
[ 1/60] Example Collection: 2 sets, 131 images
[ 2/60] Another Collection: 1 set, 200 images
Downloading: 40/131 done, 0 failed.
```

It also writes `pixiegrabber-run.log` at the output root, with one JSON object for each line. Use it to see what a finished run did:

```sh
jq -c 'select(.ev == "collection")' pixiegrabber-run.log | tail
jq -c 'select(.ev == "download_failed")' pixiegrabber-run.log
jq -c 'select(.ev == "run_end")' pixiegrabber-run.log
```

The log holds counts, identifiers and states only. It never holds a URL, a cookie, a token or an S3 key. Each run replaces the log of the run before it.

Press `Ctrl-C` to stop. Pixiegrabber saves the log, releases the lock, and exits. Run it again to continue.

## S3 object layout

Pixiegrabber uses the same names for S3 object keys:

```text
<collection-name>--<collection-id>/
  collection.json
  <set-name>--<set-id>/
    <reference-name>--<media-id>.<extension>
```

- Set `PIXIEGRABBER_S3_ACCESS_KEY` and `PIXIEGRABBER_S3_SECRET_KEY` before you run Pixiegrabber. Do not put these values in command-line flags.
- Create the bucket before you run Pixiegrabber. Pixiegrabber does not create it.
- Pixiegrabber stores the SHA-256 checksum in the object metadata and in the manifest.
- The `.pixiegrabber.lock` object prevents two processes from using the bucket at the same time. Pixiegrabber replaces this object if it is more than 10 minutes old.
- With `--verify`, Pixiegrabber uses the checksum in the object metadata when it matches the manifest. Otherwise, Pixiegrabber downloads the file again.

## Build and test

```sh
go build ./cmd/pixiegrabber
go test ./...
go vet ./...
```

## Security

Pixiegrabber does not write cookies, tokens, signed URL queries, raw API responses, passwords, PINs, or S3 credentials to logs or saved files. It does not automatically delete saved Collection, Set, Reference, or Placement data.

# crawlping

Submit URLs to IndexNow from a machine you control.

Sites deployed on Cloudflare (Workers, Pages) call `crawlping` from their own
code. The notification leaves from **your** IP instead of Cloudflare's shared
egress range, which is what `429 Too Many Requests` from `api.indexnow.org`
turns out to be about.

```
your Worker --HTTPS--> crawlping (your VPS) --> api.indexnow.org
                                                      ^
                                            egress is your IP
```

## Why not just use Cloudflare Crawler Hints?

Crawler Hints is free and it does notify IndexNow. Use it if you do not care
what gets sent. Use `crawlping` when you do:

| | Crawler Hints | crawlping |
|---|---|---|
| What triggers a ping | cache `MISS` heuristic | your code, explicitly |
| What was sent | invisible | your logs and the HTTP response |
| Manual submission | no | yes |
| Exclude one URL | only per-page `noindex` | just do not send it |
| Failure visibility | none | engine status per request |

Crawler Hints guesses that content changed because a cache entry missed. For a
Workers/SSR site that signal is noisy in both directions.

## Install

Three ways in. The binary and the container are built from the same Go
version, and CI fails the tag if `go.mod` and the Dockerfile ever disagree.

```bash
# Container (linux/amd64 and linux/arm64)
docker pull ghcr.io/rokcso/crawlping:latest

# Plain binary, no Docker: pick your platform from
#   https://github.com/rokcso/crawlping/releases
# each archive ships with a checksums.txt entry

# From source
go install github.com/rokcso/crawlping@latest
```

> **GHCR packages are private by default.** Until the package is switched to
> public in its GitHub settings, `docker pull` returns 401. The image exists,
> but only the account that pushed it can read it.

## Quick start

Generate a token, then write `config.yaml` (mode `0600`):

```bash
openssl rand -hex 32
```

Pick the shape you are deploying first, because it decides one line of that
file: `listen:`. Everything below assumes the config is at `./config.yaml`.

### Docker

In a container the service must bind every interface; who may reach it is the
host's decision, not the container's.

```yaml
listen: "0.0.0.0:8080"        # all interfaces *inside* the container

sites:
  - host: example.com
    indexnow_key: your-indexnow-key      # also served at /<key>.txt
    token: the-token-you-generated
```

```bash
docker run -d --name crawlping \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro" \
  ghcr.io/rokcso/crawlping
```

Both halves are required, and they do different jobs. `-p` forwards to the
container's **eth0** address, never to its loopback, so a container left on the
shipped `127.0.0.1:8080` is unreachable from the host even with
`-p 8080:8080` - and it still reports healthy, because its health probe dials
the same address the server is bound to. The `0.0.0.0` belongs to the container.
"Not reachable from anywhere else" belongs to the host, and it is carried by the
`127.0.0.1:` prefix in `-p 127.0.0.1:8080:8080`: the API answers on
`http://127.0.0.1:8080` from this machine and from nowhere else. Write
`-p 8080:8080` only if other machines really should reach it.

`docker-compose.yaml` ships this shape:

```bash
docker compose up -d
```

### No Docker

Run the binary directly and the shipped default is already correct - there is no
container boundary to cross:

```yaml
listen: "127.0.0.1:8080"

sites:
  - host: example.com
    indexnow_key: your-indexnow-key
    token: the-token-you-generated
```

```bash
crawlping serve -config config.yaml
```

### Behind a Cloudflare Tunnel

Publish no port at all and let the tunnel carry inbound traffic: cloudflared
reaches `http://127.0.0.1:8080`, so the shipped `listen: "127.0.0.1:8080"` is
correct here too. The commented block in `docker-compose.yaml` is this shape.
Only the **inbound** path goes through Cloudflare - outbound to IndexNow still
leaves from your machine, which is the whole point.

### Check it before trusting it

This reads the config and fetches every key URL; it does not care which shape
you deployed:

```bash
docker run --rm -v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro" \
  ghcr.io/rokcso/crawlping check -config /etc/crawlping/config.yaml
```

```
ok    example.com                      https://example.com/your-indexnow-key.txt
```

## API

### `POST /submit`

```http
Authorization: Bearer <site token>
Content-Type: application/json

{"urls": ["https://example.com/blog/new-post", "https://example.com/pricing"]}
```

The host is **not** in the request: it is looked up from the token. A leaked
token therefore cannot be used to submit URLs for a different domain, and an
internet-facing instance is not an open IndexNow relay (which would just move
the shared-IP problem onto your own server).

Every entry has to look like this, or the whole batch is refused:

- absolute `http` or `https`, with a host (`/blog/post` and `example.com/x` are
  not absolute URLs)
- on the site's own host: a lookalike such as `example.com.evil.test` counts as
  a foreign host
- on the default port only, `443` for https and `80` for http
- at most 100 URLs per request, counted after cleaning
- a fragment is stripped before sending, because `#section` marks a place inside
  a document rather than a second URL
- duplicates collapse, so the same URL twice is one submission

The body is capped at 64 KiB. A body larger than that is answered with
`413 payload_too_large` and a detail naming the limit, not with a syntax error:
valid JSON that is simply too big is not malformed JSON.

```json
{"ok": true, "status": 200, "host": "example.com", "submitted": 2}
```

`status` is the HTTP status of **this** response, the same number as the status
line. It is not the engine's status. A batch refused locally was never sent, so
there is no engine status to report, and reporting one would be a status no
engine ever returned. The engine's own status appears in the request's log line
as `engine_status`, and only when an engine actually answered.

### `GET /health`

```json
{"ok": true, "sites": 1, "keys": {"example.com": true}}
```

`keys` reports the key-file check. The check runs **after** the listener is up
and in the background, so a slow or unreachable origin cannot delay the first
byte. A host appears in `keys` only once its check has an answer, so both `true`
and `false` are results. While a check is still running its host is listed in
`pending` instead of being reported as broken:

```json
{"ok": true, "sites": 1, "keys": {}, "pending": ["example.com"]}
```

A stale key file does not fail the health check, because restarting the
container would not fix an origin that is down. A host marked `false` also
recovers by itself: the next accepted submission sets it back to `true`, so a
key file fixed at the origin does not need a restart.

## Result mapping

The engine's verdict is passed through, not flattened into success/failure.
Your caller decides whether to retry.

| HTTP | `reason` | Meaning | Retry? |
|---|---|---|---|
| 200 | - | accepted (200 or 202 from IndexNow) | no |
| 400 | `bad_request` | malformed JSON body, or nothing usable once the URLs were cleaned | no |
| 400 | `invalid_url` | an entry is empty, or not an absolute http/https URL | no - fix the caller |
| 400 | `too_many` | more than 100 URLs in one request | no - split the batch |
| 400 | `non_default_port` | an entry names a port IndexNow will not accept | no - fix the caller |
| 400 | `engine_rejected` | the engine refused this payload with a status that has no retry rule | no - resending it is pointless |
| 401 | `unknown_token` | missing or unknown bearer token | no |
| 403 | `key_rejected` | the engine did not accept the key file | no - fix the key |
| 413 | `payload_too_large` | the body was larger than the 64 KiB limit | no - send fewer URLs |
| 422 | `host_mismatch` | a URL was not on the site's own host | no - fix the caller |
| 429 | `rate_limited` | engine is throttling; see `Retry-After` | yes, later |
| 502 | `engine_error` | engine 5xx or the request never landed | yes, later |

Only `429` and `502` are worth retrying. Every other status is the same answer
if the identical request is sent again. `engine_rejected` is the one to read
carefully: the engine understood the payload and refused it, so retrying is
pointless until the payload changes. An oversized body is reported as
`payload_too_large` rather than as `bad_request`, because the JSON may be valid
and only too large.

`detail` carries the engine's own explanation for a refusal, truncated to one
short line with whitespace collapsed and control characters dropped, or the
reason a local refusal gave for refusing the batch.

`200`/`202` means the engine **received** the URLs. It does not mean they are
indexed.

## Calling it from a Cloudflare Worker

```ts
const res = await fetch("https://crawlping.example.net/submit", {
  method: "POST",
  headers: {
    authorization: `Bearer ${env.CRAWL_PING_TOKEN}`,
    "content-type": "application/json",
  },
  body: JSON.stringify({ urls: [`https://example.com/products/${slug}`] }),
});

const result = await res.json();
if (res.status === 429) {
  // Queue it for later: `result.retry_after_seconds` says for how long.
}
```

## What it deliberately does not do

- No sitemap watching or diffing. You send the URLs you changed.
- No retry loop. `429` is reported so your code can decide.
- No database, no UI, no accounts.
- No Google. Google's Indexing API accepts only `JobPosting` and
  `BroadcastEvent`, so no tool can submit ordinary pages to Google.
- No multi-channel submission yet. Bing Webmaster API and Yandex are the
  obvious next adapters, which is why the reason/status surface is explicit.

## Development

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.24-alpine go test ./...
docker build -t crawlping .
```

No dependencies beyond `gopkg.in/yaml.v3`. Everything else is the standard
library, which keeps the image at ~9 MB.

## Releasing

Tag-driven, and the tag is the only trigger:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` then runs the tests, cross-compiles five
binaries (`linux`/`darwin`/`windows` x `amd64`/`arm64` as applicable), builds and
pushes a multi-arch image tagged `0.1.0`, `0.1` and `latest`, and creates the
GitHub release with the archives and `checksums.txt` attached.

A tag containing a hyphen (`v0.2.0-rc1`) is published as a prerelease and does
not move `latest`.

What the workflow deliberately does not do: no goreleaser, no cosign, no SBOM.
At this size those are ceremony rather than protection - the build is
`go build` and the artifact is reproducible from the tag.

## License

MIT. See [LICENSE](LICENSE).

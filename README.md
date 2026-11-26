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

The image is public: `docker pull` needs no `docker login`.

## Quick start

Generate a token and copy the example config (mode `0600` - it holds tokens):

```bash
openssl rand -hex 32
cp config.example.yaml config.yaml
chmod 600 config.yaml
```

Which shape you deploy decides one line of that file - `listen:`. Everything
below assumes the config is at `./config.yaml`.

### Docker

`config.example.yaml` ships this shape, so copying it and filling in the host,
key and token is the whole configuration step:

```yaml
listen: "0.0.0.0:8080"        # all interfaces *inside* the container
```

```bash
docker run -d --name crawlping \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro" \
  ghcr.io/rokcso/crawlping
```

Both halves are required, and they do different jobs. `-p` forwards to the
container's **eth0** address, never to its loopback, so a container left on
`127.0.0.1:8080` is unreachable from the host even with
`-p 8080:8080` - and it still reports healthy, because its health probe dials
the same address the server is bound to. The `0.0.0.0` belongs to the container.
"Not reachable from anywhere else" belongs to the host, and it is carried by the
`127.0.0.1:` prefix in `-p 127.0.0.1:8080:8080`: the API answers on
`http://127.0.0.1:8080` from this machine and from nowhere else. Write
`-p 8080:8080` only if other machines really should reach it.

`docker-compose.yaml` publishes this shape on `127.0.0.1` only, so this reaches
it from this machine and from nowhere else:

```bash
docker compose up -d
```

### No Docker

Run the binary directly and switch that line to loopback. The example ships the
container shape, and with no container there is no port forwarding to satisfy,
so only this machine should be able to reach it:

```yaml
listen: "127.0.0.1:8080"
```

```bash
crawlping serve -config config.yaml
```

### Behind a Cloudflare Tunnel

Publish no port at all and let the tunnel carry inbound traffic. crawlping joins
cloudflared's network namespace, so cloudflared's `http://127.0.0.1:8080` is
crawlping's own loopback: switch that line back to `127.0.0.1:8080`, and no port
is published. The commented block in `docker-compose.yaml` is this shape.
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
FAIL  shop.example.org                 https://shop.example.org/key.txt
      key file rejected: key file does not match indexnow_key
????  blog.example.net                 https://blog.example.net/key.txt
      could not determine: key file unreachable: fetch key file: context deadline exceeded
```

It prints one line per site, and the three prefixes are the three key states
`/health` reports. `FAIL` means the file was fetched and refused, which is
`failed`. `????` means the file was never read - the origin was down, slow, or
the name did not resolve - so nothing is known about that key, and it is not
counted as a broken one. The exit status keeps the two apart, so a deploy script
can tell a wrong key from an origin that did not answer:

```
crawlping: 1 of 3 key files failed, 1 could not be checked
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

The body is capped at 65536 bytes (64 KiB), and the cap counts the bytes that
were actually sent rather than the bytes a JSON decoder happened to stop at: a
small object followed by megabytes of filler is answered with
`413 payload_too_large` and a detail naming the limit, not with a syntax error,
because valid JSON that is simply too big is not malformed JSON. A body with
anything after the JSON value - a second object, a stray brace, trailing text -
is answered with `400 bad_request` instead of having the extra bytes silently
ignored. Together they describe the one shape the endpoint accepts: a single
`{"urls": [...]}` object, then the end of the body.

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
{"ok": true, "sites": 1, "verified": 1, "failed": 0, "pending": 0}
```

That is the whole shape: counts, and no host name, key, key location or URL. The
parameterless endpoint is reachable by anything that can reach the port, and the
Cloudflare Tunnel shape above makes that the whole internet, so an anonymous GET
must not be able to enumerate which domains this instance submits for, nor learn
which of them are ready.

The three counters are the key states below and always add up to `sites`: every
configured host is counted exactly once, and a host nothing is known about yet is
`pending`, not `failed`.

The status code answers one question - is the process up - so it stays `200` even
when `failed` is non-zero. A key file that does not work is the origin's problem
to fix, restarting this container would not change it, and a probe that restarts
on a stale key turns a wrong key file into an outage.

#### `GET /health?detail=1`

The per-host view is behind a site token, the same bearer token `/submit` takes:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/health?detail=1
```

```json
{"ok": true, "sites": 2, "keys": {"example.com": "verified", "shop.example.org": "pending"}}
```

A missing, empty, wrong or unknown token is refused before a single host name is
written, so a failed guess learns nothing:

```json
{"ok": false, "status": 401, "reason": "unknown_token"}
```

`detail` counts as soon as the parameter is present, so `?detail`, `?detail=1`
and `?detail=yes` all ask for the per-host view. Only `?detail=0` and
`?detail=false` switch it off, so a typo fails towards the answer that was asked
for rather than away from it.

### Key states

Every host is in exactly one of three states. Two of them are facts about the key
and one is the absence of a fact, which is why there are not two.

| State | What it means | What sets it |
|---|---|---|
| `verified` | the key is known to work | the key file was fetched and matched, or a submission was accepted with engine `200` |
| `pending` | nothing is known yet | not checked yet, a check still running, a check cancelled or timed out, an unreachable origin, or the engine answered `202` ("received, key validation pending") |
| `failed` | the key is known not to work | the key file was fetched and definitively rejected (non-2xx status, unreadable body, or body mismatch), or the engine answered `403` |

The distinction that matters: cancellation and an unreachable origin are **not**
failures. A check that reached no answer learned nothing, so it records `pending`
- or leaves the state alone when the process is shutting down - instead of
blaming a key file nobody managed to read. Only a definitive rejection is
`failed`.

An engine error or a rate limit says nothing about the key and leaves the state
as it was. A host that is `failed` recovers by itself: the next accepted
submission with engine `200` sets it back to `verified`, so a key file fixed at
the origin does not need a restart.

The check runs **after** the listener is up and in the background, so slow or
unreachable origins cannot delay the first byte, and no outcome here stops the
process.

## Result mapping

The engine's verdict is passed through, not flattened into success/failure.
Your caller decides whether to retry.

| HTTP | `reason` | Meaning | Retry? |
|---|---|---|---|
| 200 | - | accepted (200 or 202 from IndexNow; a 202 leaves the key `pending`) | no |
| 400 | `bad_request` | the JSON did not parse, or did not end where the object ended (trailing data), or the body could not be read, or nothing usable survived URL cleaning, or the engine answered 400 | no |
| 400 | `invalid_url` | an entry is empty, or not an absolute http/https URL | no - fix the caller |
| 400 | `too_many` | more than 100 URLs in one request | no - split the batch |
| 400 | `non_default_port` | an entry names a port IndexNow will not accept | no - fix the caller |
| 400 | `engine_rejected` | the engine refused this payload with a status that has no retry rule | no - resending it is pointless |
| 401 | `unknown_token` | missing or unknown bearer token, on `/submit` or `/health?detail=1` | no |
| 403 | `key_rejected` | the engine did not accept the key file; also marks the host `failed` | no - fix the key |
| 413 | `payload_too_large` | the body was larger than the 65536 byte limit | no - send fewer URLs |
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

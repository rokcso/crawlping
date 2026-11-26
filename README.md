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

Three ways, same version:

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

```yaml
listen: "127.0.0.1:8080"

sites:
  - host: example.com
    indexnow_key: your-indexnow-key      # also served at /<key>.txt
    token: the-token-you-generated
```

```bash
docker run -d --name crawlping \
  --restart unless-stopped \
  -v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro" \
  ghcr.io/rokcso/crawlping
```

The listener is loopback-only on purpose. Expose it with a Cloudflare Tunnel
(`cloudflared tunnel --url http://127.0.0.1:8080`) or an SSH tunnel. Note that
only the **inbound** path goes through Cloudflare - outbound to IndexNow still
leaves from your machine, which is the whole point.

Check the key files are reachable before trusting it:

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

```json
{"ok": true, "status": 200, "host": "example.com", "submitted": 2}
```

### `GET /health`

```json
{"ok": true, "sites": 1, "keys": {"example.com": true}}
```

`keys` reports the startup check of each key file. A stale key file does not
fail the health check - restarting the container would not fix an origin that
is down.

## Result mapping

The engine's verdict is passed through, not flattened into success/failure.
Your caller decides whether to retry.

| HTTP | `reason` | Meaning | Retry? |
|---|---|---|---|
| 200 | - | accepted (200/202 from IndexNow) | no |
| 400 | `bad_request` | malformed body, or nothing usable after filtering | no |
| 401 | `unknown_token` | missing or unknown bearer token | no |
| 403 | `key_rejected` | the engine did not accept the key file | no - fix the key |
| 422 | `host_mismatch` | a URL was not on the site's own host | no - fix the caller |
| 429 | `rate_limited` | engine is throttling; see `Retry-After` | yes, later |
| 502 | `engine_error` | engine 5xx or the request never landed | yes, later |

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

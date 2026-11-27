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

Write a working setup, then read it before trusting it:

```bash
crawlping init -host example.com             # add -shape binary|tunnel if not Docker
```

That writes two files and nothing else: `config.yaml`, mode `0600`, carrying a
generated IndexNow key and a generated bearer token; and `<key>.txt`, the file
the site has to serve. It then prints what to do with both - the Worker route or
the `cp` into whichever directory the site publishes - the command that starts
the service for the shape you chose, and `crawlping check` as the next step.

It refuses to replace a file that already exists without `-force`, and `-force`
replaces the key too, so its output names the key file that just became orphaned:
the site goes on serving the old one, and submissions are refused `403` until the
new one is deployed.

`-shape` is the flag worth reading about, because it decides the one line that
costs the most to get wrong. `docker` (the default) writes `listen:
"0.0.0.0:8080"`, because Docker forwards a published port to the container's
eth0 and never to its loopback; `binary` and `tunnel` write `127.0.0.1:8080`,
where there is no container boundary to cross and no port is published.

Doing it by hand is the same two files:

```bash
openssl rand -hex 32
cp config.example.yaml config.yaml
chmod 600 config.yaml
```

Everything below assumes the config is at `./config.yaml`.

The other key worth knowing is `indexnow_endpoint`. It defaults to the global
`https://api.indexnow.org/indexnow`, and leaving it alone is right for almost
everyone: that endpoint shares the submission with every participating engine,
so one POST already reaches all of them. Set it only to target a single engine
instead; `config.example.yaml` lists the documented per-engine endpoints.

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

A green `check` is a local vantage point, not the engine's verdict. This command
fetches each key file from the machine crawlping runs on; the engines fetch it
themselves, from their own addresses, at submission time, and IndexNow has no
verification API to ask in advance. A key file can pass `check` and `/health`
and still be refused on the first real submission. A `403` after a green
`check` therefore usually means the engine's fetcher is being blocked - a bot
filter, or a country rule in front of the file - rather than the key being
wrong, which is a live concern on Cloudflare. The only authoritative
confirmation is the engine's own `200` or `202` on a real submission.

## API

Four endpoints. A bearer token per site decides which host a request may submit
for, so the host is never taken from the request and a leaked token cannot be
used against another domain.

| Method | Path | Auth | Success | What it does |
|---|---|---|---|---|
| `POST` | `/submit` | bearer token | `200`, `202` | Submit a batch of URLs in a JSON body |
| `GET` | `/submit` | bearer token | `200`, `202` | Submit one URL, IndexNow's own form |
| `GET` | `/health` | none; `?detail=1` needs a token | `200` | Counts, or per-host detail |
| `GET` | `/metrics` | none; host labels need a token | `200` | Prometheus text format |

```bash
curl -X POST http://127.0.0.1:8080/submit \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"urls": ["https://example.com/blog/new-post"]}'

{"ok": true, "status": 200, "host": "example.com", "submitted": 1, "engine_status": 200}
```

**The full contract is in [docs/API.md](docs/API.md)**: the URL rules a batch has
to meet, the size and rate limits, the five-minute resubmission window, the
reason vocabulary and which reasons are worth retrying, the three key states, and
a worked Worker caller. Read it before writing anything that retries.

## What it deliberately does not do

- No sitemap watching or diffing. You send the URLs you changed.
- No look at the target page. Nothing here fetches a submitted URL, so
crawlping cannot know that one is `noindex`, blocked by `robots.txt`, or already
`404`. Every submitted URL spends crawl quota, indexed or not, so that judgement
stays with the caller.
- No retry loop. `429`, `502` and `503` are reported so your code can decide;
the instance only bounds how much it will spend on your behalf.
- No per-URL memory beyond the five-minute resubmission window. The window
  remembers that a URL was sent recently, per host; it holds no history, keeps no
  record of what the engine did with it, and never schedules anything. Dedupe
  inside a single request still happens for every caller.
- No database, no UI, no accounts.
- No Google. Google's Indexing API accepts only `JobPosting` and
  `BroadcastEvent`, so no tool can submit ordinary pages to Google.
- No Bing Webmaster or Yandex webmaster API. Those are separate products, not
  more IndexNow endpoints: a submission to the global endpoint is already shared
  with every participating engine. What the webmaster APIs add is submission
  quota and index status, which IndexNow does not expose.

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

The release body is generated from the commit subjects **unless**
`docs/release-notes/<tag>.md` exists, in which case that file is used verbatim.
Write one for any release with a breaking change: a generated body cannot say
"`/health` no longer returns a per-host map", and somebody upgrading reads the
release rather than the log. `docs/release-notes/v0.2.0.md` is the worked example.

What the workflow deliberately does not do: no goreleaser, no cosign, no SBOM.
At this size those are ceremony rather than protection - the build is
`go build` and the artifact is reproducible from the tag.

## License

MIT. See [LICENSE](LICENSE).

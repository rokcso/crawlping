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
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping
```

`--user` is not decoration. The image runs as its own nonroot uid, `config.yaml`
is mode `0600` (a fact `doctor` now checks rather than assumes), and a bind mount
keeps the file's real owner and mode on Linux - so a container left on the
image's uid cannot read the file it was handed. It answers `read config: ...
permission denied` and restart-loops while the file sits right there, and nothing
in that message names the owner. Running as *your* uid makes the file's owner and
the process's user the same, which is the only version of this that needs no
`chown`. The same rule applies to every command that reads or writes the config:
add `--user "$(id -u):$(id -g)"` to `check`, `site add` and `doctor` too.

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

The compose file carries `user: "1000:1000"` for the reason above; change it to
your own `id -u` and `id -g`.

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
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/etc/crawlping:ro" \
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

## Adding a site

One command, then one copy. A token is bound to one host, so a new site needs its
own token and its own key file, and both are generated:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/work" -w /work \
  ghcr.io/rokcso/crawlping site add -host shop.example.org
```

It appends the entry to `config.yaml`, writes `<key>.txt` next to it, and prints
the one thing it cannot do: where that file has to be served. Then apply it
without dropping a request:

```bash
docker kill -s HUP crawlping
```

`serve` reloads on SIGHUP. The listener stays up, in-flight submissions finish,
and the counters in `/metrics` keep counting - a reload changes who may submit,
not what already happened. A config that does not load is refused and the running
one keeps serving, so a typo cannot take down an API a Worker is calling.

Three things do move with the new config. The resubmission window's table grows
to `limits.max_urls_per_request` if the reload raised it, so one full batch is
still remembered. A host the new list no longer carries loses its recorded key
verdict, so a host that is added back later is checked again instead of
inheriting an old `verified`. And the per-site engine share stays the one the
process started with, because it is the width of a semaphore that may already
have slots held: a reload that changes the site count enough to imply a different
share logs a warning naming both numbers, and a restart is what applies the one
the current count implies. `site add` says the same thing when it adds the second
site, because that is the one place it changes.

**Reloading only works if the config is mounted as a directory.** A bind mount of
a single file pins the inode the container started with: `site add` writes through
a temporary file and a rename (so a failed edit cannot leave a truncated config),
and the rename creates a new inode the container never sees. The signal arrives,
the reload runs, and it reloads the bytes already in memory - so `/health` still
reports the old site count and nothing tells you why. That is why every `-v` in
this README mounts a directory and names the file inside it:

```bash
-v "$PWD:/etc/crawlping:ro"        # right: the directory, so a rename is visible
-v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro"   # wrong: the inode is pinned
```

A reload that reads the bytes it already had logs a warning saying exactly this,
so a deployment that kept the old shape is told rather than left guessing.

The rest of the surface:

```bash
crawlping site list                  # every site, token masked to its last four
crawlping site rm -host shop.example.org
```

`site add` is worth reading before you trust it:

| Flag | Meaning |
|---|---|
| `-host` | the bare hostname, as everywhere else: no scheme, port or path |
| `-key` | reuse a key whose file is already deployed instead of generating one |
| `-token` | set the token yourself; generated when empty |
| `-keydir` | where `<key>.txt` goes; the config's directory by default |

It edits the YAML node tree rather than re-rendering the file, so your comments
and key order survive. It writes through a temporary file and a rename, keeps the
mode the config already had, and reads the result back with the loader the
service itself uses - if the edited file would not start the service, nothing is
written and the error names the line.

What it does not do is deploy the key file. That file has to reach the site's own
origin, and only you can do that.

## Checking a whole deployment

`check` answers one question - is each key file right. `doctor` answers whether
the deployment can work at all, including whether the config file's mode keeps its
tokens private, and it is read-only: it never submits anything, so it is safe to
run against a live instance.

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping doctor -config /etc/crawlping/config.yaml
```

```
ok    config   /etc/crawlping/config.yaml, 2 sites, endpoint https://api.indexnow.org/indexnow
ok    mode     0600, readable only by the owner
ok    listen   0.0.0.0:8080 is answering /health 200; the address is held by this instance
ok    engine   api.indexnow.org:443 reachable in 41ms
ok    key      example.com   https://example.com/<key>.txt
FAIL  key      shop.example.org  https://shop.example.org/<key>.txt
      200 but text/html, which is a page, not a key file
      the site is answering for a path it does not have; publish <key>.txt at the host root
```

The `mode` line answers a question nothing else could: `config.yaml` holds every
site's bearer token, and `init` writes it `0600`, but an editor that replaces the
file, a `cp` from a backup or a `chmod -R` leaves it `0644` and nothing used to
notice. A file the group or anyone else can read is a `FAIL` naming the mode,
with `chmod 600` in the note under it, and it exits non-zero. `0400` and `0700`
pass: only the group and other bits are judged, because those modes are private
too.

The last block is the failure that looks like success: a static site with a
catch-all route answers `200` for every path, so fetching the key URL and seeing
`200` is not evidence. The engine reads that page and refuses the key. `doctor`
compares the body and the content type, so this is a `FAIL` with a cause rather
than a green check followed by a `403`.

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
git tag v0.2.0 && git push origin v0.2.0
```

`.github/workflows/release.yml` then runs the tests, cross-compiles five
binaries (`linux`/`darwin`/`windows` x `amd64`/`arm64` as applicable), builds and
pushes a multi-arch image tagged with the tag's own version, its `major.minor`
and `latest`, and creates the GitHub release with the archives and `checksums.txt`
attached. `v0.2.0` therefore publishes `0.2.0`, `0.2` and `latest`.

A tag containing a hyphen (`v0.2.0-rc1`) is published as a prerelease and does
not move `latest`.

The release body is the `CHANGELOG.md` section for that version, read before
anything is built. A tag whose version was never written there fails the
workflow while there is still nothing to undo, rather than publishing an image
with no release beside it. What to write, and the four headings to write it
under, is at the top of [CHANGELOG.md](CHANGELOG.md).

So the order is: write the entry, move it out of `Unreleased`, then tag.

What the workflow deliberately does not do: no goreleaser, no cosign, no SBOM.
At this size those are ceremony rather than protection - the build is
`go build` and the artifact is reproducible from the tag.

## License

MIT. See [LICENSE](LICENSE).

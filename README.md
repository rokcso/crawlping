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

## The 30-second version

```bash
# 1. Write a config and the key file the site has to serve.
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/work" -w /work \
  ghcr.io/rokcso/crawlping:0.2.1 init -host example.com

# 2. Start it. Reachable from this machine only.
docker run -d --name crawlping --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping:0.2.1

# 3. Check it.
curl -s http://127.0.0.1:8080/health
```

Two things are left, and they are yours to do: serve the `<key>.txt` that step 1
wrote from the site, at `https://example.com/<key>.txt`, and call `/submit` from
your Worker with the bearer token that is now in `config.yaml`.

Everything below is one of those steps in more detail. The flags in the commands
above are load-bearing, and skipping the section on why they are is how this
deployment breaks quietly - see [Why the flags are what they
are](#why-the-flags-are-what-they-are).

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

```bash
docker pull ghcr.io/rokcso/crawlping:latest   # public, no docker login
go install github.com/rokcso/crawlping@latest # or from source
```

Plain binaries for linux/darwin/windows are on
[the releases page](https://github.com/rokcso/crawlping/releases), each archive
with a `checksums.txt` entry. The binary and the image are built from the same Go
version, and CI fails the tag if `go.mod` and the Dockerfile disagree.

## Deploy

Pick one shape. All four run the same binary.

### Docker (the default)

`config.example.yaml` already ships this shape, so `cp config.example.yaml
config.yaml`, fill in the host, key and token, and the config step is done:

```bash
docker run -d --name crawlping \
  --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping:0.2.1
```

- `--user` - the image's own uid cannot read a `0600` config.
- `-p 127.0.0.1:8080:8080` - the `127.0.0.1:` prefix is what keeps this on your
  machine; `-p 8080:8080` only if other machines really should reach it.
- `-v "$PWD:/etc/crawlping:ro"` - the **directory**, not the file.

Each of those three fails quietly when it is wrong; [Why the flags are what they
are](#why-the-flags-are-what-they-are) is what each one is for.

### Docker Compose

Same shape, publishing on loopback only:

```bash
docker compose up -d
```

The compose file carries `user: "1000:1000"`; change it to your own `id -u` and
`id -g`.

### No Docker

The example config ships the container shape, where `listen` has to be the
wildcard. With no container there is no port forwarding to satisfy, so switch it
to loopback and only this machine can reach it:

```yaml
listen: "127.0.0.1:8080"     # and start with: crawlping serve -config config.yaml
```

### Behind a Cloudflare Tunnel

Publish no port at all and let the tunnel carry inbound traffic. crawlping joins
cloudflared's network namespace, so cloudflared's `http://127.0.0.1:8080` is
crawlping's own loopback: switch `listen` back to `127.0.0.1:8080` and publish
nothing. The commented block in `docker-compose.yaml` is this shape.

Only the **inbound** path goes through Cloudflare - outbound to IndexNow still
leaves from your machine, which is the whole point.

## Publish the key file

IndexNow proves ownership by fetching `https://<host>/<key>.txt` itself. `init`
writes that file next to the config; this is the one step no command here can do
for you, and nothing is accepted until it is live.

Cloudflare Worker, above the site's own routes:

```js
if (new URL(request.url).pathname === "/<key>.txt")
  return new Response("<key>\n", { headers: { "content-type": "text/plain" } });
```

Static host - copy it into whatever directory the site publishes:

```bash
cp <key>.txt public/<key>.txt
```

Keep it at the host root. A key file inside a directory (say
`https://example.com/catalog/<key>.txt`) only covers URLs under that directory;
everything else is refused as `out_of_key_scope` before anything is sent.

## Verify a deployment

`doctor` answers whether the deployment can work at all. It is read-only, so it
is safe against a live instance:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping:0.2.1 doctor -config /etc/crawlping/config.yaml
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

That last block is the failure that looks like success: a static site with a
catch-all route answers `200` for every path, so fetching the key URL and seeing
`200` is not evidence. The engine reads that page and refuses the key. `doctor`
compares the body and the content type, so this is a `FAIL` with a cause rather
than a green check followed by a `403`.

The `mode` line is the other one worth knowing: an editor that replaces the file,
a `cp` from a backup or a `chmod -R` can leave `config.yaml` at `0644`, and it
holds every site's bearer token. Group or other readable is a `FAIL` naming the
mode, with `chmod 600` under it, and it exits non-zero. `0400` and `0700` pass -
only the group and other bits are judged.

`check` answers the narrower question - is each key file right - and prints one
line per site:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/etc/crawlping:ro" \
  ghcr.io/rokcso/crawlping:0.2.1 check -config /etc/crawlping/config.yaml
```

```
ok    example.com                      https://example.com/your-indexnow-key.txt
FAIL  shop.example.org                 https://shop.example.org/key.txt
      key file rejected: key file does not match indexnow_key
????  blog.example.net                 https://blog.example.net/key.txt
      could not determine: key file unreachable: fetch key file: context deadline exceeded
```

The three prefixes are the three key states `/health` reports. `FAIL` means the
file was fetched and refused. `????` means it was never read - the origin was
down, slow, or the name did not resolve - so nothing is known, and it is not
counted as a broken key. The exit status keeps the two apart, so a deploy script
can tell a wrong key from an origin that did not answer:

```
crawlping: 1 of 3 key files failed, 1 could not be checked
```

**A green `check` is a local vantage point, not the engine's verdict.** This
command fetches each key file from the machine crawlping runs on; the engines
fetch it themselves, from their own addresses, at submission time, and IndexNow
has no verification API to ask in advance. So a key file can pass `check` and
`/health` and still be refused on the first real submission. A `403` after a
green `check` usually means the engine's fetcher is being blocked - a bot filter,
or a country rule in front of the file, which is a live concern on Cloudflare -
rather than the key being wrong. The only authoritative confirmation is the
engine's own `200` or `202` on a real submission.

## Endpoints

A bearer token per site decides which host a request may submit for, so the host
is never taken from the request and a leaked token cannot be used against another
domain.

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

## Configuration

One file, read once at startup, mode `0600`. There is no environment layer.

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8080` | the address to bind; see [Why](#why-the-flags-are-what-they-are) |
| `indexnow_endpoint` | the global endpoint | leave it alone; see below |
| `limits.max_urls_per_request` | `100` | URLs one request may carry, 1 to 10000 |
| `limits.max_body_bytes` | `65536` | body cap; the two are one decision and are checked together |
| `sites[].host` | - | bare hostname, no scheme/port/path, punycode if IDN |
| `sites[].indexnow_key` | - | also the name of the file served at `/<key>.txt` |
| `sites[].key_location` | `https://<host>/<key>.txt` | set only when the file lives elsewhere |
| `sites[].token` | - | the bearer token; at least 16 chars, unique per site |

The defaults for `limits` are far below what IndexNow accepts (10,000 per
request), on purpose: a code path that sends hundreds of URLs is usually looping,
and a small default makes that visible instead of expensive. Raise both together
for a site migration - a mismatched pair makes every full batch a `413`, so it is
refused at startup rather than later.

`indexnow_endpoint` defaults to the global endpoint, and leaving it alone is right
for almost everyone: a submission there is shared with every participating engine,
so one POST already reaches all of them. Set it only to target a single engine
instead; `config.example.yaml` lists the documented per-engine endpoints.

## Day-to-day

**Add a site.** A token is bound to one host, so a new site needs its own token
and its own key file, and both are generated:

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/work" -w /work \
  ghcr.io/rokcso/crawlping:0.2.1 site add -host shop.example.org
```

It appends the entry to `config.yaml`, writes `<key>.txt` next to it, and prints
the one thing it cannot do: where that file has to be served. Then apply it
without dropping a request:

```bash
docker kill -s HUP crawlping
```

| `site add` flag | Meaning |
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

**The rest of the surface.**

```bash
crawlping site list                  # every site, token masked to its last four
crawlping site rm -host shop.example.org
```

**What a reload does.** `SIGHUP` swaps the config under a running process: the
listener stays up, in-flight submissions finish, and the counters keep counting.
A config that does not load is refused and the running one keeps serving, so a
typo cannot take down an API a Worker is calling.

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

## Observability

`/metrics` is Prometheus text format, unauthenticated by default. Without a token
every family is aggregated and carries no `host` label, because a Tunnel makes
this endpoint publicly reachable and your domain list is not public information;
any configured site token unlocks the labels.

```
crawlping_submissions_total{host}                        counter
crawlping_submission_reasons_total{host,reason}          counter
crawlping_engine_requests_total{host,status}             counter
crawlping_engine_duration_seconds{host}                  histogram
crawlping_urls_submitted_total{host}                     counter
crawlping_urls_skipped_total{host}                       counter
crawlping_guard_refusals_total{host,kind}                counter
crawlping_key_state{host}                                gauge (0 pending, 1 verified, 2 failed)
```

IndexNow publishes no limits, so these numbers are the only warning you get
before the engine throttles your IP. What is worth alerting on:

| Signal | Means |
|---|---|
| `crawlping_key_state == 2` for 10m | a key file is definitively broken |
| `rate(crawlping_engine_requests_total{status="429"}[5m]) > 0` | the engine is already penalising this IP - stop sending |
| rising p95 of `crawlping_engine_duration_seconds` | early warning, it precedes the 429s |
| `crawlping_submission_reasons_total{reason="throttled"}` climbing | your own caller is looping; the instance is protecting you |
| `crawlping_key_state == 0` with an old `checked_at` in `/health?detail=1` | nothing is known, the origin may be unreachable |
| `crawlping_guard_refusals_total{kind="overloaded"}` | no engine slot in time; retry budget exhausted |

Two facts the numbers cannot show you:

- **State is in memory.** The five-minute window, the counters and the key
  verdicts all reset when the process restarts. A caller retrying right after a
  restart can have a URL re-sent that went out minutes earlier, which costs one
  extra submission. Nothing is persisted, which is also why there is no database
  to lose.
- **Behind a Tunnel, every log line says `remote=127.0.0.1`.** The instance
  deliberately does not trust `X-Forwarded-For`, because the port would be
  spoofable if it were ever exposed. Your log cannot tell you which caller sent
  what in that shape; use the per-site token to attribute traffic instead.

## Why the flags are what they are

Four decisions in the commands above cost the most when they are wrong, and each
one fails quietly rather than loudly. This is what each is for.

**`--user "$(id -u):$(id -g)"`.** The image runs as its own nonroot uid,
`config.yaml` is mode `0600` (a fact `doctor` checks rather than assumes), and a
bind mount keeps the file's real owner and mode on Linux. So a container left on
the image's uid cannot read the file it was handed: it answers `read config: ...
permission denied` and restart-loops while the file sits right there, and nothing
in that message names the owner. Running as *your* uid makes the file's owner and
the process's user the same, which is the only version of this that needs no
`chown`. The same rule applies to every command that reads or writes the config:
add `--user "$(id -u):$(id -g)"` to `check`, `site add` and `doctor` too.

**`listen` inside the container vs `-p` on the host.** The two addresses do
different jobs, and mixing them up is the trap. `-p` forwards to the container's
**eth0** address, never to its loopback, so a container left on `127.0.0.1:8080`
is unreachable from the host even with `-p 8080:8080` - and it still reports
healthy, because its health probe dials the same address the server is bound to.
That is why `config.example.yaml` ships `listen: "0.0.0.0:8080"`: the wildcard
belongs to the container. "Not reachable from anywhere else" belongs to the host,
and it is carried by the `127.0.0.1:` prefix in `-p 127.0.0.1:8080:8080`.

**Mounting the directory, not the file.** A bind mount of a single file pins the
inode the container started with. `site add` writes through a temporary file and
a rename (so a failed edit cannot leave a truncated config), and the rename
creates a new inode the container never sees: the signal arrives, the reload
runs, and it reloads the bytes already in memory - so `/health` still reports the
old site count and nothing tells you why.

```bash
-v "$PWD:/etc/crawlping:ro"                             # right: a rename is visible
-v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro"     # wrong: the inode is pinned
```

A reload that reads the bytes it already had logs a warning saying exactly this,
so a deployment that kept the old shape is told rather than left guessing.

**`-force` on `init`.** It replaces the key too, so the output names the key file
that just became orphaned: the site goes on serving the old one, and submissions
are refused `403` until the new one is deployed.

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
- No database, no UI, no accounts, and nothing persisted across a restart.
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

## Repository layout

One file per concern, in the order they are used rather than alphabetically.

| Surface | File | What it holds |
|---|---|---|
| commands | `main.go` | flags, the subcommand table, `serve` and its signals |
| | `init.go` | the config and key a host starts with, and its checklist |
| | `site.go` | `site add`, `site list`, `site rm`, and the atomic config edit |
| | `doctor.go` | the read-only deployment checks, one line per fact |
| HTTP | `server.go` | the mux, the three handlers, reload, and the refusals |
| | `keys.go` | the per-host key verdict, and the recheck that moves it |
| | `metrics.go` | the counters, and the Prometheus text they render as |
| engine | `indexnow.go` | the POST, the key-file fetch, the URL scope rules |
| | `config.go` | the YAML shape, its defaults, and the checks that refuse one |
| limits | `ratelimit.go` | the per-host bucket behind an early `429` |
| | `inflight.go` | the semaphores that bound concurrent engine work |
| | `window.go` | the five-minute table that remembers what was just sent |

Each file above has a `_test.go` beside it. Two do not test a file:
`docs_test.go` checks the endpoint table above against the routes that serve it,
in both directions, and `changelog_test.go` checks `CHANGELOG.md` against what
the release script reads. `scripts/compose-smoke.sh` is the only test that sees
the assembled container, and it exists for the failure the unit tests cannot
reach - a container that builds, starts, and is unreachable from the host.

## Releasing

Tag-driven, and the tag is the only trigger: `git tag v0.2.1 && git push origin
v0.2.1`. The workflow runs the tests, cross-compiles five binaries, builds and
pushes a multi-arch image tagged with the tag's version, its `major.minor` and
`latest` (`v0.2.1` therefore publishes `0.2.1`, `0.2` and `latest`), and creates
the GitHub release with the archives and `checksums.txt` attached. A tag
containing a hyphen is a prerelease and does not move `latest`.

The release body is the `CHANGELOG.md` section for that version, read before
anything is built: a tag whose version was never written there fails the workflow
while there is still nothing to undo. So the order is: write the entry, move it
out of `Unreleased`, then tag. The four headings to write it under are at the top
of [CHANGELOG.md](CHANGELOG.md).

What the workflow deliberately does not do: no goreleaser, no cosign, no SBOM.
At this size those are ceremony rather than protection - the build is `go build`
and the artifact is reproducible from the tag.

## License

MIT. See [LICENSE](LICENSE).
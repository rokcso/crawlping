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
- on the default port only, `443` for https and `80` for http - this instance's
  rule, not IndexNow's, for the reason below
- at most 100 URLs per request, counted after cleaning (IndexNow allows 10,000
  per POST; the smaller cap is this instance's own restraint)
- within the directory the key file covers, when the key file is not at the
  host root
- a fragment is stripped before sending, because `#section` marks a place inside
  a document rather than a second URL
- duplicates collapse, so the same URL twice is one submission

Where the key file sits decides what the key covers. `key_location` defaults to
the host root, and a key file there covers the whole host. One inside a
directory, such as `https://example.com/catalog/<key>.txt`, covers only URLs
under that directory: `/catalog/a` is in scope, `/help/b` and the site root are
not. A URL outside that scope is refused locally as `out_of_key_scope` and
nothing is sent to the engine, so the fix is to move the key file to the root or
to stop submitting URLs outside its directory.

The default-port rule on that list is this instance's, not the protocol's.
IndexNow sets no port requirement - not in the specification, not in the FAQ,
not in the Bing documentation - and the usual client practice is to strip a
default port while normalising a URL rather than to reject one. crawlping still
refuses a non-default port, because one unacceptable URL can poison a whole
batch: the engine refuses the entire POST, so a caller who sends 99 good URLs
and one on `:8080` learns nothing about any of them.

A URL that now redirects, and a URL whose page now answers `404` or `410`, are
exactly the URLs worth submitting. IndexNow asks for them, because that is how
an engine learns to move or drop an entry it already has. crawlping treats them
as ordinary URLs on the host - nothing here fetches a URL or checks that it is
alive - so removing a page is a submission, not an omission.

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
{"ok": true, "status": 200, "host": "example.com", "submitted": 2, "engine_status": 200}
```

`status` is the HTTP status of **this** response, the same number as the status
line. It is not the engine's status. `engine_status` is the engine's own status,
and it is absent whenever the engine was never called - `throttled`,
`overloaded`, `payload_too_large` and every other local refusal leave it out,
because reporting a status no engine ever returned would be an invention.

The two fields together answer a question one of them cannot: a `422` with
`"reason": "host_mismatch"` and `"engine_status": 422` is the engine refusing
what you sent, while the same `422` with no `engine_status` is this instance
refusing to send it at all. Same reason, two different fixes, two different
owners.

#### Rate and concurrency limits

A token is bound to one host, so nobody can submit for a domain they do not own.
Frequency is bounded too, because the egress IP belongs to the operator and the
engine throttles it, not the caller:

- **10 requests back to back per host**, then one more per second. The burst and
the refill are per host, so one noisy site cannot silence another.
- **8 engine requests in flight at once**, across every site. A request that
finds no free slot waits up to 2 seconds for one, then gives up.

IndexNow does not publish its limits, so these are deliberately conservative.
They exist to keep a leaked token or a retry loop in one caller from pushing the
operator's IP into the engine's throttle - which would break the legitimate
submissions too, and is the exact harm this tool exists to avoid. A refused
request is answered locally and never reaches the engine.

These bounds are about how fast this instance spends its own egress IP. They say
nothing about whether a URL has already been sent, and they will not stop you
submitting one again. crawlping keeps no per-URL state: no history, no debounce,
no record that a URL was sent before. Deduplication across requests and the
resubmission interval are the caller's job, and IndexNow is explicit about both:
do not resubmit a URL that has not changed, wait at least 5 minutes before
resubmitting the same URL (10 in another part of the same documentation), and
assume every submitted URL spends crawl quota whether or not the engine already
had it. The only deduplication inside crawlping is within a single request,
where duplicates collapse before sending. A `200` means the batch was received;
resubmitting on a timer is not a faster route to being crawled.

There are two different `429`s and they are worth telling apart:

| `reason` | Owner | What happened |
|---|---|---|
| `throttled` | this instance | this host spent its burst; the engine was never called, and the egress IP is unharmed |
| `rate_limited` | the engine | the engine is already throttling this IP |

Both carry `Retry-After` and `retry_after_seconds`. Only the second one means
the operator's IP is being penalised; the first one is the guard doing its job.

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
{"ok": true, "sites": 2, "keys": {"example.com": {"state": "verified", "checked_at": "2026-11-27T09:14:02Z", "detail": ""}, "shop.example.org": {"state": "pending"}}}
```

Each host maps to an object rather than a bare state word, so an operator can
see more than the last verdict:

| Field | Meaning |
|---|---|
| `state` | one of the three key states below |
| `checked_at` | when that state was recorded, in UTC; absent while the host has never been checked |
| `detail` | the reason the last check failed or came back inconclusive, empty when `verified` |

`checked_at` is what stops `verified` from lying: a key file that broke an hour
ago is `verified` with an old timestamp, and the age is the warning.

A missing, empty, wrong or unknown token is refused before a single host name is
written, so a failed guess learns nothing:

```json
{"ok": false, "status": 401, "reason": "unknown_token"}
```

`detail` counts as soon as the parameter is present, so `?detail`, `?detail=1`
and `?detail=yes` all ask for the per-host view. Only `?detail=0` and
`?detail=false` switch it off, so a typo fails towards the answer that was asked
for rather than away from it. `recheck` follows the same rule.

#### `GET /health?detail=1&recheck=1`

`recheck=1` runs the key-file check for every configured host before answering,
bounded by a 5 second budget, so an operator who has just fixed a key file gets
a fresh answer instead of an old one:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:8080/health?detail=1&recheck=1'
```

There is no background timer and no periodic loop. A key file changes about
never, so a recurring check would pay a permanent cost - a fetch per host,
forever - for a rare event; an on-demand check costs only when it is asked for.
A recheck that runs out of its 5 second budget leaves the recorded state alone
rather than downgrading a good key because this instance ran out of patience,
and a recheck is refused without a token like any other detail request.

### Key states

Every host is in exactly one of three states. Two of them are facts about the key
and one is the absence of a fact, which is why there are not two.

| State | What it means | What sets it |
|---|---|---|
| `verified` | this instance's best evidence that the key works | the key file was fetched from here and matched, or a submission was accepted with engine `200` |
| `pending` | nothing is known yet | not checked yet, a check still running, a check cancelled or timed out, an unreachable origin, or the engine answered `202` ("received, key validation pending") |
| `failed` | the key is known not to work | the key file was fetched and definitively rejected (non-2xx status, unreadable body, or body mismatch), or the engine answered `403` |

The distinction that matters: cancellation and an unreachable origin are **not**
failures. A check that reached no answer learned nothing, so it records `pending`
- or leaves the state alone when the process is shutting down - instead of
blaming a key file nobody managed to read. Only a definitive rejection is
`failed`.

`verified` is deliberately worded as evidence rather than as proof, because the
two ways of reaching it are not equally strong. A submission the engine answered
`200` is its own verdict. A key file that matched when this instance fetched it
only says the file is correct and reachable **from here**; the engine fetches it
from somewhere else, so a bot filter or a country rule can still turn that into
`403` on the next submission. See [Check it before trusting
it](#check-it-before-trusting-it).

An engine error or a rate limit says nothing about the key and leaves the state
as it was. A host that is `failed` recovers by itself: the next accepted
submission with engine `200` sets it back to `verified`, so a key file fixed at
the origin does not need a restart.

`/health?detail=1` reports each state with the time it was recorded and the
reason behind it, so `verified` cannot quietly describe a key file that broke an
hour ago, and `failed` says what failed rather than only that something did.

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
| 400 | `non_default_port` | an entry names a port this instance will not send; IndexNow itself sets no port requirement | no - fix the caller |
| 400 | `out_of_key_scope` | a URL is outside the directory the key file covers | no - move the key file or stop submitting URLs outside its directory |
| 400 | `engine_rejected` | the engine refused this payload with a status that has no retry rule | no - resending it is pointless |
| 401 | `unknown_token` | missing or unknown bearer token, on `/submit` or `/health?detail=1` | no |
| 403 | `key_rejected` | the engine refused the key: missing, mismatched, or its own fetch of the file was blocked; also marks the host `failed` | no - fix the key |
| 413 | `payload_too_large` | the body was larger than the 65536 byte limit | no - send fewer URLs |
| 422 | `host_mismatch` | a URL was not on the site's own host | no - fix the caller |
| 429 | `rate_limited` | the engine is throttling this IP; see `Retry-After` | yes, later |
| 429 | `throttled` | this instance refused to spend any more of its egress IP on this host right now; the engine was not called | yes, after `Retry-After` |
| 502 | `engine_error` | engine 5xx or the request never landed | yes, later |
| 503 | `overloaded` | every engine slot was in use and none freed up within 2 seconds; the engine was not called | yes, shortly |

Only `429`, `502` and `503` are worth retrying. Every other status is the same
answer if the identical request is sent again. `engine_rejected` is the one to
read carefully: the engine understood the payload and refused it, so retrying is
pointless until the payload changes. An oversized body is reported as
`payload_too_large` rather than as `bad_request`, because the JSON may be valid
and only too large.

Two `429`s, two meanings. `throttled` is this instance protecting its own egress
IP, so nothing was sent and the engine never saw the request; `rate_limited` is
the engine's answer, so the IP is already being penalised. A caller that needs to
know whose limit it hit reads the reason, not the status code.

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
if (res.status === 429 || res.status === 503) {
  // Queue it for later: `result.retry_after_seconds` says for how long.
  // `result.reason` says whose limit it was: `throttled` (this instance) or
  // `rate_limited` (the engine).
  if (result.reason === "rate_limited") {
    // The IP is already being throttled: stop sending for a while.
  }
}
```

## What it deliberately does not do

- No sitemap watching or diffing. You send the URLs you changed.
- No look at the target page. Nothing here fetches a submitted URL, so
crawlping cannot know that one is `noindex`, blocked by `robots.txt`, or already
`404`. Every submitted URL spends crawl quota, indexed or not, so that judgement
stays with the caller.
- No retry loop. `429`, `502` and `503` are reported so your code can decide;
the instance only bounds how much it will spend on your behalf.
- No per-URL memory and no resubmission debounce. Only a single request is
  deduplicated, so the interval between submissions of the same URL is yours to
  enforce.
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

What the workflow deliberately does not do: no goreleaser, no cosign, no SBOM.
At this size those are ceremony rather than protection - the build is
`go build` and the artifact is reproducible from the tag.

## License

MIT. See [LICENSE](LICENSE).

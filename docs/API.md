# crawlping API

The contract a caller has to meet: four endpoints, one bearer token per site, and
a closed vocabulary of reasons to branch on. Everything here is about *calling*
the service. What it is for, and how to deploy it, is in the
[README](../README.md).


## At a glance

| Method | Path | Auth | Success | What it does |
|---|---|---|---|---|
| `POST` | `/submit` | bearer token | `200`, `202` | Submit a batch of URLs in a JSON body |
| `GET` | `/submit` | bearer token | `200`, `202` | Submit one URL, IndexNow's own form |
| `GET` | `/health` | none; `?detail=1` needs a token | `200` | Counts, or per-host detail |
| `GET` | `/metrics` | none; host labels need a token | `200` | Prometheus text format |

The token is per site and is what decides the host: the host is never taken from
the request, so a leaked token cannot be used for another domain. Every refusal
carries a `reason` from a closed vocabulary, listed under
[Result mapping](#result-mapping), and that reason is what says whether to retry.

## `POST /submit`

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
- at most 100 URLs per request by default, counted after cleaning (IndexNow
  allows 10,000 per POST; `limits.max_urls_per_request` raises this, and the
  smaller default is this instance's own restraint)
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

The body is capped at 65536 bytes (64 KiB) by default - `limits.max_body_bytes`
raises it - and the cap counts the bytes that
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

`submitted` counts the URLs that went to the engine in this request. `skipped`
counts the ones the resubmission window dropped, is absent when it is zero, and
is not a reason: see the resubmission window below.

The two fields together answer a question one of them cannot: a `422` with
`"reason": "host_mismatch"` and `"engine_status": 422` is the engine refusing
what you sent, while the same `422` with no `engine_status` is this instance
refusing to send it at all. Same reason, two different fixes, two different
owners.

### Rate and concurrency limits

A token is bound to one host, so nobody can submit for a domain they do not own.
Frequency is bounded too, because the egress IP belongs to the operator and the
engine throttles it, not the caller:

- **10 requests back to back per host**, then one more per second. The burst and
the refill are per host, so one noisy site cannot silence another.
- **8 engine requests in flight at once**, across every site, and **at most 4 of
them for one site** when this instance serves more than one. A request that
finds no free slot waits up to 2 seconds for one, then gives up. The two
refusals are reported separately because they ask for different things: the
`detail` says whether this site had used its own share or the instance had used
its, so the caller knows whether to slow one site down or all of them.
- A config with a **single site keeps all 8**. With one host a per-host cap could
only duplicate the global one, and halving it would throw away half the engine
concurrency the deployment is allowed, for a starvation problem that cannot
exist with one site.

The per-site share is a ceiling, not a reservation: a second site that is idle
still costs the busy one its other four slots. That is deliberate. Concurrency is
not the bottleneck here - the token bucket above is, and a hundred batches wait
about ninety seconds for tokens however many slots are open - so the four slots
buy a guaranteed share for a neighbour at a price the rate limit has already
made irrelevant.

IndexNow does not publish its limits, so these are deliberately conservative.
They exist to keep a leaked token or a retry loop in one caller from pushing the
operator's IP into the engine's throttle - which would break the legitimate
submissions too, and is the exact harm this tool exists to avoid. A refused
request is answered locally and never reaches the engine.

These bounds are about how fast this instance spends its own egress IP. The
resubmission window below is about what it spends its quota on, which is a
different question.

### Submission size limits

How much one request may carry is configurable, because the two situations are
genuinely different:

```yaml
limits:
  max_urls_per_request: 100        # 1 to 10000
  max_body_bytes: 65536            # at least max_urls_per_request * 16, at most 8 MiB
```

The defaults are what this instance sends when the block is absent, and they are
far below IndexNow's own 10,000-URL ceiling on purpose: a code path that sends
hundreds of URLs is usually looping, and a small default makes that visible
instead of expensive. Raise them for a site migration, which is the documented
reason to submit a whole site at once.

The two are one decision and are checked together at startup. A body that cannot
carry `max_urls_per_request` entries makes every full batch a `413`, and nothing
in the count alone shows that - so a mismatched pair is refused when the config
loads, with the number to use. Sizing a body at about 256 bytes per URL always
fits.

Raising the count also grows the resubmission window's table, so one full batch
is always remembered; a raised count whose URLs the window then forgot would
resubmit every one of them immediately.

### The resubmission window

A URL sent less than 5 minutes ago is dropped from the batch before the request
is built, and counted in `skipped` instead of `submitted`. IndexNow asks for
this: do not resubmit a URL that has not changed, and wait at least 5 minutes
before resubmitting the same one (10 in another part of the same documentation -
5 is used here, because the window exists to stop a retry loop from burning
quota, not to police how often a page may change). Every submitted URL spends
crawl quota whether or not the engine already had it, so a caller polling on a
timer was previously paying for each poll.

- The window is per site host and per URL, keyed on the URL that was actually
sent, after cleaning: fragments stripped, duplicates collapsed. One site cannot
suppress another's URL.
- A URL is remembered only when the engine accepted the batch that carried it. A
failed submission leaves you a retry to make, and that retry is not suppressed.
- A batch in which everything is inside the window is **not** an error: 200,
`ok: true`, `submitted: 0`, `skipped: n`, and no `reason`. The engine is not
called.
- The table is bounded (4096 URLs of up to 2048 bytes each) so a caller cannot
grow this process's memory with arbitrary URLs. Past that the oldest entry is
forgotten, which costs at most one early resubmission.

The window is a quota guard, not a scheduler: it never sends anything later on
your behalf, and it does not decide whether a page changed. Deduplication inside
one request still happens for every caller; this adds the same thing across
requests, for five minutes.

A `200` means the batch was received. Resubmitting on a timer is still not a
faster route to being crawled.

There are two different `429`s and they are worth telling apart:

| `reason` | Owner | What happened |
|---|---|---|
| `throttled` | this instance | this host spent its burst; the engine was never called, and the egress IP is unharmed |
| `rate_limited` | the engine | the engine is already throttling this IP |

Both carry `Retry-After` and `retry_after_seconds`. Only the second one means
the operator's IP is being penalised; the first one is the guard doing its job.

## `GET /submit`

IndexNow's own single-URL form, for citing one page from a shell:

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "https://crawlping.example.net/submit?url=https%3A%2F%2Fexample.com%2Fblog%2Fnew-post"
```

Nothing about the submission changes: the same token, the same validation, the
same reasons, statuses and log line as the POST form, because it is the same
code. What the GET cannot do is carry a body, so the body cap and the JSON
decoder do not apply to it.

The `url` parameter may be repeated to send several URLs in one call
(`?url=a&url=b`). One is the common case. There is no separate limit for the GET
form: the URL ceiling and the duplicate collapsing are the same, and the
request line is bounded by the server's header limit rather than by a body cap.

A request with no `url` parameter at all is `400 bad_request`, exactly as an
empty `urls` list is on the POST form.

`GET /submit` answers, and so does `POST /submit`. Any other method gets `405`
with `Allow: GET, POST`. There is no `HEAD`: a HEAD would have to submit
something to be honest, and a HEAD is not supposed to have side effects.

## `GET /health`

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

### `GET /health?detail=1`

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

### `GET /health?detail=1&recheck=1`

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

## `GET /metrics`

Prometheus text format, for the operator rather than for the caller. Everything
here was already written to the log as one JSON line per submission; the point of
the endpoint is that a trend is only visible once it is aggregated, and IndexNow
does not publish its limits, so the trend is the only warning you get before
`429`.

```
crawlping_submissions_total{host}                            counter
crawlping_submission_reasons_total{host,reason}              counter
crawlping_engine_requests_total{host,status}                 counter
crawlping_engine_duration_seconds{host}                      histogram
crawlping_urls_submitted_total{host}                         counter
crawlping_urls_skipped_total{host}                           counter
crawlping_guard_refusals_total{host,kind}                    counter
crawlping_key_state{host}                                    gauge
```

`kind` is `throttled` (this instance's own rate bucket) or `overloaded` (no
engine slot). `crawlping_key_state` is `0` pending, `1` verified, `2` failed, the
same three states `/health` reports. A family with no observation yet carries its
`HELP` and `TYPE` lines and no series, so a scrape before the first submission is
not an error.

**The host label follows the rule `/health` already follows.** Without a token
every family is aggregated and carries no `host` label at all, because a
Cloudflare Tunnel makes this endpoint publicly reachable and the domain list is
not public information. A token unlocks the labels:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/metrics
```

Any configured site token does, not one per site: a token is something the
operator issued in the config file, not a user account, and a series set that
depended on which token was used would break scraping. When the labels are
aggregated, `crawlping_key_state` is reported as a count per state rather than as
one number, since adding up `0`, `1` and `2` across hosts would mean nothing.

No label is ever a URL, a path, a query, a user agent or a remote address.
`host` is bounded by the config, `reason` is the closed vocabulary below, and
`status` is a small set; anything else would be a memory leak the caller controls.

## Key states

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
it](../README.md#check-it-before-trusting-it).

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
| 400 | `too_many` | more URLs than `limits.max_urls_per_request` allows (100 by default) | no - split the batch |
| 400 | `non_default_port` | an entry names a port this instance will not send; IndexNow itself sets no port requirement | no - fix the caller |
| 400 | `out_of_key_scope` | a URL is outside the directory the key file covers | no - move the key file or stop submitting URLs outside its directory |
| 400 | `engine_rejected` | the engine refused this payload with a status that has no retry rule | no - resending it is pointless |
| 401 | `unknown_token` | missing or unknown bearer token, on `/submit` or `/health?detail=1` | no |
| 403 | `key_rejected` | the engine refused the key: missing, mismatched, or its own fetch of the file was blocked; also marks the host `failed` | no - fix the key |
| 405 | `method_not_allowed` | the method is not GET or POST; `Allow` names the two that work | no |
| 413 | `payload_too_large` | the body was larger than `limits.max_body_bytes` (65536 by default) | no - send fewer URLs |
| 422 | `host_mismatch` | a URL was not on the site's own host | no - fix the caller |
| 429 | `rate_limited` | the engine is throttling this IP; see `Retry-After` | yes, later |
| 429 | `throttled` | this instance refused to spend any more of its egress IP on this host right now; the engine was not called | yes, after `Retry-After` |
| 502 | `engine_error` | engine 5xx or the request never landed | yes, later |
| 503 | `overloaded` | no engine slot freed up within 2 seconds; `detail` says whether this site had used its own share or the whole instance had. The engine was not called | yes, shortly |

Only `429`, `502` and `503` are worth retrying. Every other status is the same
answer if the identical request is sent again. `engine_rejected` is the one to
read carefully: the engine understood the payload and refused it, so retrying is
pointless until the payload changes. An oversized body is reported as
`payload_too_large` rather than as `bad_request`, because the JSON may be valid
and only too large.

A batch the resubmission window emptied is not in this table because it is not a
refusal: it is a `200` with `ok: true`, `submitted: 0` and `skipped: n`.

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


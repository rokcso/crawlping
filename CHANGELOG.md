# Changelog

What changed for somebody running crawlping or calling its API. Newest first.

Releases are published from this file: the workflow reads the section for the tag
and uses it as the release body, so a version that is not written here cannot be
released. That is the point of keeping it — one file, written for the person
upgrading rather than for the commit log.

## How entries are written

Five rules, and they are the whole convention:

1. One section per version, `## [x.y.z] - YYYY-MM-DD`, newest first. `Unreleased`
   sits at the top and holds what the next version will say.
2. Only these four headings, in this order, and only when they have something in
   them: `Breaking`, `Added`, `Changed`, `Fixed`. Anything an existing deployment
   or caller has to act on is `Breaking` and goes first; everything else is
   news.
3. One line per thing a reader would notice, in the terms they use. "A batch
   larger than the cap was reported as `422 host_mismatch`" is an entry; "refactor
   submit path" is not, and neither is a commit subject.
4. Name what to do about it when there is something to do. A breaking entry
   without an instruction is a puzzle, not a changelog.
5. Move it from `Unreleased` into the version section when the tag is made, not
   after. The tag is what makes it public.

## [Unreleased]

## [0.2.1] - 2026-11-27

### Breaking

- **The container has to run as your uid, and the config has to be mounted as a
directory.** Both are new requirements, and both fail quietly if skipped. The
image runs as its own nonroot uid while `config.yaml` is mode `0600` and a bind
mount keeps its real owner on Linux, so the process cannot read the file it was
handed: it answers `read config: ... permission denied` and restart-loops while
the file sits right there. And a single-file mount hides `site add`'s atomic
rename, so a `SIGHUP` reload reads the old inode and the site count never changes.

  ```diff
  -  -v "$PWD/config.yaml:/etc/crawlping/config.yaml:ro"
  +  -v "$PWD:/etc/crawlping:ro"
  +  --user "$(id -u):$(id -g)"
  ```

  The in-container path is unchanged, so no other flag moves. Add `user:
  "1000:1000"` to compose. Then `chown` the config to your own uid and drop any
  `chown 65532` you added to work around the old failure. Restarting once here is
  fine; the reload matters after this point. No caller-side change: `/submit` and
  `/health` take the same requests and answer the same shapes. `/metrics` counts
  guard refusals differently, which its own entry below describes.

### Added

- `crawlping site add -host ...` generates the key and token, appends the entry,
  writes `<key>.txt` beside it, and prints the one step it cannot do: serve that
  file from the site. `-key` reuses a key whose file is already deployed,
  `-token` sets the token yourself, `-keydir` moves the key file. It edits the
  YAML node tree rather than re-rendering, so comments and key order survive; it
  writes through a temporary file and a rename, keeps the mode the config already
  had, and reads the result back with the loader the service itself uses.
- `crawlping site rm -host ...` and `crawlping site list` (token masked to its last
  four characters). `rm` refuses the last site, because a config with none does
  not load.
- `SIGHUP` reloads the config: `docker kill -s HUP crawlping` applies a new site
  list with no restart. The listener stays up, in-flight submissions finish, and
  the counters keep counting. A config that does not load is refused and the
  running one keeps serving. Only hosts the previous config did not carry are
  key-checked, so a reload cannot replace the engine's own `200` verdict with a
  key-file fetch that happens to be timing out.
- `crawlping doctor` answers whether the deployment can work at all, read-only, so
  it is safe against a live instance. It checks the config, that the listen
  address is free or already answered by this instance, that the engine is
  reachable by DNS + TCP + TLS, and one line per key file. It separates "the origin
  answered 200 with its fallback page" from "the file holds the wrong key".
- `crawlping doctor` checks the mode of the config file, which holds every site's
  bearer token. An editor that replaces the file, a `cp` from a backup or a
  `chmod -R` leaves it `0644` and nothing used to notice: the line reads
  `ok    mode     0600, readable only by the owner`, or `FAIL` naming the mode
  with `chmod 600` under it, and a failure exits non-zero. `0400` and `0700` pass
  too, because only the group and other bits are judged.
- Every documented `docker run` carries `--user "$(id -u):$(id -g)"`, and the
  compose file carries `user:`.

### Changed

- **`crawlping_submissions_total` counts guard refusals now.** A request the rate
  bucket or the concurrency guard refused is one submission, one `reason`
  (`throttled` or `overloaded`) and one guard `kind`, so
  `crawlping_guard_refusals_total` is a breakdown of `submissions_total` rather
  than a total beside it. A dashboard that added the two families together counts
  those requests twice from this version on: read the submissions family alone.
  An alert on the level of the submissions family sees a step equal to the refusal
  rate, which is the traffic it was already being refused. Those refusals carry no
  engine status and no latency sample, because nothing was sent.
- The per-site engine share is decided once, from the site count at startup, and a
  reload deliberately keeps the share it is running with - the share is the width
  of a semaphore that may already have slots held, and it cannot be resized
  without losing them. So a deployment that starts with one site keeps all 8 slots
  per host after `site add` adds a second, where the documented split for two
  sites is 4. The reload logs a warning naming the share that is running and the
  one a restart would apply, and `crawlping site add` says the same thing when it
  adds the second site: restart if you want the split.

### Fixed

- A container could not read the config it was given, and the documented `init`
  and Quick start paths both produced it. Running as the invoking uid makes the
  owner and the process the same; `init` now prints that flag and says why.
- A reload could read an inode the host had already replaced, silently: the signal
  arrived, the reload ran, the log said `config reloaded`, and the site count
  never changed. Every documented mount is a directory now, the smoke test asserts
  a replaced config reaches the running process, and a reload that reads bytes it
  already had logs a warning naming the cause.
- A reload that raised `limits.max_urls_per_request` left the resubmission
  window's table at the size the process started with, so one full batch could be
  larger than what the window could remember and the URLs it could not hold were
  sent again on the next attempt. The table grows to the new count now, and never
  shrinks back when the count does.
- Two concurrent batches could send the same URL twice: both read "not in the
  window" before either had written anything, so both spent crawl quota on it. A
  URL is claimed while its batch is in flight now, so the second request skips it,
  and the claim is given back when the batch is not accepted, so a failed
  submission still leaves your retry unsuppressed.
- A host removed from the config and added back later inherited its old key
  verdict: it came back `verified` with an old `checked_at` without anything
  having checked it. A host the new config no longer lists loses its record now,
  so a re-added one starts `pending` and has its key file checked again.
- The server's write budget could close the connection before the slowest
  legitimate answer was written. A body read (15s), a wait for an engine slot
  (2s) and the engine call (15s) come to 32s, and the budget was 30s, so an
  `overloaded` or `engine_error` answer the caller could act on arrived as a
  dropped connection. It is 40s now, and `timeout_test.go` keeps it above that
  sum.

## [0.2.0] - 2026-11-27

### Breaking

- **`GET /health` reports counts instead of a per-host map.** The body is
  `{"ok":true,"sites":1,"verified":1,"failed":0,"pending":0}`. Per-host detail
  moved to `GET /health?detail=1`, which now **requires a bearer token**: behind
  a Cloudflare Tunnel `/health` is publicly reachable, and the host list was
  public with it. A check that reads `keys` will not find it — read the counts,
  or move to `?detail=1` with a token.
- **A `key_location` in a subdirectory is refused at startup.** A key file
  covers only the directory it is served from, so `/assets/key.txt` answers for
  `/assets/...` and the engine refuses the rest with `422`. Point `key_location`
  at the host root, or leave it out.
- **A URL outside the key file's directory is refused locally** as
  `400 out_of_key_scope`, a reason that did not exist before. Move the key file
  to the host root to cover the whole host again.
- **A per-host rate limit can answer `429 throttled`.** Ten requests back to back
  per host, then one per second, refused locally without reaching the engine.
  `throttled` is not `rate_limited`: the first means this instance declined to
  spend the egress IP, the second means the engine is already penalising it. Both
  carry `Retry-After`, and a caller that used to burst without limit has to handle
  it.

### Added

- `crawlping init` writes `config.yaml` and `<key>.txt`, then prints the deploy
  steps. `-shape docker|binary|tunnel` decides `listen:`, the line that costs the
  most to get wrong, and `-force` names the key file it orphaned.
- `GET /submit?url=...` submits one URL without a JSON body — IndexNow's own
  single-URL form. `url` may repeat.
- `GET /metrics` in the Prometheus text format: submissions and reasons, engine
  status and duration, URLs sent and skipped, guard refusals, key states. Host
  labels require a token.
- `limits.max_urls_per_request` and `limits.max_body_bytes` make one request's
  size configurable, up to IndexNow's own 10,000-URL ceiling; the default stays
  100. A pair that cannot carry its own batch is refused when the config loads.
- A five-minute per-URL resubmission window drops URLs sent recently and reports
  them in `skipped`, so a timer that polls stops spending crawl quota on pages
  that did not change.
- Engine concurrency is capped per site as well as per instance, so one noisy
  site cannot hold every slot.
- Three key states — verified, failed and pending — replace the old boolean. A
  check that never reached an answer is no longer reported as a broken key file.
- `healthcheck -addr` dials an address directly instead of parsing the config, so
  editing the config no longer moves the probe away from the running process.

### Fixed

- Trailing bytes after the JSON object bypassed the body limit and were silently
  ignored: `json.Decoder` stops at the end of the first value, so a small object
  followed by megabytes never reached the cap, and a second object was dropped.
  The whole body is read now and trailing data is refused.
- `check` reported a key file green while the engine was refusing it, whenever the
  file was on another host or in a subdirectory.

## [0.1.0] - 2026-11-26

- First release. A self-hosted IndexNow submitter: one bearer token per site, the
  host resolved from the token rather than the request, so an internet-facing
  instance is not an open relay.
- Engine verdicts pass through as reasons instead of being flattened into success
  or failure: `429` and `502` mean retry later, `403` and `422` mean fix it.
- Key files are checked at startup so a broken one is visible immediately.
- No sitemap diffing, no retry loop, no database, no UI.

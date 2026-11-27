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
  fine; the reload matters after this point. No API change: `/submit`, `/health`
  and `/metrics` are as v0.2.0 left them.

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
- Every documented `docker run` carries `--user "$(id -u):$(id -g)"`, and the
  compose file carries `user:`.

### Fixed

- A container could not read the config it was given, and the documented `init`
  and Quick start paths both produced it. Running as the invoking uid makes the
  owner and the process the same; `init` now prints that flag and says why.
- A reload could read an inode the host had already replaced, silently: the signal
  arrived, the reload ran, the log said `config reloaded`, and the site count
  never changed. Every documented mount is a directory now, the smoke test asserts
  a replaced config reaches the running process, and a reload that reads bytes it
  already had logs a warning naming the cause.

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

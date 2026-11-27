#!/bin/sh
#
# Assembly smoke test for the default deployment path.
#
# Unit tests cannot see this failure mode. The service can start, report
# healthy from its own point of view, and still be unreachable from the host,
# because the published port and the configured listen address disagree. That
# is how this project broke twice, so this script builds the tree under review,
# starts it the way the README's default path does, and then checks the result
# from outside the container.
#
# It is deliberately the whole assembly and not a unit: config.example.yaml is
# the source of truth for the listen address, the image is built from the
# working tree, and every assertion is made through the host's published port.
#
# Usage, from the repository root:
#   sh scripts/compose-smoke.sh
#   SMOKE_PORT=18080 sh scripts/compose-smoke.sh   # busy machine
#
# Requires docker and curl.
set -eu

image=crawlping:smoke
container="crawlping-smoke-$$"
tmpdir=
image_built=0
container_started=0

cleanup() {
	if [ "$container_started" = 1 ]; then
		docker rm -f "$container" >/dev/null 2>&1 || true
	fi
	if [ "$image_built" = 1 ]; then
		docker rmi -f "$image" >/dev/null 2>&1 || true
	fi
	if [ -n "$tmpdir" ]; then
		rm -rf "$tmpdir"
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM

fail() {
	printf 'FAIL: %s\n' "$1" >&2
	exit 1
}

describe_failure() {
	printf 'container state: %s\n' \
		"$(docker inspect --format '{{.State.Status}} health {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || echo unknown)" >&2
	printf 'published ports:\n' >&2
	docker port "$container" 2>&1 >&2 || true
	printf 'container log (last 20 lines):\n' >&2
	docker logs --tail 20 "$container" 2>&1 >&2 || true
}

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

command -v docker >/dev/null 2>&1 || fail "docker is not installed or not on PATH"
command -v curl >/dev/null 2>&1 || fail "curl is not installed or not on PATH"
[ -f config.example.yaml ] || fail "config.example.yaml is missing from $repo_dir"

# The example file, not this script, decides which port the container listens
# on. Hardcoding it here would let the shipped example drift back to an address
# nobody can reach while the smoke test kept passing.
listen=$(awk '/^[[:space:]]*listen[[:space:]]*:/ {
	sub(/^[^:]*:[[:space:]]*/, "")
	sub(/[[:space:]]*#.*$/, "")
	gsub(/["\047[:space:]]/, "")
	print
	exit
}' config.example.yaml)
[ -n "$listen" ] || fail "config.example.yaml has no listen: line"

container_port=${listen##*:}
case $container_port in
'' | *[!0-9]*) fail "cannot read a port from listen: $listen" ;;
esac

host_port=${SMOKE_PORT:-$container_port}
case $host_port in
'' | *[!0-9]*) fail "SMOKE_PORT must be a number, got: $host_port" ;;
esac

site_host=$(awk '/^[[:space:]]*-[[:space:]]*host[[:space:]]*:/ {
	sub(/^[^:]*:[[:space:]]*/, "")
	sub(/[[:space:]]*#.*$/, "")
	gsub(/["\047[:space:]]/, "")
	print
	exit
}' config.example.yaml)
[ -n "$site_host" ] || fail "config.example.yaml has no site host, so the leak check would be vacuous"

tmpdir=$(mktemp -d)
config_file="$tmpdir/config.yaml"
cp config.example.yaml "$config_file"
chmod 600 "$config_file"

printf 'building %s from the working tree\n' "$image"
image_built=1
docker build -t "$image" . || fail "docker build failed"

# Same published port and same in-container config path as the README's default
# docker run: the host-side 127.0.0.1 prefix is what keeps the API local.
printf 'starting %s, publish 127.0.0.1:%s->%s, listen %s\n' \
	"$container" "$host_port" "$container_port" "$listen"
container_started=1
# --user, because the config is 0600 and the image runs as a nonroot uid: a bind
# mount keeps the file's real owner on Linux, so without this the container
# cannot read the file it was handed and restart-loops on "permission denied".
# Running as the invoking uid is the same thing the README tells an operator to
# do, so this exercises the documented path rather than a convenient variant.
docker run -d --name "$container" \
	--user "$(id -u):$(id -g)" \
	-p "127.0.0.1:${host_port}:${container_port}" \
	-v "${tmpdir}:/etc/crawlping:ro" \
	"$image" >/dev/null || fail "docker run failed"

health_url="http://127.0.0.1:${host_port}/health"
health_body="$tmpdir/health.json"
code=000
deadline=$(($(date +%s) + 20))

while :; do
	code=$(curl -sS -o "$health_body" -w '%{http_code}' --max-time 2 "$health_url" 2>/dev/null) || code=000
	if [ "$code" = "200" ]; then
		break
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		break
	fi
	sleep 1
done

if [ "$code" != "200" ]; then
	describe_failure
	fail "GET $health_url returned $code after 20s: the container reports its own state but the host cannot reach it (listen $listen, published 127.0.0.1:${host_port}:${container_port})"
fi

grep -q '"sites"' "$health_body" || fail "GET $health_url did not return a sites count: $(cat "$health_body")"

if grep -qF "$site_host" "$health_body"; then
	fail "anonymous /health leaked a configured host name ($site_host): $(cat "$health_body")"
fi

# The container's own healthcheck is the thing docker restarts on, so it has to
# agree with what the host just observed.
health=unknown
deadline=$(($(date +%s) + 70))
while :; do
	health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || echo unknown)
	if [ "$health" = "healthy" ]; then
		break
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		break
	fi
	sleep 1
done
[ "$health" = "healthy" ] || fail "container healthcheck reported '$health' after 70s, expected healthy"

# -addr must probe without reading the config: the path below does not exist,
# so this only passes while the flag keeps its precedence.
docker run --rm --network "container:${container}" "$image" \
	healthcheck -addr "127.0.0.1:${container_port}" -config "/nonexistent/config.yaml" >/dev/null 2>&1 ||
	fail "healthcheck -addr 127.0.0.1:${container_port} failed inside the container network"

# The reload path, end to end. This is the assembly version of a trap that no
# unit test can see: a config mounted as a single file keeps the inode the
# container started with, so replacing it on the host is invisible inside and
# SIGHUP reloads the bytes it already had - silently. `site add` then looks like
# it worked. Mounting the directory is what makes the replace visible, and this
# is the assertion that keeps the shipped shape doing that.
second_host="smoke-$$.example.org"
printf '\nadding %s to the mounted config, then reloading over docker kill -HUP\n' "$second_host"
docker run --rm --user "$(id -u):$(id -g)" \
	-v "${tmpdir}:/work" -w /work \
	--entrypoint /crawlping "$image" site add -host "$second_host" >/dev/null || fail "site add failed"

docker kill -s HUP "$container" >/dev/null || fail "docker kill -s HUP failed"

# The reload happens in the signal handler, so the answer is not immediate and
# there is no event to wait on: poll the count the new site changes.
reload_deadline=$(($(date +%s) + 15))
sites_now=0
while :; do
	sites_now=$(curl -sS --max-time 2 "$health_url" 2>/dev/null | sed -n 's/.*"sites":\([0-9]*\).*/\1/p')
	if [ "${sites_now:-0}" -ge 2 ] 2>/dev/null; then
		break
	fi
	if [ "$(date +%s)" -ge "$reload_deadline" ]; then
		break
	fi
	sleep 1
done

if [ "${sites_now:-0}" -lt 2 ] 2>/dev/null; then
	describe_failure
	fail "after site add and SIGHUP, /health still reports ${sites_now:-0} sites: the container is reading a config the host already replaced"
fi

printf 'PASS: default deployment path reachable and correct (127.0.0.1:%s/health, container healthy, reload picked up a new site)\n' "$host_port"

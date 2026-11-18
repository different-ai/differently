#!/bin/sh
# Proves deploy/compose/compose.yaml the way a user meets it: the file alone
# in an empty directory, the secrets made with the commands the guide prints
# (docs/deploy/docker.md), `docker compose up`, then one sandbox created,
# used and deleted with the CLI in the server image, and the state still
# there after the stack is recreated.
#
#   test/compose/check.sh                  the published image the file names
#   test/compose/check.sh --static         only the checks that need no Docker
#   test/compose/check.sh --default-image  print the image the file names
#
# DIFFERENTLY_IMAGE and DIFFERENTLY_VERSION choose another image, as they do
# for the Compose file (`make compose-check-built` uses one built from this
# checkout). DOCKER_CONTEXT chooses the Docker host.
set -eu

repo=$(cd "$(dirname "$0")/../.." && pwd)
file=$repo/deploy/compose/compose.yaml
guide=$repo/docs/deploy/docker.md

step() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# --- Checks that need no Docker ---------------------------------------------

default_image=$(sed -n 's/^ *image: "\${DIFFERENTLY_IMAGE:-\([^}]*\)}:\${DIFFERENTLY_VERSION:-\([^}]*\)}"$/\1:\2/p' "$file")
[ -n "$default_image" ] || fail "cannot read the default image from $file"
if [ "${1:-}" = "--default-image" ]; then
	echo "$default_image"
	exit 0
fi

step "the guide and the file name the same image and the same secrets commands"
# The fenced block after the marker comment in the guide.
secrets=$(awk '
	/<!-- test\/compose\/check.sh runs the next block -->/ { want = 1; next }
	want && /^```/ { fence++; next }
	want && fence == 1 { print }
	fence == 2 { exit }' "$guide")
[ -n "$secrets" ] || fail "cannot find the secrets block in $guide"
header=$(sed -n 's/^#      //p' "$file" | sed -n '/^img=/,/^chmod/p')
[ "$header" = "$secrets" ] || fail "the secrets commands at the top of compose.yaml and in docs/deploy/docker.md differ"
printf '%s\n' "$secrets" | grep -qx "img=$default_image" ||
	fail "the guide's secrets block does not use $default_image, the image compose.yaml defaults to"
echo "default image: $default_image"

# The end-to-end stack (test/e2e) is where these services are tested in
# depth. The two files stay separate, so check that this one runs the same
# images and runtime settings.
step "the same images and runtime settings as the end-to-end stack"
for src in test/e2e/compose.yaml test/e2e/config.yaml test/opensandbox/config.toml; do
	grep -E '^[[:space:]]*(image|execd_image|lineEndings|network_mode|host_ip|drop_capabilities|no_new_privileges|pids_limit|resolve_internal|isolation)[[:space:]]*[:=]' "$repo/$src" |
		grep -v 'differently-server:e2e' | sed 's/^[[:space:]]*//; s/[[:space:]]*#.*$//' |
		while IFS= read -r line; do
			grep -qF -- "$line" "$file" || fail "$src has '$line', deploy/compose/compose.yaml does not"
			echo "same: $line"
		done
done

[ "${1:-}" != "--static" ] || exit 0

# --- The stack ---------------------------------------------------------------

image=${DIFFERENTLY_IMAGE:-${default_image%:*}}:${DIFFERENTLY_VERSION:-${default_image##*:}}
bind=${DIFFERENTLY_BIND:-127.0.0.1}
port=${DIFFERENTLY_PORT:-8080}
publish=${SANDBOX_PUBLISH_HOST:-172.17.0.1}
# Only the generated .env may supply these.
unset DIFFERENTLY_BOOTSTRAP_TOKEN DIFFERENTLY_SECRETS_KEY POSTGRES_PASSWORD OPENSANDBOX_API_KEY

work=$(mktemp -d)
started=
id=
cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ -n "$started" ]; then
		if [ "$status" -ne 0 ]; then
			docker compose ps -a || true
			docker ps -a --format '{{.Names}}\t{{.Status}}\t{{.Ports}}' || true
			docker compose logs --no-color --tail 200 || true
			# A sandbox is a container beside the stack: do not leave it behind.
			[ -z "$id" ] || cli sandbox delete "$id" >/dev/null 2>&1 || true
		fi
		docker compose down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT INT TERM

cd "$work"
cp "$file" compose.yaml # the one file a user downloads
cli() { docker compose exec -T server differently "$@"; }
# first_id PREFIX reads the first "id" with that prefix from indented JSON.
first_id() { sed -n "s/.*\"id\": *\"\\($1_[^\"]*\\)\".*/\\1/p" | head -n 1; }
# Every published port of the containers that publish a sandbox control port (44772).
sandbox_bindings() { docker ps --format '{{.Ports}}' | grep -e '->44772/tcp' | tr ',' '\n' | sed 's/^ *//' | grep -e '->' || true; }

step "without .env it refuses to start and says what to do"
if out=$(docker compose config -q 2>&1); then
	fail "docker compose accepted the file with no secrets set"
fi
case $out in
*"create .env first"*) echo "$out" ;;
*) fail "the refusal does not say what to do: $out" ;;
esac

step "create the secrets with the guide's commands (image: $image)"
printf '%s\n' "$secrets" | sed "s|^img=.*|img=$image|" | sh -eu
for name in DIFFERENTLY_BOOTSTRAP_TOKEN DIFFERENTLY_SECRETS_KEY POSTGRES_PASSWORD OPENSANDBOX_API_KEY; do
	grep -q "^$name=..*" .env || fail "$name is empty in the generated .env"
done
[ "$(wc -l <.env | tr -d ' ')" = 4 ] || fail ".env does not have exactly four lines"
[ "$(sort -u .env | cut -d= -f2- | sort -u | wc -l | tr -d ' ')" = 4 ] || fail "two secrets in .env have the same value"
case $(ls -l .env) in
-rw-------*) ;;
*) fail ".env is readable by others: $(ls -l .env)" ;;
esac
docker compose config -q

step "docker compose up -d --wait"
started=1
docker compose up -d --wait --wait-timeout 300
docker compose ps

step "the API is published on $bind:$port only, and answers"
[ "$(docker compose port server 8080)" = "$bind:$port" ] || fail "server port 8080 is published as $(docker compose port server 8080)"
curl -fsS "http://$bind:$port/healthz"
echo

step "the operator key works and the OpenSandbox pool is healthy"
tries=0
until out=$(cli doctor 2>&1) && printf '%s\n' "$out" | grep -q 'pool opensandbox (private): healthy'; do
	tries=$((tries + 1))
	[ "$tries" -lt 30 ] || fail "the pool did not become healthy: $out"
	sleep 2
done
printf '%s\n' "$out"

step "OpenSandbox refuses a call without its key"
code=$(docker compose exec -T opensandbox-server python -c "
import urllib.error, urllib.request
try:
    urllib.request.urlopen('http://localhost:8090/v1/sandboxes')
    print(200)
except urllib.error.HTTPError as e:
    print(e.code)
")
[ "$code" = 401 ] || fail "OpenSandbox answered $code, not 401, to a call without a key"
echo "401"

step "create an organization with a default project"
project=$(cli admin org create first-team | first_id prj)
[ -n "$project" ] || fail "no project ID in the answer"
echo "$project"

step "create a sandbox, run a command in it, delete it"
id=$(cli --project "$project" sandbox create --wait -o json | first_id sbx)
[ -n "$id" ] || fail "no sandbox ID in the answer"
echo "$id is running"
out=$(cli sandbox exec "$id" -- echo hello)
[ "$out" = hello ] || fail "echo hello printed: $out"
echo "$out"

bindings=$(sandbox_bindings)
[ -n "$bindings" ] || fail "no running container publishes a sandbox control port (44772)"
printf '%s\n' "$bindings"
if printf '%s\n' "$bindings" | grep -v "^$publish:" >/dev/null; then
	fail "a sandbox port is published on an address other than $publish"
fi

cli sandbox delete "$id"
tries=0
while [ -n "$(sandbox_bindings)" ]; do
	tries=$((tries + 1))
	[ "$tries" -lt 30 ] || fail "the sandbox's container is still there after delete: $(sandbox_bindings)"
	sleep 2
done
echo "deleted, and its container is gone"

step "state survives removing and recreating the containers"
docker compose down
docker compose up -d --wait --wait-timeout 300
cli admin org list | grep -q first-team || fail "the organization is gone after the stack was recreated"
echo "the organization is still there, and the same .env still opens the database"

step "ok"

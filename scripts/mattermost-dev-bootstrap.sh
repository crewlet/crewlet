#!/usr/bin/env bash
#
# mattermost-dev-bootstrap.sh — stand up the docker-compose Mattermost
# instance for local integration testing of the Crewlet Mattermost
# integration.
#
# It waits for the server, creates the admin account (the FIRST user on a
# fresh install is auto-promoted to system admin — that is the account the
# provisioner authenticates as), mints a personal access token for it and
# writes it straight to the env file, reconciles the server's own Site URL
# with the address browsers use, creates the team and its channels, proves
# a websocket can be opened, and — with COMPANY= set — provisions the agent
# bot accounts.
#
# Credentials are persisted the moment they exist, before any check that
# can abort: Mattermost returns a personal access token's value exactly
# once, so a gate that fails between minting and writing strands a live
# admin credential nobody can read.
#
# The company config references this instance as ${MATTERMOST_URL} — no
# copy or sed needed: this script writes MATTERMOST_URL and
# MATTERMOST_ADMIN_TOKEN into the env file, and every consumer (provision,
# the engine) resolves the ref from the sourced environment.
#
# Usage (from the repo root):
#   docker compose --profile mattermost up -d --wait
#   scripts/mattermost-dev-bootstrap.sh
#   # or, provision the agent bots in the same run:
#   COMPANY=my_company.yaml scripts/mattermost-dev-bootstrap.sh
#
# THE PUBLIC URL. Mattermost hands its own `ServiceSettings.SiteURL` to
# every browser that loads the web app, and the web app and its plugins
# build absolute URLs from it. Point it at localhost while people browse
# from somewhere else and the client tries to reach `localhost:8065` from
# the READER's machine — which is why a wrong Site URL shows up as
# ERR_CONNECTION_REFUSED on `/plugins/...` requests and as a live feed
# that never updates until you refresh. So this script resolves the
# address browsers will use, in order:
#
#   1. $MATTERMOST_PUBLIC_URL, when set — always wins;
#   2. the address you reached THIS host on over SSH ($SSH_CONNECTION's
#      server field) — if you had to SSH in, your browser is not on this
#      machine either, and that address is one it can reach;
#   3. http://localhost:$MATTERMOST_LISTEN_PORT — the laptop loop.
#
# It then makes the server agree with it (recreating the compose service
# when the setting is environment-managed, patching it over the API
# otherwise) and refuses to finish while the two disagree, because a
# silent disagreement is exactly the failure this reconcile exists to
# prevent.
#
# Unlike the gitlab and plane loops, nothing here has to reach the engine:
# Mattermost has no usable inbound webhook, so the engine dials OUT over a
# websocket per agent seat. This stack works behind NAT with no tunnel.
#
# Re-runnable: every step is idempotent.
set -euo pipefail

PORT="${MATTERMOST_LISTEN_PORT:-8065}"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33m    %s\033[0m\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# 0. The address BROWSERS will use
# ---------------------------------------------------------------------------
url_source="MATTERMOST_PUBLIC_URL"
URL="${MATTERMOST_PUBLIC_URL:-}"
if [ -z "$URL" ] && [ -n "${SSH_CONNECTION:-}" ]; then
  # "<client-ip> <client-port> <server-ip> <server-port>" — the third
  # field is the address the operator reached this host on, which is by
  # construction an address that resolves and routes from where they are
  # sitting. IPv6 needs the brackets a URL requires.
  ssh_host=$(printf '%s\n' "$SSH_CONNECTION" | awk '{print $3}')
  if [ -n "${ssh_host:-}" ]; then
    case "$ssh_host" in
      *:*) ssh_host="[${ssh_host}]" ;;
    esac
    URL="http://${ssh_host}:${PORT}"
    url_source="SSH_CONNECTION"
  fi
fi
if [ -z "$URL" ]; then
  URL="http://localhost:${PORT}"
  url_source="default"
fi
URL="${URL%/}"

# The address THIS SCRIPT talks to. Always local — the published port is on
# this host even when the public URL names a remote address for browsers.
API="http://localhost:${PORT}/api/v4"

ENV_FILE="${ENV_FILE:-.env}"
TEAM="${MATTERMOST_TEAM:-nimbus}"
TEAM_DISPLAY="${MATTERMOST_TEAM_DISPLAY:-Nimbus}"
# Channels the agent bots are added to. Keep in lock-step with the company
# config's `integrations.mattermost.provisioning.channels`.
CHANNELS="${MATTERMOST_CHANNELS:-engineering product}"

ADMIN_EMAIL="${MATTERMOST_ADMIN_EMAIL:-founder@nimbus.local}"
ADMIN_USER="${MATTERMOST_ADMIN_USERNAME:-founder}"
# Mattermost's default password policy needs 8+ chars; this is a dev-only
# literal, like the GITLAB_ROOT_PASSWORD default in docker-compose.yml.
ADMIN_PASS="${MATTERMOST_ADMIN_PASSWORD:-crewlet-dev-password}"

# ---------------------------------------------------------------------------
# JSON is parsed with python3 rather than sed. A greedy `.*"id":"..."`
# match returns the LAST id in a payload, not the first, which is a bug
# waiting for the day Mattermost adds a field.
# ---------------------------------------------------------------------------
json_get() {
  # json_get <json> <dotted.path> — empty string when absent.
  python3 -c '
import json, sys
try:
    value = json.loads(sys.argv[1])
except ValueError:
    sys.exit(0)
for key in sys.argv[2].split("."):
    if not isinstance(value, dict):
        sys.exit(0)
    value = value.get(key)
    if value is None:
        sys.exit(0)
print(value if isinstance(value, str) else json.dumps(value))
' "$1" "$2"
}

# ---------------------------------------------------------------------------
# 1. Wait for the server
# ---------------------------------------------------------------------------
say "Waiting for Mattermost at ${API} ..."
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null "${API}/system/ping" 2>/dev/null; then
    echo "    up after ${i}0s"
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "Mattermost did not answer /system/ping within 10 minutes." >&2
    echo "Check: docker compose --profile mattermost logs mattermost" >&2
    exit 1
  fi
  sleep 10
done

# ---------------------------------------------------------------------------
# 2. Admin account (first user is promoted to system admin automatically)
# ---------------------------------------------------------------------------
say "Creating the admin account (${ADMIN_USER}) ..."
create_body=$(printf '{"email":"%s","username":"%s","password":"%s"}' \
  "$ADMIN_EMAIL" "$ADMIN_USER" "$ADMIN_PASS")
# Tolerate "already exists" — this script is re-runnable.
curl -fsS -o /dev/null -X POST "${API}/users" \
  -H 'Content-Type: application/json' -d "$create_body" 2>/dev/null \
  && echo "    created" \
  || echo "    already exists (or signup closed) — continuing"

say "Logging in ..."
login_body=$(printf '{"login_id":"%s","password":"%s"}' "$ADMIN_USER" "$ADMIN_PASS")
# The session token comes back in a RESPONSE HEADER, not the body.
headers=$(mktemp)
user_json=$(curl -fsS -D "$headers" -X POST "${API}/users/login" \
  -H 'Content-Type: application/json' -d "$login_body")
SESSION=$(awk 'BEGIN{IGNORECASE=1} /^token:/ {print $2}' "$headers" | tr -d '\r')
rm -f "$headers"
if [ -z "$SESSION" ]; then
  echo "Login failed for ${ADMIN_USER}. If you changed the password after a" >&2
  echo "previous run, set MATTERMOST_ADMIN_PASSWORD to the current one." >&2
  exit 1
fi
USER_ID=$(json_get "$user_json" id)
echo "    logged in as ${ADMIN_USER} (${USER_ID})"

auth=(-H "Authorization: Bearer ${SESSION}")

# ---------------------------------------------------------------------------
# 3. A durable personal access token for the provisioner
# ---------------------------------------------------------------------------
# The session token above expires; the provisioner wants a PAT. Reuse the
# one this script minted on a previous run rather than accumulating a new
# token per run — Mattermost returns a token's value ONLY at creation, so a
# re-mint would strand the old one.
say "Minting the provisioning token ..."
TOKEN_DESC="crewlet-dev-bootstrap"
existing=$(curl -fsS "${auth[@]}" "${API}/users/${USER_ID}/tokens" 2>/dev/null || echo '[]')
if printf '%s' "$existing" | grep -q "\"description\":\"${TOKEN_DESC}\""; then
  echo "    a '${TOKEN_DESC}' token already exists."
  if grep -q '^MATTERMOST_ADMIN_TOKEN=' "$ENV_FILE" 2>/dev/null; then
    ADMIN_TOKEN=$(grep '^MATTERMOST_ADMIN_TOKEN=' "$ENV_FILE" | tail -1 | cut -d= -f2-)
    echo "    reusing the value in ${ENV_FILE}"
  else
    echo "    ...but ${ENV_FILE} has no MATTERMOST_ADMIN_TOKEN, and the value" >&2
    echo "    is unrecoverable (Mattermost returns it once). Revoke the old" >&2
    echo "    token in Profile > Security > Personal Access Tokens and re-run." >&2
    exit 1
  fi
else
  token_json=$(curl -fsS -X POST "${API}/users/${USER_ID}/tokens" "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"description\":\"${TOKEN_DESC}\"}")
  ADMIN_TOKEN=$(json_get "$token_json" token)
  echo "    minted"
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
  echo "Could not obtain an admin token." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 4. Persist the coordinates the config + provisioner resolve
# ---------------------------------------------------------------------------
# Written HERE, the moment the values exist, not at the end of the run:
# Mattermost returns a personal access token's value exactly ONCE, so a
# later step that fails before this point would strand a live admin
# credential nobody can read — and this script's own recovery advice for
# that is "revoke the old token and re-run". Same write-through
# discipline the provisioning sinks follow (src/crewlet/provisioning.py).
#
# MATTERMOST_PUBLIC_URL lands in the env file too, and docker compose reads
# `.env` from the project directory on every invocation — so a later
# `docker compose --profile mattermost up -d` keeps the Site URL this run
# settled on instead of silently reverting it to localhost.
say "Writing MATTERMOST_URL + MATTERMOST_PUBLIC_URL + MATTERMOST_ADMIN_TOKEN to ${ENV_FILE}"
touch "$ENV_FILE"
chmod 600 "$ENV_FILE"
python3 - "$ENV_FILE" "$URL" "$ADMIN_TOKEN" <<'PY'
import pathlib, sys

path, url, token = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
values = {
    "MATTERMOST_URL": url,
    "MATTERMOST_PUBLIC_URL": url,
    "MATTERMOST_ADMIN_TOKEN": token,
}

lines = path.read_text().splitlines() if path.exists() else []
seen = set()
for i, line in enumerate(lines):
    key = line.split("=", 1)[0].strip().removeprefix("export ").strip()
    if key in values:
        lines[i] = f"{key}={values[key]}"
        seen.add(key)
lines.extend(f"{k}={v}" for k, v in values.items() if k not in seen)
path.write_text("\n".join(lines) + "\n")
PY

# ---------------------------------------------------------------------------
# 5. Site URL — the setting every browser inherits
# ---------------------------------------------------------------------------
# Runs AFTER the admin token exists, because repairing it needs one: an
# MM_* environment variable outranks stored config, so the compose stack
# is fixed by recreating the container, and everything else by
# `PUT /config/patch` — which needs a system admin. Reading is
# unauthenticated: `/config/client` carries exactly what the web app is
# handed, so this compares the value the BROWSER will act on rather than
# the one an admin API might report. `format=old` is required — without
# it, servers up to v10.5 answer 501.
site_url() {
  local body
  body=$(curl -fsS "${API}/config/client?format=old" 2>/dev/null || echo '')
  [ -n "$body" ] || return 0
  json_get "$body" SiteURL
}

say "Reconciling the Site URL with the address browsers use"
echo "    browsers:  ${URL}   (from ${url_source})"
CURRENT_SITE_URL=$(site_url)
echo "    server:    ${CURRENT_SITE_URL:-<unset>}"

if [ "${CURRENT_SITE_URL%/}" != "$URL" ]; then
  fixed=""
  # An MM_* environment variable OUTRANKS anything written over the API,
  # so for the compose stack the only fix that takes is recreating the
  # container with the right value. Try that first, when this IS the
  # compose stack.
  mm_container=""
  if command -v docker >/dev/null 2>&1; then
    mm_container=$(docker compose --profile mattermost ps -q mattermost 2>/dev/null || true)
  fi
  if [ -n "$mm_container" ]; then
    say "Recreating the mattermost service with MATTERMOST_PUBLIC_URL=${URL}"
    if MATTERMOST_PUBLIC_URL="$URL" docker compose --profile mattermost up -d --wait mattermost; then
      # Poll for the new VALUE, not merely for an answer: during a
      # recreate the old container can still be serving, and a
      # first-non-empty read would happily accept its stale Site URL.
      for _ in $(seq 1 30); do
        CURRENT_SITE_URL=$(site_url)
        if [ "${CURRENT_SITE_URL%/}" = "$URL" ]; then
          break
        fi
        sleep 2
      done
      if [ "${CURRENT_SITE_URL%/}" = "$URL" ]; then
        fixed="recreated"
      fi
    fi
  fi

  # A Site URL that is loopback or unset is wrong for any browser that is
  # not on this host, so this script will repair it. Any OTHER address is
  # a value somebody chose — a hostname the team browses to, a proxy in
  # front — and overwriting that on a server this script does not own
  # would break every link for everyone else. So: repair the obviously
  # wrong, report the merely different.
  reported_host=${CURRENT_SITE_URL#*://}
  reported_host=${reported_host%%[:/]*}
  case "${reported_host:-unset}" in
    localhost|127.0.0.1|0.0.0.0|::1|unset) repairable=1 ;;
    *) repairable=0 ;;
  esac

  # Not our container, or the recreate did not take: patch it over the
  # API — but only when the setting is not environment-managed. An MM_*
  # variable is re-applied on every config write, so a PUT against one
  # returns 200 and changes nothing; attempting it would trade a clear
  # failure for a confusing one. (The System Console greys the field out
  # for the same reason.) `/config/patch` is a PUT: Mattermost has no
  # PATCH verb here, and `PUT /config` would demand — and clobber — the
  # whole config document.
  if [ -z "$fixed" ] && [ "$repairable" = "1" ]; then
    env_managed=$(curl -fsS "${API}/config/environment" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null || echo '{}')
    if [ "$(json_get "$env_managed" ServiceSettings.SiteURL)" = "true" ]; then
      warn "SiteURL is set from the environment (MM_SERVICESETTINGS_SITEURL);"
      warn "the API cannot change it — the container has to be recreated."
    else
      say "Patching ServiceSettings.SiteURL over the API"
      if curl -fsS -o /dev/null -X PUT "${API}/config/patch" \
          -H "Authorization: Bearer ${ADMIN_TOKEN}" \
          -H 'Content-Type: application/json' \
          -d "{\"ServiceSettings\":{\"SiteURL\":\"${URL}\"}}" 2>/dev/null; then
        CURRENT_SITE_URL=$(site_url)
        if [ "${CURRENT_SITE_URL%/}" = "$URL" ]; then
          fixed="patched"
        fi
      fi
    fi
  fi

  if [ -n "$fixed" ]; then
    echo "    now:       ${CURRENT_SITE_URL}  (${fixed})"
  elif [ "$repairable" = "0" ]; then
    warn "Site URL is ${CURRENT_SITE_URL}, and this run resolved ${URL}."
    warn "Both are real addresses, so this script will not overwrite one"
    warn "somebody chose. If people browse to ${CURRENT_SITE_URL}, that is"
    warn "correct and nothing is wrong — Mattermost accepts a websocket"
    warn "only from a browser whose Origin matches it, so whichever address"
    warn "they type is the one SiteURL has to name. The websocket check"
    warn "below probes ${URL}; a 403 there means this is the wrong one."
  else
    warn "Site URL is still ${CURRENT_SITE_URL:-<unset>}, not ${URL}."
    warn ""
    warn "Mattermost accepts a websocket upgrade only when the browser's"
    warn "Origin matches SiteURL exactly, host AND scheme, so every browser"
    warn "on any other address gets 403 on /api/v4/websocket and falls back"
    warn "to refresh-to-see-new-messages. Plugins compound it: they build"
    warn "their URLs from SiteURL too, so their requests go to a host the"
    warn "reader's machine cannot reach. Fix it with:"
    warn ""
    warn "  MATTERMOST_PUBLIC_URL=${URL} \\"
    warn "    docker compose --profile mattermost up -d --wait"
    warn ""
    warn "MM_* values are read-only in the System Console by design, so for"
    warn "the compose stack recreating the container is the only fix that"
    warn "takes. On a Mattermost you configure by hand, set it under"
    warn "System Console > Environment > Web Server."
    exit 1
  fi
else
  echo "    match"
fi

# ---------------------------------------------------------------------------
# 6. Team + channels
# ---------------------------------------------------------------------------
say "Creating the '${TEAM}' team ..."
team_json=$(curl -fsS "${auth[@]}" "${API}/teams/name/${TEAM}" 2>/dev/null || echo '')
if [ -z "$team_json" ]; then
  team_json=$(curl -fsS -X POST "${API}/teams" "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"${TEAM}\",\"display_name\":\"${TEAM_DISPLAY}\",\"type\":\"O\"}")
  echo "    created"
else
  echo "    already exists"
fi
TEAM_ID=$(json_get "$team_json" id)

# The admin has to be IN the team to administer its channels.
curl -fsS -o /dev/null -X POST "${API}/teams/${TEAM_ID}/members" "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d "{\"team_id\":\"${TEAM_ID}\",\"user_id\":\"${USER_ID}\"}" 2>/dev/null || true

say "Creating channels: ${CHANNELS}"
for ch in $CHANNELS; do
  if curl -fsS -o /dev/null "${auth[@]}" \
      "${API}/teams/${TEAM_ID}/channels/name/${ch}" 2>/dev/null; then
    echo "    ${ch}: exists"
    continue
  fi
  curl -fsS -o /dev/null -X POST "${API}/channels" "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"team_id\":\"${TEAM_ID}\",\"name\":\"${ch}\",\"display_name\":\"${ch}\",\"type\":\"O\"}"
  echo "    ${ch}: created"
done

# ---------------------------------------------------------------------------
# 7. Prove a websocket opens
# ---------------------------------------------------------------------------
# The one check that exercises what actually breaks in practice. Everything
# above can pass against a server whose websocket endpoint is unreachable —
# and a Mattermost whose websocket never connects looks ALIVE: pages load,
# messages send, and only the live feed is dead, so readers refresh to see
# replies instead of reporting an outage. It is also the engine's ONLY
# inbound path, so a failure here means no agent ever hears anything.
say "Checking the websocket upgrade"
ws_check() {
  python3 - "$1" <<'PY'
import base64, os, socket, ssl, sys
from urllib.parse import urlparse

url = urlparse(sys.argv[1])
host = url.hostname or "localhost"
port = url.port or (443 if url.scheme == "https" else 80)
path = (url.path or "").rstrip("/") + "/api/v4/websocket"
try:
    sock = socket.create_connection((host, port), timeout=10)
except OSError as exc:
    print(f"unreachable: {exc}")
    sys.exit(2)
try:
    if url.scheme == "https":
        sock = ssl.create_default_context().wrap_socket(sock, server_hostname=host)
    request = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {url.netloc}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {base64.b64encode(os.urandom(16)).decode()}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        f"Origin: {url.scheme}://{url.netloc}\r\n"
        "\r\n"
    )
    sock.sendall(request.encode())
    raw = sock.recv(4096).decode("latin-1", "replace")
except OSError as exc:
    print(f"handshake failed: {exc}")
    sys.exit(2)
finally:
    sock.close()
status = raw.split("\r\n", 1)[0] if raw else "<no response>"
print(status)
sys.exit(0 if " 101 " in f" {status} " else 1)
PY
}
ws_status=$(ws_check "$URL") && ws_ok=1 || ws_ok=0
if [ "$ws_ok" = "1" ]; then
  echo "    ${URL} → ${ws_status}"
else
  echo "    ${URL} → ${ws_status}"
  # Distinguish "Mattermost refuses to upgrade" from "something in front
  # of Mattermost eats the Upgrade header" — the fix is in a different
  # place for each, and the published port answers the question.
  local_url="http://localhost:${PORT}"
  local_status=$(ws_check "$local_url") && local_ok=1 || local_ok=0
  echo "    ${local_url} → ${local_status}"
  warn ""
  case "$ws_status" in
    *40[13]*)
      # gorilla answers a rejected CheckOrigin with 403 — and the only
      # input to Mattermost's checker is SiteURL. Reaching here means the
      # reconcile above settled on an address this probe is not sending.
      warn "The server rejected the upgrade rather than dropping it, which"
      warn "is Mattermost's origin check: it accepts a websocket only from"
      warn "a browser whose Origin matches ServiceSettings.SiteURL exactly"
      warn "(${CURRENT_SITE_URL:-<unset>}), host and scheme. Set"
      warn "MATTERMOST_PUBLIC_URL to the address people actually type."
      ;;
    *)
      if [ "$local_ok" = "1" ]; then
        warn "Mattermost upgrades fine on its own published port, but ${URL}"
        warn "does not — so the break is IN FRONT of Mattermost, not in it:"
        warn "  * a reverse proxy that does not forward the Upgrade and"
        warn "    Connection headers (nginx needs proxy_set_header Upgrade"
        warn "    \$http_upgrade; proxy_set_header Connection \"upgrade\";)"
        warn "  * a firewall, CDN or load balancer between here and that"
        warn "    address that allows HTTP but not websockets."
      else
        warn "Mattermost did not upgrade on its own published port either."
        warn "Check: docker compose --profile mattermost logs mattermost"
      fi
      ;;
  esac
  warn ""
  warn "Live updates in the web app and every inbound message the engine"
  warn "sees travel over this connection, so both stay dead until it works."
  exit 1
fi

# ---------------------------------------------------------------------------
# 8. Optional: provision the agent bots
# ---------------------------------------------------------------------------
if [ -n "${COMPANY:-}" ]; then
  say "Provisioning agent bots from ${COMPANY}"
  MATTERMOST_ADMIN_TOKEN="$ADMIN_TOKEN" MATTERMOST_URL="$URL" \
    crewlet mattermost provision "$COMPANY" --env-file "$ENV_FILE"
else
  say "Skipping bot provisioning (set COMPANY=<company.yaml> to include it)"
fi

cat <<EOF

$(printf '\033[1mDone.\033[0m')

  Mattermost   ${URL}
  Admin        ${ADMIN_USER} / ${ADMIN_PASS}
  Team         ${TEAM}
  Env file     ${ENV_FILE}  (MATTERMOST_URL, MATTERMOST_PUBLIC_URL, MATTERMOST_ADMIN_TOKEN)

Next:
  1. Point your company config at this instance:

       integrations:
         mattermost:
           enabled: true
           url: "\${MATTERMOST_URL}"
           team: ${TEAM}

  2. Provision the bots (if you did not pass COMPANY= above):

       crewlet mattermost provision <company.yaml>

  3. Check the whole path before you boot:

       crewlet mattermost doctor <company.yaml>

  4. Start the engine — it opens one websocket per agent seat:

       crewlet run config.yaml

Nothing needs to reach the engine from Mattermost, so no tunnel is needed.
See docs/integrations/mattermost.md for the full walkthrough.
EOF

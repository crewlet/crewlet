#!/usr/bin/env bash
#
# mattermost-dev-bootstrap.sh — stand up the docker-compose Mattermost
# instance for local integration testing of the Crewlet Mattermost
# integration.
#
# It waits for the server, creates the admin account (the FIRST user on a
# fresh install is auto-promoted to system admin — that is the account the
# provisioner authenticates as), mints a personal access token for it,
# creates the team and its channels, and — with COMPANY= set — provisions
# the agent bot accounts.
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
# On a REMOTE host, set MATTERMOST_PUBLIC_URL (compose + this script read
# it) to the address browsers use, so the server's own links carry that
# address instead of localhost:
#   MATTERMOST_PUBLIC_URL=http://203.0.113.7:8065 docker compose --profile mattermost up -d --wait
#   MATTERMOST_PUBLIC_URL=http://203.0.113.7:8065 scripts/mattermost-dev-bootstrap.sh
#
# Unlike the gitlab and plane loops, nothing here has to reach the engine:
# Mattermost has no usable inbound webhook, so the engine dials OUT over a
# websocket per agent seat. This stack works behind NAT with no tunnel.
#
# Re-runnable: every step is idempotent.
set -euo pipefail

PORT="${MATTERMOST_LISTEN_PORT:-8065}"
URL="${MATTERMOST_PUBLIC_URL:-http://localhost:${PORT}}"
# The address THIS SCRIPT talks to. Always local — the published port is on
# this host even when MATTERMOST_PUBLIC_URL names a remote address for
# browsers.
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

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

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
USER_ID=$(printf '%s' "$user_json" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
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
  ADMIN_TOKEN=$(printf '%s' "$token_json" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' | head -1)
  echo "    minted"
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
  echo "Could not obtain an admin token." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 4. Team + channels
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
TEAM_ID=$(printf '%s' "$team_json" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)

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
# 5. Persist the coordinates the config + provisioner resolve
# ---------------------------------------------------------------------------
say "Writing MATTERMOST_URL + MATTERMOST_ADMIN_TOKEN to ${ENV_FILE}"
touch "$ENV_FILE"
chmod 600 "$ENV_FILE"
python3 - "$ENV_FILE" "$URL" "$ADMIN_TOKEN" <<'PY'
import pathlib, sys

path, url, token = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
values = {"MATTERMOST_URL": url, "MATTERMOST_ADMIN_TOKEN": token}

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
# 6. Optional: provision the agent bots
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
  Env file     ${ENV_FILE}  (MATTERMOST_URL, MATTERMOST_ADMIN_TOKEN)

Next:
  1. Point your company config at this instance:

       integrations:
         mattermost:
           enabled: true
           url: "\${MATTERMOST_URL}"
           team: ${TEAM}

  2. Provision the bots (if you did not pass COMPANY= above):

       crewlet mattermost provision <company.yaml>

  3. Start the engine — it opens one websocket per agent seat:

       crewlet run config.yaml

Nothing needs to reach the engine from Mattermost, so no tunnel is needed.
See docs/integrations/mattermost.md for the full walkthrough.
EOF

#!/usr/bin/env bash
#
# plane-dev-bootstrap.sh — stand up the docker-compose Plane fork for local
# integration testing of the Crewlet Plane integration.
#
# It waits for the Plane API to come up (migrations included), creates the
# founder (instance admin) plus their personal API token via an idempotent
# django-shell pass, creates the `nimbus` workspace through the public API,
# archives the demo project Plane seeds into fresh workspaces, and — with
# COMPANY= set — provisions the Nimbus agent seats and imports the example
# docs + tool skills. It ends by printing the next steps (engine + webhook
# receiver).
#
# examples/nimbus.company.yaml references this instance as ${PLANE_URL} —
# no copy or sed needed: this script writes PLANE_URL into the env file,
# and every consumer (provision, import, the engine) resolves the ref from
# the sourced environment.
#
# Usage (from the repo root):
#   docker compose --profile plane up -d
#   scripts/plane-dev-bootstrap.sh
#   # or, provision + import in the same run:
#   COMPANY=examples/nimbus.company.yaml scripts/plane-dev-bootstrap.sh
#
# On a REMOTE host, set PLANE_PUBLIC_URL (compose + this script read it) to
# the address browsers use — e.g. http://<server-ip>:8091 — so Plane's
# redirects, shared links, and the company config all carry that address
# instead of localhost:
#   PLANE_PUBLIC_URL=http://203.0.113.7:8091 docker compose --profile plane up -d
#   PLANE_PUBLIC_URL=http://203.0.113.7:8091 scripts/plane-dev-bootstrap.sh
#
# (No `up --wait`: plane-migrator is a one-shot job that exits after
# migrating, which `--wait` treats as a failure — step 1 below polls the
# API instead.)
#
# Re-runnable: every step is idempotent.
set -euo pipefail

COMPOSE=(docker compose --profile plane)
# The address this script (and the company config's ${PLANE_URL}) uses.
# Chain: explicit PLANE_URL > PLANE_PUBLIC_URL (the compose knob for
# remote hosts) > localhost on the published proxy port.
PLANE_URL="${PLANE_URL:-${PLANE_PUBLIC_URL:-http://localhost:${PLANE_LISTEN_PORT:-8091}}}"
FOUNDER_EMAIL="${PLANE_FOUNDER_EMAIL:-founder@nimbus.local}"
FOUNDER_PASSWORD="${PLANE_FOUNDER_PASSWORD:-crewlet-dev-password}"
# Pinned by the Nimbus example: workspace slug `nimbus` (the company
# config's integrations.plane.workspace) — not overridable, because every
# downstream step (provision, import, the engine) expects this slug.
WORKSPACE_NAME="Nimbus"
WORKSPACE_SLUG="nimbus"
# Where minted values land (the provisioner's default too).
ENV_FILE="${PLANE_ENV_FILE:-.env.plane}"
# A company config whose integrations.plane.url is "${PLANE_URL}" (the
# shipped examples/nimbus.company.yaml is). Set COMPANY=<path> to auto
# provision + import; leave unset to just print the commands.
COMPANY="${COMPANY:-}"
# Where the compose Plane should reach THE ENGINE — its base address, not
# its webhook path. The engine serves seven webhook routes and owns every
# one of those paths, so the provisioner is handed a host and derives
# `/webhooks/plane` itself: an operator typing the path can get it wrong,
# and a hook pointing at a path nothing serves leaves the workspace
# reporting a healthy integration that delivers nowhere.
#
# The engine's EMBEDDED API serves that route: examples/nimbus.config.yaml
# sets api.port: 80, so a plain `crewlet run` binds it (on Linux that
# needs privileged-port access — see the api block's comment in that
# file). Do not also start a separate API process on this host — that's
# the split-deployment form and the two would fight over the port.
# host.docker.internal resolves via the extra_hosts mapping on the
# plane-api and plane-worker services.
#
# The knob used to be WEBHOOK_URL and took a full endpoint. Silently
# ignoring one left in an operator's shell would send hooks to the default
# host, which on a remote host is exactly the failure this address exists
# to avoid — so it is refused rather than dropped.
if [ -n "${WEBHOOK_URL:-}" ]; then
  echo "WEBHOOK_URL is no longer read: the engine owns the webhook path and" >&2
  echo "derives it. Set ENGINE_URL to the engine's base address instead." >&2
  exit 2
fi
ENGINE_URL="${ENGINE_URL:-http://host.docker.internal:80}"
# Display only, derived exactly as the provisioner derives it, so what this
# script PRINTS cannot drift from what it registers.
WEBHOOK_URL="${ENGINE_URL%/}/webhooks/plane"

if [ -z "$("${COMPOSE[@]}" ps -q plane-api 2>/dev/null)" ]; then
  echo "Plane containers not found. Run: docker compose --profile plane up -d" >&2
  exit 1
fi

# ── Step 1: wait for the API ─────────────────────────────────────────────
# /api/instances/ is public (AllowAny) and only answers once the api
# serves — and its entrypoint's wait_for_migrations gates that on the
# one-shot migrator having finished, so a 200 means migrations are done.
# The poll goes through plane-proxy (the same path the UI and webhooks
# use), capped at 10 minutes: a cold first boot (migrate + collectstatic)
# fits well inside that, so hitting the cap means something is actually
# broken and dots-forever would just hide it.
echo "==> Waiting for the Plane API (first boot runs migrations; a few minutes)…"
attempts=0
max_attempts=120                     # 120 × 5 s = 10 min
until curl -fsS "${PLANE_URL}/api/instances/" >/dev/null 2>&1; do
  attempts=$((attempts + 1))
  if [ "${attempts}" -ge "${max_attempts}" ]; then
    echo "" >&2
    echo "ERROR: Plane API not answering at ${PLANE_URL}/api/instances/ after 10 minutes." >&2
    echo "Diagnose with:" >&2
    echo "    docker compose --profile plane ps" >&2
    echo "    docker compose --profile plane logs plane-migrator plane-api" >&2
    echo "(This poll goes through plane-proxy, which is only created once" >&2
    echo " plane-api is healthy. To probe the api directly, bypassing the proxy:" >&2
    echo "    docker compose --profile plane exec -T plane-api \\" >&2
    echo "      python -c 'import urllib.request; urllib.request.urlopen(\"http://localhost:8000/api/instances/\")')" >&2
    exit 1
  fi
  printf '.'
  sleep 5
done
echo " ready."

# ── Step 2: founder + instance admin + founder API token ─────────────────
# One atomic, idempotent django-shell pass, mirroring the god-mode
# InstanceAdminSignUpEndpoint creation block field-for-field (including
# its transaction.atomic) — because no management command does all of
# this: create_instance_admin cannot create the user, hard-errors when
# the admin already exists, and never sets is_setup_done. Idempotent AND
# self-repairing: the whole pass is one transaction, every object is
# get-or-create, and the Profile + activation stamps sit outside the
# user-creation guard — so a founder half-created by an interrupted
# earlier run is repaired, and a re-run re-prints the same UUID + token.
echo "==> Ensuring founder ${FOUNDER_EMAIL} (instance admin) + API token…"
OUT=$("${COMPOSE[@]}" exec -T \
  -e PLANE_FOUNDER_EMAIL="${FOUNDER_EMAIL}" \
  -e PLANE_FOUNDER_PASSWORD="${FOUNDER_PASSWORD}" \
  plane-api python manage.py shell <<'PY'
import os
import uuid

from django.contrib.auth.hashers import make_password
from django.db import transaction
from django.utils import timezone

from plane.db.models import APIToken, Profile, User
from plane.license.models import Instance, InstanceAdmin

email = os.environ["PLANE_FOUNDER_EMAIL"].strip().lower()
password = os.environ["PLANE_FOUNDER_PASSWORD"]

with transaction.atomic():
    user = User.objects.filter(email=email).first()
    if user is None:
        user = User.objects.create(
            first_name="Founder",
            last_name="",
            email=email,
            username=uuid.uuid4().hex,
            password=make_password(password),
            is_password_autoset=False,
        )
    # Outside the creation guard: Profile is a REQUIRED OneToOne (the UI
    # 500s without it), so a user left Profile-less or inactive by an
    # interrupted run is repaired here instead of being skipped forever.
    Profile.objects.get_or_create(user=user)
    user.is_active = True
    user.last_active = timezone.now()
    user.last_login_time = timezone.now()
    user.token_updated_at = timezone.now()
    user.save()

    # The api entrypoint runs register_instance before serving, so the row
    # exists by the time step 1's poll succeeded.
    instance = Instance.objects.first()
    assert instance is not None, "no Instance row — did register_instance run?"
    InstanceAdmin.objects.get_or_create(
        user=user, instance=instance, defaults={"role": 20}
    )
    if not instance.is_setup_done:
        instance.is_setup_done = True
        instance.save()

    # The model default generates the value: "plane_api_" + uuid4().hex.
    token, _ = APIToken.objects.get_or_create(
        user=user, label="crewlet-dev-bootstrap"
    )
print(f"CREWLET_BOOTSTRAP_FOUNDER_USER_ID={user.id}")
print(f"CREWLET_BOOTSTRAP_FOUNDER_TOKEN={token.token}")
PY
)
FOUNDER_USER_ID=$(printf '%s\n' "${OUT}" | sed -n 's/^CREWLET_BOOTSTRAP_FOUNDER_USER_ID=//p' | tail -1)
FOUNDER_TOKEN=$(printf '%s\n' "${OUT}" | sed -n 's/^CREWLET_BOOTSTRAP_FOUNDER_TOKEN=//p' | tail -1)
if [ -z "${FOUNDER_USER_ID}" ] || [ -z "${FOUNDER_TOKEN}" ]; then
  echo "Founder bootstrap failed — django shell output:" >&2
  printf '%s\n' "${OUT}" >&2
  exit 1
fi

# Plane caches the /api/instances/ payload for 2 h — and step 1's poll has
# just primed that cache with is_setup_done=false. The writes above went
# through the django shell, not the request path, so nothing invalidated
# it; without a clear_cache the web UI and god-mode keep serving the stale
# "not set up" payload (dropping you into a setup flow that then
# hard-errors ADMIN_ALREADY_EXIST) for up to two hours.
"${COMPOSE[@]}" exec -T plane-api python manage.py clear_cache

# The bootstrap-owned var: the shipped example's founder seat carries
# contact.plane_user_id: "${PLANE_FOUNDER_USER_ID}" — this write closes
# that loop (mention attribution + lookup_colleague for the founder).
upsert_env_var() {
  local file="$1" key="$2" value="$3" tmp
  touch "${file}"
  if grep -Eq "^(export[[:space:]]+)?${key}=" "${file}"; then
    # Rewrite via mktemp in the same directory, removed on failure — the
    # temp copy holds minted tokens and must never be left lying around.
    tmp=$(mktemp "${file}.XXXXXX")
    if sed -E "s|^(export[[:space:]]+)?${key}=.*|\\1${key}=${value}|" \
        "${file}" > "${tmp}"; then
      mv "${tmp}" "${file}"
    else
      rm -f "${tmp}"
      return 1
    fi
  else
    printf '%s=%s\n' "${key}" "${value}" >> "${file}"
  fi
}
upsert_env_var "${ENV_FILE}" PLANE_FOUNDER_USER_ID "${FOUNDER_USER_ID}"
echo "    founder ${FOUNDER_USER_ID} (PLANE_FOUNDER_USER_ID written to ${ENV_FILE})"
# The company config references this instance as ${PLANE_URL}
# (integrations.plane.url, skill_variables, the MCP server's
# PLANE_BASE_URL) — persisting it here means sourcing the env file is all
# any consumer needs.
upsert_env_var "${ENV_FILE}" PLANE_URL "${PLANE_URL}"

# ── Step 3: workspace ────────────────────────────────────────────────────
# Public API (#9400): the token user becomes owner + role-20 member.
# Both curls keep the response body + status: the API names the real
# cause exactly (400 "slug" = taken, 403 = DISABLE_WORKSPACE_CREATION,
# 401 = bad token), so a failure prints that instead of a guess-list.
echo "==> Ensuring workspace '${WORKSPACE_SLUG}'…"
CREATE_RESP=$(curl -sS -w '\n%{http_code}' -X POST "${PLANE_URL}/api/v1/workspaces/" \
    -H "X-API-Key: ${FOUNDER_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\": \"${WORKSPACE_NAME}\", \"slug\": \"${WORKSPACE_SLUG}\"}")
CREATE_STATUS=$(printf '%s' "${CREATE_RESP}" | tail -n1)
CREATE_BODY=$(printf '%s' "${CREATE_RESP}" | sed '$d')
if [ "${CREATE_STATUS}" -ge 200 ] && [ "${CREATE_STATUS}" -lt 300 ]; then
  echo "    created."
else
  # Idempotent re-run: the create rejects a taken slug. The list endpoint
  # returns only the token user's memberships, so finding the slug there
  # proves the workspace exists AND the founder is a member.
  LIST_RESP=$(curl -sS -w '\n%{http_code}' "${PLANE_URL}/api/v1/workspaces/" \
      -H "X-API-Key: ${FOUNDER_TOKEN}")
  LIST_STATUS=$(printf '%s' "${LIST_RESP}" | tail -n1)
  LIST_BODY=$(printf '%s' "${LIST_RESP}" | sed '$d')
  if [ "${LIST_STATUS}" = "200" ] \
      && printf '%s' "${LIST_BODY}" \
      | grep -qE "\"slug\":[[:space:]]*\"${WORKSPACE_SLUG}\""; then
    echo "    exists."
  else
    echo "ERROR: workspace '${WORKSPACE_SLUG}' could not be created and the founder" >&2
    echo "       is not a member of it. Create response (HTTP ${CREATE_STATUS}):" >&2
    printf '%s\n' "${CREATE_BODY}" >&2
    if [ "${LIST_STATUS}" != "200" ]; then
      echo "       Membership list failed too (HTTP ${LIST_STATUS}):" >&2
      printf '%s\n' "${LIST_BODY}" >&2
    fi
    echo "       Fix it in the UI: ${PLANE_URL}" >&2
    exit 1
  fi
fi
# Same 2-hour cache as step 2: the cached /api/instances/ payload carries
# workspaces_exist, and the public workspace create doesn't invalidate it.
"${COMPOSE[@]}" exec -T plane-api python manage.py clear_cache

# ── Step 4: archive the seeded demo project ──────────────────────────────
# Workspace creation queues an ASYNC seed task that creates a demo project
# NAMED AFTER THE WORKSPACE (identifier: first five letters — "NIMBU")
# whose members are whoever exists at seed time — the founder only, since
# the agent seats arrive in step 5. Left alone it is a decoy: an agent
# picking a project by name lands in it and 403s on every write. Detect
# it by its seeded description (the name is workspace-branded, the
# description survives verbatim) and archive it — reversible in the UI
# (project settings → restore) if you ever want the demo content back.
echo "==> Archiving the seeded demo project (if present)…"
if [ "${CREATE_STATUS}" -ge 200 ] && [ "${CREATE_STATUS}" -lt 300 ]; then
  demo_attempts=12    # fresh workspace: the async seed needs a moment (~60 s cap)
else
  demo_attempts=1     # re-run: whatever the seed made is already listed
fi
DEMO_ID=""
while :; do
  DEMO_ID=$(curl -sS "${PLANE_URL}/api/v1/workspaces/${WORKSPACE_SLUG}/projects/" \
      -H "X-API-Key: ${FOUNDER_TOKEN}" \
    | python3 -c '
import json, sys

try:
    payload = json.load(sys.stdin)
except Exception:
    sys.exit(0)
rows = payload.get("results") if isinstance(payload, dict) else payload
for p in rows or []:
    desc = p.get("description") or ""
    if desc.startswith("Welcome to the Plane Demo Project") and not p.get("archived_at"):
        print(p["id"])
        break
')
  demo_attempts=$((demo_attempts - 1))
  if [ -n "${DEMO_ID}" ] || [ "${demo_attempts}" -le 0 ]; then
    break
  fi
  sleep 5
done
if [ -n "${DEMO_ID}" ]; then
  ARCH_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
      "${PLANE_URL}/api/v1/workspaces/${WORKSPACE_SLUG}/projects/${DEMO_ID}/archive/" \
      -H "X-API-Key: ${FOUNDER_TOKEN}")
  if [ "${ARCH_STATUS}" -ge 200 ] && [ "${ARCH_STATUS}" -lt 300 ]; then
    echo "    archived (${DEMO_ID})."
  else
    echo "    WARNING: archive returned HTTP ${ARCH_STATUS} — archive it in the" >&2
    echo "    UI instead (the project named '${WORKSPACE_NAME}', not your org's)." >&2
  fi
else
  echo "    none found (already archived, or seeding disabled)."
fi

# ── Steps 5-6: provision + import (with COMPANY=) ────────────────────────
if [ -n "${COMPANY}" ]; then
  # The founder token IS a workspace-admin personal API token (owner via
  # the create above). -create-projects creates LEAD/ENG/PROD/TS; the
  # webhook secret Plane generates is captured into ${PLANE_WEBHOOK_SECRET}
  # in the env file.
  echo "==> Provisioning agents from ${COMPANY}…"
  PLANE_ADMIN_TOKEN="${FOUNDER_TOKEN}" \
  PLANE_URL="${PLANE_URL}" \
    crewlet plane provision "${COMPANY}" \
      -create-projects \
      -public-url "${ENGINE_URL}" \
      -env-file "${ENV_FILE}"

  # One walk publishes BOTH the tool skills (examples/tool-skills/,
  # `trigger:` ⇒ the TS project) and the knowledge docs + per-unit
  # Onboarding pages (examples/nimbus-docs/, parent dir ⇒ project). The
  # import client authenticates as the engine account with the just-minted
  # ${PLANE_ENGINE_TOKEN} — source the env file first.
  echo "==> Importing docs + tool skills from examples/…"
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
  crewlet plane import "${COMPANY}" examples/
fi

# ── Step 7: next steps ───────────────────────────────────────────────────
cat <<EONOTE

==> Done.
   - Plane UI:         ${PLANE_URL}  (${FOUNDER_EMAIL} / \$PLANE_FOUNDER_PASSWORD;
                       email+password login is on by default)
   - Founder token:    ${FOUNDER_TOKEN}
   - Founder UUID:     ${FOUNDER_USER_ID}  (PLANE_FOUNDER_USER_ID in ${ENV_FILE})
   - Workspace:        ${WORKSPACE_SLUG}
   - Webhooks post to: ${WEBHOOK_URL}
EONOTE

if [ -z "${COMPANY}" ]; then
  cat <<EONEXT
   - Provision + import the shipped example (its integrations.plane.url is
     \${PLANE_URL}, already written to ${ENV_FILE}):
       PLANE_ADMIN_TOKEN=${FOUNDER_TOKEN} PLANE_URL=${PLANE_URL} \\
         crewlet plane provision examples/nimbus.company.yaml \\
           -create-projects -public-url ${ENGINE_URL} -env-file ${ENV_FILE}
       set -a; source ${ENV_FILE}; set +a
       crewlet plane import examples/nimbus.company.yaml examples/
   - Then run the engine — its EMBEDDED API (api.port: 80 in the Tier A
     file) receives the Plane webhooks and serves the dashboard:
       source ${ENV_FILE}
       crewlet run examples/nimbus.config.yaml --import-company examples/nimbus.company.yaml
EONEXT
else
  cat <<EONEXT
   - Tokens written:   ${ENV_FILE}
   - Next, run the engine — its EMBEDDED API (api.port: 80 in the Tier A
     file) receives the Plane webhooks and serves the dashboard:
       source ${ENV_FILE}
       crewlet run examples/nimbus.config.yaml --import-company ${COMPANY}
EONEXT
fi

cat <<'EOTAIL'
   - Fill any human contact.* fields (e.g. contact.plane_user_id) from the
     provision report's workspace member table — the founder UUID is
     already captured.
   - If deliveries fail 5x while the engine is down, Plane auto-disables
     the webhook — re-run `crewlet plane provision` to repair it.
EOTAIL

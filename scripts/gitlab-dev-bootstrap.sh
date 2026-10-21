#!/usr/bin/env bash
#
# gitlab-dev-bootstrap.sh — stand up the docker-compose GitLab for local
# integration testing of the Crewlet GitLab integration.
#
# It waits for GitLab to be healthy, mints a deterministic root PAT, opens
# the SSRF allowlist so webhooks can reach a host-side engine, disables PAT
# expiry (dev), and seeds the `nimbus-hq` group + a project. It then prints
# the `crewlet gitlab provision` command to run against a company config
# whose integrations.gitlab.url points at THIS local instance — through the
# RUNNING engine, which is where the provisioner reads the company's values
# and records what it mints.
#
# examples/nimbus.company.yaml targets gitlab.com as shipped. To provision
# it against this local GitLab, copy it and set integrations.gitlab.url to
# http://gitlab.local:8929 (and swap gitlab.com -> gitlab.local:8929 in the
# git-auth setup step), then pass that copy as COMPANY below.
#
# Usage:
#   docker compose --profile gitlab up -d
#   scripts/gitlab-dev-bootstrap.sh
#   # then start the engine and provision your local-pointed config through
#   # it (see the printed commands)
#
# Re-runnable: every step is idempotent.
set -euo pipefail

GITLAB_CONTAINER="${GITLAB_CONTAINER:-$(docker compose ps -q gitlab 2>/dev/null || true)}"
GITLAB_URL="${GITLAB_URL:-http://localhost:8929}"
ROOT_TOKEN="${GITLAB_ROOT_TOKEN:-glpat-crewlet-dev-bootstrap}"
GROUP="${GITLAB_GROUP:-nimbus-hq}"
PROJECT="${GITLAB_PROJECT:-nimbuscore}"
# A company config whose integrations.gitlab.url is THIS local instance.
# Set COMPANY=path/to/your-local-config.yaml to auto-provision it; leave
# unset to just seed + print the commands.
#
# AUTO-PROVISIONING GOES THROUGH THE RUNNING ENGINE: start `crewlet run` on
# the Tier A file CREWLET_CONFIG names first, and export CREWLET_API_TOKEN as
# one of that file's api.auth.tokens carrying admin. The provisioner reads the
# company's values through the node's break-glass reveal — which refuses a
# machine token from `crewlet iam token` — and records what it mints in the
# fleet's secret store. Against a STOPPED node it can read only that node's
# own rows, which cannot say whether the signing secret is set elsewhere, so
# it refuses to generate one rather than guess.
COMPANY="${COMPANY:-}"
CREWLET_CONFIG="${CREWLET_CONFIG:-crewlet.yaml}"
# Where the compose GitLab should reach THE ENGINE — its base address, not
# its webhook path. The engine serves seven webhook routes and owns every
# one of those paths, so the provisioner is handed a host and derives
# `/webhooks/gitlab` itself: an operator typing the path can get it wrong,
# and a hook pointing at a path nothing serves leaves the group reporting a
# healthy integration that delivers nowhere.
#
# The engine's EMBEDDED API serves that route when the Tier A file sets
# api.port: 80 (the single-host walkthrough shape — see
# docs/integrations/gitlab.md § Local testing). On Docker Desktop reach the
# host via host.docker.internal; on Linux the gateway IP works.
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
WEBHOOK_URL="${ENGINE_URL%/}/webhooks/gitlab"

# REFUSED BEFORE ANYTHING IS SEEDED, because the provisioning step at the end
# would be: without a credential for the running engine it cannot read the
# company's values, and finding that out after a five-minute wait for GitLab
# is a wait for nothing.
if [ -n "${COMPANY}" ] && [ -z "${CREWLET_API_TOKEN:-}" ]; then
  echo "COMPANY is set, so this provisions it through the running engine," >&2
  echo "which needs CREWLET_API_TOKEN: one of ${CREWLET_CONFIG}'s api.auth.tokens" >&2
  echo "carrying admin. Start \`crewlet run -config ${CREWLET_CONFIG}\` and export it." >&2
  exit 2
fi

if [ -z "${GITLAB_CONTAINER}" ]; then
  echo "GitLab container not found. Run: docker compose --profile gitlab up -d" >&2
  exit 1
fi

echo "==> Waiting for GitLab readiness (first boot takes 3-6 min)…"
# /-/readiness is IP-restricted to the monitoring allowlist (default: localhost
# only). A host-side curl to the published port arrives with the Docker gateway
# IP, not 127.0.0.1, so GitLab returns 404 forever. Poll from INSIDE the
# container, where localhost is allowlisted — the same check the compose
# healthcheck runs. (The REST API used below is not IP-restricted, so those
# host-side calls are fine.)
until docker exec "${GITLAB_CONTAINER}" \
  curl -fsS http://localhost:8929/-/readiness >/dev/null 2>&1; do
  printf '.'
  sleep 10
done
echo " ready."

echo "==> Minting a deterministic root PAT (idempotent)…"
docker exec -i "${GITLAB_CONTAINER}" gitlab-rails runner - <<RUBY
u = User.find_by_username('root')
t = u.personal_access_tokens.active.find_by(name: 'crewlet-bootstrap')
unless t
  t = u.personal_access_tokens.create!(
    scopes: ['api', 'sudo'],
    name: 'crewlet-bootstrap',
    expires_at: 365.days.from_now,
  )
  t.set_token('${ROOT_TOKEN}')
  t.save!
end
RUBY

api() { curl -fsS -H "PRIVATE-TOKEN: ${ROOT_TOKEN}" "$@"; }

echo "==> Opening the webhook SSRF allowlist + disabling PAT expiry (dev)…"
api -X PUT "${GITLAB_URL}/api/v4/application/settings" \
  --data "allow_local_requests_from_web_hooks_and_services=true" \
  --data "allow_local_requests_from_system_hooks=true" \
  --data "require_personal_access_token_expiry=false" >/dev/null

echo "==> Ensuring group '${GROUP}' and project '${GROUP}/${PROJECT}'…"
if ! api "${GITLAB_URL}/api/v4/groups/${GROUP}" >/dev/null 2>&1; then
  api -X POST "${GITLAB_URL}/api/v4/groups" \
    --data "name=${GROUP}" --data "path=${GROUP}" --data "visibility=private" >/dev/null
fi
GROUP_ID=$(api "${GITLAB_URL}/api/v4/groups/${GROUP}" | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)
if ! api "${GITLAB_URL}/api/v4/projects/${GROUP}%2F${PROJECT}" >/dev/null 2>&1; then
  api -X POST "${GITLAB_URL}/api/v4/projects" \
    --data "name=${PROJECT}" --data "namespace_id=${GROUP_ID}" \
    --data "initialize_with_readme=true" >/dev/null
fi

if [ -n "${COMPANY}" ]; then
  echo "==> Provisioning agents from ${COMPANY} through the engine ${CREWLET_CONFIG} names…"
  GITLAB_ADMIN_TOKEN="${ROOT_TOKEN}" \
    crewlet gitlab provision "${COMPANY}" \
      -config "${CREWLET_CONFIG}" \
      -public-url "${ENGINE_URL}" \
      -secret-store
fi

cat <<EONOTE

==> Done.
   - GitLab UI:      ${GITLAB_URL}  (root / \$GITLAB_ROOT_PASSWORD)
   - Root PAT:       ${ROOT_TOKEN}
   - Group/project:  ${GROUP}/${PROJECT}
   - Webhooks post to: ${WEBHOOK_URL}
EONOTE

if [ -z "${COMPANY}" ]; then
  cat <<EONEXT
   - Start the engine on a local-pointed config (integrations.gitlab.url =
     ${GITLAB_URL}), then provision it through the engine, as one of
     ${CREWLET_CONFIG}'s Tier A tokens:
       crewlet run -config ${CREWLET_CONFIG} -company <your-config>.yaml
       export CREWLET_API_TOKEN=<a token from ${CREWLET_CONFIG}'s api.auth.tokens>
       GITLAB_ADMIN_TOKEN=${ROOT_TOKEN} \\
         crewlet gitlab provision <your-config>.yaml \\
           -config ${CREWLET_CONFIG} -public-url ${ENGINE_URL} -secret-store
   - Then re-activate the company so the engine reads what was minted:
       curl -fsS -X POST -H "Authorization: Bearer \$CREWLET_API_TOKEN" \\
         <api.external_url from ${CREWLET_CONFIG}>/config/reload
EONEXT
else
  cat <<EONEXT
   - Tokens and the signing secret: the fleet's secret store
   - Next: re-activate the company so the engine reads them:
       curl -fsS -X POST -H "Authorization: Bearer \$CREWLET_API_TOKEN" \\
         <api.external_url from ${CREWLET_CONFIG}>/config/reload
EONEXT
fi

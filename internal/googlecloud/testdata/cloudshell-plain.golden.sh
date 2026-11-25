(
set -uo pipefail
export CLOUDSDK_CORE_DISABLE_PROMPTS=1
IDENTITY=acme-crewlet-id
PROV=crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
JWKS="$(mktemp)"
trap 'rm -f "$JWKS"' EXIT
stop() { echo "Crewlet setup stopped: $*" >&2; exit 1; }
fail() { stop "could not $*"; }
# A new service account can be unknown to IAM for a minute or two, so a
# grant naming it is retried for up to three minutes.
grant() {
  local out
  for _ in $(seq 18); do
    out="$("$@" 2>&1)" && return 0
    case "$out" in *"does not exist"*) sleep 10 ;; *) break ;; esac
  done
  echo "$out" >&2
  return 1
}

# Before anything changes: an active account, and a project used only for Crewlet.
EMPTY="Run this block in a new, empty project used only for Crewlet: the provisioner becomes Service Account Admin of the whole project, so it could change or act as every service account in it."
[ -n "$(gcloud config get-value account 2>/dev/null)" ] ||
  stop "Cloud Shell has no active account. Run gcloud auth login, then run this block again."
case "$(gcloud projects describe "$IDENTITY" --format='value(lifecycleState)' 2>/dev/null)" in
  ACTIVE) ;;
  "")
    gcloud projects create "$IDENTITY" --name="Crewlet" --format=none ||
      stop "$IDENTITY does not exist and could not be created (Google says why above). Create it on the new project page, or type another ID in Crewlet and copy the new block." ;;
  *)
    stop "$IDENTITY is being deleted. Restore it with gcloud projects undelete $IDENTITY, or type another ID in Crewlet and copy the new block." ;;
esac
APIS="$(gcloud services list --enabled --project="$IDENTITY" --format='value(config.name)')" ||
  fail "read the APIs enabled on $IDENTITY"
has() { case $'\n'"$APIS"$'\n' in *$'\n'"$1"$'\n'*) return 0 ;; esac; return 1; }
APPS=""
has firebase.googleapis.com && APPS="${APPS:+$APPS, }Firebase"
has appengine.googleapis.com && APPS="${APPS:+$APPS, }App Engine"
has compute.googleapis.com && APPS="${APPS:+$APPS, }Compute Engine"
has run.googleapis.com && APPS="${APPS:+$APPS, }Cloud Run"
has cloudfunctions.googleapis.com && APPS="${APPS:+$APPS, }Cloud Functions"
has container.googleapis.com && APPS="${APPS:+$APPS, }GKE"
[ -z "$APPS" ] || stop "$IDENTITY runs workloads ($APPS). $EMPTY"
if has iam.googleapis.com; then
  ACCOUNTS="$(gcloud iam service-accounts list --project="$IDENTITY" --format='value(email)')" ||
    fail "list the service accounts of $IDENTITY"
  N=0 OTHER=""
  while IFS= read -r SA; do
    case "$SA" in ""|crewlet-*) continue ;; esac
    N=$((N + 1)) OTHER="${OTHER:-$SA}"
  done <<<"$ACCOUNTS"
  case "$N" in
    0) ;;
    1) stop "$IDENTITY holds a service account Crewlet did not make ($OTHER). $EMPTY" ;;
    *) stop "$IDENTITY holds $N service accounts Crewlet did not make, such as $OTHER. $EMPTY" ;;
  esac
fi

gcloud services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com cloudresourcemanager.googleapis.com serviceusage.googleapis.com \
  --project="$IDENTITY" --format=none || fail "enable the identity project's APIs"

if ! gcloud iam service-accounts describe "$PROV" --project="$IDENTITY" --format='value(email)' >/dev/null 2>&1; then
  gcloud iam service-accounts create crewlet-provisioner --project="$IDENTITY" \
    --display-name="Crewlet provisioner" \
    --description="Creates and manages the service accounts of Crewlet's agents" --format=none ||
    fail "create the provisioner"
fi
# The provisioner may turn on an API in this project and nothing more, so Crewlet turns on
# each Google product's APIs itself when the product is turned on.
if gcloud iam roles describe crewletApiEnabler --project="$IDENTITY" >/dev/null 2>&1; then
  gcloud iam roles update crewletApiEnabler --project="$IDENTITY" --permissions=serviceusage.services.enable --format=none
else
  gcloud iam roles create crewletApiEnabler --project="$IDENTITY" --title="Crewlet API enabler" \
    --description="Turn on the APIs of the Google products Crewlet is given. Do not add permissions." \
    --stage=GA --permissions=serviceusage.services.enable --format=none
fi || fail "make the role that lets Crewlet turn on APIs"
for ROLE in roles/iam.serviceAccountAdmin roles/serviceusage.serviceUsageViewer \
  "projects/$IDENTITY/roles/crewletApiEnabler"; do
  grant gcloud projects add-iam-policy-binding "$IDENTITY" --member="serviceAccount:$PROV" \
    --role="$ROLE" --condition=None --format=none || fail "grant the provisioner $ROLE"
done

cat > "$JWKS" <<'CREWLET_JWKS'
{"keys":[{"crv":"P-256","kty":"EC","x":"v-r1f6K0PliGPYvtk5OCKVfymzw9Zfn5Fbn-pS7I2Ho","y":"EFpWX5qWMMmetZJO06qUovLPWw3Tdv9tE86vw9jiKQ4","alg":"ES256","use":"sig","kid":"ifmQq_ath2HgMoexyn9FLn7qRARY5oAry4D3Kjcxr9w"}]}
CREWLET_JWKS

if ! gcloud iam workload-identity-pools describe crewlet --project="$IDENTITY" --location=global \
  --format='value(name)' >/dev/null 2>&1; then
  gcloud iam workload-identity-pools create crewlet --project="$IDENTITY" --location=global \
    --display-name="Crewlet engines" --format=none || fail "create the workload identity pool"
fi
if gcloud iam workload-identity-pools providers describe crewlet-engine --project="$IDENTITY" --location=global --workload-identity-pool=crewlet \
  --format='value(name)' >/dev/null 2>&1; then
  gcloud iam workload-identity-pools providers update-oidc crewlet-engine --project="$IDENTITY" --location=global --workload-identity-pool=crewlet \
    --issuer-uri="https://crewlet.invalid/acme" \
    --attribute-mapping="google.subject=assertion.sub" \
    --attribute-condition="assertion.sub == 'crewlet:acme'" \
    --jwk-json-path="$JWKS" --format=none ||
    fail "update the provider with Crewlet's key"
else
  gcloud iam workload-identity-pools providers create-oidc crewlet-engine --project="$IDENTITY" --location=global --workload-identity-pool=crewlet \
    --display-name="Crewlet engine" \
    --issuer-uri="https://crewlet.invalid/acme" \
    --attribute-mapping="google.subject=assertion.sub" \
    --attribute-condition="assertion.sub == 'crewlet:acme'" \
    --jwk-json-path="$JWKS" --format=none ||
    fail "create the provider"
fi

NUMBER="$(gcloud projects describe "$IDENTITY" --format='value(projectNumber)')" &&
  [ -n "$NUMBER" ] || fail "read the identity project's number"
grant gcloud iam service-accounts add-iam-policy-binding "$PROV" --project="$IDENTITY" \
  --role=roles/iam.workloadIdentityUser --format=none \
  --member="principal://iam.googleapis.com/projects/${NUMBER}/locations/global/workloadIdentityPools/crewlet/subject/crewlet:acme" ||
  fail "let Crewlet act as the provisioner"

gcloud iam workload-identity-pools providers describe crewlet-engine --project="$IDENTITY" --location=global --workload-identity-pool=crewlet \
  --format='value(name)'
)

package googlecloud

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/setup"
)

// The workload identity pool and provider the Cloud Shell block makes in the
// identity project, which the provider name it prints ends with.
const (
	PoolID     = "crewlet"
	ProviderID = "crewlet-engine"
)

// ProviderName is the full resource name of the block's provider in the
// identity project whose number is number.
func ProviderName(number string) string {
	return "projects/" + number + "/locations/global/workloadIdentityPools/" + PoolID + "/providers/" + ProviderID
}

// jwksEnd ends the block's heredoc. A JWKS is one line of JSON, so it can
// never hold a line reading this.
const jwksEnd = "CREWLET_JWKS"

// CloudShellBlock is the one block a person runs, once, to connect Google
// Cloud: pasted whole into Cloud Shell, it readies project as company's
// identity project for the key's public half and prints the provider's full
// name as its last line. It holds public names and the public half alone
// ([setup.PublicOf]), so it is drawn from what the form holds before anything
// is saved. It first stops, changing nothing, where it cannot be Crewlet's
// project ([preflightLines]), and creates the project where it does not exist.
// SAFE TO RUN AGAIN: each make is a describe first, and a subshell stops a
// failed block, not Cloud Shell. It NEVER CHANGES as Google products are
// turned on: the provisioner may turn on their APIs itself ([EnablerRoleID]).
// WITH ATLAS it makes the atlas's account ([atlasLines]).
func CloudShellBlock(company, project, key string, atlas bool) (string, error) {
	project = strings.TrimSpace(project)
	if !config.GoogleCloudIdentityProject(project) {
		return "", errors.New("googlecloud: the identity project is not a Google Cloud project ID")
	}
	if org.Slugify(company) == "" {
		return "", errors.New("googlecloud: the company's name has no letter or digit to sign its federation token as")
	}
	jwks, err := setup.ES256JWKS(key)
	if err != nil {
		return "", err
	}
	principal := googleauth.Principal(ProviderName("${NUMBER}"), company)
	provider := `--project="$IDENTITY" --location=global --workload-identity-pool=` + PoolID
	oidc := ` \
    --issuer-uri="` + googleauth.Issuer(company) + `" \
    --attribute-mapping="google.subject=assertion.sub" \
    --attribute-condition="assertion.sub == '` + googleauth.Subject(company) + `'" \
    --jwk-json-path="$JWKS" --format=none`
	lines := []string{
		"(",
		"set -uo pipefail",
		"export CLOUDSDK_CORE_DISABLE_PROMPTS=1",
		"IDENTITY=" + project,
		"PROV=" + config.GoogleCloudProvisioner(project),
		`JWKS="$(mktemp)"`,
		`trap 'rm -f "$JWKS"' EXIT`,
		`stop() { echo "` + StopPrefix + `$*" >&2; exit 1; }`,
		`fail() { stop "could not $*"; }`,
		"# A new service account can be unknown to IAM for a minute or two, so a",
		"# grant naming it is retried for up to three minutes.",
		`grant() {`,
		`  local out`,
		`  for _ in $(seq 18); do`,
		`    out="$("$@" 2>&1)" && return 0`,
		`    case "$out" in *"does not exist"*) sleep 10 ;; *) break ;; esac`,
		`  done`,
		`  echo "$out" >&2`,
		`  return 1`,
		`}`,
		"",
	}
	lines = append(lines, preflightLines()...)
	lines = append(lines, []string{
		`gcloud services enable ` + strings.Join(identityAPIs, " ") + ` \`,
		`  --project="$IDENTITY" --format=none || fail "enable the identity project's APIs"`,
		"",
		`if ! gcloud iam service-accounts describe "$PROV" --project="$IDENTITY" --format='value(email)' >/dev/null 2>&1; then`,
		`  gcloud iam service-accounts create ` + config.GoogleCloudProvisionerID + ` --project="$IDENTITY" \`,
		`    --display-name="Crewlet provisioner" \`,
		`    --description="Creates and manages the service accounts of Crewlet's agents" --format=none ||`,
		`    fail "create the provisioner"`,
		`fi`,
		"# The provisioner may turn on an API in this project and nothing more, so Crewlet turns on",
		"# each Google product's APIs itself when the product is turned on.",
		`if gcloud iam roles describe ` + EnablerRoleID + ` --project="$IDENTITY" >/dev/null 2>&1; then`,
		`  gcloud iam roles update ` + EnablerRoleID + ` --project="$IDENTITY" --permissions=` + EnablerPermission + ` --format=none`,
		`else`,
		`  gcloud iam roles create ` + EnablerRoleID + ` --project="$IDENTITY" --title="Crewlet API enabler" \`,
		`    --description="Turn on the APIs of the Google products Crewlet is given. Do not add permissions." \`,
		`    --stage=GA --permissions=` + EnablerPermission + ` --format=none`,
		`fi || fail "make the role that lets Crewlet turn on APIs"`,
		`for ROLE in roles/iam.serviceAccountAdmin roles/serviceusage.serviceUsageViewer \`,
		`  "projects/$IDENTITY/roles/` + EnablerRoleID + `"; do`,
		`  grant gcloud projects add-iam-policy-binding "$IDENTITY" --member="serviceAccount:$PROV" \`,
		`    --role="$ROLE" --condition=None --format=none || fail "grant the provisioner $ROLE"`,
		`done`,
		"",
		`cat > "$JWKS" <<'` + jwksEnd + `'`,
		jwks,
		jwksEnd,
		"",
		`if ! gcloud iam workload-identity-pools describe ` + PoolID + ` --project="$IDENTITY" --location=global \`,
		`  --format='value(name)' >/dev/null 2>&1; then`,
		`  gcloud iam workload-identity-pools create ` + PoolID + ` --project="$IDENTITY" --location=global \`,
		`    --display-name="Crewlet engines" --format=none || fail "create the workload identity pool"`,
		`fi`,
		`if gcloud iam workload-identity-pools providers describe ` + ProviderID + ` ` + provider + ` \`,
		`  --format='value(name)' >/dev/null 2>&1; then`,
		`  gcloud iam workload-identity-pools providers update-oidc ` + ProviderID + ` ` + provider + oidc + ` ||`,
		`    fail "update the provider with Crewlet's key"`,
		`else`,
		`  gcloud iam workload-identity-pools providers create-oidc ` + ProviderID + ` ` + provider + ` \`,
		`    --display-name="Crewlet engine"` + oidc + ` ||`,
		`    fail "create the provider"`,
		`fi`,
		"",
		`NUMBER="$(gcloud projects describe "$IDENTITY" --format='value(projectNumber)')" &&`,
		`  [ -n "$NUMBER" ] || fail "read the identity project's number"`,
		`grant gcloud iam service-accounts add-iam-policy-binding "$PROV" --project="$IDENTITY" \`,
		`  --role=roles/iam.workloadIdentityUser --format=none \`,
		`  --member="` + principal + `" ||`,
		`  fail "let Crewlet act as the provisioner"`,
		"",
	}...)
	if atlas {
		lines = append(lines, atlasLines(principal)...)
	}
	lines = append(lines,
		`gcloud iam workload-identity-pools providers describe `+ProviderID+` `+provider+` \`,
		`  --format='value(name)'`,
		")",
	)
	return strings.Join(lines, "\n") + "\n", nil
}

// StopPrefix opens the one line the block prints when it stops.
const StopPrefix = "Crewlet setup stopped: "

// workloadAPIs are the APIs that run code as an identity project's service
// accounts, each with its product's name: a project with one enabled is not
// Crewlet's alone.
//
//nolint:gochecknoglobals // an immutable list, not state
var workloadAPIs = [][2]string{
	{"firebase.googleapis.com", "Firebase"}, {"appengine.googleapis.com", "App Engine"},
	{"compute.googleapis.com", "Compute Engine"}, {"run.googleapis.com", "Cloud Run"},
	{"cloudfunctions.googleapis.com", "Cloud Functions"}, {"container.googleapis.com", "GKE"},
}

// preflightLines stop the block, before it changes anything, without an active
// account or in a project that is not Crewlet's alone: one that runs workloads,
// or holds a service account [crewletsOwn] does not claim. A project that does
// not exist yet is created, so the person types an ID and never visits the
// console. Reads only otherwise, through the Service Usage and IAM APIs gcloud
// already reaches; with IAM off no account can be listed, and an app's own
// accounts come with the APIs checked first.
func preflightLines() []string {
	apps := make([]string, 0, len(workloadAPIs))
	for _, api := range workloadAPIs {
		apps = append(apps, `has `+api[0]+` && APPS="${APPS:+$APPS, }`+api[1]+`"`)
	}
	again := "type another ID in Crewlet and copy the new block"
	return append(append([]string{
		"# Before anything changes: an active account, and a project used only for Crewlet.",
		`EMPTY="Run this block in a new, empty project used only for Crewlet: the provisioner becomes ` +
			`Service Account Admin of the whole project, so it could change or act as every service account in it."`,
		`[ -n "$(gcloud config get-value account 2>/dev/null)" ] ||`,
		`  stop "Cloud Shell has no active account. Run gcloud auth login, then run this block again."`,
		`case "$(gcloud projects describe "$IDENTITY" --format='value(lifecycleState)' 2>/dev/null)" in`,
		`  ACTIVE) ;;`,
		`  "")`,
		`    gcloud projects create "$IDENTITY" --name="Crewlet" --format=none ||`,
		`      stop "$IDENTITY does not exist and could not be created (Google says why above). Create it on ` +
			`the new project page, or ` + again + `." ;;`,
		`  *)`,
		`    stop "$IDENTITY is being deleted. Restore it with gcloud projects undelete $IDENTITY, or ` + again + `." ;;`,
		`esac`,
		`APIS="$(gcloud services list --enabled --project="$IDENTITY" --format='value(config.name)')" ||`,
		`  fail "read the APIs enabled on $IDENTITY"`,
		`has() { case $'\n'"$APIS"$'\n' in *$'\n'"$1"$'\n'*) return 0 ;; esac; return 1; }`,
		`APPS=""`,
	}, apps...),
		`[ -z "$APPS" ] || stop "$IDENTITY runs workloads ($APPS). $EMPTY"`,
		`if has iam.googleapis.com; then`,
		`  ACCOUNTS="$(gcloud iam service-accounts list --project="$IDENTITY" --format='value(email)')" ||`,
		`    fail "list the service accounts of $IDENTITY"`,
		`  N=0 OTHER=""`,
		`  while IFS= read -r SA; do`,
		`    case "$SA" in ""|`+accountPrefix+`*) continue ;; esac`,
		`    N=$((N + 1)) OTHER="${OTHER:-$SA}"`,
		`  done <<<"$ACCOUNTS"`,
		`  case "$N" in`,
		`    0) ;;`,
		`    1) stop "$IDENTITY holds a service account Crewlet did not make ($OTHER). $EMPTY" ;;`,
		`    *) stop "$IDENTITY holds $N service accounts Crewlet did not make, such as $OTHER. $EMPTY" ;;`,
		`  esac`,
		`fi`,
		"",
	)
}

// atlasLines make the atlas's service account and grant it on the
// provisioner's projects, for the Crewlet subject principal.
func atlasLines(principal string) []string {
	return []string{
		"# Atlas (integrations.google_cloud.provisioning.atlas_read): a service account that may only search",
		"# what each of Crewlet's projects runs, by name and type.",
		"ATLAS=" + AtlasAccountID + `@"$IDENTITY".iam.gserviceaccount.com`,
		`gcloud services enable cloudasset.googleapis.com --project="$IDENTITY" --format=none ||`,
		`  fail "enable the Cloud Asset API in the identity project"`,
		`if ! gcloud iam service-accounts describe "$ATLAS" --project="$IDENTITY" --format='value(email)' >/dev/null 2>&1; then`,
		`  gcloud iam service-accounts create ` + AtlasAccountID + ` --project="$IDENTITY" \`,
		`    --display-name="Crewlet atlas" \`,
		`    --description="Searches what Crewlet's projects run, by name and type, for the atlas" --format=none ||`,
		`    fail "create Atlas's service account"`,
		`fi`,
		`grant gcloud projects add-iam-policy-binding "$IDENTITY" --member="serviceAccount:$ATLAS" \`,
		`  --role=roles/serviceusage.serviceUsageConsumer --condition=None --format=none ||`,
		`  fail "let Atlas's service account call the Cloud Asset API"`,
		`grant gcloud iam service-accounts add-iam-policy-binding "$ATLAS" --project="$IDENTITY" \`,
		`  --role=` + workloadIdentityUser + ` --format=none --member="` + principal + `" ||`,
		`  fail "let Crewlet act as Atlas's service account"`,
		`for P in $(gcloud projects list --filter=lifecycleState:ACTIVE --format='value(projectId)'); do`,
		`  [ "$P" = "$IDENTITY" ] && continue`,
		`  gcloud projects get-iam-policy "$P" --flatten='bindings[].members' \`,
		`    --filter="bindings.members:serviceAccount:$PROV" --format='value(bindings.role)' 2>/dev/null | grep -q . ||`,
		`    continue`,
		`  if gcloud iam roles describe ` + AtlasRoleID + ` --project="$P" >/dev/null 2>&1; then`,
		`    gcloud iam roles update ` + AtlasRoleID + ` --project="$P" --permissions=` + AtlasPermission + ` --format=none`,
		`  else`,
		`    gcloud iam roles create ` + AtlasRoleID + ` --project="$P" --title="Crewlet Atlas reader" \`,
		`      --description="Search resources by name and type, for Crewlet's atlas. Do not add permissions." \`,
		`      --stage=GA --permissions=` + AtlasPermission + ` --format=none`,
		`  fi &&`,
		`  gcloud projects add-iam-policy-binding "$P" --member="serviceAccount:$ATLAS" \`,
		`    --role="projects/$P/roles/` + AtlasRoleID + `" --condition=None --format=none &&`,
		`  echo "Atlas reads $P" >&2 ||`,
		`  echo "Atlas could not be granted on $P: an Owner of $P can run this block again" >&2`,
		`done`,
		"",
	}
}

// cloudShellView is the block as the federation key's row shows it, nil where
// it cannot be drawn: a project that is no project ID, or a key that is not
// one the engine mints, which the Test reports.
func cloudShellView(company, project, key string, atlas bool) *setup.PublicView {
	block, err := CloudShellBlock(company, project, key, atlas)
	if err != nil {
		return nil
	}
	view, err := liveView(key, block)
	if err != nil {
		return nil
	}
	return view
}

// liveView is block as a view the form redraws from what is typed ([Draw]).
func liveView(key, block string) (*setup.PublicView, error) {
	view, err := setup.PublicOf(key, block, "sh")
	if err != nil {
		return nil, err
	}
	view.DrawnFrom = []string{"identity_project", "atlas_read"}
	view.Live = true
	return view, nil
}

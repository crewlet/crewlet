package googlecloud

import (
	"errors"
	"slices"
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

// CloudShellBlock is the block the connect form shows once the federation key
// is held: pasted whole into Cloud Shell, it readies the identity project for
// company, provisioner and the key's public half, and prints the provider's
// full name as its last line. Its issuer, subject and principal are the
// engine's own ([googleauth.Issuer]), and only the public half is written
// ([setup.PublicOf]). It first stops, changing nothing, in a project that is
// not Crewlet's alone ([preflightLines]). SAFE TO RUN AGAIN: each make is a
// describe first, and a subshell stops a failed block, not Cloud Shell. WITH
// ATLAS it makes the atlas's account and grants it [AtlasRoleID] on the
// provisioner's projects, naming and skipping one the person may not change
// ([atlasLines]); WITH GROWTH PRODUCTS it enables their APIs and makes the
// company's reader ([growthLines]).
func CloudShellBlock(company, provisioner, key string, atlas bool, growth []googleauth.Product) (string, error) {
	identity, ok := config.GoogleCloudServiceAccount(provisioner)
	if !ok {
		return "", errors.New("googlecloud: the provisioner is not a service account's email")
	}
	if org.Slugify(company) == "" {
		return "", errors.New("googlecloud: the company's name has no letter or digit to sign its federation token as")
	}
	jwks, err := setup.ES256JWKS(key)
	if err != nil {
		return "", err
	}
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
		"IDENTITY=" + identity,
		"PROV=" + strings.TrimSpace(provisioner),
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
		`gcloud services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com \`,
		`  cloudresourcemanager.googleapis.com serviceusage.googleapis.com --project="$IDENTITY" --format=none ||`,
		`  fail "enable the identity project's APIs"`,
		"",
		`if ! gcloud iam service-accounts describe "$PROV" --project="$IDENTITY" --format='value(email)' >/dev/null 2>&1; then`,
		`  gcloud iam service-accounts create "${PROV%%@*}" --project="$IDENTITY" \`,
		`    --display-name="Crewlet provisioner" \`,
		`    --description="Creates and manages the service accounts of Crewlet's agents" --format=none ||`,
		`    fail "create the provisioner"`,
		`fi`,
		`for ROLE in roles/iam.serviceAccountAdmin roles/serviceusage.serviceUsageViewer; do`,
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
		`  --member="` + googleauth.Principal(ProviderName("${NUMBER}"), company) + `" ||`,
		`  fail "let Crewlet act as the provisioner"`,
		"",
	}...)
	if atlas {
		lines = append(lines, atlasLines(googleauth.Principal(ProviderName("${NUMBER}"), company))...)
	}
	if len(growth) > 0 {
		lines = append(lines, growthLines(company, googleauth.Principal(ProviderName("${NUMBER}"), company), growth)...)
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
// or holds a service account [crewletsOwn] does not claim. Reads only, through the
// Service Usage and IAM APIs gcloud already reaches; with IAM off no account
// can be listed, and an app's own accounts come with the APIs checked first.
func preflightLines() []string {
	apps := make([]string, 0, len(workloadAPIs))
	for _, api := range workloadAPIs {
		apps = append(apps, `has `+api[0]+` && APPS="${APPS:+$APPS, }`+api[1]+`"`)
	}
	return append(append([]string{
		"# Before anything changes: an active account, and a project used only for Crewlet.",
		`EMPTY="Run this block in a new, empty project used only for Crewlet: the provisioner becomes ` +
			`Service Account Admin of the whole project, so it could change or act as every service account in it."`,
		`[ -n "$(gcloud config get-value account 2>/dev/null)" ] ||`,
		`  stop "Cloud Shell has no active account. Run gcloud auth login, then run this block again."`,
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
		`    case "$SA" in ""|"$PROV"|`+accountPrefix+`*) continue ;; esac`,
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
		`  --role=roles/iam.workloadIdentityUser --format=none --member="` + principal + `" ||`,
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

// growthLines enable the growth products' APIs in the identity project and
// make the company's reader, for the Crewlet subject principal alone. The
// reader holds no role here: a person adds it to each product at its read
// role, as the connect form of each product says.
func growthLines(company, principal string, growth []googleauth.Product) []string {
	var names, apis []string
	for _, p := range growth {
		names = append(names, p.Name)
		for _, api := range p.APIs {
			if !slices.Contains(apis, api) {
				apis = append(apis, api)
			}
		}
	}
	reader := googleauth.ReaderAccountID(company)
	return []string{
		"# Google growth products (" + strings.Join(names, ", ") + "): their APIs in the identity project, and",
		"# the company's reader, which reads their figures for the whole company.",
		`gcloud services enable ` + strings.Join(apis, " ") + ` --project="$IDENTITY" --format=none ||`,
		`  fail "enable the growth products' APIs in the identity project"`,
		"READER=" + reader + `@"$IDENTITY".iam.gserviceaccount.com`,
		`if ! gcloud iam service-accounts describe "$READER" --project="$IDENTITY" --format='value(email)' >/dev/null 2>&1; then`,
		`  gcloud iam service-accounts create ` + reader + ` --project="$IDENTITY" \`,
		`    --display-name="Crewlet growth reader" \`,
		`    --description="Reads growth figures for the whole company, for Crewlet" --format=none ||`,
		`    fail "create the growth reader"`,
		`fi`,
		`grant gcloud iam service-accounts add-iam-policy-binding "$READER" --project="$IDENTITY" \`,
		`  --role=roles/iam.workloadIdentityUser --format=none --member="` + principal + `" ||`,
		`  fail "let Crewlet act as the growth reader"`,
		`echo "Add $READER to each growth product at its read role" >&2`,
		"",
	}
}

// cloudShellView is the block as the federation key's row shows it, nil where
// it cannot be drawn yet: the provisioner or the key not held, or a key that
// is not one the engine mints, which the Test reports.
func cloudShellView(company, provisioner, key string, atlas bool, growth []googleauth.Product) *setup.PublicView {
	block, err := CloudShellBlock(company, provisioner, key, atlas, growth)
	if err != nil {
		return nil
	}
	view, err := setup.PublicOf(key, block, "sh")
	if err != nil {
		return nil
	}
	return view
}

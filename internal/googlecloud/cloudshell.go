package googlecloud

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
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
// full name as its last line.
//
// THE ISSUER, SUBJECT AND PRINCIPAL ARE THE CLIENT'S OWN ([Issuer], [Subject],
// [Principal]), so the provider trusts exactly the token the engine signs.
// Only the key's public half is written; the private half is refused by the
// caller ([setup.PublicOf]).
//
// SAFE TO RUN AGAIN: each make is a describe first, and a provider that
// exists is updated with the company's issuer, subject pin and current key,
// which is how a changed key or a hand edit is put right. It runs in a
// subshell, so a failed step stops the block without closing Cloud Shell.
//
// WITH ATLAS, it also makes the atlas's service account, lets Crewlet's
// subject act as it, and grants it the one-permission role on every project
// the provisioner holds a role in ([AtlasRoleID]); a project the person
// running it may not change is named and skipped.
func CloudShellBlock(company, provisioner, key string, atlas bool) (string, error) {
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
    --issuer-uri="` + Issuer(company) + `" \
    --attribute-mapping="google.subject=assertion.sub" \
    --attribute-condition="assertion.sub == '` + Subject(company) + `'" \
    --jwk-json-path="$JWKS" --format=none`
	lines := []string{
		"(",
		"set -uo pipefail",
		"export CLOUDSDK_CORE_DISABLE_PROMPTS=1",
		"IDENTITY=" + identity,
		"PROV=" + strings.TrimSpace(provisioner),
		`JWKS="$(mktemp)"`,
		`trap 'rm -f "$JWKS"' EXIT`,
		`fail() { echo "Crewlet setup stopped: could not $*" >&2; exit 1; }`,
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
		`  --member="` + Principal(ProviderName("${NUMBER}"), company) + `" ||`,
		`  fail "let Crewlet act as the provisioner"`,
		"",
	}
	if atlas {
		lines = append(lines, atlasLines(Principal(ProviderName("${NUMBER}"), company))...)
	}
	lines = append(lines,
		`gcloud iam workload-identity-pools providers describe `+ProviderID+` `+provider+` \`,
		`  --format='value(name)'`,
		")",
	)
	return strings.Join(lines, "\n") + "\n", nil
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

// cloudShellView is the block as the federation key's row shows it, nil where
// it cannot be drawn yet: the provisioner or the key not held, or a key that
// is not one the engine mints, which the Test reports.
func cloudShellView(company, provisioner, key string, atlas bool) *setup.PublicView {
	block, err := CloudShellBlock(company, provisioner, key, atlas)
	if err != nil {
		return nil
	}
	view, err := setup.PublicOf(key, block, "sh")
	if err != nil {
		return nil
	}
	return view
}

package harnessprofile

import "strings"

// BundleManifest is the name a credential bundle keeps its manifest under:
// the one entry that is not a login file, naming the harness the bundle was
// exported from — `{"harness": "codex"}`.
//
// A BUNDLE NAMES ITS HARNESS BECAUSE A LOGIN FILE'S NAME DOES NOT: harnesses
// share `auth.json`, and a bundle is keyed by the entry, so one restored by file
// name after an entry changed harness writes a credential the new harness cannot
// read and blocks every later restore as a login already present.
//
// ONE NAME FOR EVERY PROFILE, the engine's as [CredentialStoreDocument] is: no
// profile may keep a login file under it ([Profile.Validate]).
const BundleManifest = "crewlet-bundle.json"

// LoginInstallFile is the name the engine keeps a login's install record under
// in the credential directory, beside the login (internal/providers/llm/harness,
// install.go). The engine's name, as [BundleManifest] is, and refused as a
// credential path for the same reason: a login file under it would be the
// record, and the record a login file.
const LoginInstallFile = ".crewlet-login"

// ZoneVariable is the variable the engine hands every harness, on its host and in
// a box, to name the zone it runs in (internal/providers/llm/harness, env.go):
// the engine's own TZ, else UTC. The engine reads a reset the harness names with
// no zone of its own in that zone, so an entry may not set it (config refuses
// it in providers.llm.<key>.env).
const ZoneVariable = "TZ"

// BundleVarName is the conventional secret name a providers.llm entry's
// exported credential bundle lives under: CREWLET_LLM_CLI_<KEY>_CREDENTIALS.
//
// A CONVENTION rather than a required setting, so `crewlet llm export
// -secret-store` and the engine's boot-time restore agree without the operator
// wiring auth.credential_bundle by hand. HERE, beside the variables a profile
// names, because three packages read it — the writer, the engine's restore and
// the company's own account of which variables it reads — and one function is
// what keeps them from deriving it differently.
func BundleVarName(key string) string {
	var b strings.Builder
	b.WriteString("CREWLET_LLM_CLI_")
	for _, r := range strings.ToUpper(key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_CREDENTIALS")
	return b.String()
}

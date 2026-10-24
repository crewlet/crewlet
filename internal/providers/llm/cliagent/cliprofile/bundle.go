package cliprofile

import "strings"

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

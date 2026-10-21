package cliprofile

import "strings"

// BundleVarName is the conventional secret name the `cli-agent` entry under
// key keeps its exported credential bundle in, when its document points
// cli.auth.credential_bundle at none.
//
// A CONVENTION rather than a required setting, so `crewlet llm export
// -secret-store` and the engine's boot-time restore agree without the operator
// wiring cli.auth.credential_bundle by hand — and one function, so the writer
// and the reader cannot derive it differently.
//
// HERE rather than beside the backend that restores the bundle, because a
// third reader has to derive it too: config lists every variable a company
// document reads ([config.Company.VariablesRead]), a conventional one
// included, so a CLI run that resolves the document through a running node
// asks the node for this name — and config imports this package and not the
// backend.
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

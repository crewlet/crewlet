package seataccess

import (
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
)

// SinkName is the one name a seat's credential field is sealed under:
// `<KIND>_<FIELD>_<HANDLE>`, upper-snaked (`DATADOG_APP_KEY_SRE`,
// `ATLASSIAN_TOKEN_SRE`, `GITHUB_APP_KEY_SENIOR_ENGINEER`).
//
// ONE NAMING FUNCTION. It is [envref.SealedName], which setup.SecretNameFor
// (the connect form, GitHub's per-seat app values) and the org model's tool
// binding use too, and which the operator's `ProvisionedName` mirrors, so the
// engine's rendering and the operator's agree by construction. The name is
// never written in the company document: the org model binds it to the seat's
// tool ([ToolBinding]), and nothing a writer submits can point a seat's
// credential anywhere else (D4).
func SinkName(kind integration.Kind, field, handle string) string {
	return envref.SealedName(string(kind), field, handle)
}

// SinkNames is every sealed name for one seat at one kind, by field.
func SinkNames(cat Catalog, handle string) map[string]string {
	out := make(map[string]string, len(cat.Credentials))
	for _, field := range cat.Credentials {
		out[field] = SinkName(cat.Kind, field, handle)
	}
	return out
}

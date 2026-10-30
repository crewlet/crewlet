package seataccess

import (
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// SinkName is the one name a seat's credential field is sealed under:
// `<KIND>_<FIELD>_<HANDLE>`, upper-snaked (`DATADOG_APP_KEY_SRE`,
// `ATLASSIAN_TOKEN_SRE`, `GITHUB_APP_KEY_SENIOR_ENGINEER`).
//
// ONE NAMING FUNCTION. It is [setup.SecretNameFor]'s, which the connect form
// and GitHub's per-seat app values already use and which the operator's
// `ProvisionedName` mirrors, so the engine's rendering and the operator's
// agree by construction. The name is never written in the company document:
// the runtime binds it to the seat's tool ([ToolBinding]), and nothing a
// writer submits can point a seat's credential anywhere else (D4).
func SinkName(kind integration.Kind, field, handle string) string {
	return setup.SecretNameFor(kind, setup.Requirement{Field: field, Seat: handle})
}

// SinkNames is every sealed name for one seat at one kind, by field.
func SinkNames(cat Catalog, handle string) map[string]string {
	out := make(map[string]string, len(cat.Credentials))
	for _, field := range cat.Credentials {
		out[field] = SinkName(cat.Kind, field, handle)
	}
	return out
}

package llmapi

import (
	"errors"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
)

// hostLogin serves POST /llm/providers/{key}/host-login/check: the CLI's own
// status command, run on the answering node now, and the entries one login
// signs in with it.
//
// NEVER A SIGN-IN. The answer names the command an operator runs on that node,
// `crewlet llm login <key>`; no route relays a CLI's login.
func (s *Service) hostLogin(w http.ResponseWriter, r *http.Request, _ *types.ModelActed) {
	key := r.PathValue("key")
	company, active, ok := s.activeEntry(w, r, key)
	if !ok {
		return
	}
	if active == nil {
		notFound(w, key)
		return
	}
	login, err := s.engine.CheckHostLogin(r.Context(), key)
	switch {
	case errors.Is(err, ErrNoSuchModel):
		notFound(w, key)
		return
	case errors.Is(err, ErrNoHostLogin):
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeNoHostLogin, map[string]string{
			"detail": key + " signs in with auth.mode " + string(active.ResolvedAuthMode()) +
				"; there is no CLI login to check"})
		return
	case err != nil:
		s.fail(w, "check the host login", err)
		return
	}
	var checked any
	if !login.CheckedAt.IsZero() {
		checked = login.CheckedAt
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"node": s.node, "state": login.State, "method": optional(login.Method),
		"account": optional(login.Account), "detail": optional(login.Detail),
		"checked_at": checked, "command": "crewlet llm login " + key,
		"shares_login_with": sharesLogin(company, key, active),
	})
}

// sharesLogin is every other entry of company on the same CLI and the same
// state directory as key's, so one login signs them all in.
func sharesLogin(company *config.Company, key string, entry *config.LLMProvider) []string {
	out := []string{}
	dir, err := entry.ResolvedStateDir(key)
	if err != nil {
		return out
	}
	for _, other := range company.Providers.ProviderOrder() {
		if other == key {
			continue
		}
		candidate := company.Providers.LLM[other]
		if candidate.Harness != entry.Harness {
			continue
		}
		if theirs, err := candidate.ResolvedStateDir(other); err == nil && theirs == dir {
			out = append(out, other)
		}
	}
	slices.Sort(out)
	return out
}

// optional is a text that may be empty, as a JSON null when it is.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

package setupapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/setup"
)

// verify serves POST /setup/integrations/{kind}/verify: a typed credential
// checked at its vendor, and nothing stored.
//
// # It writes nothing, anywhere
//
// Not the value, not a pointer to it, not a revision and not a reconcile
// finding. The check is a read the vendor answers, and what it found goes
// back to the person who asked and to no shared record: a finding written
// from a value nobody saved would describe a company that does not exist.
//
// # The connect verb, and not the credential write
//
// It is `setup.connect` because it spends a credential at a vendor on a
// person's hand, which is the gesture of whoever connects the integration
// rather than of a reader. It does NOT ask `secrets.write` on top, as a
// submission carrying a credential does: nothing is sealed.
//
// # Every other field of the form is read the way the write would read it
//
// A check needs more than the one value (the region a Datadog key belongs
// to, the instance a token authenticates at). Those are what the person has
// typed beside it, else what the company document holds, else the field's
// default, so a person changing one key on a working integration tests it
// against the address already in force.
func (s *Service) verify(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	company := fc.company
	state, ok := s.stateFor(r.Context(), company, fc.roster, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{
			"detail": "this build serves no setup for " + string(kind),
			"hint":   "one of " + kindList(),
		})
		return
	}

	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req struct {
		Field  string            `json:"field"`
		Value  string            `json:"value"`
		Seat   string            `json:"seat"`
		Values map[string]string `json:"values"`
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := decode(body, &req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": plainjson.Unreadable("the body", err),
		})
		return
	}

	// THE SAME LIST A SUBMISSION IS MEASURED AGAINST: a seat's own, for a
	// credential that lives on the seat.
	against := state.Requirements
	if req.Seat != "" {
		found := false
		for _, seat := range state.Seats {
			if seat.Handle == req.Seat {
				against, found = seat.Requirements, true
				break
			}
		}
		if !found {
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeInvalidInput, map[string]string{
				"detail": "no seat called " + req.Seat + " takes per-seat setup for " + string(kind),
			})
			return
		}
	}

	target, refusal := checkedField(kind, req.Seat, req.Field, req.Value, req.Values, against)
	if refusal != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": refusal,
		})
		return
	}

	// A FIELD NOTHING CAN CHECK AHEAD answers its own sentence, and no
	// vendor is asked: see [setup.Requirement.CheckNote].
	if !target.Checkable {
		s.answerVerdict(w, r, kind, req.Seat, target.Field, setup.Unchecked(target.CheckNote))
		return
	}

	typed := map[string]string{target.Field: req.Value}
	for field, value := range req.Values {
		if field != target.Field {
			typed[field] = value
		}
	}
	// EVERY TYPED REFERENCE IS RESOLVED HERE, and one naming nothing is the
	// request's fault, named: checked as the literal text `${NAME}`, a
	// vendor would refuse it and the person would rotate a key that may
	// be fine, under a name that does not exist.
	for field, value := range typed {
		resolved, missing := s.dereference(value)
		if missing != "" {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
				"detail": "There is no secret " + missing + ".",
				"field":  field,
				"hint":   "store it first, or type the value itself",
			})
			return
		}
		typed[field] = resolved
	}
	// THE SITE, where only the Atlassian organization knows it. The same
	// discovery a submission makes, so a token is checked at the site it
	// would be saved for. It reads the organization and writes nothing.
	if note := s.discoverSite(r.Context(), company, kind, against, typed); note != "" {
		s.answerVerdict(w, r, kind, req.Seat, target.Field, setup.Verdict{
			Outcome: setup.VerifyUnreachable,
			Detail: "The Atlassian organization could not be read, so there is no " +
				"site to check this against. Nothing was stored.",
		})
		return
	}

	probe := s.probes[kind]
	if probe == nil {
		// [New] refuses a probe set missing a kind, so this is unreachable
		// short of a kind added to the switch in [Service.state] and not to
		// [integration.Kinds].
		log.ErrorContext(r.Context(), "setup_probe_missing", "kind", kind)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setup.VerifyBudget)
	defer cancel()
	verdict, err := probe(ctx, setup.Check{
		Field: target.Field,
		Value: s.formValues(typed, against),
		// Nil-safe on a company with no Atlassian block, which declared
		// nothing.
		Declared: company.Integrations.Atlassian.Declared(),
	})
	switch {
	case errors.Is(err, setup.ErrUnusableInput):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": err.Error(),
		})
		return
	case err != nil:
		log.ErrorContext(r.Context(), "setup_probe_failed",
			"kind", kind, "field", target.Field, "error", err.Error())
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	case !verdict.Outcome.Valid():
		log.ErrorContext(r.Context(), "setup_probe_failed", "kind", kind,
			"field", target.Field, "error", "the probe answered no outcome this build knows",
			"outcome", string(verdict.Outcome))
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	s.answerVerdict(w, r, kind, req.Seat, target.Field, verdict)
}

// answerVerdict writes a verdict and logs that a check was made.
//
// NAMES, NEVER VALUES, in the log as in every other line this surface
// writes: which field of which integration was checked, by whom, and what
// the vendor said.
func (s *Service) answerVerdict(w http.ResponseWriter, r *http.Request,
	kind integration.Kind, seat, field string, verdict setup.Verdict,
) {
	by := attributionOf(r)
	log.InfoContext(r.Context(), "setup_credential_checked",
		"kind", kind, "field", field, "seat", seat, "outcome", string(verdict.Outcome),
		"by", by.Name, "operator", by.OperatorID)
	httpjson.Write(w, http.StatusOK, verdict)
}

// checkedField is the requirement a check is about, or the sentence refusing
// the request.
func checkedField(kind integration.Kind, seat, field, value string,
	values map[string]string, against []setup.Requirement,
) (setup.Requirement, string) {
	where := string(kind)
	if seat != "" {
		where = seat + "'s " + string(kind) + " setup"
	}
	byField := make(map[string]setup.Requirement, len(against))
	for _, r := range against {
		byField[r.Field] = r
	}
	if strings.TrimSpace(field) == "" {
		return setup.Requirement{}, "Name the field to check."
	}
	target, ok := byField[field]
	if !ok {
		return setup.Requirement{}, field + " is not a field of " + where + "."
	}
	if target.Kind != setup.KindSecret {
		return setup.Requirement{}, field + " is not a credential, and only a " +
			"credential can be checked."
	}
	if strings.TrimSpace(value) == "" {
		return setup.Requirement{}, "Enter the value to check."
	}
	// THE REST OF THE FORM IS HELD TO THE SAME LIST, the way a submission's
	// values are: a key that names no field is a form and this surface
	// disagreeing about what the integration takes, and silently dropping
	// it would check against a document the person did not describe.
	for name := range values {
		if _, ok := byField[name]; !ok {
			return setup.Requirement{}, name + " is not a field of " + where + "."
		}
	}
	return target, ""
}

// dereference resolves a whole `${NAME}` through this node's chain, and
// answers the name when nothing holds it. Anything else is the value as
// typed.
func (s *Service) dereference(value string) (resolved, missing string) {
	trimmed := strings.TrimSpace(value)
	name, whole := envref.Whole(trimmed)
	if !whole {
		return value, ""
	}
	got, ok := s.resolve(name)
	if !ok || strings.TrimSpace(got) == "" {
		return "", name
	}
	return strings.TrimSpace(got), ""
}

// formValues is [setup.Check.Value] over one request: what was typed, else
// what the document holds (a whole `${VAR}` read through this node's
// chain), else the field's default.
//
// THROUGH [setup.Deref] FOR THE STORED HALF, the rule the form's own links
// are drawn by, so the address a check is made against is the address the
// form displayed.
func (s *Service) formValues(typed map[string]string, against []setup.Requirement) func(string) string {
	return func(field string) string {
		if value := strings.TrimSpace(typed[field]); value != "" {
			return value
		}
		for _, r := range against {
			if r.Field != field {
				continue
			}
			if stored := setup.Deref(r.Stored, s.resolve); stored != "" {
				return stored
			}
			return strings.TrimSpace(r.Default)
		}
		return ""
	}
}

// probesComplete reports the integration kinds a probe set leaves out.
//
// EVERY KIND THIS BUILD SETS UP HAS A CREDENTIAL A PERSON TYPES, so a set
// missing one is a Test button that answers 500 on that integration's form.
func probesComplete(probes map[integration.Kind]setup.Probe) []string {
	var missing []string
	for _, kind := range integration.Kinds {
		if probes[kind] == nil {
			missing = append(missing, string(kind))
		}
	}
	return missing
}

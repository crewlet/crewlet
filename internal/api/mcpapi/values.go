package mcpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/secrets"
)

// `PUT /mcp-servers/{name}/values/{key}` stores `{"value": "..."}` for a key
// (`seat` for one seat's, `test=false` untested): admitted, slotted, tested,
// sealed write-only, then pointed or re-activated (ADR-0070). Nothing answered,
// logged or audited carries the value. The key is a header or variable name,
// or `oauth.client_secret` ([config.MCPValueClientSecret]) for the secret of
// the company's own OAuth client: a service identity's is tested by signing in
// with it; a person's grant's is stored untested, since it is first sent at the
// grant's next renewal.

// ValueStore seals a value write-only and lists the rows by name.
type ValueStore interface {
	SetWriteOnly(ctx context.Context, name, value string, by secrets.Author, source secrets.Source,
		now time.Time) error
	List(ctx context.Context) ([]secrets.Record, error)
}

// ValueDocuments writes a stored value's pointer ([configapi.Service]).
type ValueDocuments interface {
	Authorize(by iam.Actor) error
	ActiveCompany(ctx context.Context) (*config.Company, string, error)
	CheckMCPValue(ctx context.Context, slot config.MCPValueSlot, by iam.Actor) (*config.Company, error)
	PointMCPValue(ctx context.Context, slot config.MCPValueSlot, summary string, by iam.Actor) (configapi.Applied, error)
	Reload(ctx context.Context, summary string, by iam.Actor, expect string) (configapi.Applied, error)
}

// ValueTry is a value tested before it is stored; Value is never answered or
// logged.
type ValueTry struct {
	Key, Variable, Value string
}

// valueRequest is the body the route reads.
type valueRequest struct {
	Value *string `json:"value"`
}

// maxValueBody is a value of the store's bound at its longest JSON spelling.
const maxValueBody = 6*secrets.MaxValueBytes + 64

// ValueAnswer is what the route answers once the value is stored.
type ValueAnswer struct {
	Server string `json:"server"`
	Key    string `json:"key"`
	// Seat is "" for the server's own value.
	Seat     string `json:"seat"`
	Variable string `json:"variable"`
	Tested   bool   `json:"tested"`
	// Check is the test, null for a value stored untested.
	Check *CheckAnswer `json:"check"`
	// Pointed is whether the document was written to read the variable.
	Pointed bool `json:"pointed"`
	// Reload is the revision that resolves the value: the pointer's write
	// or the re-activation.
	Reload *configapi.Reloaded `json:"reload"`
	// Values are the server's, or the seat's, now. Always a list.
	Values []queries.MCPValueState `json:"values"`
}

// serveValue stores a value of the server its path names.
func (s *Service) serveValue(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	ctx := r.Context()
	name, key := r.PathValue("name"), r.PathValue("key")
	rec.Key = key
	// The pointer is a document write, decided before the body is read.
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
		return
	}
	if s.documents == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no configuration surface to write the value's " +
				"pointer through; ask through a node that serves the dashboard"})
		return
	}
	by := auth.AttributionOf(ctx)
	var managed *configapi.ManagedError
	if err := s.documents.Authorize(by); errors.As(err, &managed) {
		configapi.RefuseManaged(w, managed)
		return
	}
	seat, test, ok := valueQuery(w, r)
	if !ok {
		return
	}
	rec.Seat = seat
	value, ok := readValue(w, r)
	if !ok {
		return
	}

	company, _, err := s.documents.ActiveCompany(ctx)
	if !configapi.RefuseDraft(w, err) {
		return
	}
	slot, err := company.MCPValueSlot(name, seat, key)
	if err != nil {
		refuseSlot(w, name, seat, key, err)
		return
	}
	rec.Key, rec.Variable = slot.Key, slot.Variable
	if slot.Repoint {
		if _, err = s.documents.CheckMCPValue(ctx, slot, by); err != nil {
			refusePointer(w, slot, err, false)
			return
		}
	}

	answer := ValueAnswer{Server: name, Key: slot.Key, Seat: seat, Variable: slot.Variable, Tested: test,
		Values: []queries.MCPValueState{}}
	if test && slot.As == config.MCPValueOAuth && !serviceIdentity(company, name) {
		// A PERSON'S GRANT SENDS ITS CLIENT'S SECRET ONLY WHEN IT RENEWS,
		// under the fleet's renewal lease, which a test does not take: a
		// test would pass on the grant's token and say nothing of the
		// secret.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			httpjson.Detail{"detail": "the secret of the client a person signed MCP server " + quoted(name) +
				" in with is first sent at the grant's next renewal, which a test does not make: store " +
				"it with test=false"})
		return
	}
	if test {
		check, ok := s.testValue(w, r, company, slot, value, by)
		if !ok {
			return
		}
		answer.Check = &check
	}

	// Past the test, the write finishes whatever the caller does.
	ctx = context.WithoutCancel(ctx)
	author := secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
	if err = s.values.SetWriteOnly(ctx, slot.Variable, value, author, secrets.SourceMCP, s.now()); err != nil {
		log.ErrorContext(ctx, "mcp_value_seal_failed", "server", name, "key", slot.Key, "seat", seat,
			"variable", slot.Variable, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	log.InfoContext(ctx, "mcp_value_stored", "server", name, "key", slot.Key, "seat", seat,
		"variable", slot.Variable, "bytes", len(value), "tested", test, "by", by.Name, "operator", by.OperatorID)

	if slot.Repoint {
		applied, pointErr := s.documents.PointMCPValue(ctx, slot, valueSummary(slot), by)
		if pointErr != nil {
			refusePointer(w, slot, pointErr, true)
			return
		}
		answer.Pointed = true
		answer.Reload = &configapi.Reloaded{RevisionID: applied.RevisionID, Epoch: applied.Epoch}
		if err = company.PointMCPValue(slot); err != nil {
			log.ErrorContext(ctx, "mcp_value_answer_failed", "server", name, "key", slot.Key, "error", err)
		}
	} else {
		// The key already reads the variable: re-activate (ADR-0070).
		applied, reloadErr := s.documents.Reload(ctx, "reload: secret "+slot.Variable+" written for MCP server "+name,
			by, "")
		answer.Reload = configapi.ReloadOutcome(applied, reloadErr)
		if reloadErr != nil {
			log.WarnContext(ctx, "mcp_value_reload_failed", "server", name, "variable", slot.Variable,
				"error", reloadErr, "detail", "the value is sealed; POST /config/reload re-activates the company")
		}
	}
	answer.Values, err = s.valuesOf(ctx, company, name, seat)
	if err != nil {
		// The write landed; the detail answers the listing on its next read.
		log.WarnContext(ctx, "mcp_values_unread", "server", name, "error", err)
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// testValue tests the value as the entry will run with it; ok is false when
// the refusal has been answered.
func (s *Service) testValue(w http.ResponseWriter, r *http.Request, company *config.Company,
	slot config.MCPValueSlot, value string, by iam.Actor) (CheckAnswer, bool) {

	if slot.Seat == "" && !sharedServer(company, slot.Server) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			httpjson.Detail{"detail": "MCP server " + quoted(slot.Server) + " runs one instance per agent, " +
				"so its own value is tested as one of them: store the value for a seat with seat=<handle>, " +
				"or store it untested with test=false"})
		return CheckAnswer{}, false
	}
	// THE ENTRY AS THE FLEET'S ACTIVE REVISION STATES IT, sent with the test:
	// a value stored straight after its entry's write is tested on nodes that
	// have not applied that write yet, rather than refused by one whose
	// company does not have the server.
	entry := activeEntry(company, slot.Server)
	done, err := s.checker.CheckServer(r.Context(), CheckRequest{Server: slot.Server, Seat: slot.Seat,
		Entry: entry, Values: []ValueTry{{Key: slot.Key, Variable: slot.Variable, Value: value}}, By: by})
	if err != nil {
		s.refuseServerGesture(w, r, slot.Server, slot.Seat, err, "mcp_value_test_failed",
			"this node could not ask the fleet to test the value; nothing was stored; ask again")
		return CheckAnswer{}, false
	}
	check := CheckAnswerOf(done)
	if done.OK {
		return check, true
	}
	failed := failedNode(done)
	if failed == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds, httpjson.Detail{
			"detail": "no node running MCP server " + quoted(slot.Server) + " answered the test, so nothing " +
				"was stored: ask again, or store it untested with test=false",
			"check": check})
		return CheckAnswer{}, false
	}
	stage, class, said := failedAt(failed.Report)
	message := "MCP server " + quoted(slot.Server) + " did not work with this value on " + failed.Node
	if stage != "" {
		message += ": it stopped at " + string(stage)
	}
	if class != "" {
		message += " (" + string(class) + ")"
	}
	if said != "" {
		message += ": " + said
	}
	message = httpjson.Sentence(message) + " Nothing was stored."
	path := config.Path{"values", slot.Key}
	httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValueTestFailed, httpjson.Detail{
		"detail": message, "stage": stage, "class": class, "node": failed.Node, "check": check,
		"problems": []config.Problem{{Path: path.String(), Segments: path, Kind: "invalid", Message: message}},
	})
	return CheckAnswer{}, false
}

// activeEntry is a copy of the entry called name in the active company, nil
// where it declares none (the slot that named it read the same company).
func activeEntry(company *config.Company, name string) *config.MCPServer {
	for i := range company.MCPServers {
		if company.MCPServers[i].Name == name {
			entry := company.MCPServers[i]
			return &entry
		}
	}
	return nil
}

// serviceIdentity reports whether the server called name signs in as a
// service identity (oauth.grant: client_credentials).
func serviceIdentity(company *config.Company, name string) bool {
	for i := range company.MCPServers {
		if company.MCPServers[i].Name == name {
			return company.MCPServers[i].OAuth.ServiceIdentity()
		}
	}
	return false
}

// sharedServer reports whether the server called name runs one instance.
func sharedServer(company *config.Company, name string) bool {
	for i := range company.MCPServers {
		if company.MCPServers[i].Name == name {
			return company.MCPServers[i].IsShared()
		}
	}
	return true
}

// failedNode is the first answering node not found serving, nil for none.
func failedNode(c Check) *CheckedNode {
	for i, n := range c.Nodes {
		if n.Answered && !n.Report.Serving() {
			return &c.Nodes[i]
		}
	}
	return nil
}

// failedAt is a test's failed stage, class and (redacted) sentence.
func failedAt(report mcp.TestReport) (mcp.TestStage, mcp.FailureClass, string) {
	var stage mcp.TestStage
	for _, step := range report.Steps {
		if step.State == mcp.StepFailed {
			stage = step.Stage
			break
		}
	}
	if report.Failure == nil {
		return stage, "", ""
	}
	return stage, report.Failure.Class, report.Failure.Said
}

// valuesOf is the server's values, or the seat's, against the store's rows.
func (s *Service) valuesOf(ctx context.Context, company *config.Company, name, seat string) (
	[]queries.MCPValueState, error) {

	keys, ok := company.MCPValueKeys(name, seat)
	if !ok {
		return []queries.MCPValueState{}, nil
	}
	rows, err := s.values.List(ctx)
	if err != nil {
		return []queries.MCPValueState{}, err
	}
	return queries.MCPValuesOf(keys, rows), nil
}

// valueSummary is the revision's summary for a pointer: names only.
func valueSummary(slot config.MCPValueSlot) string {
	out := "store " + slot.Key + " of MCP server " + slot.Server
	if slot.Seat != "" {
		out += " for seat " + slot.Seat
	}
	return out + " as " + slot.Variable
}

// valueQuery reads `seat` and `test` (default true), each at most once.
func valueQuery(w http.ResponseWriter, r *http.Request) (seat string, test, ok bool) {
	query := r.URL.Query()
	for param, values := range query {
		if (param != "seat" && param != "test") || len(values) != 1 {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				httpjson.Detail{"detail": "this route takes seat=<handle> and test=true|false, each at most once, " +
					"and no other parameter"})
			return "", false, false
		}
	}
	test = true
	if values, given := query["test"]; given {
		if values[0] != "true" && values[0] != "false" {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				httpjson.Detail{"detail": "test must be true or false"})
			return "", false, false
		}
		test = values[0] == "true"
	}
	return strings.TrimSpace(query.Get("seat")), test, true
}

// readValue reads the body's value, trimmed, one line, within the store's
// bound; a refusal never quotes it.
func readValue(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req valueRequest
	if !decodeBodyWithin(w, r, &req, `{"value": "..."}`, maxValueBody) {
		return "", false
	}
	if req.Value == nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": `the body is {"value": "..."}, what the server is sent under the key`})
		return "", false
	}
	value, refusal := checkValue(*req.Value)
	if refusal != nil {
		refusal.answer(w, "the value")
		return "", false
	}
	return value, true
}

// valueRefusal is why a value is not one a server can be sent, said of the
// value without quoting it.
type valueRefusal struct {
	status int
	code   httpjson.Code
	// why follows the value's name in the sentence.
	why string
}

func (v *valueRefusal) answer(w http.ResponseWriter, what string) {
	httpjson.FailWithFields(w, v.status, v.code, httpjson.Detail{"detail": what + " " + v.why})
}

// checkValue is raw as a server is sent it, trimmed: one line, not empty, and
// within the store's bound. Shared by a stored value and a draft's.
func checkValue(raw string) (string, *valueRefusal) {
	value := strings.TrimSpace(raw)
	switch {
	case value == "":
		return "", &valueRefusal{http.StatusBadRequest, httpjson.CodeInvalidBody,
			"is empty: send what the server is sent under the key"}
	case strings.ContainsAny(value, "\r\n\x00"):
		return "", &valueRefusal{http.StatusBadRequest, httpjson.CodeInvalidBody,
			"holds a line break or a NUL inside it: a header's or a variable's value is one line"}
	}
	if err := secrets.CheckValue(value); err != nil {
		return "", &valueRefusal{http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge, "is too long: " + err.Error()}
	}
	return value, nil
}

// refuseSlot answers a slot the document refused.
func refuseSlot(w http.ResponseWriter, name, seat, key string, err error) {
	var shared *config.MCPValueSharedError
	var moved *configapi.MCPValueMovedError
	switch {
	case errors.Is(err, config.ErrNoMCPServerEntry):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "the active configuration declares no MCP server " + quoted(name)})
	case errors.Is(err, config.ErrMCPValueSeatOnShared):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			httpjson.Detail{"detail": "MCP server " + quoted(name) + " is shared: it runs one instance for " +
				"the company with its own values, so name no seat"})
	case errors.Is(err, config.ErrMCPValueNoSeat):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNoSuchSeat,
			httpjson.Detail{"detail": "the organization has no agent seat " + quoted(seat)})
	case errors.Is(err, config.ErrMCPValueNotGranted):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "seat " + quoted(seat) + " is not granted MCP server " + quoted(name) +
				", so it runs no instance to store a value for; grant it the server first"})
	case errors.Is(err, config.ErrMCPValueBound):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": httpjson.Sentence(strings.TrimPrefix(err.Error(), "config: ")) +
				" The integration hands each agent its own, so nobody stores one here."})
	case errors.Is(err, config.ErrMCPValueKey):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeBadParams,
			httpjson.Detail{"detail": quoted(key) + " is not a header or variable name"})
	case errors.Is(err, config.ErrMCPValueNoSecret):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "MCP server " + quoted(name) + " signs in with no client secret: " +
				config.MCPValueClientSecret + " is the secret of the company's own OAuth client " +
				"(oauth.grant: client_credentials, or oauth.client: own), and its oauth block names neither"})
	case errors.As(err, &shared):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "this value would be stored as " + shared.Variable + ", which " +
				strings.Join(shared.Readers, ", ") + " already reads; give that field a variable of its " +
				"own, or rename one of the servers", "readers": shared.Readers})
	case errors.As(err, &moved):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "the configuration changed while the value was prepared: the key now " +
				"reads " + moved.Now + "; send the value again"})
	default:
		log.Error("mcp_value_slot_failed", "server", name, "key", key, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}

// refusePointer answers a refused pointer; after the seal (stored), it says
// the value is stored and the same request again finishes it.
func refusePointer(w http.ResponseWriter, slot config.MCPValueSlot, err error, stored bool) {
	var moved *configapi.MCPValueMovedError
	var shared *config.MCPValueSharedError
	switch {
	case !stored && (errors.As(err, &moved) || errors.As(err, &shared) || isSlotErr(err)):
		refuseSlot(w, slot.Server, slot.Seat, slot.Key, err)
		return
	case !stored:
		configapi.RefuseDraft(w, err)
		return
	}
	detail := httpjson.Detail{
		"variable": slot.Variable, "stored": true,
		"detail": "The value is stored as " + slot.Variable + ", and the configuration was not changed " +
			"to read it: " + strings.TrimPrefix(err.Error(), "configapi: "),
		"hint": "send the same value again: the next write points the key at it",
	}
	var managed *configapi.ManagedError
	var invalid *configapi.ValidationError
	switch {
	case errors.As(err, &managed):
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeConfigManaged, detail)
	case errors.As(err, &invalid):
		for k, v := range configapi.RefusalFields(err) {
			if _, set := detail[k]; !set {
				detail[k] = v
			}
		}
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError, detail)
	case errors.As(err, &moved), errors.As(err, &shared), isSlotErr(err):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, detail)
	default:
		log.Error("mcp_value_pointer_failed", "server", slot.Server, "key", slot.Key,
			"variable", slot.Variable, "error", err)
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeInternalError, detail)
	}
}

// isSlotErr reports whether err is one of the slot's own refusals.
func isSlotErr(err error) bool {
	for _, sentinel := range []error{config.ErrNoMCPServerEntry, config.ErrMCPValueSeatOnShared,
		config.ErrMCPValueNoSeat, config.ErrMCPValueNotGranted, config.ErrMCPValueBound, config.ErrMCPValueKey} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

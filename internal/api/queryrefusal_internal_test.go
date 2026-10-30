package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A REFUSED QUESTION ANSWERS IN THE AUTHORITY TABLE'S ENVELOPE.
//
// A question's REST refusal is the registry's, carried whole, and it is
// answered through the table's one mapping — so its status, its code and its
// fields are byte for byte what the router writes for the same decision on
// every other surface ([authz.EnvelopeRefusal]). It spelled `unauthorized`
// beside the reason and the grants for itself, which agreed with the mapping
// for every refusal a question can reach today and would have answered a
// stale proof as a missing permission, its window dropped — and an artifact's
// refusal for a role without the role it needs.
//
// Mutation: answer with httpjson.CodeUnauthorized and the reason and grants
// alone again and the step-up, token, role and no-relation cases go red.
func TestARefusedQuestionAnswersInTheAuthorityTablesEnvelope(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]authz.Decision{
		"stage":    {Reason: authz.ReasonStage},
		"no grant": {Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}},
		"not lead": {Reason: authz.ReasonNotLead, Grants: []iam.Grant{iam.GrantAdmin}},
		"unseated": {Reason: authz.ReasonUnseated},
		"step up": {Reason: authz.ReasonStepUp, Recency: iam.RecencyStepUp,
			Grants: []iam.Grant{iam.GrantAdmin}},
		"token refused": {Reason: authz.ReasonTokenRefused},
		"role":          {Reason: authz.ReasonRole, Role: artifacts.RoleEditor, Grants: []iam.Grant{}},
		"no relation":   {Reason: authz.ReasonNotShared},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := httptest.NewRecorder()
			// WRAPPED, as an answer's failure reaches the mapping through
			// the registry's classification.
			writeQueryError(got, httptest.NewRequest(http.MethodGet, "/query/q", nil), "q",
				fmt.Errorf("answering q: %w", queries.NewRefusal("q", d)))
			want := httptest.NewRecorder()
			authz.EnvelopeRefusal(want, httptest.NewRequest(http.MethodGet, "/query/q", nil),
				authz.Policy{}, d)
			if got.Code != want.Code || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
				t.Errorf("a refused question answered %d %s, want the table's %d %s",
					got.Code, got.Body, want.Code, want.Body)
			}
			if got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Errorf("Content-Type = %q, want %q", got.Header().Get("Content-Type"),
					want.Header().Get("Content-Type"))
			}
		})
	}
}

// A BARE REFUSAL SENTINEL NAMES NO DECISION, AND IS NO REFUSAL.
//
// Only a [queries.Refusal] carries the decision the mapping words a 403 from,
// so the one shape that could reach the mapping without one — the sentinel
// alone — is answered as the fault it is, never as an `unauthorized` that
// cannot say what would change it.
//
// Mutation: answer a bare queries.ErrUnauthorized with a 403 again and this
// goes red.
func TestABareRefusalSentinelIsAFault(t *testing.T) {
	t.Parallel()
	got := httptest.NewRecorder()
	writeQueryError(got, httptest.NewRequest(http.MethodGet, "/query/q", nil), "q",
		fmt.Errorf("answering q: %w", queries.ErrUnauthorized))
	if got.Code != http.StatusInternalServerError {
		t.Errorf("a refusal naming no decision answered %d %s, want the fault's 500",
			got.Code, got.Body)
	}
}

// A REVISION THIS NODE CANNOT OPEN IS ANSWERED IN THE /config ROUTE'S CODE AND
// WORDS on the REST question route too, as the socket's frame carries them:
// 409 under the code, with the detail that names what fixes it. Answered by
// the default arm, a missing key was a 500 nobody could act on.
//
// Mutation: drop the arm and this answers 500.
func TestAQuestionOnARevisionThisNodeCannotOpenIsTheRoutesConflict(t *testing.T) {
	t.Parallel()
	got := httptest.NewRecorder()
	refusal := &stream.RevisionError{What: "config", Code: stream.CodeUnreadableFleetRevision,
		Detail: "This node doesn't hold the key the fleet sealed this revision under. Add " +
			"the key the fleet seals under to this node's secrets.keys and restart it."}
	writeQueryError(got, httptest.NewRequest(http.MethodGet, "/query/config", nil), "config",
		fmt.Errorf("answering config: %w", refusal))
	var body struct{ Error, Detail string }
	_ = json.Unmarshal(got.Body.Bytes(), &body)
	if got.Code != http.StatusConflict || body.Error != stream.CodeUnreadableFleetRevision ||
		body.Detail != refusal.Detail {
		t.Errorf("a question on a revision this node cannot open answered %d %s, want 409 "+
			"%s with the refusal's detail", got.Code, got.Body, stream.CodeUnreadableFleetRevision)
	}
}

package assistantapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/assistantapi"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval/approvaltest"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// audits is the compliance read's domain half, recording the order of its two
// steps.
type audits struct {
	mu       sync.Mutex
	steps    []string
	reason   string
	reader   iam.Actor
	owner    uuid.UUID
	missing  bool
	unopened bool
}

func (a *audits) RecordAuditRead(_ context.Context, c assistantdomain.Call, _ uuid.UUID,
	reason string) (assistantdomain.AuditReceipt, error) {

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.missing {
		return assistantdomain.AuditReceipt{}, assistantdomain.ErrNotFound
	}
	a.steps, a.reason, a.reader = append(a.steps, "record"), reason, c.By
	return assistantdomain.AuditReceipt{}, nil
}

func (a *audits) AuditTranscript(_ context.Context, _ assistantdomain.AuditReceipt) (
	assistantdomain.Transcript, error) {

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unopened {
		return assistantdomain.Transcript{}, assistantdomain.ErrNotFound
	}
	a.steps = append(a.steps, "open")
	return assistantdomain.Transcript{Conversation: assistantdomain.Conversation{Owner: a.owner}}, nil
}

// published is every runtime audit record.
type published struct {
	mu     sync.Mutex
	events []*events.Event
}

func (p *published) Publish(_ context.Context, _ string, ev *events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

// surface is the decision routes over one fleet, behind the authority table.
type surface struct {
	fleet   *approvaltest.Fleet
	audits  *audits
	audit   *published
	handler http.Handler
}

func newSurface(t *testing.T) *surface {
	t.Helper()
	s := &surface{fleet: approvaltest.NewFleet(), audits: &audits{owner: uuid.New()},
		audit: &published{}}
	d, err := assistantapi.NewDecisions(assistantapi.DecisionsDeps{
		Approver: s.fleet.Node(t), Audits: s.audits, Audit: s.audit,
		Render: func(_ context.Context, t assistantdomain.Transcript) (wire.Transcript, error) {
			s.audits.mu.Lock()
			defer s.audits.mu.Unlock()
			s.audits.steps = append(s.audits.steps, "render")
			return wire.Transcript{Conversation: wire.ConversationSummary{ID: t.Conversation.ID}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := d.Mount(authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))); err != nil {
		t.Fatal(err)
	}
	s.handler = mux
	return s
}

// send is one request as p, answering its status and envelope.
func (s *surface) send(t *testing.T, p iam.Principal, method, path string, body any,
	header map[string]string) (int, map[string]any) {

	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r = r.WithContext(approvaltest.As(r.Context(), p))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(opkey.Header, uuid.Must(uuid.NewV7()).String())
	for k, v := range header {
		if v == "" {
			r.Header.Del(k)
			continue
		}
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// decide is a decision body echoing digest.
func decide(digest string) wire.ProposalDecision {
	return wire.ProposalDecision{ProtocolVersion: wire.ProtocolVersion, Digest: digest}
}

func approvePath(p assistantdomain.Proposal) string {
	return "/assistant/proposals/" + p.ID.String() + "/approve"
}

// AN APPROVAL ANSWERS THE STATE IT LEFT THE PROPOSAL IN, and where the resumed
// turn streams.
func TestAnApprovalAnswersWhereTheTurnResumes(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := s.fleet.Propose(t, ada, nil)
	status, body := s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
	if status != http.StatusAccepted {
		t.Fatalf("approve answered %d %v", status, body)
	}
	ref, _ := body["proposal"].(map[string]any)
	if ref["state"] != string(wire.ProposalApplied) || body["stream_url"] != approvaltest.StreamURL ||
		body["turn_id"] != p.Turn.String() {
		t.Errorf("approve answered %v", body)
	}
}

// EVERY DECISION REFUSES IN THE ENGINE'S ENVELOPE, each refusal its own code
// and status, so the card renders the right state and offers the right next
// step. Mutation: map any sentinel to another code and its row fails.
func TestEveryDecisionRefusesInTheEnvelope(t *testing.T) {
	t.Parallel()
	fresh := approvaltest.Noon.Add(time.Hour)
	cases := []struct {
		name   string
		run    func(t *testing.T, s *surface) (int, map[string]any)
		status int
		code   string
	}{{
		name: "another person's proposal",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			p := s.fleet.Propose(t, approvaltest.Person(fresh), nil)
			return s.send(t, approvaltest.Person(fresh), http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusNotFound, code: "not_found",
	}, {
		name: "an id that is no id",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			return s.send(t, approvaltest.Person(fresh), http.MethodPost,
				"/assistant/proposals/x/approve", decide("sha256:"+strings.Repeat("a", 64)), nil)
		},
		status: http.StatusNotFound, code: "not_found",
	}, {
		name: "a proposal already decided",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusConflict, code: "proposal_already_decided",
	}, {
		name: "an expired proposal",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh.Add(time.Hour))
			p := s.fleet.Propose(t, ada, nil)
			s.fleet.Set(p.ExpiresAt)
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusGone, code: "proposal_expired",
	}, {
		name: "another card's digest",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			return s.send(t, ada, http.MethodPost, approvePath(p),
				decide("sha256:"+strings.Repeat("b", 64)), nil)
		},
		status: http.StatusConflict, code: "proposal_digest_mismatch",
	}, {
		name: "a stale revision",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			s.fleet.Revision = func(string) (string, error) { return "task:ENG-1@9", nil }
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusConflict, code: "proposal_stale",
	}, {
		name: "a step-up the write asks",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(approvaltest.Noon.Add(-time.Minute))
			p := s.fleet.Propose(t, ada, func(d *proposal.Draft) { d.Preview.StepUp = true })
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusForbidden, code: "step_up_required",
	}, {
		name: "a claim another tab made",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, pageAction)
			s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
			claim := wire.ActionReport{ProtocolVersion: wire.ProtocolVersion,
				State: wire.ActionApplying, Revision: "r1"}
			s.send(t, ada, http.MethodPut, "/assistant/actions/"+p.ID.String(), claim, nil)
			return s.send(t, ada, http.MethodPut, "/assistant/actions/"+p.ID.String(), claim, nil)
		},
		status: http.StatusConflict, code: "action_already_claimed",
	}, {
		name: "another protocol",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			body := decide(p.Digest)
			body.ProtocolVersion = wire.ProtocolVersion + 1
			return s.send(t, ada, http.MethodPost, approvePath(p), body, nil)
		},
		status: http.StatusConflict, code: "assistant_protocol_mismatch",
	}, {
		name: "no operation",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest),
				map[string]string{opkey.Header: ""})
		},
		status: http.StatusBadRequest, code: "op_id_invalid",
	}, {
		name: "a body that is not JSON",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest),
				map[string]string{"Content-Type": "text/plain"})
		},
		status: http.StatusUnsupportedMediaType, code: "unsupported_media_type",
	}, {
		name: "a decision nobody can confirm",
		run: func(t *testing.T, s *surface) (int, map[string]any) {
			ada := approvaltest.Person(fresh)
			p := s.fleet.Propose(t, ada, nil)
			s.fleet.Store.AnswerUnknown()
			return s.send(t, ada, http.MethodPost, approvePath(p), decide(p.Digest), nil)
		},
		status: http.StatusServiceUnavailable, code: "unavailable",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			status, body := tc.run(t, s)
			if status != tc.status || body["error"] != tc.code {
				t.Fatalf("answered %d %v, want %d %s", status, body, tc.status, tc.code)
			}
			if message, _ := body["message"].(string); message == "" {
				t.Errorf("the refusal carries no sentence: %v", body)
			}
		})
	}
}

// pageAction makes a draft a page action.
func pageAction(d *proposal.Draft) {
	d.Tool, d.Act, d.Scope = "propose_page_change", "", wire.ScopePage
	d.Preview.SourceRevision, d.PageRevision = "", "r1"
	d.Preview.Request.Method = proposal.PageMethod
	d.Preview.Request.Path = proposal.PageActionPath + "work.filters"
	d.Preview.Request.Body = []byte(`{"status":"open"}`)
}

// THE DECISION ROUTES ARE NEVER THE ASSISTANT'S: a principal riding an
// assistant grant is refused every one by the authority table, so the
// assistant cannot approve, decline or report on its own proposal even where
// the guard's own refusal of `/assistant/` were missing.
func TestTheAssistantCannotApproveItsOwnProposal(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := s.fleet.Propose(t, ada, nil)
	assisted := ada
	assisted.Via, assisted.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), iam.AssistScopeExec
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, approvePath(p)},
		{http.MethodPost, "/assistant/proposals/" + p.ID.String() + "/decline"},
		{http.MethodPut, "/assistant/actions/" + p.ID.String()},
	} {
		status, body := s.send(t, assisted, route.method, route.path, decide(p.Digest), nil)
		if status != http.StatusForbidden || body["reason"] != string(authz.ReasonAssistRefused) {
			t.Errorf("%s %s through the assistant answered %d %v", route.method, route.path,
				status, body)
		}
	}
	if len(s.fleet.API.Sent()) != 0 || s.fleet.Store.Recorded() != 0 {
		t.Error("an assistant's own approval reached the proposal")
	}
}

// admin is an administrator, proved until reauth.
func admin(reauth time.Time) iam.Principal {
	p := approvaltest.Person(reauth)
	p.Login, p.Seat, p.Grants = "grace", "grace", []iam.Grant{iam.GrantAdmin}
	return p
}

// readBody is a compliance read's body.
func readBody(reason string) wire.AuditReadRequest {
	return wire.AuditReadRequest{ProtocolVersion: wire.ProtocolVersion, Reason: reason}
}

const auditPath = "/assistant/audit/conversations/018f0000-0000-7000-8000-000000000001/read"

// A COMPLIANCE READ IS AUDITED BEFORE IT OPENS (D06): the read is recorded in
// the conversation's own domain with the reader and the reason, which is what
// its owner is shown; the conversation opens only for that record; and the
// runtime audit names the reader, the owner and the reason before the
// conversation is answered. Mutation: open before recording and the order
// fails.
func TestAComplianceReadIsAuditedAndShownToTheOwner(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	// PROVED ON THE WALL CLOCK, which is the clock the authority table
	// judges a route's step-up on.
	grace := admin(time.Now().Add(time.Hour))
	status, body := s.send(t, grace, http.MethodPost, auditPath, readBody("  Legal hold 42 "), nil)
	if status != http.StatusOK {
		t.Fatalf("the read answered %d %v", status, body)
	}
	if got := strings.Join(s.audits.steps, ","); got != "record,open,render" {
		t.Errorf("the read went %s, want record,open,render", got)
	}
	if s.audits.reason != "Legal hold 42" || s.audits.reader.Name != "grace" {
		t.Errorf("the record names %q for %q", s.audits.reader.Name, s.audits.reason)
	}
	if len(s.audit.events) != 1 {
		t.Fatalf("%d runtime audit records, want one", len(s.audit.events))
	}
	ev := s.audit.events[0]
	read, ok := ev.Data.(*types.AssistantConversationRead)
	if !ok || ev.Source != types.OperatorSource || read.Reason != "Legal hold 42" ||
		read.OwnerID != s.audits.owner.String() || read.ActorName != "grace" ||
		read.ConversationID == "" {
		t.Errorf("the runtime audit is %s %+v", ev.Source, ev.Data)
	}

	// A RECORD NOT YET APPLIED HERE OPENS NOTHING, and leaves no runtime
	// record of a read that did not happen.
	s2 := newSurface(t)
	s2.audits.unopened = true
	if status, body := s2.send(t, grace, http.MethodPost, auditPath, readBody("Legal hold"), nil); status != http.StatusServiceUnavailable || body["op_id"] == nil {
		t.Errorf("a read whose record has not applied answered %d %v", status, body)
	}
	if len(s2.audit.events) != 0 {
		t.Error("a read that never opened was recorded as one")
	}
}

// A COMPLIANCE READ NEEDS A REASON, and only an administrator who has
// recently confirmed it is them may make one.
func TestAComplianceReadNeedsAReason(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	fresh := time.Now().Add(time.Hour)
	for _, reason := range []string{"", "   ", strings.Repeat("x", wire.AuditReasonMaxRunes+1)} {
		if status, body := s.send(t, admin(fresh), http.MethodPost, auditPath, readBody(reason), nil); status != http.StatusBadRequest || body["error"] != "invalid_body" {
			t.Errorf("a read with reason %q answered %d %v", reason, status, body)
		}
	}
	if status, body := s.send(t, approvaltest.Person(fresh), http.MethodPost, auditPath,
		readBody("Legal hold"), nil); status != http.StatusForbidden || body["error"] != "unauthorized" {
		t.Errorf("a person who is not an administrator answered %d %v", status, body)
	}
	if status, body := s.send(t, admin(time.Now().Add(-time.Minute)), http.MethodPost, auditPath,
		readBody("Legal hold"), nil); status != http.StatusForbidden || body["error"] != "step_up_required" {
		t.Errorf("an administrator who has not confirmed it is them answered %d %v", status, body)
	}
	missing := newSurface(t)
	missing.audits.missing = true
	if status, _ := missing.send(t, admin(fresh), http.MethodPost, auditPath, readBody("Legal hold"), nil); status != http.StatusNotFound {
		t.Errorf("a read of a conversation nobody holds answered %d", status)
	}
	if len(s.audits.steps) != 0 || len(s.audit.events) != 0 {
		t.Errorf("refused reads left %v and %d records", s.audits.steps, len(s.audit.events))
	}
}

// NEW REFUSES A SURFACE WITH A MISSING DEPENDENCY, by name.
func TestTheDecisionRoutesRefuseAMissingDependency(t *testing.T) {
	t.Parallel()
	if _, err := assistantapi.NewDecisions(assistantapi.DecisionsDeps{}); err == nil {
		t.Error("NewDecisions built routes over nothing")
	}
}

// A DECLINE IS ONE OPERATION, as an approval is: the route refuses a decline
// that names none, the same decline sent again under its key answers the state
// it already left the proposal in, and a second decline under another key is
// told the proposal is decided. Mutation: drop opkey.Require from decision and
// the first case answers 200.
func TestADeclineIsOneOperation(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	ada := approvaltest.Person(approvaltest.Noon.Add(time.Hour))
	p := s.fleet.Propose(t, ada, nil)
	path := "/assistant/proposals/" + p.ID.String() + "/decline"

	status, body := s.send(t, ada, http.MethodPost, path, decide(p.Digest),
		map[string]string{opkey.Header: ""})
	if status != http.StatusBadRequest || body["error"] != "op_id_invalid" {
		t.Fatalf("a decline with no operation answered %d %v", status, body)
	}
	key := map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()}
	for range 2 {
		status, body = s.send(t, ada, http.MethodPost, path, decide(p.Digest), key)
		ref, _ := body["proposal"].(map[string]any)
		if status != http.StatusOK || ref["state"] != string(wire.ProposalDeclined) {
			t.Fatalf("the decline under one operation answered %d %v", status, body)
		}
	}
	status, body = s.send(t, ada, http.MethodPost, path, decide(p.Digest), nil)
	if status != http.StatusConflict || body["error"] != "proposal_already_decided" {
		t.Fatalf("a second decline answered %d %v", status, body)
	}
}

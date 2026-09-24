package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// recordedAudit is every event an app published as a runtime audit record,
// with the topic each went to.
type recordedAudit struct {
	mu     sync.Mutex
	topics []string
	events []*events.Event
}

func (r *recordedAudit) Publish(_ context.Context, topic string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics = append(r.topics, topic)
	r.events = append(r.events, ev)
	return nil
}

func (r *recordedAudit) take() ([]string, []*events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, e := r.topics, r.events
	r.topics, r.events = nil, nil
	return t, e
}

// auditWork is a tracker writer that lands every create at one position.
type auditWork struct{}

func (auditWork) CreateTask(context.Context, string, tracker.Task,
	*tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{
		Result: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 7},
			Version:  7,
		},
		Key: "ENG-1",
	}, nil
}

func (auditWork) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, tracker.ChangeKind, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

// auditedCompany binds the token `founder` to Jane Founder and leaves `ci`
// bound to nobody.
func auditedCompany() func() *config.Company {
	company := &config.Company{
		Name: "Nimbus",
		Roles: []config.Role{
			{Name: "Jane Founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
			{Name: "CTO"},
		},
	}
	return func() *config.Company { return company }
}

// auditedApp is an app serving the act transport and the backup route, both
// auditing into one record.
func auditedApp(t *testing.T, taker *fakeBackup) (*api.App, *recordedAudit) {
	t.Helper()
	audit := &recordedAudit{}
	company := auditedCompany()
	chart := func() *org.Organization {
		o, err := company().Organization()
		if err != nil {
			t.Fatalf("the test company does not derive: %v", err)
		}
		return o
	}
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: "founder-secret"}, {ID: "ci", Token: "ci-secret"},
	}
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: company, NodeID: "node-a"},
		Operator: operatorSurface(t, operator.Options{
			Work: builtin.WorkDeps{
				Reader: stubWorkReader{},
				Writer: func(builtin.Actor) builtin.WorkWriter { return auditWork{} },
				Actor:  operator.WorkActor(chart),
			},
			Org:   chart,
			Audit: audit,
		}),
		Backup: taker,
		Audit:  audit,
	})
	return a, audit
}

// actRequest posts one act request and returns its status.
func actRequest(t *testing.T, a *api.App, token, tool, args string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tool,
		strings.NewReader(`{"request_id":"0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c","args":`+args+`}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	return rec.Code
}

// EVERY RUNTIME WRITE LEAVES EXACTLY ONE AUDIT RECORD, and a request that
// never reached a write leaves none.
//
// The table is the runtime writes this build serves over HTTP outside the
// configuration and credential surfaces — every act tool call, whatever
// became of it, and every backup, whatever became of it — each driven through
// the real app behind the real guard. Each record must be persisted (a
// category, so the event store writes it), carry the operator source every
// audit reader selects on, name the credential as the actor and the person it
// is bound to, and say what became of the call. The refusals before a call
// (a read on the write transport, a token that is nobody, a backup with no
// destination) are the counterfactual: an audit that recorded every request
// would pass the rest of the table.
func TestEachRuntimeWriteEmitsOneAuditedEvent(t *testing.T) {
	t.Parallel()
	type want struct {
		eventType string
		outcome   types.AuditOutcome
		failed    bool
		fields    map[string]any
	}
	cases := []struct {
		name   string
		taker  *fakeBackup
		run    func(*testing.T, *api.App) int
		status int
		want   *want
	}{{
		name: "an act that lands",
		run: func(t *testing.T, a *api.App) int {
			return actRequest(t, a, "founder-secret", tracker.CreateWorkItemTool,
				`{"title":"Rotate the signing key","project":"ENG"}`)
		},
		status: http.StatusOK,
		want: &want{eventType: "operator_acted", outcome: types.AuditApplied, fields: map[string]any{
			"transport": types.TransportAct, "tool": tracker.CreateWorkItemTool,
			"request_id": "0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c",
			"position":   "CREWLET_TRACKER_LOG@1:7",
		}},
	}, {
		name: "an act the tool refuses",
		run: func(t *testing.T, a *api.App) int {
			return actRequest(t, a, "founder-secret", tracker.CreateWorkItemTool, `{"project":"ENG"}`)
		},
		status: http.StatusUnprocessableEntity,
		want: &want{eventType: "operator_acted", outcome: types.AuditRefused, failed: true,
			fields: map[string]any{"refusal": "invalid", "tool": tracker.CreateWorkItemTool}},
	}, {
		name: "a read refused on the write transport",
		run: func(t *testing.T, a *api.App) int {
			return actRequest(t, a, "founder-secret", tracker.ListWorkItemsTool, `{}`)
		},
		status: http.StatusBadRequest,
	}, {
		name: "an act by a token that is nobody",
		run: func(t *testing.T, a *api.App) int {
			return actRequest(t, a, "ci-secret", tracker.CreateWorkItemTool,
				`{"title":"Rotate the signing key","project":"ENG"}`)
		},
		status: http.StatusForbidden,
	}, {
		name:  "a backup that is taken",
		taker: &fakeBackup{took: backup.Manifest{Streams: make([]backup.StreamArtifact, 3)}},
		run: func(t *testing.T, a *api.App) int {
			status, _ := post(t, a, "/backup?dir=/var/backups/one", "founder-secret")
			return status
		},
		status: http.StatusOK,
		want: &want{eventType: "backup_requested", outcome: types.AuditApplied, fields: map[string]any{
			"dir": "/var/backups/one", "streams": float64(3),
		}},
	}, {
		name:  "a backup that fails",
		taker: &fakeBackup{err: errors.New("disk full")},
		run: func(t *testing.T, a *api.App) int {
			status, _ := post(t, a, "/backup?dir=/var/backups/two", "founder-secret")
			return status
		},
		status: http.StatusInternalServerError,
		want: &want{eventType: "backup_requested", outcome: types.AuditFailed, failed: true,
			fields: map[string]any{"dir": "/var/backups/two"}},
	}, {
		name: "a backup that names no destination",
		run: func(t *testing.T, a *api.App) int {
			status, _ := post(t, a, "/backup", "founder-secret")
			return status
		},
		status: http.StatusBadRequest,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			taker := tc.taker
			if taker == nil {
				taker = &fakeBackup{}
			}
			a, audit := auditedApp(t, taker)
			if status := tc.run(t, a); status != tc.status {
				t.Fatalf("answered %d, want %d", status, tc.status)
			}
			published, recorded := audit.take()
			if tc.want == nil {
				if len(recorded) != 0 {
					t.Fatalf("a request that reached no write left %d audit records: %s",
						len(recorded), recorded[0].Type)
				}
				return
			}
			if len(recorded) != 1 {
				t.Fatalf("one runtime write left %d audit records, want exactly one", len(recorded))
			}
			ev := recorded[0]
			if ev.Type != tc.want.eventType || published[0] != topics.Event(ev.Type) {
				t.Errorf("the record is a %s on %s, want a %s on its own event subject",
					ev.Type, published[0], tc.want.eventType)
			}
			if ev.Source != types.OperatorSource {
				t.Errorf("the envelope's source is %q, want %q — the value every audit "+
					"reader selects runtime writes by", ev.Source, types.OperatorSource)
			}
			if ev.Actor() != "founder" {
				t.Errorf("the actor is %q, want the credential that made the call", ev.Actor())
			}
			rec, persisted := observe.Record(ev)
			if !persisted {
				t.Fatalf("a %s is not written to the event store, so the audit it is "+
					"for never has a row", ev.Type)
			}
			if rec.Actor != "founder" || rec.Source != types.OperatorSource {
				t.Errorf("the stored row names actor %q and source %q", rec.Actor, rec.Source)
			}
			if (rec.Tags["failed"] == "true") != tc.want.failed {
				t.Errorf("the row's failed tag is %q, want failed=%v", rec.Tags["failed"], tc.want.failed)
			}
			body := decodeFlat(t, ev)
			if body["operator_id"] != "founder" || body["actor_seat"] != "jane-founder" {
				t.Errorf("the record names operator %v and seat %v, want the token and "+
					"the person it is bound to", body["operator_id"], body["actor_seat"])
			}
			if body["outcome"] != string(tc.want.outcome) {
				t.Errorf("the outcome is %v, want %s", body["outcome"], tc.want.outcome)
			}
			for key, value := range tc.want.fields {
				if body[key] != value {
					t.Errorf("%s = %v, want %v", key, body[key], value)
				}
			}
			// THE NODE IS THE QUEUE'S TO STAMP, on every record alike. The
			// route publishes through its own node's queue, which names the
			// origin on the way out; a record that arrived here already naming
			// one is a publisher claiming the envelope's fact for itself.
			if node, named := body["node"]; named {
				t.Errorf("the record names node %v before any queue saw it — the "+
					"origin is stamped by the queue it is published through", node)
			}
			if _, leaked := body["args"]; leaked {
				t.Error("the record carries the call's arguments, which are the company's " +
					"content and already live in the history of what they changed")
			}
		})
	}
}

// decodeFlat is an event as the one flat object the wire and the store carry.
func decodeFlat(t *testing.T, ev *events.Event) map[string]any {
	t.Helper()
	raw, err := ev.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

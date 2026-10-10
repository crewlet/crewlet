package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Every catalogue tool is served at a reach (ADR-0031), and the one dispatch
// both transports share refuses a caller below it. These cases hold the three
// places that reach shows — what a press answers, what an assistant is listed,
// and what `viewer.acts` offers — to the same table, and the audit to the role
// the refused key held.

// twoPeopleChart binds the member key `founder` to Jane Founder and the admin
// key `cofounder` to Ada Cofounder, so each role has a person to act as.
func twoPeopleChart() *org.Organization {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Jane Founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
			{Name: "Ada Cofounder", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "cofounder"}},
		},
	}
	o.Normalize()
	return o
}

// recordingCatalogue is the company's catalogue writer, recording every
// declaration it is asked to make.
type recordingCatalogue struct {
	mu     sync.Mutex
	writes []builtin.Actor
}

type recordingCatalogueWriter struct {
	c     *recordingCatalogue
	actor builtin.Actor
}

func (c *recordingCatalogue) writer(actor builtin.Actor) builtin.CatalogueWriter {
	return recordingCatalogueWriter{c: c, actor: actor}
}

func (w recordingCatalogueWriter) WriteTypes(context.Context, string,
	[]tracker.TaskType) (tracker.WriteResult, error) {

	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	w.c.writes = append(w.c.writes, w.actor)
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (w recordingCatalogueWriter) WriteFields(ctx context.Context, opID string,
	_ []tracker.FieldDef) (tracker.WriteResult, error) {

	return w.WriteTypes(ctx, opID, nil)
}

func (c *recordingCatalogue) made() []builtin.Actor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]builtin.Actor(nil), c.writes...)
}

// reachSurface is the operator surface with the company's catalogue writer on
// it — the one tool served to an admin's key alone — auditing into a log.
func reachSurface(t *testing.T) (*operator.Server, *recordingCatalogue, *auditLog) {
	t.Helper()
	catalogue := &recordingCatalogue{}
	audit := &auditLog{}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: stubWorkWriter,
			CatalogueWriter: catalogue.writer,
			Actor:           operator.WorkActor(twoPeopleChart, nil),
		},
		Org:   twoPeopleChart,
		Audit: audit,
	})
	return s, catalogue, audit
}

// catalogueArgs declares one task type, which is a whole call.
func catalogueArgs() map[string]any {
	return map[string]any{"types": []any{map[string]any{"slug": "incident", "name": "Incident"}}}
}

func catalogueBody(requestID string) string {
	raw, _ := json.Marshal(map[string]any{"request_id": requestID, "args": catalogueArgs()})
	return string(raw)
}

// A MEMBER'S PRESS ON AN ADMIN'S TOOL IS REFUSED `forbidden` NAMING WHO MAY,
// before the tool runs, and the refusal is AUDITED with the role the key held
// — the record that says a member reached past their role, rather than a
// write that silently did not happen. The admin's press on the same tool is
// made, so a dispatch that refused everybody fails here too.
func TestAToolPastTheCallersReachIsRefusedAndAudited(t *testing.T) {
	t.Parallel()
	s, catalogue, audit := reachSurface(t)
	h := guarded(s, false)

	status, answer := act(t, h, "founder-secret", tracker.WriteWorkCatalogueTool,
		"application/json", catalogueBody(requestA))
	if status != http.StatusForbidden || answer["error"] != string(tools.RefusalForbidden) {
		t.Fatalf("a member's press answered %d %v, want 403 forbidden", status, answer)
	}
	if detail, _ := answer["detail"].(string); !strings.HasPrefix(detail, "Only an admin can") {
		t.Errorf("the refusal says %q — it must name who may, or the person asks "+
			"the wrong somebody next", detail)
	}
	if made := catalogue.made(); len(made) != 0 {
		t.Fatalf("a member's press reached the catalogue writer as %+v", made)
	}

	status, answer = act(t, h, "cofounder-secret", tracker.WriteWorkCatalogueTool,
		"application/json", catalogueBody(requestB))
	if status != http.StatusOK {
		t.Fatalf("an admin's press answered %d %v, want it made", status, answer)
	}
	if made := catalogue.made(); len(made) != 1 || made[0].Seat != "ada-cofounder" {
		t.Errorf("the admin's press wrote as %+v, want one write as the person", made)
	}

	recorded := audit.published()
	if len(recorded) != 2 {
		t.Fatalf("two presses left %d audit records, want both", len(recorded))
	}
	refused, made := payloadOf(t, recorded[0]), payloadOf(t, recorded[1])
	if refused.OperatorID != "founder" || refused.KeyRole != config.RoleMember ||
		refused.Outcome != types.AuditRefused || refused.Refusal != string(tools.RefusalForbidden) {

		t.Errorf("the refused press is recorded as %+v, want the member's key, its "+
			"role and `refused`/`forbidden`", refused)
	}
	if made.OperatorID != "cofounder" || made.KeyRole != config.RoleAdmin ||
		made.Outcome != types.AuditApplied {

		t.Errorf("the made press is recorded as %+v, want the admin's key, its role "+
			"and `applied`", made)
	}
}

// AN ASSISTANT IS LISTED WHAT ITS KEY MAY CALL, and the dispatch refuses the
// rest whatever the listing said. The listing is checked through a real MCP
// client, because what a client receives is the SDK's serialisation of what
// was registered; the refusal through the dispatch itself, because a session
// opened under one key and carried on under another would otherwise call a
// tool it was never listed.
func TestAnAssistantIsListedOnlyWhatItsKeyMayCall(t *testing.T) {
	t.Parallel()
	s, catalogue, _ := reachSurface(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	listedAs := func(sess *mcp.ClientSession) []string {
		listed, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		var names []string
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	memberSession := dialOperator(t, s, asMember("founder"))
	member := listedAs(memberSession)
	admin := listedAs(dialOperator(t, s, asAdmin("cofounder")))
	if slices.Contains(member, tracker.WriteWorkCatalogueTool) {
		t.Errorf("a member's assistant is listed write_work_catalogue, which "+
			"fails on every call it makes: %v", member)
	}
	if !slices.Contains(admin, tracker.WriteWorkCatalogueTool) {
		t.Errorf("an admin's assistant is not listed write_work_catalogue: %v", admin)
	}
	for _, name := range member {
		if !slices.Contains(admin, name) {
			t.Errorf("a member is listed %q and an admin is not", name)
		}
	}
	if len(admin) != len(member)+1 {
		t.Errorf("an admin is listed %d tools and a member %d; exactly one is "+
			"served to an admin alone", len(admin), len(member))
	}

	// A CALL BY NAME FROM THE MEMBER'S SESSION is not made either, whatever
	// the SDK answers an unlisted name with.
	if res, err := memberSession.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.WriteWorkCatalogueTool, Arguments: catalogueArgs(),
	}); err == nil && !res.IsError {
		t.Errorf("a member's assistant called write_work_catalogue: %s", textOf(res))
	}

	got, served, err := s.Dispatch(auth.WithPrincipal(ctx, asMember("founder")),
		tracker.WriteWorkCatalogueTool, catalogueArgs())
	if err != nil || !served || !got.Failed || got.Refusal != tools.RefusalForbidden {
		t.Errorf("the dispatch answered a member's call %+v (served=%v err=%v), "+
			"want it refused forbidden", got, served, err)
	}
	if made := catalogue.made(); len(made) != 0 {
		t.Errorf("a member's call reached the catalogue writer as %+v", made)
	}
}

// THE ACTOR SAYS WHETHER ITS KEY IS AN ADMIN'S, from the principal the guard
// resolved — the one authority a tool grants past the chart (pause any seat,
// steer any turn, answer any parked run). A member's key, linked or not, is
// not one; a disabled guard's caller is, as the guard makes every caller.
func TestTheWorkActorSaysWhetherItsKeyIsAnAdmins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		caller auth.Principal
		admin  bool
	}{
		{"a linked member key", asMember("founder"), false},
		{"a linked admin key", asAdmin("cofounder"), true},
		{"an unlinked admin key", asAdmin("ops-bot"), true},
		{"a disabled guard's caller", auth.Principal{ID: auth.AnonymousOperator,
			Role: config.RoleAdmin, Reach: auth.ReachAdmin}, true},
	} {
		actor, err := operator.WorkActor(twoPeopleChart, nil)(
			auth.WithPrincipal(t.Context(), tc.caller), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if actor.Admin != tc.admin {
			t.Errorf("%s resolved Admin=%v, want %v", tc.name, actor.Admin, tc.admin)
		}
	}
}

// A PERSON'S PRESS ON A SEAT THEY DO NOT LEAD IS REFUSED NAMING WHO MAY, at the
// act transport as the tool refused it: 403 `forbidden`, the tool's own
// sentence as `detail` — which the dashboard draws as it stands — and the
// refusal audited. The same press by an admin's key is made.
func TestAPressOnASeatThePersonDoesNotLeadIsRefusedNamingWhoMay(t *testing.T) {
	t.Parallel()
	chart := func() *org.Organization {
		o := &org.Organization{
			Name: "Nimbus",
			Roles: []*org.Role{
				{Name: "Jane Founder", Kind: org.KindHuman, Manages: []string{"Agent CTO"},
					Contact: &org.HumanContact{CrewletOperatorID: "cofounder"}},
				{Name: "Rui Santos", Kind: org.KindHuman,
					Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
				{Name: "Agent CTO"},
			},
		}
		o.Normalize()
		return o
	}
	audit := &auditLog{}
	s := newSurface(t, operator.Options{
		Pauses: builtin.SeatPauseDeps{
			Pauses: coordmem.NewFleet(), Announce: &auditLog{}, Org: chart,
			Actor: operator.WorkActor(chart, nil),
		},
		Org: chart, Audit: audit,
	})
	h := guarded(s, false)
	pause := func(requestID string) string {
		raw, _ := json.Marshal(map[string]any{"request_id": requestID,
			"args": map[string]any{"handle": "agent-cto"}})
		return string(raw)
	}

	// Rui's key is a member's, linked to a seat that leads nobody.
	status, answer := act(t, h, "founder-secret", builtin.PauseSeatTool,
		"application/json", pause(requestA))
	if status != http.StatusForbidden || answer["error"] != string(tools.RefusalForbidden) {
		t.Fatalf("a member's pause of a seat they do not lead answered %d %v, want 403 forbidden",
			status, answer)
	}
	if detail, _ := answer["detail"].(string); !strings.HasPrefix(detail,
		"Only Jane Founder or an admin can pause Agent CTO") {
		t.Errorf("the refusal says %q, want it to name who may", detail)
	}
	if recorded := audit.published(); len(recorded) != 1 ||
		payloadOf(t, recorded[0]).Refusal != string(tools.RefusalForbidden) {
		t.Errorf("the refused pause was not audited as refused forbidden: %d records", len(recorded))
	}

	// Jane's key is an admin's AND she leads the seat: either would do.
	if status, answer := act(t, h, "cofounder-secret", builtin.PauseSeatTool,
		"application/json", pause(requestB)); status != http.StatusOK {
		t.Fatalf("Jane's pause answered %d %v, want it made", status, answer)
	}
}

// WHAT A PERSON IS OFFERED TO PRESS IS WHAT THEIR REACH IS SERVED, so a member
// is never shown a control the dispatch refuses on every press — and a reach
// below member, which reaches neither transport, is offered nothing.
func TestActsAnswersEachReachWhatItMayCall(t *testing.T) {
	t.Parallel()
	s := fullSurface(t)
	member, admin := s.Acts(auth.ReachMember), s.Acts(auth.ReachAdmin)
	if slices.Contains(member, tracker.WriteWorkCatalogueTool) {
		t.Errorf("a member is offered write_work_catalogue: %v", member)
	}
	if !slices.Contains(admin, tracker.WriteWorkCatalogueTool) {
		t.Errorf("an admin is not offered write_work_catalogue: %v", admin)
	}
	if !slices.Contains(member, tracker.CreateWorkItemTool) {
		t.Errorf("a member is not offered the create every person files with: %v", member)
	}
	for _, name := range member {
		if !slices.Contains(admin, name) {
			t.Errorf("a member is offered %q and an admin is not", name)
		}
	}
	for _, reach := range []auth.Reach{auth.ReachOpen, auth.ReachPublic, ""} {
		if got := s.Acts(reach); len(got) != 0 {
			t.Errorf("a caller at reach %q is offered %v", reach, got)
		}
	}
}

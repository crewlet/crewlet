package alarmwake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// grantedCompany is a company of two agent seats whose roles carry the health
// grant, one whose role does not, and a person.
const grantedCompany = `
name: Nimbus
roles:
  - name: SRE
    handle: sre
    health_tools: true
  - name: Ops
    handle: ops
    health_tools: true
  - name: Dev
    handle: dev
  - name: Founder
    handle: founder
    kind: human
`

// companyOf is doc's organization.
func companyOf(t *testing.T, doc string) *org.Organization {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return o
}

// transitions observes alarms on a tracker of node node, in turn, and answers
// every transition it handed on.
func transitions(t *testing.T, node string, clock *time.Time, rounds ...[]statelog.Alarm) []statelog.AlarmTransition {
	t.Helper()
	tracker := statelog.NewTracker(nil, func() time.Time { return *clock })
	var out []statelog.AlarmTransition
	tracker.OnTransition(node, func(tr statelog.AlarmTransition) { out = append(out, tr) })
	for _, alarms := range rounds {
		tracker.Observe(context.Background(), alarms)
		*clock = clock.Add(statelog.AlarmInterval)
	}
	return out
}

// parse is what p routes w to against reg, failing the case on an error.
func parse(t *testing.T, p *alarmwake.Parser, tr statelog.AlarmTransition, reg *notify.Registry) []notify.Routed {
	t.Helper()
	w, err := alarmwake.Encode(tr)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	routed, err := p.Parse(context.Background(), w, reg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return routed
}

func handlesOf(routed []notify.Routed) []string {
	var out []string
	for _, r := range routed {
		out = append(out, r.To.Handle)
	}
	return out
}

func idsOf(routed []notify.Routed) []uuid.UUID {
	var out []uuid.UUID
	for _, r := range routed {
		out = append(out, r.WakeID)
	}
	return out
}

// AN ALARM TRANSITION WAKES EACH GRANTED SEAT ONCE.
//
// One transition, as a real tracker hands it on, reaches every agent seat
// whose role carries the health grant — once each, however the grant list
// names them — and no seat whose role does not, and no person. A REPUBLISH,
// the same transition encoded and parsed again as the outbox's retry or a
// second node's delivery would, routes to the very same wake ids, which is
// what the inbox's same-id dedupe and the completion ledger collapse; the
// clear that follows is a transition of its own and wakes under other ids.
// Each wake is dated by the transition, never by its delivery, and names the
// alarm instance as what starts its turn — the fire and its clear one
// instance, each under what it did. A granted seat the company no longer
// employs is not woken.
//
// Mutation: a random wake id, or one derived from the delivery rather than
// the transition, and the republish's ids differ; routing every seat rather
// than the granted ones, and the ungranted seat is woken; name no origin, or
// the transition rather than the instance, and the fire's and the clear's
// origins are wrong.
func TestAnAlarmTransitionWakesEachGrantedSeatOnce(t *testing.T) {
	t.Parallel()
	o := companyOf(t, grantedCompany)
	granted := orgchart.GrantedSeats(o, authz.SeatGrantHealth)
	if !slices.Equal(granted, []string{"sre", "ops"}) {
		t.Fatalf("the premise: the granted seats are %v, want sre and ops", granted)
	}
	reg := notify.NewRegistry(o, nil)
	// THE LIST NAMES A SEAT TWICE, and it is still woken once: one reason
	// a handle, routing's add-once rule.
	p := alarmwake.NewParser(alarmwake.ParserOptions{Granted: append(granted, "sre")})

	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	lag := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 4m12s behind the log"}
	seen := transitions(t, "node-a", &clock, nil, []statelog.Alarm{lag}, nil)
	if len(seen) != 2 || seen[0].Change != statelog.TransitionFired ||
		seen[1].Change != statelog.TransitionCleared {
		t.Fatalf("the premise: the tracker handed on %+v, want a fire and its clear", seen)
	}
	fired, cleared := seen[0], seen[1]

	first := parse(t, p, fired, reg)
	if got := handlesOf(first); !slices.Equal(got, []string{"sre", "ops"}) {
		t.Errorf("the fire woke %v, want each granted seat once: sre, ops", got)
	}
	for _, r := range first {
		if r.WakeID == uuid.Nil {
			t.Errorf("%s's wake carries no derived id", r.To.Handle)
		}
		if !r.WakeAt.Equal(fired.At) {
			t.Errorf("%s's wake is dated %v, want the transition's %v", r.To.Handle, r.WakeAt, fired.At)
		}
		if r.Source != alarmwake.Source || r.Metadata[alarmwake.TransitionField] != fired.ID {
			t.Errorf("%s's wake is %s about %q, want %s about %s", r.To.Handle, r.Source,
				r.Metadata[alarmwake.TransitionField], alarmwake.Source, fired.ID)
		}
		if want := (types.TriggerOrigin{Kind: types.OriginAlarm, ID: string(statelog.KindApplyLag),
			Reason: string(statelog.TransitionFired)}); r.Origin != want {
			t.Errorf("%s's wake names %+v as what starts its turn, want %+v", r.To.Handle, r.Origin, want)
		}
	}
	for _, r := range parse(t, p, cleared, reg) {
		if want := (types.TriggerOrigin{Kind: types.OriginAlarm, ID: string(statelog.KindApplyLag),
			Reason: string(statelog.TransitionCleared)}); r.Origin != want {
			t.Errorf("%s's wake of the clear names %+v as what starts its turn, want %+v: the fire's "+
				"instance, cleared", r.To.Handle, r.Origin, want)
		}
	}
	if ids := idsOf(first); ids[0] == ids[1] {
		t.Errorf("two seats share the wake id %s: the second would be deduplicated away", ids[0])
	}

	again := parse(t, p, fired, reg)
	if !slices.Equal(idsOf(again), idsOf(first)) {
		t.Errorf("the republished fire routes to %v, want the first delivery's %v — a copy "+
			"that does not collapse wakes every seat twice", idsOf(again), idsOf(first))
	}
	for _, id := range idsOf(parse(t, p, cleared, reg)) {
		if slices.Contains(idsOf(first), id) {
			t.Errorf("the clear wakes under the fire's id %s, so it would be deduplicated away", id)
		}
	}

	// A GRANTED SEAT THE COMPANY NO LONGER EMPLOYS: the roster of a later
	// epoch without `ops`, against a parser still holding the old list.
	later := notify.NewRegistry(companyOf(t, strings.Replace(grantedCompany,
		"  - name: Ops\n    handle: ops\n    health_tools: true\n", "", 1)), nil)
	if got := handlesOf(parse(t, p, fired, later)); !slices.Equal(got, []string{"sre"}) {
		t.Errorf("against a roster without ops the fire woke %v, want sre alone", got)
	}
	// AND A COMPANY THAT GRANTED NOBODY wakes nobody.
	if got := parse(t, alarmwake.NewParser(alarmwake.ParserOptions{}), fired, reg); len(got) != 0 {
		t.Errorf("a company granting nobody woke %v", handlesOf(got))
	}
}

// AN ALARM WAKE SENDS ITS SEAT TO THE FLEET'S CURRENT STATE.
//
// Every copy names the alarm, the node, the log a per-log alarm stands on and
// the measurement, says what the alarm means in the table's own words, gives
// the remedy while it stands, and sends the seat to fleet_health, asking for
// the nodes with an alarm firing, before it acts: a fire says when it fired and
// a clear how long it stood and no remedy. Nobody is waiting on an answer, and the trigger
// is a pointer. One alarm is one conversation, on every node and log it stands
// on; another alarm is another.
//
// Mutation: drop the fleet_health pointer or its `firing`, or the remedy, or
// key the conversation by node, and a case is red.
func TestAnAlarmWakeSendsItsSeatToTheFleetsCurrentState(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	trim := statelog.Alarm{Kind: statelog.KindTrimBlocked, Detail: "the tracker log's trim is held",
		Domains: []statelog.AlarmDomain{{Domains: []string{"tracker"}, Detail: "held 3h"},
			{Domains: []string{"pages"}, Detail: "held 2h"}}}
	seen := transitions(t, "node-b", &clock, []statelog.Alarm{trim}, nil, []statelog.Alarm{trim})
	byChange := map[statelog.TransitionChange]statelog.AlarmTransition{}
	for _, tr := range seen {
		if tr.Domain == "tracker" {
			byChange[tr.Change] = tr
		}
	}
	meaning, remedy, ok := statelog.Describe(statelog.KindTrimBlocked)
	if !ok {
		t.Fatal("the premise: the table describes no trim_blocked")
	}
	p := alarmwake.NewParser(alarmwake.ParserOptions{Granted: []string{"sre"}})
	prompt := alarmwake.Prompt{}
	for change, want := range map[statelog.TransitionChange][]string{
		statelog.TransitionCleared: {"it cleared", "having stood since"},
		statelog.TransitionFired:   {"it fired", remedy},
	} {
		tr, ok := byChange[change]
		if !ok {
			t.Fatalf("the premise: the tracker handed on no %s for the tracker log: %+v", change, seen)
		}
		routed := parse(t, p, tr, nil)
		if len(routed) != 1 {
			t.Fatalf("%s routed %d copies, want 1", change, len(routed))
		}
		n := routed[0].Inbound
		body := prompt.Build(n, nil)
		for _, part := range append(want, "`trim_blocked`", "`node-b`", "`tracker` log", "held 3h",
			meaning, "`fleet_health` with `firing: true`") {
			if !strings.Contains(body, part) {
				t.Errorf("the %s prompt does not say %q:\n%s", change, part, body)
			}
		}
		if change == statelog.TransitionCleared && strings.Contains(body, remedy) {
			t.Errorf("the clear's prompt still asks for the remedy:\n%s", body)
		}
		if prompt.Addressed(n) || !prompt.RequiresRecon(n) || prompt.SubjectIsLabel(n) {
			t.Errorf("%s: addressed %v, recon %v, label %v — want an unaddressed pointer whose "+
				"subject is content", change, prompt.Addressed(n), prompt.RequiresRecon(n),
				prompt.SubjectIsLabel(n))
		}
	}

	key := func(node, kind, domain string) string {
		return prompt.PartitionKey(map[string]string{alarmwake.NodeField: node,
			alarmwake.AlarmField: kind, alarmwake.DomainField: domain}, "")
	}
	if key("node-b", "trim_blocked", "tracker") != key("node-b", "trim_blocked", "pages") {
		t.Error("one alarm on one node is two conversations, one per log it stands on")
	}
	if key("node-b", "trim_blocked", "tracker") != key("node-c", "trim_blocked", "tracker") {
		t.Error("one alarm on two nodes is two conversations, a turn per node for one read")
	}
	if key("node-b", "trim_blocked", "") == key("node-b", "apply_lag", "") {
		t.Error("two alarms on one node are one conversation")
	}
	if got := prompt.PartitionKey(map[string]string{alarmwake.NodeField: "node-b"}, "x"); got != "" {
		t.Errorf("a copy naming no alarm partitions as %q, want never merged", got)
	}
}

// A DELIVERY THAT IS NO TRANSITION WAKES NOBODY: one with no id cannot be told
// apart from its own republish, and one with no node, alarm or change names
// nothing a seat could look up, so each is refused as a parse failure, which
// the spine records. A change this build does not know is a newer node's word
// for what its alarm did and is read, and the parsed body serves where a
// delivery carries no bytes.
func TestADeliveryThatIsNoTransitionWakesNobody(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	whole := statelog.AlarmTransition{ID: "t-1", Node: "node-a", Kind: statelog.KindApplyLag,
		Change: statelog.TransitionFired, Since: at, At: at}
	p := alarmwake.NewParser(alarmwake.ParserOptions{Granted: []string{"sre"}})
	for name, broken := range map[string]func(*statelog.AlarmTransition){
		"no id":     func(tr *statelog.AlarmTransition) { tr.ID = "" },
		"no node":   func(tr *statelog.AlarmTransition) { tr.Node = "" },
		"no alarm":  func(tr *statelog.AlarmTransition) { tr.Kind = "" },
		"no change": func(tr *statelog.AlarmTransition) { tr.Change = "" },
	} {
		tr := whole
		broken(&tr)
		if _, err := alarmwake.Encode(tr); !errors.Is(err, alarmwake.ErrNotATransition) {
			t.Errorf("%s: Encode = %v, want ErrNotATransition", name, err)
		}
	}
	// ON THE WIRE TOO: a delivery whose id was emptied after it was encoded
	// is refused by the parser, never routed.
	w, err := alarmwake.Encode(whole)
	if err != nil {
		t.Fatal(err)
	}
	w.BodyRaw = []byte(strings.Replace(string(w.BodyRaw), `"`+whole.ID+`"`, `""`, 1))
	if routed, err := p.Parse(context.Background(), w, nil); !errors.Is(err, alarmwake.ErrNotATransition) {
		t.Errorf("a delivery with no id parsed as %v (%v), want ErrNotATransition", routed, err)
	}

	newer := whole
	newer.Change = "escalated"
	routed := parse(t, p, newer, nil)
	if len(routed) != 1 || !strings.Contains(routed[0].Subject, "escalated") {
		t.Errorf("a change this build does not know routed %+v, want it read and repeated", routed)
	}
	if w, err = alarmwake.Encode(whole); err != nil {
		t.Fatal(err)
	}
	got, err := alarmwake.Decode(types.RawWebhook{Body: w.Body})
	if err != nil || got.ID != whole.ID || !got.At.Equal(at) {
		t.Errorf("the parsed body decoded as %+v (%v), want transition %s", got, err, whole.ID)
	}
}

// AN ALARM INSTANCE IS NAMED BY ITS ALARM AND ITS LOG — as the alarms page
// names it — and by nothing a transition adds: a fire, its clear, a later
// fire of the same alarm on the same log, and the same alarm on another node
// are one instance to the turns they start; the same alarm on another log, or
// node-wide, is another.
//
// Mutation: drop the log or the entry, and two instances are one; key on the
// transition's id, its raising instant or its node, and the fire and its
// clear, or two nodes' fires, are two.
func TestAnAlarmInstanceIsNamedByItsAlarmAndLog(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	base := statelog.AlarmTransition{Node: "node-a", Kind: statelog.KindApplyLag, Domain: "code",
		Change: statelog.TransitionFired, Since: at, At: at}
	base.ID = statelog.TransitionID(base.Node, base.Kind, base.Domain, base.Entry, base.Change, base.Since)
	if got := alarmwake.InstanceID(base); got != "apply_lag:code" {
		t.Errorf("the instance is %q, want apply_lag:code", got)
	}
	cleared, later, otherNode := base, base, base
	cleared.Change, cleared.At = statelog.TransitionCleared, at.Add(time.Minute)
	later.Since, later.At = at.Add(time.Hour), at.Add(time.Hour)
	otherNode.Node = "node-b"
	for _, same := range []statelog.AlarmTransition{cleared, later, otherNode} {
		if alarmwake.InstanceID(same) != alarmwake.InstanceID(base) {
			t.Errorf("%s %s on %s since %v is instance %q, want the fire's %q", same.Kind, same.Change,
				same.Node, same.Since, alarmwake.InstanceID(same), alarmwake.InstanceID(base))
		}
	}
	otherLog, nodeWide, entry := base, base, base
	otherLog.Domain, nodeWide.Domain = "worklet", ""
	entry.Domain, entry.Entry = "", "github"
	seen := map[string]bool{alarmwake.InstanceID(base): true}
	for _, other := range []statelog.AlarmTransition{otherLog, nodeWide, entry} {
		id := alarmwake.InstanceID(other)
		if seen[id] {
			t.Errorf("%s on log %q is instance %q, which another instance already is", other.Kind, other.Domain, id)
		}
		seen[id] = true
	}
	if got := alarmwake.InstanceID(nodeWide); got != "apply_lag" {
		t.Errorf("a node-wide instance is %q, want apply_lag", got)
	}
	if got := alarmwake.InstanceID(entry); got != "apply_lag:github" {
		t.Errorf("a per-entry instance is %q, want apply_lag:github", got)
	}
}

// A PER-ENTRY ALARM WAKES PER ENTRY: two MCP servers that go down in one
// evaluation wake each granted seat twice, under ids of their own so the inbox
// and the completion ledger do not collapse the second into the first, each
// wake naming its server in its metadata, its subject, its prompt and its origin.
//
// Mutation: drop the entry from the transition's id, and the wake ids collide;
// from the metadata or [alarmwake.InstanceID], and the prompt or the origin
// cannot say which server; call every entry a model entry, and the prompt
// misnames it.
func TestAPerEntryAlarmWakesPerEntryAndNamesIt(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	down := statelog.Alarm{Kind: statelog.KindMCPServerFailing, Detail: "servers are down",
		Entries: []statelog.AlarmEntry{{Key: "github", Detail: "github is down"},
			{Key: "linear", Detail: "linear is down"}}}
	seen := transitions(t, "node-a", &clock, nil, []statelog.Alarm{down})
	if len(seen) != 2 {
		t.Fatalf("the premise: the tracker handed on %+v, want one fire per server", seen)
	}
	p := alarmwake.NewParser(alarmwake.ParserOptions{Granted: []string{"sre", "ops"}})
	prompt := alarmwake.Prompt{}
	wakes := map[uuid.UUID]bool{}
	for _, tr := range seen {
		routed := parse(t, p, tr, nil)
		if got := handlesOf(routed); !slices.Equal(got, []string{"sre", "ops"}) {
			t.Fatalf("%s woke %v, want sre and ops", tr.Entry, got)
		}
		for _, r := range routed {
			if wakes[r.WakeID] {
				t.Errorf("%s's wake about %s repeats the id %s, so it would be dropped as a duplicate",
					r.To.Handle, tr.Entry, r.WakeID)
			}
			wakes[r.WakeID] = true
			if got := r.Metadata[alarmwake.EntryField]; got != tr.Entry {
				t.Errorf("%s's wake names the entry %q, want %q", r.To.Handle, got, tr.Entry)
			}
			if !strings.Contains(r.Subject, tr.Entry) {
				t.Errorf("the subject %q does not name %s", r.Subject, tr.Entry)
			}
			if body := prompt.Build(r.Inbound, nil); !strings.Contains(body, "for the MCP server `"+tr.Entry+"`") {
				t.Errorf("the prompt does not name the server %s:\n%s", tr.Entry, body)
			}
			if want := "mcp_server_failing:" + tr.Entry; r.Origin.ID != want {
				t.Errorf("the wake's origin is %q, want %q", r.Origin.ID, want)
			}
		}
	}
}

// A MODEL ENTRY'S ALARM CALLS ITS ENTRY A MODEL ENTRY, the table's other half
// of the per-entry kinds, and no other kind names an entry at all.
//
// Mutation: read the noun from anything but the table's split, or name an entry
// for a kind with none, and the case is red.
func TestAPromptCallsAnEntryWhatTheTableCallsIt(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	limit := statelog.Alarm{Kind: statelog.KindLLMLimit, Detail: "at its limit",
		Entries: []statelog.AlarmEntry{{Key: "sub", Detail: "sub is at its limit"}}}
	lag := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "behind"}
	seen := transitions(t, "node-a", &clock, nil, []statelog.Alarm{limit, lag})
	if len(seen) != 2 {
		t.Fatalf("the premise: the tracker handed on %+v, want two fires", seen)
	}
	p := alarmwake.NewParser(alarmwake.ParserOptions{Granted: []string{"sre"}})
	for _, tr := range seen {
		routed := parse(t, p, tr, nil)
		if len(routed) != 1 {
			t.Fatalf("%s routed %d copies, want 1", tr.Kind, len(routed))
		}
		body := alarmwake.Prompt{}.Build(routed[0].Inbound, nil)
		named := strings.Contains(body, "for the model entry `sub`")
		if want := tr.Kind == statelog.KindLLMLimit; named != want {
			t.Errorf("%s: the prompt names the model entry: %v, want %v:\n%s", tr.Kind, named, want, body)
		}
		if strings.Contains(body, "MCP server") {
			t.Errorf("%s's prompt speaks of an MCP server:\n%s", tr.Kind, body)
		}
	}
}

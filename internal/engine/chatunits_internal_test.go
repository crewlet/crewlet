package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// WHO BELONGS IN A UNIT'S ROOM, as arithmetic over the chart.
//
// Separated from the reconcile that writes it, for the reason internal/worklet
// and internal/textindex separate theirs: a rule exercised only through a
// database and a broker is a rule nobody re-reads, and this one decides two
// things that are easy to get quietly wrong — who is IN the room, and which of
// them every message wakes.
func chartFor(t *testing.T) *org.Organization {
	t.Helper()
	eng := &org.Unit{
		Name: "engineering", Lead: "dana", Channel: "eng",
		Roles: []*org.Role{
			{Name: "dana", Kind: org.KindAgent},
			{Name: "erin", Kind: org.KindHuman},
		},
		Children: []*org.Unit{{
			Name: "platform", Channel: "platform",
			Roles: []*org.Role{{Name: "frank", Kind: org.KindAgent}},
		}},
	}
	o := &org.Organization{Name: "acme", Units: []*org.Unit{eng}}
	o.Normalize()
	return o
}

func handlesOf(members []chatlet.Member) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Handle)
	}
	return out
}

// THE SUBTREE IS IN THE ROOM, not just the direct members.
//
// A division's room is where its teams are reachable. The other reading leaves
// a lead talking to an empty room while the people doing the work sit one
// level down.
func TestAUnitsRoomHoldsItsWholeSubtree(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	got := handlesOf(unitRoomMembers(chart, chart.Unit("engineering")))
	want := []string{"dana", "erin", "frank"}
	if !slices.Equal(got, want) {
		t.Errorf("engineering's room holds %v, want %v — a descendant's seat "+
			"is unreachable in the room its division talks in", got, want)
	}
}

// AND EVERY MESSAGE WAKES THIS UNIT'S OWN AGENTS AND NOBODY ELSE.
//
// THE THREE CASES ARE ONE RULE AND HAVE TO BE ASSERTED TOGETHER:
//
//   - this unit's own AGENT seat follows everything, because the room is where
//     it is addressed;
//   - a DESCENDANT's agent does not, because it has its own room and a message
//     in a division's room would otherwise be a turn for every agent beneath
//     it, each with its own executor in a box;
//   - a HUMAN never does, because a person is addressable and runs no turn.
//
// Asserting only the first would pass against a build that woke the entire
// subtree, which is the expensive failure.
func TestOnlyTheUnitsOwnAgentsFollowEverything(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	follow := map[string]bool{}
	for _, m := range unitRoomMembers(chart, chart.Unit("engineering")) {
		follow[m.Handle] = m.FollowAll
	}
	for handle, want := range map[string]bool{
		"dana":  true,  // this unit's own agent, and its lead
		"erin":  false, // a person: addressable, never woken
		"frank": false, // a descendant's agent: its own room wakes it
	} {
		if got := follow[handle]; got != want {
			t.Errorf("%s follows every message = %v, want %v", handle, got, want)
		}
	}
}

// A LEAD WHO IS NOT IN THE SUBTREE IS STILL IN THE ROOM.
//
// The lead answers for the unit — the chat routing's own fallback resolves
// through it — so a room it could not read would route a person's question to
// somebody who never sees it.
func TestTheLeadIsInTheRoomEvenFromOutsideTheSubtree(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "acme",
		Roles: []*org.Role{{Name: "gita", Kind: org.KindAgent}},
		Units: []*org.Unit{{
			Name: "support", Lead: "gita", Channel: "support",
			Roles: []*org.Role{{Name: "hugo", Kind: org.KindAgent}},
		}},
	}
	o.Normalize()
	got := handlesOf(unitRoomMembers(o, o.Unit("support")))
	if !slices.Contains(got, "gita") {
		t.Errorf("support's room holds %v, without the lead that answers for "+
			"it — the routing's unit fallback resolves to somebody who cannot "+
			"read the room", got)
	}
}

// A UNIT WITH NOBODY IN IT GETS NO MEMBERSHIP, and therefore no room.
//
// This kind refuses a join, so an empty unit room is one nothing could ever
// reach — and creating it would claim the name against the day the unit is
// actually staffed.
func TestAnEmptyUnitYieldsNoMembers(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "acme",
		Units: []*org.Unit{{Name: "ghost", Channel: "ghost"}},
	}
	o.Normalize()
	if got := unitRoomMembers(o, o.Unit("ghost")); len(got) != 0 {
		t.Errorf("an unstaffed unit yielded %v", handlesOf(got))
	}
}

// ---- the reconcile's own decisions --------------------------------------- //

// chartAt is the activation these cases apply their charts at.
var chartAt = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

// fakeRoom is one room as the fleet holds it: what the reconcile reads, and
// the membership and stamp the store decides against.
type fakeRoom struct {
	held    heldRoom
	members []chatlet.Member
	epoch   int64
}

// fleetRooms is the one address space every node's creates contend for: the
// broker lets the first create of a name take it and refuses every later one.
type fleetRooms struct {
	byName  map[string]*fakeRoom
	creates int
}

func newFleetRooms() *fleetRooms { return &fleetRooms{byName: map[string]*fakeRoom{}} }

// nodeRooms is one node's side of it: the writer the reconcile calls, and the
// rows this node has applied, which a node that lost a race may not have read
// yet when it decided to create.
type nodeRooms struct {
	node    string
	fleet   *fleetRooms
	applied map[string]heldRoom

	// answer, where set, is the outcome every write here answers with.
	answer *statelog.Result

	// sets is every membership this node WROTE — a decision the store's
	// own rule made, not every call.
	sets [][]chatlet.Member

	// calls is every write this node was ASKED for, decided or not — what
	// a gate that must stop the reconcile before the store is measured by.
	calls int
}

func newNodeRooms(node string, fleet *fleetRooms) *nodeRooms {
	return &nodeRooms{node: node, fleet: fleet, applied: map[string]heldRoom{}}
}

func (n *nodeRooms) CreateChannel(_ context.Context, _ chatlet.Actor,
	in chatlet.NewChannel) (chatlet.Written, error) {

	n.calls++
	name := chatlet.NormalizeName(in.Name)
	if room, taken := n.fleet.byName[name]; taken {
		// REFUSED ONLY ONCE THIS NODE HAS APPLIED THE WINNER, which is
		// the store's own order: the broker's refusal sends the writer
		// to catch up, and the name's guard is then this node's row.
		n.applied[name] = room.held
		return chatlet.Written{}, fmt.Errorf("%w: %s", chatlet.ErrNameTaken, name)
	}
	if n.answer != nil {
		return chatlet.Written{Outcome: *n.answer}, nil
	}
	room := &fakeRoom{
		held: heldRoom{id: "room-" + name + "-" + n.node, name: name,
			kind: string(in.Kind), unit: in.Unit},
		members: in.Members, epoch: configplane.ActivationStamp(in.ActivatedAt),
	}
	n.fleet.byName[name] = room
	n.fleet.creates++
	n.applied[name] = room.held
	return chatlet.Written{Channel: chatlet.Channel{ID: room.held.id},
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// SetUnitMembers decides on the store's own rule ([chatlet.Store.SetUnitMembers]):
// an older activation writes nothing, and so does the same activation over the
// same set and unit; a newer one over the same set and unit is a re-stamp, and
// a room recorded under another unit is re-homed.
func (n *nodeRooms) SetUnitMembers(_ context.Context, _ chatlet.Actor, channelID, unit string,
	members []chatlet.Member, activatedAt time.Time) (chatlet.Written, chatlet.MembershipWrite, error) {

	return n.chartWrite(channelID, unit, members, activatedAt)
}

// ReleaseUnitRoom is a chart write to nobody that moves no unit
// ([chatlet.Store.ReleaseUnitRoom]).
func (n *nodeRooms) ReleaseUnitRoom(_ context.Context, _ chatlet.Actor, channelID string,
	activatedAt time.Time) (chatlet.Written, chatlet.MembershipWrite, error) {

	return n.chartWrite(channelID, "", nil, activatedAt)
}

// chartWrite is both chart writes, decided against the fleet's own copy of the
// room — the store's snapshot after it has caught up with the broker — and
// applied to this node's rows.
func (n *nodeRooms) chartWrite(channelID, unit string, members []chatlet.Member,
	activatedAt time.Time) (chatlet.Written, chatlet.MembershipWrite, error) {

	n.calls++
	if n.answer != nil {
		n.sets = append(n.sets, members)
		return chatlet.Written{Outcome: *n.answer}, chatlet.MembershipMoved, nil
	}
	applied := chatlet.Written{Outcome: statelog.Result{Outcome: statelog.OutcomeApplied}}
	epoch := configplane.ActivationStamp(activatedAt)
	for _, room := range n.fleet.byName {
		if room.held.id != channelID {
			continue
		}
		same := slices.Equal(room.members, members)
		rehome := unit != "" && unit != room.held.unit
		switch {
		case room.epoch > epoch, room.epoch == epoch && same && !rehome:
			return applied, chatlet.MembershipUnchanged, nil
		case same && !rehome:
			room.epoch = epoch
			return applied, chatlet.MembershipRestamped, nil
		}
		room.members, room.epoch = members, epoch
		n.sets = append(n.sets, members)
		if !rehome {
			return applied, chatlet.MembershipMoved, nil
		}
		room.held.unit = unit
		n.applied[room.held.name] = room.held
		return applied, chatlet.MembershipRehomed, nil
	}
	return chatlet.Written{}, chatlet.MembershipUnchanged,
		fmt.Errorf("%w: room %s", chatlet.ErrNotFound, channelID)
}

func (n *nodeRooms) room(_ context.Context, name string) (heldRoom, bool, error) {
	held, ok := n.applied[name]
	return held, ok, nil
}

// unitRooms is every unit room this node has applied, by name.
func (n *nodeRooms) unitRooms(context.Context) ([]heldRoom, error) {
	var out []heldRoom
	for _, held := range n.applied {
		if held.kind == string(chatlet.KindUnit) {
			out = append(out, held)
		}
	}
	slices.SortFunc(out, func(a, b heldRoom) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// failuresOf is the units a pass could not bring into line, failing the case
// for each.
func failuresOf(t *testing.T, results []unitRoomResult) {
	t.Helper()
	for _, r := range results {
		if r.err != nil {
			t.Errorf("unit %s's room #%s was not reconciled: %v", r.unit, r.channel, r.err)
		}
	}
}

// TWO NODES RECONCILING ONE UNIT MAKE ONE ROOM, AND THE ONE THAT LOST THE RACE
// SUCCEEDS.
//
// Every node runs the unit-room reconcile on every apply, so two of them
// routinely read "no room yet" and both create it. The broker lets one take
// the name and refuses the other as a name that is taken — which the reconcile
// used to treat as a failure, logging `chat_unit_room_not_reconciled` at ERROR
// on every node but one, on every apply, for a room that was exactly as it
// should be. The refusal now reads the room that took the name and judges it
// like any room that was already there: this unit's is success, and its
// membership is already the chart's at this activation, so nothing more is
// written.
func TestTwoNodesReconcilingOneUnitMakeOneRoom(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	unit := chart.Unit("engineering")
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	fleet := newFleetRooms()
	a, b := newNodeRooms("a", fleet), newNodeRooms("b", fleet)

	// BOTH READ BEFORE EITHER WRITES: b's read is decided from rows that do
	// not hold a's room yet.
	if _, err := applyUnitRoom(t.Context(), a, a, actor, chart, unit, "eng",
		chartAt); err != nil {
		t.Fatalf("the node that won the name: %v", err)
	}
	if _, err := applyUnitRoom(t.Context(), b, b, actor, chart, unit, "eng",
		chartAt); err != nil {
		t.Fatalf("the node that lost the race reported %v — the room it was "+
			"creating is there, made by another node for this same unit", err)
	}
	if fleet.creates != 1 {
		t.Fatalf("the fleet made %d rooms for one unit", fleet.creates)
	}
	if len(b.sets) != 0 {
		t.Errorf("the node that lost the race rewrote the membership %v — the "+
			"winner made the room with the chart's set at this activation", b.sets)
	}
}

// A NAME SOMEBODY ELSE TOOK IN THE RACE IS STILL NOT THE CHART'S TO TAKE OVER.
func TestARaceLostToAnotherRoomIsReportedAsTheTakeover(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	unit := chart.Unit("engineering")
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	fleet := newFleetRooms()
	fleet.byName["eng"] = &fakeRoom{held: heldRoom{id: "room-by-hand",
		kind: string(chatlet.KindPublic)}}
	node := newNodeRooms("a", fleet)
	_, err := applyUnitRoom(t.Context(), node, node, actor, chart, unit, "eng", chartAt)
	if err == nil || !strings.Contains(err.Error(), "will not take it over") {
		t.Fatalf("a name a person's public room took answered %v, want the "+
			"takeover refusal", err)
	}
	if len(node.sets) != 0 {
		t.Errorf("the chart wrote a membership into somebody else's room: %v", node.sets)
	}
}

// AN UNKNOWN CREATE, OR AN UNKNOWN MEMBERSHIP, IS NOT A RECONCILED ROOM.
//
// Logging it as created told an operator reading the log that the room
// exists when the create may never have landed; it is reported as not
// reconciled instead, naming its operation, and the next pass — which reads
// the row first — decides it again.
func TestAnUnknownUnitRoomWriteIsNotReportedAsDone(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	unit := chart.Unit("engineering")
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	unknown := statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op-lost"}

	creating := newNodeRooms("a", newFleetRooms())
	creating.answer = &unknown
	if _, err := applyUnitRoom(t.Context(), creating, creating, actor, chart, unit,
		"eng", chartAt); err == nil || !strings.Contains(err.Error(), "op-lost") {
		t.Errorf("an unknown create answered %v, want it reported with its operation", err)
	}

	stale := newNodeRooms("a", newFleetRooms())
	stale.applied["eng"] = heldRoom{id: "room-eng", kind: string(chatlet.KindUnit),
		unit: unit.Key()}
	stale.answer = &unknown
	if _, err := applyUnitRoom(t.Context(), stale, stale, actor, chart, unit,
		"eng", chartAt); err == nil || !strings.Contains(err.Error(), "op-lost") {
		t.Errorf("an unknown membership write answered %v, want it reported "+
			"with its operation", err)
	}
}

// A ROOM BELONGS TO THE UNIT THAT DECLARES ITS CHANNEL, and a child that only
// inherits one gets none of its own.
//
// [org.Unit.Channel] is the EFFECTIVE value: a child that names no channel
// inherits its parent's. The reconcile used to walk that field, so the parent's
// room was reconciled once more for every child that wrote nothing — whichever
// unit the walk reached first made it under its own key and subtree, and every
// other one was refused as a takeover, an ERROR per inheriting unit on every
// apply. A child that declares a channel of its own still gets that room.
func TestOnlyAUnitThatDeclaresAChannelGetsARoom(t *testing.T) {
	t.Parallel()
	eng := &org.Unit{
		Name: "engineering", Lead: "dana", Channel: "eng",
		Roles: []*org.Role{{Name: "dana", Kind: org.KindAgent}},
		Children: []*org.Unit{
			{
				// INHERITS #eng, and must not reconcile it.
				Name:  "infra",
				Roles: []*org.Role{{Name: "ivan", Kind: org.KindAgent}},
			},
			{
				Name: "platform", Channel: "platform",
				Roles: []*org.Role{{Name: "frank", Kind: org.KindAgent}},
			},
		},
	}
	chart := &org.Organization{Name: "acme", Units: []*org.Unit{eng}}
	chart.Normalize()
	if got := chart.Unit("infra").Channel; got != "eng" {
		t.Fatalf("the child's effective channel is %q — the case needs it to "+
			"inherit its parent's", got)
	}

	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, chart, chartAt))
	if fleet.creates != 2 {
		t.Errorf("the chart made %d rooms, want 2 — one per unit that declares a "+
			"channel", fleet.creates)
	}
	for name, unit := range map[string]string{"eng": "engineering", "platform": "platform"} {
		room, held := fleet.byName[name]
		if !held {
			t.Errorf("#%s was never created", name)
			continue
		}
		if room.held.unit != chart.Unit(unit).Key() {
			t.Errorf("#%s belongs to %q, want the unit that declares it, %q",
				name, room.held.unit, chart.Unit(unit).Key())
		}
	}
	// AND THE INHERITING CHILD'S SEAT IS IN ITS PARENT'S ROOM, which is where
	// it was always reachable: the room holds the declaring unit's subtree.
	if !slices.Contains(handlesOf(fleet.byName["eng"].members), "ivan") {
		t.Errorf("#eng holds %v, without the inheriting child's seat",
			handlesOf(fleet.byName["eng"].members))
	}
	// AS A MEMBER, NOT A FOLLOWER. Its prompt names #eng as its team's room,
	// but a channel cascades down a whole branch, so following it would make
	// every message in a division's room a turn for every agent beneath it.
	// It is reached by a mention, a thread it is in, `@channel` or the lead.
	for _, m := range fleet.byName["eng"].members {
		if want := m.Handle == "dana"; m.FollowAll != want {
			t.Errorf("%s follows every message in #eng = %v, want %v — only the "+
				"declaring unit's own agents do", m.Handle, m.FollowAll, want)
		}
	}
}

// EVERY WRITE CARRIES THE ACTIVATION THE CHART CAME FROM — the create and the
// membership both — so the store can refuse an older chart applied late.
//
// The reconcile used to compare the membership itself and write only a set
// that moved, with no stamp: a node booting on a revision the fleet had since
// replaced wrote its own chart's team back over the current one. And it used
// to skip the write whenever the set was unchanged, which also skips the
// RE-STAMP a newer activation owes a room whose set did not move.
func TestTheChartsActivationTravelsWithEveryUnitRoomWrite(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	unit := chart.Unit("engineering")
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)

	if _, err := applyUnitRoom(t.Context(), node, node, actor, chart, unit, "eng",
		chartAt); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, want := fleet.byName["eng"].epoch, configplane.ActivationStamp(chartAt); got != want {
		t.Errorf("the create carried activation %d, want %d", got, want)
	}

	// AN OLDER CHART WITH ANOTHER TEAM, applied late: nothing is written.
	older := chartFor(t)
	older.Unit("engineering").Roles = older.Unit("engineering").Roles[:1]
	if _, err := applyUnitRoom(t.Context(), node, node, actor, older,
		older.Unit("engineering"), "eng", chartAt.Add(-time.Minute)); err != nil {
		t.Fatalf("an older chart: %v", err)
	}
	if len(node.sets) != 0 {
		t.Errorf("an older chart wrote %v over the current membership", node.sets)
	}

	// A NEWER ACTIVATION OF THE SAME CHART reaches the store, which re-stamps.
	newer := chartAt.Add(time.Minute)
	did, err := applyUnitRoom(t.Context(), node, node, actor, chart, unit, "eng", newer)
	if err != nil {
		t.Fatalf("a newer activation: %v", err)
	}
	if got, want := fleet.byName["eng"].epoch, configplane.ActivationStamp(newer); got != want {
		t.Errorf("after a newer activation over the same set the room is at %d, "+
			"want %d — the reconcile skipped the re-stamp", got, want)
	}
	// AND A RE-STAMP IS NOT A MEMBERSHIP CHANGE: it is what every room does
	// on every activation, re-activating an unchanged revision included.
	if did.membership != chatlet.MembershipRestamped {
		t.Errorf("a newer activation over the same set reported %q, want %q",
			did.membership, chatlet.MembershipRestamped)
	}
}

// WHAT A PASS LOGS IS WHAT IT DID, and a room that was already right is not
// news.
//
// The reconcile runs on every node at every boot and every apply, so the
// ordinary outcome is a room that already says what the chart says — and a
// re-stamp is the ordinary outcome of every activation. The store used to
// answer a re-stamp as a write, and the reconcile logged
// chat_unit_room_membership_set for every unit room on every activation,
// re-activating an unchanged revision (the credential-rotation gesture)
// included, with a member count beside a membership nobody moved.
func TestAUnitRoomPassLogsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	failed := errors.New("refused")
	for _, tc := range []struct {
		name   string
		result unitRoomResult
		level  slog.Level
		event  string
	}{
		{"a failure", unitRoomResult{err: failed}, slog.LevelError,
			"chat_unit_room_not_reconciled"},
		{"a room opened", unitRoomResult{created: true}, slog.LevelInfo,
			"chat_unit_room_created"},
		{"a membership that moved", unitRoomResult{membership: chatlet.MembershipMoved},
			slog.LevelInfo, "chat_unit_room_membership_set"},
		{"a room that followed its channel to another unit",
			unitRoomResult{membership: chatlet.MembershipRehomed}, slog.LevelInfo,
			"chat_unit_room_rehomed"},
		{"a re-stamp", unitRoomResult{membership: chatlet.MembershipRestamped},
			slog.LevelDebug, "chat_unit_room_restamped"},
		{"a room the chart stopped naming, emptied",
			unitRoomResult{released: true, membership: chatlet.MembershipMoved},
			slog.LevelInfo, "chat_unit_room_released"},
		{"a room the chart stopped naming, already empty",
			unitRoomResult{released: true, membership: chatlet.MembershipUnchanged}, 0, ""},
		{"a room already right", unitRoomResult{membership: chatlet.MembershipUnchanged},
			0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			level, event := unitRoomEvent(tc.result)
			if event != tc.event || (event != "" && level != tc.level) {
				t.Errorf("logged %q at %v, want %q at %v", event, level, tc.event, tc.level)
			}
		})
	}
}

// ---- the gates in front of the reconcile ---------------------------------- //

// unitRoomChart is a company on Chatlet whose one unit declares #eng —
// chat derives chatlet, since no vendor chat surface is declared — with a
// hole a case fills with a chat block.
const unitRoomChart = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
CHAT
units:
  - name: Engineering
    channel: eng
    roles:
      - name: CEO
        handle: ceo
        llm: zulu
`

// THE RECONCILE WRITES ONLY FOR A COMPANY ON CHATLET, AND ONLY WITH AN
// ACTIVATION — and both refusals are made before the store is asked.
//
//   - A company that moved to a vendor surface, or to none, publishes nothing
//     on chat's log: that is [Engine.chatRetentionPolicy]'s rule for this
//     domain, and the chat store is built once per process from the first
//     company, so a revision leaving Chatlet used to go on creating and
//     re-stamping unit rooms at every boot and every apply of every data node.
//   - A company no activation has named is applied by its first activation.
//     Without the guard the store refuses the zero instant
//     ([chatlet.ErrNoActivation]), so nothing lands — and one ERROR per unit is
//     logged at every unactivated boot, which no other case would notice.
//
// THE CONTROL is the same company, on Chatlet and activated, opening its room: a
// gate that refused everything would pass both cases above.
func TestTheUnitRoomReconcileWritesOnlyForAnActivatedChatletCompany(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chat      string
		activated time.Time
		writes    bool
	}{
		"a Chatlet company, activated":            {"", chartAt, true},
		"a Chatlet company no activation named":   {"", time.Time{}, false},
		"a company that moved off Chatlet":        {"chat:\n  backend: none", chartAt, false},
		"a company that names chatlet explicitly": {"chat:\n  backend: chatlet", chartAt, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := companyWith(t, strings.Replace(unitRoomChart, "CHAT", tc.chat, 1))
			node := newNodeRooms("a", newFleetRooms())
			failuresOf(t, applyCompanyRooms(t.Context(), node, node, c, tc.activated))
			if wrote := node.calls > 0; wrote != tc.writes {
				t.Errorf("the reconcile asked the store for %d write(s), want "+
					"writes=%v", node.calls, tc.writes)
			}
		})
	}
}

// A NODE IN A CAPACITY WINDOW OPENS NO UNIT ROOM — at boot or on an apply.
//
// The reconcile's writer is [Engine.ChatStore], which is [Engine.writeSide]'s
// and is nil in `maintenance` and `seal`: a window is measured against the
// logs holding still, and a seal's acknowledgement is evidence only because
// nothing on the node writes. [TestANodeInACapacityWindowAppendsNothingToItsLogs]
// cannot see this path — it boots with no activation and a company with no
// units, so the reconcile returns before reaching any writer. This boots the
// company it acts on, activated, and asks again through an apply.
//
// THE CONTROL is the same boot in `normal` opening the room, or a node that
// never wrote anywhere would pass.
func TestANodeInACapacityWindowOpensNoUnitRoom(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(unitRoomChart, "CHAT", "", 1)
	for _, mode := range []statelog.MaintenanceMode{
		statelog.ModeMaintenance, statelog.ModeSeal, statelog.ModeNormal,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			e, js := aNodeWithCompanyInMode(t, mode, doc, chartAt)
			if mode == statelog.ModeNormal {
				waitUntil(t, 20*time.Second, "the publishing node to open #eng",
					func() bool { return unitRoomHeld(t, e, "eng") })
				return
			}
			end := lastSeq(t, js, topics.ChatLogStream)
			if unitRoomHeld(t, e, "eng") {
				t.Fatalf("a %s-mode boot opened #eng", mode)
			}
			cfg, err := config.ParseCompany([]byte(doc))
			if err != nil {
				t.Fatalf("parse the company: %v", err)
			}
			if _, _, err := e.Apply(t.Context(), cfg, chartAt.Add(time.Minute)); err != nil {
				t.Fatalf("Apply in %s mode: %v", mode, err)
			}
			if unitRoomHeld(t, e, "eng") {
				t.Errorf("an apply on a %s-mode node opened #eng", mode)
			}
			if got := lastSeq(t, js, topics.ChatLogStream); got != end {
				t.Errorf("the chat log moved from %d to %d on a %s-mode node",
					end, got, mode)
			}
		})
	}
}

// aNodeWithCompanyInMode boots a node the way `crewlet run -mode <mode>` does,
// over this company, activated at activatedAt.
func aNodeWithCompanyInMode(t *testing.T, mode statelog.MaintenanceMode, doc string,
	activatedAt time.Time) (*Engine, natsjs.JetStream) {

	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back,
		Mode: mode, ActivatedAt: activatedAt})
	if err != nil {
		t.Fatalf("New in %s mode: %v", mode, err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return e, js
}

// unitRoomHeld reports whether this node's rows hold a room named name.
func unitRoomHeld(t *testing.T, e *Engine, name string) bool {
	t.Helper()
	_, found, err := unitRoomRows{db: e.domainEstate()}.room(t.Context(), name)
	if err != nil {
		t.Fatalf("read #%s: %v", name, err)
	}
	return found
}

// ---- who is in a room the chart moved on from ------------------------------ //

// A UNIT THAT LOSES ITS LAST SEAT KEEPS ITS ROOM AND LOSES ITS MEMBERS.
//
// The "nobody in it gets no room" rule returned before the room was even
// read, so it skipped an EXISTING room too: a unit with no lead up its chain
// that lost its last seat kept its last members for good, FollowAll flags
// included, and every message posted there went on waking an agent the chart
// had moved to another team — each an executor in a box, in a room its
// prompt no longer names. The rule is about CREATING a room nobody could
// reach; a room that exists is restated like any other, as nobody.
func TestAUnitThatLosesItsLastSeatEmptiesItsRoom(t *testing.T) {
	t.Parallel()
	staffed := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Channel: "mkt",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "sales", Channel: "sales",
			Roles: []*org.Role{{Name: "sam", Kind: org.KindAgent}}},
	}}
	staffed.Normalize()
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, staffed, chartAt))

	// ANN MOVES TO SALES, and marketing — no lead anywhere — is empty.
	moved := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Channel: "mkt"},
		{Name: "sales", Channel: "sales", Roles: []*org.Role{
			{Name: "ann", Kind: org.KindAgent}, {Name: "sam", Kind: org.KindAgent}}},
	}}
	moved.Normalize()
	newer := chartAt.Add(time.Minute)
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, moved, newer))
	mkt := fleet.byName["mkt"]
	if len(mkt.members) != 0 || mkt.epoch != configplane.ActivationStamp(newer) {
		t.Errorf("#mkt holds %v at %d after its last seat moved away, want nobody "+
			"at %d", mkt.members, mkt.epoch, configplane.ActivationStamp(newer))
	}

	// AND A UNIT THAT NEVER HAD ANYBODY STILL GETS NO ROOM CREATED.
	if _, held := fleet.byName["ghost"]; held {
		t.Fatal("the premise: #ghost exists before its unit does")
	}
	ghost := &org.Organization{Name: "acme", Units: []*org.Unit{{Name: "ghost", Channel: "ghost"}}}
	ghost.Normalize()
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, ghost, newer))
	if _, held := fleet.byName["ghost"]; held {
		t.Error("an unstaffed unit's room was created, claiming the name " +
			"against the day the unit is staffed")
	}
}

// A ROOM THE CHART NO LONGER NAMES IS STAFFED BY NOBODY — and kept.
//
// The chart is the only writer of a unit room's membership: joining, leaving
// and setting one are refused for this kind. So a unit that stopped declaring
// its channel, or left the chart, left a room whose members nothing could
// ever change — its agents following every message in a room their prompt no
// longer names. The room stays (a transcript is the only copy of what people
// said); its people are what the chart says, which is nobody.
func TestARoomTheChartNoLongerNamesIsEmptied(t *testing.T) {
	t.Parallel()
	before := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Channel: "mkt",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "growth", Channel: "growth",
			Roles: []*org.Role{{Name: "gus", Kind: org.KindAgent}}},
		{Name: "sales", Channel: "sales",
			Roles: []*org.Role{{Name: "sam", Kind: org.KindAgent}}},
	}}
	before.Normalize()
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, before, chartAt))

	// MARKETING DROPS ITS CHANNEL, AND GROWTH LEAVES THE CHART.
	after := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "sales", Channel: "sales", Roles: []*org.Role{
			{Name: "sam", Kind: org.KindAgent}, {Name: "gus", Kind: org.KindAgent}}},
	}}
	after.Normalize()
	newer := chartAt.Add(time.Minute)
	results := applyChartRooms(t.Context(), node, node, actor, after, newer)
	failuresOf(t, results)
	for _, name := range []string{"mkt", "growth"} {
		room := fleet.byName[name]
		if len(room.members) != 0 || room.epoch != configplane.ActivationStamp(newer) {
			t.Errorf("#%s holds %v at %d after the chart stopped naming it, want "+
				"nobody at %d", name, room.members, room.epoch,
				configplane.ActivationStamp(newer))
		}
	}
	released := map[string]bool{}
	for _, r := range results {
		if r.released && r.membership == chatlet.MembershipMoved {
			released[r.channel] = true
		}
	}
	if !released["mkt"] || !released["growth"] || released["sales"] {
		t.Errorf("released %v, want #mkt and #growth and never a room the chart names",
			released)
	}
	// THE CONTROL: the room the chart still names keeps its team.
	if got := handlesOf(fleet.byName["sales"].members); !slices.Equal(got, []string{"gus", "sam"}) {
		t.Errorf("#sales holds %v, want [gus sam]", got)
	}

	// AND THE NEXT PASS AT THE SAME ACTIVATION WRITES NOTHING.
	writes := len(node.sets)
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, after, newer))
	if len(node.sets) != writes {
		t.Errorf("a second pass at one activation wrote %d membership(s)",
			len(node.sets)-writes)
	}

	// AND DECLARING IT AGAIN STAFFS IT AGAIN.
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, before,
		newer.Add(time.Minute)))
	if got := handlesOf(fleet.byName["mkt"].members); !slices.Equal(got, []string{"ann"}) {
		t.Errorf("#mkt holds %v after marketing declared it again, want [ann]", got)
	}
}

// ---- whose room it is ---------------------------------------------------- //

// A ROOM'S STORED UNIT IS READ BACK THROUGH THE CHART'S ONE RESOLVER, so every
// spelling of one team is that team.
//
// A room records its unit's key once, at the create, and the reconcile reads
// it back at every later activation — a durable unit reference, which
// [org.Organization.UnitByRef] is the one resolver of: an id or a name, under
// the fold the admission rule claims a key with. The reconcile used to match
// it exactly, so a unit whose name changed only in case (one team, to the
// chart and to the room's own answerer) was refused its room with an
// ERROR and had it emptied by the sweep, on every pass.
func TestEverySpellingOfAUnitKeepsItsRoom(t *testing.T) {
	t.Parallel()
	staffed := func(name, id string) *org.Organization {
		o := &org.Organization{Name: "acme", Units: []*org.Unit{{
			Name: name, ID: id, Channel: "plat",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}},
		}}}
		o.Normalize()
		return o
	}
	for name, tc := range map[string]struct{ before, after *org.Organization }{
		"a name whose case changed": {staffed("Platform", ""), staffed("platform", "")},
		"an id added to the unit":   {staffed("Platform", ""), staffed("Platform", "u-7")},
		"an id whose case changed":  {staffed("Platform", "U-7"), staffed("Platform", "u-7")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := newFleetRooms()
			node := newNodeRooms("a", fleet)
			actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
			failuresOf(t, applyChartRooms(t.Context(), node, node, actor, tc.before, chartAt))
			results := applyChartRooms(t.Context(), node, node, actor, tc.after,
				chartAt.Add(time.Minute))
			failuresOf(t, results)
			for _, r := range results {
				if r.released {
					t.Errorf("#%s was released — the chart still declares it on "+
						"the unit whose room it is", r.channel)
				}
			}
			if got := handlesOf(fleet.byName["plat"].members); !slices.Equal(got, []string{"ann"}) {
				t.Errorf("#plat holds %v, want [ann]", got)
			}
			if fleet.creates != 1 {
				t.Errorf("the fleet made %d rooms for one unit", fleet.creates)
			}
		})
	}
}

// AN ID BEATS ANOTHER UNIT'S NAME, in the per-unit pass and the sweep alike.
//
// A stored chart may carry a unit whose id is another unit's name — refused
// when a document is written, still run when one is stored. The resolver
// settles it (the id's unit), and the reconcile has to ask it in BOTH places:
// with an exact match of its own, the sweep found the unit NAMED "platform"
// first, saw it declare no such room and emptied #plat, while the per-unit
// pass staffed #plat as the id's — two writes at one activation, on every
// pass, ending in whichever came last.
func TestAnIDOwnsTheRoomOverAnotherUnitsName(t *testing.T) {
	t.Parallel()
	chart := &org.Organization{Name: "acme", Units: []*org.Unit{
		// FIRST IN THE WALK, so a walk-order resolver finds it first. It
		// declares no room: a unit whose NAME is another's id loses every
		// durable reference to the id, a room keyed on it included.
		{Name: "platform",
			Roles: []*org.Role{{Name: "pat", Kind: org.KindAgent}}},
		{Name: "Platform Services", ID: "platform", Channel: "plat",
			Roles: []*org.Role{{Name: "sam", Kind: org.KindAgent}}},
	}}
	chart.Normalize()
	fleet := newFleetRooms()
	fleet.byName["plat"] = &fakeRoom{held: heldRoom{id: "room-plat", name: "plat",
		kind: string(chatlet.KindUnit), unit: "platform"}}
	node := newNodeRooms("a", fleet)
	node.applied["plat"] = fleet.byName["plat"].held
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}

	for pass := range 2 {
		results := applyChartRooms(t.Context(), node, node, actor, chart, chartAt)
		failuresOf(t, results)
		for _, r := range results {
			if r.released {
				t.Errorf("pass %d released #%s — the unit whose id the room "+
					"names declares it", pass, r.channel)
			}
		}
		if got := handlesOf(fleet.byName["plat"].members); !slices.Equal(got, []string{"sam"}) {
			t.Errorf("after pass %d #plat holds %v, want [sam] — the team of the "+
				"unit whose ID the room names", pass, got)
		}
	}
}

// A ROOM THE CHART STAFFS WITH NOBODY WAKES NO LEAD, and a room it staffs
// wakes the lead of the unit that owns it.
//
// The sweep empties a room whose unit stopped declaring it, and the room's
// answerer used to resolve that unit's lead regardless, so every
// message a person posted there woke the old lead under a reason that obliges
// an answer, from a room no prompt names as theirs: the cost the sweep was
// written to remove. Both now ask [unitRoomOwner], and this holds them to one
// answer for every room a pass leaves behind — a room released has no lead,
// a room kept has its owner's — and to what the routing does with it.
func TestARoomsLeadIsTheLeadOfTheUnitThatStillDeclaresIt(t *testing.T) {
	t.Parallel()
	before := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Lead: "ann", Channel: "mkt",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "sales", Lead: "sam", Channel: "sales",
			Roles: []*org.Role{{Name: "sam", Kind: org.KindAgent}}},
	}}
	before.Normalize()
	// MARKETING DROPS ITS CHANNEL and keeps its lead.
	after := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Lead: "ann",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "sales", Lead: "sam", Channel: "sales",
			Roles: []*org.Role{{Name: "sam", Kind: org.KindAgent}}},
	}}
	after.Normalize()
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, before, chartAt))
	if got := unitRoomLead(before, "marketing", "mkt"); got != "ann" {
		t.Fatalf("the premise: #mkt's lead is %q while marketing declares it, "+
			"want ann", got)
	}
	results := applyChartRooms(t.Context(), node, node, actor, after, chartAt.Add(time.Minute))
	failuresOf(t, results)

	released := map[string]bool{}
	for _, r := range results {
		if r.released {
			released[r.channel] = true
		}
	}
	for name, want := range map[string]string{"mkt": "", "sales": "sam"} {
		room := fleet.byName[name].held
		lead := unitRoomLead(after, room.unit, room.name)
		if lead != want {
			t.Errorf("#%s's lead is %q, want %q", name, lead, want)
		}
		if released[name] != (lead == "") {
			t.Errorf("#%s: released=%v with lead %q: the sweep and the room's "+
				"answerer answered two different owners", name, released[name], lead)
		}
		var fallback []string
		candidates, _ := chatlet.Resolve(chatlet.Routing{ChannelKind: chatlet.KindUnit,
			AuthorKind: chatlet.AuthorHuman, Answerer: lead})
		for _, c := range candidates {
			if c.Reason == chatlet.ReasonAnswerer {
				fallback = append(fallback, c.Handle)
			}
		}
		if wake := want != ""; wake != (len(fallback) == 1) {
			t.Errorf("a person's post in #%s wakes %v as its answerer, "+
				"want a lead woken = %v", name, fallback, wake)
		}
	}
}

// A ROOM MADE BY HAND IS ANSWERED BY ITS MOST SENIOR AGENT MEMBER.
//
// The chart is the only thing that says who in a room answers for the others,
// and the routing reads it here: the agent member with the fewest managers
// above it, never a person (who runs no turn, however senior), never a seat
// the chart no longer holds, and never a seat that is not in the room, so a
// private room's answerer can always read it. Mutation: admit people, or rank
// by the unit tree rather than the management chain, and a row fails.
func TestARoomMadeByHandIsAnsweredByItsMostSeniorAgentMember(t *testing.T) {
	t.Parallel()
	chart := &org.Organization{Name: "acme",
		Roles: []*org.Role{
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Manages: []string{"ceo"}},
			{Name: "CEO", DeclaredHandle: "ceo", Kind: org.KindAgent, Manages: []string{"vp"}},
		},
		Units: []*org.Unit{{Name: "engineering", Lead: "vp", Roles: []*org.Role{
			{Name: "VP", DeclaredHandle: "vp", Kind: org.KindAgent, Manages: []string{"dev", "qa"}},
			{Name: "Dev", DeclaredHandle: "dev", Kind: org.KindAgent},
			{Name: "QA", DeclaredHandle: "qa", Kind: org.KindAgent},
		}}},
	}
	chart.Normalize()
	for _, tc := range []struct {
		name    string
		members []string
		want    string
	}{
		{"the manager of the others", []string{"dev", "qa", "vp"}, "vp"},
		{"a person outranks everybody and runs no turn", []string{"founder", "dev", "ceo"}, "ceo"},
		{"peers resolve by the chart's order", []string{"qa", "dev"}, "dev"},
		{"a seat the chart no longer holds is no seat", []string{"retired", "qa"}, "qa"},
		{"a room of people has no answerer", []string{"founder"}, ""},
		{"an empty room has no answerer", nil, ""},
	} {
		if got := seniorAgent(chart, tc.members); got != tc.want {
			t.Errorf("%s: the answerer of a room of %v is %q, want %q", tc.name,
				tc.members, got, tc.want)
		}
	}
}

// ---- a declared name somebody else holds ---------------------------------- //

// A ROOM HELD ELSEWHERE IS REPORTED FOR WHY, WITH THE REMEDY THAT WORKS.
//
// The refusal used to read "rename the unit's channel or the room" for every
// case, and half of it was impossible: a room's name never changes in this
// build, and an archived room keeps it. Two cases are left now that a room
// follows its channel to the unit that declares it (see
// [TestAChannelMovedToAnotherUnitTakesItsRoom]): a room a person opened, and
// a room another unit declares too. Each says which it is.
func TestARoomHeldElsewhereSaysWhyAndWhatWorks(t *testing.T) {
	t.Parallel()
	engineering := &org.Unit{Name: "engineering", Channel: "eng"}
	design := &org.Unit{Name: "design", Channel: "eng"}
	for name, tc := range map[string]struct {
		held  heldRoom
		owner *org.Unit
		says  []string
	}{
		"a room a person opened": {
			heldRoom{id: "r1", name: "eng", kind: string(chatlet.KindPublic)}, nil,
			[]string{"will not take it over", "never changes", "a channel nobody holds"},
		},
		"the room of another unit that declares it too": {
			heldRoom{id: "r2", name: "eng", kind: string(chatlet.KindUnit), unit: "design"},
			design,
			[]string{`unit "design"`, "declares it too", `give "engineering" a channel of its own`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := heldElsewhere(tc.held, engineering, tc.owner, "eng")
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal reads %q, which does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "or the room") {
				t.Errorf("the refusal offers renaming the room, which this build "+
					"cannot do: %q", err)
			}
		})
	}
}

// ---- a room follows its channel ------------------------------------------- //

// A CHANNEL MOVED TO ANOTHER UNIT TAKES ITS ROOM WITH IT.
//
// A room's unit used to be fixed at its create, so moving a team's channel to
// the unit that replaces it in a reorg — a document that validates clean —
// had the new declarer refused the room at ERROR on every apply of every data
// node, while the sweep emptied it because the unit it was recorded under no
// longer declared it: the team without the room its prompt names, for good.
// The room now follows its channel, staffed with the new unit's people and
// answering to its lead. A node whose rows still say the old unit decides the
// same, and an older activation, applied late, moves it back no more than it
// moves the members back.
func TestAChannelMovedToAnotherUnitTakesItsRoom(t *testing.T) {
	t.Parallel()
	before := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Lead: "ann", Channel: "mkt",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "growth", Lead: "gus",
			Roles: []*org.Role{{Name: "gus", Kind: org.KindAgent}}},
	}}
	before.Normalize()
	after := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Lead: "ann",
			Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "growth", Lead: "gus", Channel: "mkt",
			Roles: []*org.Role{{Name: "gus", Kind: org.KindAgent}}},
	}}
	after.Normalize()
	fleet := newFleetRooms()
	a := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), a, a, actor, before, chartAt))
	// A SECOND NODE THAT HAS APPLIED THE ROOM AS MARKETING'S, and nothing
	// since.
	b := newNodeRooms("b", fleet)
	b.applied["mkt"] = a.applied["mkt"]

	// BEFORE THE RE-HOME LANDS, a person's post already reaches the lead
	// of the unit the room is moving to — never the old one.
	if got := unitRoomLead(after, "marketing", "mkt"); got != "gus" {
		t.Errorf("#mkt's lead under the new chart, recorded as marketing's, is "+
			"%q, want gus", got)
	}

	newer := chartAt.Add(time.Minute)
	results := applyChartRooms(t.Context(), a, a, actor, after, newer)
	failuresOf(t, results)
	var rehomed []unitRoomResult
	for _, r := range results {
		if r.released {
			t.Errorf("#%s was released — growth declares it", r.channel)
		}
		if r.membership == chatlet.MembershipRehomed {
			rehomed = append(rehomed, r)
		}
	}
	if len(rehomed) != 1 || rehomed[0].unit != "growth" || rehomed[0].from != "marketing" {
		t.Errorf("re-homed %+v, want #mkt once, from marketing to growth", rehomed)
	}
	room := fleet.byName["mkt"]
	if room.held.unit != "growth" || !slices.Equal(handlesOf(room.members), []string{"gus"}) {
		t.Errorf("#mkt is %q's with %v, want growth's with [gus]", room.held.unit,
			handlesOf(room.members))
	}
	if fleet.creates != 1 {
		t.Errorf("the fleet made %d rooms for one channel", fleet.creates)
	}

	writes := len(a.sets)
	failuresOf(t, applyChartRooms(t.Context(), b, b, actor, after, newer))
	if len(b.sets) != 0 || len(a.sets) != writes {
		t.Errorf("a node whose rows still said marketing wrote %v", b.sets)
	}

	failuresOf(t, applyChartRooms(t.Context(), a, a, actor, before, chartAt))
	if room.held.unit != "growth" || !slices.Equal(handlesOf(room.members), []string{"gus"}) {
		t.Errorf("an older chart, applied late, moved #mkt back to %q's %v",
			room.held.unit, handlesOf(room.members))
	}

	// AND WHEN NOBODY DECLARES IT, the node whose rows still say marketing
	// empties it under the unit it IS recorded under — the release names
	// no unit, so it cannot write back the one this node read.
	nobody := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "marketing", Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "growth", Roles: []*org.Role{{Name: "gus", Kind: org.KindAgent}}},
	}}
	nobody.Normalize()
	failuresOf(t, applyChartRooms(t.Context(), b, b, actor, nobody, newer.Add(time.Minute)))
	if room.held.unit != "growth" || len(room.members) != 0 {
		t.Errorf("a stale node's release left #mkt %q's with %v, want growth's, "+
			"empty", room.held.unit, handlesOf(room.members))
	}
}

// A UNIT RENAMED WITHOUT AN ID KEEPS ITS ROOM, AND ITS PEOPLE IN IT.
//
// [org.Organization.UnitByRef] cannot cover this one — the old name is not a
// spelling of the new unit — so the room used to be read as the room of a unit
// the chart no longer has: the renamed unit refused it, and the sweep emptied
// it. It follows its channel to the renamed unit instead.
func TestAUnitRenamedWithoutAnIDKeepsItsRoom(t *testing.T) {
	t.Parallel()
	named := func(name string) *org.Organization {
		o := &org.Organization{Name: "acme", Units: []*org.Unit{{
			Name: name, Channel: "plat",
			Roles: []*org.Role{{Name: "pat", Kind: org.KindAgent}},
		}}}
		o.Normalize()
		return o
	}
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	failuresOf(t, applyChartRooms(t.Context(), node, node, actor, named("platform"), chartAt))
	results := applyChartRooms(t.Context(), node, node, actor, named("core"),
		chartAt.Add(time.Minute))
	failuresOf(t, results)
	for _, r := range results {
		if r.released {
			t.Errorf("#%s was released — the renamed unit declares it", r.channel)
		}
	}
	room := fleet.byName["plat"]
	if room.held.unit != "core" || !slices.Equal(handlesOf(room.members), []string{"pat"}) {
		t.Errorf("#plat is %q's with %v, want core's with [pat]", room.held.unit,
			handlesOf(room.members))
	}
}

// TWO UNITS DECLARING ONE CHANNEL — A STORED COMPANY'S SHAPE — MAKE ONE ROOM,
// and the second is reported as exactly that.
//
// A submitted document is refused for it (config's validateUnitRooms), but an
// admission rule lets a stored one run: the first unit the walk reaches keeps
// its room and its team, and the other is told the room is a unit's that
// declares it too, rather than "somebody else's".
func TestTwoUnitsDeclaringOneChannelLeaveOneRoomAndOneReport(t *testing.T) {
	t.Parallel()
	chart := &org.Organization{Name: "acme", Units: []*org.Unit{
		{Name: "alpha", Channel: "eng", Roles: []*org.Role{{Name: "ann", Kind: org.KindAgent}}},
		{Name: "beta", Channel: "eng", Roles: []*org.Role{{Name: "bob", Kind: org.KindAgent}}},
	}}
	chart.Normalize()
	fleet := newFleetRooms()
	node := newNodeRooms("a", fleet)
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	var reported []string
	for _, r := range applyChartRooms(t.Context(), node, node, actor, chart, chartAt) {
		if r.err != nil {
			reported = append(reported, r.unit+": "+r.err.Error())
		}
	}
	if fleet.creates != 1 || fleet.byName["eng"].held.unit != "alpha" {
		t.Fatalf("the fleet holds %d room(s), #eng belonging to %q — want one, alpha's",
			fleet.creates, fleet.byName["eng"].held.unit)
	}
	if got := handlesOf(fleet.byName["eng"].members); !slices.Equal(got, []string{"ann"}) {
		t.Errorf("#eng holds %v, want alpha's team alone", got)
	}
	if len(reported) != 1 || !strings.HasPrefix(reported[0], "beta: ") ||
		!strings.Contains(reported[0], "declares it too") {
		t.Errorf("reported %q, want beta told the room is a unit's that declares "+
			"it too", reported)
	}
}

// THE ROOM READS ARE THE ONE PIECE OF THIS RECONCILE A FAKE CANNOT COVER.
//
// Every case above runs against nodeRooms, so the SQL that feeds the real
// reconcile is never run there — and a wrong column or a lost filter would
// show up only as one `chat_unit_room_not_reconciled` line per apply, with the
// rooms the chart stopped naming quietly left staffed. So both reads run
// against the real schema here: the room that holds a name, and the live unit
// rooms the sweep walks — an archived one, which takes no message and wakes
// nobody, and a room of another kind left out.
func TestTheUnitRoomReadsRunAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	at := store.EncodeTime(chartAt)
	err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, room := range []struct {
			id, name, kind, unit string
			archived             any
		}{
			{"r-eng", "eng", string(chatlet.KindUnit), "engineering", nil},
			{"r-old", "old", string(chatlet.KindUnit), "retired", at},
			{"r-mkt", "mkt", string(chatlet.KindUnit), "marketing", nil},
			{"r-lobby", "lobby", string(chatlet.KindPublic), "", nil},
		} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_channels (id, name, name_norm, kind, unit,
				  archived_at, created_at, version, document)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1, X'7b7d')`,
				room.id, room.name, room.name, room.kind, room.unit,
				room.archived, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the rooms: %v", err)
	}
	rows := unitRoomRows{db: estate.Reader()}

	held, found, err := rows.room(t.Context(), "eng")
	if err != nil || !found {
		t.Fatalf("#eng: found=%v err=%v", found, err)
	}
	if want := (heldRoom{id: "r-eng", name: "eng", kind: string(chatlet.KindUnit),
		unit: "engineering"}); held != want {
		t.Errorf("#eng read as %+v, want %+v", held, want)
	}
	if _, found, err := rows.room(t.Context(), "nowhere"); err != nil || found {
		t.Errorf("a name nobody holds: found=%v err=%v", found, err)
	}

	live, err := rows.unitRooms(t.Context())
	if err != nil {
		t.Fatalf("unitRooms: %v", err)
	}
	var ids []string
	for _, room := range live {
		ids = append(ids, room.id)
	}
	if !slices.Equal(ids, []string{"r-eng", "r-mkt"}) {
		t.Errorf("the live unit rooms are %v, want [r-eng r-mkt] — never an "+
			"archived room or one of another kind", ids)
	}
}

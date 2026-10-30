package chatlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/configplane"
)

// A UNIT ROOM FOLLOWS ITS CHANNEL TO THE UNIT THAT DECLARES IT NOW.
//
// A room records its unit at the create, and that used to be the end of it:
// a channel moved to the unit that replaced its team in a reorg, or a unit
// renamed without an `id`, validated clean and then had its new declarer
// refused the room at ERROR on every apply of every data node — while the
// room, no longer declared by the unit it was recorded under, was emptied.
// The team lost the room its prompt names, and the name was unusable by any
// other unit for the life of the deployment. The chart's own membership write
// now carries the unit it staffs the room for, and re-homes a room recorded
// under another one, in the same record and under the same activation rules
// as the members.

// lastMemberSet is the membership the last record published, decoded.
func (r *writeRound) lastMemberSet() chatlet.MemberSet {
	r.t.Helper()
	payload, err := chatlet.DecodeMutation(r.lastRecord())
	if err != nil {
		r.t.Fatalf("decode the last record: %v", err)
	}
	set, ok := payload.(chatlet.MemberSet)
	if !ok {
		r.t.Fatalf("the last record carries a %T, not a membership", payload)
	}
	return set
}

// A NEWER CHART GIVING THE ROOM TO ANOTHER UNIT MOVES IT THERE, with that
// unit's team — and publishes it, because the room now answers to somebody
// else.
func TestAChartGivingAUnitRoomToAnotherUnitMovesItThere(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	published := r.logEnd()
	newer := chartActivation.Add(time.Minute)

	if got := r.setUnitMembers(made.Channel.ID, "platform", newer, "jane"); got !=
		chatlet.MembershipRehomed {
		t.Errorf("staffing the room for another unit answered %q, want %q",
			got, chatlet.MembershipRehomed)
	}
	if set := r.lastMemberSet(); set.Unit != "platform" {
		t.Errorf("the record re-homes to %q, want platform", set.Unit)
	}
	after := r.unitRoomState(made.Channel.ID)
	if after.unit != "platform" || !slices.Equal(after.members, []string{"jane"}) ||
		after.epoch != configplane.ActivationStamp(newer) {
		t.Errorf("after the re-home the room is %+v, want platform's [jane] at %d",
			after, configplane.ActivationStamp(newer))
	}
	if got := r.logEnd(); got != published+1 {
		t.Errorf("the re-home published %d record(s), want one", got-published)
	}

	// AND AN OLDER CHART, applied late, MOVES IT BACK NO MORE THAN IT MOVES
	// THE MEMBERS BACK.
	end := r.logEnd()
	if got := r.setUnitMembers(made.Channel.ID, "engineering", chartActivation,
		"bob"); got != chatlet.MembershipUnchanged {
		t.Errorf("an older chart naming the old unit answered %q, want %q",
			got, chatlet.MembershipUnchanged)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("an older chart published %d record(s)", got-end)
	}
	if again := r.unitRoomState(made.Channel.ID); again.unit != "platform" ||
		!slices.Equal(again.members, after.members) {
		t.Errorf("an older chart walked the room back to %+v", again)
	}

	// AND THE SAME CHART AGAIN WRITES NOTHING.
	if got := r.setUnitMembers(made.Channel.ID, "platform", newer, "jane"); got !=
		chatlet.MembershipUnchanged {
		t.Errorf("re-applying the re-home answered %q, want %q", got,
			chatlet.MembershipUnchanged)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("re-applying the re-home published %d record(s)", got-end)
	}
}

// A UNIT RENAMED WITHOUT AN ID KEEPS ITS ROOM AND ITS PEOPLE.
//
// The seats did not move, so their rows are already right; the room answers
// to the new key, which is a change a screen and the lead fallback can see,
// so it is published — and reported as a re-home rather than as a re-stamp,
// which would say nothing happened.
func TestARenamedUnitKeepsItsRoomAndItsPeople(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("platform", "platform", "bob", "jane")
	before := r.unitRoomState(made.Channel.ID)
	published := r.logEnd()
	newer := chartActivation.Add(time.Minute)

	if got := r.setUnitMembers(made.Channel.ID, "core", newer, "bob", "jane"); got !=
		chatlet.MembershipRehomed {
		t.Errorf("staffing the room for the renamed unit answered %q, want %q",
			got, chatlet.MembershipRehomed)
	}
	after := r.unitRoomState(made.Channel.ID)
	if after.unit != "core" || !slices.Equal(after.members, before.members) {
		t.Errorf("after the rename the room is %+v, want core's, with %v",
			after, before.members)
	}
	if got := r.logEnd(); got != published+1 {
		t.Errorf("a re-home over the same seats published %d record(s), want "+
			"one — the room answers to another unit now", got-published)
	}
}

// A WRITE THAT MOVES NO UNIT CARRIES NONE, so the record says exactly what
// changed: a re-stamp and a membership change under the room's own unit name
// no unit at all.
func TestAChartWriteNamesAUnitOnlyWhenItMovesOne(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	for i, seats := range [][]string{{"bob"}, {"jane"}} {
		r.setUnitMembers(made.Channel.ID, "engineering",
			chartActivation.Add(time.Duration(i+1)*time.Minute), seats...)
		if set := r.lastMemberSet(); set.Unit != "" {
			t.Errorf("a write staying with the room's own unit carries unit %q",
				set.Unit)
		}
	}
}

// THE ROOM NO UNIT DECLARES IS EMPTIED UNDER WHATEVER UNIT IT IS RECORDED
// UNDER — never under the one its caller read.
//
// The release names no unit, so a room another node re-homed between the
// caller's read and its write stays where that node put it.
func TestReleasingAUnitRoomKeepsItsUnit(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	r.setUnitMembers(made.Channel.ID, "platform", chartActivation.Add(time.Minute), "jane")

	newest := chartActivation.Add(2 * time.Minute)
	_, wrote, err := r.store.ReleaseUnitRoom(t.Context(), orgChart, made.Channel.ID, newest)
	if err != nil {
		t.Fatalf("release the room: %v", err)
	}
	r.drain()
	if wrote != chatlet.MembershipMoved {
		t.Errorf("releasing a staffed room answered %q, want %q", wrote,
			chatlet.MembershipMoved)
	}
	if set := r.lastMemberSet(); set.Unit != "" || set.Members == nil {
		t.Errorf("the release carries unit %q and members %v — it moves no "+
			"unit, and nobody is spelled by the empty list", set.Unit, set.Members)
	}
	after := r.unitRoomState(made.Channel.ID)
	if after.unit != "platform" || len(after.members) != 0 ||
		after.epoch != configplane.ActivationStamp(newest) {
		t.Errorf("after the release the room is %+v, want platform's, empty, at %d",
			after, configplane.ActivationStamp(newest))
	}
}

// ONLY THE ENGINE WRITES A UNIT ROOM'S MEMBERSHIP OR ITS UNIT, and only with a
// unit to staff it for.
//
// The create of a unit room is the chart's alone; the writes that maintain it
// were reachable by any actor a caller handed in, so a seat or a token holding
// the store could have chosen a membership — and now an owner — the chart
// decides. Refused before anything is published.
func TestOnlyTheEngineWritesAUnitRoomsMembershipOrUnit(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	made := r.unitRoom("engineering", "engineering", "jane")
	end := r.logEnd()
	newer := chartActivation.Add(time.Minute)
	for _, actor := range []chatlet.Actor{person("jane"), viaToken("jane"),
		seat("agent-cto", "run-1")} {
		if _, _, err := r.store.SetUnitMembers(t.Context(), actor, made.Channel.ID,
			"platform", members("jane"), newer); !errors.Is(err, chatlet.ErrForbidden) {
			t.Errorf("%s (%s) staffing a unit room answered %v, want %v",
				actor.Name(), actor.Kind, err, chatlet.ErrForbidden)
		}
		if _, _, err := r.store.ReleaseUnitRoom(t.Context(), actor, made.Channel.ID,
			newer); !errors.Is(err, chatlet.ErrForbidden) {
			t.Errorf("%s (%s) releasing a unit room answered %v, want %v",
				actor.Name(), actor.Kind, err, chatlet.ErrForbidden)
		}
	}
	for _, unit := range []string{"", "   "} {
		if _, _, err := r.store.SetUnitMembers(t.Context(), orgChart, made.Channel.ID,
			unit, members("jane"), newer); !errors.Is(err, chatlet.ErrInvalid) {
			t.Errorf("staffing a unit room for unit %q answered %v, want %v",
				unit, err, chatlet.ErrInvalid)
		}
	}
	if got := r.logEnd(); got != end {
		t.Errorf("the refused writes published %d record(s)", got-end)
	}
	if after := r.unitRoomState(made.Channel.ID); after.unit != "engineering" {
		t.Errorf("a refused write moved the room to %q", after.unit)
	}
	// THE CONTROL: the engine's own write gets through.
	if got := r.setUnitMembers(made.Channel.ID, "platform", newer, "jane"); got !=
		chatlet.MembershipRehomed {
		t.Errorf("the engine's re-home answered %q", got)
	}
}

// A RE-HOME IS A UNIT ROOM'S ALONE, and the record says so on every node.
func TestARehomeOfAnotherKindOfRoomIsNotARecordThisBuildWrites(t *testing.T) {
	t.Parallel()
	if err := (chatlet.MemberSet{V: 1, Unit: "platform"}).Validate(); !errors.Is(err,
		chatlet.ErrInvalid) || !strings.Contains(err.Error(), "activation") {
		t.Errorf("a re-home with no activation validated as %v", err)
	}
	h := newApplyHarness(t)
	if _, _, err := h.apply(applyCreateRoom("room-1", "launch",
		chatlet.Member{Handle: "ada"})); err != nil {
		t.Fatalf("create: %v", err)
	}
	rehome := applyRecord(chatlet.ChannelSubject("room-1"), chatlet.OpMembers,
		"op-rehome-1", chatlet.MemberSet{
			V: chatlet.DocumentVersion, Members: []chatlet.Member{{Handle: "ada"}},
			ChartEpoch: 1_772_445_600_000, Unit: "platform",
		}, chatlet.ScopeSet{Subject: true})
	if _, _, err := h.apply(rehome); err == nil {
		t.Error("a record re-homing a public room applied — it would hand a " +
			"person's room a lead to fall back to")
	}
	if got := h.scalar(`SELECT unit FROM chat_channels WHERE id = 'room-1'`); got != "" {
		t.Errorf("the public room is recorded under unit %q", got)
	}
}

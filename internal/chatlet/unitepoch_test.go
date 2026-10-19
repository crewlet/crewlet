package chatlet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/configplane"
)

// A UNIT ROOM'S MEMBERSHIP IS STAMPED WITH THE ACTIVATION IT CAME FROM, AND AN
// OLDER ONE NEVER WALKS IT BACK.
//
// Every node applies one configuration separately — at its reconcile tick and
// again at every boot — so during a rollout, and on any node that boots on a
// revision the fleet has since replaced, an OLDER chart is applied after a
// newer one. The membership write used to be a whole set with no stamp, so
// whichever apply arrived second won: a node restarting on last week's chart
// put last week's team back in the room.

// unitRoomState is what these cases compare: the stamp, the unit and who is
// in the room.
type unitRoomState struct {
	epoch   int64
	unit    string
	members []string
}

func (r *writeRound) unitRoomState(channelID string) unitRoomState {
	r.t.Helper()
	var out unitRoomState
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		var document []byte
		var column string
		if err := tx.QueryRowContext(r.t.Context(),
			`SELECT unit, document FROM chat_channels WHERE id = ?`, channelID).
			Scan(&column, &document); err != nil {
			return err
		}
		room, err := chatlet.DecodeChannel(document)
		if err != nil {
			return err
		}
		// THE COLUMN AND THE DOCUMENT ARE ONE FACT, read by different
		// readers: the reconcile's rows read the column, every screen
		// and the write path's snapshot the document.
		// Returned rather than fatal HERE: a Fatalf inside the read
		// would leave its transaction open under the next case.
		if column != room.Unit {
			return fmt.Errorf("the unit column says %q and the document %q",
				column, room.Unit)
		}
		out.epoch, out.unit = room.ChartEpoch, room.Unit
		rows, err := tx.QueryContext(r.t.Context(),
			`SELECT handle FROM chat_members WHERE channel_id = ? ORDER BY handle`,
			channelID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var handle string
			if err := rows.Scan(&handle); err != nil {
				return err
			}
			out.members = append(out.members, handle)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read unit room %s: %v", channelID, err)
	}
	return out
}

// logEnd is the log's last position, so a case can say nothing was published.
func (r *writeRound) logEnd() uint64 {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return end
}

// setUnitMembers is the chart's membership write at one activation, staffing
// the room for unit, applied, answered with what the store says it wrote.
func (r *writeRound) setUnitMembers(channelID, unit string, at time.Time,
	seats ...string) chatlet.MembershipWrite {

	r.t.Helper()
	_, wrote, err := r.store.SetUnitMembers(r.t.Context(), orgChart, channelID,
		unit, members(seats...), at)
	if err != nil {
		r.t.Fatalf("set the unit room's members at %s: %v", at, err)
	}
	if !wrote.Valid() {
		r.t.Fatalf("the store answered %q, which is none of its answers", wrote)
	}
	r.drain()
	return wrote
}

// THE CREATE CARRIES THE ACTIVATION IT CAME FROM.
func TestAUnitRoomIsStampedWithItsChartsActivation(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	if got, want := r.unitRoomState(made.Channel.ID).epoch,
		configplane.ActivationStamp(chartActivation); got != want {
		t.Errorf("the unit room is stamped %d, want its chart's activation %d",
			got, want)
	}
}

// AN OLDER CHART APPLIED LATE DECIDES NOTHING.
func TestAnOlderChartDoesNotWalkAUnitRoomBack(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	before, end := r.unitRoomState(made.Channel.ID), r.logEnd()

	if got := r.setUnitMembers(made.Channel.ID, "engineering", chartActivation.Add(-time.Second),
		"jane"); got != chatlet.MembershipUnchanged {
		t.Errorf("an older chart's membership answered %q, want %q", got,
			chatlet.MembershipUnchanged)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("an older chart published %d record(s)", got-end)
	}
	if after := r.unitRoomState(made.Channel.ID); !slices.Equal(after.members,
		before.members) || after.epoch != before.epoch {
		t.Errorf("after an older chart the room is %+v, want it unchanged at %+v",
			after, before)
	}
}

// THE SAME ACTIVATION OVER THE SAME SET WRITES NOTHING — which is every boot of
// every node on a company nobody has edited.
func TestReapplyingOneChartToAUnitRoomWritesNothing(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	end := r.logEnd()
	if got := r.setUnitMembers(made.Channel.ID, "engineering", chartActivation, "bob"); got !=
		chatlet.MembershipUnchanged {
		t.Errorf("reapplying the activation the room carries answered %q, want %q",
			got, chatlet.MembershipUnchanged)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("reapplying one activation published %d record(s) — every "+
			"restart of every node would", got-end)
	}
}

// A NEWER CHART'S SET IS WRITTEN, and its stamp with it.
func TestANewerChartMovesAUnitRoomsMembership(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	newer := chartActivation.Add(time.Minute)
	if got := r.setUnitMembers(made.Channel.ID, "engineering", newer, "jane"); got != chatlet.MembershipMoved {
		t.Errorf("a newer chart's membership answered %q, want %q", got,
			chatlet.MembershipMoved)
	}
	after := r.unitRoomState(made.Channel.ID)
	if !slices.Equal(after.members, []string{"jane"}) ||
		after.epoch != configplane.ActivationStamp(newer) {
		t.Errorf("after a newer chart the room is %+v, want [jane] at %d",
			after, configplane.ActivationStamp(newer))
	}
}

// A NEWER CHART THAT STAFFS NOBODY EMPTIES THE ROOM.
//
// The chart restates the WHOLE set, and the whole set can be nobody: a unit
// whose last seat moved away, or one that stopped naming the room. An empty
// set is a real gesture rather than the zero value's meaning ([MemberSet]), so
// it reaches the applier as one and takes the last member out — a member left
// behind with FollowAll would go on being woken by every message in a room the
// chart no longer puts it in.
func TestANewerChartThatStaffsNobodyEmptiesAUnitRoom(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob", "jane")
	newer := chartActivation.Add(time.Minute)
	// NOBODY AS A NIL SLICE, which is how a reconcile with no seats to name
	// hands it over.
	_, wrote, err := r.store.SetUnitMembers(t.Context(), orgChart, made.Channel.ID,
		"engineering", nil, newer)
	if err != nil {
		t.Fatalf("empty the room: %v", err)
	}
	r.drain()
	if wrote != chatlet.MembershipMoved {
		t.Errorf("emptying the room answered %q, want %q", wrote, chatlet.MembershipMoved)
	}
	if mutation := string(r.lastRecord().Mutation); !strings.Contains(mutation,
		`"members":[]`) {
		t.Errorf("the record emptying the room reads %s — nobody is spelled by "+
			"the empty list, never by null", mutation)
	}
	after := r.unitRoomState(made.Channel.ID)
	if len(after.members) != 0 || after.epoch != configplane.ActivationStamp(newer) {
		t.Errorf("after a chart that staffs nobody the room is %+v, want empty at %d",
			after, configplane.ActivationStamp(newer))
	}
	// AND STAYS EMPTY, writing nothing more at that activation.
	end := r.logEnd()
	if got := r.setUnitMembers(made.Channel.ID, "engineering", newer); got != chatlet.MembershipUnchanged {
		t.Errorf("emptying an empty room at its own activation answered %q", got)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("emptying an empty room published %d record(s)", got-end)
	}
}

// A NEWER ACTIVATION OVER AN UNCHANGED SET RE-STAMPS THE ROOM — and moves
// nobody, and SAYS SO.
//
// Left at the older stamp, the room is open to any activation between the two:
// a node booting on a revision activated in between would find its own stamp
// newer than the room's and write its chart back over the current one. So the
// stamp moves; and because nothing else did, the answer does not say its
// membership changed. It used to be one
// bool, true for a re-stamp, so the reconcile logged a membership change for
// every unit room on every activation, re-activating an unchanged revision
// (the credential-rotation gesture) included.
func TestANewerChartOverTheSameSetRestampsAndMovesNobody(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made := r.unitRoom("engineering", "engineering", "bob")
	before := r.unitRoomState(made.Channel.ID)
	newest := chartActivation.Add(2 * time.Minute)
	if got := r.setUnitMembers(made.Channel.ID, "engineering", newest, "bob"); got != chatlet.MembershipRestamped {
		t.Errorf("a newer activation over the same set answered %q, want %q — "+
			"no write leaves the room open to every activation between the two, "+
			"and a move reports a membership change nobody made",
			got, chatlet.MembershipRestamped)
	}
	after := r.unitRoomState(made.Channel.ID)
	if after.epoch != configplane.ActivationStamp(newest) {
		t.Errorf("the room is stamped %d after a newer chart, want %d",
			after.epoch, configplane.ActivationStamp(newest))
	}
	if !slices.Equal(after.members, before.members) {
		t.Errorf("a re-stamp moved the membership from %v to %v",
			before.members, after.members)
	}

	// AND AN ACTIVATION BETWEEN THE TWO, applied late, is now refused.
	between := chartActivation.Add(time.Minute)
	if got := r.setUnitMembers(made.Channel.ID, "engineering", between, "jane"); got != chatlet.MembershipUnchanged {
		t.Errorf("an activation between the room's two stamps answered %q over "+
			"the newer one, want %q", got, chatlet.MembershipUnchanged)
	}
	if got := r.unitRoomState(made.Channel.ID).members; !slices.Equal(got,
		before.members) {
		t.Errorf("an activation between the two moved the room to %v", got)
	}
}

// A UNIT ROOM NAMES ITS ACTIVATION, AND NO OTHER ROOM DOES.
//
// There is no honest default: a create or a membership with no activation is
// a chart nothing can compare a later one against, and a room of another kind
// carrying one would claim the chart decided a membership somebody chose.
func TestOnlyAUnitRoomNamesItsChartsActivation(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	if _, err := r.store.CreateChannel(t.Context(), orgChart, chatlet.NewChannel{
		Name: "engineering", Kind: chatlet.KindUnit, Unit: "engineering",
		Members: members("jane"),
	}); !errors.Is(err, chatlet.ErrNoActivation) {
		t.Errorf("a unit room with no activation answered %v, want %v",
			err, chatlet.ErrNoActivation)
	}
	if _, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "watercooler", Kind: chatlet.KindPublic, ActivatedAt: chartActivation,
	}); !errors.Is(err, chatlet.ErrInvalid) {
		t.Errorf("a public room naming an activation answered %v, want %v",
			err, chatlet.ErrInvalid)
	}
	made := r.unitRoom("engineering", "engineering", "jane")
	if _, _, err := r.store.SetUnitMembers(t.Context(), orgChart, made.Channel.ID,
		"engineering", members("jane"), time.Time{}); !errors.Is(err, chatlet.ErrNoActivation) {
		t.Errorf("a unit room's membership with no activation answered %v, "+
			"want %v", err, chatlet.ErrNoActivation)
	}
}

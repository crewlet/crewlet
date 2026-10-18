package engine_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
)

// unitRoomDoc is a company on Chatlet — no vendor chat surface is
// declared, so its chat derives chatlet — whose one unit declares #eng, with
// its roles left as a hole a case fills: the membership is the one thing the
// chart decides about a unit's room.
const unitRoomDoc = `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
units:
  - name: Engineering
    channel: eng
    roles:
ROLES
`

// unitRoomCompany is that company with these handles as its agent seats.
func unitRoomCompany(handles ...string) string {
	var roles strings.Builder
	for _, h := range handles {
		roles.WriteString("      - name: " + h + "\n        handle: " + h +
			"\n        llm: zulu\n")
	}
	return strings.Replace(unitRoomDoc, "ROLES\n", roles.String(), 1)
}

// unitRoom is a unit room as these cases read it: its stamp and who is in it.
type unitRoom struct {
	epoch   int64
	members []string
}

// readUnitRoom reads #name off this node's replicated rows, false where there
// is no such room.
func readUnitRoom(t *testing.T, e *engine.Engine, name string) (unitRoom, bool) {
	t.Helper()
	var out unitRoom
	err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var id string
		var document []byte
		if err := tx.QueryRowContext(t.Context(),
			`SELECT id, document FROM chat_channels WHERE name_norm = ?`, name).
			Scan(&id, &document); err != nil {
			return err
		}
		room, err := chatlet.DecodeChannel(document)
		if err != nil {
			return err
		}
		out.epoch = room.ChartEpoch
		rows, err := tx.QueryContext(t.Context(),
			`SELECT handle FROM chat_members WHERE channel_id = ? ORDER BY handle`, id)
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
	})
	if errors.Is(err, sql.ErrNoRows) {
		return unitRoom{}, false
	}
	if err != nil {
		t.Fatalf("read #%s: %v", name, err)
	}
	return out, true
}

// eventuallyUnitRoom waits for #name to reach this node with these members.
func eventuallyUnitRoom(t *testing.T, e *engine.Engine, name string,
	members ...string) unitRoom {

	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		room, found := readUnitRoom(t, e, name)
		if found && slices.Equal(room.members, members) {
			return room
		}
		if time.Now().After(deadline) {
			t.Fatalf("#%s never reached this node holding %v (found=%v, %+v) — "+
				"the chart's rooms were not applied", name, members, found, room)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A BOOT STAMPS A UNIT'S ROOM WITH THE INSTANT ITS COMPANY WAS ACTIVATED.
func TestABootStampsAUnitRoomWithItsCompanysActivation(t *testing.T) {
	t.Parallel()
	activated := time.Date(2026, 3, 2, 10, 0, 0, 250_000_000, time.UTC)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, unitRoomCompany("ada", "bob")), ActivatedAt: activated,
	})
	room := eventuallyUnitRoom(t, e, "eng", "ada", "bob")
	if want := configplane.ActivationStamp(activated); room.epoch != want {
		t.Errorf("#eng is stamped %d, want the activation's own %d", room.epoch, want)
	}
}

// A COMPANY NO ACTIVATION HAS NAMED HAS NO ROOM OPENED AT BOOT, and the
// activation that does name it is what opens and stamps it.
//
// A Tier B file a node booted with has no instant: its reconciler publishes it
// a moment later and applies it with the pointer's. A room opened at boot would
// be stamped with no activation at all, which nothing could compare the next
// chart against.
func TestAnUnactivatedCompanysUnitRoomIsOpenedByItsActivation(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, unitRoomCompany("ada", "bob")),
	})
	if room, found := readUnitRoom(t, e, "eng"); found {
		t.Fatalf("a boot with no activation opened #eng: %+v", room)
	}
	activated := time.Now().Add(-time.Minute)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, unitRoomCompany("ada", "bob")),
		activated); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	room := eventuallyUnitRoom(t, e, "eng", "ada", "bob")
	if want := configplane.ActivationStamp(activated); room.epoch != want {
		t.Errorf("#eng is stamped %d, want the activation's %d", room.epoch, want)
	}
}

// AN OLDER ACTIVATION APPLIED LATE DOES NOT WALK A UNIT ROOM'S MEMBERSHIP BACK.
//
// The membership write is the whole set, so with no stamp whichever apply
// arrived second won: a node restarting on last week's chart put last week's
// team back in the room. Stamped with the activation, the later ACTIVATION
// wins, and a newer one still moves the room.
func TestAnOlderActivationAppliedLateDoesNotWalkAUnitRoomBack(t *testing.T) {
	t.Parallel()
	newer := time.Date(2026, 3, 2, 10, 0, 1, 0, time.UTC)
	older := newer.Add(-500 * time.Millisecond)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, unitRoomCompany("ada", "bob")), ActivatedAt: newer,
	})
	eventuallyUnitRoom(t, e, "eng", "ada", "bob")

	if _, _, err := e.Apply(t.Context(), parsedCompany(t, unitRoomCompany("ada")),
		older); err != nil {
		t.Fatalf("Apply the older chart: %v", err)
	}
	room, _ := readUnitRoom(t, e, "eng")
	if !slices.Equal(room.members, []string{"ada", "bob"}) ||
		room.epoch != configplane.ActivationStamp(newer) {
		t.Errorf("after the older activation #eng is %+v, want [ada bob] at %d",
			room, configplane.ActivationStamp(newer))
	}

	newest := newer.Add(time.Minute)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, unitRoomCompany("ada")),
		newest); err != nil {
		t.Fatalf("Apply the newer chart: %v", err)
	}
	room = eventuallyUnitRoom(t, e, "eng", "ada")
	if room.epoch != configplane.ActivationStamp(newest) {
		t.Errorf("after a newer activation #eng is stamped %d, want %d",
			room.epoch, configplane.ActivationStamp(newest))
	}
}

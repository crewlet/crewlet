package memsync

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// EACH TABLE'S REPLAY READS TO THE SEAT'S BARRIER.
//
// A hydration publishes a barrier on the seat's barrier subject before its
// first table, and each table's replay reads the table's subjects and that one
// through one consumer until it has received a barrier at or past its
// hydration's, then its own end barrier.

// barrierMessage is the seat's barrier as the changelog holds it now, read
// through a consumer as a replay receives it, metadata and all.
func barrierMessage(t *testing.T, conn *nats.Conn) jetstream.Msg {
	t.Helper()
	client := jsOn(t, conn)
	consumer, err := client.CreateConsumer(t.Context(), topics.MemoryStream, jetstream.ConsumerConfig{
		FilterSubject: barrierSubject(seat.AgentID),
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatalf("open a read of the seat's barrier: %v", err)
	}
	t.Cleanup(func() {
		_ = client.DeleteConsumer(context.Background(), topics.MemoryStream, consumer.CachedInfo().Name)
	})
	if n := consumer.CachedInfo().NumPending; n != 1 {
		t.Fatalf("the changelog holds %d barriers of the seat's, want the one a hydration leaves", n)
	}
	batch, err := consumer.Fetch(1)
	if err != nil {
		t.Fatalf("read the seat's barrier: %v", err)
	}
	var barrier jetstream.Msg
	for msg := range batch.Messages() {
		barrier = msg
	}
	if err := batch.Error(); err != nil || barrier == nil {
		t.Fatalf("read the seat's barrier: %v", err)
	}
	return barrier
}

// seatMessages counts the changelog's messages on the seat's subjects: rows
// on a carried or retired table's subjects, and barriers, every other one
// however it is spelled.
func seatMessages(t *testing.T, conn *nats.Conn) (rows, barriers int) {
	t.Helper()
	stream, err := jsOn(t, conn).Stream(t.Context(), topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}
	info, err := stream.Info(t.Context(), jetstream.WithSubjectFilter(topics.MemoryPrefix+seat.AgentID+".>"))
	if err != nil {
		t.Fatalf("read the seat's subjects on the changelog: %v", err)
	}
	names := slices.Clone(retiredTables)
	for _, spec := range tables {
		names = append(names, spec.name)
	}
	for subject, n := range info.State.Subjects {
		row := slices.ContainsFunc(names, func(name string) bool {
			return topics.Match(tableFilter(seat.AgentID, name), subject)
		})
		if row {
			rows += int(n)
		} else {
			barriers += int(n)
		}
	}
	return rows, barriers
}

// A BARRIER BELOW THE HYDRATION'S DOES NOT END A REPLAY.
//
// The leader holds one barrier of the seat's, the newest hydration's, but a
// copy behind it may still hand over an earlier one before rows it has not
// applied yet; taken for the end, it would cut the replay short of the
// table's newest rows. Here the diary's copy holds an earlier hydration's
// barrier and lacks a later note and barrier until it catches up half a
// second later, on a bubble's clock over the model changelog
// (copyfake_test.go).
//
// Mutation: end a replay at any barrier, not only one at or past its
// hydration's, and this goes red: the hydration comes back without the note.
func TestABarrierBelowTheHydrationsDoesNotEndAReplay(t *testing.T) {
	t.Parallel()
	holder := openStore(t)
	seedMemory(t, holder)
	published := publishedBy(t, holder)
	fresh := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		onto(log, published)
		// THE EARLIER HYDRATION'S BARRIER, and the note written after it.
		earlier := log.publish(barrierSubject(seat.AgentID), nil)
		note := noteOfSize(t, "newest", 512)
		body, err := encode(note)
		if err != nil {
			t.Fatal(err)
		}
		log.publish(registered(t, "agent_diary").subject(seat.AgentID, note.Values), body)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{behind: true, holdsTo: earlier,
			catchUpAfter: 500 * time.Millisecond}}
		syncer := bubbleSyncer(t, fresh, copies, false)
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(tables)+1 {
			t.Fatalf("Hydrate = (%d, %v), want the seat's %d rows and the note its holder wrote "+
				"after the earlier hydration", carried, err, len(tables)+1)
		}
		diaryContent(t, syncer, "newest")
	})
}

// EVERY HYDRATION OF A SEAT LEAVES IT ONE BARRIER, AND A PUBLISH NONE.
//
// The barrier is the seat's one subject that is no row's, and the changelog
// keeps one message per subject, so a hydration's barrier replaces only the
// one before it. A publish writes none. Here a seat is published, then
// hydrated three times onto stores that never held it.
//
// Mutation: keep more than one message per subject on the changelog, and this
// goes red.
func TestEveryHydrationOfASeatLeavesItOneBarrier(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if rows, barriers := seatMessages(t, conn); rows != len(tables) || barriers != 0 {
		t.Fatalf("a publish of the seat's memory left %d rows and %d barriers on the changelog, "+
			"want its %d rows and no barrier: only a hydration writes one", rows, barriers, len(tables))
	}
	for hydration := 1; hydration <= 3; hydration++ {
		carried, err := syncerOn(t, openStore(t), conn).Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(tables) {
			t.Fatalf("hydration %d = (%d, %v), want the seat's %d rows", hydration, carried, err, len(tables))
		}
		if rows, barriers := seatMessages(t, conn); rows != len(tables) || barriers != 1 {
			t.Fatalf("after hydration %d the changelog holds %d rows and %d barriers of the seat's, "+
				"want its %d rows and one barrier", hydration, rows, barriers, len(tables))
		}
	}
}

// TWO HYDRATIONS OF A SEAT RACING EACH OTHER BOTH END.
//
// A seat can be hydrated on two nodes at once while it moves. The later
// barrier replaces the earlier on the seat's one subject, so the earlier
// hydration's replays never see their own again: they end at a barrier at or
// past their hydration's, the later one. Here a second hydration runs whole
// between the first's barrier and its first pull.
//
// Mutation: end a replay only at its own hydration's barrier, and this goes
// red: the first hydration waits for a barrier the changelog no longer holds.
func TestTwoHydrationsOfASeatRacingEachOtherBothEnd(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	first := syncerOn(t, openStore(t), conn)
	second := syncerOn(t, openStore(t), conn)
	var later struct {
		carried int
		err     error
	}
	newSpyCopies(first).beforeAsk = func(filter string, _ replayPulls) {
		if filter == tableFilter(seat.AgentID, tables[0].name) {
			later.carried, later.err = second.Hydrate(t.Context(), seat.Handle)
		}
	}
	carried, err := first.Hydrate(t.Context(), seat.Handle)
	if later.err != nil || later.carried != len(tables) {
		t.Fatalf("the later hydration = (%d, %v), want the seat's %d rows", later.carried, later.err,
			len(tables))
	}
	if err != nil || carried != len(tables) {
		t.Fatalf("the earlier hydration, whose barrier the later one replaced, = (%d, %v), want "+
			"the seat's %d rows", carried, err, len(tables))
	}
	if _, barriers := seatMessages(t, conn); barriers != 1 {
		t.Errorf("the changelog holds %d barriers of the seat's after two hydrations, want one", barriers)
	}
}

// A BARRIER PUSHES NO ROW OUT.
//
// The changelog's discard policy is old, so under any limit but its one
// message per subject a full changelog would drop its oldest row to take a
// barrier. As the queue provisions it there is no age, byte, message-count or
// message-size limit, and the per-subject limit drops the older message,
// never refusing the newer.
//
// Mutation: give the changelog a byte limit, and this goes red.
func TestABarrierPushesNoRowOut(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	stream, err := jsOn(t, conn).Stream(t.Context(), topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.MaxMsgsPerSubject != 1 || cfg.DiscardNewPerSubject {
		t.Errorf("the changelog keeps %d messages per subject (discard new per subject %t), want "+
			"one, the older dropped as a newer lands", cfg.MaxMsgsPerSubject, cfg.DiscardNewPerSubject)
	}
	if cfg.MaxAge != 0 || cfg.MaxBytes > 0 || cfg.MaxMsgs > 0 || cfg.MaxMsgSize > 0 {
		t.Errorf("the changelog holds messages to an age of %v, %d bytes, %d messages and %d bytes "+
			"a message, want no limit but the one per subject: under any other its discard policy, "+
			"%v, drops a row to take a barrier", cfg.MaxAge, cfg.MaxBytes, cfg.MaxMsgs, cfg.MaxMsgSize,
			cfg.Discard)
	}
}

// THE BARRIER IS NEVER A ROW.
//
// A hydration's barrier is a message of no bytes on the seat's one subject
// that is no table's, and every table's replay receives it. It is read for
// its sequence alone: never decoded (no bytes are no row), written or counted
// in what the hydration reports.
//
// Mutation: read the barrier as a row, or count the messages a pull delivered
// rather than the rows its transaction wrote, and this goes red.
func TestTheBarrierIsNeverARow(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fresh := openStore(t)
	carried, err := syncerOn(t, fresh, conn).Hydrate(t.Context(), seat.Handle)
	if err != nil || carried != len(tables) {
		t.Fatalf("Hydrate = (%d, %v), want the seat's %d rows and nothing for its barrier",
			carried, err, len(tables))
	}
	for _, spec := range tables {
		if got := countRows(t, fresh, spec.name); got != 1 {
			t.Errorf("%s holds %d rows after the hydration, want the seat's 1", spec.name, got)
		}
	}
	if barrier := barrierMessage(t, conn); len(barrier.Data()) != 0 {
		t.Errorf("the seat's barrier carries %d bytes, want none", len(barrier.Data()))
	}
}

// A ROW WRITTEN PAST THE BARRIER IS READ TOO.
//
// A replay ends at its own end barrier after the hydration's, never at the
// hydration's alone: a racing publish (a releasing node's last flush) writes
// rows past the barrier, and every row stored before the end barrier is read,
// whatever the copy's count said as the barrier arrived. Here a note is
// published as the diary's replay sends its first pull. A pull that delivered
// only a barrier still delivered a message, not the end of the table.
//
// Mutation: end a replay at its barrier, or take a pull that delivered no row
// for one that delivered nothing, and this goes red.
func TestARowWrittenPastTheBarrierIsReadToo(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	note := noteOfSize(t, "racing", 512)

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	newSpyCopies(syncer).beforeAsk = func(filter string, _ replayPulls) {
		if filter == tableFilter(seat.AgentID, "agent_diary") {
			publishOn(t, conn, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)
		}
	}
	carried, err := syncer.Hydrate(t.Context(), seat.Handle)
	if err != nil || carried != 1 {
		t.Fatalf("Hydrate = (%d, %v), want the note published past its barrier", carried, err)
	}
	if got := countRows(t, fresh, "agent_diary"); got != 1 {
		t.Errorf("the diary holds %d notes after the hydration, want the racing one", got)
	}
}

// THE BARRIER'S TOKEN IS NO TABLE'S NAME, AND NONE CAN EVER TAKE IT.
//
// The barrier is crewlet.memory.<seat>.<token>: four tokens where a row's
// subject has five, so no table's filter matches it and a replay's two
// filters never overlap, as nats-server requires of a consumer's. The token
// is no SQL identifier, which every memory table's name is (written unquoted
// into its statements). Held for every carried and retired table, whose
// filter a retired table's count and purge take, so neither reaches the
// barrier.
//
// Mutation: name the token as a table could be named, and this goes red.
func TestTheBarrierTokenIsNoTablesName(t *testing.T) {
	t.Parallel()
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	token := topics.MemoryBarrierToken
	if identifier.MatchString(token) {
		t.Errorf("the barrier's token %q is a SQL identifier, which a memory table could be named", token)
	}
	if token == "" || strings.ContainsAny(token, ".*> \t") {
		t.Errorf("the barrier's token %q is not one token of a subject", token)
	}
	names := slices.Clone(retiredTables)
	for _, spec := range tables {
		names = append(names, spec.name)
	}
	barrier := barrierSubject(seat.AgentID)
	for _, name := range names {
		if !identifier.MatchString(name) {
			t.Errorf("the memory table %q is no SQL identifier, which is what keeps the barrier's "+
				"token from ever being a table's name", name)
		}
		if name == token {
			t.Errorf("the memory table %q is named as the barrier's token", name)
		}
		if topics.Match(tableFilter(seat.AgentID, name), barrier) {
			t.Errorf("%s's filter %s matches the seat's barrier %s", name,
				tableFilter(seat.AgentID, name), barrier)
		}
	}
}

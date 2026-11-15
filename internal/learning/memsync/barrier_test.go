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
// through one consumer, until it has received a barrier at or past its
// hydration's, and then its own end barrier. The cases below hold what a
// replay does with a barrier, on the package's broker.

// barrierMessage is the seat's barrier as the changelog holds it now, read
// through a consumer as a replay delivers it, metadata and all.
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

// seatMessages is what the changelog holds on the seat's subjects: its rows,
// the messages on a carried or a retired table's subjects, and every other
// message of the seat's, which is a barrier however it is spelled.
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
// The changelog keeps one message per subject, so its leader holds one barrier
// of the seat's, the newest hydration's. A copy behind the leader may still
// hold an earlier hydration's, and hands it over before the rows past it that
// the copy has not applied yet: taken for the end, it ended the replay short
// of those rows, the table's newest. A replay ends only at a barrier at or
// past the one its own hydration published, so it goes past an earlier one,
// waits for its copy, and carries the rest. Here an earlier hydration of the
// seat left its barrier on the changelog, the seat's holder then wrote a note,
// and a later hydration's copy of the diary holds the earlier barrier and
// lacks the note and the later barrier until it catches up, half a second
// later, played on a bubble's clock over the model changelog
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
		// THE EARLIER HYDRATION'S BARRIER, and the note the seat's holder
		// wrote after it.
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
// keeps one message per subject, so a hydration's barrier replaces the one
// before it and takes nothing else's place: however often a seat is hydrated,
// the changelog holds one barrier of it and every row it held. A publish of
// the seat's memory writes none. Here a seat is published and then hydrated
// three times, each time onto a store that never held it.
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
// A seat can be hydrated on two nodes at once while it moves
// (crewlet/crewlet#511's window). Each publishes a barrier to the seat's one
// subject, so the later replaces the earlier, and the earlier hydration's copy
// then holds the later barrier and never its own again: its replays end at a
// barrier at or past their hydration's, so they end at the later one, as the
// later hydration's do, and the changelog holds one barrier of the seat. Here
// a second hydration of the seat runs whole between the first's barrier and
// its first pull.
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
// A hydration's barrier is one more message on the changelog, and the
// changelog's discard policy is old: under any limit but its one message per
// subject, a full changelog would drop its oldest row to take the barrier. It
// has no other, as the queue provisions it: no age, no byte, message-count or
// message-size limit, and the one per subject dropping only the older of two
// messages on one subject, never refusing the newer, so a barrier replaces the
// seat's previous barrier and nothing else. Read here off the changelog the
// package's broker provisions, as the queue provisions it.
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
// that is no table's, and every table's replay receives it. It is read for its
// sequence alone: never decoded — no bytes are no row — never written, and
// never counted in what the hydration reports. Here a seat with one row in
// every table hydrates onto a store that never held it: the hydration reports
// the seat's rows and no more, the store holds them and no more, and the
// changelog holds the barrier beside them, of no bytes.
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
// A replay is done once it has received a barrier at or past its hydration's
// and then its own end barrier, never at the barrier alone: a publish racing
// the hydration — a releasing node's last flush still landing, say — writes
// rows past the barrier, and every row stored before the end barrier lies
// below it, so the replay reads it before it ends, whatever the copy's count
// said as the barrier arrived. Here a seat with nothing on the changelog has
// a note published as the diary's replay sends its first pull, after its
// barrier. A pull that delivered a barrier alone delivered a message, so it is
// no row past the pull's bytes either.
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
// The barrier is the seat's one subject that is no row's,
// crewlet.memory.<seat>.<token>: four tokens where a row's subject has five,
// so no table's filter matches it and a replay's two filters never overlap,
// as nats-server requires of a consumer's. Its token is no name a table could
// take, either: a memory table's name is a SQL identifier, written unquoted
// into every statement that reads or writes it, and the token is none. Held
// here for every table this build carries and every one it has retired, whose
// filter is the one a retired table's count and purge take, so neither ever
// reaches the barrier.
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

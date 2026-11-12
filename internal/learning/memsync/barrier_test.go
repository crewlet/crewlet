package memsync

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
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
// hydration's and its copy holds nothing more. The cases below hold what a
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
// lacks the note and the later barrier until it catches up. The later barrier
// replaces the earlier on the changelog, which holds one of the seat's after
// the two hydrations.
//
// Mutation: end a replay at any barrier, not only one at or past its
// hydration's, and this goes red: the hydration comes back without the note.
func TestABarrierBelowTheHydrationsDoesNotEndAReplay(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := syncerOn(t, openStore(t), conn).Hydrate(t.Context(), seat.Handle); err != nil {
		t.Fatalf("the earlier hydration: %v", err)
	}
	earlier := barrierMessage(t, conn)
	note := noteOfSize(t, "newest", 512)
	publishOn(t, conn, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	syncer.js = laggingJS{JetStream: syncer.js, behind: 2, catchesUp: true, stale: earlier}
	carried, err := syncer.Hydrate(t.Context(), seat.Handle)
	if err != nil || carried != len(tables)+1 {
		t.Fatalf("Hydrate = (%d, %v), want the seat's %d rows and the note its holder wrote after "+
			"the earlier hydration", carried, err, len(tables)+1)
	}
	var content string
	if err := fresh.SQL().QueryRowContext(t.Context(),
		"SELECT content FROM agent_diary WHERE id = 'newest'").Scan(&content); err != nil {
		t.Fatalf("the note written after the earlier hydration did not arrive: %v", err)
	}
	if _, barriers := seatMessages(t, conn); barriers != 1 {
		t.Errorf("the changelog holds %d barriers of the seat's after two hydrations, want the "+
			"later one alone", barriers)
	}
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
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
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
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	first := syncerOn(t, openStore(t), conn)
	second := syncerOn(t, openStore(t), conn)
	var later struct {
		carried int
		err     error
	}
	first.js = racingJS{JetStream: first.js, filter: tableFilter(seat.AgentID, tables[0].name),
		race: func(jetstream.Consumer) {
			later.carried, later.err = second.Hydrate(t.Context(), seat.Handle)
		}}
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
	if _, err := syncerOn(t, holder, conn).Publish(t.Context(), seat.Handle); err != nil {
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

// racingJS is a JetStream whose replay of one table, the one filter names,
// runs race before its first pull, handed that replay's consumer: a publish or
// a hydration racing the hydration, landing after its barrier.
type racingJS struct {
	jetstream.JetStream
	filter string
	race   func(replay jetstream.Consumer)
}

func (j racingJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	consumer, err := j.JetStream.CreateConsumer(ctx, stream, cfg)
	if err != nil || !slices.Contains(cfg.FilterSubjects, j.filter) {
		return consumer, err
	}
	return &racingConsumer{Consumer: consumer, race: j.race}, nil
}

type racingConsumer struct {
	jetstream.Consumer
	race func(replay jetstream.Consumer)
	once sync.Once
}

func (c *racingConsumer) Fetch(batch int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	c.once.Do(func() { c.race(c.Consumer) })
	return c.Consumer.Fetch(batch, opts...)
}

// counted waits until the copy serving replay counts want messages for it.
//
// The server counts a message for a consumer a moment after it has
// acknowledged it, from the queue it signals consumers through, and a replay
// reads only what its copy has counted by the time a barrier at or past its
// hydration's says the copy holds nothing more ([Syncer.replay], "What the
// copy's count can miss").
func counted(t *testing.T, replay jetstream.Consumer, want uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var last uint64
	for {
		info, err := replay.Info(ctx)
		if err == nil {
			if last = info.NumPending; last == want {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the replay's copy counted %d messages for it, never %d: %v", last, want, err)
		case <-time.After(10 * time.Millisecond): //fixedwait:liveness
		}
	}
}

// A ROW WRITTEN PAST THE BARRIER IS READ TOO.
//
// A replay is done once it has received a barrier at or past its hydration's
// AND its copy holds nothing more, never at the barrier alone: a publish
// racing the hydration — a releasing node's last flush still landing, say —
// writes rows past the barrier, and a copy that has counted one by the time it
// hands the barrier over is read to it. Here a seat with nothing on the
// changelog has a note published as the diary's replay sends its first pull,
// for which the reply that created the replay counted the barrier alone: the
// case waits for the copy to count the note too, as it does a moment after
// acknowledging it, and then the pull hands over the barrier, which says one
// more message follows, and the replay pulls the note. A note the copy has not
// counted by the time it hands the barrier over is left for the seat's next
// hydration ([Syncer.replay]). A pull that delivered the barrier alone
// delivered a message, so it is no row past the pull's bytes either.
//
// Mutation: end a replay at its barrier whatever its copy still holds, or take
// a pull that delivered no row for one that delivered nothing, and this goes
// red.
func TestARowWrittenPastTheBarrierIsReadToo(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	note := noteOfSize(t, "racing", 512)

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	syncer.js = racingJS{JetStream: syncer.js, filter: tableFilter(seat.AgentID, "agent_diary"),
		race: func(replay jetstream.Consumer) {
			publishOn(t, conn, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)
			counted(t, replay, 2)
		}}
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

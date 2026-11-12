package memsync

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// otherSeat is a second seat's id on the same changelog.
const otherSeat = "99999999-2222-3333-4444-555555555555"

// publishRows puts n rows of table on agent's subjects, as a build that
// carried the table wrote them.
func publishRows(t *testing.T, conn *nats.Conn, agent, table string, n int) {
	t.Helper()
	client := jsOn(t, conn)
	for i := range n {
		body, err := json.Marshal(Row{Table: table, Values: map[string]any{"id": fmt.Sprintf("r%d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		subject := fmt.Sprintf("%s%s.%s.%032x", topics.MemoryPrefix, agent, table, i)
		if _, err := client.Publish(context.Background(), subject, body); err != nil {
			t.Fatalf("publish a %s row: %v", table, err)
		}
	}
}

// rowsOf is how many rows of table the changelog holds for agent.
func rowsOf(t *testing.T, conn *nats.Conn, agent, table string) int {
	t.Helper()
	ctx := context.Background()
	stream, err := jsOn(t, conn).Stream(ctx, topics.MemoryStream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(topics.MemoryPrefix+agent+"."+table+".>"))
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	total := 0
	for _, n := range info.State.Subjects {
		total += int(n)
	}
	return total
}

// A RETIRED TABLE'S ROWS ARE PURGED BY THE FIRST HYDRATION THAT FINDS THEM, AND
// ONLY THE SEAT'S. They were replayed and warned about once per row at every
// hydration because nothing republishes or replaces them; one hydration now
// takes them off the changelog for every node, so the next, here or on another
// node, finds none and has nothing to say. Another seat's rows wait for its own
// hydration, and a newer build's table is never read, and never purged.
//
// Mutation: skip the purge, purge by the table alone, or purge a newer build's
// table, and this goes red.
func TestARetiredTablesRowsArePurgedOnceForTheSeat(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	owner := openStore(t)
	seedMemory(t, owner)
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	publishRows(t, conn, seat.AgentID, "conversation_sessions", 120)
	publishRows(t, conn, otherSeat, "conversation_sessions", 7)
	publishRows(t, conn, seat.AgentID, "a_newer_builds_table", 3)

	for node := range 2 {
		carried, err := syncerOn(t, openStore(t), conn).Hydrate(ctx, seat.Handle)
		if err != nil || carried != len(tables) {
			t.Fatalf("hydration %d carried %d rows (%v), want the seat's %d", node, carried, err, len(tables))
		}
		if got := rowsOf(t, conn, seat.AgentID, "conversation_sessions"); got != 0 {
			t.Fatalf("after hydration %d the seat's retired rows are still %d", node, got)
		}
	}
	if got := rowsOf(t, conn, otherSeat, "conversation_sessions"); got != 7 {
		t.Errorf("another seat's retired rows are %d after this seat's hydration, want its 7", got)
	}
	if got := rowsOf(t, conn, seat.AgentID, "a_newer_builds_table"); got != 3 {
		t.Errorf("a newer build's rows are %d after the hydration, want all 3 kept", got)
	}
	for _, spec := range tables {
		if got := rowsOf(t, conn, seat.AgentID, spec.name); got != 1 {
			t.Errorf("the seat's %s rows are %d after the purge, want its 1", spec.name, got)
		}
	}
}

// A RETIRED TABLE IS ONE THIS BUILD NO LONGER CARRIES: a table both carried and
// retired would be purged by every hydration after its own publish.
//
// Mutation: list a carried table as retired, and this goes red.
func TestNoCarriedTableIsRetired(t *testing.T) {
	t.Parallel()
	for _, spec := range tables {
		if slices.Contains(retiredTables, spec.name) {
			t.Errorf("%s is both carried and retired", spec.name)
		}
	}
}

// A NOTE'S CLASS AND ITS BUILD TRAVEL WITH IT, so the node that takes the seat
// holds back what the node that wrote it would have.
//
// Mutation: leave either column out of the carried row, and this goes red.
func TestANotesClassAndBuildTravelWithIt(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	owner := openStore(t)
	if _, err := owner.SQL().ExecContext(ctx, `INSERT INTO agent_diary (id, agent_id, kind, content,
		created_at, provenance, engine_build) VALUES ('d9', ?, 'diary_long', 'my box has no browser', 1,
		'capability', 'v1')`, seat.AgentID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	taker := openStore(t)
	if _, err := syncerOn(t, taker, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	var about, build string
	if err := taker.SQL().QueryRowContext(ctx,
		`SELECT provenance, engine_build FROM agent_diary WHERE id = 'd9'`).Scan(&about, &build); err != nil {
		t.Fatalf("read the carried note: %v", err)
	}
	if about != "capability" || build != "v1" {
		t.Errorf("the carried note is about %q by build %q, want capability by v1", about, build)
	}
}

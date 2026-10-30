package chatlet_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTION IS WRITTEN TO THIS LOG BY ITS OWN GATE WRITER, AND READ BACK FROM
// THIS LOG'S OWN ROWS — install, lift, and install again.
//
// This domain had an applier, a fence and a table for an eviction and no way to
// write one: nothing could fill `chat_evictions`, the chat applier never
// dropped an evicted node's records, and the trim never stopped counting the
// node on this log — which grew toward the ceiling past which a message
// somebody typed is refused. [chatlet.GateWriter] writes it now, and
// [chatlet.Domain.Evictions] is what the trim reads, so what that answers after
// each step is the contract.
func TestAnEvictionIsWrittenToThisLogAndReadBackFromIt(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	operator := chatlet.Actor{Handle: "founder", Kind: chatlet.AuthorOperator,
		OperatorID: "founder"}
	// AND THE SAME STANDING READ OFF THE LOG ITSELF ([statelog.EvictedOnLog]),
	// which is how a node a peer re-anchored past sees an eviction its
	// stopped applier never reaches.
	onLog := func(want bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), chatlet.Domain{},
			chatlet.Domain{}.Stream(), statelog.StandingOf(r.log, r.verifier), "node-b")
		if err != nil || !found || evicted != want {
			t.Fatalf("node-b's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", evicted, found, err, want)
		}
	}

	res, err := r.gate.EvictNode(t.Context(), operator, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	if res.Position.Seq == 0 || res.OpID != "op-evict" {
		t.Fatalf("the eviction answered %+v — a gate write reports where it "+
			"landed and under which operation, or a partial gesture could not "+
			"be retried", res)
	}
	r.drain()
	row, held := evictionOf(t, r, "node-b")
	switch {
	case !held:
		t.Fatal("the eviction landed and this log's rows hold no row for the node")
	case row.Back:
		t.Fatalf("node-b reads as back straight after its eviction: %+v", row)
	case row.From != uint64(res.Position.Packed()):
		t.Fatalf("the eviction takes effect above %d, want its own position %d — "+
			"the gate drops records by comparing against exactly this",
			row.From, res.Position.Packed())
	case row.By != operator.Name():
		t.Fatalf("the eviction was run by %q, want %q", row.By, operator.Name())
	case row.At.IsZero():
		t.Fatal("the eviction carries no instant, and the fence window is " +
			"measured from it")
	}
	onLog(true)

	// AND THE GATE IT INSTALLED DROPS WHAT THE NODE SAYS: a post node-b
	// publishes into a room after its eviction lands on the log and applies
	// nowhere, while the same room goes on taking everybody else's.
	room := r.room(person("jane"), "launch")
	r.peerRecord(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			OpID: "op-evicted-post", Subject: chatlet.MessageSubject(room.Channel.ID),
			Op: chatlet.OpPost, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer: "node-b", Scope: chatlet.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(t, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: "message-from-node-b",
			Body: "said by a node the fleet evicted", Author: "jane",
			AuthorKind: chatlet.AuthorHuman,
		}),
		Actor: "jane", ActorKind: chatlet.AuthorHuman,
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?`,
		"message-from-node-b"); got != 0 {
		t.Fatal("an evicted node's post applied — the gate its eviction " +
			"installed drops nothing")
	}
	r.say(person("jane"), room.Channel.ID, "still here", "submit-1")

	back, err := r.gate.ReadmitNode(t.Context(), operator, "op-back", "node-b")
	if err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	if row, held = evictionOf(t, r, "node-b"); !held || !row.Back ||
		row.Readmitted != uint64(back.Position.Packed()) {
		t.Fatalf("node-b after its readmission reads %+v (held %v), want its row "+
			"kept and back at %d — a readmission is an inverse commit, not a "+
			"delete", row, held, back.Position.Packed())
	}
	onLog(false)

	if _, err := r.gate.EvictNode(t.Context(), operator, "op-again", "node-b"); err != nil {
		t.Fatalf("EvictNode again: %v", err)
	}
	r.drain()
	if row, held = evictionOf(t, r, "node-b"); !held || row.Back {
		t.Fatalf("node-b evicted a second time reads %+v (held %v) — a "+
			"re-eviction clears the readmission", row, held)
	}
	onLog(true)
}

// A GATE WRITE REFUSES WHAT IT CANNOT MEAN, before it forms a record.
func TestAnEvictionNamingNothingIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	operator := chatlet.Actor{Handle: "founder", Kind: chatlet.AuthorOperator,
		OperatorID: "founder"}
	if _, err := r.gate.EvictNode(t.Context(), operator, "op", ""); err == nil {
		t.Fatal("an eviction naming no node was written")
	}
	if _, err := r.gate.EvictNode(t.Context(), operator, "", "node-b"); err == nil {
		t.Fatal("an eviction with no operation id was written — a retry of it " +
			"could never be told from a second eviction")
	}
	if last, err := r.log.End(t.Context()); err != nil || last != 0 {
		t.Fatalf("the log's end is %d (err %v) after two refused gate writes", last, err)
	}
}

// A RETRIED GATE WRITE IS ANSWERED BY WHAT ITS RECORD STILL MEANS.
//
// The ledger answers a retry of a gate operation before any decision runs, and
// on this log that answer is only true while the record the operation landed
// is still in force: an eviction retried after the node was readmitted would
// otherwise be told "applied" about a gate that no longer holds, and the
// operator who retried it would believe the node is out. So the retry is judged
// against the node's own row in the same snapshot ([statelog.GateStanding]) —
// still standing is the operation's own answer, collapsed onto its first
// position; superseded is refused by name, and nothing is appended either way.
func TestARetriedGateWriteIsAnsweredByWhatItsRecordStillMeans(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	operator := chatlet.Actor{Handle: "founder", Kind: chatlet.AuthorOperator,
		OperatorID: "founder"}
	end := func() uint64 {
		t.Helper()
		last, err := r.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		return last
	}

	evicted, err := r.gate.EvictNode(t.Context(), operator, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	r.drain()
	appended := end()
	again, err := r.gate.EvictNode(t.Context(), operator, "op-evict", "node-b")
	switch {
	case err != nil:
		t.Fatalf("a retry of a standing eviction was refused: %v", err)
	case again.Outcome != statelog.OutcomeApplied || !again.Collapsed ||
		again.Position != evicted.Position:
		t.Fatalf("a retry of a standing eviction answered %+v, want applied and "+
			"collapsed at its first position %s", again, evicted.Position)
	case end() != appended:
		t.Fatal("a retry of a standing eviction appended a second record")
	}

	if _, err := r.gate.ReadmitNode(t.Context(), operator, "op-readmit", "node-b"); err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	appended = end()

	_, err = r.gate.EvictNode(t.Context(), operator, "op-evict", "node-b")
	var unavailable *statelog.Unavailable
	switch {
	case !errors.As(err, &unavailable):
		t.Fatalf("an eviction retried after the node's readmission answered %v — "+
			"its record no longer holds, so it is refused rather than reported "+
			"applied", err)
	case unavailable.Reason != statelog.ReasonSuperseded:
		t.Fatalf("an eviction retried after the node's readmission was refused "+
			"as %q, want %q", unavailable.Reason, statelog.ReasonSuperseded)
	case end() != appended:
		t.Fatal("a superseded retry appended a record")
	}
	if row, held := evictionOf(t, r, "node-b"); !held || !row.Back {
		t.Fatalf("node-b after a superseded retry reads %+v (held %v) — the "+
			"readmission stands", row, held)
	}
}

// THE GATE WRITER NEEDS A PUBLISHER, AND NOTHING ELSE.
//
// Refused by name without one, because a writer with none validates an eviction
// and writes it nowhere — and built with nothing BUT one, because nothing else
// is needed: an eviction resolves no recipient, and the standing a retry is
// judged by is read in the framework's own snapshot transaction
// ([chatlet.GateOptions.Publisher]). The engine's node gate has no roster to hand
// it, which is why the writer cannot be the room store.
func TestTheGateWriterNeedsAPublisherAndNothingElse(t *testing.T) {
	t.Parallel()
	if _, err := chatlet.NewGateWriter(chatlet.GateOptions{}); err == nil {
		t.Fatal("a gate writer with no publisher was built")
	}
}

// THE AUTHOR KINDS ARE THE IDENTITY DIRECTORY'S OWN WORDS, one for one.
//
// The engine's node gate records whoever pressed it on this log by taking the
// kind [iam.ActorFor] gives the principal as an author kind unchanged, as the
// tracker's and the wiki's logs take theirs, and [chatlet.AuthorKind] promises
// the four are those words. Nothing else ties the two lists: a kind the directory
// gained and this domain did not would be refused by every eviction such a
// principal made here while the other logs took it, and a kind this domain
// spelled its own way would record one party as two in the audit feed the
// logs are read side by side in. Mutation: drop a kind from
// [chatlet.AuthorKinds], or respell one, and this names it.
func TestTheAuthorKindsAreTheIdentityDirectorysWords(t *testing.T) {
	t.Parallel()
	var directory, authors []string
	for _, kind := range iam.ActorKinds {
		directory = append(directory, string(kind))
	}
	for _, kind := range chatlet.AuthorKinds() {
		authors = append(authors, string(kind))
	}
	slices.Sort(directory)
	slices.Sort(authors)
	if !slices.Equal(directory, authors) {
		t.Fatalf("chat's author kinds are %q and the identity directory's %q — "+
			"the node gate records a principal's kind on this log as it finds it",
			authors, directory)
	}
}

// evictionOf is one node's eviction row as the trim reads it.
func evictionOf(t *testing.T, r *writeRound, node string) (statelog.EvictionRow, bool) {
	t.Helper()
	rows, err := chatlet.Domain{}.Evictions(t.Context(), r.db.Reader())
	if err != nil {
		t.Fatalf("read the evictions: %v", err)
	}
	for _, row := range rows {
		if row.NodeID == node {
			return row, true
		}
	}
	return statelog.EvictionRow{}, false
}

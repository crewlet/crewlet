package chatlet_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A REANCHOR'S RECORD LANDS WHERE A READER ASKS FOR IT, AND APPLIES AS THE
// AUDIT ROW IT CLAIMS TO BE.
//
// The encoder is the one step of a reanchor only this domain can take, and both
// of its halves are read by somebody who never sees the other. The SUBJECT is
// what [statelog.GenerationOpeners] asks for to learn who opened a generation —
// a record published anywhere else is a generation nobody can attribute, and
// the transition that reads back whose record landed reads nothing. The
// PAYLOAD is what every node's applier turns into `chat_log_generations` — a
// high-water mark or a stream instant lost on the way is an audit row that
// names the wrong log the fleet walked away from. The framework's suite builds
// its own generation record, so nothing but this case holds the encoder the
// engine actually calls to either.
func TestTheGenerationRecordLandsOnItsSubjectAndAppliesAsItsAuditRow(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	created := time.Date(2031, 3, 30, 9, 0, 0, 0, time.UTC)
	facts := statelog.GenerationFacts{
		Generation: 3, By: "tok-1", Writer: "node-a",
		At: time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC),
		Inputs: statelog.ReanchorInputs{
			Stream: topics.ChatLogStream, StreamCreatedAt: created, Highest: 4242,
		},
	}
	enc := chatlet.GenerationRecord{}
	rec, kept, err := enc.GenerationRecord(facts)
	if err != nil || !kept {
		t.Fatalf("encode generation 3: %v (kept: %v)", err, kept)
	}
	want, keeps := enc.GenerationSubject(facts.Generation)
	if !keeps {
		t.Fatal("this domain keeps a generation record and names no subject for it")
	}
	if rec.Subject != want {
		t.Fatalf("generation 3's record is published on %s, and a reader asking "+
			"who opened it asks %s", rec.Subject, want)
	}
	env, err := chatlet.Domain{}.Envelope(rec.Payload)
	switch {
	case err != nil:
		t.Fatalf("the record's envelope does not decode: %v", err)
	case env.Subject != want:
		t.Fatalf("the record says it is on %s and was published on %s — the "+
			"applier files it under the first", env.Subject, want)
	case env.OpID != rec.OpID:
		t.Fatalf("the record carries operation %q and was published as %q",
			env.OpID, rec.OpID)
	case env.Gen != facts.Generation || env.Writer != facts.Writer:
		t.Fatalf("the record is stamped generation %d by %q, want %d by %q",
			env.Gen, env.Writer, facts.Generation, facts.Writer)
	}

	stored := time.Date(2031, 4, 2, 3, 0, 1, 0, time.UTC)
	record := statelog.Record{
		Envelope: env,
		Position: statelog.Position{
			Stream: topics.ChatLogStream, Generation: facts.Generation, Seq: 1,
		},
		Payload: rec.Payload, StoredAt: stored,
	}
	if err := h.db.Tx(t.Context(), func(tx *sql.Tx) error {
		reason, gated, err := h.applier.Gated(t.Context(), tx, record)
		if err != nil {
			return err
		}
		if gated {
			t.Fatalf("the generation record was gated as %q", reason)
		}
		_, err = h.applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{
			Now: stored, StoredAt: stored, MaxVariables: h.maxVariables,
		})
		return err
	}); err != nil {
		t.Fatalf("apply the generation record: %v", err)
	}

	var row struct {
		generation              uint32
		at, streamCreated, prev int64
		by, recordID            string
	}
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT generation, at, by, new_stream_created_at, prev_last_seq_seen,
			       record_id
			FROM chat_log_generations`).Scan(&row.generation, &row.at, &row.by,
			&row.streamCreated, &row.prev, &row.recordID)
	}); err != nil {
		t.Fatalf("read the generation's audit row: %v", err)
	}
	switch {
	case row.generation != facts.Generation:
		t.Errorf("the audit row is generation %d, want %d", row.generation,
			facts.Generation)
	case row.by != facts.By:
		t.Errorf("the audit row names %q as the operator, want %q", row.by, facts.By)
	case uint64(row.prev) != facts.Inputs.Highest:
		t.Errorf("the audit row's previous high-water mark is %d, want %d — "+
			"the number the old generation ended at", row.prev, facts.Inputs.Highest)
	case row.streamCreated != store.EncodeTime(created):
		t.Errorf("the audit row's stream instant is %v, want %v — the stream "+
			"the fleet moved onto", store.DecodeTime(row.streamCreated), created)
	case row.at != store.EncodeTime(stored):
		t.Errorf("the audit row's instant is %v, want the broker's %v",
			store.DecodeTime(row.at), stored)
	case row.recordID != rec.OpID:
		t.Errorf("the audit row names record %q, want the operation %q that "+
			"wrote it", row.recordID, rec.OpID)
	}
}

// A REANCHOR'S RECORD NAMES WHOEVER RAN IT IN CHAT'S OWN WORDS.
//
// The engine hands the transition its operator as the identity directory names
// a party ([iam.ActorFor]'s four kinds, which are chat's author kinds word for
// word), and a reanchor run by nobody in particular is this node's own. Each
// lands on the record as the kind every chat reader renders, and a credential
// acting as itself carries its login as the credential when nothing else is
// named — the same reading the knowledge base's generation record makes.
// Mutation: map any kind below to another, or drop the credential's default,
// and its case says so.
func TestAGenerationRecordNamesItsAuthorInChatsWords(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		by       string
		byKind   iam.ActorKind
		operator string
		want     chatlet.Actor
	}{
		"nobody named": {"", "", "",
			chatlet.Actor{Handle: "node-a", Kind: chatlet.AuthorSystem}},
		"a person, as the directory names one": {"jane", iam.ActorHuman, "pat:1",
			chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorHuman, OperatorID: "pat:1"}},
		"a seat": {"eng", iam.ActorAgent, "",
			chatlet.Actor{Handle: "eng", Kind: chatlet.AuthorAgent}},
		"the engine": {"engine", iam.ActorSystem, "",
			chatlet.Actor{Handle: "engine", Kind: chatlet.AuthorSystem}},
		"a credential acting as itself": {"ops-1", iam.ActorOperator, "",
			chatlet.Actor{Handle: "ops-1", Kind: chatlet.AuthorOperator, OperatorID: "ops-1"}},
		"a kind nobody stated": {"ci", "", "",
			chatlet.Actor{Handle: "ci", Kind: chatlet.AuthorOperator, OperatorID: "ci"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec, kept, err := chatlet.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
				Generation: 2, By: c.by, ByKind: c.byKind, OperatorID: c.operator,
				Writer: "node-a", At: time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC),
				Inputs: statelog.ReanchorInputs{Stream: topics.ChatLogStream,
					StreamCreatedAt: time.Date(2031, 3, 30, 9, 0, 0, 0, time.UTC)},
			})
			if err != nil || !kept {
				t.Fatalf("encode: %v (kept %v)", err, kept)
			}
			got, err := chatlet.Decode(rec.Payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Actor != c.want.Handle || got.ActorKind != c.want.Kind ||
				got.OperatorID != c.want.OperatorID {
				t.Errorf("the record names (%q, %q, %q), want (%q, %q, %q)",
					got.Actor, got.ActorKind, got.OperatorID,
					c.want.Handle, c.want.Kind, c.want.OperatorID)
			}
		})
	}
}

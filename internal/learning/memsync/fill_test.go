package memsync

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// A FILLED VECTOR REACHES THE SEAT'S NEXT HOLDER. A diary note written with no
// vector is carried to a peer as it is; when the holder later fills its vector,
// that change has to travel too, or the peer's copy stays vectorless — and the
// day the seat moves there, that stale copy is the holder's and the note is
// out of reach of every similarity recall. Two things carry it: the fill
// re-files the row past the watermark the first carry left, and the peer's
// import takes the carried vector over a stored one of no model.

// carryRows moves rows exported from one store into another, the way a
// hydration replays them.
func carryRows(t *testing.T, to *store.DB, spec table, rows []Row) {
	t.Helper()
	ctx := context.Background()
	for _, row := range rows {
		body, err := encode(row)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		decoded, decodedSpec, known, err := decode(body)
		if err != nil || !known {
			t.Fatalf("decode: known=%v err=%v", known, err)
		}
		if err := to.Tx(ctx, func(tx *sql.Tx) error {
			return upsert(ctx, tx, decodedSpec, decoded)
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
}

// diaryVector reads one note's stored vector and model.
func diaryVector(t *testing.T, db *store.DB, id string) ([]byte, string) {
	t.Helper()
	var (
		blob  []byte
		model sql.NullString
	)
	if err := db.SQL().QueryRowContext(t.Context(),
		"SELECT embedding, embedding_model FROM agent_diary WHERE id = ?", id,
	).Scan(&blob, &model); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return blob, model.String
}

func TestAFilledVectorReachesAPeerHoldingTheVectorlessNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	holder, peer := openStore(t), openStore(t)
	at := time.Now().UTC().Add(-time.Hour)
	note := learning.DiaryEntry{
		ID: "d1", AgentID: seat.AgentID, Kind: learning.DiaryLong,
		Content: "the release train is thursdays", Source: "reflect", CreatedAt: at,
	}
	if err := learning.NewDiary(holder).Write(ctx, note); err != nil {
		t.Fatalf("write the note: %v", err)
	}
	diary := tables[0]
	if diary.name != "agent_diary" {
		t.Fatalf("fixture drifted: tables[0] is %s", diary.name)
	}

	// THE FIRST CARRY: the peer holds the note, with no vector.
	first, mark, err := export(ctx, holder.SQL(), diary, seat, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first export = %d rows, %v", len(first), err)
	}
	carryRows(t, peer, diary, first)
	// The peer's own bookkeeping on its copy, which the fill must not touch.
	if _, err := peer.SQL().ExecContext(ctx,
		"UPDATE agent_diary SET retrieval_count = 7 WHERE id = 'd1'"); err != nil {
		t.Fatalf("the peer's bookkeeping: %v", err)
	}

	// THE FILL, on the holder.
	filled, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.DiaryFill{{
		ID: "d1", Vector: learning.Vector{Values: []float32{0.5, 0.5}, Model: "model-a"},
	}})
	if err != nil || filled != 1 {
		t.Fatalf("FillEmbeddings = %d, %v; want the one note", filled, err)
	}

	// THE NEXT CARRY sees it, past the mark the first one left.
	again, _, err := export(ctx, holder.SQL(), diary, seat, mark)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("the filled note was not carried again: %d rows past the watermark", len(again))
	}
	carryRows(t, peer, diary, again)

	if blob, model := diaryVector(t, peer, "d1"); len(blob) == 0 || model != "model-a" {
		t.Fatalf("the peer still holds the vectorless copy: %d bytes in %q", len(blob), model)
	}
	var retrievals int
	if err := peer.SQL().QueryRowContext(ctx,
		"SELECT retrieval_count FROM agent_diary WHERE id = 'd1'").Scan(&retrievals); err != nil {
		t.Fatalf("read the peer's bookkeeping: %v", err)
	}
	if retrievals != 7 {
		t.Errorf("the fill rewrote the peer's own bookkeeping: retrieval_count = %d", retrievals)
	}

	// A STALE COPY NEVER ERASES IT: the vectorless row the first carry
	// moved, replayed again, leaves the filled vector where it is.
	carryRows(t, peer, diary, first)
	if blob, model := diaryVector(t, peer, "d1"); len(blob) == 0 || model != "model-a" {
		t.Fatalf("a vectorless replay erased the filled vector: %d bytes in %q", len(blob), model)
	}
}

// A RE-EMBED UNDER ANOTHER MODEL TRAVELS TOO: a company that moved models has
// every note re-filled by the holder, and a peer's copy in the old space is
// replaced rather than kept because it already had a vector.
func TestAReembeddedNoteReplacesAPeersCopyFromTheOldModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	holder, peer := openStore(t), openStore(t)
	note := learning.DiaryEntry{
		ID: "d1", AgentID: seat.AgentID, Kind: learning.DiaryLong,
		Content: "the release train is thursdays", Source: "reflect",
		CreatedAt: time.Now().UTC(), Embedding: []float32{1, 0}, EmbeddingModel: "model-a",
	}
	if err := learning.NewDiary(holder).Write(ctx, note); err != nil {
		t.Fatalf("write the note: %v", err)
	}
	diary := tables[0]
	first, mark, err := export(ctx, holder.SQL(), diary, seat, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first export = %d rows, %v", len(first), err)
	}
	carryRows(t, peer, diary, first)

	if _, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.DiaryFill{{
		ID: "d1", Vector: learning.Vector{Values: []float32{0, 1}, Model: "model-b"},
	}}); err != nil {
		t.Fatalf("re-embed: %v", err)
	}
	again, _, err := export(ctx, holder.SQL(), diary, seat, mark)
	if err != nil || len(again) != 1 {
		t.Fatalf("the re-embedded note was not carried: %d rows, %v", len(again), err)
	}
	carryRows(t, peer, diary, again)
	if _, model := diaryVector(t, peer, "d1"); model != "model-b" {
		t.Fatalf("the peer kept the old model's vector: %q", model)
	}
}

// THE SAME MODEL AT ANOTHER WIDTH IS ANOTHER SPACE TOO: a restart that moved
// the width leaves notes of the same model the recall's width filter no longer
// admits, and the holder's re-fill has to replace a peer's copy at the old
// width rather than be skipped because the model matches.
func TestARefillAtANewWidthReplacesAPeersCopyAtTheOld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	holder, peer := openStore(t), openStore(t)
	note := learning.DiaryEntry{
		ID: "d1", AgentID: seat.AgentID, Kind: learning.DiaryLong,
		Content: "the release train is thursdays", Source: "reflect",
		CreatedAt: time.Now().UTC(), Embedding: []float32{1, 0}, EmbeddingModel: "model-a",
	}
	if err := learning.NewDiary(holder).Write(ctx, note); err != nil {
		t.Fatalf("write the note: %v", err)
	}
	diary := tables[0]
	first, mark, err := export(ctx, holder.SQL(), diary, seat, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first export = %d rows, %v", len(first), err)
	}
	carryRows(t, peer, diary, first)

	if filled, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.DiaryFill{{
		ID: "d1", Vector: learning.Vector{Values: []float32{0, 1, 0, 0}, Model: "model-a"},
	}}); err != nil || filled != 1 {
		t.Fatalf("re-fill at the new width = %d, %v; want the note", filled, err)
	}
	again, _, err := export(ctx, holder.SQL(), diary, seat, mark)
	if err != nil || len(again) != 1 {
		t.Fatalf("the re-filled note was not carried: %d rows, %v", len(again), err)
	}
	carryRows(t, peer, diary, again)
	if blob, _ := diaryVector(t, peer, "d1"); len(blob) != 16 {
		t.Fatalf("the peer kept the old width's vector: %d bytes, want 16", len(blob))
	}
}

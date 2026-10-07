package memsync

import (
	"context"
	"database/sql"
	"slices"
	"strings"
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
	filled, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.VectorFill{{
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

	if _, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.VectorFill{{
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

	if filled, err := learning.NewDiary(holder).FillEmbeddings(ctx, []learning.VectorFill{{
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

// AN EPISODE IS FILLED AND CARRIED LIKE A NOTE, and what its turn was asked
// travels with it: the ask is the part of the episode's vector nothing else on
// the row holds, so a peer that took the seat with a row lacking it could never
// make that vector again.
func TestAFilledEpisodeAndItsAskReachAPeerHoldingTheVectorlessRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	holder, peer := openStore(t), openStore(t)
	at := time.Now().UTC().Add(-time.Hour)
	if _, err := learning.NewEpisodes(holder).Append(ctx, learning.Episode{
		ID: "e1", Handle: seat.Handle, Role: "Engineer", TurnID: "t1",
		StartedAt: at, EndedAt: at, TaskSummary: "Message from Ana: Slack message",
		Ask: "The staging deploy keeps failing.", PlanSummary: "rolled back the cache change",
		ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	episodes := tables[1]
	if episodes.name != "episodes" || !episodes.fillsVector {
		t.Fatalf("fixture drifted: tables[1] is %s, fills %v", episodes.name, episodes.fillsVector)
	}
	first, mark, err := export(ctx, holder.SQL(), episodes, seat, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first export = %d rows, %v", len(first), err)
	}
	carryRows(t, peer, episodes, first)
	var ask string
	if err := peer.SQL().QueryRowContext(ctx, "SELECT ask FROM episodes WHERE id = 'e1'").Scan(&ask); err != nil {
		t.Fatalf("read the carried ask: %v", err)
	}
	if ask != "The staging deploy keeps failing." {
		t.Fatalf("the peer holds the ask %q", ask)
	}

	if filled, err := learning.NewEpisodes(holder).FillEmbeddings(ctx, []learning.VectorFill{{
		ID: "e1", Vector: learning.Vector{Values: []float32{0.5, 0.5}, Model: "model-a"},
	}}); err != nil || filled != 1 {
		t.Fatalf("FillEmbeddings = %d, %v; want the episode", filled, err)
	}
	again, _, err := export(ctx, holder.SQL(), episodes, seat, mark)
	if err != nil || len(again) != 1 {
		t.Fatalf("the filled episode was not carried: %d rows, %v", len(again), err)
	}
	carryRows(t, peer, episodes, again)
	var (
		blob  []byte
		model string
	)
	if err := peer.SQL().QueryRowContext(ctx,
		"SELECT embedding, embedding_model FROM episodes WHERE id = 'e1'").Scan(&blob, &model); err != nil {
		t.Fatalf("read the peer's episode: %v", err)
	}
	if len(blob) == 0 || model != "model-a" {
		t.Fatalf("the peer still holds the vectorless episode: %d bytes in %q", len(blob), model)
	}
}

// THE ASK AN OLDER HOLDER LOST IS TAKEN BACK FROM A COPY THAT HAS IT.
//
// A holder on a build from before node migration 0042 has no `ask` column: it
// hydrates the seat's episodes without their asks and, holding the seat with
// its watermark empty, republishes every one of them — over the copy that had
// the ask, since the changelog keeps one message a row. A node hydrating that
// copy lands ''. Taking only a carried vector, its import never took the ask
// back when a newer holder republished the row, so the node read every such
// turn without what it was asked, filled its vector without it, and republished
// both as the seat's whenever it held the seat.

// episodeRow is the history-bearing half of a stored episode.
type episodeRow struct {
	ask   string
	blob  []byte
	model sql.NullString
}

// readEpisode reads one episode's ask and vector.
func readEpisode(t *testing.T, db *store.DB, id string) episodeRow {
	t.Helper()
	var row episodeRow
	if err := db.SQL().QueryRowContext(t.Context(),
		"SELECT ask, embedding, embedding_model FROM episodes WHERE id = ?", id,
	).Scan(&row.ask, &row.blob, &row.model); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return row
}

// askedEpisode writes, on db, the episode e1 with an ask, and — when vector is
// set — the vector its holder made of its whole text; and returns the rows a
// publish of the seat's episodes would carry.
func askedEpisode(t *testing.T, db *store.DB, vector []float32) []Row {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	episodes := learning.NewEpisodes(db)
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "e1", Handle: seat.Handle, Role: "Engineer", TurnID: "t1",
		StartedAt: at, EndedAt: at, TaskSummary: "Message from Ana: Slack message",
		Ask: "The staging deploy keeps failing.", PlanSummary: "rolled back the cache change",
		ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if vector != nil {
		if filled, err := episodes.FillEmbeddings(t.Context(), []learning.VectorFill{{
			ID: "e1", Vector: learning.Vector{Values: vector, Model: "model-a"},
		}}); err != nil || filled != 1 {
			t.Fatalf("FillEmbeddings = %d, %v", filled, err)
		}
	}
	rows, _, err := export(t.Context(), db.SQL(), tables[1], seat, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("export = %d rows, %v", len(rows), err)
	}
	return rows
}

// asAnOlderBuildCarriesIt is rows as a build before node migrations 0039 and
// 0042 republishes them: with no model on the vector and no ask.
func asAnOlderBuildCarriesIt(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, row := range rows {
		values := make(map[string]any, len(row.Values))
		for column, cell := range row.Values {
			if column != "ask" && column != "embedding_model" {
				values[column] = cell
			}
		}
		out[i] = Row{Table: row.Table, Values: values}
	}
	return out
}

// fillWithoutTheAsk is what a newer node does with the copy an older holder
// left it: it embeds the row as it stands — without the ask — in model-a.
func fillWithoutTheAsk(t *testing.T, db *store.DB) {
	t.Helper()
	if filled, err := learning.NewEpisodes(db).FillEmbeddings(t.Context(), []learning.VectorFill{{
		ID: "e1", Vector: learning.Vector{Values: []float32{0.1, 0.9}, Model: "model-a"},
	}}); err != nil || filled != 1 {
		t.Fatalf("FillEmbeddings = %d, %v", filled, err)
	}
}

func TestAnAskAnOlderHolderLostIsTakenBackWithItsVector(t *testing.T) {
	t.Parallel()
	holder, peer := openStore(t), openStore(t)
	full := askedEpisode(t, holder, []float32{0.5, 0.5})
	want := readEpisode(t, holder, "e1")

	carryRows(t, peer, tables[1], asAnOlderBuildCarriesIt(full))
	if got := readEpisode(t, peer, "e1"); got.ask != "" {
		t.Fatalf("the fixture is wrong: the older build's copy landed the ask %q", got.ask)
	}
	fillWithoutTheAsk(t, peer)

	// A newer holder republishes the row whole.
	carryRows(t, peer, tables[1], full)
	got := readEpisode(t, peer, "e1")
	if got.ask != "The staging deploy keeps failing." {
		t.Fatalf("the peer still holds the ask %q after a copy carrying it arrived", got.ask)
	}
	if !slices.Equal(got.blob, want.blob) || got.model.String != "model-a" {
		t.Fatalf("the peer kept the vector it made without the ask (%d bytes in %q), want "+
			"the holder's, made from the whole text", len(got.blob), got.model.String)
	}
}

// A ROW THAT TAKES ITS ASK BACK FROM A COPY WITH NO VECTOR DROPS THE ONE IT MADE
// WITHOUT IT, so the holder's fill makes the vector again from the whole text —
// rather than keeping, beside the ask, a vector of a text the row no longer is.
func TestAnAskTakenBackWithoutAVectorLeavesTheRowToBeFilledWhole(t *testing.T) {
	t.Parallel()
	holder, peer := openStore(t), openStore(t)
	full := askedEpisode(t, holder, nil)
	carryRows(t, peer, tables[1], asAnOlderBuildCarriesIt(full))
	fillWithoutTheAsk(t, peer)

	carryRows(t, peer, tables[1], full)
	got := readEpisode(t, peer, "e1")
	if got.ask != "The staging deploy keeps failing." || got.blob != nil || got.model.Valid {
		t.Fatalf("the peer holds ask %q with %d bytes of vector in %q, want the ask and no "+
			"vector", got.ask, len(got.blob), got.model.String)
	}
	left, err := learning.NewEpisodes(peer).Unfilled(t.Context(), seat.Handle, "model-a",
		learning.FillCursor{}, 10)
	if err != nil || len(left) != 1 || !strings.Contains(left[0].Text, "staging deploy") {
		t.Fatalf("unfilled = %+v, %v; want the row, to be embedded with its ask", left, err)
	}
}

// A COPY THAT LOST THE ASK TAKES NOTHING FROM A ROW THAT HAS IT: not the ask's
// absence, and not the vector made without it — which is of another text, and
// is not this row's vector however much newer its space. Taken over a stored
// row with no vector, it was the row's from then on.
func TestACopyThatLostTheAskTakesNothingFromARowThatHasIt(t *testing.T) {
	t.Parallel()
	holder, peer, lossy := openStore(t), openStore(t), openStore(t)
	full := askedEpisode(t, holder, nil)
	carryRows(t, peer, tables[1], full)

	carryRows(t, lossy, tables[1], asAnOlderBuildCarriesIt(full))
	fillWithoutTheAsk(t, lossy)
	weak, _, err := export(t.Context(), lossy.SQL(), tables[1], seat, 0)
	if err != nil || len(weak) != 1 {
		t.Fatalf("export = %d rows, %v", len(weak), err)
	}
	carryRows(t, peer, tables[1], weak)
	got := readEpisode(t, peer, "e1")
	if got.ask != "The staging deploy keeps failing." {
		t.Fatalf("a copy without the ask took it away: the peer holds %q", got.ask)
	}
	if got.blob != nil || got.model.Valid {
		t.Fatalf("the peer took a vector made without the ask (%d bytes in %q)",
			len(got.blob), got.model.String)
	}
}

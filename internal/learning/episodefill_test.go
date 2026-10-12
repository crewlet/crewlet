package learning_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// AN EPISODE'S VECTOR CAN BE MADE AGAIN, AS IT WAS MADE: the row stores what
// the turn was asked, so the text the fill is handed for a row with no vector
// of the current model is byte for byte the text the episodist embedded. With
// no ask stored, a row that missed its vector, or had one of a model the
// company left, would be out of similarity search for good.
func TestAnEpisodesFillTextIsTheTextItWasEmbeddedFrom(t *testing.T) {
	t.Parallel()
	e := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	var sent string
	w := episodist(t, e, func(o *learning.EpisodistOptions) {
		o.Embed = func(_ context.Context, text string) (learning.Vector, error) {
			sent = text
			return learning.Vector{Values: []float32{1, 0, 0, 0}, Model: "old-model"}, nil
		}
	})
	turn := epTurn()
	turn.Event.TaskSummary = "Message from Ana: Slack message"
	turn.Event.Interactions = []types.InboundInteraction{
		{Sender: types.CanonicalIdentity{ExternalID: "U1", Platform: "slack"},
			Body: "The staging deploy keeps failing."},
	}
	reflectEpisode(t, w, turn)

	recent, err := e.Recent(context.Background(), turn.Event.AgentHandle, 1)
	if err != nil || len(recent) != 1 {
		t.Fatalf("Recent = %v, %v", recent, err)
	}
	if recent[0].Ask != "The staging deploy keeps failing." {
		t.Fatalf("the row stored the ask %q", recent[0].Ask)
	}

	unfilled, err := e.Unfilled(context.Background(), turn.Event.AgentHandle, "new-model",
		learning.FillCursor{}, 10)
	if err != nil || len(unfilled) != 1 {
		t.Fatalf("Unfilled under a new model = %v, %v; want the episode", unfilled, err)
	}
	if unfilled[0].Text != sent {
		t.Fatalf("the fill is handed %q\nthe episodist embedded %q", unfilled[0].Text, sent)
	}
	if left, err := e.Unfilled(context.Background(), turn.Event.AgentHandle, "old-model",
		learning.FillCursor{}, 10); err != nil || len(left) != 0 {
		t.Fatalf("an episode with a vector of the current model is unfilled: %v, %v", left, err)
	}
}

// THE FILL READS WHAT RECALL CANNOT REACH, AND ONLY THAT: raw episodes with no
// vector of the model at this width, newest first, a page at a time past the
// cursor — never a compacted row, which recall never searches, never another
// seat's, never one with nothing to embed. Unsearchable counts the SAME rows:
// a row with no text is not a turn the search could not reach but one nothing
// will ever embed, and counting it told the seat for ever that it was being
// embedded again.
func TestAnEpisodeFillReadsWhatRecallCannotReach(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	e := learning.NewEpisodes(db)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	add := func(id, handle string, minutes int, change func(*learning.Episode)) {
		t.Helper()
		episode := ep(id, handle, at(minutes))
		episode.EmbeddingModel = ""
		if change != nil {
			change(&episode)
		}
		mustAppend(t, e, episode)
	}
	add("bare", "ceo", 1, nil)
	add("old-model", "ceo", 2, func(x *learning.Episode) {
		x.Embedding, x.EmbeddingModel = []float32{1, 0, 0, 0}, "old-model"
	})
	add("current", "ceo", 3, func(x *learning.Episode) {
		x.Embedding, x.EmbeddingModel = []float32{1, 0, 0, 0}, testModel
	})
	add("compacted", "ceo", 4, func(x *learning.Episode) {
		x.Kind, x.Count, x.CommonTaskPattern = learning.KindCompacted, 3, "deploys"
	})
	add("theirs", "cto", 5, nil)
	add("blank", "ceo", 6, func(x *learning.Episode) { x.TaskSummary = " \n " })
	// Blank to the embedder too, which drops every space unicode.IsSpace
	// names — a no-break space and an ideographic one among them.
	add("wide-blank", "ceo", 8, func(x *learning.Episode) { x.TaskSummary = "\u00a0\u3000\u2028" })
	add("later", "ceo", 7, nil)

	first, err := e.Unfilled(context.Background(), "ceo", testModel, learning.FillCursor{}, 2)
	if err != nil {
		t.Fatalf("Unfilled: %v", err)
	}
	rest, err := e.Unfilled(context.Background(), "ceo", testModel, first[len(first)-1].Cursor, 10)
	if err != nil {
		t.Fatalf("Unfilled past the cursor: %v", err)
	}
	var ids []string
	for _, row := range append(first, rest...) {
		ids = append(ids, row.ID)
	}
	if want := []string{"later", "old-model", "bare"}; !slices.Equal(ids, want) {
		t.Fatalf("unfilled = %v, want %v", ids, want)
	}

	n, err := e.Unsearchable(context.Background(), "ceo", testModel)
	if err != nil || n != 3 {
		t.Fatalf("Unsearchable = %d, %v; want the three rows the fill reads, and neither blank one",
			n, err)
	}

	filled, err := e.FillEmbeddings(context.Background(), []learning.VectorFill{
		{ID: "bare", Vector: learning.Vector{Values: []float32{0, 1, 0, 0}, Model: testModel}},
		{ID: "compacted", Vector: learning.Vector{Values: []float32{0, 1, 0, 0}, Model: testModel}},
	})
	if err != nil || filled != 1 {
		t.Fatalf("FillEmbeddings = %d, %v; want the raw row filled and the compacted one not", filled, err)
	}
	if n, _ := e.Unsearchable(context.Background(), "ceo", testModel); n != 2 {
		t.Errorf("after the fill %d rows are unsearchable, want 2", n)
	}
	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{0, 1, 0, 0}, Model: testModel,
	})
	if err != nil || len(hits) != 1 || hits[0].Episode.ID != "bare" {
		t.Fatalf("recall after the fill = %v, %v; want the filled episode", hits, err)
	}
}

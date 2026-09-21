package search_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// TestTheVectorCorpusIsPagesAndTasksOnly is the gate behind ADR-0019, and the
// decision it holds is a COST rather than a taste.
//
// Every member of [search.Sources] is a corpus the fleet-singleton embedding
// duty walks, pays a provider for, writes a record per document for, and keeps
// a vector per document of in the replicated estate — which every snapshot
// copies and every joining node transfers. The duty's whole budget is about a
// thousand sources a minute, shared across every corpus a company has.
//
// So the list is not "what could usefully be embedded"; it is "what a company
// can afford to embed". Chat is the corpus that makes the difference visible:
// at the declared census of 20,000 messages a day, one year is 7.3 million
// documents against a supported vector corpus of a few hundred thousand — an
// order of magnitude past the whole budget, spent on the corpus with the
// lowest value per document, and taken from the knowledge base's own
// embeddings to pay for it. Chat gets a keyword index of its own instead
// (`chat_docs`, `chat_postings`), which is affordable, and no semantic half at
// all.
//
// THE ENUM IS THE GATE because it is what the duty reads. A source added here
// is a bill a company starts paying on its next boot, with nothing else in the
// tree asking whether it can.
func TestTheVectorCorpusIsPagesAndTasksOnly(t *testing.T) {
	t.Parallel()
	want := []search.Source{search.SourcePage, search.SourceTask}
	if !slices.Equal(search.Sources, want) {
		t.Fatalf("the embedding duty walks %v, and this build embeds %v. A "+
			"source here is a vector per document in the replicated estate, a "+
			"record per document on the log and a provider bill, against a duty "+
			"budget of about a thousand sources a minute shared by every corpus "+
			"the company has. If a corpus genuinely belongs in the semantic half, "+
			"change ADR-0019 and this case together; if it belongs in search at "+
			"all, the lexical half is what it is affordable in",
			want, search.Sources)
	}

	// AND CHAT IS THE ONE NAMED, because it is the corpus somebody will
	// add: it is the most searched thing in a company, it is already
	// indexed lexically, and the one line it takes here is invisible next
	// to what it costs. The shard source the chat index writes is
	// deliberately NOT a member of this enum for exactly that reason.
	for _, s := range search.Sources {
		if string(s) == "message" || string(s) == "chat" {
			t.Fatalf("%q is in the vector corpus. Chat is a keyword corpus "+
				"here: a year at the declared census is several times the whole "+
				"supported vector corpus, and embedding it starves the knowledge "+
				"base's own vectors to do it. See ADR-0019", s)
		}
	}
}

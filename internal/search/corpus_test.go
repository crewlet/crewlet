package search_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// TestTheVectorCorpusIsPagesAndTasksOnly is the gate behind ADR-0071, and the
// decision it holds is a COST rather than a taste.
//
// Every member of [search.Sources] is a corpus the embedding duty walks, pays a
// provider for and keeps a replicated vector per document of; its budget is
// about a thousand sources a minute across every corpus. So the list is "what a
// company can afford to embed": a year of chat at 20,000 messages a day is 7.3
// million documents against a supported corpus of about a million (ADR-0028).
// Chat has a keyword index (`chat_docs`, `chat_postings`), no semantic half.
//
// THE ENUM IS THE GATE because the duty reads it: a source added here is a
// bill a company starts paying on its next boot.
func TestTheVectorCorpusIsPagesAndTasksOnly(t *testing.T) {
	t.Parallel()
	want := []search.Source{search.SourcePage, search.SourceTask}
	if !slices.Equal(search.Sources, want) {
		t.Fatalf("the embedding duty walks %v, and this build embeds %v. A "+
			"source here is a vector per document in the replicated estate, a "+
			"record per document on the log and a provider bill, against a duty "+
			"budget of about a thousand sources a minute shared by every corpus "+
			"the company has. If a corpus genuinely belongs in the semantic half, "+
			"change ADR-0071 and this case together; if it belongs in search at "+
			"all, the lexical half is what it is affordable in",
			want, search.Sources)
	}

	// AND CHAT IS THE ONE NAMED, because it is the corpus somebody will
	// add: it is the most searched thing in a company, it is already
	// indexed lexically, and the one line it takes here is invisible next
	// to what it costs. The namespace [search.ChatShardOf] buckets a
	// message under is deliberately NOT a member of this enum for exactly
	// that reason.
	for _, s := range search.Sources {
		if string(s) == "message" || string(s) == "chat" {
			t.Fatalf("%q is in the vector corpus. Chat is a keyword corpus "+
				"here: a year at the declared census is several times the whole "+
				"supported vector corpus, and embedding it starves the knowledge "+
				"base's own vectors to do it. See ADR-0071", s)
		}
	}
}

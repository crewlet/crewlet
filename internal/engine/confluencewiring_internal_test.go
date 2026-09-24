package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// ONLY A COMPANY WHOSE KNOWLEDGE BASE IS CONFLUENCE GETS ITS READING HALF.
//
// A company on `backend: none` may keep an `integrations.confluence` block for
// its page routing alone. Its parser routes page activity as it would for any
// company, but a searcher built for it would search a wiki the company says it
// does not run, and the org client beside it is what the promotion pass drafts
// through. The control is the same block on `backend: confluence`, whose
// org token builds both.
func TestOnlyAConfluenceKnowledgeBaseGetsItsSearcher(t *testing.T) {
	t.Parallel()
	block := &config.Confluence{URL: "https://wiki.example.com", Token: "t"}
	for _, tc := range []struct {
		name    string
		backend config.KnowledgeBackend
		reads   bool
	}{
		{"its knowledge base", config.KnowledgeConfluence, true},
		{"routing alone, on backend none", config.KnowledgeNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Company{
				Config: &config.Company{
					Name:         "Acme",
					Knowledge:    config.Knowledge{Backend: tc.backend},
					Integrations: config.Integrations{Confluence: block},
				},
				Org: &org.Organization{Name: "Acme"},
			}
			parts, err := (&Engine{}).startConfluence(c, block)
			if err != nil {
				t.Fatalf("startConfluence: %v", err)
			}
			if parts.parser == nil {
				t.Fatal("no parser was built, so page activity routes nowhere")
			}
			if got := parts.searcher != nil; got != tc.reads {
				t.Errorf("a searcher was built = %v, want %v", got, tc.reads)
			}
			if got := parts.pages != nil; got != tc.reads {
				t.Errorf("an org client was built = %v, want %v", got, tc.reads)
			}
		})
	}
}

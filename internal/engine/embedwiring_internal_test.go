package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
)

// The embed seams, as an apply wires them.
//
// Every learning path that embeds — the turn-start prefetch, the pull tools
// that re-run it, the episodist — reads the engine's CURRENT embedder when it
// is called. These tests reach that through the real apply step (equip) and a
// real provider over a stand-in endpoint, because the bug they guard was an
// ORDER inside the apply: the tools were built before the embedder they were
// meant to use was stored, so they ran one epoch behind for their whole life.

// keyedEmbeddings is an OpenAI-compatible embeddings endpoint that records
// which credential each request carried.
type keyedEmbeddings struct {
	srv *httptest.Server

	mu   sync.Mutex
	keys []string
}

func newKeyedEmbeddings(t *testing.T, width int) *keyedEmbeddings {
	t.Helper()
	k := &keyedEmbeddings{}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		inputs := 1
		var many []string
		if json.Unmarshal(req.Input, &many) == nil {
			inputs = len(many)
		}
		k.mu.Lock()
		k.keys = append(k.keys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		k.mu.Unlock()
		vector := strings.TrimSuffix(strings.Repeat("0.5,", width), ",")
		items := make([]string, inputs)
		for i := range items {
			items[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[%s]}`, i, vector)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[` + strings.Join(items, ",") +
			`],"model":"text-embedding-3-small"}`))
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// used is every credential the endpoint has been called with, in order.
func (k *keyedEmbeddings) used() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

// keyedCompany is a company whose embeddings provider is the stand-in
// endpoint, authorised with key.
func keyedCompany(t *testing.T, endpoint, key string) *Company {
	t.Helper()
	return companyWith(t, fmt.Sprintf(`
name: Nimbus
providers:
  llm:
    scripted:
      type: anthropic
      model: claude-x
      api_keys: ["sk-test"]
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: %s
    base_url: %s
    dimensions: 64
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`, key, endpoint))
}

// recallByMeaning calls an epoch's query_episodes with a query, as the seat
// would, and returns what the model is told.
func recallByMeaning(t *testing.T, c *Company) string {
	t.Helper()
	entry, ok := c.Tools.Lookup(builtin.QueryEpisodesTool)
	if !ok {
		t.Fatal("the epoch has no query_episodes")
	}
	tool, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("query_episodes is a %T, not a seat tool", entry.Tool)
	}
	seat := c.Org.AgentSeatByHandle("ceo")
	res, err := tool.CallForTurn(t.Context(), &turnctx.Turn{Seat: seat, Org: c.Org},
		map[string]any{"query": "the staging deploy keeps failing"})
	if err != nil {
		t.Fatalf("query_episodes: %v", err)
	}
	return res.Output
}

// THE FIRST EPOCH A NODE APPLIES CAN SEARCH BY MEANING. Its tools are built
// before its embedder is stored, so a seam captured at build held nothing: for
// the whole of a node's first epoch query_episodes told every seat "no
// embeddings are configured" on a company that configured them.
func TestTheBootEpochsPullToolsSearchWithItsEmbedder(t *testing.T) {
	t.Parallel()
	api := newKeyedEmbeddings(t, 64)
	e := engineOverStore(t, 64)
	c := keyedCompany(t, api.srv.URL, "sk-first")
	if err := e.equip(t.Context(), c); err != nil {
		t.Fatalf("equip: %v", err)
	}
	out := recallByMeaning(t, c)
	if strings.Contains(out, "embeddings") {
		t.Fatalf("the boot epoch's query_episodes could not search by meaning: %q", out)
	}
	if got := api.used(); len(got) != 1 || got[0] != "sk-first" {
		t.Fatalf("the provider was called with %q, want one search with the "+
			"configured key", got)
	}
}

// A ROTATED KEY REACHES THE PULL TOOLS. Re-activating an unchanged revision is
// the documented way to rotate a credential, and it builds the new epoch's
// tools before it stores the provider holding the new key — so a seam captured
// at build went on sending the retired key until the NEXT apply. The tools of
// the epoch before it are reached through seat clones until those are refiled,
// so they too must embed with what is current.
func TestARotatedKeyReachesThePullTools(t *testing.T) {
	t.Parallel()
	api := newKeyedEmbeddings(t, 64)
	e := engineOverStore(t, 64)
	first := keyedCompany(t, api.srv.URL, "sk-retired")
	if err := e.equip(t.Context(), first); err != nil {
		t.Fatalf("equip the first epoch: %v", err)
	}
	second := keyedCompany(t, api.srv.URL, "sk-rotated")
	if err := e.equip(t.Context(), second); err != nil {
		t.Fatalf("equip the rotated epoch: %v", err)
	}
	recallByMeaning(t, second)
	recallByMeaning(t, first)
	got := api.used()
	if len(got) != 2 || got[0] != "sk-rotated" || got[1] != "sk-rotated" {
		t.Fatalf("after the rotation the provider was called with %q, want the "+
			"rotated key on both epochs' tools", got)
	}
}

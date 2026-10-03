package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// poolCompany declares a two-key anthropic entry that repeats one key and
// names an unset variable, an openai entry leaning on the conventional
// variable, and a cli-agent entry with no key bag at all.
func poolCompany(t *testing.T, r *config.Resolver) *Company {
	t.Helper()
	cfg, err := config.ParseCompany(fmt.Appendf(nil, `
name: Acme
providers:
  llm:
    smart:
      type: anthropic
      model: claude-test
      api_keys: ["${KEY_A}", "${KEY_B}", "${KEY_A}", "${KEY_UNSET}"]
      cooldowns: {rate_limit_seconds: 600}
    fast:
      type: openai
      model: gpt-test
    subscription:
      type: cli-agent
      model: sonnet
      cli: {agent: claude-code, state_dir: %q}
roles:
  - name: CEO
    handle: ceo
`, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCompanyWith(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func poolEngine(t *testing.T) (*Engine, *Company) {
	t.Helper()
	r := config.NewResolver(config.MapSource{
		"KEY_A":          "sk-ant-value-a",
		"KEY_B":          "sk-ant-value-b",
		"OPENAI_API_KEY": "sk-openai-conventional",
	})
	c := poolCompany(t, r)
	e := &Engine{}
	e.epoch.current.Store(c)
	return e, c
}

// EVERY KEY IS REPORTED WHERE IT CAME FROM, joined to its pool's state on the
// hint — the only thing the document's provenance and the pool both hold.
func TestCredentialPoolsJoinProvenanceToThePool(t *testing.T) {
	t.Parallel()
	e, c := poolEngine(t)

	// Bench KEY_B on this node, the way a 429 does.
	provider, _ := c.Models.Provider("smart")
	pool := provider.(interface{ Pool() *credential.Pool }).Pool()
	// Held rather than returned as they are taken: selection is
	// least-in-flight, so a returned lease is handed straight back.
	var benched string
	var held []*credential.Lease
	for range pool.Size() {
		lease, ok := pool.Acquire()
		if !ok {
			t.Fatal("the pool had nothing to lease")
		}
		if lease.Hint() == credential.Hint("sk-ant-value-b") {
			benched = lease.Hint()
			lease.Fail(t.Context(), llm.KindRateLimit, 0)
			break
		}
		held = append(held, lease)
	}
	for _, lease := range held {
		lease.Succeed()
	}
	if benched == "" {
		t.Fatal("KEY_B was never leased")
	}

	got := e.CredentialPools()
	if len(got) != 3 || got[0].Provider != "smart" || got[1].Provider != "fast" || got[2].Provider != "subscription" {
		t.Fatalf("pools = %+v, want smart, fast, subscription in config order", got)
	}

	smart := got[0]
	if !smart.Pooled || smart.RateLimit != 10*time.Minute || smart.Auth != 5*time.Minute {
		t.Errorf("smart = %+v, want a pooled entry with its own and the default bench times", smart)
	}
	if len(smart.Keys) != 4 {
		t.Fatalf("smart has %d keys, want the four it declares", len(smart.Keys))
	}
	a, b, again, unset := smart.Keys[0], smart.Keys[1], smart.Keys[2], smart.Keys[3]
	if a.Ref != "KEY_A" || a.Hint != credential.Hint("sk-ant-value-a") || a.Duplicate || a.Cooling != 0 {
		t.Errorf("KEY_A = %+v, want a ready reference", a)
	}
	if b.Ref != "KEY_B" || b.Hint != benched || b.Cooling <= 0 || b.Uses != 1 {
		t.Errorf("KEY_B = %+v, want the bench this node just took", b)
	}
	if !again.Duplicate || again.Hint != a.Hint || again.Uses != 0 {
		t.Errorf("the repeated KEY_A = %+v, want a duplicate with no state of its own", again)
	}
	if unset.Ref != "KEY_UNSET" || unset.Hint != "" {
		t.Errorf("KEY_UNSET = %+v, want a reference that resolved to nothing", unset)
	}

	fast := got[1]
	if !fast.Pooled || len(fast.Keys) != 1 || !fast.Keys[0].Default || fast.Keys[0].Ref != config.OpenAIKeyVar ||
		fast.Keys[0].Hint != credential.Hint("sk-openai-conventional") {
		t.Errorf("fast = %+v, want one key read from the conventional variable", fast)
	}

	if sub := got[2]; sub.Pooled || len(sub.Keys) != 0 {
		t.Errorf("subscription = %+v, want an entry with no key bag", sub)
	}
}

// NO VALUE EVER LEAVES. The report is what an operator surface serialises,
// so a value on it — in any field, under any name — is a credential on the
// wire.
func TestCredentialPoolsCarryNoValue(t *testing.T) {
	t.Parallel()
	e, _ := poolEngine(t)
	raw, err := json.Marshal(e.CredentialPools())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"sk-ant-value-a", "sk-ant-value-b", "sk-openai-conventional"} {
		if strings.Contains(string(raw), value) {
			t.Errorf("the report carries %q: %s", value, raw)
		}
	}
}

// A REFERENCE THAT RESOLVES TO NOTHING RUNS ON NOTHING. The provider used to
// fall back to the conventional variable whenever the bag it was handed was
// empty — and it is handed the RESOLVED bag, so `api_keys: [${ACME_KEY}]`
// with ACME_KEY unset ran on the company's ANTHROPIC_API_KEY.
func TestAnUnresolvedKeyNeverBorrowsTheConventionalOne(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	r := tierB(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-conventional", "LLM_BASE_URL": srv.URL})
	p, err := buildProvider("acme", config.LLMProvider{
		Type: config.LLMAnthropic, Model: "claude-test", BaseURL: "${LLM_BASE_URL}",
		APIKeys: []string{"${ACME_KEY}"},
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	mu.Lock()
	defer mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("the provider sent nothing, so this proves nothing about which key it sends")
	}
	for _, key := range sent {
		if key == "sk-ant-conventional" {
			t.Fatal("an entry naming ACME_KEY, unset, ran on ANTHROPIC_API_KEY")
		}
	}
}

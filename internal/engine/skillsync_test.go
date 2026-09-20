package engine_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/engine"
)

// wikiWithOneSkill is a Confluence instance holding a single skill page in TS,
// enough for the walk the engine runs: the space listing, and the one page.
func wikiWithOneSkill(t *testing.T, key string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(confluence.EncodeSkillPage(
		"key: "+key+"\ntitle: Deploying\nsummary: how this company deploys\n"+
			"phases: [execute]\ntrigger:\n  tool: deploy", "Tag the release."))
	if err != nil {
		t.Fatalf("encode the page body: %v", err)
	}
	page := fmt.Sprintf(`{"id":"1001","title":"Deploying","type":"page",`+
		`"space":{"key":"TS"},"body":{"storage":{"value":%s}},`+
		`"version":{"number":3},"ancestors":[],`+
		`"metadata":{"labels":{"results":[]}}}`, body)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/rest/api/content" && req.URL.Query().Get("start") == "0":
			fmt.Fprintf(rw, `{"results":[%s]}`, page)
		case req.URL.Path == "/rest/api/content":
			fmt.Fprint(rw, `{"results":[]}`)
		default:
			rw.WriteHeader(http.StatusNotFound)
			fmt.Fprint(rw, `{"message":"no such route"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// CONNECTING THE KNOWLEDGE BASE LOADS ITS SKILLS, AND DISCONNECTING IT RETIRES
// THEM.
//
// Neither used to happen: the skills walk ran once, at boot, from the
// notification wiring, and no apply ever reached the registry. So a company
// that connected Confluence after starting loaded no skills until the process
// restarted, and one that disconnected it went on serving the disconnected
// wiki's guidance for the life of the process, under a credential the
// revision had just revoked.
func TestConnectingTheKnowledgeBaseLoadsItsSkillsAndDisconnectingRetiresThem(t *testing.T) {
	t.Parallel()
	wiki := wikiWithOneSkill(t, "deploy")

	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e.Skills().Len() != 0 {
		t.Fatal("a company with no knowledge base has skills")
	}

	connected := companyDoc + `
integrations:
  confluence:
    url: ` + wiki.URL + `
    email: bot@example.com
    token: t
    webhook_secret: cf
knowledge:
  backend: confluence
`
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, connected)); err != nil {
		t.Fatalf("Apply with confluence: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := e.Skills().Get("deploy"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connecting the knowledge base loaded no skills: the apply " +
				"never walked the skills space")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// DISCONNECTED. The revision names another knowledge backend, so the
	// wiki's skills describe a stack this company no longer runs, and they
	// go with the apply rather than with the next restart.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc)); err != nil {
		t.Fatalf("Apply without confluence: %v", err)
	}
	if got := e.Skills().Len(); got != 0 {
		t.Fatalf("a disconnected knowledge base left %d skill(s) registered", got)
	}
}

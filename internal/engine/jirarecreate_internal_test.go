package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// jiraHookHost is a Data Center instance that already carries this engine's
// hook, converged: the right name, the right address, every event, enabled.
// It records every write to its webhook routes, which is the only place the
// difference between keeping a hook and recreating it shows.
type jiraHookHost struct {
	server *httptest.Server

	mu      sync.Mutex
	deleted []string
	created []string // the secret each create was signed with
	updated int
}

const (
	jiraHookHostToken = "org-token"
	jiraHookHostBase  = "https://engine.example.com"
)

func newJiraHookHost(t *testing.T) *jiraHookHost {
	t.Helper()
	h := &jiraHookHost{}
	const hooks = "/rest/webhooks/1.0/webhook"
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+jiraHookHostToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/2/myself":
			fmt.Fprint(w, `{"name":"crewlet-org","displayName":"Org"}`)
		case r.Method == http.MethodGet && r.URL.Path == hooks:
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"self":    h.server.URL + hooks + "/7",
				"name":    "crewlet",
				"url":     jiraHookHostBase + "/webhooks/jira",
				"events":  jira.WebhookEvents,
				"enabled": true,
			}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, hooks+"/"):
			h.deleted = append(h.deleted, strings.TrimPrefix(r.URL.Path, hooks+"/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == hooks:
			var body struct {
				Secret string `json:"secret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.created = append(h.created, body.Secret)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"self":%q,"name":"crewlet","url":%q,"enabled":true}`,
				h.server.URL+hooks+"/8", jiraHookHostBase+"/webhooks/jira")
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, hooks+"/"):
			h.updated++
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "no such route", http.StatusNotFound)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

// THE SETUP ROUTE'S recreate_webhooks REACHES THE JIRA PASS.
//
// POST /setup/integrations/{kind}/provision hands its `recreate_webhooks` to
// the pass as setup.PassInput.Recreate, and the GitHub, GitLab and Confluence
// passes passed it on. The Jira pass built its options without it, so a
// caller who asked Jira for a fresh secret — because the old one leaked, say
// — was answered 200 by a pass that left the hook and its secret exactly as
// they were, while the route's documentation said it re-registers with a
// fresh one.
//
// Driven through Run, against an instance whose hook is already converged, so
// that a pass which never hears the flag does nothing at all: one delete and
// one create are what recreating is, and the secret the new hook is signed
// with has to be a fresh one, recorded where the engine reads it.
func TestTheSetupRoutesRecreateReachesTheJiraPass(t *testing.T) {
	t.Parallel()
	const held = "the-secret-every-node-holds"
	for _, tc := range []struct {
		name     string
		recreate provision.FreshSwitch
	}{
		// THE PREMISE: without the flag, a converged hook is left alone, so
		// any write in the other case is the flag's doing.
		{"kept without it", ""},
		{"recreated with it", "recreate_webhooks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := newJiraHookHost(t)
			e := &Engine{environ: config.MapSource{
				"JIRA_ORG_TOKEN":      jiraHookHostToken,
				"JIRA_WEBHOOK_SECRET": held,
			}}
			cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  jira:
    url: ` + host.server.URL + `
    token: "${JIRA_ORG_TOKEN}"
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			company, err := NewCompanyWith(cfg, e.resolver())
			if err != nil {
				t.Fatalf("company: %v", err)
			}
			e.epoch.current.Store(company)

			sink := newSealingTestSink()
			if _, err := (&jiraPass{engine: e}).Run(t.Context(), setup.PassInput{
				Sink: sink, WebhookBase: jiraHookHostBase, Recreate: tc.recreate,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			host.mu.Lock()
			deleted, created, updated := host.deleted, host.created, host.updated
			host.mu.Unlock()
			recorded, _, _ := sink.Value(t.Context(), "JIRA_WEBHOOK_SECRET")
			if !tc.recreate.Asked() {
				if len(deleted)+len(created)+updated != 0 || recorded != "" {
					t.Fatalf("a pass not asked to recreate wrote to a converged "+
						"hook: deleted %v, created %d, updated %d, recorded %q",
						deleted, len(created), updated, recorded)
				}
				return
			}
			if len(deleted) != 1 || deleted[0] != "7" || len(created) != 1 || updated != 0 {
				t.Fatalf("recreate_webhooks: deleted %v, created %d, updated %d; "+
					"want the existing hook deleted and one created: the pass "+
					"never heard the flag the route was sent", deleted, len(created), updated)
			}
			if fresh := created[0]; fresh == "" || fresh == held {
				t.Errorf("the recreated hook is signed with %q, want a fresh secret "+
					"in place of the one every node holds", fresh)
			} else if recorded != fresh {
				t.Errorf("the sink holds %q for JIRA_WEBHOOK_SECRET, want the "+
					"secret the new hook signs with (%q)", recorded, fresh)
			}
		})
	}
}

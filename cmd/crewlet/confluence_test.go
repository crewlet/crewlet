package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What `crewlet confluence resync` is for.
//
// A tool skill that does not load is INVISIBLE — the registry is populated by
// one walk of one space at boot, and the only symptom of a page that failed to
// admit is guidance that never appears in a Plan prompt. This command runs the
// engine's own walk against a throwaway registry so an operator can see what
// the next boot will see, without restarting anything and without a running
// engine to ask.
//
// The cases below are about that promise: the walk reaches the right space,
// the counts are the registry's own, and a page that MEANT to be a skill and
// is not is reported loudly rather than silently counted as ordinary.

// skillPage is a Confluence storage-format body carrying skill frontmatter.
func skillPage(frontmatter, prose string) string {
	return `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">yaml` +
		`</ac:parameter><ac:plain-text-body><![CDATA[` + frontmatter +
		`]]></ac:plain-text-body></ac:structured-macro><p>` + prose + `</p>`
}

// confluenceStub serves one space's pages and records the queries it was asked.
type confluenceStub struct {
	*httptest.Server
	queries []string
}

func newConfluenceStub(t *testing.T, pages []map[string]any) *confluenceStub {
	t.Helper()
	stub := &confluenceStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.queries = append(stub.queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		// One short batch ends the walk, which is what a real space of
		// this size returns.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": pages, "size": len(pages),
		})
	}))
	t.Cleanup(stub.Close)
	return stub
}

func confluenceCompany(t *testing.T, url, skillsSpace string) string {
	t.Helper()
	doc := `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  confluence:
    url: ` + url + `
    email: bot@example.com
    token: "${CONFLUENCE_TOKEN}"
    webhook_secret: "${CONFLUENCE_WEBHOOK_SECRET}"
    skills_space: ` + skillsSpace + `
roles:
  - name: SWE
    handle: swe
    goal: ship
`
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func page(id, title, body string) map[string]any {
	return map[string]any{
		"id": id, "title": title, "type": "page",
		"space": map[string]any{"key": "SKILLS"},
		"body":  map[string]any{"storage": map[string]any{"value": body}},
	}
}

// THE WALK REPORTS WHAT THE REGISTRY WOULD HOLD: which pages admitted as
// skills, and which are ordinary pages the registry ignores.
func TestConfluenceResyncReportsWhatTheNextBootWouldLoad(t *testing.T) {
	stub := newConfluenceStub(t, []map[string]any{
		page("1", "Deploy", skillPage("---\nkey: deploy\nsummary: How to cut a release\ntrigger:\n  tool: run_pipeline\n---\n", "Tag the release.")),
		page("2", "Onboarding notes", `<p>Just some notes.</p>`),
	})
	t.Setenv("CONFLUENCE_TOKEN", "secret")

	var out, errs bytes.Buffer
	err := runConfluenceResync(
		[]string{confluenceCompany(t, stub.URL, "SKILLS"), "-config", filepath.Join(t.TempDir(), "absent.yaml")},
		&out, &errs)
	if err != nil {
		t.Fatalf("resync: %v\nstderr: %s", err, errs.String())
	}
	if !strings.Contains(out.String(), "SKILLS holds 2 page(s): 1 skill(s), 1 ordinary page(s).") {
		t.Errorf("summary line missing from:\n%s", out.String())
	}
	// THE KEY, not the title: the key is what a prompt loads a skill by, and
	// an operator checking for drift is checking that.
	if !strings.Contains(out.String(), "deploy") {
		t.Errorf("the admitted skill's key was not printed:\n%s", out.String())
	}
	// AND IT REALLY ASKED FOR THAT SPACE. Without this the case would pass
	// against a walk of the wrong container.
	if len(stub.queries) == 0 || !strings.Contains(stub.queries[0], "spaceKey=SKILLS") {
		t.Errorf("the walk asked %v, which does not name the skills space", stub.queries)
	}
}

// A PAGE THAT MEANT TO BE A SKILL AND DID NOT PARSE IS AN ERROR, not a line of
// output. Somebody wrote a trigger and got the rest wrong; counting it as an
// ordinary page is exactly how the guidance goes missing unnoticed.
func TestConfluenceResyncFailsOnAPageThatMeantToBeASkill(t *testing.T) {
	stub := newConfluenceStub(t, []map[string]any{
		page("1", "Broken deploy", skillPage("---\ntrigger:\n  tool: run_pipeline\nnot_a_setting: x\n---\n", "No key.")),
	})
	t.Setenv("CONFLUENCE_TOKEN", "secret")

	var out, errs bytes.Buffer
	err := runConfluenceResync(
		[]string{confluenceCompany(t, stub.URL, "SKILLS"), "-config", filepath.Join(t.TempDir(), "absent.yaml")},
		&out, &errs)
	if err == nil {
		t.Fatalf("a page declaring a trigger that does not parse was reported as success:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Broken deploy") {
		t.Errorf("the undecodable page was not named:\n%s", out.String())
	}
}

// -space OVERRIDES THE CONFIGURED SPACE, which is how an operator checks a
// container the company document does not name yet.
func TestConfluenceResyncHonoursTheSpaceFlag(t *testing.T) {
	stub := newConfluenceStub(t, nil)
	t.Setenv("CONFLUENCE_TOKEN", "secret")

	var out, errs bytes.Buffer
	if err := runConfluenceResync([]string{
		confluenceCompany(t, stub.URL, "SKILLS"), "-space", "OTHER",
		"-config", filepath.Join(t.TempDir(), "absent.yaml"),
	}, &out, &errs); err != nil {
		t.Fatalf("resync: %v\nstderr: %s", err, errs.String())
	}
	if len(stub.queries) == 0 || !strings.Contains(stub.queries[0], "spaceKey=OTHER") {
		t.Errorf("-space did not reach the walk: %v", stub.queries)
	}
}

// A COMPANY WITH NO CONFLUENCE BLOCK IS TOLD SO, rather than reaching an
// empty URL and reporting a transport failure.
func TestConfluenceResyncNeedsAConfiguredInstance(t *testing.T) {
	doc := "name: Nimbus\nroles:\n  - name: SWE\n    handle: swe\n    goal: ship\n"
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	err := runConfluenceResync([]string{path}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "integrations.confluence") {
		t.Fatalf("error = %v, want one naming integrations.confluence", err)
	}
}

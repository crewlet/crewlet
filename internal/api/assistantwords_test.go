package api_test

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// apiWords are what a card's words must never carry: a method before a path, a
// path, a route's wildcard, the revision summary's body key, the word API, and
// the em dash the product's copy does not use.
var apiWords = regexp.MustCompile(`\b(GET|PUT|POST|PATCH|DELETE)\b|/|\{|_summary|\bAPI\b|` + "\u2014")

// cardWords are the words of a preview a person reads before any value: its
// title, its approve label, what it says above and under the rows, and every
// row's label.
func cardWords(p proposal.Preview) []string {
	out := []string{p.Title, p.ApproveLabel, p.Summary, p.Note}
	for _, c := range p.Changes {
		out = append(out, c.Label)
	}
	return out
}

// opaqueWords match an id no person reads: a UUID, anywhere in a sentence.
var opaqueWords = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// computedRows are the rows whose value the card computes rather than the
// request sends: what a change that sends nothing applies to, and what a
// delete of nothing read deletes. A person reads them as the card's words.
var computedRows = []string{"object", "removes"}

// EVERY GENERIC CARD SPEAKS THE PERSON'S WORDS.
//
// For every write route the App mounts for the assistant, a change previewed
// against a resource is titled, labelled and noted in words: no method, no
// path, no wildcard, no `_summary`, no "API", and no opaque id. The request
// itself is the administrator's technical details, never a row. This is the
// class "Change through the API" over `PUT /config/units/core` and a raw JSON
// body belonged to. Every route is previewed twice: against a record with a
// name, and against one known only by its id while every id its path takes is
// a UUID, which the card names by what the record is called or in neutral
// words, never by that id. A verb nothing undoes (authz.IrreversibleOf) is
// destructive on every route it decides, and a route of one is never skipped.
// Mutation: title a change by its method, put the request back among the
// rows, name a record by the id its path carries, or rate a purge or an erase
// by its method, and this fails.
func TestEveryGenericCardSpeaksThePersonsWords(t *testing.T) {
	t.Parallel()
	routes := everySurfaceApp(t).AssistantRoutes()
	table, err := catalog.NewRouteTable(routes)
	if err != nil {
		t.Fatal(err)
	}
	admin := iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}}
	var change catalog.Tool
	for _, tool := range catalog.GenericTools(table, admin, nil) {
		if tool.Name == catalog.ProposeAPIChangeTool {
			change = tool
		}
	}
	const id = "7f3c2a9e-4b1d-4c8e-9a6f-2d5e8b1c0a47"
	for _, record := range []struct {
		name string
		// doc is every read's answer, and body every change's.
		doc  string
		body map[string]any
		// filled is what a wildcard takes: an id wildcard a UUID where
		// the record is known by one.
		filled func(wildcard string) string
	}{
		{"a named record", `{"name":"Example","goal":"Keep it running"}`,
			map[string]any{"name": "Example", "goal": "Keep it running well"},
			func(string) string { return "example" }},
		{"a record known by its id", `{"id":"` + id + `","goal":"Keep it running"}`,
			map[string]any{"goal": "Keep it running well"},
			func(wildcard string) string {
				if wildcard == "id" || wildcard == "cid" || strings.HasSuffix(wildcard, "_id") {
					return id
				}
				return "example"
			}},
	} {
		// THE RESOURCE AS A ROUTE WOULD ANSWER IT, and a check that admits
		// the change.
		read := func(_ context.Context, r exec.Request) (exec.Response, error) {
			if r.Method != http.MethodGet {
				return exec.Response{Status: http.StatusOK, Body: []byte(`{"valid":true}`)}, nil
			}
			return exec.Response{Status: http.StatusOK, Body: []byte(record.doc)}, nil
		}
		previewed := 0
		for _, r := range table.Routes() {
			if r.Method() == http.MethodGet || r.Method() == http.MethodHead || r.Pattern == catalog.ActRoute {
				continue
			}
			irreversible, _ := authz.IrreversibleOf(r.Action)
			path := map[string]any{}
			for _, segment := range strings.Split(r.Path(), "/") {
				if strings.HasPrefix(segment, "{") && segment != "{$}" {
					wildcard := strings.TrimSuffix(strings.Trim(segment, "{}"), "...")
					path[wildcard] = record.filled(wildcard)
				}
			}
			args := map[string]any{"route": r.Pattern, "path": path, "body": record.body,
				"summary": "Keep the example running well"}
			if r.Method() == http.MethodDelete {
				delete(args, "body")
			}
			p, err := change.Preview(t.Context(), read, time.Now(), args)
			switch {
			case err != nil && irreversible:
				t.Errorf("%s, %s: a verb nothing undoes previewed no card: %v", record.name,
					r.Pattern, err)
				continue
			case err != nil:
				// A route whose body this generic one does not fit refuses
				// it before any card, which is not a card to read.
				continue
			}
			previewed++
			words := cardWords(p)
			for _, c := range p.Changes {
				if slices.Contains(computedRows, c.Field) {
					words = append(words, c.To)
				}
			}
			for _, w := range words {
				if apiWords.MatchString(w) || opaqueWords.MatchString(w) {
					t.Errorf("%s, %s: a person reads %q", record.name, r.Pattern, w)
				}
			}
			if p.Technical == nil || p.Technical.Method != r.Method() {
				t.Errorf("%s, %s: an administrator's card carries the technical details %+v",
					record.name, r.Pattern, p.Technical)
			}
			if (r.Method() == http.MethodDelete || irreversible) && p.Risk != wire.RiskDestructive {
				t.Errorf("%s, %s: a change nothing undoes is at risk %q", record.name, r.Pattern,
					p.Risk)
			}
		}
		if previewed < 10 {
			t.Errorf("%s: only %d write routes previewed; the walk read nothing", record.name,
				previewed)
		}
	}
}

// A SEAT IS ADDED AND REMOVED IN THE PERSON'S WORDS, through the real unit
// route.
//
// Adding a Senior Software Engineer to Engineering is "Add seat Senior Software
// Engineer", its rows labelled by the seat's name; removing it again is
// "Remove seat Senior Software Engineer", destructive, saying what goes and
// that the configuration history can revert it. Mutation: label a member by
// its handle, or compute the risk from anything but the diff, and this fails.
func TestASeatIsAddedAndRemovedInThePersonsWords(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	cto := map[string]any{"name": "CTO", "handle": "cto", "llm": "zulu"}
	swe := map[string]any{"name": "Senior Software Engineer", "handle": "crewlet-swe",
		"llm": "zulu", "goal": "Ship the importer"}
	p, req := prepareChange(t, fx, read, map[string]any{
		"route": "PUT /config/units/{id}", "path": map[string]any{"id": "engineering"},
		"summary": "Add a senior software engineer",
		"body": map[string]any{"id": "engineering", "name": "Engineering",
			"roles": []any{cto, swe}}})
	if p.Title != "Add seat Senior Software Engineer" || p.ApproveLabel != "Approve and add" ||
		p.Risk != "" {
		t.Errorf("the seat's addition reads %q / %q at risk %q", p.Title, p.ApproveLabel, p.Risk)
	}
	labelled := false
	for _, c := range p.Changes {
		if strings.Contains(c.Label, "crewlet") {
			t.Errorf("row %s is labelled by the seat's handle: %q", c.Field, c.Label)
		}
		labelled = labelled || c.Label == "Senior Software Engineer › Goal"
	}
	if !labelled {
		t.Errorf("no row names the new seat's goal by the seat's name: %+v", p.Changes)
	}
	resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.Action(p.Act))), req)
	if err != nil || (resp.Status != http.StatusOK && resp.Status != http.StatusCreated) {
		t.Fatalf("the approved addition answered %d %s, %v", resp.Status, resp.Body, err)
	}

	p, _ = prepareChange(t, fx, read, map[string]any{
		"route": "PUT /config/units/{id}", "path": map[string]any{"id": "engineering"},
		"summary": "Remove the senior software engineer", "removes": []any{"roles.crewlet-swe"},
		"body": map[string]any{"id": "engineering", "name": "Engineering",
			"roles": []any{cto}}})
	if p.Title != "Remove seat Senior Software Engineer" || p.Risk != wire.RiskDestructive ||
		p.Summary != "This removes seat Senior Software Engineer from unit Engineering." ||
		!strings.Contains(p.Note, "configuration history") {
		t.Errorf("the seat's removal reads %q: %q, %q at risk %q", p.Title, p.Summary, p.Note,
			p.Risk)
	}
}

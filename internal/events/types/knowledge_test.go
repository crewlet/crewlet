package types

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
)

// A READ'S QUERY IS CLIPPED ON A RUNE AND SAYS SO. The query is a label a
// reader recognises a search by, and a clip through a multi-byte rune is
// invalid UTF-8 that a JSON encoder substitutes — so the stored label would
// not be the text anybody typed, nor valid text at all.
func TestAReadsQueryIsClippedOnARuneBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"a short query is untouched, trimmed", "  deploy rollback  ", "deploy rollback"},
		{"an empty query stays empty", "", ""},
		{"exactly at the cap is untouched",
			strings.Repeat("a", KnowledgeReadQueryMax), strings.Repeat("a", KnowledgeReadQueryMax)},
	} {
		if got := KnowledgeReadQuery(tc.query); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// Two-byte runes, so the cap falls between the bytes of one of them.
	long := strings.Repeat("é", KnowledgeReadQueryMax)
	got := KnowledgeReadQuery(long)
	if len(got) > KnowledgeReadQueryMax {
		t.Errorf("clipped query is %d bytes, over the %d cap", len(got), KnowledgeReadQueryMax)
	}
	if !utf8.ValidString(got) {
		t.Errorf("clipped query %q is not valid UTF-8: the cut went through a rune", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipped query %q does not say it was clipped", got)
	}
}

// AN UNKNOWN WAY IN IS A VALUE, NOT A FAILURE: a newer build's via decodes, and
// Valid is what tells a reader it is not one of this build's.
func TestAnUnknownReadViaIsAValueNotAPanic(t *testing.T) {
	t.Parallel()
	for _, via := range []KnowledgeReadVia{
		ReadViaGetPage, ReadViaSearch, ReadViaPrefetch, ReadViaSkillLoaded, ReadViaSkillInjected,
	} {
		if !via.Valid() {
			t.Errorf("%q is this build's own and reads as invalid", via)
		}
	}
	const raw = `{"id":"6f1c3d2e-0000-4000-8000-000000000031","type":"knowledge_read",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"cto","agent_id":"a",` +
		`"agent_handle":"cto","role":"CTO","turn_id":"run-1","via":"skimmed",` +
		`"backend":"native","pages":[{"id":"p1"}]}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	read, ok := events.DataAs[*KnowledgeRead](&ev)
	if !ok {
		t.Fatalf("decoded as %T", ev.Data)
	}
	if read.Via != "skimmed" || read.Via.Valid() {
		t.Errorf("via = %q valid=%v; want the newer build's value, reported unknown",
			read.Via, read.Via.Valid())
	}
	if len(read.Pages) != 1 || read.Pages[0].ID != "p1" || read.Pages[0].Rank != 0 {
		t.Errorf("pages = %+v", read.Pages)
	}
}

// A SKILL LOAD AN OLDER BUILD WROTE STILL DECODES, naming no page — which is
// the honest reading of a record from before a load could say where its skill
// came from — and re-encodes without inventing one.
func TestASkillUsedFromBeforeItNamedItsPageStillDecodes(t *testing.T) {
	t.Parallel()
	const raw = `{"id":"6f1c3d2e-0000-4000-8000-000000000032","type":"skill_used",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"cto","agent_id":"a",` +
		`"agent_handle":"cto","role":"CTO","turn_id":"run-1","skill_name":"deploys",` +
		`"skill_id":"","source_kind":"registry","file_loaded":""}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	used, ok := events.DataAs[*SkillUsed](&ev)
	if !ok {
		t.Fatalf("decoded as %T", ev.Data)
	}
	if used.SourcePageID != "" || used.SourceContainer != "" || used.SkillName != "deploys" {
		t.Errorf("decoded %+v", *used)
	}
	out, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if strings.Contains(string(out), "source_page_id") || strings.Contains(string(out), "source_container") {
		t.Errorf("re-encoding an old record invented a page: %s", out)
	}
}

// A feed line names the page for a single-page read and the query for a
// search, which is what a line about a read is scanned for.
func TestAReadsSummaryNamesThePageOrTheQuery(t *testing.T) {
	t.Parallel()
	one := []KnowledgeReadPage{{ID: "p1", Title: "Deploy Runbook"}}
	two := []KnowledgeReadPage{{ID: "p1"}, {ID: "p2"}}
	for _, tc := range []struct {
		read KnowledgeRead
		want string
	}{
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaGetPage, Pages: one}, "CTO read 'Deploy Runbook'"},
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaGetPage, Pages: []KnowledgeReadPage{{ID: "p9"}}},
			"CTO read 'p9'"},
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaSearch, Query: "deploys", Pages: two},
			`CTO searched knowledge for "deploys" (2 pages)`},
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaPrefetch, Pages: two}, "CTO was given 2 pages at turn start"},
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaSkillLoaded, Pages: one},
			"CTO loaded tool skill 'Deploy Runbook'"},
		{KnowledgeRead{RoleName: "CTO", Via: ReadViaSkillInjected, Pages: two},
			"CTO was offered tool skills from 2 pages"},
	} {
		if got := summaryOf(tc.read, ""); got != tc.want {
			t.Errorf("%s: summary = %q, want %q", tc.read.Via, got, tc.want)
		}
	}
}

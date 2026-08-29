package confluence_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
)

// A STATEFUL fake, unlike the canned-reply one the read-side tests use: an
// import is a sequence of reads and writes that have to agree with each
// other, and a server that answered the same thing twice would let a second
// run "create" a page the first one already made.
type wiki struct {
	*httptest.Server

	mu     sync.Mutex
	next   int
	pages  map[string]*wikiPage
	spaces map[string]bool
	// refuseLabel fails the label call, the one write whose failure must
	// not fail the page.
	refuseLabel bool
	// refuseWalk fails the space enumeration, which must stop a prune
	// rather than shrink it.
	refuseWalk bool
	// refuseDelete refuses the delete, so an orphan that stays is
	// reported rather than counted as pruned.
	refuseDelete bool
	// lockedTitle refuses updates to one page — the transient failure a
	// prune must not read as "this skill is gone".
	lockedTitle string
}

type wikiPage struct {
	ID      string
	Space   string
	Title   string
	Body    string
	Labels  []string
	Version int
}

func newWiki(t *testing.T, spaces ...string) *wiki {
	t.Helper()
	w := &wiki{pages: map[string]*wikiPage{}, spaces: map[string]bool{}}
	for _, key := range spaces {
		w.spaces[strings.ToUpper(key)] = true
	}
	w.Server = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.Close)
	return w
}

func (w *wiki) serve(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var body json.RawMessage
	_ = json.NewDecoder(req.Body).Decode(&body)
	path := strings.TrimPrefix(req.URL.Path, "/rest/api")
	rw.Header().Set("Content-Type", "application/json")

	switch {
	case strings.HasPrefix(path, "/space/"):
		if !w.spaces[strings.ToUpper(strings.TrimPrefix(path, "/space/"))] {
			rw.WriteHeader(http.StatusNotFound)
			fmt.Fprint(rw, `{"message":"no such space"}`)
			return
		}
		fmt.Fprint(rw, `{}`)

	case strings.HasSuffix(path, "/label") && req.Method == http.MethodPost:
		if w.refuseLabel {
			rw.WriteHeader(http.StatusForbidden)
			fmt.Fprint(rw, `{"message":"labels are restricted"}`)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/content/"), "/label")
		var labels []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &labels)
		if page := w.pages[id]; page != nil {
			for _, label := range labels {
				page.Labels = append(page.Labels, label.Name)
			}
		}
		fmt.Fprint(rw, `{}`)

	case path == "/content" && req.Method == http.MethodGet:
		q := req.URL.Query()
		if title := q.Get("title"); title != "" {
			for _, page := range w.pages {
				if page.Title == title &&
					strings.EqualFold(page.Space, q.Get("spaceKey")) {
					fmt.Fprintf(rw, `{"results":[%s]}`, page.wire())
					return
				}
			}
			fmt.Fprint(rw, `{"results":[]}`)
			return
		}
		// The space walk.
		if w.refuseWalk {
			rw.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(rw, `{"message":"upstream is unwell"}`)
			return
		}
		var rows []string
		if q.Get("start") == "" || q.Get("start") == "0" {
			for _, id := range w.sortedIDs() {
				page := w.pages[id]
				if strings.EqualFold(page.Space, q.Get("spaceKey")) {
					rows = append(rows, page.wire())
				}
			}
		}
		fmt.Fprintf(rw, `{"results":[%s]}`, strings.Join(rows, ","))

	case path == "/content" && req.Method == http.MethodPost:
		var in struct {
			Title string `json:"title"`
			Space struct {
				Key string `json:"key"`
			} `json:"space"`
			Body struct {
				Storage struct {
					Value string `json:"value"`
				} `json:"storage"`
			} `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		w.next++
		page := &wikiPage{
			ID: fmt.Sprintf("p%d", w.next), Space: in.Space.Key,
			Title: in.Title, Body: in.Body.Storage.Value, Version: 1,
		}
		w.pages[page.ID] = page
		fmt.Fprint(rw, page.wire())

	case strings.HasPrefix(path, "/content/") && req.Method == http.MethodPut:
		id := strings.TrimPrefix(path, "/content/")
		var in struct {
			Title string `json:"title"`
			Body  struct {
				Storage struct {
					Value string `json:"value"`
				} `json:"storage"`
			} `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		page := w.pages[id]
		if page == nil {
			rw.WriteHeader(http.StatusNotFound)
			fmt.Fprint(rw, `{"message":"gone"}`)
			return
		}
		if w.lockedTitle != "" && page.Title == w.lockedTitle {
			rw.WriteHeader(http.StatusForbidden)
			fmt.Fprint(rw, `{"message":"this page is restricted"}`)
			return
		}
		page.Title, page.Body = in.Title, in.Body.Storage.Value
		page.Version++
		fmt.Fprint(rw, page.wire())

	case strings.HasPrefix(path, "/content/") && req.Method == http.MethodDelete:
		if w.refuseDelete {
			rw.WriteHeader(http.StatusForbidden)
			fmt.Fprint(rw, `{"message":"only a space admin may delete"}`)
			return
		}
		delete(w.pages, strings.TrimPrefix(path, "/content/"))
		rw.WriteHeader(http.StatusNoContent)

	default:
		rw.WriteHeader(http.StatusNotFound)
		fmt.Fprint(rw, `{"message":"no such route"}`)
	}
}

func (w *wiki) sortedIDs() []string {
	out := make([]string, 0, len(w.pages))
	for id := range w.pages {
		out = append(out, id)
	}
	// Insertion order is not stable across map walks; sorting keeps the
	// fake's answer the same on every run.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (p *wikiPage) wire() string {
	labels := make([]string, 0, len(p.Labels))
	for _, name := range p.Labels {
		labels = append(labels, fmt.Sprintf(`{"name":%q}`, name))
	}
	body, _ := json.Marshal(p.Body)
	return fmt.Sprintf(
		`{"id":%q,"title":%q,"type":"page","space":{"key":%q},`+
			`"body":{"storage":{"value":%s}},"version":{"number":%d},`+
			`"metadata":{"labels":{"results":[%s]}}}`,
		p.ID, p.Title, p.Space, body, p.Version, strings.Join(labels, ","))
}

// seed puts a page into the space directly, the way a person writing in the
// UI would — with no label unless one is named.
func (w *wiki) seed(space, title, body string, labels ...string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	id := fmt.Sprintf("p%d", w.next)
	w.pages[id] = &wikiPage{
		ID: id, Space: space, Title: title, Body: body,
		Labels: labels, Version: 1,
	}
	return id
}

func (w *wiki) titles(space string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, id := range w.sortedIDs() {
		if page := w.pages[id]; strings.EqualFold(page.Space, space) {
			out = append(out, page.Title)
		}
	}
	return out
}

// seedIDOf finds the id of a page by space and title, so a test can assert
// about a page the importer created rather than one it seeded.
func (w *wiki) seedIDOf(t *testing.T, space, title string) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range w.sortedIDs() {
		page := w.pages[id]
		if strings.EqualFold(page.Space, space) && page.Title == title {
			return id
		}
	}
	t.Fatalf("%s/%s was never written", space, title)
	return ""
}

// refuseUpdate makes the next write to one page fail, which is the
// transient 403 a prune must not read as "this skill is gone".
func (w *wiki) refuseUpdate(t *testing.T, title string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lockedTitle = title
}

func (w *wiki) labelsOf(id string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if page := w.pages[id]; page != nil {
		return append([]string(nil), page.Labels...)
	}
	return nil
}

func wikiClient(t *testing.T, w *wiki) *confluence.Client {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{URL: w.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const importedSkill = `---
key: deploy
title: Deploying
summary: How this company deploys.
phases: [execute]
trigger:
  mcp_server: gitlab
---

# Deploying

- Tag the release.
`

const retiredSkill = `---
key: retired
title: The Old Way
summary: Nobody does this any more.
phases: [execute]
trigger:
  mcp_server: gitlab
---

# The Old Way

- Do not.
`

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func publish(t *testing.T, w *wiki, root, skillsSpace string, prune bool) *confluence.PublishResult {
	t.Helper()
	plan, err := confluence.Walk(root, skillsSpace)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	res, err := confluence.Publish(context.Background(), confluence.PublishOptions{
		Client: wikiClient(t, w), Plan: plan, Prune: prune, SkillsSpace: skillsSpace,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return res
}

// A SKILL FILE GOES TO THE SKILLS SPACE whatever directory it sits in, and a
// doc goes to the space its directory names.
func TestAFileIsRoutedByWhatItDeclaresNotWhereItSits(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS", "ENG")
	root := tree(t, map[string]string{
		"ENG/deploy.md":     importedSkill,
		"ENG/onboarding.md": "# Onboarding\n\nWelcome.\n",
	})
	res := publish(t, w, root, "TS", false)
	if len(res.Created) != 2 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := w.titles("TS"); len(got) != 1 || got[0] != "Deploying" {
		t.Errorf("the skills space holds %v", got)
	}
	if got := w.titles("ENG"); len(got) != 1 || got[0] != "Onboarding" {
		t.Errorf("ENG holds %v", got)
	}
}

// PROVENANCE IS STAMPED, because it is the only thing that later tells a
// page this tool wrote from a page a person wrote.
func TestEverySkillPagePublishedIsLabelled(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS", "ENG")
	root := tree(t, map[string]string{
		"ENG/deploy.md":     importedSkill,
		"ENG/onboarding.md": "# Onboarding\n\nWelcome.\n",
	})
	publish(t, w, root, "TS", false)
	skill := w.seedIDOf(t, "TS", "Deploying")
	if got := w.labelsOf(skill); len(got) != 1 || got[0] != confluence.ImportedSkillLabel {
		t.Errorf("the skill page carries %v", got)
	}
	// AN ORDINARY DOC IS NOT LABELLED: the label means "prune may delete
	// this", and a knowledge doc is never pruned.
	doc := w.seedIDOf(t, "ENG", "Onboarding")
	if got := w.labelsOf(doc); len(got) != 0 {
		t.Errorf("a knowledge doc was labelled %v", got)
	}
}

// A LABEL THAT CANNOT BE WRITTEN IS A NOTE, NOT A FAILURE: the page is
// published and correct, and what was lost is the ability to prune it later.
func TestALabelFailureDoesNotFailThePage(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	w.refuseLabel = true
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	res := publish(t, w, root, "TS", false)
	if len(res.Created) != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !hasNote(res.Notes, "could not be labelled") {
		t.Errorf("nothing reported the missing label: %v", res.Notes)
	}
}

// THE ORPHAN IS THE ONE THIS RUN DID NOT PUBLISH.
func TestPruneDeletesTheSkillNoLocalFilePublishes(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	root := tree(t, map[string]string{
		"TS/deploy.md":  importedSkill,
		"TS/retired.md": retiredSkill,
	})
	publish(t, w, root, "TS", false)

	// The retired skill's source file is deleted, the way a real one goes.
	if err := os.Remove(filepath.Join(root, "TS", "retired.md")); err != nil {
		t.Fatal(err)
	}
	res := publish(t, w, root, "TS", true)
	if len(res.Pruned) != 1 || !strings.Contains(res.Pruned[0], "The Old Way") {
		t.Fatalf("pruned = %v", res.Pruned)
	}
	if got := w.titles("TS"); len(got) != 1 || got[0] != "Deploying" {
		t.Errorf("the space holds %v", got)
	}
}

// A HAND-AUTHORED SKILL HAS NO LABEL AND IS NEVER TOUCHED. A lead who wrote
// a skill in the wiki has no local .md, so every prune would otherwise
// delete their work.
func TestPruneLeavesAHandAuthoredSkillAlone(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	w.seed("TS", "Written By A Person",
		confluence.EncodeSkillPage(
			"key: byhand\ntitle: Written By A Person\nsummary: Theirs.\n"+
				"phases: [execute]\ntrigger:\n  mcp_server: gitlab", "Do it their way."))
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	res := publish(t, w, root, "TS", true)
	if len(res.Pruned) != 0 {
		t.Fatalf("pruned %v", res.Pruned)
	}
	if got := w.titles("TS"); len(got) != 2 {
		t.Errorf("the space holds %v", got)
	}
}

// AN ORDINARY PAGE FILED IN THE SKILLS SPACE IS NOT A SKILL, even carrying
// the label — and it is not a BROKEN skill either. A prune that asked the
// parser about every page would report a finding on the space home page on
// every run, which is the noise that trains an operator to ignore the notes.
func TestPruneLeavesAnOrdinaryPageAloneAndSaysNothingAboutIt(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	w.seed("TS", "Team Notes", "<p>Just notes.</p>", confluence.ImportedSkillLabel)
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	res := publish(t, w, root, "TS", true)
	if len(res.Pruned) != 0 {
		t.Fatalf("pruned %v", res.Pruned)
	}
	if hasNote(res.Notes, "does not parse") {
		t.Errorf("an ordinary page was reported as a broken skill: %v", res.Notes)
	}
}

// A PAGE THAT DECLARES A TRIGGER AND DOES NOT PARSE has an unknown key, so
// it cannot be matched against this run — deleting it would be deleting a
// page on the strength of a guess.
func TestPruneReportsAnUndecodableSkillRatherThanDeletingIt(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	w.seed("TS", "Half Written",
		confluence.EncodeSkillPage("trigger:\n  tool: run_pipeline", "Unfinished."),
		confluence.ImportedSkillLabel)
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	res := publish(t, w, root, "TS", true)
	if len(res.Pruned) != 0 {
		t.Fatalf("pruned %v", res.Pruned)
	}
	if !hasNote(res.Notes, "does not parse") {
		t.Errorf("the undecodable page was not reported: %v", res.Notes)
	}
}

// A PRUNE THAT CANNOT ENUMERATE DELETES NOTHING. The orphan set is derived
// by subtraction, so a partial read deletes live pages.
func TestAPruneThatCannotEnumerateDeletesNothing(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	plan, err := confluence.Walk(root, "TS")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.refuseWalk = true
	res, err := confluence.Publish(context.Background(), confluence.PublishOptions{
		Client: wikiClient(t, w), Plan: plan, Prune: true, SkillsSpace: "TS",
	})
	if err == nil {
		t.Fatal("an unreadable space was pruned anyway")
	}
	if res == nil || len(res.Pruned) != 0 {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(err.Error(), "nothing was pruned") {
		t.Errorf("error = %v", err)
	}
}

// AN ORPHAN THAT WILL NOT DELETE IS REPORTED, because it stays in every
// planner's tool-skill catalogue until somebody removes it by hand.
func TestAnUndeletableOrphanIsReported(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	root := tree(t, map[string]string{
		"TS/deploy.md":  importedSkill,
		"TS/retired.md": retiredSkill,
	})
	publish(t, w, root, "TS", false)
	if err := os.Remove(filepath.Join(root, "TS", "retired.md")); err != nil {
		t.Fatal(err)
	}
	w.refuseDelete = true
	res := publish(t, w, root, "TS", true)
	if len(res.Pruned) != 0 {
		t.Fatalf("pruned %v", res.Pruned)
	}
	if !hasNote(res.Notes, "could not be deleted") {
		t.Errorf("nothing reported the orphan: %v", res.Notes)
	}
}

// NO SKILLS SPACE MEANS NO PRUNE, whatever the flag says: a prune with no
// container to scope it is a delete pass over the instance.
func TestAPruneWithNoSkillsSpaceIsRefusedAndSaysSo(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "ENG")
	w.seed("ENG", "Deploying", confluence.EncodeSkillPage(
		"key: deploy\ntitle: Deploying\nsummary: x\nphases: [execute]\n"+
			"trigger:\n  mcp_server: gitlab", "Tag it."),
		confluence.ImportedSkillLabel)
	root := tree(t, map[string]string{"ENG/onboarding.md": "# Onboarding\n\nHi.\n"})
	res := publish(t, w, root, "", true)
	if len(res.Pruned) != 0 {
		t.Fatalf("pruned %v", res.Pruned)
	}
	if !hasNote(res.Notes, "no tool-skills space") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// A SKILL PAGE THAT FAILED TO PUBLISH IS NOT PUBLISHED, so the run must not
// then prune it as an orphan of itself.
func TestASkillThatFailedToWriteIsNotPrunedAsAnOrphan(t *testing.T) {
	t.Parallel()
	w := newWiki(t, "TS")
	root := tree(t, map[string]string{"TS/deploy.md": importedSkill})
	publish(t, w, root, "TS", false)
	w.refuseUpdate(t, "Deploying")
	res := publish(t, w, root, "TS", true)
	if len(res.Failed) != 1 {
		t.Fatalf("result = %+v", res)
	}
	// The page is still there, and this run could not confirm it. Deleting
	// it would destroy the only copy of a skill nothing could rewrite.
	if got := w.titles("TS"); len(got) != 1 {
		t.Errorf("the space holds %v", got)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("a page this run failed to write was pruned: %v", res.Pruned)
	}
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

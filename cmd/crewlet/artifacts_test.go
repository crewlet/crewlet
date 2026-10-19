package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifactupload"
)

// fakeArtifactsNode answers the artifacts surface, recording what it was sent.
type fakeArtifactsNode struct {
	server *httptest.Server

	mu        sync.Mutex
	carried   []string
	intents   []artifacts.UploadRequest
	keys      []string
	puts      map[string]string
	creates   []map[string]any
	publishes []map[string]any
	ifMatch   []string
	shares    []map[string]any
	purges    int
	refuse    map[string]any
}

const cliArtifact = "0a6f1c8e-6a41-5b6e-9d8c-3f1e2d4c5b6a"

func newFakeArtifactsNode(t *testing.T) *fakeArtifactsNode {
	t.Helper()
	n := &fakeArtifactsNode{puts: map[string]string{}}
	answer := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		if n.refuse != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(n.refuse)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	intent := func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		var req artifacts.UploadRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		n.intents = append(n.intents, req)
		n.keys = append(n.keys, r.Header.Get(opkey.Header))
		out := artifacts.UploadIntent{Upload: "u-1", Carried: n.carried}
		for _, e := range req.Entries {
			if !slices.Contains(n.carried, e.Path) {
				out.Transfers = append(out.Transfers, artifacts.Transfer{Path: e.Path, Method: "PUT",
					URL: artifactupload.TransferPath("u-1", e.Path)})
			}
		}
		answer(w, http.StatusCreated, out)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /artifacts/uploads", intent)
	mux.HandleFunc("POST /artifacts/{id}/uploads", intent)
	mux.HandleFunc("PUT /artifacts/uploads/{upload}/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.puts[r.PathValue("path")] = string(raw)
		n.mu.Unlock()
		answer(w, http.StatusOK, map[string]any{"stored": true})
	})
	record := func(into *[]map[string]any, number int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			n.mu.Lock()
			*into = append(*into, body)
			n.keys = append(n.keys, r.Header.Get(opkey.Header))
			n.ifMatch = append(n.ifMatch, r.Header.Get("If-Match"))
			n.mu.Unlock()
			answer(w, http.StatusOK, map[string]any{"artifact": cliArtifact, "number": number,
				"outcome": "applied"})
		}
	}
	mux.HandleFunc("POST /artifacts", record(&n.creates, 1))
	mux.HandleFunc("POST /artifacts/{id}/versions", record(&n.publishes, 4))
	mux.HandleFunc("PUT /artifacts/{id}/shares", record(&n.shares, 0))
	mux.HandleFunc("GET /artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "head_number": 3})
	})
	mux.HandleFunc("GET /artifacts/sources", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, map[string]any{"sources": dashspec.Catalogue()})
	})
	mux.HandleFunc("POST /artifacts/{id}/purge", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.purges++
		n.mu.Unlock()
		answer(w, http.StatusOK, map[string]any{"outcome": "applied"})
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// site is a directory of files to publish, a dotted one beside them.
func site(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":     "<!doctype html><link rel=stylesheet href=style.css>",
		"style.css":      "body{margin:0}",
		"img/logo.svg":   "<svg/>",
		".git/HEAD":      "ref: refs/heads/main",
		".DS_Store":      "junk",
		"notes/.private": "not published",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A FIRST PUBLISH DECLARES EVERY FILE, SENDS EACH, AND CREATES THE ARTIFACT: a
// dotted name is left out, the entry is index.html, and the intent and the
// create each carry an operation key.
func TestArtifactsPublishCreatesFromADirectory(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	stdout, stderr, err := cli(t, "artifacts", "publish", site(t), "-title", "Launch plan",
		"-work-item", "CRW-412", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	var declared []string
	for _, e := range node.intents[0].Entries {
		declared = append(declared, e.Path)
	}
	slices.Sort(declared)
	if !slices.Equal(declared, []string{"img/logo.svg", "index.html", "style.css"}) {
		t.Errorf("declared %v", declared)
	}
	if node.puts["img/logo.svg"] != "<svg/>" || len(node.puts) != 3 {
		t.Errorf("sent %v", node.puts)
	}
	create := node.creates[0]
	if create["title"] != "Launch plan" || create["kind"] != "html" || create["entry"] != "index.html" ||
		create["upload"] != "u-1" || create["work_item"] != "CRW-412" {
		t.Errorf("created %v", create)
	}
	if len(node.keys) != 2 || node.keys[0] == "" || node.keys[1] == "" || node.keys[0] == node.keys[1] {
		t.Errorf("the intent and the create were keyed %v", node.keys)
	}
	if !strings.Contains(stdout, "version 1") {
		t.Errorf("printed %q", stdout)
	}
}

// A NEW VERSION SENDS ONLY THE BYTES THE ARTIFACT DOES NOT HOLD: the files the
// intent found are carried by their digest, the rest uploaded, and the publish
// names the head it read as its base.
func TestArtifactsPublishSendsOnlyNewBytes(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	node.carried = []string{"style.css", "img/logo.svg"}
	if _, stderr, err := cli(t, "artifacts", "publish", site(t), "-artifact", cliArtifact,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if len(node.puts) != 1 || node.puts["index.html"] == "" {
		t.Errorf("sent %v, want index.html alone", node.puts)
	}
	if got := node.ifMatch[len(node.ifMatch)-1]; got != `"3"` {
		t.Errorf("If-Match = %q, want the head read", got)
	}
	files := node.publishes[0]["files"].([]any)
	carriedBy := map[string]bool{}
	for _, f := range files {
		spec := f.(map[string]any)
		carriedBy[spec["path"].(string)] = spec["carry"] != nil
	}
	if !carriedBy["style.css"] || !carriedBy["img/logo.svg"] || carriedBy["index.html"] {
		t.Errorf("the publish carried %v", carriedBy)
	}
}

// A SHARE NAMES ITS AUDIENCE AND ITS ROLE, and a role the share cannot grant is
// refused before anything is sent.
func TestArtifactsShareNamesTheAudienceAndTheRole(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	if _, _, err := cli(t, "artifacts", "share", cliArtifact, "seat:bob", "owner", cfg); err == nil {
		t.Error("a share granting owner was sent")
	}
	if _, stderr, err := cli(t, "artifacts", "share", cliArtifact, "unit:ENG", "editor", cfg); err != nil {
		t.Fatalf("share: %v\n%s", err, stderr)
	}
	if len(node.shares) != 1 {
		t.Fatalf("shares sent: %v", node.shares)
	}
	got := node.shares[0]
	if a := got["audience"].(map[string]any); a["kind"] != "unit" || a["id"] != "ENG" || got["role"] != "editor" {
		t.Errorf("shared %v", got)
	}
}

// A PURGE ASKS FOR THE ID TWICE, and sends nothing until it has it.
func TestArtifactsPurgeAsksForTheIDTwice(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	if _, _, err := cli(t, "artifacts", "purge", cliArtifact, cfg); err == nil || node.purges != 0 {
		t.Errorf("a purge with no confirmation: %v, %d sent", err, node.purges)
	}
	if _, stderr, err := cli(t, "artifacts", "purge", cliArtifact, "-confirm", cliArtifact, cfg); err != nil ||
		node.purges != 1 {
		t.Errorf("a confirmed purge: %v %s, %d sent", err, stderr, node.purges)
	}
}

// AN UNKNOWN OUTCOME PRINTS THE OPERATION TO RETRY UNDER, never a fresh one.
func TestArtifactsAnUnknownOutcomeNamesTheOperation(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	node.refuse = map[string]any{"error": "unavailable", "outcome": "unknown", "op_id": "x"}
	_, _, err := cli(t, "artifacts", "share", cliArtifact, "seat:bob", "viewer",
		bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "-op-id") {
		t.Errorf("an unknown outcome: %v", err)
	}
}

// A NEW VERSION KEEPS THE HEAD'S CAPABILITIES UNLESS -capabilities SAYS
// OTHERWISE: a publish that does not name the flag sends no capabilities, which
// the node reads as "keep the head's", so republishing a page that reads its
// datastore never strips `db` from it. Naming the flag declares that set, and
// `none` (or an empty value) declares none on purpose.
func TestArtifactsPublishKeepsTheHeadsCapabilitiesUnlessNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want any // nil: the field is absent
	}{
		{"absent", nil, nil},
		{"named", []string{"-capabilities", "db, state"}, []any{"db", "state"}},
		{"none", []string{"-capabilities", "none"}, []any{}},
		{"empty", []string{"-capabilities", ""}, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeArtifactsNode(t)
			args := append([]string{"artifacts", "publish", site(t), "-artifact", cliArtifact},
				tc.args...)
			if _, stderr, err := cli(t, append(args, bootstrapForURL(t, node.server.URL))...); err != nil {
				t.Fatalf("publish: %v\n%s", err, stderr)
			}
			got, present := node.publishes[0]["capabilities"]
			if tc.want == nil {
				if present {
					t.Errorf("sent capabilities %v, want the field absent so the head's are kept", got)
				}
				return
			}
			if !present || !slices.Equal(anyStrings(got), anyStrings(tc.want)) {
				t.Errorf("sent capabilities %v (present %v), want %v", got, present, tc.want)
			}
		})
	}
}

// A capability other than one this build knows is refused before a byte moves.
func TestArtifactsPublishRefusesAnUnknownCapability(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	_, _, err := cli(t, "artifacts", "publish", site(t), "-capabilities", "db,camera",
		bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "camera") {
		t.Fatalf("publish = %v, want the unknown capability named", err)
	}
	if len(node.intents) != 0 {
		t.Errorf("declared %d uploads before refusing", len(node.intents))
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// writeTree writes files under a fresh directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// THE KIND IS INFERRED FROM THE ENTRY BY THE KIND TABLE: a directory whose one
// default entry is a Markdown file creates a document, and the entry it opens
// on is that file.
func TestArtifactsPublishInfersTheKindFromTheEntry(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	dir := writeTree(t, map[string]string{"index.md": "# Plan", "notes.txt": "more"})
	if _, stderr, err := cli(t, "artifacts", "publish", dir, bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	create := node.creates[0]
	if create["kind"] != string(artifacts.KindDoc) || create["entry"] != "index.md" {
		t.Errorf("created %v", create)
	}
}

// A DECK IS TOLD APART BY ITS INDEX'S PATH, not its type: a directory holding
// `deck.json` and its slides creates slides opening on the index, while a
// JSON file anywhere else infers no kind at all.
func TestArtifactsPublishInfersADeckFromItsIndex(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	dir := writeTree(t, map[string]string{
		"deck.json":         `{"title":"Plan","order":["cover"]}`,
		"slides/cover.html": `<section id="cover" style="background:#ffffff"><h1>Plan</h1></section>`,
	})
	if _, stderr, err := cli(t, "artifacts", "publish", dir, bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if create := node.creates[0]; create["kind"] != string(artifacts.KindSlides) || create["entry"] != "deck.json" {
		t.Errorf("created %v", create)
	}
	other := writeTree(t, map[string]string{"data.json": `{}`})
	if _, _, err := cli(t, "artifacts", "publish", other, bootstrapForURL(t, node.server.URL)); err == nil ||
		!strings.Contains(err.Error(), "-kind") {
		t.Errorf("a lone JSON file answered %v", err)
	}
}

// TWO DEFAULT ENTRIES ARE THE AUTHOR'S TO CHOOSE BETWEEN: a directory holding
// both is refused before a byte moves, and -kind settles it.
func TestArtifactsPublishRefusesTwoCandidateEntries(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	dir := writeTree(t, map[string]string{"index.html": "<!doctype html>", "index.md": "# Plan"})
	_, _, err := cli(t, "artifacts", "publish", dir, bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "index.html and index.md") {
		t.Fatalf("publish of two entries answered %v", err)
	}
	if len(node.intents) != 0 {
		t.Fatalf("the refused publish declared %d uploads", len(node.intents))
	}
	if _, stderr, err := cli(t, "artifacts", "publish", dir, "-kind", "doc",
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish with -kind: %v\n%s", err, stderr)
	}
	if create := node.creates[0]; create["kind"] != "doc" || create["entry"] != "index.md" {
		t.Errorf("created %v", create)
	}
}

// THE CATALOGUE PRINTS FROM THE NODE, whole or one source, and the optional
// name never reads as the optional config path.
func TestArtifactsSourcesPrintsTheCatalogue(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	out, stderr, err := cli(t, "artifacts", "sources", cfg)
	if err != nil {
		t.Fatalf("sources: %v\n%s", err, stderr)
	}
	for _, src := range dashspec.Catalogue() {
		if !strings.Contains(out, src.Name+"  ("+string(src.Domain)+")") {
			t.Errorf("the catalogue's print lacks %s:\n%s", src.Name, out)
		}
	}
	out, stderr, err = cli(t, "artifacts", "sources", "spend.daily", cfg)
	if err != nil {
		t.Fatalf("sources spend.daily: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "param group: seat|phase") || strings.Contains(out, "tracker.items") {
		t.Errorf("one source printed:\n%s", out)
	}
	if _, _, err := cli(t, "artifacts", "sources", "crm.deals", cfg); err == nil {
		t.Error("an unknown source printed nothing and succeeded")
	}
}

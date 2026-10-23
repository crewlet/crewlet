package main

import (
	"cmp"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	patches   []map[string]any
	ifMatch   []string
	files     map[string]int
	head      map[string]any
	versions  []map[string]any
	shares    []map[string]any
	purges    int
	refuse    map[string]any

	// outcome is what a create or a publish answers, `applied` unset.
	outcome string
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
			outcome := cmp.Or(n.outcome, "applied")
			n.mu.Unlock()
			out := map[string]any{"artifact": cliArtifact, "number": number, "outcome": outcome}
			if n.files != nil {
				out["files"] = n.files
			}
			answer(w, http.StatusOK, out)
		}
	}
	mux.HandleFunc("POST /artifacts", record(&n.creates, 1))
	mux.HandleFunc("POST /artifacts/{id}/versions", record(&n.publishes, 4))
	mux.HandleFunc("PUT /artifacts/{id}/shares", record(&n.shares, 0))
	mux.HandleFunc("PATCH /artifacts/{id}", record(&n.patches, 0))
	mux.HandleFunc("GET /artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		head := map[string]any{"id": r.PathValue("id"), "head_number": 3}
		maps.Copy(head, n.head)
		answer(w, http.StatusOK, head)
	})
	mux.HandleFunc("GET /artifacts/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"versions": n.versions})
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

// tree writes files (slash-separated path to body) under a fresh directory.
func tree(t *testing.T, files map[string]string) string {
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

// declaredPaths is every path the node's first upload intent declared, sorted.
func (n *fakeArtifactsNode) declaredPaths() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.intents[0].Entries {
		out = append(out, e.Path)
	}
	slices.Sort(out)
	return out
}

// -exclude LEAVES OUT WHAT IT MATCHES, AND ONLY THAT: a pattern without a slash
// matches a name at any depth, one with a slash the path from the directory, a
// directory matched takes everything beneath it, and the walker's own rules
// (no dotted name, no symbolic link followed) hold beside it.
func TestArtifactsPublishExcludesWhatThePatternsMatch(t *testing.T) {
	t.Parallel()
	outside := tree(t, map[string]string{"secret.txt": "outside", "dir/also.txt": "outside"})
	dir := tree(t, map[string]string{
		"index.html":         "<!doctype html>",
		"app.js":             "1",
		"app.js.map":         "{}",
		"lib/vendor.js.map":  "{}",
		"lib/vendor.js":      "2",
		"drafts/old.html":    "old",
		"drafts/deep/x.html": "x",
		"notes/a.md":         "a",
		"notes/keep.txt":     "k",
		"notes/sub/b.md":     "b",
		".env":               "SECRET=1",
		"img/.hidden.png":    "h",
		"img/logo.svg":       "<svg/>",
	})
	for link, target := range map[string]string{"linked.txt": "secret.txt", "linked": "dir"} {
		if err := os.Symlink(filepath.Join(outside, target), filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	node := newFakeArtifactsNode(t)
	if _, stderr, err := cli(t, "artifacts", "publish", dir, "-exclude", "*.map", "-exclude", "./drafts/",
		"-exclude", "notes/*.md", bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	want := []string{"app.js", "img/logo.svg", "index.html", "lib/vendor.js", "notes/keep.txt", "notes/sub/b.md"}
	if got := node.declaredPaths(); !slices.Equal(got, want) {
		t.Errorf("declared %v, want %v", got, want)
	}
}

// A MALFORMED OR ESCAPING PATTERN IS REFUSED BEFORE ANYTHING IS SENT, and so is
// -exclude beside a single file, which has nothing to leave out.
func TestArtifactsPublishRefusesABadExclusion(t *testing.T) {
	t.Parallel()
	dir := site(t)
	for name, args := range map[string][]string{
		"malformed":     {dir, "-exclude", "[a-"},
		"escaping":      {dir, "-exclude", "../site"},
		"absolute":      {dir, "-exclude", "/index.html"},
		"empty":         {dir, "-exclude", " "},
		"a single file": {filepath.Join(dir, "index.html"), "-exclude", "*.css"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := newFakeArtifactsNode(t)
			args := append(append([]string{"artifacts", "publish"}, args...), bootstrapForURL(t, node.server.URL))
			if _, _, err := cli(t, args...); err == nil {
				t.Fatal("the publish was not refused")
			}
			if len(node.intents) != 0 {
				t.Errorf("declared %d uploads before refusing", len(node.intents))
			}
		})
	}
}

// A ROOT THAT IS ITSELF A LINK IS WALKED WHERE IT POINTS: naming it is the
// operator's choice, and walked as a link it held nothing to publish.
func TestArtifactsPublishWalksALinkedRoot(t *testing.T) {
	t.Parallel()
	target := tree(t, map[string]string{"index.html": "<!doctype html>", "css/app.css": "a{}"})
	link := filepath.Join(t.TempDir(), "dist")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	node := newFakeArtifactsNode(t)
	if _, stderr, err := cli(t, "artifacts", "publish", link, bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if got := node.declaredPaths(); !slices.Equal(got, []string{"css/app.css", "index.html"}) {
		t.Errorf("declared %v", got)
	}
}

// A NEW ARTIFACT CARRIES ITS DESCRIPTION AND ICON in the create, beside its
// title.
func TestArtifactsPublishDescribesANewArtifact(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	if _, stderr, err := cli(t, "artifacts", "publish", site(t), "-description", "The Q3 launch plan",
		"-icon", "rocket", bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	create := node.creates[0]
	if create["title"] != "index" || create["description"] != "The Q3 launch plan" || create["icon"] != "rocket" {
		t.Errorf("created %v", create)
	}
}

// A NEW VERSION DESCRIBES NOTHING BUT ITSELF: a head field or the kind named
// beside -artifact is refused before anything is sent, pointing at `update`,
// rather than dropped as if it had landed.
func TestArtifactsPublishRefusesHeadFieldsOnANewVersion(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-title", "-description", "-icon", "-kind"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			node := newFakeArtifactsNode(t)
			_, _, err := cli(t, "artifacts", "publish", site(t), "-artifact", cliArtifact, flag, "html",
				bootstrapForURL(t, node.server.URL))
			if err == nil || !strings.Contains(err.Error(), flag) {
				t.Fatalf("publish %s on a new version = %v, want it refused by name", flag, err)
			}
			if flag != "-kind" && !strings.Contains(err.Error(), "crewlet artifacts update") {
				t.Errorf("the refusal %q does not point at update", err)
			}
			if len(node.intents) != 0 || len(node.publishes) != 0 {
				t.Errorf("sent %d intents and %d publishes before refusing", len(node.intents),
					len(node.publishes))
			}
		})
	}
}

// AN UPDATE SENDS EXACTLY THE FIELDS IT NAMES, an empty one included (which
// clears it), and refuses a call that names none before anything is sent.
func TestArtifactsUpdateSendsOnlyTheNamedFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want map[string]any
		says string
	}{
		{"title", []string{"-title", "Launch plan"}, map[string]any{"title": "Launch plan"},
			"updated the title of " + cliArtifact},
		{"clear the description", []string{"-description", "", "-icon", "rocket"},
			map[string]any{"description": "", "icon": "rocket"},
			"updated the description and icon of " + cliArtifact},
		{"every field", []string{"-icon", "chart", "-title", "T", "-description", "D"},
			map[string]any{"title": "T", "description": "D", "icon": "chart"},
			"updated the title, description and icon of " + cliArtifact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeArtifactsNode(t)
			args := append([]string{"artifacts", "update", cliArtifact}, tc.args...)
			stdout, stderr, err := cli(t, append(args, bootstrapForURL(t, node.server.URL))...)
			if err != nil {
				t.Fatalf("update: %v\n%s", err, stderr)
			}
			if len(node.patches) != 1 || !maps.Equal(node.patches[0], tc.want) {
				t.Errorf("patched %v, want %v", node.patches, tc.want)
			}
			if len(node.keys) != 1 || node.keys[0] == "" {
				t.Errorf("the update was keyed %v", node.keys)
			}
			if !strings.Contains(stdout, tc.says) {
				t.Errorf("printed %q, want %q", stdout, tc.says)
			}
		})
	}
	node := newFakeArtifactsNode(t)
	_, _, err := cli(t, "artifacts", "update", cliArtifact, bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "name what to change") || len(node.patches) != 0 {
		t.Errorf("an update naming nothing = %v, %d sent", err, len(node.patches))
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

// THE PUBLISH LINE COUNTS WHAT IT SENT AND WHAT THE VERSION HOLDS NEW
// SEPARATELY: a page the intent asked for is sent, and the node may still find
// its linted form already held, so "sent" is the transfers and "new" and
// "already held" are the node's count of the manifest that landed.
func TestArtifactsPublishCountsWhatWasSentAndWhatWasHeld(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	node.carried = []string{"style.css", "img/logo.svg"}
	node.files = map[string]int{"stored": 0, "carried": 3}
	stdout, stderr, err := cli(t, "artifacts", "publish", site(t), "-artifact", cliArtifact,
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "version 4 (files: 1 sent, 0 new, 3 already held)") {
		t.Errorf("printed %q", stdout)
	}

	// A node that answers no count (a rollback's answer has none) is
	// reported by what was sent alone, never as nothing held.
	bare := newFakeArtifactsNode(t)
	stdout, stderr, err = cli(t, "artifacts", "publish", site(t), bootstrapForURL(t, bare.server.URL))
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "version 1 (files: 3 sent)") {
		t.Errorf("printed %q", stdout)
	}
}

// GET AND VERSIONS SHOW EACH VERSION'S RUNTIME CONTRACT AND CAPABILITIES, an
// empty set as `none` rather than a missing line, so a version whose
// capabilities were stripped is visible as such.
func TestArtifactsGetAndVersionsShowTheContractAndCapabilities(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	node.head = map[string]any{"contract": "1.0.0", "capabilities": []string{"db", "state"}}
	stdout, stderr, err := cli(t, "artifacts", "get", cliArtifact, cfg)
	if err != nil {
		t.Fatalf("get: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "  contract 1.0.0\n") || !strings.Contains(stdout, "  can      db,state\n") {
		t.Errorf("get printed %q", stdout)
	}
	node.head = map[string]any{"kind": "doc", "capabilities": []string{}}
	if stdout, _, _ = cli(t, "artifacts", "get", cliArtifact, cfg); !strings.Contains(stdout, "  contract none\n") ||
		!strings.Contains(stdout, "  can      none\n") {
		t.Errorf("get of a version declaring nothing printed %q", stdout)
	}

	node.versions = []map[string]any{
		{"number": 2, "contract": "1.0.0", "capabilities": []string{}, "files": 1, "bytes": 9},
		{"number": 1, "contract": "1.0.0", "capabilities": []string{"db"}, "files": 1, "bytes": 9},
	}
	stdout, stderr, err = cli(t, "artifacts", "versions", cliArtifact, cfg)
	if err != nil {
		t.Fatalf("versions: %v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "contract 1.0.0  can none") ||
		!strings.Contains(lines[1], "contract 1.0.0  can db") {
		t.Errorf("versions printed %q", stdout)
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

// A PUBLISH FINISHES AN OPEN OUTCOME UNDER THE OPERATION IT SENT, by the rule
// every keyed write follows ([keyedWrite]): the upload intent goes under the
// invocation's key and the publish under a step of it, so the retry an open
// answer offers is -op-id with the INTENT's key — answered the same upload,
// and landing the same version — never the step's, which the command derives
// again, and never a fresh one. A 2xx whose outcome nobody can vouch for
// fails rather than printing "published", and a 503 unknown says to retry.
//
// Mutation: drop the publish's confirmed check and the 2xx row exits zero;
// settle the publish's error under its step key and both rows name the wrong
// operation.
func TestArtifactsPublishFinishesAnOpenOutcomeUnderItsOperation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome string
		refuse  map[string]any
		says    string
	}{
		{name: "a 2xx unknown", outcome: "unknown", says: "could not confirm the write"},
		{name: "a 503 unknown", refuse: map[string]any{"error": "unavailable",
			"outcome": "unknown", "op_id": "the-scoped-key.k0123456789abcdef"},
			says: "No acknowledgement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeArtifactsNode(t)
			node.outcome, node.refuse = tc.outcome, tc.refuse
			stdout, stderr, err := cli(t, "artifacts", "publish", site(t),
				bootstrapForURL(t, node.server.URL))
			if err == nil {
				t.Fatalf("an open outcome exited zero: %s", stdout)
			}
			said := err.Error() + "\n" + stderr
			if len(node.keys) == 0 {
				t.Fatal("the publish sent no operation key")
			}
			offered := regexp.MustCompile(`-op-id ([^\s,]+)`).FindAllStringSubmatch(said, -1)
			if !strings.Contains(said, tc.says) || len(offered) == 0 {
				t.Errorf("answered %q, want %q and a retry under the intent's key %s", said,
					tc.says, node.keys[0])
			}
			for _, m := range offered {
				if m[1] != node.keys[0] {
					t.Errorf("offered a retry under %s, want the intent's key %s:\n%s", m[1],
						node.keys[0], said)
				}
			}
			if strings.Contains(stdout, "published") {
				t.Errorf("printed %q for a publish nobody could vouch for", stdout)
			}
		})
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

// A TABBED DOC OPENS ON ITS INDEX: a directory holding `doc.json` and its tabs
// creates a doc opening on the index, never on one of the tabs.
func TestArtifactsPublishInfersATabbedDocFromItsIndex(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	dir := writeTree(t, map[string]string{
		"doc.json":      `{"v":1,"tabs":[{"path":"tabs/plan.md","title":"Plan"}]}`,
		"tabs/plan.md":  "# Plan",
		"tabs/notes.md": "# Notes",
	})
	if _, stderr, err := cli(t, "artifacts", "publish", dir, bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if create := node.creates[0]; create["kind"] != string(artifacts.KindDoc) || create["entry"] != "doc.json" {
		t.Errorf("created %v", create)
	}
}

// TWO DEFAULT ENTRIES ARE THE AUTHOR'S TO CHOOSE BETWEEN: a directory holding
// both is refused before a byte moves, and -kind settles it.
func TestArtifactsPublishRefusesTwoCandidateEntries(t *testing.T) {
	t.Parallel()
	node := newFakeArtifactsNode(t)
	dir := writeTree(t, map[string]string{"index.html": "<!doctype html>", "index.md": "# Plan"})
	_, _, err := cli(t, "artifacts", "publish", dir, bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), `"index.html" and "index.md"`) {
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

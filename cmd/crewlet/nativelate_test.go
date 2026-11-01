package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/iam"
)

// A COMPANY APPLIED AFTER BOOT IS SERVED WHOLE: every native surface the API
// carries answers for it without a restart, because each is asked of the
// engine per request rather than captured when the API was assembled.
//
// The node boots with no company — the quickstart's live bootstrap, and the
// dashboard's company creation — so its native runtime is built by the first
// apply, after the API already exists. Every surface resolved at assembly
// read "no tracker" off a runtime that did not exist yet, and kept reading it
// for the life of the process: the board's questions came back
// `unknown_query` until somebody restarted the node.
func TestNativeSurfacesServeACompanyAppliedAfterBoot(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token-long-enough-for-tier-a")
	n.reconcile()
	if err := n.settle(n.put(companyYAML + `  - name: Founder
    handle: founder
    kind: human
`)); err != nil {
		t.Fatalf("the company: %v", err)
	}
	n.bindToken("ops", "founder")

	// THE READS: the board's and the knowledge base's questions, and the
	// state log's own retention answer, which a node running no state log
	// at boot did not register either.
	for _, question := range []string{"work_items", "pages", "retention"} {
		status, body := n.get(t, "/query/"+question)
		if status != http.StatusOK {
			t.Errorf("GET /query/%s = %d %s, want it answered for the company applied "+
				"after boot", question, status, body)
		}
	}

	// THE ROUTES: a file's bytes, the purge and the eviction gate, each
	// reaching its own handler — which answers with a refusal of its own
	// that writes nothing — where a route resolved at assembly answered the
	// mux's `no_route` (the file bytes and the purge, never mounted) or
	// `no_state_log` (the gate, handed no state log) for the life of the
	// process.
	for _, route := range []struct {
		method, path string
		status       int
		code         string
	}{
		// The tracker answered: there is no such project.
		{http.MethodGet, "/worklet/files/NOSUCH/no-such.txt", http.StatusNotFound, "no_project"},
		// And the upload, the write half on its own, through the same
		// tracker read — before a byte of the body is stored.
		{http.MethodPut, "/worklet/files/NOSUCH/no-such.txt", http.StatusNotFound, "no_project"},
		// The purge's own refusal — a confirmation with no reason — before
		// it reads a row.
		{http.MethodPost, "/worklet/items/no-such/purge?confirm=X", http.StatusBadRequest,
			"invalid_query"},
		// The gate's own refusal, before anything is judged.
		{http.MethodPost, "/worklet/retention/evict/no-such-node", http.StatusBadRequest,
			"confirm_required"},
	} {
		status, body := n.send(t, route.method, route.path)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &answer)
		if status != route.status || answer.Error != route.code {
			t.Errorf("%s %s = %d %s, want %d %s from its own handler for the company "+
				"applied after boot", route.method, route.path, status, body,
				route.status, route.code)
		}
	}

	// THE OPERATOR'S TOOLS: the one catalogue the dashboard's act transport
	// and an assistant's MCP both serve, built from the halves the runtime
	// runs. Built at assembly, it was built from a runtime that did not
	// exist yet — the fleet's own gestures and nothing of the tracker or the
	// knowledge base — and served that for the life of the process, so the
	// person the company binds could not file a task from the dashboard.
	status, body := n.get(t, "/query/viewer")
	var viewer struct {
		Handle string   `json:"handle"`
		Acts   []string `json:"acts"`
	}
	if err := json.Unmarshal([]byte(body), &viewer); err != nil || status != http.StatusOK {
		t.Fatalf("GET /query/viewer = %d %s (%v)", status, body, err)
	}
	if viewer.Handle == "" || !slices.Contains(viewer.Acts, builtin.CreateWorkItemTool) {
		t.Errorf("the person the company applied after boot binds (%q) may do %v, "+
			"want %s among it", viewer.Handle, viewer.Acts, builtin.CreateWorkItemTool)
	}
	// And the act transport serves it: a press reaches the tool and is
	// refused for its body, which writes nothing — where the catalogue built
	// at assembly answered that it serves no such tool.
	status, body = n.send(t, http.MethodPost, "/operator/act/"+builtin.CreateWorkItemTool)
	var act struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &act)
	if status != http.StatusUnsupportedMediaType ||
		act.Error != string(httpjson.CodeUnsupportedMediaType) {
		t.Errorf("POST /operator/act/%s = %d %s, want %d %s from the tool's own "+
			"transport for the company applied after boot", builtin.CreateWorkItemTool,
			status, body, http.StatusUnsupportedMediaType, httpjson.CodeUnsupportedMediaType)
	}
}

// A COMPANY APPLIED AFTER BOOT IS SERVED GIT AND ITS CODE READS. The code host
// is built with the native runtime, which a node booted with no company builds
// at its first apply — after the API was assembled — so the git routes and
// the /code/ reads are asked of the engine per request. Before the company,
// both say it hosts no code; after the apply that sets code.native, a person
// clones and pushes with their own token through the API `crewlet run` serves,
// and the push is read back as theirs.
//
// The hook is THIS test binary through the engine's DEFAULT GitHook — the
// production argv, os.Executable as `code hook proc-receive` — which TestMain
// runs as main() would.
//
// Mutation: capture the code host at api.New, and the clone after the apply is
// refused as a company that hosts no code.
func TestCodeRoutesServeACompanyAppliedAfterBoot(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const token = "a-test-token-long-enough-for-tier-a"
	n := bootUnconfiguredWith(t, token, func(b *config.Bootstrap) {
		// THE CASE'S OWN, never the machine's: an explicit cache budget and
		// a pinned transfer lane, so the room this node's disk has and the
		// cores it runs on decide nothing about whether it hosts code.
		b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
		b.Git.MaxTransfers = 4
	})
	n.reconcile()
	repo := n.base + "/git/web.git"
	const notHosted = "this company does not host code (code.native is not set)"

	// BEFORE THE COMPANY: neither half has a code host to serve.
	status, body := n.get(t, "/code/repos")
	var refusal struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(body), &refusal)
	// THE ENVELOPE'S DETAIL IS A SENTENCE ([httpjson.Sentence]), and git's
	// one line the bare clause it prints as `remote:` text.
	if status != http.StatusNotFound || refusal.Error != "not_found" ||
		refusal.Detail != httpjson.Sentence(notHosted) {
		t.Fatalf("GET /code/repos before any company = %d %s, want 404 not_found naming code.native",
			status, body)
	}
	status, kind, text := gitGet(t, n, repo+"/info/refs?service=git-upload-pack", token)
	if status != http.StatusNotFound || !strings.HasPrefix(kind, "text/plain") || text != notHosted+"\n" {
		t.Fatalf("a clone's advertisement before any company = %d %q %q, want 404 text/plain %q",
			status, kind, text, notHosted)
	}

	if err := n.settle(n.put(companyYAML + `  - name: Founder
    handle: founder
    kind: human
code:
  native: true
  repos:
    - name: web
      owner: {seat: founder}
`)); err != nil {
		t.Fatalf("the company: %v", err)
	}
	n.bindToken("ops", "founder")

	// THE READS: the declared repository, read at the log's head — the
	// reconcile that creates it follows the apply.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status, body = n.get(t, "/code/repos?read_level=linearizable")
		var listing struct {
			Repos []struct {
				Name string `json:"name"`
			} `json:"repos"`
		}
		if status == http.StatusOK && json.Unmarshal([]byte(body), &listing) == nil &&
			len(listing.Repos) == 1 && listing.Repos[0].Name == "web" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the declared repository was never listed: GET /code/repos = %d %s", status, body)
		}
	}

	// THE GIT ROUTES: the founder clones the empty repository and pushes
	// main through the API, with their own token as git's Basic password.
	founder := codetest.PersonWithToken(t, token)
	dir := founder.Clone(repo)
	commit := founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := founder.Push(t.Context(), dir, repo, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push through the API did not land:\n%s", pushed)
	}

	// AND THE PUSH IS READ BACK AS THEIRS.
	status, body = n.get(t, "/code/repos/web/pushes?read_level=linearizable")
	var history struct {
		Pushes []struct {
			Actor     string `json:"actor"`
			ActorSeat string `json:"actor_seat"`
			Commands  []struct {
				Ref string `json:"ref"`
				New string `json:"new"`
			} `json:"commands"`
		} `json:"pushes"`
	}
	if err := json.Unmarshal([]byte(body), &history); err != nil || status != http.StatusOK {
		t.Fatalf("GET /code/repos/web/pushes = %d %s (%v)", status, body, err)
	}
	// THE CREDENTIAL IS THE ACTOR, under its whole login, and the seat the
	// directory binds it to is the person the push was theirs as.
	if len(history.Pushes) != 1 || history.Pushes[0].ActorSeat != "founder" ||
		history.Pushes[0].Actor != iam.TokenLogin("ops") || len(history.Pushes[0].Commands) != 1 ||
		history.Pushes[0].Commands[0].New != commit {
		t.Fatalf("the repository's history is %s, want the founder's one push of %s as %s",
			body, commit, iam.TokenLogin("ops"))
	}
}

// A NODE WHOSE CODE HOST COULD NOT START SAYS SO, and never that the company
// hosts no code. An apply builds the code host before it publishes its epoch,
// so a node whose apply of the configuration that turns code.native on fails —
// here a cache directory somebody else's files already fill, which the cache
// refuses to take — still serves the previous epoch, one that hosts no code;
// read off that epoch, the person who had just turned code.native on was told
// "this company does not host code". The answer is read off what the fleet
// activated instead: 503 config_apply_failed, with no Retry-After — waiting
// clears nothing, another node serves it — as the envelope's code on a /code/
// read and in brackets on git's one line on a git route.
//
// Mutation: answer from the published epoch alone (engine.CodeUnserved), and
// the read says the company hosts no code.
func TestANodeWhoseCodeHostCannotStartSaysSo(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const token = "a-test-token-long-enough-for-tier-a"
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "somebody-elses.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := bootUnconfiguredWith(t, token, func(b *config.Bootstrap) {
		b.Git.CacheDir = occupied
		b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
		b.Git.MaxTransfers = 4
	})
	n.reconcile()
	n.put(companyYAML + `  - name: Founder
    handle: founder
    kind: human
code:
  native: true
`)

	var refusal struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	var (
		status int
		body   string
		after  string
		// read is whether this node has answered anything but "not
		// hosted" since the activation — evidence its reconcile tick has
		// read it.
		read bool
	)
	// THE WINDOW THE CONTRACT ALLOWS (engine.CodeUnserved): until this
	// node's reconcile tick reads the activation — milliseconds on the nudge
	// it broadcasts, a reconcile interval at most — the node answers by the
	// activation it last read, which hosted no code. So a 404 is the node's
	// honest answer until then, and the walk-back this case exists to catch
	// only after it: once the node has answered anything else, or once a
	// tick must have read it — one interval for the tick to come round and
	// one more for a tick already in flight to finish.
	activated := time.Now()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.base+"/code/repos", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := n.client.Do(req)
		if err != nil {
			t.Fatalf("GET /code/repos: %v", err)
		}
		raw, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		status, body, after = res.StatusCode, string(raw), res.Header.Get("Retry-After")
		refusal.Error = ""
		_ = json.Unmarshal(raw, &refusal)
		if refusal.Error == string(httpjson.CodeConfigApplyFailed) {
			break
		}
		unread := status == http.StatusNotFound && !read &&
			time.Since(activated) < 2*configplane.ReconcileInterval
		read = read || status != http.StatusNotFound
		if status == http.StatusNotFound && !unread || time.Now().After(deadline) {
			t.Fatalf("GET /code/repos on a node whose code host could not start = %d %s, want 503 "+
				"naming config_apply_failed", status, body)
		}
	}
	if status != http.StatusServiceUnavailable || after != "" ||
		strings.Contains(refusal.Detail, "does not host code") {
		t.Errorf("GET /code/repos = %d %s (Retry-After %q), want 503 config_apply_failed naming the failed apply "+
			"and no Retry-After", status, body, after)
	}
	status, kind, text := gitGet(t, n, n.base+"/git/web.git/info/refs?service=git-upload-pack", token)
	if status != http.StatusServiceUnavailable || !strings.HasPrefix(kind, "text/plain") ||
		!strings.Contains(text, "config_apply_failed") {
		t.Errorf("a clone's advertisement on a node whose code host could not start = %d %q %q, want 503 "+
			"text/plain naming config_apply_failed", status, kind, text)
	}
}

// gitGet sends one GET to url as git's first request after its challenge —
// the token as the Basic password — and answers the status, the content type
// and the body, which on a git route is the text git prints.
func gitGet(t *testing.T, n *unconfiguredNode, url, token string) (status int, contentType, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("crewlet", token)
	res, err := n.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), string(raw)
}

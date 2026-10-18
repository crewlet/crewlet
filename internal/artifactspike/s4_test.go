package artifactspike

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// SPIKE S4: the publish-time linter and the strict script policy over a
// corpus of model-shaped pages.
//
// testdata/s4 holds pages written the way a model writes an artifact when it
// is asked for a dashboard, a game, a form or a tool: CDN libraries pinned and
// unpinned, inline handlers, browser storage, remote APIs, embeds. Each is put
// through the shipped linter ([lint.HTML], with the CDN copy-in answered
// locally, as the publish pipeline answers it from its cache), and each page
// the linter publishes is read against the policy the artifact origin serves
// it under (internal/api/pagepolicy: a fresh nonce with 'strict-dynamic',
// handler digests, 'none' for frames, forms and the base URL, the artifact's
// own origin for every connection and image, workers from blob: only) by a
// STATIC reading ([policyBreaks]) of what in the stored page that policy
// stops. The static reading is a model, and it says which rules it applies;
// the browser half of the spike (S4 in docs/reference/artifact-capacity.md)
// opens the same pages on a serving node and counts the console's policy
// violations.
//
// testdata/s4/corpus.json records, per page, what the linter refuses and
// what the policy stops; [TestTheS4CorpusReadsAsRecorded] holds both to it,
// so a linter or policy change that moves a page fails until the record (and
// the capacity page's figures) move with it.
//
// Run: go test -run '^$' -bench S4 -benchtime 1x ./internal/artifactspike

// corpusDir is where the pages live.
const corpusDir = "testdata/s4"

// The causes the static reading names: what the strict policy stops in a page
// the linter published.
const (
	// breakEval is string evaluation (eval, new Function, a string timer, a
	// template compiler) in a page whose policy does not admit it: only an
	// in-browser transform earns 'unsafe-eval'.
	breakEval = "eval"

	// breakLibraryEval is a library build known to compile with new
	// Function at run time (the Vue full build, Alpine's standard build).
	breakLibraryEval = "library_eval"

	// breakRuntimeHandler is an inline event handler a script writes at
	// run time whose text no published handler shares, so no digest in the
	// policy admits it.
	breakRuntimeHandler = "runtime_handler"

	// breakRemoteRuntime is a script reaching another origin at run time
	// (fetch, a socket, map tiles, audio): connect-src, img-src and
	// media-src name the artifact's own origin alone.
	breakRemoteRuntime = "remote_runtime"

	// breakRemoteMedia is markup or a stylesheet loading an image, a frame
	// or media from another origin (the linter warns remote_blocked).
	breakRemoteMedia = "remote_media"

	// breakFrame is an iframe: frame-src is 'none'.
	breakFrame = "frame"

	// breakFormNavigation is a form whose submission navigates, with no
	// script preventing it: form-action is 'none'.
	breakFormNavigation = "form_navigation"

	// breakPopup is window.open: the frame's sandbox allows no popups, and
	// links reach the reader through the dashboard's interstitial instead.
	breakPopup = "popup"

	// breakWorkerURL is a worker started from a URL: worker-src is blob:.
	breakWorkerURL = "worker_url"

	// breakBase is a base element: base-uri is 'none'.
	breakBase = "base"
)

// escapeImportMap is an import map naming a module on another origin: a
// nonced module's imports are fetched under its nonce, so the module loads in
// the viewer's browser from wherever the map points. Not a break (the page
// works) but an ESCAPE from the publish-time copy-in: the linter refuses the
// same reference written as a literal import (lint.CodeRemoteImport) and
// publishes it written as a map entry.
const escapeImportMap = "import_map_remote"

// storageShared is browser storage, which works per viewer on a per-artifact
// origin and throws on the shared mode's opaque origin: recorded apart from
// the breaks, since whether it breaks is the deployment's origin mode.
const storageShared = "storage_shared_mode"

// corpusEntry is one page's record.
type corpusEntry struct {
	File    string   `json:"file"`
	What    string   `json:"what"`
	Refused []string `json:"refused"`
	Breaks  []string `json:"breaks"`
	Escapes []string `json:"escapes"`
	Storage bool     `json:"storage"`
}

// corpusReading is what the spike read in one page.
type corpusReading struct {
	refused []string
	breaks  []string
	escapes []string
	storage bool
	policy  artifacts.ScriptPolicy
}

func loadCorpus(tb testing.TB) []corpusEntry {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, "corpus.json"))
	if err != nil {
		tb.Fatalf("read the corpus record: %v", err)
	}
	var out []corpusEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		tb.Fatalf("decode the corpus record: %v", err)
	}
	return out
}

// corpusOptions are the linter's options as the publish pipeline sets them,
// with the CDN copy-in answered locally: the copy is fetched and checked by
// internal/artifactupload/vendor, and what the policy reads is the local path
// the linter rewrites a reference to, which this answers the same way.
func corpusOptions() lint.Options {
	return lint.Options{Hosts: lint.DefaultAllowlist(), Resolve: func(ref lint.Ref) (string, error) {
		local := "_vendor/" + ref.URL.Host + ref.URL.Path
		if dir := path.Dir(ref.From); dir != "." {
			local = strings.Repeat("../", strings.Count(dir, "/")+1) + local
		}
		return local, nil
	}}
}

// readPage lints one page and, published, reads it against the policy.
func readPage(tb testing.TB, name string, src []byte) corpusReading {
	tb.Helper()
	page, err := lint.HTML("index.html", src, corpusOptions())
	var refusal *lint.Refusal
	switch {
	case errors.As(err, &refusal):
		var codes []string
		for _, f := range refusal.Findings {
			codes = append(codes, string(f.Code))
		}
		slices.Sort(codes)
		return corpusReading{refused: slices.Compact(codes)}
	case err != nil:
		tb.Fatalf("%s: the linter failed rather than answer: %v", name, err)
	}
	breaks, storage := policyBreaks(page, src)
	return corpusReading{breaks: breaks, escapes: policyEscapes(page), storage: storage,
		policy: page.Policy}
}

// TestTheS4CorpusReadsAsRecorded holds every page of the corpus to its
// record: what the linter refuses, and what the strict policy stops in what
// it publishes.
func TestTheS4CorpusReadsAsRecorded(t *testing.T) {
	t.Parallel()
	entries := loadCorpus(t)
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(files) {
		t.Fatalf("the record names %d pages and the corpus holds %d: every page is "+
			"recorded, and every record names a page", len(entries), len(files))
	}
	if len(entries) < minCorpusPages {
		t.Fatalf("the corpus holds %d pages, under the %d the capacity page's figures "+
			"are drawn from", len(entries), minCorpusPages)
	}
	for _, e := range entries {
		src, err := os.ReadFile(filepath.Join(corpusDir, e.File))
		if err != nil {
			t.Fatalf("%s: %v", e.File, err)
		}
		got := readPage(t, e.File, src)
		if !slices.Equal(nonNil(got.refused), nonNil(e.Refused)) ||
			!slices.Equal(nonNil(got.breaks), nonNil(e.Breaks)) ||
			!slices.Equal(nonNil(got.escapes), nonNil(e.Escapes)) || got.storage != e.Storage {
			t.Errorf("%s (%s): refused %v, breaks %v, escapes %v, storage %v; recorded "+
				"refused %v, breaks %v, escapes %v, storage %v", e.File, e.What, got.refused,
				got.breaks, got.escapes, got.storage, e.Refused, e.Breaks, e.Escapes, e.Storage)
		}
	}
}

// minCorpusPages is the corpus's floor: the plan names 200 model-made pages,
// and a hand-written corpus of this size covers every shape the linter and the
// policy decide on (the capacity page says so).
const minCorpusPages = 40

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// BenchmarkS4Corpus reports the corpus's figures: the share refused, the share
// published and stopped by the policy, each cause's count, and the linter's
// time per page.
func BenchmarkS4Corpus(b *testing.B) {
	entries := loadCorpus(b)
	pages := make([][]byte, len(entries))
	for i, e := range entries {
		src, err := os.ReadFile(filepath.Join(corpusDir, e.File))
		if err != nil {
			b.Fatalf("%s: %v", e.File, err)
		}
		pages[i] = src
	}
	var (
		refused, broken, storage, escaped int
		causes                            = map[string]int{}
		refusals                          = map[string]int{}
		linting                           time.Duration
	)
	b.ResetTimer()
	for range b.N {
		refused, broken, storage, escaped = 0, 0, 0, 0
		clear(causes)
		clear(refusals)
		linting = 0
		for i, e := range entries {
			t0 := time.Now()
			r := readPage(b, e.File, pages[i])
			linting += time.Since(t0)
			switch {
			case len(r.refused) > 0:
				refused++
				for _, c := range r.refused {
					refusals[c]++
				}
			case len(r.breaks) > 0:
				broken++
			}
			for _, c := range r.breaks {
				causes[c]++
			}
			if r.storage {
				storage++
			}
			if len(r.escapes) > 0 {
				escaped++
			}
		}
	}
	b.StopTimer()
	n := float64(len(entries))
	published := n - float64(refused)
	b.ReportMetric(n, "pages")
	b.ReportMetric(float64(refused)/n, "refused-share")
	b.ReportMetric(published/n, "published-share")
	b.ReportMetric(float64(broken)/n, "published-and-broken-share")
	b.ReportMetric((published-float64(broken))/n, "published-and-clean-share")
	b.ReportMetric(float64(storage)/n, "storage-share")
	b.ReportMetric(float64(escaped), "escaped-pages")
	b.ReportMetric(float64(linting.Microseconds())/n, "lint-us/page")
	for _, c := range slices.Sorted(maps.Keys(causes)) {
		b.ReportMetric(float64(causes[c]), "break-"+c)
	}
	for _, c := range slices.Sorted(maps.Keys(refusals)) {
		b.ReportMetric(float64(refusals[c]), "refused-"+c)
	}
}

// THE STATIC READING. Each rule below is one directive of the served policy
// and the source shapes it stops; nothing is inferred beyond the shapes named.
var (
	// stringEval is string evaluation in a page's own script.
	stringEval = regexp.MustCompile(`\beval\s*\(|\bnew\s+Function\s*\(|\bset(?:Timeout|Interval)\s*\(\s*["'` + "`" + `]|\b_\.template\s*\(|\bHandlebars\.compile\s*\(`)

	// evalLibraries are library builds that compile templates with new
	// Function at run time, matched on the referenced URL's path.
	evalLibraries = []*regexp.Regexp{
		regexp.MustCompile(`/vue@[0-9.]+/dist/vue\.global(\.prod)?\.js$`),
		regexp.MustCompile(`/alpinejs@[0-9.]+/dist/cdn(\.min)?\.js$`),
	}

	// writtenHandler is an inline handler attribute inside a script's
	// string: `onclick="..."` in markup a script builds.
	writtenHandler = regexp.MustCompile(`\bon[a-z]+\s*=\s*\\?"([^"\\]*)\\?"`)

	// setHandler is a handler set by attribute name.
	setHandler = regexp.MustCompile(`setAttribute\(\s*["']on[a-z]+["']\s*,`)

	// remoteLiteral is a script literal naming another origin.
	remoteLiteral = regexp.MustCompile(`["'` + "`" + `](?:https?|wss?)://([^/"'` + "`" + `]+)`)

	// networkAPI is a script API that reaches the URL it is given.
	networkAPI = regexp.MustCompile(`\bfetch\s*\(|XMLHttpRequest|\bnew\s+WebSocket\s*\(|\bnew\s+EventSource\s*\(|\bgetJSON\s*\(|\$\.ajax\s*\(|\bnew\s+Audio\s*\(|\bnew\s+Image\s*\(|\btileLayer\s*\(|\bd3\.(?:csv|json|tsv|text)\s*\(|\baxios\b`)

	// popup is a script opening a window.
	popup = regexp.MustCompile(`\bwindow\.open\s*\(`)

	// workerURL is a worker started from a literal URL.
	workerURL = regexp.MustCompile(`\bnew\s+(?:Shared)?Worker\s*\(\s*["'` + "`" + `]`)

	// browserStorage is browser storage.
	browserStorage = regexp.MustCompile(`\blocalStorage\b|\bsessionStorage\b|\bindexedDB\b`)
)

// policyBreaks reads a published page (its stored text and the policy the
// linter derived) against the served policy, and answers the causes it finds,
// sorted, and whether the page uses browser storage.
func policyBreaks(page lint.Page, src []byte) ([]string, bool) {
	found := map[string]bool{}
	var scripts []string
	forms, preventing := 0, false
	z := html.NewTokenizer(bytes.NewReader(page.Text))
	inScript := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			switch tok.Data {
			case "script":
				inScript = tt == html.StartTagToken && attr(tok, "src") == ""
			case "iframe":
				found[breakFrame] = true
			case "base":
				found[breakBase] = true
			case "form":
				forms++
				if strings.Contains(attr(tok, "onsubmit"), "return false") {
					preventing = true
				}
			}
		case html.TextToken:
			if inScript {
				scripts = append(scripts, string(z.Text()))
			}
		case html.EndTagToken:
			if tok := z.Token(); tok.Data == "script" {
				inScript = false
			}
		}
	}
	js := strings.Join(scripts, "\n")
	if !page.Policy.UnsafeEval && stringEval.MatchString(js) {
		found[breakEval] = true
	}
	for _, ref := range sourceScriptURLs(src) {
		for _, re := range evalLibraries {
			if re.MatchString(ref) && !page.Policy.UnsafeEval {
				found[breakLibraryEval] = true
			}
		}
	}
	for _, m := range writtenHandler.FindAllStringSubmatch(js, -1) {
		if strings.Contains(m[1], "${") || !slices.Contains(page.Policy.HandlerHashes, handlerDigest(m[1])) {
			found[breakRuntimeHandler] = true
		}
	}
	if setHandler.MatchString(js) {
		found[breakRuntimeHandler] = true
	}
	if networkAPI.MatchString(js) {
		for _, m := range remoteLiteral.FindAllStringSubmatch(js, -1) {
			if !strings.HasSuffix(m[1], "w3.org") {
				found[breakRemoteRuntime] = true
			}
		}
	}
	if slices.ContainsFunc(page.Warnings, func(f lint.Finding) bool { return f.Code == lint.CodeRemoteBlocked }) {
		found[breakRemoteMedia] = true
	}
	if forms > 0 && !preventing && !strings.Contains(js, "preventDefault") {
		found[breakFormNavigation] = true
	}
	if popup.MatchString(js) {
		found[breakPopup] = true
	}
	if workerURL.MatchString(js) {
		found[breakWorkerURL] = true
	}
	out := slices.Sorted(maps.Keys(found))
	if out == nil {
		out = []string{}
	}
	return out, browserStorage.MatchString(js)
}

// policyEscapes reads a published page for module sources the copy-in never
// saw: an import map entry naming another origin.
func policyEscapes(page lint.Page) []string {
	z := html.NewTokenizer(bytes.NewReader(page.Text))
	inMap := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return []string{}
		case html.StartTagToken:
			tok := z.Token()
			inMap = tok.Data == "script" && strings.EqualFold(attr(tok, "type"), "importmap")
		case html.EndTagToken:
			inMap = false
		case html.TextToken:
			if !inMap {
				continue
			}
			var m struct {
				Imports map[string]string            `json:"imports"`
				Scopes  map[string]map[string]string `json:"scopes"`
			}
			if err := json.Unmarshal(z.Text(), &m); err != nil {
				// AN IMPORT MAP THE BROWSER CANNOT PARSE maps nothing.
				continue
			}
			targets := slices.Collect(maps.Values(m.Imports))
			for _, scope := range m.Scopes {
				targets = append(targets, slices.Collect(maps.Values(scope))...)
			}
			if slices.ContainsFunc(targets, lint.IsRemote) {
				return []string{escapeImportMap}
			}
		}
	}
}

// sourceScriptURLs are the script sources the author wrote, before the linter
// pointed them at the version's copies.
func sourceScriptURLs(src []byte) []string {
	var out []string
	z := html.NewTokenizer(bytes.NewReader(src))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			if tok := z.Token(); tok.Data == "script" {
				if s := attr(tok, "src"); s != "" {
					out = append(out, s)
				}
			}
		}
	}
}

func attr(tok html.Token, key string) string {
	for _, a := range tok.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// handlerDigest is an inline handler's digest as the policy names it: the
// standard base64 of its SHA-256, the form the linter records.
func handlerDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return base64.StdEncoding.EncodeToString(sum[:])
}

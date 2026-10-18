package artifactserve_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE GATES: the properties an artifact's origin holds by construction, each a
// test the decision that binds the listener, the API and the dashboard names.

// TestTheArtifactListenerServesOnlyTheServingRoutes walks the route table whole
// and holds it to the reviewed set, and probes every shape of path the API
// serves: none of them answers anything here but an empty 404, and the four
// serving routes do answer (the control, without which refusing everything
// would pass). Mutation: route `/health` or any API prefix to a handler and a
// probe below answers it.
func TestTheArtifactListenerServesOnlyTheServingRoutes(t *testing.T) {
	t.Parallel()
	want := []string{
		"GET /t/{ticket}/{path...}",
		"GET /t/{ticket}/_assets/{asset}",
		"GET /_crewlet/runtime/{contract}/{file...}",
		"GET /_crewlet/kit/{version}/{file...}",
	}
	if !slices.Equal(artifactserve.Routes, want) {
		t.Fatalf("the listener's routes are %v, want exactly %v", artifactserve.Routes, want)
	}
	r := newRig(t)
	f := r.frame()
	for _, path := range []string{
		"/", "/health", "/ready", "/ws/stream", "/auth/session", "/auth/login",
		"/auth/logout", "/artifacts", "/artifacts/" + r.reads.head.ID.String() + "/tickets",
		"/operator/act", "/operator/mcp", "/iam/people", "/config", "/secrets",
		"/static/dashboard/index.html", "/favicon.ico", "/metrics", "/_edge/cache",
		"/t", "/t/", "/_crewlet/", "/_crewlet/runtime/", "/_crewlet/runtime/1.0.0/",
		"/robots.txt", "/.well-known/security.txt",
	} {
		empty(t, r.do(request{url: f.Origin + path}), http.StatusNotFound)
	}
	res := r.do(request{url: base(f) + "index.html", method: http.MethodPost, dest: "iframe"})
	if res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("a POST was answered with Allow %q", res.Header.Get("Allow"))
	}
	empty(t, res, http.StatusMethodNotAllowed)

	for _, u := range []string{
		f.Origin + "/_crewlet/runtime/1.0.0/runtime.js",
		f.Origin + "/_crewlet/kit/0.18.0/kit.css",
		base(f) + "app.js",
		base(f) + "_assets/" + r.reads.assets[0].ID.String(),
	} {
		if res := r.do(request{url: u}); res.StatusCode != http.StatusOK {
			t.Errorf("the serving route %s answered %d", u, res.StatusCode)
		}
	}
	if res := r.document(f); res.StatusCode != http.StatusOK {
		t.Errorf("the ticket's entry answered %d", res.StatusCode)
	}
}

// TestTheArtifactListenerReadsNoCookie: a request carrying the dashboard's
// session cookie is answered exactly as one carrying none, no response ever
// sets a cookie, and no source file of the package so much as names the
// Cookie header or a cookie call. Mutation: read r.Cookie anywhere in the
// package and the source walk fails; set one in a response and the header
// checks do.
func TestTheArtifactListenerReadsNoCookie(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	cookie := map[string]string{"Cookie": "__Host-crewlet_session=v2.abc.def; crewlet_session=x"}
	for _, u := range []string{base(f) + "app.js", base(f) + "logo.png",
		f.Origin + "/_crewlet/runtime/1.0.0/runtime.js", f.Origin + "/health",
		strings.Replace(base(f), "/t/", "/t/x", 1) + "app.js"} {
		plain, with := r.do(request{url: u}), r.do(request{url: u, headers: cookie})
		pb, wb := body(t, plain), body(t, with)
		if plain.StatusCode != with.StatusCode || string(pb) != string(wb) {
			t.Errorf("%s: a cookie changed the answer: %d %q vs %d %q", u, plain.StatusCode,
				pb, with.StatusCode, wb)
		}
		for name := range plain.Header {
			if plain.Header.Get(name) != with.Header.Get(name) {
				t.Errorf("%s: a cookie changed %s: %q vs %q", u, name, plain.Header.Get(name),
					with.Header.Get(name))
			}
		}
		for _, res := range []*reply{plain, with} {
			if res.Header.Get("Set-Cookie") != "" {
				t.Errorf("%s set a cookie: %q", u, res.Header.Get("Set-Cookie"))
			}
		}
	}
	doc := r.do(request{url: strings.SplitN(f.URL, "#", 2)[0], dest: "iframe", headers: cookie})
	if doc.StatusCode != http.StatusOK || doc.Header.Get("Set-Cookie") != "" {
		t.Errorf("a document request with a cookie answered %d, Set-Cookie %q", doc.StatusCode,
			doc.Header.Get("Set-Cookie"))
	}

	// THE SOURCE NAMES NO COOKIE: no call reading or writing one, and no
	// string naming the header.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the sources: %v", err)
	}
	fset := token.NewFileSet()
	forbidden := regexp.MustCompile(`(?i)cookie`)
	walked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		walked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if forbidden.MatchString(x.Sel.Name) {
					t.Errorf("%s: %s names a cookie call", fset.Position(x.Pos()), x.Sel.Name)
				}
			case *ast.BasicLit:
				if forbidden.MatchString(x.Value) {
					t.Errorf("%s: the literal %s names a cookie", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
	}
	if walked == 0 {
		t.Fatal("no source file was walked, so the walk certifies nothing")
	}
}

// TestAnArtifactGetsAnOriginOnlyOnASiteOfItsOwn: from Tier A to the iframe and
// the document's own policy, an artifact keeps its real origin (and so its
// browser storage) only where the template names hosts on a registrable domain
// that is not the dashboard's; on the dashboard's site, or with one shared
// origin, every document is opaque and its files are cross-origin. Mutation:
// grant allow-same-origin by the template alone and the same-site case fails.
func TestAnArtifactGetsAnOriginOnlyOnASiteOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		external string
		own      bool
	}{
		{"a template on a registrable domain of its own",
			"https://{artifact}.example-usercontent.net", true},
		{"a template on the dashboard's own site",
			"https://{artifact}.artifacts.example.com", false},
		{"one shared origin", "https://artifacts.example-usercontent.net", false},
		{"a second port on the dashboard's host", "https://crewlet.example.com:8443", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.API.ExternalURL = dashboard
			b.API.Artifacts.ExternalURL = tc.external
			cfg := artifactserve.OriginsConfigOf(&b)
			r := newRigWith(t, cfg.Mode, cfg.External)
			f := r.frame()
			sameOrigin := strings.Contains(f.Sandbox, "allow-same-origin")
			if sameOrigin != tc.own || (f.Mode == artifacts.OriginPerArtifact) != tc.own {
				t.Fatalf("mode %s, sandbox %q; want a real origin %v", f.Mode, f.Sandbox, tc.own)
			}
			doc := r.document(f)
			if doc.StatusCode != http.StatusOK {
				t.Fatalf("the document answered %d", doc.StatusCode)
			}
			sandbox := directive(doc.Header.Get("Content-Security-Policy"), "sandbox")
			if strings.Contains(sandbox, "allow-same-origin") != tc.own || sandbox != "sandbox "+f.Sandbox {
				t.Errorf("the document's sandbox is %q, the frame's %q", sandbox, f.Sandbox)
			}
			file := r.do(request{url: base(f) + "app.js", dest: "script"})
			corp, acao := "same-origin", ""
			if !tc.own {
				corp, acao = "cross-origin", "*"
			}
			if file.Header.Get("Cross-Origin-Resource-Policy") != corp ||
				file.Header.Get("Access-Control-Allow-Origin") != acao {
				t.Errorf("a file is served CORP %q ACAO %q, want %q %q",
					file.Header.Get("Cross-Origin-Resource-Policy"),
					file.Header.Get("Access-Control-Allow-Origin"), corp, acao)
			}
		})
	}
}

// directive is the named directive of a policy, as written.
func directive(policy, name string) string {
	for _, part := range strings.Split(policy, ";") {
		part = strings.TrimSpace(part)
		if part == name || strings.HasPrefix(part, name+" ") {
			return part
		}
	}
	return ""
}

// nonceOf is the nonce a document policy's script-src names.
func nonceOf(t *testing.T, policy string) string {
	t.Helper()
	m := regexp.MustCompile(`'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(policy)
	if m == nil {
		t.Fatalf("no nonce in %q", policy)
	}
	return m[1]
}

// TestEveryServedDocumentCarriesAFreshNonce: each response's policy names a
// nonce never sent before, the body carries it where the linter's placeholder
// was and the placeholder nowhere, and the policy is the strict one: the
// nonce with strict-dynamic, the published handler by digest alone. Mutation:
// keep the nonce per template rather than per response and the two responses
// share it; skip the substitution and the placeholder reaches the browser.
func TestEveryServedDocumentCarriesAFreshNonce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	seen := map[string]bool{}
	for range 3 {
		res := r.document(f)
		got := string(body(t, res))
		policy := res.Header.Get("Content-Security-Policy")
		nonce := nonceOf(t, policy)
		if seen[nonce] {
			t.Fatalf("the nonce %q was served twice", nonce)
		}
		seen[nonce] = true
		if strings.Contains(got, artifacts.NoncePlaceholder) {
			t.Fatalf("the placeholder reached the browser: %s", got)
		}
		if strings.Count(got, `nonce="`+nonce+`"`) != 3 {
			t.Fatalf("the body does not carry the response's nonce on the runtime and "+
				"both of the page's scripts: %s", got)
		}
		script := directive(policy, "script-src")
		want := "script-src 'nonce-" + nonce + "' 'strict-dynamic' 'unsafe-hashes' 'sha256-" +
			handlerDigest + "'"
		if script != want {
			t.Fatalf("script-src is %q, want %q", script, want)
		}
		if res.Header.Get("Cache-Control") != "private, no-store" || res.Header.Get("ETag") != "" {
			t.Errorf("a document is cached: Cache-Control %q, ETag %q",
				res.Header.Get("Cache-Control"), res.Header.Get("ETag"))
		}
	}
	// A SCRIPTABLE FILE THE FRAME NAVIGATES TO is a document too: a nonce
	// policy that runs none of its script, since nothing in it carries the
	// nonce.
	svg := r.do(request{url: base(f) + "diagram.svg", dest: "iframe"})
	if policy := svg.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "'strict-dynamic'") ||
		strings.Contains(policy, "unsafe-hashes") {
		t.Errorf("an SVG the frame navigated to is served under %q", policy)
	}
}

// TestARevokedShareStopsTheNextDocumentRequest: withdrawing the share stops the
// very next document request with no movement heard at all (a document reads
// no remembered verdict), a subresource is refused the moment the applier's
// movement is heard, and the same holds for a credential that ends and an
// artifact that is trashed. Mutation: let a document read the verdict cache and
// the first refusal below is a 200.
func TestARevokedShareStopsTheNextDocumentRequest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	if res := r.document(f); res.StatusCode != http.StatusOK {
		t.Fatalf("the shared document answered %d", res.StatusCode)
	}
	if res := r.do(request{url: base(f) + "app.js", dest: "script"}); res.StatusCode != http.StatusOK {
		t.Fatalf("its script answered %d", res.StatusCode)
	}
	r.reads.set(func(r *reads) { r.head.Shares, r.head.AccessEpoch = nil, r.head.AccessEpoch+1 })
	empty(t, r.document(f), http.StatusNotFound)
	if r.rec.count(metrics.ArtifactsTicketRefusals, metrics.Attrs{"reason": "revoked"}) == 0 {
		t.Error("the refusal was not counted as a revocation")
	}

	// THE SUBRESOURCE rides the document's verdict until the movement is
	// heard, and not a request after.
	if res := r.do(request{url: base(f) + "app.js", dest: "script"}); res.StatusCode != http.StatusOK {
		t.Fatalf("before the movement was heard a subresource answered %d", res.StatusCode)
	}
	r.handler.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: r.reads.head.ID,
		What: artifactsdomain.MoveShare, AccessEpoch: r.reads.head.AccessEpoch}})
	empty(t, r.do(request{url: base(f) + "app.js", dest: "script"}), http.StatusNotFound)

	// A CREDENTIAL THAT ENDS stops the next document, again with nothing
	// heard.
	ended := newRig(t)
	ef := ended.frame()
	if res := ended.document(ef); res.StatusCode != http.StatusOK {
		t.Fatalf("the document answered %d", res.StatusCode)
	}
	ended.creds.set(ended.lineage, iam.Principal{}, iam.Anonymous)
	empty(t, ended.document(ef), http.StatusNotFound)

	// AND A TRASH.
	trashed := newRig(t)
	tf := trashed.frame()
	trashed.reads.set(func(r *reads) { r.head.Status = artifactsdomain.StatusTrashed })
	empty(t, trashed.document(tf), http.StatusNotFound)

	// AND THE THIRTY SECONDS: a verdict nobody invalidated is not used past
	// its age.
	aged := newRig(t)
	af := aged.frame()
	if res := aged.document(af); res.StatusCode != http.StatusOK {
		t.Fatalf("the document answered %d", res.StatusCode)
	}
	aged.reads.set(func(r *reads) { r.head.Shares = nil })
	aged.clock.advance(artifacts.SubresourceVerdictAge)
	empty(t, aged.do(request{url: base(af) + "app.js", dest: "script"}), http.StatusNotFound)
}

// TestAStaleServingNodeAnswersUnavailableRatherThanServe: every way this node
// cannot vouch for what it would decide from answers 503 with a retry hint and
// an empty body, never a document: a copy past the staleness bound (which the
// read must ask for), no data node answering through the router, an identity
// copy that cannot tell, a copy older than the ticket it is asked about, and a
// chart that cannot say whether a unit share admits. Mutation: drop the bound
// from the read, or serve on an unknown, and the matching case fails.
func TestAStaleServingNodeAnswersUnavailableRatherThanServe(t *testing.T) {
	t.Parallel()
	unavailable := func(t *testing.T, r *rig, f artifacts.Frame) {
		t.Helper()
		res := r.document(f)
		if res.Header.Get("Retry-After") == "" {
			t.Errorf("a 503 carries no Retry-After")
		}
		empty(t, res, http.StatusServiceUnavailable)
	}

	stale := newRig(t)
	f := stale.frame()
	stale.reads.set(func(r *reads) { r.headErr = errStale })
	unavailable(t, stale, f)
	asked := stale.reads.asked[len(stale.reads.asked)-1]
	if asked.Level != statelog.ReadStale || asked.MaxLag != statelog.StallGrace {
		t.Errorf("the head was read at %+v, want a stale read bounded by the stall grace", asked)
	}
	if stale.rec.count(metrics.ArtifactsTicketRefusals, metrics.Attrs{"reason": "stale"}) == 0 {
		t.Error("the 503 was not counted as stale")
	}

	peer := newRig(t)
	pf := peer.frame()
	peer.local.serves = false
	peer.reads.set(func(r *reads) { r.headErr = errUnreachable })
	unavailable(t, peer, pf)
	if peer.rec.count(metrics.ArtifactsAuthzPeerChecks, metrics.Attrs{"outcome": "unavailable"}) != 1 {
		t.Error("a decision no peer answered was not counted")
	}
	peer.reads.set(func(r *reads) { r.headErr = nil })
	if res := peer.document(pf); res.StatusCode != http.StatusOK {
		t.Fatalf("a peer's answer was refused: %d", res.StatusCode)
	}
	if peer.rec.count(metrics.ArtifactsAuthzPeerChecks, metrics.Attrs{"outcome": "served"}) != 1 {
		t.Error("a decision a peer served was not counted")
	}

	identity := newRig(t)
	idf := identity.frame()
	identity.creds.set(identity.lineage, iam.Principal{}, iam.Unknown)
	unavailable(t, identity, idf)

	behind := newRig(t)
	bf := behind.frame()
	behind.reads.set(func(r *reads) { r.head.AccessEpoch-- })
	unavailable(t, behind, bf)

	undecided := newRig(t)
	undecided.reads.set(func(r *reads) {
		r.head.Shares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceUnit,
			ID: "platform"}, Role: artifacts.RoleViewer}}
	})
	uf := undecided.frame()
	undecided.chart.err = statelog.ErrUnavailable
	unavailable(t, undecided, uf)
}

// TestOnlyALintedPageHasItsPlaceholderReplaced: the placeholder is a public
// constant, and the linter reads only a version's HTML files, so an SVG or XML
// file of a version, an asset of any type, and a page whose script facts were
// never written can each carry it unread. Served as a document (the frame
// navigating to it), each is answered byte for byte as stored, the placeholder
// left inert and no runtime placed, so the remote script it names never
// carries the response's nonce. Mutation: substitute the nonce whatever the
// script facts and every case below carries the nonce its policy admits.
func TestOnlyALintedPageHasItsPlaceholderReplaced(t *testing.T) {
	t.Parallel()
	smuggled := `<script nonce="` + artifacts.NoncePlaceholder +
		`" href="https://cdn.example.net/x.js"/>`
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg">` + smuggled + `</svg>`)
	page := []byte(`<!doctype html><script nonce="` + artifacts.NoncePlaceholder +
		`" src="https://cdn.example.net/x.js"></script>`)

	r := newRig(t)
	svgObj, pageObj, assetObj := r.put(svg), r.put(page), r.put(page)
	assetID := uuid.MustParse("0192f00d-0000-7000-8000-0000000000b6")
	r.reads.set(func(rd *reads) {
		v := rd.versions[2]
		files := append([]artifacts.Entry{}, v.Manifest.Files...)
		for i := range files {
			if files[i].Path == "diagram.svg" {
				files[i].Object = svgObj
			}
		}
		files = append(files, artifacts.Entry{Path: "unread.html",
			ContentType: "text/html; charset=utf-8", Object: pageObj})
		slices.SortFunc(files, func(a, b artifacts.Entry) int { return strings.Compare(a.Path, b.Path) })
		v.Manifest.Files = files
		rd.versions[2] = v
		rd.assets = append(append([]artifactsdomain.Asset{}, rd.assets...), artifactsdomain.Asset{
			Artifact: rd.head.ID, ID: assetID, Name: "x.html",
			ContentType: "text/html; charset=utf-8", Object: assetObj})
	})
	f := r.frame()
	for name, url := range map[string]string{
		"an SVG file of the version":          base(f) + "diagram.svg",
		"a page with no script facts":         base(f) + "unread.html",
		"an HTML asset of the artifact store": base(f) + "_assets/" + assetID.String(),
	} {
		res := r.do(request{url: url, dest: "iframe"})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", name, res.StatusCode)
		}
		nonce := nonceOf(t, res.Header.Get("Content-Security-Policy"))
		got := string(body(t, res))
		if strings.Contains(got, nonce) {
			t.Errorf("%s carries the response's nonce: %s", name, got)
		}
		if !strings.Contains(got, artifacts.NoncePlaceholder) || strings.Contains(got, "/_crewlet/runtime/") {
			t.Errorf("%s was not served as stored: %s", name, got)
		}
	}
}

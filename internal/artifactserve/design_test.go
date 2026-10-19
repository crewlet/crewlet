package artifactserve_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DESIGN IS MORE THAN ONE FRAMED DOCUMENT PER VERSION: every artboard is a
// page of its own, framed under a ticket of its own, beside every other board
// on screen. These cases hold that each such document keeps every guarantee
// adr/0035 gives one page (a ticket, a fresh nonce under the strict policy,
// fail-closed revocation, no cookie) and that a ticket opens its own board and
// no other.

// board is an artboard page: a script carrying the linter's placeholder, so a
// served board carries the response's nonce where it ran.
func board(name string) string {
	return `<!doctype html><html><head><title>` + name + `</title>` +
		`<link rel="stylesheet" href="site.css">` +
		`<script nonce="` + artifacts.NoncePlaceholder + `">window.board = "` + name + `";</script>` +
		`</head><body><h1>` + name + `</h1></body></html>`
}

// designRig is the rig's artifact as a design: a canvas, two boards and the
// stylesheet they share, at version 2.
func designRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.files["boards/hero.html"] = []byte(board("Hero"))
	r.files["boards/pricing.html"] = []byte(board("Pricing"))
	r.files["boards/site.css"] = []byte("h1{color:CanvasText}")
	r.files["canvas.json"] = []byte(`{"v": 1, "boards": {` +
		`"boards/hero.html": {"x": 0, "y": 0, "w": 1440, "h": 900}, ` +
		`"boards/pricing.html": {"x": 1540, "y": 0, "w": 1440, "h": 900}}, ` +
		`"order": ["boards/hero.html", "boards/pricing.html"]}`)
	var entries []artifacts.Entry
	for _, path := range []string{"boards/hero.html", "boards/pricing.html", "boards/site.css",
		"canvas.json"} {
		obj := r.put(r.files[path])
		typ, _ := mediatype.For(path)
		e := artifacts.Entry{Path: path, ContentType: typ.Served, Object: obj}
		if strings.HasSuffix(path, ".html") {
			e.Script = &artifacts.ScriptPolicy{Nonced: true}
		}
		entries = append(entries, e)
		r.objects[path] = obj
	}
	r.reads.set(func(rd *reads) {
		rd.head.Kind = artifacts.KindDesign
		v := rd.versions[2]
		v.Entry = "canvas.json"
		v.Manifest = artifacts.Manifest{Entry: "canvas.json", Files: entries}
		rd.versions[2] = v
	})
	return r
}

// root is a frame's URL up to and including its ticket's trailing slash: where
// every path of the version resolves under that ticket.
func root(f artifacts.Frame) string {
	at := strings.Index(f.URL, "/t/") + len("/t/")
	return f.URL[:at+strings.Index(f.URL[at:], "/")+1]
}

// boardFrame mints the reviewer's frame for one board.
func (r *rig) boardFrame(doc string) artifacts.Frame {
	r.t.Helper()
	f, err := r.frames.For(artifactserve.FrameRequest{
		Principal: r.person, Lineage: r.person.Via, Head: r.reads.head,
		Version: r.reads.versions[2], Document: doc,
		Granted: []artifacts.Capability{artifacts.CapabilityAssets},
	})
	if err != nil {
		r.t.Fatalf("Frames.For(%s): %v", doc, err)
	}
	return f
}

// EVERY BOARD IS A DOCUMENT OF ITS OWN: two boards' frames are two tickets, each
// naming its board, each answered with its own board under its own fresh nonce
// and the strict policy, with the runtime placed first. The bare ticket path is
// the ticket's board, never the canvas the dashboard draws.
func TestEveryBoardIsADocumentUnderItsOwnTicket(t *testing.T) {
	t.Parallel()
	r := designRig(t)
	hero, pricing := r.boardFrame("boards/hero.html"), r.boardFrame("boards/pricing.html")
	if hero.Document != "boards/hero.html" || !strings.Contains(hero.URL, "/boards/hero.html#n=") {
		t.Fatalf("the hero frame opens %q at %q", hero.Document, hero.URL)
	}
	if root(hero) == root(pricing) || hero.Nonce == pricing.Nonce {
		t.Fatal("two boards share a ticket")
	}
	nonces := map[string]bool{}
	for name, f := range map[string]artifacts.Frame{"Hero": hero, "Pricing": pricing} {
		res := r.document(f)
		got := string(body(t, res))
		if res.StatusCode != http.StatusOK || !strings.Contains(got, "<h1>"+name+"</h1>") {
			t.Fatalf("%s answered %d: %s", name, res.StatusCode, got)
		}
		policy := res.Header.Get("Content-Security-Policy")
		nonce := nonceOf(t, policy)
		if nonces[nonce] {
			t.Fatal("two boards were served one nonce")
		}
		nonces[nonce] = true
		if directive(policy, "script-src") != "script-src 'nonce-"+nonce+"' 'strict-dynamic'" ||
			directive(policy, "frame-src") != "frame-src 'none'" {
			t.Errorf("%s is served under %q", name, policy)
		}
		if strings.Count(got, `nonce="`+nonce+`"`) != 2 || strings.Contains(got,
			artifacts.NoncePlaceholder) {
			t.Errorf("%s does not carry its nonce on the runtime and its script: %s", name, got)
		}
		if bare := r.do(request{url: root(f), dest: "iframe"}); !strings.Contains(
			string(body(t, bare)), "<h1>"+name+"</h1>") {
			t.Errorf("the bare ticket path of %s is not its board", name)
		}
	}
	// THE STYLESHEET THE BOARDS SHARE loads under either ticket, and the
	// canvas is a subresource like any data file, never a document.
	for _, f := range []artifacts.Frame{hero, pricing} {
		if res := r.do(request{url: root(f) + "boards/site.css", dest: "style"}); res.StatusCode != http.StatusOK {
			t.Errorf("the shared stylesheet answered %d", res.StatusCode)
		}
	}
	canvas := r.do(request{url: root(hero) + "canvas.json", dest: "empty"})
	if canvas.StatusCode != http.StatusOK || canvas.Header.Get("Content-Type") != "application/json" ||
		strings.Contains(canvas.Header.Get("Content-Security-Policy"), "nonce-") {
		t.Errorf("the canvas answered %d as %q under %q", canvas.StatusCode,
			canvas.Header.Get("Content-Type"), canvas.Header.Get("Content-Security-Policy"))
	}
}

// A TICKET OPENS ITS OWN BOARD AND NO OTHER: a board's frame that navigates to
// another board, fetches it, or loads an HTML asset as a document is refused
// with an empty 404 counted under `document`. A page's ticket keeps moving
// between its own pages, as a site does. Mutation: answer `true` from `opens`
// for a design and the first refusal below serves the pricing board.
func TestADesignTicketOpensItsOwnBoardAndNoOther(t *testing.T) {
	t.Parallel()
	r := designRig(t)
	hero := r.boardFrame("boards/hero.html")
	if res := r.document(hero); res.StatusCode != http.StatusOK {
		t.Fatalf("the hero board answered %d", res.StatusCode)
	}
	for _, dest := range []string{"iframe", "empty", ""} {
		empty(t, r.do(request{url: root(hero) + "boards/pricing.html", dest: dest}),
			http.StatusNotFound)
	}
	if got := r.rec.count(metrics.ArtifactsTicketRefusals, metrics.Attrs{"reason": "document"}); got != 3 {
		t.Errorf("%d refusals counted under document, want 3", got)
	}
	r.reads.set(func(rd *reads) {
		rd.assets[0].ContentType = "text/html; charset=utf-8"
	})
	r.handler.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: r.reads.head.ID,
		What: artifactsdomain.MoveAsset}})
	empty(t, r.do(request{url: root(hero) + "_assets/" + r.reads.assets[0].ID.String(),
		dest: "iframe"}), http.StatusNotFound)

	// A PAGE IS A SITE: its ticket loads any page of its version.
	site := newRig(t)
	site.files["about.html"] = []byte(board("About"))
	obj := site.put(site.files["about.html"])
	site.reads.set(func(rd *reads) {
		v := rd.versions[2]
		v.Manifest.Files = append(v.Manifest.Files, artifacts.Entry{Path: "about.html",
			ContentType: "text/html; charset=utf-8", Object: obj,
			Script: &artifacts.ScriptPolicy{Nonced: true}})
		slices.SortFunc(v.Manifest.Files, func(a, b artifacts.Entry) int {
			return strings.Compare(a.Path, b.Path)
		})
		rd.versions[2] = v
	})
	if res := site.do(request{url: root(site.frame()) + "about.html", dest: "iframe"}); res.StatusCode != http.StatusOK {
		t.Errorf("a page's ticket was refused its own second page: %d", res.StatusCode)
	}
}

// A DESIGN'S FRAME NAMES ITS BOARD: no document (its entry is the canvas), the
// canvas itself, a stylesheet, or a path the version does not hold is refused
// at the mint, before any ticket exists.
func TestADesignFrameNamesAPageOfItsVersion(t *testing.T) {
	t.Parallel()
	r := designRig(t)
	for _, doc := range []string{"", "canvas.json", "boards/site.css", "boards/about.html"} {
		_, err := r.frames.For(artifactserve.FrameRequest{Principal: r.person,
			Lineage: r.person.Via, Head: r.reads.head, Version: r.reads.versions[2], Document: doc})
		if !errors.Is(err, artifactserve.ErrNoDocument) {
			t.Errorf("a frame for %q answered %v, want ErrNoDocument", doc, err)
		}
	}
}

// REVOCATION IS EVERY BOARD'S NEXT DOCUMENT REQUEST: with several boards on
// screen, a withdrawn share stops each one's next document request with
// nothing heard, a copy that cannot vouch answers each with a 503, and a
// request carrying the dashboard's cookie is answered as one carrying none
// and sets none. Mutation: decide a board's document from the verdict cache
// and the first board after the withdrawal is a 200.
func TestEveryBoardFailsClosedOnItsNextDocument(t *testing.T) {
	t.Parallel()
	r := designRig(t)
	frames := []artifacts.Frame{r.boardFrame("boards/hero.html"), r.boardFrame("boards/pricing.html")}
	cookie := map[string]string{"Cookie": "__Host-crewlet_session=v2.abc.def"}
	for _, f := range frames {
		with := r.do(request{url: strings.SplitN(f.URL, "#", 2)[0], dest: "iframe", headers: cookie})
		if with.StatusCode != http.StatusOK || with.Header.Get("Set-Cookie") != "" {
			t.Fatalf("a board with a cookie answered %d, Set-Cookie %q", with.StatusCode,
				with.Header.Get("Set-Cookie"))
		}
	}
	r.reads.set(func(rd *reads) { rd.head.Shares, rd.head.AccessEpoch = nil, rd.head.AccessEpoch+1 })
	for _, f := range frames {
		empty(t, r.document(f), http.StatusNotFound)
	}

	stale := designRig(t)
	sf := []artifacts.Frame{stale.boardFrame("boards/hero.html"), stale.boardFrame("boards/pricing.html")}
	stale.reads.set(func(rd *reads) { rd.headErr = errStale })
	for _, f := range sf {
		res := stale.document(f)
		if res.Header.Get("Retry-After") == "" {
			t.Error("a 503 carries no Retry-After")
		}
		empty(t, res, http.StatusServiceUnavailable)
	}

	ended := designRig(t)
	ef := ended.boardFrame("boards/pricing.html")
	ended.creds.set(ended.lineage, iam.Principal{}, iam.Anonymous)
	empty(t, ended.document(ef), http.StatusNotFound)
}

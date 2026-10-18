package artifactsapi_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
)

// ready is the node's beacon as the surface sees it: it keeps every report it
// was handed and answers err, or hands each to a real beacon where a case sets
// one.
type ready struct {
	mu      sync.Mutex
	reports []objectives.ReadyReport
	err     error
	beacon  *objectives.Beacon
}

func (b *ready) Accept(r objectives.ReadyReport) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.reports = append(b.reports, r)
	if b.beacon != nil {
		return b.beacon.Accept(r)
	}
	return nil
}

func (b *ready) taken() []objectives.ReadyReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]objectives.ReadyReport(nil), b.reports...)
}

func (b *ready) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// frameURL is a frame the rig's framer minted for p, expiring at expires.
const frameURL = "https://page.artifacts.example.net/t/ticket/index.html#n=bridge-nonce"

func (g *rig) mintFor(p iam.Principal, expires time.Time) {
	g.framer.mu.Lock()
	defer g.framer.mu.Unlock()
	g.framer.minted[frameURL] = artifacts.TicketClaims{Artifact: page, Version: 2,
		Principal: iam.RecordOwner(p), Lineage: p.Via, Nonce: "bridge-nonce", Expires: expires}
}

func readyBody(url, outcome string, ms int64) string {
	return fmt.Sprintf(`{"frame_url":%q,"outcome":%q,"elapsed_ms":%d}`, url, outcome, ms)
}

// A READY REPORT ABOUT THE CALLER'S OWN FRAME IS COUNTED, bound to the frame's
// credential and nonce, with the earliest instant the frame can have been
// minted. Mutation: drop the principal or the credential check, and the other
// person's report is counted.
func TestAReadyReportIsBoundToTheCallersOwnFrame(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	expires := time.Now().Add(50 * time.Minute).Truncate(time.Second)
	g.mintFor(person("jane"), expires)

	w := g.do(person("jane"), http.MethodPost, "/artifacts/ready", readyBody(frameURL, "ready", 1200))
	if w.Code != http.StatusNoContent {
		t.Fatalf("the report answered %d: %s", w.Code, w.Body.String())
	}
	got := g.ready.taken()
	if len(got) != 1 {
		t.Fatalf("reports %+v, want one", got)
	}
	r := got[0]
	if r.Viewer != person("jane").Via || r.Frame != "bridge-nonce" || r.Outcome != objectives.ReadyOK ||
		r.Elapsed != 1200*time.Millisecond || !r.Expires.Equal(expires) ||
		!r.Minted.Equal(expires.Add(-artifacts.TicketLifetime)) {
		t.Errorf("report %+v", r)
	}

	for name, p := range map[string]iam.Principal{
		"another person":                  person("bob"),
		"the same person through a token": token("jane"),
	} {
		w := g.do(p, http.MethodPost, "/artifacts/ready", readyBody(frameURL, "ready", 1200))
		if w.Code != http.StatusUnprocessableEntity || errorOf(t, w) != "invalid" {
			t.Errorf("%s: %d %s, want the frame refused as no frame at all", name, w.Code, w.Body.String())
		}
	}
	if len(g.ready.taken()) != 1 {
		t.Error("a report about somebody else's frame was counted")
	}
}

// A REPORT THAT IS NO REPORT IS REFUSED BEFORE THE BEACON: no frame, an
// outcome outside the set, a time past a frame's life, a field the route does
// not take.
func TestAMalformedReadyReportIsRefused(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.mintFor(person("jane"), time.Now().Add(time.Hour))
	for name, c := range map[string]struct {
		body   string
		status int
	}{
		"a URL that is no frame":     {readyBody("https://elsewhere.example.net/t/x/index.html", "ready", 10), http.StatusUnprocessableEntity},
		"an outcome outside the set": {readyBody(frameURL, "rendered", 10), http.StatusUnprocessableEntity},
		"a negative time":            {readyBody(frameURL, "ready", -1), http.StatusUnprocessableEntity},
		"a time past a frame's life": {readyBody(frameURL, "ready", artifacts.TicketLifetime.Milliseconds()+1), http.StatusUnprocessableEntity},
		"a field the route does not take": {`{"frame_url":"` + frameURL + `","outcome":"ready","elapsed_ms":1,"artifact":"x"}`,
			http.StatusBadRequest},
	} {
		if w := g.do(person("jane"), http.MethodPost, "/artifacts/ready", c.body); w.Code != c.status {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body.String(), c.status)
		}
	}
	if len(g.ready.taken()) != 0 {
		t.Error("a malformed report reached the beacon")
	}
	unframed := newRig(t, false)
	w := unframed.do(person("jane"), http.MethodPost, "/artifacts/ready", readyBody(frameURL, "ready", 10))
	if w.Code != http.StatusConflict || errorOf(t, w) != "artifact_origin_unset" {
		t.Errorf("a deployment with no artifact origin: %d %s", w.Code, w.Body.String())
	}
}

// THE BEACON'S REFUSALS ARE ANSWERED UNDER THEIR OWN CODES: a frame already
// reported is a conflict, a credential past its rate waits as long as the
// beacon says, and an implausible report is invalid.
func TestTheBeaconsRefusalsAreAnswered(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"a frame already reported": {objectives.ErrReported, http.StatusConflict, "conflict"},
		"a credential past its rate": {&objectives.Throttled{RetryAfter: 2500 * time.Millisecond},
			http.StatusTooManyRequests, "throttled"},
		"a report that could not have been measured": {fmt.Errorf("%w: too long", objectives.ErrImplausible),
			http.StatusUnprocessableEntity, "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newRig(t, true)
			g.mintFor(person("jane"), time.Now().Add(time.Hour))
			g.ready.fail(c.err)
			w := g.do(person("jane"), http.MethodPost, "/artifacts/ready", readyBody(frameURL, "failed", 3000))
			if w.Code != c.status || errorOf(t, w) != c.code {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), c.status, c.code)
			}
			if c.status == http.StatusTooManyRequests && w.Header().Get("Retry-After") != "3" {
				t.Errorf("Retry-After %q, want the beacon's wait rounded up", w.Header().Get("Retry-After"))
			}
		})
	}
}

// THE REAL BEACON BEHIND THE ROUTE COUNTS EACH FRAME ONCE.
func TestEachFrameIsCountedOnceThroughTheRoute(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.mintFor(person("jane"), time.Now().Add(50*time.Minute))
	beacon, err := objectives.NewBeacon(objectives.NewMeter(nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ready.mu.Lock()
	g.ready.beacon = beacon
	g.ready.mu.Unlock()
	first := g.do(person("jane"), http.MethodPost, "/artifacts/ready", readyBody(frameURL, "ready", 900))
	second := g.do(person("jane"), http.MethodPost, "/artifacts/ready", readyBody(frameURL, "ready", 900))
	if first.Code != http.StatusNoContent || second.Code != http.StatusConflict {
		t.Fatalf("first %d, second %d: want 204 then 409", first.Code, second.Code)
	}
}

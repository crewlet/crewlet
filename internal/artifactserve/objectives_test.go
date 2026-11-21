package artifactserve_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// judged is the objective meter as the handler sees it, keeping every event.
type judged struct {
	mu     sync.Mutex
	events []judgedEvent
}

type judgedEvent struct {
	name     objectives.Name
	elapsed  time.Duration
	answered bool
}

func (j *judged) Record(n objectives.Name, elapsed time.Duration, answered bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, judgedEvent{n, elapsed, answered})
}

func (j *judged) Abandon(objectives.Name, time.Duration) {}

func (j *judged) all() []judgedEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]judgedEvent(nil), j.events...)
}

// EVERY DOCUMENT'S FIRST BYTE IS ONE VIEW EVENT, answered under every status
// but a 5xx, and a subresource is none: the objective is the frame's document.
// Mutation: count a 503 as answered, or count subresources, and a case fails.
func TestEveryDocumentsFirstByteIsAViewEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	if res := r.document(f); res.StatusCode != http.StatusOK {
		t.Fatalf("the document answered %d", res.StatusCode)
	}
	r.do(request{url: base(f) + "logo.png", dest: "image"})
	r.do(request{url: f.Origin + "/_crewlet/runtime/1.0.0/runtime.js"})
	got := r.judged.all()
	if len(got) != 1 {
		t.Fatalf("events %+v, want the one document", got)
	}
	if got[0].name != objectives.ViewFirstByte || !got[0].answered || got[0].elapsed != 0 {
		t.Errorf("event %+v, want an answered view_first_byte at the frozen clock's zero", got[0])
	}

	r.reads.set(func(r *reads) { r.headErr = errStale })
	if res := r.document(f); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the stale document answered %d", res.StatusCode)
	}
	if got := r.judged.all(); len(got) != 2 || got[1].answered {
		t.Errorf("events %+v: a stale node's 503 is the failure the objective counts", got)
	}

	r.reads.set(func(r *reads) { r.headErr = nil })
	r.clock.advance(artifacts.TicketLifetime + time.Minute)
	if res := r.document(f); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the expired document answered %d", res.StatusCode)
	}
	if got := r.judged.all(); len(got) != 3 || !got[2].answered {
		t.Errorf("events %+v: an expired ticket is the viewer's to re-mint, and an answer", got)
	}
}

// A REQUEST ITS VIEWER ABANDONED IS NO BAD VIEW EVENT AND NO 5XX: a read the
// request's own cancellation ended (a frame torn down, a browser gone) is no
// fault of the store's. One already late when its viewer left, and one the
// store failed on its own while the viewer waited, still burn.
//
// Mutation: drop the abandoned branch in ServeHTTP, and the first case is a bad
// event and a 5xx.
func TestARequestItsViewerAbandonedIsNoBadViewEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	rec := &recorder{counts: map[string]uint64{}}
	r.meter = objectives.NewMeter(rec, r.clock.now)
	r.build()
	f := r.frame()
	doc := request{url: strings.SplitN(f.URL, "#", 2)[0], dest: "iframe"}
	check := func(when string, bad, abandoned, failed uint64) {
		t.Helper()
		got := []uint64{
			rec.count(metrics.ObjectivesEvents, metrics.Attrs{"outcome": objectives.OutcomeBad}),
			r.rec.count(metrics.ArtifactsServeRequests, metrics.Attrs{"dest": "document", "status": "abandoned"}),
			r.rec.count(metrics.ArtifactsServeRequests, metrics.Attrs{"dest": "document", "status": "5xx"}),
		}
		if got[0] != bad || got[1] != abandoned || got[2] != failed {
			t.Errorf("%s: bad view events, abandoned and 5xx documents %v, want %v", when, got,
				[]uint64{bad, abandoned, failed})
		}
	}

	ctx, leave := context.WithCancel(t.Context())
	r.whileHeld(ctx, doc, func(chan<- error) { leave() })
	check("the viewer left inside the bound", 0, 1, 0)

	ctx, leave = context.WithCancel(t.Context())
	r.whileHeld(ctx, doc, func(chan<- error) {
		r.clock.advance(objectives.ViewFirstByteWithin + time.Millisecond)
		leave()
	})
	check("the viewer left a late document", 1, 2, 0)

	if code := r.whileHeld(t.Context(), doc, func(fail chan<- error) {
		fail <- errors.New("holding: the broker did not answer")
	}); code != http.StatusServiceUnavailable {
		t.Errorf("a store that failed while the viewer waited answered %d, want 503", code)
	}
	check("the store failed while the viewer waited", 2, 2, 1)

	ctx, leave = context.WithCancel(t.Context())
	r.whileHeld(ctx, request{url: base(f) + "logo.png", dest: "image"}, func(chan<- error) { leave() })
	if n := r.rec.count(metrics.ArtifactsServeRequests,
		metrics.Attrs{"dest": "subresource", "status": "abandoned"}); n != 1 {
		t.Errorf("an abandoned file was counted abandoned %d times, want once", n)
	}
}

// A FRAME IS READ BACK FROM ITS URL exactly as the origin would verify it, and
// a URL that is not a live frame minted here is refused without saying why.
func TestAFrameIsReadBackFromItsURL(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	claims, err := r.frames.Claims(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Principal != iam.RecordOwner(r.person) || claims.Lineage != r.lineage ||
		claims.Nonce != f.Nonce || claims.Artifact != r.reads.head.ID {
		t.Errorf("claims %+v do not name the frame's viewer, credential, nonce and artifact", claims)
	}
	other := "https://" + artifacts.IDFor("another").Label().String() + ".example-usercontent.net"
	for name, u := range map[string]string{
		"another artifact's origin": strings.Replace(f.URL, f.Origin, other, 1),
		"no ticket":                 f.Origin + "/index.html",
		"a forged ticket":           f.Origin + "/t/not-a-ticket/index.html",
		"not a URL":                 "::",
	} {
		if _, err := r.frames.Claims(u); !errors.Is(err, artifactserve.ErrNotAFrame) {
			t.Errorf("%s: %v, want ErrNotAFrame", name, err)
		}
	}
	r.clock.advance(artifacts.TicketLifetime)
	if _, err := r.frames.Claims(f.URL); !errors.Is(err, artifactserve.ErrNotAFrame) {
		t.Errorf("a frame whose ticket ended: %v, want ErrNotAFrame", err)
	}
}

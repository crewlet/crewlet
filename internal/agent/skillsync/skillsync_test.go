package skillsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// --- a wiki the tests own -------------------------------------------------- //

// wiki is a knowledge backend holding pages in containers, counting what the
// sync asked of it so a test can say what a change COST as well as what it did.
type wiki struct {
	mu    sync.Mutex
	pages map[string]wikiPage

	walks, reads int

	// failWalks fails that many walks before one succeeds; failReads fails
	// every single-page read while set.
	failWalks int
	failReads bool

	// hold, when set, blocks a walk after it has read the pages and before
	// it returns them, until the channel is closed. It does NOT observe the
	// walk's context: it is a backend whose answer is already in hand when
	// a cancellation lands, which is the case only a generation check can
	// catch.
	hold chan struct{}
}

type wikiPage struct {
	container, text string
	version         int
}

func newWiki() *wiki { return &wiki{pages: map[string]wikiPage{}} }

// skillText is a page declaring a skill with this key, carrying a body the
// test can recognise.
func skillText(key, body string) string {
	return "---\nkey: " + key + "\ntitle: " + key + "\nsummary: how to use " + key +
		"\ntrigger:\n  tool: " + key + "_tool\n---\n" + body
}

func (w *wiki) put(id, container, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	page := w.pages[id]
	w.pages[id] = wikiPage{container: container, text: text, version: page.version + 1}
}

func (w *wiki) remove(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pages, id)
}

func (w *wiki) counts() (walks, reads int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.walks, w.reads
}

func (w *wiki) walk(_ context.Context, container string) ([]skills.Page, error) {
	w.mu.Lock()
	w.walks++
	if w.failWalks > 0 {
		w.failWalks--
		w.mu.Unlock()
		return nil, errors.New("the wiki is unreachable")
	}
	var out []skills.Page
	for id, page := range w.pages {
		if strings.EqualFold(page.container, container) {
			out = append(out, skills.Page{ID: id, Title: id, Version: page.version, Text: page.text})
		}
	}
	hold := w.hold
	w.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return out, nil
}

func (w *wiki) read(_ context.Context, id string) (PageRead, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads++
	if w.failReads {
		return PageRead{}, errors.New("the wiki is unreachable")
	}
	page, ok := w.pages[id]
	if !ok {
		return PageRead{}, nil
	}
	return PageRead{
		Page:      skills.Page{ID: id, Title: id, Version: page.version, Text: page.text},
		Container: page.container, Exists: true,
	}, nil
}

// source is the wiki's container as a sync source.
func (w *wiki) source(container string) Source {
	return Source{
		Backend: "confluence", Container: container, Location: "https://wiki.example.com",
		Walk: w.walk, Page: w.read,
	}
}

// --- harness --------------------------------------------------------------- //

// syncer is a started loop over a fresh registry, with the retry and the
// periodic walk sized for a test rather than for a wiki.
func syncer(t *testing.T, stream Stream, node string, tune func(*Syncer)) (*Syncer, *skills.Registry) {
	t.Helper()
	registry := skills.NewRegistry()
	s, err := New(Options{Registry: registry, Stream: stream, Node: node})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.interval = time.Hour
	s.retryBase = time.Millisecond
	if tune != nil {
		tune(s)
	}
	s.Start(t.Context())
	t.Cleanup(func() { s.Stop(context.Background()) })
	return s, registry
}

// eventually polls a condition, failing with what it last saw.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, saw := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened: %s", what, saw)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// serves waits until a key is served with a body containing want.
func serves(t *testing.T, r *skills.Registry, key, want string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%q serving %q", key, want), func() (bool, string) {
		s, ok := r.Get(key)
		return ok && strings.Contains(s.Body, want), fmt.Sprintf("%+v (present %v)", s, ok)
	})
}

// absent waits until a key is not served.
func absent(t *testing.T, r *skills.Registry, key string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%q gone", key), func() (bool, string) {
		s, ok := r.Get(key)
		return !ok, fmt.Sprintf("%+v", s)
	})
}

// settle gives the loop time to act on something that must NOT happen, so an
// assertion that nothing changed is not merely an assertion made too early.
func settle() { time.Sleep(50 * time.Millisecond) }

// --- the failure path ------------------------------------------------------ //

// A FAILED WALK IS RETRIED. The boot walk used to run once, so a wiki that
// was down for that one request left the node without skills until a page
// edit or a restart.
func TestAFailedWalkIsRetriedUntilItSucceeds(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))
	w.failWalks = 3

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))

	serves(t, r, "deploy", "tag the release")
	if walks, _ := w.counts(); walks != 4 {
		t.Errorf("the walk ran %d times, want three failures and one success", walks)
	}
}

// AN APPLY RETRIES A SOURCE WHOSE LAST WALK FAILED, without waiting out the
// backoff: an apply is the gesture that fixes a broken credential.
func TestAnApplyRetriesASourceWhoseLastWalkFailed(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))
	w.failWalks = 1

	s, r := syncer(t, nil, "", func(s *Syncer) { s.retryBase = time.Hour })
	s.SetSource(w.source("TS"))
	eventually(t, "the first walk", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 1, fmt.Sprintf("%d walks", walks)
	})
	settle()
	if r.Len() != 0 {
		t.Fatal("a failed walk registered something")
	}

	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")
}

// --- applies --------------------------------------------------------------- //

// CONNECTING A KNOWLEDGE BACKEND LIVE LOADS ITS SKILLS, moving the container
// serves the new one's and never the old one's, and turning skills off (or
// disconnecting the backend) serves nothing.
func TestAnApplyThatMovesTheSourceRewalksAndRetiresTheOld(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("old", "from TS"))
	w.put("2", "OPS", skillText("new", "from OPS"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(Source{})
	settle()
	if r.Len() != 0 {
		t.Fatal("a company with skills off got some")
	}

	s.SetSource(w.source("TS"))
	serves(t, r, "old", "from TS")

	s.SetSource(w.source("OPS"))
	// RETIRED AT ONCE, not when the new walk lands: the gap is the new
	// walk's length, and serving the old container meanwhile is serving
	// guidance from a container the company no longer names.
	if _, ok := r.Get("old"); ok {
		t.Fatal("moving the container left the old container's skill served")
	}
	serves(t, r, "new", "from OPS")

	s.SetSource(Source{Backend: "confluence"})
	if r.Len() != 0 {
		t.Fatalf("turning skills off left %d skills served", r.Len())
	}
}

// AN APPLY THAT KEEPS THE SOURCE WALKS NOTHING. An apply that changed a seat's
// model has nothing to say about skills, and rebuilding the client around a
// rotated credential names the same source.
func TestAnApplyThatKeepsTheSourceWalksNothing(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	for range 3 {
		s.SetSource(w.source("ts"))
	}
	settle()
	if walks, _ := w.counts(); walks != 1 {
		t.Fatalf("three applies of an unchanged source walked %d times in all", walks)
	}
	if r.Len() != 1 {
		t.Fatal("an unchanged source lost its skills")
	}
}

// A SOURCE THIS NODE CANNOT READ serves nothing it did not already hold for
// that source, and walks nothing; a source it CAN read that becomes unreadable
// keeps what it had, because nothing has said those skills are wrong.
func TestAnUnreadableSourceWalksNothing(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	unreadable := w.source("TS")
	unreadable.Walk, unreadable.Page = nil, nil
	unreadable.Unreadable = "no org token"
	s.SetSource(unreadable)
	s.Refresh("confluence")
	settle()
	if walks, _ := w.counts(); walks != 1 {
		t.Fatalf("an unreadable source walked (%d walks)", walks)
	}
	if r.Len() != 1 {
		t.Fatal("a source that became unreadable lost the skills it held")
	}

	moved := unreadable
	moved.Container = "OPS"
	s.SetSource(moved)
	if r.Len() != 0 {
		t.Fatal("an unreadable NEW source kept the previous source's skills")
	}
}

// A WALK THAT A SOURCE CHANGE OVERTOOK NEVER INSTALLS. Its answer is for a
// container the company no longer names, and installing it after the change
// emptied the registry would put the retired skills straight back.
func TestAWalkASourceChangeOvertookNeverInstalls(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("old", "from TS"))
	w.hold = make(chan struct{})

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	eventually(t, "the walk to start", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 1, fmt.Sprintf("%d walks", walks)
	})

	s.SetSource(Source{Backend: "confluence"})
	w.mu.Lock()
	close(w.hold)
	w.hold = nil
	w.mu.Unlock()
	settle()
	if r.Len() != 0 {
		t.Fatalf("an overtaken walk installed %d skills", r.Len())
	}
}

// --- page changes ---------------------------------------------------------- //

// A PAGE CHANGE READS ONE PAGE. It used to walk the whole space, spending a
// request per page of the container on every edit.
func TestAPageChangeReadsOnlyThatPage(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))
	w.put("2", "TS", skillText("review", "read the diff"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	if err := s.PageChanged(t.Context(), Change{Backend: "confluence", Container: "TS", PageID: "1"}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	serves(t, r, "deploy", "tag and sign the release")

	walks, reads := w.counts()
	if walks != 1 || reads != 1 {
		t.Fatalf("one page edit cost %d walks and %d reads, want 1 and 1", walks, reads)
	}
	if _, ok := r.Get("review"); !ok {
		t.Fatal("a one-page update lost another page's skill")
	}
}

// A REMOVED PAGE IS DROPPED WITHOUT A READ: the page is gone, so there is
// nothing to ask the backend for.
func TestARemovedPageIsDroppedWithoutARead(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	w.remove("1")
	if err := s.PageChanged(t.Context(), Change{
		Backend: "confluence", Container: "TS", PageID: "1", Removed: true,
	}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	absent(t, r, "deploy")
	if _, reads := w.counts(); reads != 0 {
		t.Fatalf("dropping a removed page read the backend %d times", reads)
	}
}

// A PAGE THAT LEFT THE CONTAINER IS DROPPED, although the delivery names the
// container it moved to. The previous path read the page, saw a foreign space
// and returned, so a skill moved out of the skills space was served for ever.
func TestAPageThatLeftTheContainerIsDropped(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	w.put("1", "HANDBOOK", skillText("deploy", "tag the release"))
	if err := s.PageChanged(t.Context(), Change{
		Backend: "confluence", Container: "HANDBOOK", PageID: "1",
	}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	absent(t, r, "deploy")
}

// AN EDIT ANYWHERE ELSE IN THE WIKI COSTS NOTHING. Every page event in every
// space reaches the sync, and reading each one to find out it is not a skill
// is a request per edit across the whole wiki.
func TestAChangeOutsideTheContainerCostsNothing(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))
	w.put("9", "HANDBOOK", "Welcome to the handbook.")

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	for _, change := range []Change{
		{Backend: "confluence", Container: "HANDBOOK", PageID: "9"},
		{Backend: "native", Container: "TS", PageID: "1"},
		{Backend: "confluence", Container: "TS"},
	} {
		if err := s.PageChanged(t.Context(), change); err != nil {
			t.Fatalf("PageChanged: %v", err)
		}
	}
	settle()
	if walks, reads := w.counts(); walks != 1 || reads != 0 {
		t.Fatalf("edits that concern no skill cost %d walks and %d reads", walks, reads)
	}
}

// A PAGE THAT CANNOT BE READ WALKS THE CONTAINER, which is the path that knows
// how to back off; it also covers every change that arrives before the wiki
// answers again.
func TestAPageReadFailureWalksInstead(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	w.mu.Lock()
	w.failReads = true
	w.mu.Unlock()
	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	if err := s.PageChanged(t.Context(), Change{Backend: "confluence", Container: "TS", PageID: "1"}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	serves(t, r, "deploy", "tag and sign the release")
	if walks, _ := w.counts(); walks != 2 {
		t.Fatalf("a failed page read walked %d times in all, want the boot walk and one more", walks)
	}
}

// A PAGE CHANGE THAT ARRIVES DURING A WALK IS NOT LOST. The walk may already
// have read the page before the edit, and the change is the only thing that
// knows it moved.
func TestAPageChangeDuringAWalkIsAppliedAfterIt(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))
	w.hold = make(chan struct{})

	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	eventually(t, "the walk to read", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 1, fmt.Sprintf("%d walks", walks)
	})

	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	if err := s.PageChanged(t.Context(), Change{Backend: "confluence", Container: "TS", PageID: "1"}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	w.mu.Lock()
	close(w.hold)
	w.hold = nil
	w.mu.Unlock()

	serves(t, r, "deploy", "tag and sign the release")
}

// A REFRESH WALKS, for a backend that can say something moved but not which
// page.
func TestARefreshWalks(t *testing.T) {
	t.Parallel()
	w := newWiki()
	s, r := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	eventually(t, "the first walk", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 1, fmt.Sprintf("%d walks", walks)
	})

	w.put("1", "TS", skillText("deploy", "tag the release"))
	s.Refresh("confluence")
	serves(t, r, "deploy", "tag the release")
}

// A REFRESH FROM ANOTHER BACKEND WALKS NOTHING. The native page projection runs
// for the life of the node, so it goes on reporting native skill pages after an
// apply moved the company's skills to Confluence, and walking the wiki for each
// of those is a request that can find out nothing.
func TestARefreshFromAnotherBackendWalksNothing(t *testing.T) {
	t.Parallel()
	w := newWiki()
	s, _ := syncer(t, nil, "", nil)
	s.SetSource(w.source("TS"))
	eventually(t, "the first walk", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 1, fmt.Sprintf("%d walks", walks)
	})

	s.Refresh("native")
	settle()
	if walks, _ := w.counts(); walks != 1 {
		t.Fatalf("a native refresh walked a Confluence source (%d walks in all)", walks)
	}
}

// A SOURCE THAT BECOMES READABLE AGAIN IS WALKED, and its periodic walk runs
// again after it. While a source cannot be read, every page change is dropped
// and a periodic walk that comes due finds nothing to read, so the apply that
// restores it (a token put back) is the only thing left that can bring the
// registry up to date: leaving it to the periodic walk left it stale for good,
// because only a walk re-arms that timer.
func TestASourceThatBecomesReadableAgainIsWalked(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	const interval = 20 * time.Millisecond
	s, r := syncer(t, nil, "", func(s *Syncer) { s.interval = interval })
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	unreadable := w.source("TS")
	unreadable.Walk, unreadable.Page = nil, nil
	unreadable.Unreadable = "no org token"
	s.SetSource(unreadable)
	walks, _ := w.counts()
	// SEVERAL INTERVALS, so the periodic walk has certainly come due while
	// there was nothing it could read.
	time.Sleep(10 * interval)
	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	if after, _ := w.counts(); after != walks {
		t.Fatalf("an unreadable source was walked (%d walks, then %d)", walks, after)
	}

	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag and sign the release")

	w.put("1", "TS", skillText("deploy", "tag, sign and announce the release"))
	serves(t, r, "deploy", "tag, sign and announce the release")
}

// --- the fleet ------------------------------------------------------------- //

// A CHANGE ONE NODE HEARD REACHES EVERY NODE. Inbound deliveries are a
// fleet-wide consumer group, so exactly one member parses each webhook; every
// other node used to keep the old skill until it restarted.
func TestTwoNodesConvergeOnAChangeOnlyOneHeard(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	start := func(name string) (*Syncer, *skills.Registry) {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("queue Start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		s, r := syncer(t, q, name, nil)
		s.SetSource(w.source("TS"))
		serves(t, r, "deploy", "tag the release")
		return s, r
	}
	won, wonRegistry := start("node-a")
	_, peerRegistry := start("node-b")

	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	if err := won.PageChanged(t.Context(), Change{Backend: "confluence", Container: "TS", PageID: "1"}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	serves(t, wonRegistry, "deploy", "tag and sign the release")
	serves(t, peerRegistry, "deploy", "tag and sign the release")

	// EACH NODE READ THE PAGE ONCE, and the node that won the delivery did
	// not read it again on hearing its own nudge.
	settle()
	if walks, reads := w.counts(); walks != 2 || reads != 2 {
		t.Fatalf("one edit on a two-node fleet cost %d walks and %d reads, "+
			"want the two boot walks and one read per node", walks, reads)
	}

	// AND A REMOVAL travels the same way.
	w.remove("1")
	if err := won.PageChanged(t.Context(), Change{
		Backend: "confluence", Container: "TS", PageID: "1", Removed: true,
	}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	absent(t, wonRegistry, "deploy")
	absent(t, peerRegistry, "deploy")
}

// A NUDGE A NODE NEVER HEARD CONVERGES ON THE PERIODIC WALK. The nudge is best
// effort by design, so what bounds a missed one has to be something that asks
// the wiki again without being told to.
func TestAMissedNudgeConvergesOnThePeriodicWalk(t *testing.T) {
	t.Parallel()
	w := newWiki()
	w.put("1", "TS", skillText("deploy", "tag the release"))

	// A node with no stream hears no peer at all, which is the limiting
	// case of a nudge that was lost.
	s, r := syncer(t, nil, "deaf", func(s *Syncer) { s.interval = 20 * time.Millisecond })
	s.SetSource(w.source("TS"))
	serves(t, r, "deploy", "tag the release")

	w.put("1", "TS", skillText("deploy", "tag and sign the release"))
	serves(t, r, "deploy", "tag and sign the release")
}

// A NUDGE FROM A NODE ON ANOTHER SOURCE IS IGNORED: during a rolling apply the
// fleet briefly disagrees about the container, and a node must not read a page
// from a container its own epoch does not name.
func TestANudgeForAnotherContainerIsIgnored(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	w := newWiki()
	w.put("1", "OPS", skillText("deploy", "from OPS"))

	qa, qb := broker.Client(), broker.Client()
	for _, q := range []*memory.Queue{qa, qb} {
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("queue Start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
	}
	moved, _ := syncer(t, qa, "node-a", nil)
	moved.SetSource(w.source("OPS"))
	stale, staleRegistry := syncer(t, qb, "node-b", nil)
	stale.SetSource(w.source("TS"))
	eventually(t, "both boot walks", func() (bool, string) {
		walks, _ := w.counts()
		return walks == 2, fmt.Sprintf("%d walks", walks)
	})

	if err := moved.PageChanged(t.Context(), Change{Backend: "confluence", Container: "OPS", PageID: "1"}); err != nil {
		t.Fatalf("PageChanged: %v", err)
	}
	settle()
	if _, reads := w.counts(); reads != 1 {
		t.Fatalf("the page was read %d times, want once by the node that serves "+
			"its container and never by the node that does not", reads)
	}
	if staleRegistry.Len() != 0 {
		t.Fatal("a node on another container registered a page it does not serve")
	}
}

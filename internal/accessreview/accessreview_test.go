package accessreview_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/toolname"
)

// request is one review of the SRE.
func request() accessreview.Request {
	return accessreview.Request{Handle: "sre", Baseline: "t-told",
		BaselineAt: time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC), Digest: "d-new",
		ChangedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
		Lines:     []string{"Cloudflare: your level is now Read (was Operate)."}, Open: 3}
}

// A REQUEST IS ITS DELIVERY'S ONE SHAPE: what is published reads back whole,
// and a request no wake can be derived from is refused at both ends.
//
// Mutation: drop a field from the check and a request naming no told record
// is published.
func TestARequestIsItsDeliverysOneShape(t *testing.T) {
	t.Parallel()
	w, err := accessreview.Encode(request())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := accessreview.Decode(w)
	if err != nil || got.Handle != "sre" || got.Baseline != "t-told" || got.Digest != "d-new" ||
		got.Open != 3 || len(got.Lines) != 1 || !got.ChangedAt.Equal(request().ChangedAt) {
		t.Fatalf("the request read back as %+v (%v)", got, err)
	}
	for name, broken := range map[string]func(*accessreview.Request){
		"no seat":     func(r *accessreview.Request) { r.Handle = " " },
		"no baseline": func(r *accessreview.Request) { r.Baseline = "" },
		"no digest":   func(r *accessreview.Request) { r.Digest = "" },
		"no instant":  func(r *accessreview.Request) { r.ChangedAt = time.Time{} },
	} {
		r := request()
		broken(&r)
		if _, err := accessreview.Encode(r); !errors.Is(err, accessreview.ErrNotARequest) {
			t.Errorf("%s: Encode answered %v, want ErrNotARequest", name, err)
		}
	}
}

// A REQUEST'S LINES ARE WHAT A TURN IS TOLD: at most twenty, each at most a
// change line's bound.
func TestARequestsLinesAreBoundedAsATurnsAre(t *testing.T) {
	t.Parallel()
	r := request()
	r.Lines = nil
	for range capability.MaxLines + 5 {
		r.Lines = append(r.Lines, strings.Repeat("x", capability.MaxLineBytes+40))
	}
	w, err := accessreview.Encode(r)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := accessreview.Decode(w)
	if err != nil || len(got.Lines) != capability.MaxLines {
		t.Fatalf("the lines read back as %d (%v), want %d", len(got.Lines), err, capability.MaxLines)
	}
	for _, line := range got.Lines {
		if len(line) > capability.MaxLineBytes {
			t.Fatalf("a line of %d bytes, want at most %d", len(line), capability.MaxLineBytes)
		}
	}
}

// A WAKE IS DERIVED FROM ITS SEAT, ITS TOLD RECORD, ITS DIGEST AND WHEN THE
// CHANGE WAS MEASURED: a republish is the same wake, and the same change made
// again after a revert, measured afresh, is a wake of its own.
//
// Mutation: leave the measured instant out of the id and the redo collapses.
func TestAWakeIsDerivedFromItsSeatItsToldRecordAndItsDigest(t *testing.T) {
	t.Parallel()
	a, b := request(), request()
	b.Lines, b.Open = nil, 9
	if accessreview.WakeID(a) != accessreview.WakeID(b) {
		t.Error("two copies of one review derived two wakes")
	}
	for name, change := range map[string]func(*accessreview.Request){
		"a newer told record": func(r *accessreview.Request) { r.Baseline = "t-review" },
		"another seat":        func(r *accessreview.Request) { r.Handle = "pm" },
		"the change made again": func(r *accessreview.Request) {
			r.ChangedAt = r.ChangedAt.Add(time.Minute)
		},
	} {
		r := request()
		change(&r)
		if accessreview.WakeID(r) == accessreview.WakeID(a) {
			t.Errorf("%s derived the same wake", name)
		}
	}
}

// EVERY COPY OF ONE WAKE CARRIES ONE INSTANT, the baseline told record's,
// which the turn derives its operation ids from.
//
// Mutation: stamp the wake with the parse's clock and two copies differ.
func TestEveryCopyOfAWakeCarriesOneInstant(t *testing.T) {
	t.Parallel()
	var at []time.Time
	for range 2 {
		w, err := accessreview.Encode(request())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		routed, err := accessreview.Parser{}.Parse(t.Context(), w, nil)
		if err != nil || len(routed) != 1 {
			t.Fatalf("Parse answered %d wakes (%v)", len(routed), err)
		}
		at = append(at, routed[0].WakeAt)
	}
	if !at[0].Equal(at[1]) || !at[0].Equal(request().BaselineAt) {
		t.Errorf("two copies of one wake carry %v, want the baseline told record's %v", at, request().BaselineAt)
	}
}

// A QUESTION IS STALE UNTIL THE CHANGE IS TOLD: one asked before any turn was
// told it, or before the turn that was, and none asked after.
func TestAQuestionIsStaleUntilTheChangeIsTold(t *testing.T) {
	t.Parallel()
	told := time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)
	untold := accessreview.Pending{Baseline: "t-told", Digest: "d"}
	if !untold.Stale(told.Add(time.Hour)) {
		t.Error("a question while no turn was told the change is not stale")
	}
	reviewing := untold
	reviewing.ToldTurn, reviewing.ToldAt = "t-review", told
	if !reviewing.Stale(told.Add(-time.Minute)) || reviewing.Stale(told.Add(time.Minute)) {
		t.Error("the told instant does not divide the stale questions from the current ones")
	}
}

// A REVIEW IS THE COMPANY'S OWN TURN: its wake names no sender and no writer,
// so the turn it starts is held to the seat's own authority alone (ADR-0109),
// and nobody waits on an answer from it.
//
// Mutation: name the engine as the wake's sender and the requester ceiling
// reads the turn as a stranger's.
func TestAReviewIsTheCompanysOwnTurn(t *testing.T) {
	t.Parallel()
	w, err := accessreview.Encode(request())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	routed, err := accessreview.Parser{}.Parse(t.Context(), w, nil)
	if err != nil || len(routed) != 1 {
		t.Fatalf("Parse answered %d wakes (%v), want one", len(routed), err)
	}
	r := routed[0]
	if r.To.Handle != "sre" || r.Sender != "" || r.Writer != nil ||
		r.WakeID != accessreview.WakeID(request()) || !r.WakeAt.Equal(request().BaselineAt) {
		t.Errorf("the wake is %+v, want the SRE's, named by nobody, with its derived id", r)
	}
	p := accessreview.Prompt{}
	if p.Addressed(r.Inbound) || p.PartitionKey(r.Metadata, r.Subject) == "" ||
		p.PartitionKey(r.Metadata, r.Subject) != p.ConversationIdentity(r.Metadata, r.Subject) {
		t.Error("a review is addressed, or reviews of one seat do not merge into one conversation")
	}
	if got := notify.Render(p, r.Inbound, nil); !strings.Contains(got, accessreview.SectionTitle) ||
		!strings.Contains(got, "What changed since your last turn") || !strings.Contains(got, "change nothing else") {
		t.Errorf("the review's task is:\n%s", got)
	}
}

// THE SECTION NAMES THE WORK BY KEY AND ID, NEVER BY ANYBODY'S WORDS, and says
// what to do with each item and with which tool.
//
// Mutation: drop the withdraw instruction and a seat has no way to close a
// question it can no longer act on.
func TestTheSectionNamesTheWorkAndHowToWithdrawIt(t *testing.T) {
	t.Parallel()
	if got := (accessreview.Work{}).Section(); got != "" {
		t.Fatalf("no open work renders:\n%s", got)
	}
	w := accessreview.Work{
		Asks: []string{"ENG-12", "ENG-31"}, MoreAsks: 2,
		Runs: []accessreview.Run{{Turn: "t-1", Tool: "ssh_operate", Server: "ssh", Task: "ENG-12"},
			{Turn: "t-2"}},
		Items: []accessreview.Item{{Key: "ENG-14", Status: "in_progress"},
			{Key: "ENG-20", Status: "todo", Blocked: true}},
	}
	// DISTINCT ITEMS: ENG-12 carries a question and a waiting run, and counts
	// once; the run on no item counts on its own, and the rest as counted.
	if w.Count() != 7 {
		t.Errorf("the work counts %d items, want 7", w.Count())
	}
	got := w.Section()
	for _, want := range []string{
		"### " + accessreview.SectionTitle, "ENG-12, ENG-31, and 2 more",
		"a call to `ssh_operate` on `ssh`, held for a person's approval (run `t-1`, on ENG-12)",
		"a question to a person (run `t-2`)", "ENG-14 (in progress)", "ENG-20 (todo, blocked)",
		"`" + toolname.WithdrawAsk + "`", accessreview.UpdatedPrefix,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the section does not say %q:\n%s", want, got)
		}
	}
}

package toolloop_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A response's text reaches the live view WHILE it is being written, as a
// partial kept apart from the narration the record commits.

// A RESPONSE IS VISIBLE WHILE IT IS STILL BEING WRITTEN, and the partial
// APPENDS: a consumer that replaced rather than appended would show only the
// last fragment.
func TestAResponseIsVisibleWhileItIsStillBeingWritten(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := &fragmenting{Script: script(runOf(says("looking it up"))), first: "look"}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var partials []string
	for _, f := range fr.list() {
		if f.Partial != nil {
			partials = append(partials, f.Partial.Content)
		}
	}
	if len(partials) == 0 {
		t.Fatal("nothing was published while the response was being written")
	}
	if last := partials[len(partials)-1]; !strings.HasPrefix(last, "look") || last == "look" {
		t.Errorf("partials = %q, want the fragments accumulated", partials)
	}
}

// fragmenting streams a fragment of its own before the script's response — a
// backend whose text arrives in pieces — and, with restart, abandons it first
// as a chain whose member failed partway does.
type fragmenting struct {
	*llmtest.Script
	first   string
	restart bool
}

func (f *fragmenting) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	req.Send(llm.Delta{Content: f.first})
	if f.restart {
		req.Send(llm.Delta{Restart: true, Model: "backup"})
	}
	return f.Script.Complete(ctx, req)
}

// THE PARTIAL IS GONE ONCE ITS RESPONSE IS COMMITTED. It is a view of a request
// in progress, never a second source of truth: no frame carries a partial for a
// round the record already holds, or the live view shows a finished round and
// a piece of the same round at once — and the finished result carries none.
func TestThePartialIsClearedWhenItsRoundCommits(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := script(runOf(calling("reading", "read"), calling("writing", "read"), says("done")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 4,
		StreamPartials: true, PartialInterval: time.Nanosecond, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	streamed := false
	for i, f := range fr.list() {
		if f.Partial == nil {
			continue
		}
		streamed = true
		for _, r := range f.Rounds {
			if r.Round == f.Partial.Round {
				t.Errorf("frame %d carries a partial of round %d, which its record already holds", i, r.Round)
			}
		}
	}
	if !streamed {
		t.Fatal("no partial was ever published — the scenario did not run as written")
	}
	if last := fr.last(); last.Partial != nil {
		t.Error("the last published view still carried a fragment of a committed round")
	}
	if res.Partial != nil {
		t.Error("a finished Result carries a partial; it is a live-only view")
	}
}

// AN ABANDONED ATTEMPT IS KEPT AND THE RETRY STARTS CLEAN. A failover is
// invisible from inside a delta stream: without the restart a consumer appends
// the replacement to the abandoned attempt and shows two half-answers from two
// models as one paragraph.
func TestAnAbandonedAttemptIsKeptAndTheRetryStartsClean(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := &fragmenting{Script: script(runOf(says("second try"))), first: "first try", restart: true}
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var seen *toolloop.Partial
	for _, f := range fr.list() {
		if f.Partial != nil {
			seen = f.Partial
		}
	}
	if seen == nil {
		t.Fatal("no partial was ever published")
	}
	if strings.Contains(seen.Content, "first try") {
		t.Errorf("content = %q — the retry was appended to the abandoned attempt", seen.Content)
	}
	if len(seen.Abandoned) != 1 || !strings.Contains(seen.Abandoned[0].Content, "first try") {
		t.Errorf("abandoned = %#v, want the attempt that was given up on", seen.Abandoned)
	}
}

// A PROVIDER IS NOT ASKED TO STREAM UNLESS SOMETHING IS WATCHING: a loop with no
// progress listener, or one that did not ask for partials, takes the unary path.
func TestAProviderIsNotAskedToStreamUnlessSomethingIsWatching(t *testing.T) {
	t.Parallel()
	for _, cfg := range []toolloop.Config{
		{StreamPartials: true},
		{OnProgress: func(toolloop.Result) {}},
	} {
		prov := script(runOf(says("hi")))
		cfg.Provider, cfg.Surface, cfg.MaxRounds = prov, newSurface(), 2
		if _, err := run(t, cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if prov.Seen()[0].Streaming() {
			t.Errorf("a loop with stream %v and a listener %v asked the provider to stream",
				cfg.StreamPartials, cfg.OnProgress != nil)
		}
	}
}

// A STREAMED RESPONSE NAMES A MODEL BEFORE THE RUN RETURNS: the configured name
// stands in until the real one exists — and the model that ACTUALLY served
// still wins, since it is the billable fact.
func TestAStreamedResponseNamesAModelBeforeTheRunReturns(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := &configured{Script: script(runOf(says("done"))), name: "configured-model", served: "served-model"}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 2,
		StreamPartials: true, PartialInterval: time.Nanosecond, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	streamed := false
	for _, f := range fr.list() {
		if f.Partial != nil {
			streamed = true
			if f.Model == "" {
				t.Fatalf("a streamed response published with no model: %+v", f)
			}
		}
	}
	if !streamed {
		t.Fatal("nothing was streamed — the scenario did not run as written")
	}
	if res.Model != "served-model" {
		t.Errorf("model = %q, want the one the answer named", res.Model)
	}
}

// TEXT IS FILED UNDER THE REQUEST IT BELONGS TO, not the one after it. A CLI's
// record can show a request before its prose is written — Claude Code's
// opens at message_start — so the rest of that prose arrives after the host
// already counts the request; filed by arrival, it was drawn as the next
// round, a round the model had not begun.
func TestTextStreamedAfterItsRequestIsShownStaysOnThatRequest(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := script(runOf(llmtest.Response{Text: "first", Trailing: " and the rest"}))
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 3,
		StreamPartials: true, PartialInterval: time.Nanosecond, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range fr.list() {
		if f.Partial != nil && f.Partial.Round != 1 {
			t.Errorf("a live frame drew %q as round %d, which the model never began",
				f.Partial.Content, f.Partial.Round)
		}
	}
}

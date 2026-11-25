package sandboxtest

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A JOB'S MEASURE IS ITS OWN, OR CARRIED ALONG ITS COPY. The read that collects a
// job records what the newest request of the session its box ran carried, beside
// how its harness ended, by the copy's lineage: a request the job's own run
// made is that job's own measure; an earlier request of the session it took up
// is the measure the record carried where its figure is the same and the box did
// not say it started a session of its own, else the session's, whose job is not
// known; none keeps what the record carried, unless its box said it started a
// session of its own, whose figure the carried one never was. Every later write
// of the job's record writes it back as read, until the next launch's record.
//
// Mutation: invert any of the four, record a carried figure as the job's own
// request, or rebuild the record in a write that is not a relaunch's.
func testAJobsMeasureIsItsOwnOrCarriedAlongItsCopy(t *testing.T, s sandbox.PendingStore) {
	carried := &sandbox.SessionMeasure{Tokens: 61000, Source: sandbox.MeasureCapture, Launch: "job-earlier"}
	// launched opens turn's job, its attach naming its command and the measure
	// its launch carried, and claims it, as its collection is made on a claim.
	launched := func(t *testing.T, turn string) (string, sandbox.PendingRun) {
		t.Helper()
		launch := mustLaunched(t, s, run(turn))
		if err := s.AttachSandbox(t.Context(), turn, sandbox.BoxRef{SandboxID: "box-" + turn, CommandID: "cmd-" + turn,
			Brief: sandbox.BriefContinued, Context: carried}, sandbox.Fence{}); err != nil {
			t.Fatalf("the attach that names the job's command: %v", err)
		}
		if got := mustGet(t, s, turn).Launch.Context; !sameMeasure(got, carried) {
			t.Fatalf("the attach recorded the measure %+v, want the one its launch carried, %+v", got, carried)
		}
		return launch, mustClaim(t, s, turn)
	}
	for name, tc := range map[string]struct {
		turn    string
		started sandbox.SessionStart
		read    sandbox.RequestMeasure
		want    func(launch string) *sandbox.SessionMeasure
	}{
		"a request the job's own run made": {turn: "t-measure-own",
			read: sandbox.RequestMeasure{Tokens: 4320, Own: true},
			want: func(launch string) *sandbox.SessionMeasure {
				return &sandbox.SessionMeasure{Tokens: 4320, Source: sandbox.MeasureRequest, Launch: launch}
			},
		},
		"an earlier request of the session, the figure carried": {turn: "t-measure-carried",
			read: sandbox.RequestMeasure{Tokens: 61000},
			want: func(string) *sandbox.SessionMeasure { return carried },
		},
		"an earlier request of the session its box started, the carried figure by chance": {
			turn: "t-measure-fresh-earlier", started: sandbox.SessionStartFresh,
			read: sandbox.RequestMeasure{Tokens: 61000},
			want: func(string) *sandbox.SessionMeasure {
				return &sandbox.SessionMeasure{Tokens: 61000, Source: sandbox.MeasureSession}
			},
		},
		"an earlier request of the session, another figure": {turn: "t-measure-session",
			read: sandbox.RequestMeasure{Tokens: 70000},
			want: func(string) *sandbox.SessionMeasure {
				return &sandbox.SessionMeasure{Tokens: 70000, Source: sandbox.MeasureSession}
			},
		},
		"no request, the box having said nothing": {turn: "t-measure-unsaid",
			want: func(string) *sandbox.SessionMeasure { return carried },
		},
		"no request, the box having taken the session up": {turn: "t-measure-continued",
			started: sandbox.SessionStartContinued,
			want:    func(string) *sandbox.SessionMeasure { return carried },
		},
		"no request, the box having started a session of its own": {turn: "t-measure-fresh",
			started: sandbox.SessionStartFresh,
			want:    func(string) *sandbox.SessionMeasure { return nil },
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			turn := tc.turn
			launch, claimed := launched(t, turn)
			if tc.started != "" {
				if ok, err := s.MarkSessionStart(ctx, turn, launch, tc.started); err != nil || !ok {
					t.Fatalf("MarkSessionStart = %v, %v", ok, err)
				}
			}
			if _, err := s.MarkHarnessEnd(ctx, turn, launch, sandbox.EndRead{End: sandbox.HarnessEndOwnRun,
				Request: tc.read}); err != nil {
				t.Fatalf("MarkHarnessEnd: %v", err)
			}
			want := tc.want(launch)
			check := func(step string) {
				t.Helper()
				got := mustGet(t, s, turn).Launch.Context
				if !sameMeasure(got, want) {
					t.Errorf("after %s the job's record holds the measure %+v, want %+v", step, got, want)
				}
			}
			check("the read that collected the job")
			mustRelease(t, s, claimed)
			check("the release of its claim")
			if err := s.SetStatus(ctx, turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
				t.Fatal(err)
			}
			check("a status write")
			if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
				Call: sandbox.BridgeCall{Name: "read_page"}}); err != nil {
				t.Fatal(err)
			}
			check("a bridged call")
			park(t, s, turn)
			check("a park")
			mustBeginLaunch(t, s, run(turn))
			if got := mustGet(t, s, turn).Launch.Context; got != nil {
				t.Errorf("the next launch's job reads the measure %+v, the job before's, want none", got)
			}
		})
	}
}

// A JOB'S COUNT IS ITS OWN READS', AND NEVER CARRIED. The read that collects a job
// records what its harness counts the copy its box holds at beside how its
// harness ended: the read's count wherever it made one, a mark among them, so a
// later read that could not bound the copy replaces an earlier bound; the
// record's own where a read made none; a mark longer than a record carries
// written as unread; a restart longer than a uuid's written as none read and the
// count as unread, so a record never keeps a mark beside no identity. Every
// later write of the record keeps it, and the next launch's starts with none: a
// count is the read of this job's copy, never a lineage's. Mutation: carry the
// count through the reset, write a mark or restart past its bound whole, keep a
// mark beside a dropped restart, or keep an earlier bound over a later mark.
func testAJobsCountIsItsOwnReadsAndNeverCarried(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-count-own"
	launch := mustLaunched(t, s, run(turn))
	claimed := mustClaim(t, s, turn)
	bound := &sandbox.ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("u-1")}
	long := strings.Repeat("u", sandbox.RestartIDMaxBytes+1)
	mark := func(read *sandbox.ResumedCount) {
		t.Helper()
		if _, err := s.MarkHarnessEnd(t.Context(), turn, launch, sandbox.EndRead{End: sandbox.HarnessEndOwnRun,
			Count: read}); err != nil {
			t.Fatalf("MarkHarnessEnd: %v", err)
		}
	}
	check := func(step string, want *sandbox.ResumedCount) {
		t.Helper()
		if got := mustGet(t, s, turn).Launch.ResumedCount; !sameCount(got, want) {
			t.Errorf("after %s the job's record holds the count %+v, want %+v", step, got, want)
		}
	}
	mark(bound)
	check("the read that collected the job", bound)
	mark(nil)
	check("a read that made no count", bound)
	mark(&sandbox.ResumedCount{Mark: sandbox.CountMedia, Restart: new(long)})
	check("a marked read naming a restart past its bound", &sandbox.ResumedCount{Mark: sandbox.CountUnread})
	mark(bound)
	mark(&sandbox.ResumedCount{Tokens: 42_000, Restart: new(long)})
	check("a read naming a restart past its bound", &sandbox.ResumedCount{Mark: sandbox.CountUnread})
	mark(&sandbox.ResumedCount{Mark: sandbox.CountUnread})
	check("a later read that could not bound the copy", &sandbox.ResumedCount{Mark: sandbox.CountUnread})
	mark(&sandbox.ResumedCount{Mark: sandbox.CountMark(strings.Repeat("m", 33))})
	check("a read naming a mark past its bound", &sandbox.ResumedCount{Mark: sandbox.CountUnread})
	mark(bound)
	mustRelease(t, s, claimed)
	check("the release of its claim", bound)
	if err := s.SetStatus(t.Context(), turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write", bound)
	park(t, s, turn)
	check("a park", bound)
	mustBeginLaunch(t, s, run(turn))
	check("the next launch's begin", nil)
}

// A COUNT LANDS WHERE THE END IS ALREADY RECORDED. The read that collects a job
// records its harness's end, its newest request's measure and its count in one
// write, which lands where any changes: a collection made again, whose end the
// first read recorded already, brings a count the first had none of. The same
// read again changes nothing.
//
// Mutation: write only where the end or the measure changes, and the count is
// never recorded.
func testACountLandsWhereTheEndIsAlreadyRecorded(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-count-after-end"
	launch := mustLaunched(t, s, run(turn))
	mustClaim(t, s, turn)
	if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil ||
		!ok {
		t.Fatalf("the first read's record = %v, %v", ok, err)
	}
	count := &sandbox.ResumedCount{Tokens: 42_000, Restart: new("")}
	read := sandbox.EndRead{End: sandbox.HarnessEndCutOff, Count: count}
	if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || !ok {
		t.Fatalf("a read bringing a count beside the end recorded = %v, %v, want it written", ok, err)
	}
	if got := mustGet(t, s, turn).Launch.ResumedCount; !sameCount(got, count) {
		t.Errorf("the job's record holds the count %+v, want %+v", got, count)
	}
	if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || ok {
		t.Errorf("the same read again = %v, %v, want nothing written", ok, err)
	}
}

// THE NEXT REQUEST IS NEVER CARRIED. The read that collects a job records what
// the harness estimates the next request of the copy its box holds at beside
// how its harness ended: the read's wherever it made one, and the record's own
// where a read made none, since the copy of a job that is over does not change.
// Every write of the job's record after it keeps it, and the next launch's
// record starts with none: an estimate is the read of this job's copy, never a
// lineage's, since a job that made no request of its own may have grown the
// copy it took up by its brief.
//
// Mutation: carry the estimate through the relaunch's reset, and this goes red.
func testTheNextRequestIsNeverCarried(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-next-own"
	launch := mustLaunched(t, s, run(turn))
	claimed := mustClaim(t, s, turn)
	estimate := sandbox.NextRequest{Tokens: 42_000, OlderTokens: 31_500}
	mark := func(read sandbox.NextRequest) {
		t.Helper()
		if _, err := s.MarkHarnessEnd(t.Context(), turn, launch, sandbox.EndRead{End: sandbox.HarnessEndOwnRun,
			Next: read}); err != nil {
			t.Fatalf("MarkHarnessEnd: %v", err)
		}
	}
	check := func(step string, want sandbox.NextRequest) {
		t.Helper()
		if got := mustGet(t, s, turn).Launch.NextRequest(); got != want {
			t.Errorf("after %s the job's record holds the next request %+v, want %+v", step, got, want)
		}
	}
	mark(estimate)
	check("the read that collected the job", estimate)
	mark(sandbox.NextRequest{})
	check("a read that made none", estimate)
	mustRelease(t, s, claimed)
	check("the release of its claim", estimate)
	if err := s.SetStatus(t.Context(), turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write", estimate)
	park(t, s, turn)
	check("a park", estimate)
	mustBeginLaunch(t, s, run(turn))
	check("the next launch's begin", sandbox.NextRequest{})
}

// A HARNESS END WRITES THE NEXT REQUEST. The read that collects a job records its
// harness's end, its newest request's measure, its count and its estimate of the
// next request in one write, which lands where any changes: a collection made
// again, whose end the first read recorded already, brings an estimate the
// first had none of, or another. The same read again changes nothing.
//
// Mutation: leave the estimate out of the write's test of a change, and this
// goes red.
func testAHarnessEndWritesTheNextRequest(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-next-after-end"
	launch := mustLaunched(t, s, run(turn))
	mustClaim(t, s, turn)
	if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil ||
		!ok {
		t.Fatalf("the first read's record = %v, %v", ok, err)
	}
	for _, estimate := range []sandbox.NextRequest{{Tokens: 42_000, OlderTokens: 31_500}, {Tokens: 43_000}} {
		read := sandbox.EndRead{End: sandbox.HarnessEndCutOff, Next: estimate}
		if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || !ok {
			t.Fatalf("a read bringing the estimate %+v beside the end recorded = %v, %v, want it written",
				estimate, ok, err)
		}
		if got := mustGet(t, s, turn).Launch.NextRequest(); got != estimate {
			t.Errorf("the job's record holds the next request %+v, want %+v", got, estimate)
		}
		if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || ok {
			t.Errorf("the same read again = %v, %v, want nothing written", ok, err)
		}
	}
}

// A HARNESS END WRITES THE COUNT'S PROVIDER AND WRITER: a read that brings an
// OpenCode count whose figure is already on the job's record, anchored on
// another provider's reply or read off a copy another release wrote, is a
// change to record beside the end, since a figure is the report of the provider
// and the release beside it; and the same read again writes nothing.
//
// Mutation: leave the provider or the writer out of the write's test of a
// change, and this goes red.
func testAHarnessEndWritesTheCountsProvider(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-provider-after-end"
	launch := mustLaunched(t, s, run(turn))
	mustClaim(t, s, turn)
	count := &sandbox.ResumedCount{Tokens: 42_000, Restart: new("msg_c")}
	for _, said := range []struct{ provider, release string }{{"stub", "2.3.2"}, {"openrouter", "2.3.2"},
		{"openrouter", "2.4.0"}} {
		read := sandbox.EndRead{End: sandbox.HarnessEndOwnRun, Count: count, CountProvider: said.provider,
			CountRelease: said.release}
		if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || !ok {
			t.Fatalf("a read bringing %+v beside the count recorded = %v, %v, want it written", said, ok, err)
		}
		if got := mustGet(t, s, turn).Launch; got.CountProvider != said.provider ||
			got.CountRelease != said.release {
			t.Errorf("the job's record names the provider %q written by %q, want %+v", got.CountProvider,
				got.CountRelease, said)
		}
		if ok, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil || ok {
			t.Errorf("the same read again = %v, %v, want nothing written", ok, err)
		}
	}
}

// THE COPY'S WRITER IS HANDED TO THE RELAUNCH AND NEVER CARRIED. The release
// the read that collected a job names beside its count is kept by a later read
// that brings no count, and the relaunch that replaces the job's record is
// handed it as the writer of the copy the box holds, in the write that replaces
// the record, whose own record names none: the next job's copy is its own run's.
//
// Mutation: hand the relaunch no writer, or carry it onto the new record, and
// this goes red.
func testTheCopysWriterIsHandedToTheRelaunchAndNeverCarried(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-writer-handed"
	launch := mustLaunched(t, s, run(turn))
	claimed := mustClaim(t, s, turn)
	count := &sandbox.ResumedCount{Tokens: 42_000, Restart: new("msg_c")}
	for _, read := range []sandbox.EndRead{
		{End: sandbox.HarnessEndOwnRun, Count: count, CountProvider: "stub", CountRelease: "2.3.2"},
		{End: sandbox.HarnessEndOwnRun},
	} {
		if _, err := s.MarkHarnessEnd(t.Context(), turn, launch, read); err != nil {
			t.Fatalf("MarkHarnessEnd: %v", err)
		}
		if got := mustGet(t, s, turn).Launch.CountRelease; got != "2.3.2" {
			t.Errorf("after a read bringing %+v the job's record names the writer %q, want 2.3.2", read, got)
		}
	}
	mustRelease(t, s, claimed)
	park(t, s, turn)
	opened, err := s.BeginLaunch(t.Context(), run(turn), sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if opened.Prior.Writer != "2.3.2" {
		t.Errorf("the relaunch was handed the writer %q, want 2.3.2", opened.Prior.Writer)
	}
	if got := mustGet(t, s, turn).Launch.CountRelease; got != "" {
		t.Errorf("the relaunch's own record names the writer %q, want none", got)
	}
}

// sameCount reports whether a and b say the same of a session's count, a nil one
// none at all.
func sameCount(a, b *sandbox.ResumedCount) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	case (a.Restart == nil) != (b.Restart == nil):
		return false
	case a.Restart != nil && *a.Restart != *b.Restart:
		return false
	}
	return a.Tokens == b.Tokens && a.Mark == b.Mark && a.Window == b.Window
}

// sameMeasure reports whether a and b say the same of a session's measure: its
// figure, where it came from and whose it is, a nil one none at all.
func sameMeasure(a, b *sandbox.SessionMeasure) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Tokens == b.Tokens && a.Source == b.Source && a.Launch == b.Launch
}

// A MEASURE LANDS WHERE THE END IS ALREADY RECORDED. The read that collects a job
// records its harness's end and its newest request's measure in one write, which
// lands where either changes: a collection made again, whose end the first read
// recorded already, brings a measure the first had none of, and a test of the end
// alone would drop it. The same end and measure again change nothing.
//
// Mutation: write only where the end changes, and the measure is never recorded.
func testAMeasureLandsWhereTheEndIsAlreadyRecorded(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-measure-after-end"
	launch := mustLaunched(t, s, run(turn))
	mustClaim(t, s, turn)
	if ok, err := s.MarkHarnessEnd(ctx, turn, launch, sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil ||
		!ok {
		t.Fatalf("the first read's record = %v, %v", ok, err)
	}
	read := sandbox.EndRead{End: sandbox.HarnessEndCutOff, Request: sandbox.RequestMeasure{Tokens: 4320, Own: true}}
	if ok, err := s.MarkHarnessEnd(ctx, turn, launch, read); err != nil || !ok {
		t.Fatalf("a read bringing a measure beside the end recorded = %v, %v, want it written", ok, err)
	}
	if got := mustGet(t, s, turn).Launch; got.HarnessEnd != sandbox.HarnessEndCutOff ||
		!sameMeasure(got.Context, &sandbox.SessionMeasure{Tokens: 4320, Source: sandbox.MeasureRequest, Launch: launch}) {
		t.Errorf("the job's record says %q and holds the measure %+v, want the cut-off end and the job's own "+
			"request", got.HarnessEnd, got.Context)
	}
	if ok, err := s.MarkHarnessEnd(ctx, turn, launch, read); err != nil || ok {
		t.Errorf("the same read again = %v, %v, want nothing written", ok, err)
	}
}

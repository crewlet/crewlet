package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// A COLLECTION RECORDS WHAT THE JOB'S LAST REQUEST CARRIED. The read that collects
// a job writes on the job's own record, beside how its harness ended, what the
// newest request of the session its box ran carried as its run's own account
// measured it (Result.LastRequest): the figure, as a request, of that job. A
// stop's read records nothing: the run's ending is decided, and nothing
// continues its copy.
//
// Mutation: drop the read's measure from the record's write (markHarnessEnd
// handed the end alone), and the record holds none.
func TestACollectionRecordsWhatTheJobsLastRequestCarried(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		read BoxRead
		want func(launch string) *SessionMeasure
	}{
		"a collection's read": {read: BoxReadCollect, want: func(launch string) *SessionMeasure {
			return &SessionMeasure{Tokens: 4320, Source: MeasureRequest, Launch: launch}
		}},
		"a stop's read": {read: BoxReadStop, want: func(string) *SessionMeasure { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			launch := rig.launch("t1").LaunchID
			rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
				LastRequest: RequestMeasure{Tokens: 4320, Own: true}})
			if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), tc.read); err != nil {
				t.Fatalf("readBox: %v", err)
			}
			got, want := rig.get("t1").Launch.Context, tc.want(launch)
			if !got.same(want) {
				t.Errorf("the job's record holds the measure %+v, want %+v", got, want)
			}
		})
	}
}

// A COLLECTION MADE AGAIN KEEPS A LATER BUILD'S WORD ON ITS MEASURE. Mid-upgrade, a
// later build may collect a job first and write its measure with a key this build
// does not know beside the figure. A collection of the job made again on this
// build (a claim handed back after a resume that failed) finds the same end and
// the same figure of the job's own request, which changes nothing this build
// knows: it writes nothing, and the later build's key stays.
//
// Mutation: hand back a measure built afresh where the record's says the same,
// and the write lands and drops the key.
func TestACollectionMadeAgainKeepsALaterBuildsWordOnItsMeasure(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launch := rig.launch("t1").LaunchID
	word := json.RawMessage(`{"window":200000}`)
	if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		run.Launch.HarnessEnd = HarnessEndOwnRun
		run.Launch.Context = &SessionMeasure{Tokens: 4320, Source: MeasureRequest, Launch: launch,
			Extra: map[string]json.RawMessage{"a_later_builds_word": word}}
		return true
	}); err != nil {
		t.Fatalf("the later build's collection: %v", err)
	}
	written, err := rig.pending.MarkHarnessEnd(t.Context(), "t1", launch, EndRead{End: HarnessEndOwnRun,
		Request: RequestMeasure{Tokens: 4320, Own: true}})
	if err != nil {
		t.Fatalf("MarkHarnessEnd: %v", err)
	}
	got := rig.get("t1").Launch.Context
	if written || got == nil || string(got.Extra["a_later_builds_word"]) != string(word) {
		t.Errorf("the collection made again wrote %v, the record holding the measure %+v, want nothing "+
			"written and the later build's word kept", written, got)
	}
}

// A CLAIM IS TOLD WHAT THE READ MEASURED. The claim the collection is made for was
// read before the read wrote the job's record, and the resume that follows on it
// (a round a replaced sign-in carries on in the job's box, in the same frame)
// holds its relaunch to the measure the claim carries: so the collection merges
// the read's measure onto the claim by the rule the record follows.
//
// Mutation: leave the claim's measure as it was read, and the resume reads none.
func TestAClaimIsToldWhatTheReadMeasured(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launch := rig.launch("t1").LaunchID
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
		LastRequest: RequestMeasure{Tokens: 4320, Own: true}})
	var (
		mu      sync.Mutex
		claimed []*SessionMeasure
	)
	rig.resumer.failWith(errors.New("held for the case to read the claim"))
	rig.resumer.during = func(_ context.Context, run PendingRun) {
		mu.Lock()
		defer mu.Unlock()
		claimed = append(claimed, run.Launch.Context)
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Logf("OnCompleted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := &SessionMeasure{Tokens: 4320, Source: MeasureRequest, Launch: launch}
	if len(claimed) != 1 || !claimed[0].same(want) {
		t.Errorf("the resume on the claim read the measures %+v, want once %+v", claimed, want)
	}
}

// A LAUNCH CARRIES THE MEASURE OF THE SESSION IT HANDS ITS RUN, along that session's
// lineage, saying where it came from: a relaunch that continues the copy the job
// before left in the box, that job's record's measure of it, read by the reset
// that opened the launch's job in the write that replaced that record; one that
// continues the turn's kept capture, the capture's own measure of its archive,
// naming the capture's job as the archive's and never as that job's request; one
// that continues the session the conversation's last turn left, the frozen
// base's; and one that starts a session of its own, none. The job's record holds
// it from its command's attach, for a job that makes no request of its own.
//
// Mutation: give the copy in place the capture's measure, or none, and its row
// goes red; name the capture's job as its request's, and that row does.
func TestALaunchCarriesTheMeasureOfTheSessionItHandsItsRun(t *testing.T) {
	t.Parallel()
	t.Run("the copy in its box", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		var first string
		askAndAnswer(t, rig, false, func(ctx context.Context, r PendingRun, asked *AskedRun) {
			first = r.LaunchID
			// THE ASKING JOB'S COLLECTION MEASURED ITS NEWEST REQUEST.
			if _, err := rig.pending.MarkHarnessEnd(ctx, "t1", r.LaunchID, EndRead{End: HarnessEndOwnRun,
				Request: RequestMeasure{Tokens: 4320, Own: true}}); err != nil {
				t.Errorf("MarkHarnessEnd: %v", err)
			}
			if _, err := rig.launchVia(ctx, rig.manager, continuing(asked)); err != nil {
				t.Errorf("relaunch: %v", err)
			}
		})
		want := &SessionMeasure{Tokens: 4320, Source: MeasureRequest, Launch: first}
		if got := rig.get("t1").Launch.Context; !got.same(want) {
			t.Errorf("the job continuing the copy in its box holds the measure %+v, want its job's %+v", got, want)
		}
	})
	t.Run("a kept capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			kept.ContextTokens = 61_000
			if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
				run.Kept.ContextTokens = kept.ContextTokens
				return true
			}); err != nil {
				t.Fatalf("measuring the kept capture: %v", err)
			}
			if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			want := &SessionMeasure{Tokens: 61_000, Source: MeasureCapture, Launch: kept.Launch}
			if got := rig.get("t1").Launch.Context; !got.same(want) {
				t.Errorf("the job continuing the kept capture holds the measure %+v, want the capture's %+v", got,
					want)
			}
		})
	})
	t.Run("the conversation's session", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, logbook, _ := conversingRig(t)
			base := logbook.endedTurn(t, "b1", "s-base", "the base's transcript")
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager,
				continuingBase("t2", frozenReadOf(logbook, base))); err != nil {
				t.Fatalf("the turn's first launch: %v", err)
			}
			want := &SessionMeasure{Tokens: base.ContextTokens, Source: MeasureBase}
			if got := rig.get("t2").Launch.Context; !got.same(want) {
				t.Errorf("the job continuing the conversation's session holds the measure %+v, want the base's %+v",
					got, want)
			}
		})
	})
	t.Run("a session of its own", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if got := rig.get("t1").Launch.Context; got != nil {
			t.Errorf("a job starting a session of its own holds the measure %+v, want none", got)
		}
	})
}

// AN OLDER BUILD'S WRITE CARRIES A JOB'S MEASURE. A fleet mid-upgrade rewrites the
// job's record on a build that does not know the field, and the relaunch that
// reads it may run on this build: so the measure rides the record under its own
// key, which a build that does not declare it keeps verbatim and writes back
// (wirekeys.go); and a key a later build adds inside the measure rides the
// measure's own Extra through this build's writes.
//
// Mutation: leave the field off the wire, and no older build's record holds a key
// of it; drop the carried keys from the record's encode (marshalCarrying), or
// from the measure's (its MarshalJSON writing its plain twin alone), and a
// carried key is gone after the write.
func TestAnOlderBuildsWriteCarriesAJobsMeasure(t *testing.T) {
	t.Parallel()
	measure := &SessionMeasure{Tokens: 4320, Source: MeasureRequest, Launch: "job-1"}
	raw, err := json.Marshal(LaunchRecord{Model: "m", Context: measure})
	if err != nil {
		t.Fatal(err)
	}
	declared := wireKeys(reflect.TypeFor[LaunchRecord]())
	if !declared["session_context"] {
		t.Fatalf("the job's record declares no key session_context: %v", declared)
	}
	older := maps.Clone(declared)
	delete(older, "session_context")
	type plain LaunchRecord
	var read plain
	extra, err := unmarshalCarrying(raw, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	read.Context, read.Model = nil, "m2"
	written, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back LaunchRecord
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if back.Context == nil || back.Context.Tokens != 4320 || back.Context.Source != MeasureRequest ||
		back.Context.Launch != "job-1" || back.Model != "m2" {
		t.Errorf("after the older build's write the job's record holds the measure %+v and model %q, want %+v "+
			"beside its own write: %s", back.Context, back.Model, measure, written)
	}

	// A LATER BUILD'S KEY INSIDE THE MEASURE, through this build's write of the
	// record.
	later := []byte(`{"model":"m","session_context":{"tokens":4320,"source":"request","launch":"job-1",` +
		`"a_later_builds_word":{"window":200000}}}`)
	var record LaunchRecord
	if err := json.Unmarshal(later, &record); err != nil {
		t.Fatal(err)
	}
	record.Model = "m3"
	rewritten, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var kept struct {
		Context map[string]json.RawMessage `json:"session_context"`
	}
	if err := json.Unmarshal(rewritten, &kept); err != nil {
		t.Fatal(err)
	}
	if got := string(kept.Context["a_later_builds_word"]); got != `{"window":200000}` {
		t.Errorf("after this build's write the measure carries %q of a later build's key, want it verbatim: %s",
			got, rewritten)
	}
}

// EVERY MEASURE SOURCE IS VALID, and nothing else is: a record a later build wrote
// may name a source this build does not know, which is read as the value it is,
// never as a fresh measure of the job.
//
// Mutation: leave a source out of the list, and its row goes red.
func TestEveryMeasureSourceIsValid(t *testing.T) {
	t.Parallel()
	for _, source := range []MeasureSource{MeasureRequest, MeasureCapture, MeasureBase, MeasureSession} {
		if !source.Valid() {
			t.Errorf("%q is not valid", source)
		}
	}
	for _, source := range []MeasureSource{"", "a-later-builds-source", "Request"} {
		if source.Valid() {
			t.Errorf("%q is valid", source)
		}
	}
	if !slices.Equal(MeasureSources, []MeasureSource{MeasureRequest, MeasureCapture, MeasureBase, MeasureSession}) {
		t.Errorf("the sources are %v, want the four", MeasureSources)
	}
}

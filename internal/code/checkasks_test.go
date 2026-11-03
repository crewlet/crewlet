package code_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// rerunning asks for the attempt after key's, asked by by.
func rerunning(key code.CheckKey, by code.Principal) code.ChecksRequest {
	key.Attempt++
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_rerun"),
		Asker: by, Rerun: &code.CheckRerun{CheckKey: key}}
}

// reporting asks for attempt key reported in state by the person ada.
func reporting(key code.CheckKey, state code.CheckState, url string) code.ChecksRequest {
	r := &code.CheckReported{CheckKey: key, State: state, URL: url}
	if state != code.CheckStateRunning {
		r.Summary = "ci said " + string(state)
	}
	return code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_report"), Asker: ada, Report: r}
}

// ranTo queues, claims and ends attempt key of a check the engine runs, in
// state for reason.
func ranTo(t *testing.T, w *code.ChecksWriter, key code.CheckKey, state code.CheckState, reason code.CheckReason) {
	t.Helper()
	if key.Attempt == 1 {
		res, err := w.Publish(t.Context(), queueing(key, code.QueueDispatch))
		requireChecked(t, "the dispatch", res, err)
	}
	res, err := w.Publish(t.Context(), claiming(key, "git-1", ""))
	requireChecked(t, "the claim", res, err)
	res, err = w.Publish(t.Context(), ending(key, state, reason))
	requireChecked(t, "the ending", res, err)
}

// A SEAT RERUNS ONLY WHAT ITS INFRASTRUCTURE ENDED: a measurement stands
// against a seat however its grant reads — `failed` is refused
// rerun_genuine, so no agent re-rolls a red check until a flake turns it
// green — while a person reruns it, the attempt carrying the definition the
// one before ran under and naming who asked; an errored attempt a seat that
// may write the repository reruns, and a seat that may only read it may not;
// and a rerun of a latest the rows have moved past is answered from the
// attempt already queued.
func TestAnAgentCannotRerunAGenuineFailure(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, _, _, _ := checksWriterOn(t, n)
	first := checkOn(repo, 1)
	ranTo(t, w, first, code.CheckStateFailed, code.CheckReasonExit)

	res, err := w.Publish(t.Context(), rerunning(first, dev))
	requireCheckRefused(t, "a seat's rerun of a failure", res, err, code.CheckRefusalRerunGenuine)

	res, err = w.Publish(t.Context(), rerunning(first, ada))
	requireChecked(t, "a person's rerun of the failure", res, err)
	if res.Row.Attempt != 2 || res.Row.State != code.CheckStateQueued || res.Row.TimeoutSeconds != 600 ||
		res.Row.Purpose != code.PurposeTip {
		t.Fatalf("the person's rerun queued %+v, want attempt 2 queued under the first's definition", res.Row)
	}

	second := checkOn(repo, 2)
	ranTo(t, w, second, code.CheckStateErrored, code.CheckReasonBoxFailed)
	res, err = w.Publish(t.Context(), rerunning(second, agent("reader")))
	requireCheckRefused(t, "a rerun by a seat that may only read", res, err, code.CheckRefusalNotPermitted)
	res, err = w.Publish(t.Context(), rerunning(second, dev))
	requireChecked(t, "a seat's rerun of an errored attempt", res, err)

	res, err = w.Publish(t.Context(), rerunning(second, ada))
	if err != nil || !res.Unchanged || res.Row == nil || res.Row.Attempt != 3 {
		t.Fatalf("a second rerun of attempt 2 answered %+v, %v; want attempt 3 answered unchanged", res, err)
	}
}

// A CHECK IS ONE RUNNER'S OR ONE CI'S: a person's CI reports a check the
// engine does not run onto the same rows — running as its claim, its ending
// naming the person — and its ending sent again is answered from the row;
// a report of a check the engine queued, and a rerun of one a CI reports, are
// refused native_check; a report is a person's, so a seat's is no request the
// writer decides; and a reported ending is the CI's, placed external where it
// is announced and counted, whether or not its run was reported first.
//
// Mutation: drop the native_check refusal of a report, let a seat report, or
// count a reported ending by its row's placement alone → red.
func TestAReportedCheckIsItsCIsAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	w, _, stream, rec := checksWriterOn(t, n)
	ci := checkOn(repo, 1)
	ci.Name = "lint"

	res, err := w.Publish(t.Context(), reporting(ci, code.CheckStateRunning, ""))
	requireChecked(t, "the CI's run reported", res, err)
	if res.Row.State != code.CheckStateRunning || res.Row.Placement != code.PlacementExternal {
		t.Fatalf("the run reported holds %+v, want it running, placed external", res.Row)
	}
	res, err = w.Publish(t.Context(), reporting(ci, code.CheckStateFailed, "https://ci.example.com/1"))
	requireChecked(t, "the CI's ending reported", res, err)
	if res.Row.State != code.CheckStateFailed || res.Row.Reporter != "founder" ||
		res.Row.Reason != code.CheckReasonReported {
		t.Fatalf("the ending reported holds %+v, want it failed, reported by founder", res.Row)
	}
	res, err = w.Publish(t.Context(), reporting(ci, code.CheckStateFailed, "https://ci.example.com/1"))
	if err != nil || !res.Unchanged {
		t.Fatalf("the ending sent again answered %+v, %v; want it unchanged", res, err)
	}
	res, err = w.Publish(t.Context(), rerunning(ci, ada))
	requireCheckRefused(t, "a rerun of the CI's check", res, err, code.CheckRefusalNativeCheck)

	native := checkOn(repo, 1)
	ranTo(t, w, native, code.CheckStatePassed, code.CheckReasonExit)
	res, err = w.Publish(t.Context(), reporting(native, code.CheckStateFailed, ""))
	requireCheckRefused(t, "a report of the engine's check", res, err, code.CheckRefusalNativeCheck)

	// A SEAT'S REPORT, of a run started as of one ended: the claim names no
	// reporter a record's own rules could hold to a person, so the request's
	// asker is what refuses it.
	for _, state := range []code.CheckState{code.CheckStateRunning, code.CheckStatePassed} {
		seatsCheck := checkOn(repo, 1)
		seatsCheck.Name = "seat-" + string(state)
		bySeat := reporting(seatsCheck, state, "")
		bySeat.Asker = dev
		if _, err := w.Publish(t.Context(), bySeat); !errors.Is(err, code.ErrInvalidProposal) {
			t.Fatalf("a seat's report of a run %s answered %v, want code.ErrInvalidProposal", state, err)
		}
	}

	// AN ENDING REPORTED WITH NO RUN BEFORE IT is a CI's all the same: it is
	// announced and counted placed external, as lint's is.
	e2e := checkOn(repo, 1)
	e2e.Name = "e2e"
	res, err = w.Publish(t.Context(), reporting(e2e, code.CheckStateFailed, ""))
	requireChecked(t, "the CI's ending reported alone", res, err)
	waitAnnounced(t, stream, 3)
	for _, ev := range stream.all() {
		if e, ok := ev.Data.(*types.CodeCheckFinished); ok && e.Name == "e2e" && e.Placement != "external" {
			t.Errorf("e2e's reported ending was announced placed %q, want external", e.Placement)
		}
	}
	for _, c := range []struct {
		placement string
		want      uint64
	}{{"external", 2}, {"none", 0}} {
		attrs := metrics.Attrs{"state": "failed", "placement": c.placement, "replaced": "none"}
		if got := readingOf(rec, metrics.CodeChecksFinished, attrs).Total; got != c.want {
			t.Errorf("crewlet.code.checks.finished%v = %v, want %v", attrs, got, c.want)
		}
	}
}

// A PRINCIPAL'S CHECK WRITE WEIGHS WHAT IT ASKS: a rerun or a report carries
// no record until its decide forms one, so the checks writer's node bytes
// weigh what it asks — a report's tail among it — and never the nothing its
// record is before the decide, which would let a burst of the largest reports
// past the bytes bound at a few bytes each.
//
// Mutation: weigh a principal's write by its record alone — the report weighs
// four bytes, red.
func TestAPrincipalsCheckWriteWeighsWhatItAsks(t *testing.T) {
	t.Parallel()
	key := code.CheckKey{RepoID: "019a0c2e-0000-7000-8000-000000000001", OID: oidOf("B"), Target: mainRef,
		Name: "unit", Attempt: 1}
	report := reporting(key, code.CheckStateFailed, "")
	report.Report.Tail = strings.Repeat("x", 4<<10)
	weight, err := code.ChecksWeight(report)
	if err != nil {
		t.Fatalf("weigh a report: %v", err)
	}
	if weight < len(report.Report.Tail) {
		t.Errorf("a report carrying a %d-byte tail weighs %d bytes, want at least its tail", len(report.Report.Tail),
			weight)
	}
	rerun := rerunning(key, ada)
	if weight, err = code.ChecksWeight(rerun); err != nil {
		t.Fatalf("weigh a rerun: %v", err)
	}
	if weight < len(key.Name)+len(key.OID)+len(key.Target) {
		t.Errorf("a rerun of %+v weighs %d bytes, want at least its key's", key, weight)
	}
}

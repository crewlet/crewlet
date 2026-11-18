package gitlet_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// changeRecordBytes is what each record of a change's life takes the code log at the reference
// company's text, alone in the log and measured by its growth as BenchmarkPushLogBytes measures a
// push: the figures internal/worklet's codeLogDay prices changes at (its codeChange… constants
// state them too, and its test reads these lines). Each record is formed as the test below builds
// it, at the widest shape the day prices: mergeability_refresh with a checks position beside the
// versions; check_dispatched carrying the reference company's three required checks, since a
// head's check gets a record of its own only when queued again; check_started, check_named and
// check_finished pricing a tip's and a candidate's check too, whose keys are a head's; and
// publication_tail over compact.PublishPacks (64) packs, publication_one over one. Each is held
// within changeRecordTolerance ABOVE what the log takes: a running node's broker instants print a
// few bytes wider than the harness's, and a record grown past its figure, or shrunk well below
// it, is a code log's day the docs no longer state.
var changeRecordBytes = map[string]int64{
	"open":                   3_160,
	"edit":                   2_770,
	"reviewers":              1_240,
	"revise":                 1_460,
	"mergeability":           1_030,
	"mergeability_recheck":   880,
	"mergeability_refresh":   860,
	"mergeability_ready":     1_460,
	"check_dispatched_empty": 840,
	"review_findings":        5_070,
	"review":                 2_380,
	"answer":                 1_340,
	"comment":                1_950,
	"thread_resolve":         840,
	"merge":                  2_040,
	"enqueue":                1_030,
	"candidate":              1_290,
	"candidate_check_queued": 1_010,
	"land":                   1_530,
	"close_settled":          690,
	"check_dispatched":       2_290,
	"check_started":          780,
	"check_named":            840,
	"check_finished":         840,
	"tip_check_queued":       960,
	"broken":                 1_040,
	"green":                  800,
	"publication_tail":       3_830,
	"publication_one":        1_110,
}

// changeRecordTolerance is how far above a record's measured bytes its figure may be, and never
// below: three hundredths, a dozen bytes or so on the smallest, for the instants a running node
// prints wider.
const changeRecordTolerance = 0.03

// A CHANGE'S RECORDS COST THE LOG WHAT THE CODE LOG'S DAY PRICES THEM AT: each record of a
// change's life, formed by the writer that appends it (a check's, whose runners and the home's
// dispatch form it outside this package, built here as they build it), takes the log no more
// than its changeRecordBytes figure and no less than that figure less changeRecordTolerance.
//
// Mutation: carry a change's description twice in its open record, or drop a review's findings
// from its record: red.
func TestAChangesRecordsCostTheLogWhatTheCodeLogsDayPricesThemAt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	measured := map[string]int64{}
	took := func(name string, write func()) {
		t.Helper()
		before, err := n.log.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		write()
		after, err := n.log.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if records := after.Messages - before.Messages; records != 1 {
			t.Fatalf("the %s appended %d records, want one", name, records)
		}
		measured[name] = int64(after.Bytes - before.Bytes)
		_, body, _, ok, err := n.records.At(t.Context(), n.end())
		if err != nil || !ok {
			t.Fatalf("read the %s's record: %v (held: %v)", name, err, ok)
		}
		rec, err := gitlet.Decode(body)
		if err != nil {
			t.Fatalf("decode the %s's record: %v", name, err)
		}
		// THE KEYS IT CARRIES, which internal/worklet's codeLogFields lists
		// as the ones the term was measured with.
		t.Logf("a change's %s carries %q", name, carriedKeys(rec.Mutation))
	}
	description, remark := referenceText(2<<10), referenceText(1<<10)
	// WHOM THE REFERENCE COMPANY'S SEATS WRITE FOR: the person who assigned the change's work
	// item (ADR-0109). Every record that carries a carrier (an open's, a merge's and an
	// enqueue's payload; the routing of an ask, a review and an answer) carries theirs. The
	// home's revision facts and queue landing read it back and carry it only where they wake a
	// seat, which the reference facts (asking nobody) and landing (telling only the seat that
	// queued its own change) do not; a closed settle routes nobody.
	founder := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"})
	forFounder := func(w gitlet.ConversationWrite) gitlet.ConversationWrite {
		w.WrittenFor = founder
		return w
	}

	// THE CHANGE, a seat's, opened from its own branch into a protected main
	// on a repository its unit owns.
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, mergeGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(seatRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, seatRef, mainRef, dev)
	open.WrittenFor = founder
	open.Open.Title, open.Open.Body = "Retry the upload when the store answers busy", description
	open.Open.TurnID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("the turn")).String()
	open.Open.WorkItem = &types.WorkItem{Backend: types.WorkWorklet,
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ENG-1234")).String(), Key: "ENG-1234", Project: "ENG"}
	var change string
	var version int64
	took("open", func() {
		res, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, res, err)
		change, version = res.ChangeID, res.Version
	})
	took("edit", func() {
		rewritten := "In short: " + description[:len(description)-len("In short: ")]
		res, err := c.ProposeChange(t.Context(), writing(dev, version, func(p *gitlet.ChangeProposal) {
			p.WrittenFor, p.Edit = founder, &gitlet.ChangeEdit{RepoID: repo, ChangeID: change, Body: &rewritten}
		}))
		requireChanged(t, res, err)
	})
	// A SEAT'S TURN AND MODEL, as the conversation tools stamp them on its review and comment:
	// a UUID turn id and a seventeen-byte model id, the sizes a push's run names.
	turn := uuid.NewSHA1(uuid.NameSpaceOID, []byte("the review turn")).String()
	const model = "claude-sonnet-4-6"
	took("reviewers", func() { converse(t, w, forFounder(asking(repo, change, dev, "plat-lead"))) })
	took("revise", func() {
		converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go",
			"upload/retry_test.go", "upload/store.go", "upload/store_test.go", "docs/upload.md"))
	})
	took("mergeability", func() { converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean)) })
	var findings []string
	took("review_findings", func() {
		first, second := raising(gitlet.SeverityMajor, 12, "The retry never stops"),
			raising(gitlet.SeverityMinor, 30, "Name the wait")
		first.Body, second.Body = remark, remark
		review := forFounder(reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil, first, second))
		review.Review.Summary, review.Review.TurnID, review.Review.Model = remark, turn, model
		res, err := w.Converse(t.Context(), review)
		findings = requireReviewed(t, res, err)
	})
	took("answer", func() {
		converse(t, w, forFounder(answering(repo, change, dev, gitlet.FindingResolved, findings...)))
	})
	var thread string
	took("comment", func() {
		comment := forFounder(saying(repo, change, dev, "", remark))
		comment.Comment.TurnID = turn
		thread = converse(t, w, comment).ID
	})
	took("thread_resolve", func() { converse(t, w, forFounder(resolving(repo, change, dev, thread, true))) })
	took("review", func() {
		review := forFounder(reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil))
		review.Review.Summary, review.Review.TurnID, review.Review.Model = remark, turn, model
		converse(t, w, review)
	})
	took("merge", func() {
		// A seat's code_merge_change lands as its push would, its principal
		// naming the run behind it: its turn, its work item and its model.
		merger := dev
		merger.Run = gitlet.RunProvenance{TurnID: turn, WorkBackend: "native", WorkKey: "ENG-1234", Model: model}
		p := merging(repo, change, merger, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
		p.WrittenFor, p.Merge.DeleteSource = founder, true
		res, err := c.ProposeMerge(t.Context(), p)
		requireMerged(t, res, err, oidOf("M"))
	})

	// THE SAME CHANGE INTO A TARGET WITH A QUEUE ([queueGate]), under the same title and work
	// item, so its landing deletes its source and routes as the merge did.
	qs := suiteSettings()
	qs.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	qs.Protections[0].Gate = queueGate()
	queue := n.ensure("queue", qs, t1).RepoID
	n.push(queue, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(seatRef, gitlet.ZeroOID, oidOf("B"))))
	reopened := opening(queue, seatRef, mainRef, dev)
	reopened.Open.Title, reopened.Open.WorkItem = open.Open.Title, open.Open.WorkItem
	opened, err := c.ProposeChange(t.Context(), reopened)
	requireChanged(t, opened, err)
	queued := opened.ChangeID
	ready(t, n, w, queue, queued)
	enqueued := enqueueing(queue, queued, dev, oidOf("B"))
	enqueued.WrittenFor, enqueued.Enqueue.DeleteSource = founder, true
	took("enqueue", func() {
		res, err := c.ProposeQueue(t.Context(), enqueued)
		requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	})

	// A HEAD'S CHECKS, built as their writers build them: the home's dispatch, which queues in
	// the record naming the head every check the checks file defines (the reference company's
	// three, each required by main's gate), then one claimed by its runner, its box named, and
	// its ending, passed.
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	tip, at := n.tip(repo, mainRef)
	defs := oidOf("the checks file")
	checked := func(name string, op gitlet.OpKind, payload any) {
		took(name, func() {
			if _, err := n.publishCheck(repo, op, payload); err != nil {
				t.Fatalf("publish the %s: %v", name, err)
			}
		})
	}
	var attempts []gitlet.CheckQueued
	for _, def := range []gitlet.CheckDef{{Name: "unit", Run: "go test ./...", TimeoutSeconds: 600},
		{Name: "vet", Run: "go vet ./...", TimeoutSeconds: 600},
		{Name: "build", Run: "go build ./...", TimeoutSeconds: 600}} {
		head := key
		head.Name = def.Name
		attempts = append(attempts, gitlet.CheckQueued{CheckKey: head, Purpose: gitlet.PurposeHead, ChangeID: change,
			Revision: 1, DefsOID: defs, Def: &def, Required: true, Reason: gitlet.QueueDispatch, QueuedAt: harnessAt})
	}
	checked("check_dispatched", gitlet.OpCheckDispatched, gitlet.CheckDispatched{RepoID: repo, Target: mainRef,
		Tip: tip, TipPosition: at, DefsOID: defs,
		Heads:    []gitlet.DispatchedHead{{OID: oidOf("B"), ChangeID: change, Revision: 1}},
		Attempts: attempts, DispatchedAt: harnessAt})
	started := gitlet.CheckStarted{CheckKey: key, Runner: "git-a", Placement: gitlet.PlacementContainer,
		StartedAt: harnessAt}
	checked("check_started", gitlet.OpCheckStarted, started)
	started.Box = "crewlet-check-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("the box")).String()
	checked("check_named", gitlet.OpCheckStarted, started)
	exit := 0
	checked("check_finished", gitlet.OpCheckFinished, gitlet.CheckFinished{CheckKey: key,
		State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit, ExitCode: &exit, DurationMs: 41_200,
		Summary: "exit 0 after 41s", FinishedAt: harnessAt.Add(41 * time.Second)})
	// ONE CHECK OF A LANDING'S TIP, as the home's tip work queues it: the merge's commit on
	// main, under the checks file at the base it landed on.
	checked("tip_check_queued", gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: gitlet.CheckKey{RepoID: repo,
		OID: oidOf("M"), Target: mainRef, Name: "unit", Attempt: 1}, Purpose: gitlet.PurposeTip,
		Base: oidOf("A"), DefsOID: defs, Def: &gitlet.CheckDef{Name: "unit", Run: "go test ./...",
			TimeoutSeconds: 600}, Required: true, Reason: gitlet.QueueDispatch})

	// THE QUEUED CHANGE TAKEN ON BY ITS HOME'S QUEUE WORKER: a train of one candidate of one
	// entry (a queue keeping up), merged onto main's tip as M; the required check queued on the
	// candidate as the worker queues it, then passed; and the tested prefix landed, telling the
	// change's authors it merged.
	train := training(queue, oidOf("A"), enqueued.Enqueue.EntryID, oidOf("M"))
	took("candidate", func() {
		res, err := c.ProposeQueue(t.Context(), train)
		requireRecorded(t, res, err)
	})
	candidateKey := gitlet.CheckKey{RepoID: queue, OID: oidOf("M"), Target: mainRef, Name: "unit", Attempt: 1}
	took("candidate_check_queued", func() {
		if _, err := n.publishCheck(queue, gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: candidateKey,
			Purpose: gitlet.PurposeCandidate, CandidateID: train.Candidate.Candidates[0].CandidateID,
			Base: oidOf("A"), DefsOID: defs, Def: &gitlet.CheckDef{Name: "unit", Run: "go test ./...",
				TimeoutSeconds: 600}, Required: true, Reason: gitlet.QueueDispatch}); err != nil {
			t.Fatalf("publish the candidate_check_queued: %v", err)
		}
	})
	for _, step := range []struct {
		op      gitlet.OpKind
		payload any
	}{
		{gitlet.OpCheckStarted, gitlet.CheckStarted{CheckKey: candidateKey, Runner: "git-a",
			Placement: gitlet.PlacementContainer, StartedAt: harnessAt}},
		{gitlet.OpCheckFinished, gitlet.CheckFinished{CheckKey: candidateKey, State: gitlet.CheckStatePassed,
			Reason: gitlet.CheckReasonExit, ExitCode: &exit, DurationMs: 41_200, Summary: "exit 0 after 41s",
			FinishedAt: harnessAt.Add(41 * time.Second)}},
	} {
		if _, err := n.publishCheck(queue, step.op, step.payload); err != nil {
			t.Fatalf("publish the candidate check's %s: %v", step.op, err)
		}
	}
	took("land", func() {
		res, err := c.ProposeQueue(t.Context(), landingOf(queue, train.Candidate.Train, oidOf("A"),
			gitlet.LandEntry{EntryID: enqueued.Enqueue.EntryID, ChangeID: queued, Commit: oidOf("M")}))
		requireRecorded(t, res, err)
	})
	// A LANDING'S CLOSES SETTLED: the one record its home appends once the enqueuers' closes of
	// its changes' work items landed (M5-D27), carrying no routing, since a closed item tells nobody.
	owing, _, owingEntry, _ := landedOwingClose(t, n, c, w, "owing", open.Open.WorkItem)
	took("close_settled", func() {
		res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
			p.CloseSettled = &gitlet.CloseSettled{RepoID: owing,
				Settled: []gitlet.SettledClose{{EntryID: owingEntry, Outcome: gitlet.CloseClosed}}}
		}))
		requireRecorded(t, res, err)
	})

	// A MOVED TARGET'S CHECKS FILE READ AGAIN with no head to dispatch: the
	// home's dispatch of none, which still records the tip it read.
	checked("check_dispatched_empty", gitlet.OpCheckDispatched, gitlet.CheckDispatched{RepoID: repo,
		Target: mainRef, Tip: tip, TipPosition: at, DefsOID: defs, DispatchedAt: harnessAt})

	// A CHANGE'S RE-EVALUATIONS, on a change into a gated main under the reference title and
	// work item (which the ready record's routing names): its home's merge of it (not ready, so
	// concerning nobody), a recheck of its moved target that merges clean again (kept by its tip,
	// gitlet.MergeabilityKept) and a refresh after one of its head's checks was queued, each
	// keeping the row's outcome; then the refresh that finds it ready, routed to whoever may
	// merge it.
	gated := suiteSettings()
	gated.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	gated.Protections[0].Gate = queueGate()
	kept := n.ensure("kept", gated, t1).RepoID
	n.push(kept, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	keptOpen := opening(kept, featureRef, mainRef, dev)
	keptOpen.Open.Title, keptOpen.Open.WorkItem = open.Open.Title, open.Open.WorkItem
	keptOpened, err := c.ProposeChange(t.Context(), keptOpen)
	requireChanged(t, keptOpened, err)
	keptChange := keptOpened.ChangeID
	converse(t, w, n.revising(kept, keptChange, 1, oidOf("B"), mainRef, nil, "upload/retry.go"))
	converse(t, w, n.merging(kept, keptChange, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	n.push(kept, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	took("mergeability_recheck", func() {
		converse(t, w, n.merging(kept, keptChange, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	})
	refresh := func() gitlet.ConversationWrite {
		return gitlet.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""), Mergeability: &gitlet.Mergeability{
			RepoID: kept, ChangeID: keptChange, Revision: 1, HeadOID: oidOf("B"), TargetRef: mainRef,
			Refresh: true, Node: n.id}}
	}
	unit := gitlet.CheckKey{RepoID: kept, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	if _, err := n.publishCheck(kept, gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: unit,
		Purpose: gitlet.PurposeHead, ChangeID: keptChange, Revision: 1, DefsOID: defs, Def: &gitlet.CheckDef{
			Name: "unit", Run: "go test ./...", TimeoutSeconds: 600}, Required: true,
		Reason: gitlet.QueueDispatch}); err != nil {
		t.Fatalf("queue the kept change's check: %v", err)
	}
	took("mergeability_refresh", func() { converse(t, w, refresh()) })
	converse(t, w, reviewing(kept, keptChange, platLead, 1, gitlet.VerdictApprove, nil))
	if _, err := n.publishCheck(kept, gitlet.OpCheckFinished, gitlet.CheckFinished{CheckKey: unit,
		State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit, ExitCode: &exit, DurationMs: 41_200,
		Summary: "exit 0 after 41s", FinishedAt: harnessAt.Add(41 * time.Second)}); err != nil {
		t.Fatalf("end the kept change's check: %v", err)
	}
	took("mergeability_ready", func() { converse(t, w, refresh()) })

	// THE SAME CHANGE LANDED AHEAD OF ITS CHECKS by a person's override past its target's
	// queue: its tip judged broken where its required check failed, then green once it passed.
	tipped, landing := queueRepo(t, n, c, "tip")
	ready(t, n, w, tipped, landing)
	override := merging(tipped, landing, ada, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	landed := merged.Position
	judged := func(name string, set func(*gitlet.QueueProposal)) {
		took(name, func() {
			res, err := c.ProposeQueue(t.Context(), queueRecord(set))
			requireRecorded(t, res, err)
		})
	}
	n.checked(tipped, oidOf("M"), mainRef, "unit", 1, gitlet.CheckStateFailed)
	judged("broken", func(p *gitlet.QueueProposal) {
		p.Broken = &gitlet.Broken{RepoID: tipped, TargetRef: mainRef, Position: landed, Ordinal: 1,
			OID: oidOf("M"), Checks: []string{"unit"}}
	})
	n.checked(tipped, oidOf("M"), mainRef, "unit", 2, gitlet.CheckStatePassed)
	judged("green", func(p *gitlet.QueueProposal) {
		p.Green = &gitlet.Green{RepoID: tipped, TargetRef: mainRef, OID: oidOf("M"), Through: landed}
	})

	// A REPOSITORY'S PUBLICATIONS through the committer's compact path: a busy one's tail of
	// sixty-four packs and a young one's single pack, each a layer of one part and no repack.
	for name, packs := range map[string]int{"publication_tail": 64, "publication_one": 1} {
		published, _ := packedRepo(t, n, strings.ReplaceAll(name, "_", "-"), packs)
		took(name, func() {
			res, err := c.ProposeCompaction(t.Context(), gitlet.CompactionProposal{Compaction: gitlet.Compaction{
				RepoID: published, CompactionID: statelog.NewOpID(time.Now(), ""), Node: n.id,
				Seen: n.waiter.Committed(), Layer: layerNamed(name, "", harnessAt,
					n.liveChecksums(published)...)}})
			requireApplied(t, res, err)
		})
	}

	for name, got := range measured {
		want, priced := changeRecordBytes[name]
		t.Logf("a change's %s takes the log %d bytes", name, got)
		switch {
		case !priced:
			t.Errorf("the %s is measured and changeRecordBytes states no figure for it", name)
		case want < got || float64(want) > float64(got)*(1+changeRecordTolerance):
			t.Errorf("a change's %s takes the log %d bytes, and changeRecordBytes states %d: restate it, "+
				"and the code log's day internal/worklet prices it in", name, got, want)
		}
	}
	for name := range changeRecordBytes {
		if _, ok := measured[name]; !ok {
			t.Errorf("changeRecordBytes states a figure for the %s, which this test no longer measures", name)
		}
	}
}

// carriedKeys is every key a record's payload carries, as internal/worklet's codeLogFields
// spells one: a nested key after its parent's and a dot, a list's elements' keys under the
// list's own, each once, in order.
func carriedKeys(raw json.RawMessage) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []string{"(" + err.Error() + ")"}
	}
	var keys []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(v)) {
				walk(strings.TrimPrefix(prefix+"."+k, "."), v[k])
			}
		case []any:
			for _, e := range v {
				walk(prefix, e)
			}
		default:
			if !slices.Contains(keys, prefix) {
				keys = append(keys, prefix)
			}
		}
	}
	walk("", v)
	return keys
}

// referenceText is n bytes of prose, the length of the reference company's
// description or comment.
func referenceText(n int) string {
	const sentence = "The upload retries once the store answers busy, waiting a little longer each time. "
	return strings.Repeat(sentence, n/len(sentence)+1)[:n]
}

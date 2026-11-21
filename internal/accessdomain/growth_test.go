package accessdomain_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heldStep is step of seat's held spend call held: a raise of c1's budget.
func heldStep(seat, held string, step growth.Step) accessdomain.NewGrowth {
	n := accessdomain.NewGrowth{Handle: seat, Turn: uuid.NewString(), HeldCall: held, Product: "fakeads",
		Connection: "eu", Account: "act_1", Target: "c1", Tool: "fakeads_raise_budget", Class: growth.ClassSpend,
		Step: step, Operation: fmt.Sprintf("%064x", 1), Before: fmt.Sprintf("%064x", 2),
		Changes: []accessdomain.GrowthChange{{Field: "daily_budget", Label: "Daily budget", Before: "50000000",
			After: "60000000", Money: true}},
		Money: &accessdomain.GrowthMoney{Currency: "EUR", DeltaMicros: 10_000_000, DailyMicros: 50_000_000,
			DailyAfterMicros: 60_000_000, PerChangeCapMicros: 20_000_000, DailyCapMicros: 100_000_000}}
	switch step {
	case growth.StepAllowed, growth.StepDenied:
		n.Answerer = "ops-lead"
	case growth.StepFailed:
		n.Failure = growth.FailureVendorRefused
	}
	return n
}

// madeStep is seat's call of class made at once on product's target: a pause.
func madeStep(seat, product string, class growth.Class, target string) accessdomain.NewGrowth {
	return accessdomain.NewGrowth{Handle: seat, Turn: uuid.NewString(), Call: uuid.NewString(), Product: product,
		Connection: "eu", Account: "act_1", Target: target, Tool: product + "_pause", Class: class,
		Step: growth.StepMade, Operation: fmt.Sprintf("%064x", 3),
		Changes: []accessdomain.GrowthChange{{Field: "status", Label: "Status", Before: "ACTIVE", After: "PAUSED"}}}
}

// growthCall is the engine's write of n, under the operation its act and step
// derive.
func growthCall(n accessdomain.NewGrowth) accessdomain.Call {
	act := n.HeldCall
	if act == "" {
		act = n.Call
	}
	return accessdomain.Call{By: engineCall().By, OpID: accessdomain.GrowthOpID(writerClock, n.Handle, act, n.Step)}
}

// record writes n on node, failing the test where it does not land.
func record(t *testing.T, node *writeNode, n accessdomain.NewGrowth) {
	t.Helper()
	if res, err := node.writer.Growth(t.Context(), growthCall(n), n); err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("record %s's %s: %+v, %v", n.Handle, n.Step, res, err)
	}
}

// A GROWTH RECORD KEEPS NO CREDENTIAL, NO PERSON'S DATA AND NO PUBLISHED TEXT:
// a value, a vendor id or a label that reads as a credential or holds an email
// address, a field naming a person's data or a credential, an answerer who is
// not a seat, and a published text in the clear are each refused before
// anything is published; a published text is kept as its SHA-256 and length,
// on no record of the log and no row in the clear.
//
// Mutation: drop the redact check from checkGrowthWords and the token is
// recorded; let a text change carry its words and the reply is on the log.
func TestAGrowthRecordKeepsNoCredentialPersonalDataOrPublishedText(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	const reply = "Thanks for the report on build 4417-ledger, the crash is fixed"
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	for name, change := range map[string]func(n *accessdomain.NewGrowth){
		"a token as a value": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "note", After: "ghp_" + strings.Repeat("aB3dE6", 6)}
		},
		"a web token as the target": func(n *accessdomain.NewGrowth) { n.Target = jwt },
		"an email address as a value": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "note", After: "beta tester ada@example.com"}
		},
		"an email address in a label": func(n *accessdomain.NewGrowth) {
			n.Changes[0].Label = "Budget set by ada@example.com"
		},
		"a field naming a person's email": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "contactEmail", After: "x"}
		},
		"a field naming testers": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "beta_testers", After: "3"}
		},
		"a field naming a reviewer's name": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "review.first_name", After: "Ada"}
		},
		"a field naming a credential": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "system_user.access_token", After: "x"}
		},
		"a field naming a password": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "demoAccountPassword", After: "x"}
		},
		"published text in the clear": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.PublishedText("reply", "Reply", "", reply)
			n.Changes[0].After = reply
		},
		"a value past its bound": func(n *accessdomain.NewGrowth) {
			n.Changes[0] = accessdomain.GrowthChange{Field: "name", After: strings.Repeat("a",
				accessdomain.GrowthValueMaxBytes+1)}
		},
		"an answerer who is not a seat": func(n *accessdomain.NewGrowth) {
			*n = heldStep(n.Handle, n.HeldCall, growth.StepAllowed)
			n.Answerer = "ada@example.com"
		},
	} {
		n := heldStep("growth", uuid.NewString(), growth.StepAsked)
		change(&n)
		if _, err := node.writer.Growth(t.Context(), growthCall(n), n); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
	if end, err := log.End(t.Context()); err != nil || end != 0 {
		t.Fatalf("refused records reached the log: end %d, %v", end, err)
	}
	public := heldStep("growth", uuid.NewString(), growth.StepAsked)
	public.Class, public.Tool, public.Target, public.Money = growth.ClassPublic, "fakeads_reply", "review-9", nil
	public.Changes = []accessdomain.GrowthChange{accessdomain.PublishedText("reply", "Reply", "", reply)}
	record(t, node, public)
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, _, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte("4417-ledger")) {
			t.Fatalf("record %d on the log carries the published text in the clear", seq)
		}
	}
	for table, rows := range node.dump() {
		for _, row := range rows {
			if strings.Contains(row, "4417-ledger") {
				t.Fatalf("a row of %s holds the published text in the clear: %s", table, row)
			}
		}
	}
	page, err := node.reader().GrowthChanges(t.Context(), accessdomain.GrowthQuery{}, staleRead)
	if err != nil || len(page.Rows) != 1 || len(page.Rows[0].Changes) != 1 {
		t.Fatalf("the changes answer %+v, %v", page, err)
	}
	sum := sha256.Sum256([]byte(reply))
	want := accessdomain.GrowthChange{Field: "reply", Label: "Reply", Text: true,
		AfterSHA256: hex.EncodeToString(sum[:]), AfterBytes: len(reply)}
	if got := page.Rows[0].Changes[0]; got != want {
		t.Errorf("the published text is kept as %+v, want %+v", got, want)
	}
}

// A GROWTH ACT HOLDS ONE ANSWER AND ONE OUTCOME, applied identically on every
// data node: the engine's lapse and an administrator's allow on one held call
// are one answer, the first written, from either node; an applied call is not
// also failed; a retry of one write's own operation is answered as its first
// copy; and two nodes applying the log, and a redelivery of it, hold the same
// rows.
//
// Mutation: derive a step's id from the step rather than its phase and the
// call holds a lapse and an allow; drop ON CONFLICT DO NOTHING from
// applyGrowth and the redelivery fails.
func TestAGrowthActHoldsOneAnswerAndOneOutcome(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	held := uuid.NewString()
	asked := heldStep("growth", held, growth.StepAsked)
	record(t, a, asked)
	if res, err := a.writer.Growth(t.Context(), growthCall(asked), asked); err != nil || !res.Collapsed {
		t.Errorf("a retry of the ask's own operation answers %+v, %v; want its first copy", res, err)
	}
	b.drain()
	record(t, b, heldStep("growth", held, growth.StepLapsed))
	allowed := heldStep("growth", held, growth.StepAllowed)
	if _, err := a.writer.Growth(t.Context(), growthCall(allowed), allowed); !errors.Is(err, accessdomain.ErrRecorded) {
		t.Errorf("an allow after the lapse answers %v, want ErrRecorded", err)
	}
	other := uuid.NewString()
	record(t, a, heldStep("growth", other, growth.StepAsked))
	record(t, a, heldStep("growth", other, growth.StepAllowed))
	record(t, b, heldStep("growth", other, growth.StepApplied))
	failed := heldStep("growth", other, growth.StepFailed)
	if _, err := a.writer.Growth(t.Context(), growthCall(failed), failed); !errors.Is(err, accessdomain.ErrRecorded) {
		t.Errorf("a failure after the apply answers %v, want ErrRecorded", err)
	}
	a.drain()
	b.drain()
	page, err := a.reader().GrowthChanges(t.Context(), accessdomain.GrowthQuery{}, staleRead)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, row := range page.Rows {
		steps = append(steps, row.Act()[:8]+":"+string(row.Step)+":"+string(row.AnsweredBy)+":"+row.Answerer)
	}
	want := []string{other[:8] + ":applied::", other[:8] + ":allowed:person:ops-lead", other[:8] + ":asked::",
		held[:8] + ":lapsed:engine:", held[:8] + ":asked::"}
	if !slices.Equal(steps, want) {
		t.Errorf("the acts' steps are %v, want %v", steps, want)
	}
	if !maps.EqualFunc(a.dump(), b.dump(), slices.Equal) {
		t.Fatal("the two nodes' rows differ")
	}
	a.redeliver()
	if !maps.EqualFunc(a.dump(), b.dump(), slices.Equal) {
		t.Fatal("a redelivered log moved a node's rows")
	}
}

// A GROWTH STEP IS HELD TO ITS ACT, ITS CLASS, ITS ANSWERER AND ITS FAILURE:
// made is a read or operate call made at once; every other step but a failure
// is a held public, spend or admin call's; only a failure names a closed
// failure word; an allow or deny names the administrator's seat and a lapse
// none; and the digests, the money and the change list keep their shapes.
//
// Mutation: accept made for a held call and an applied spend reads as an
// operate call nobody was asked about.
func TestAGrowthStepIsHeldToItsActClassAndFailure(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	for name, n := range map[string]accessdomain.NewGrowth{
		"made for a held call": heldStep("growth", uuid.NewString(), growth.StepMade),
		"a spend made at once": func() accessdomain.NewGrowth {
			n := madeStep("growth", "fakeads", growth.ClassSpend, "c1")
			return n
		}(),
		"an ask of an operate call": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Class = growth.ClassOperate
			return n
		}(),
		"an ask made at once": func() accessdomain.NewGrowth {
			n := heldStep("growth", "", growth.StepAsked)
			n.Call = uuid.NewString()
			return n
		}(),
		"both acts": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Call = uuid.NewString()
			return n
		}(),
		"a never class": func() accessdomain.NewGrowth {
			n := madeStep("growth", "fakeads", growth.ClassNever, "c1")
			return n
		}(),
		"an unknown step": heldStep("growth", uuid.NewString(), "approved"),
		"a failure naming none": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepFailed)
			n.Failure = ""
			return n
		}(),
		"a failure in the vendor's words": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepFailed)
			n.Failure = "budget below minimum for act_1"
			return n
		}(),
		"an apply naming a failure": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepApplied)
			n.Failure = growth.FailureTimedOut
			return n
		}(),
		"an allow naming nobody": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAllowed)
			n.Answerer = ""
			return n
		}(),
		"a lapse naming a seat": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepLapsed)
			n.Answerer = "ops-lead"
			return n
		}(),
		"no operation digest": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Operation = "abc"
			return n
		}(),
		"a currency nobody issues": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Money.Currency = "euro"
			return n
		}(),
		"money that is not micros": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Changes[0].After = "60.00"
			return n
		}(),
		"too many changes": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Changes = slices.Repeat(n.Changes, accessdomain.MaxGrowthChanges+1)
			return n
		}(),
		"an agent seat that is not one": func() accessdomain.NewGrowth {
			return heldStep("Growth Lead", uuid.NewString(), growth.StepAsked)
		}(),
		"a connection that is not an id": func() accessdomain.NewGrowth {
			n := heldStep("growth", uuid.NewString(), growth.StepAsked)
			n.Connection = "EU team"
			return n
		}(),
	} {
		if _, err := node.writer.Growth(t.Context(), growthCall(n), n); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
	for name, n := range map[string]accessdomain.NewGrowth{
		"a read made": madeStep("growth", "fakeads", growth.ClassRead, ""),
		"an operate call failing": func() accessdomain.NewGrowth {
			n := madeStep("growth", "fakeads", growth.ClassOperate, "c1")
			n.Step, n.Failure = growth.StepFailed, growth.FailureRateLimited
			return n
		}(),
		"a held call voided": heldStep("growth", uuid.NewString(), growth.StepVoid),
	} {
		if _, err := node.writer.Growth(t.Context(), growthCall(n), n); err != nil {
			t.Errorf("%s is refused: %v", name, err)
		}
	}
}

// A GROWTH RECORD IS RETAINED BY A BUILD THAT PREDATES IT: the writer stamps
// every step at the version its kind was introduced at, which a build reading
// only up to the verdicts holds back rather than faults on, while a told
// record beside it stays readable by every build.
//
// Mutation: drop the growth row from versionedFields and the step is stamped
// at 1, which a build without its applier would fault on.
func TestAGrowthRecordIsRetainedByABuildThatPredatesIt(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	if _, err := node.writer.Tell(t.Context(), engineCall(), told(t, "growth", uuid.NewString(), "operate", true)); err != nil {
		t.Fatal(err)
	}
	record(t, node, heldStep("growth", uuid.NewString(), growth.StepAsked))
	record(t, node, madeStep("growth", "fakeads", growth.ClassOperate, "c1"))
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stamped := map[accessdomain.OpKind][]int{}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, _, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		body, opened := node.verifier.Open(payload)
		if opened != recordseal.Verified {
			t.Fatalf("record %d opens %s", seq, opened)
		}
		env, err := accessdomain.Domain{}.Envelope(body)
		if err != nil {
			t.Fatal(err)
		}
		stamped[accessdomain.OpKind(env.Op)] = append(stamped[accessdomain.OpKind(env.Op)], env.V)
		if env.Kind == string(accessdomain.KindGrowth) && (env.ReadableBy(2) || !env.ReadableBy(accessdomain.RecordVersion)) {
			t.Errorf("the growth record on %s is readable by a build at 2, or not by this one", env.Subject)
		}
	}
	if want := map[accessdomain.OpKind][]int{accessdomain.OpTold: {1}, accessdomain.OpGrowth: {3, 3}}; !maps.EqualFunc(stamped, want, slices.Equal) {
		t.Errorf("the records are stamped %v, want %v", stamped, want)
	}
}

// THE RECENT AGENT CHANGES ARE EVERY STEP BUT A READ'S, newest first and
// paged: an operate call made or failed, and each step of a held call, for one
// product or all; a read, made or failed, is on the log and on no page.
//
// Mutation: drop `class != 'read'` from GrowthChanges and the page lists every
// campaign the agent read.
func TestTheRecentAgentChangesAreEveryStepButARead(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	held := uuid.NewString()
	readFailed := madeStep("growth", "fakeads", growth.ClassRead, "")
	readFailed.Step, readFailed.Failure = growth.StepFailed, growth.FailureUnauthorized
	operateFailed := madeStep("growth", "fakeads", growth.ClassOperate, "c2")
	operateFailed.Step, operateFailed.Failure = growth.StepFailed, growth.FailureVendorRefused
	for _, n := range []accessdomain.NewGrowth{
		madeStep("growth", "fakeads", growth.ClassRead, ""),
		madeStep("growth", "fakeads", growth.ClassOperate, "c1"),
		readFailed,
		operateFailed,
		heldStep("growth", held, growth.StepAsked),
		madeStep("growth", "otherads", growth.ClassOperate, "x9"),
		heldStep("growth", held, growth.StepAllowed),
		heldStep("growth", held, growth.StepApplied),
	} {
		record(t, node, n)
	}
	label := func(rows []accessdomain.GrowthRow) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.Product+":"+string(row.Class)+":"+string(row.Step))
		}
		return out
	}
	r := node.reader()
	for name, tc := range map[string]struct {
		q    accessdomain.GrowthQuery
		want []string
	}{
		"every product": {accessdomain.GrowthQuery{}, []string{"fakeads:spend:applied", "fakeads:spend:allowed",
			"otherads:operate:made", "fakeads:spend:asked", "fakeads:operate:failed", "fakeads:operate:made"}},
		"one product": {accessdomain.GrowthQuery{Product: "otherads"}, []string{"otherads:operate:made"}},
	} {
		page, err := r.GrowthChanges(t.Context(), tc.q, staleRead)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := label(page.Rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s answers %v, want %v", name, got, tc.want)
		}
	}
	var paged []string
	q := accessdomain.GrowthQuery{Product: "fakeads", Limit: 2}
	for range 4 {
		page, err := r.GrowthChanges(t.Context(), q, staleRead)
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, label(page.Rows)...)
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if want := []string{"fakeads:spend:applied", "fakeads:spend:allowed", "fakeads:spend:asked",
		"fakeads:operate:failed", "fakeads:operate:made"}; !slices.Equal(paged, want) {
		t.Errorf("pages of two answer %v, want %v", paged, want)
	}
	for name, q := range map[string]accessdomain.GrowthQuery{
		"a product that is no kind": {Product: "Fake Ads"},
		"a page too long":           {Limit: accessdomain.MaxPageSize + 1},
		"a cursor no page answered": {Cursor: "x"},
	} {
		if _, err := r.GrowthChanges(t.Context(), q, staleRead); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
}

// A VENDOR'S RATE RULE COUNTS THE CHANGES THAT TOOK EFFECT ON ONE TARGET: an
// allowed call applied and an operate call made, by every seat or by one, since
// an instant; an ask, an allow, a failure, a read and another target's change
// are not counted.
//
// Mutation: count every step on the target and the ask and the allow of one
// raise count three changes, so Meta's fourth budget change an hour is refused
// at the second.
func TestAVendorsRateRuleCountsTheChangesThatTookEffect(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	held := uuid.NewString()
	failed := madeStep("growth", "fakeads", growth.ClassOperate, "c1")
	failed.Step, failed.Failure = growth.StepFailed, growth.FailureRateLimited
	for _, n := range []accessdomain.NewGrowth{
		heldStep("growth", held, growth.StepAsked),
		heldStep("growth", held, growth.StepAllowed),
		heldStep("growth", held, growth.StepApplied),
		madeStep("growth", "fakeads", growth.ClassOperate, "c1"),
		madeStep("growth", "fakeads", growth.ClassRead, "c1"),
		madeStep("growth", "fakeads", growth.ClassOperate, "c2"),
		madeStep("ads-ops", "fakeads", growth.ClassOperate, "c1"),
		failed,
	} {
		record(t, node, n)
	}
	r := node.reader()
	// THE BROKER'S INSTANT is what a count reads, never the writer's clock.
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		q    accessdomain.GrowthCount
		want int
	}{
		"every seat":      {accessdomain.GrowthCount{Product: "fakeads", Account: "act_1", Target: "c1", Since: since}, 3},
		"one seat":        {accessdomain.GrowthCount{Product: "fakeads", Account: "act_1", Target: "c1", Handle: "growth", Since: since}, 2},
		"another seat":    {accessdomain.GrowthCount{Product: "fakeads", Account: "act_1", Target: "c1", Handle: "ads-ops", Since: since}, 1},
		"another account": {accessdomain.GrowthCount{Product: "fakeads", Account: "act_2", Target: "c1", Since: since}, 0},
		"since later":     {accessdomain.GrowthCount{Product: "fakeads", Account: "act_1", Target: "c1", Since: time.Now().Add(time.Hour)}, 0},
	} {
		got, err := r.GrowthChanged(t.Context(), tc.q, staleRead)
		if err != nil || got != tc.want {
			t.Errorf("%s counts %d (%v), want %d", name, got, err, tc.want)
		}
	}
	for name, q := range map[string]accessdomain.GrowthCount{
		"no target":  {Product: "fakeads", Account: "act_1", Since: since},
		"no instant": {Product: "fakeads", Account: "act_1", Target: "c1"},
		"no product": {Account: "act_1", Target: "c1", Since: since},
		"a bad seat": {Product: "fakeads", Account: "act_1", Target: "c1", Handle: "Ops Lead", Since: since},
	} {
		if _, err := r.GrowthChanged(t.Context(), q, staleRead); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
}

// A GROWTH RECORD OUTLIVES ITS SEAT'S REMOVAL AND GOES WITH THE RETENTION, as
// a verdict does: forgetting a removed seat leaves its growth records, the hour
// of one past the cutoff is what the duty nominates, and an expiry through the
// cutoff takes it and keeps what is newer.
//
// Mutation: drop access_growth from expiryStatements and the record outlives
// the company's retention.
func TestAGrowthRecordOutlivesItsSeatAndGoesWithTheRetention(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	record(t, node, heldStep("growth", uuid.NewString(), growth.StepAsked))
	if _, err := node.writer.Issued(t.Context(), engineCall(), certificate("ops", uuid.NewString(), 4)); err != nil {
		t.Fatal(err)
	}
	cutoff := certificateAt(t, node, 4)
	if _, err := node.writer.Forget(t.Context(), engineCall(), "growth"); err != nil {
		t.Fatal(err)
	}
	if got := node.count(`SELECT COUNT(*) FROM access_growth WHERE agent_handle = 'growth'`); got != 1 {
		t.Fatalf("the seat's removal left %d growth records, want its 1", got)
	}
	hours, err := node.reader().ExpirableHours(t.Context(), cutoff, 10)
	if err != nil || len(hours) != 1 {
		t.Fatalf("the duty nominates %v, %v; want the record's hour", hours, err)
	}
	if _, err := node.writer.Expire(t.Context(), engineCall(), hours[0], cutoff); err != nil {
		t.Fatalf("expire %s: %v", hours[0], err)
	}
	for statement, want := range map[string]int{
		`SELECT COUNT(*) FROM access_growth`:       0,
		`SELECT COUNT(*) FROM access_certificates`: 1,
	} {
		if got := node.count(statement); got != want {
			t.Errorf("%s = %d after the retention, want %d", statement, got, want)
		}
	}
}

package engine

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// fleetReconciler is a reconciler for e over e's own backends: the one whose
// applied epoch every irreversible judgement against the active revision reads
// ([Engine.servedFleetRevision]).
func fleetReconciler(t *testing.T, e *Engine) *Reconciler {
	t.Helper()
	r, err := e.NewReconciler(ReconcilerOptions{Store: e.backends.Store, Fleet: e.backends.Fleet,
		Queue: e.backends.Queue, NodeID: "node-purge"})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r
}

// activateRevision stores doc as a revision and points the fleet at it, as a
// configuration write does, and returns its epoch. Nothing applies it: that is
// a reconciler's tick.
func activateRevision(t *testing.T, e *Engine, doc string) int64 {
	t.Helper()
	return activateRevisionAt(t, e, doc).Epoch
}

// activateRevisionAt is [activateRevision] answering the whole activation: its
// epoch, and the instant a node booted on its revision is stamped with
// ([Options.ActivatedAt]), which is how that node knows it serves the
// pointer's revision ([Engine.servedFleetRevision]).
//
// SEALED under e's own keyring in both places, as every writer stores and
// publishes it: a node holding a keyring applies only a sealed document, since
// a plaintext one is one anything reaching the store or the coordination
// bucket could have authored.
func activateRevisionAt(t *testing.T, e *Engine, doc string) coord.Activation {
	t.Helper()
	document, err := json.Marshal(parseCompany(t, doc))
	if err != nil {
		t.Fatalf("encode the revision: %v", err)
	}
	payload, err := secrets.Seal(e.Cipher(), document)
	if err != nil {
		t.Fatalf("seal the revision: %v", err)
	}
	now := time.Now().UTC()
	id, err := e.backends.Store.Configs().InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator, Source: "test", CreatedBy: "operator",
		Summary: "revision", Payload: payload, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	activation, err := e.backends.Fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "revision", Payload: payload, At: now,
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
			Source: "api", CreatedAt: now},
	})
	if err != nil {
		t.Fatalf("activate the revision: %v", err)
	}
	return activation
}

// tickApplies is one reconcile tick that must leave e serving epoch, its
// reconcile of code.repos finished.
func tickApplies(t *testing.T, e *Engine, r *Reconciler, epoch int64) {
	t.Helper()
	if err := r.Tick(t.Context()); err != nil {
		t.Fatalf("reconcile tick: %v", err)
	}
	if got := r.Applied(); got != epoch {
		t.Fatalf("after its tick the node serves activation epoch %d, want %d", got, epoch)
	}
	reconciled(t, e)
}

// retainedFor is codeReposDoc hosting its repositories with a deleted one kept
// restorable for days.
func retainedFor(days int, repos string) string {
	return strings.Replace(codeReposDoc(repos, ""), "code:\n  native: true\n",
		fmt.Sprintf("code:\n  native: true\n  deleted_retention_days: %d\n", days), 1)
}

// purgeProbe is the code-retention duty as e builds it, watched: its claims
// counted, its give-backs counted and answered, its decline lines and its
// passes' lines captured at every level, and its pass run as though a day less a nanosecond had passed since every deletion — the retention the duty reads
// off the epoch, less that, is what both the listing and every purge step
// judge a deletion's age by, so a day's retention is past at once and a longer
// one is not. The engine's own loop is stopped, so every turn is the test's.
type purgeProbe struct {
	duty              *codeRetentionDuty
	claims, gaveBacks atomic.Int64
	mu                sync.Mutex
	logs              bytes.Buffer
}

func newPurgeProbe(e *Engine) *purgeProbe {
	e.stopCodeRetention()
	p := &purgeProbe{duty: e.newCodeRetentionDuty(e.writeSide().code, e.native.Load().codeReader, e.core.Load().nodeID)}
	claim := p.duty.claim
	p.duty.claim = func(ctx context.Context) (bool, error) {
		p.claims.Add(1)
		if claim == nil {
			return true, nil
		}
		return claim(ctx)
	}
	p.duty.giveBack = func(context.Context) (bool, error) {
		p.gaveBacks.Add(1)
		return true, nil
	}
	p.duty.declines.log = slog.New(slog.NewJSONHandler(&lockedWriter{mu: &p.mu, w: &p.logs},
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	p.duty.log = p.duty.declines.log
	epochs := p.duty.pass.Retention
	p.duty.pass.Retention = func() time.Duration { return epochs() - (24*time.Hour - time.Nanosecond) }
	return p
}

// declined is one turn that must decline, and asks again after next.
func (p *purgeProbe) declined(t *testing.T, next time.Duration, why string) {
	t.Helper()
	claims := p.claims.Load()
	if got := p.duty.tick(t.Context()); got != next {
		t.Errorf("%s: the duty asks again after %v, want %v", why, got, next)
	}
	if n := p.claims.Load() - claims; n != 0 {
		t.Errorf("%s: the turn claimed the duty %d time(s), want none: the decline comes first", why, n)
	}
}

// declineLine is one of the duty's decline lines.
type declineLine struct {
	Msg, Level, Reason, Error string
	Applied                   int64 `json:"applied_epoch"`
	Fleet                     int64 `json:"fleet_epoch"`
}

// lines is every `_declined` and `_resumed` line the duty logged, in order.
func (p *purgeProbe) lines(t *testing.T) []declineLine {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []declineLine
	scanner := bufio.NewScanner(bytes.NewReader(p.logs.Bytes()))
	for scanner.Scan() {
		var line declineLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a decline line is no JSON: %v: %s", err, scanner.Bytes())
		}
		if line.Msg == "code_retention_duty_declined" || line.Msg == "code_retention_duty_resumed" {
			out = append(out, line)
		}
	}
	return out
}

// passLine is one of the duty's `code_retention_pass` lines, with the counts
// a sweep adds to it.
type passLine struct {
	Msg, Level string
	Sweeps     int `json:"sweeps"`
	SweptRows  int `json:"swept_rows"`
}

// passes is every `code_retention_pass` line the duty logged, in order.
func (p *purgeProbe) passes(t *testing.T) []passLine {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []passLine
	scanner := bufio.NewScanner(bytes.NewReader(p.logs.Bytes()))
	for scanner.Scan() {
		var line passLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a duty line is no JSON: %v: %s", err, scanner.Bytes())
		}
		if line.Msg == "code_retention_pass" {
			out = append(out, line)
		}
	}
	return out
}

// noWarning holds that the duty logged nothing at WARN or above.
func (p *purgeProbe) noWarning(t *testing.T, why string) {
	t.Helper()
	for _, line := range p.lines(t) {
		if line.Level == "WARN" || line.Level == "ERROR" {
			t.Fatalf("%s: the duty logged %+v — a fault's line for what every node meets", why, line)
		}
	}
}

// noPurge holds that the turns since the code log ended at end wrote nothing,
// and that web is still merely deleted — restorable.
func noPurge(t *testing.T, e *Engine, end uint64, why string) {
	t.Helper()
	if writes := codeLogWrites(t, e, end); len(writes) != 0 {
		t.Fatalf("%s: the code-retention duty appended %v to the code log, want nothing", why, writes)
	}
	if web := mustRepo(t, e, "web"); web.Status != code.RepoStatusDeleted {
		t.Fatalf("%s: web is %s, want it still deleted and restorable", why, web.Status)
	}
}

// A CODE-RETENTION HOLDER BEHIND THE FLEET'S REVISION STARTS NO PURGE.
//
// The duty purges by the code.deleted_retention_days of the revision its node
// serves, and a purge has no undo. A company that lengthens the retention to
// save a deleted repository activates a revision that says so; a holder that
// has not applied it — propagation, a refused apply, a node stuck on an older
// revision — would purge the repository by the retention the company replaced.
// So the duty is taken only on a node serving the revision the fleet's
// activation pointer names, the rule the mailbox retirement already followed:
// behind, the turn declines BEFORE its claim, gives the duty back, and purges
// nothing; once the node applies the revision, the longer retention holds the
// repository. And NOTHING ACTIVATED IS NOT CURRENT for this rule: a node booted
// on a company file purges nothing before the fleet has activated a revision.
//
// WHAT IT SAYS depends on whether being behind is a fault. Applying the
// revision, which every node does at every activation, is DEBUG and asked again
// at the reconcile cadence; a lag the posture's rule confirms is a WARN naming
// both epochs, asked again a tick later; and the decline it ends on is closed
// at INFO, the level of the loudest line the episode logged.
//
// Mutations: drop the decline and the behind node purges web by the day the
// company replaced; answer the company on an unset pointer
// ([Engine.servedFleetRevision]) and the unactivated node purges it; read every
// lag as confirmed and the activation still being applied warns; read none as
// confirmed and the lag the reconciler confirmed never warns; drop the episode's
// loudest level and the WARN is closed at DEBUG.
func TestACodeRetentionHolderBehindTheFleetsRevisionStartsNoPurge(t *testing.T) {
	t.Parallel()
	t.Run("behind the fleet's revision", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, retainedFor(1, webRepo(""))))
		servesCodeReads(t, e)
		reconciled(t, e)
		r := fleetReconciler(t, e)
		// THE FLEET REMOVES web WITH A DAY'S RETENTION, and this node
		// applies it: web is deleted, and a day is past at once.
		removed := activateRevision(t, e, retainedFor(1, ""))
		tickApplies(t, e, r, removed)
		if web := mustRepo(t, e, "web"); web.Status != code.RepoStatusDeleted {
			t.Fatalf("the premise: the revision removing web leaves it %s", web.Status)
		}
		// THEN LENGTHENS IT TO A MONTH, to save web — and this node has
		// not applied that yet.
		saved := activateRevision(t, e, retainedFor(30, ""))
		if got := e.codeDeletedRetention(); got != 24*time.Hour {
			t.Fatalf("the premise: the node still purges after %v, want the day it serves", got)
		}

		p := newPurgeProbe(e)
		end := codeLogEnd(t, e)
		// STILL PROPAGATING: this node's tick has not met the activation
		// yet, which every node is for a moment at every activation.
		p.declined(t, configplane.ReconcileInterval, "a turn meeting an activation not yet applied")
		p.noWarning(t, "a turn meeting an activation not yet applied")
		// CONFIRMED: an attempt of this node's at the revision concluded in
		// failure — what a refused apply leaves.
		r.publish(applyProgress{applied: removed, target: saved, attempts: 1})
		for range 2 {
			p.declined(t, code.RetentionInterval, "a turn on a node that failed the fleet's revision")
		}
		noPurge(t, e, end, "a holder behind the fleet's revision")
		if n := p.gaveBacks.Load(); n != 3 {
			t.Errorf("the three declined turns gave the duty back %d time(s), want each — left to "+
				"lapse, the lease keeps it from a node that serves the fleet's revision", n)
		}
		lines := p.lines(t)
		if len(lines) != 2 || lines[0].Level != "DEBUG" || lines[0].Reason != "propagating" ||
			lines[0].Fleet != saved ||
			lines[1].Msg != "code_retention_duty_declined" || lines[1].Level != "WARN" ||
			lines[1].Reason != "behind" || lines[1].Applied != removed || lines[1].Fleet != saved {
			t.Fatalf("the declined turns logged %+v, want one DEBUG line while the activation "+
				"propagated, then one WARN code_retention_duty_declined naming epochs %d and %d "+
				"once the apply failed", lines, removed, saved)
		}
		// A LAG THE GRACE CONFIRMS is the same fault, and the same reason —
		// no second line.
		r.publish(applyProgress{applied: removed, target: saved,
			behindSince: r.now().Add(-configplane.LagGrace)})
		p.declined(t, code.RetentionInterval, "a turn on a node behind for longer than propagation")
		if lines := p.lines(t); len(lines) != 2 {
			t.Fatalf("a lag confirmed by the grace rather than a failure logged %+v, want it "+
				"read as the same reason", lines[2:])
		}
		// A PASS ALREADY RUNNING ASKS THE SAME before every renewal, and is
		// ended by it.
		if p.duty.renewal.declined == nil || !p.duty.renewal.declined(t.Context()) {
			t.Fatal("the renewal of a pass in flight does not decline on a node behind the " +
				"fleet's revision: a pass begun before an activation this node failed would " +
				"purge on by the retention the company replaced")
		}

		// THE FLEET'S NEXT REVISION, still a month, is one this node has not
		// met yet: propagation again, logged at DEBUG.
		fixed := activateRevision(t, e, retainedFor(30, ""))
		p.declined(t, configplane.ReconcileInterval, "a turn meeting the fleet's next revision")
		// ONCE THE NODE APPLIES IT, the month holds web — and the episode,
		// which warned, is closed where its WARN was read.
		tickApplies(t, e, r, fixed)
		end = codeLogEnd(t, e)
		report, ran := p.duty.turn(t.Context())
		if !ran || len(report.Purged) != 0 {
			t.Fatalf("the turn on a node serving the fleet's revision ran %v and purged %v, "+
				"want a pass that purges nothing", ran, report.Purged)
		}
		noPurge(t, e, end, "a holder serving the revision that keeps web a month")
		if p.duty.renewal.declined(t.Context()) {
			t.Error("the renewal of a pass declines on a node serving the fleet's revision")
		}
		lines = p.lines(t)
		if last := lines[len(lines)-1]; len(lines) != 4 || lines[2].Level != "DEBUG" ||
			lines[2].Reason != "propagating" || last.Msg != "code_retention_duty_resumed" ||
			last.Level != "INFO" {
			t.Fatalf("a decline that warned and ended on propagation logged %+v, want its "+
				"code_retention_duty_resumed at INFO, where its WARN was read", lines)
		}
	})
	t.Run("nothing activated", func(t *testing.T) {
		t.Parallel()
		// A COMPANY BOOTED FROM A FILE, which no activation has put on the
		// pointer yet — its reconciler has ticked and found nothing to apply.
		e := newSandboxNode(t, parseCompany(t, retainedFor(1, webRepo(""))))
		servesCodeReads(t, e)
		reconciled(t, e)
		if err := fleetReconciler(t, e).Tick(t.Context()); err != nil {
			t.Fatalf("reconcile tick: %v", err)
		}
		applyReconciled(t, e, retainedFor(1, ""))
		if web := mustRepo(t, e, "web"); web.Status != code.RepoStatusDeleted {
			t.Fatalf("the premise: the company without web leaves it %s", web.Status)
		}

		p := newPurgeProbe(e)
		end := codeLogEnd(t, e)
		// ASKED AGAIN at the reconcile cadence: the seed's first activation
		// is a tick away at most.
		p.declined(t, configplane.ReconcileInterval, "a turn on a node whose fleet has activated nothing")
		noPurge(t, e, end, "a holder whose fleet has activated nothing")
		if lines := p.lines(t); len(lines) != 1 || lines[0].Level != "DEBUG" ||
			lines[0].Reason != "not_activated" {
			t.Fatalf("the declined turn logged %+v, want one DEBUG code_retention_duty_declined "+
				"saying nothing is activated — a fresh deployment meets it at every boot", lines)
		}
	})
}

// A NODE STILL REACHING THE FLEET'S REVISION DECLINES THE PURGE QUIETLY, AND
// ASKS AGAIN SOON — AND A NODE BOOTED ON THE FLEET'S REVISION PURGES AT ONCE.
//
// The duty's loop is armed inside [New] and its first turn runs at once —
// before `crewlet run` has built the reconciler, which it does only once New
// has returned, and long before that reconciler's first tick. A node booted on
// the revision the pointer names serves it already: the company it boots on
// carries that activation's instant, and that is what says a node serves the
// fleet's revision ([Engine.servedFleetRevision]), reconciler or none. A node
// booted on a revision the fleet has since moved past — another node activated
// a newer one while this one was down — does not, and neither does an apply
// that builds the native runtime, which arms the duty inside its own tick. None
// of that is a fault: the turn declines at DEBUG, gives nothing to warn about,
// and asks again at the reconcile cadence, so the restart costs the purge a
// reconcile interval rather than a tick. A node with no reconciler yet reads as
// one whose reconciler has not ticked, which the posture reads as waiting too.
//
// Mutations: log the boot's decline as unknown (the base) and the restart
// behind the fleet warns; drop [dutyDecline.again] and it waits a whole tick;
// judge a node with no reconciler behind whatever company it holds, and the
// restart on the fleet's own revision declines.
func TestANodeStillReachingTheFleetsRevisionDeclinesThePurgeQuietly(t *testing.T) {
	t.Parallel()
	removed := retainedFor(1, "")
	t.Run("restarted on the fleet's own revision", func(t *testing.T) {
		t.Parallel()
		b := codeNodeStore(t)
		applied := removedOnce(t, &b, removed, func(*Engine) {})

		// THE RESTART, in `crewlet run`'s order: the engine on the store's
		// revision, stamped with its activation — the one the pointer names
		// — and no reconciler.
		e, _ := bootCodeNode(t, &b, parseCompany(t, removed), applied.At)
		p := newPurgeProbe(e)
		report, ran := p.duty.turn(t.Context())
		if !ran || !slices.Equal(report.Purged, []string{"web"}) {
			t.Fatalf("a node restarted on the fleet's revision, with no reconciler yet, ran %v and "+
				"purged %v, want web purged: it serves the revision the pointer names", ran, report.Purged)
		}
		if got := p.duty.wait(); got != code.RetentionInterval {
			t.Fatalf("a turn that ran its pass asks again after %v, want the duty's tick", got)
		}
		if lines := p.lines(t); len(lines) != 0 {
			t.Fatalf("a node serving the fleet's revision logged %+v, want no decline", lines)
		}
	})
	t.Run("restarted behind the fleet's revision", func(t *testing.T) {
		t.Parallel()
		b := codeNodeStore(t)
		// WHILE THE NODE IS DOWN, the fleet activates the revision again —
		// the rotation gesture: an activation of its own, and one this node's
		// store never applied.
		var next coord.Activation
		applied := removedOnce(t, &b, removed, func(first *Engine) {
			next = activateRevisionAt(t, first, removed)
		})

		e, _ := bootCodeNode(t, &b, parseCompany(t, removed), applied.At)
		d := e.codeRetention
		if d == nil {
			t.Fatal("the premise: the restarted node armed no code-retention duty")
		}
		var reason declineReason
		waitUntil(t, 20*time.Second, "the duty's first turn to decline", func() bool {
			d.declines.mu.Lock()
			defer d.declines.mu.Unlock()
			reason = d.declines.out
			return reason.key != ""
		})
		if reason.key != "propagating" || reason.level != slog.LevelDebug {
			t.Fatalf("the restarted node's first turn declined for %q at %v, want propagating at "+
				"DEBUG: every restart onto a revision the fleet moved past would log the warning "+
				"that names a node that failed an apply", reason.key, reason.level)
		}
		if got := d.wait(); got != configplane.ReconcileInterval {
			t.Fatalf("the restarted node asks again after %v, want the reconcile cadence: "+
				"a restart would cost its purge a whole tick", got)
		}
		// THE REST OF THE DUTY'S TURNS ARE THE TEST'S, on the duty the boot
		// armed, rather than a race with the loop's own.
		e.stopCodeRetention()
		// THE RECONCILER `crewlet run` BUILDS, and its first tick.
		tickApplies(t, e, fleetReconciler(t, e), next.Epoch)
		if _, ran := d.turn(t.Context()); !ran {
			t.Fatal("the turn after the reconciler's first tick ran no pass on a node serving " +
				"the fleet's revision")
		}
		if got := d.wait(); got != code.RetentionInterval {
			t.Fatalf("a turn that ran its pass asks again after %v, want the duty's tick", got)
		}
	})
	t.Run("before and through its reconciler's first tick", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, retainedFor(1, webRepo(""))))
		servesCodeReads(t, e)
		reconciled(t, e)
		// A NODE BEHIND THE POINTER: it serves the revision removing web from
		// an activation the fleet has since moved past, with no reconciler
		// to apply the newer one yet.
		if status, applied, err := e.Apply(t.Context(), parseCompany(t, removed),
			time.Now().Add(-time.Hour)); err != nil || status != configplane.StatusOK {
			t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
		}
		reconciled(t, e)
		epoch := activateRevision(t, e, removed)

		p := newPurgeProbe(e)
		end := codeLogEnd(t, e)
		p.declined(t, configplane.ReconcileInterval, "a turn before the reconciler is built")
		// BUILT, and not yet ticked: it has read nothing.
		r := fleetReconciler(t, e)
		p.declined(t, configplane.ReconcileInterval, "a turn before the reconciler's first tick")
		noPurge(t, e, end, "a node that has not reconciled")
		// ITS FIRST TICK: the next turn purges web, a day past.
		tickApplies(t, e, r, epoch)
		report, ran := p.duty.turn(t.Context())
		if !ran || !slices.Equal(report.Purged, []string{"web"}) {
			t.Fatalf("the turn after the reconciler's first tick ran %v and purged %v, want web "+
				"purged", ran, report.Purged)
		}
		p.noWarning(t, "a node reconciling at boot")
		if lines := p.lines(t); len(lines) != 2 || lines[0].Reason != "propagating" ||
			lines[1].Msg != "code_retention_duty_resumed" || lines[1].Level != "DEBUG" {
			t.Fatalf("a node reconciling at boot logged %+v, want one DEBUG decline and one "+
				"DEBUG resume", lines)
		}
	})
}

// A RETIREMENT IS SWEPT BY A RECORD THE ENGINE RUNS. A retired reference keeps
// its object from the collector only until its domain deletes the row
// (ADR-0051), and the code host's retired packs are deleted by a retire_sweep
// record of the code-retention duty — every applier at one position — never
// by a job of the maintenance worker, whose list
// (TestTheEngineSweepsEveryShortHorizonTable) holds per-node deletes alone and
// could not name this one. So the duty the engine builds is held here to it: a
// pass on a node serving the fleet's revision removes a repository's retired
// row past code.RetireGrace by one record on the repository's subject, keeps a
// row still inside the grace, and says what it swept at INFO.
//
// THE ROWS ARE SEEDED into the node's estate, a day and an hour old and an
// hour old by the wall clock, because the duty the engine builds judges the
// grace by its own clock, which the case does not set, and the broker's
// instants cannot be moved.
//
// Mutation: drop the sweep step from the pass, and the row stays with nothing
// on the log; drop the sweep's counts from the pass's INFO rule, and the line
// is DEBUG.
func TestARetirementIsSweptByARecordTheEngineRuns(t *testing.T) {
	t.Parallel()
	doc := retainedFor(7, webRepo(""))
	e := newSandboxNode(t, parseCompany(t, doc))
	servesCodeReads(t, e)
	reconciled(t, e)
	// SERVING THE FLEET'S REVISION, which a node must to hold the duty at all.
	epoch := activateRevision(t, e, doc)
	tickApplies(t, e, fleetReconciler(t, e), epoch)
	web := mustRepo(t, e, "web").ID
	now := time.Now()
	seedRetiredPack(t, e, web, 1, now.Add(-code.RetireGrace-time.Hour))
	seedRetiredPack(t, e, web, 2, now.Add(-time.Hour))

	p := newPurgeProbe(e)
	end := codeLogEnd(t, e)
	report, ran := p.duty.turn(t.Context())
	if !ran || report.Sweeps != 1 || report.SweptRows != 1 || len(report.Failed) != 0 {
		t.Fatalf("the duty's turn ran %v and reported %+v; want a pass that swept the one row past "+
			"its grace", ran, report)
	}
	if writes := codeLogWrites(t, e, end); !slices.Equal(writes, []code.Subject{code.RepoSubject(web)}) {
		t.Fatalf("the pass appended %v to the code log, want one record on web's subject", writes)
	}
	var due, inside int
	if err := e.backends.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT
			(SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?1 AND retired_position = 1),
			(SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?1 AND retired_position = 2)`,
			web).Scan(&due, &inside)
	}); err != nil {
		t.Fatalf("read web's retired packs: %v", err)
	}
	if due != 0 || inside != 1 {
		t.Fatalf("after the pass web holds %d retired row(s) past the grace and %d inside it, "+
			"want 0 and 1", due, inside)
	}
	if lines := p.passes(t); len(lines) != 1 || lines[0].Level != "INFO" || lines[0].Sweeps != 1 ||
		lines[0].SweptRows != 1 {
		t.Fatalf("the pass logged %+v, want one INFO code_retention_pass with sweeps 1 and "+
			"swept_rows 1", lines)
	}
}

// seedRetiredPack writes one retired pack row of repository id into e's
// estate, retired by the record at the packed position and stamped at — the
// row a compaction's apply writes, which this case needs at an instant the
// broker never stamps.
func seedRetiredPack(t *testing.T, e *Engine, id string, position int64, at time.Time) {
	t.Helper()
	pack := fmt.Sprintf("%s-retired-%d", id, position)
	key := objstore.KeyAt(at)
	if err := e.backends.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO code_packs_retired (repo_id, pack_id, part,
			object, hash, size, index_bytes, reverse_bytes, git_checksum, objects, kind, added_position,
			retired_position, retired_at) VALUES (?, ?, 'pack', ?, ?, 100, 0, 0, ?, 1, 'push', 1, ?, ?)`,
			id, pack, key.String(), string(objstore.HashOf([]byte(pack))), strings.Repeat("0", 40),
			position, store.EncodeTime(at))
		return err
	}); err != nil {
		t.Fatalf("seed %s's retired pack at %d: %v", id, position, err)
	}
}

// codeNodeStore is a node's Tier A over a store and a stream of its own,
// seeded, which a restart opens again.
func codeNodeStore(t *testing.T) config.Bootstrap {
	t.Helper()
	b := testBootstrap(t)
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	b.Stream.StoreDir = filepath.Join(dir, "stream")
	PinGitLanes(&b)
	SeedStore(t, &b)
	return b
}

// removedOnce is a node's first boot on Tier A b — web declared, then removed
// by the activation of doc, which the node applies — and that activation. then
// runs on the node before it stops.
func removedOnce(t *testing.T, b *config.Bootstrap, doc string, then func(*Engine)) coord.Activation {
	t.Helper()
	first, back := bootCodeNode(t, b, parseCompany(t, retainedFor(1, webRepo(""))),
		time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC))
	servesCodeReads(t, first)
	reconciled(t, first)
	applied := activateRevisionAt(t, first, doc)
	tickApplies(t, first, fleetReconciler(t, first), applied.Epoch)
	then(first)
	first.Stop(context.Background())
	back.Close(context.Background())
	return applied
}

// bootCodeNode boots a node on Tier A b over backends of its own, as
// `crewlet run` does up to New's return: the engine on company, stamped with
// the instant of the activation it was applied from, with no reconciler.
func bootCodeNode(t *testing.T, b *config.Bootstrap, company *config.Company,
	activatedAt time.Time) (*Engine, *Backends) {
	t.Helper()
	RequireNodeGit(t, b, company)
	back, err := OpenBackends(t.Context(), b, company, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: b, Company: company, Backends: back,
		ActivatedAt: activatedAt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e, back
}

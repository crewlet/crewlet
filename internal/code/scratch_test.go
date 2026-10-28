package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE-RETENTION PASS'S TWO LIFECYCLE STEPS — the release of a renamed
// repository's old name once its redirect has ended, and the expiry of a seat's
// scratch repository idle past code.scratch.idle_days — and THE SCRATCH HOLD
// that keeps a repository a live turn has named from the expiry (M3-decisions
// M3-O61). The pass's clock is the harness's, years past every record's broker
// instant, so a scratch repository made here is idle to it unless a case says
// otherwise.

// activity is a repository's idle clock, its last push and its version, as its
// row holds them.
type activity struct{ lastActivity, lastPush, version int64 }

func (n *node) activityOf(id string) activity {
	n.t.Helper()
	var a activity
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT last_activity_at, COALESCE(last_push_at, 0), version
			FROM code_repos WHERE id = ?`, id).Scan(&a.lastActivity, &a.lastPush, &a.version)
	}); err != nil {
		n.t.Fatalf("read %s's activity: %v", id, err)
	}
	return a
}

// statusOf is a repository's status as its row holds it.
func (n *node) statusOf(id string) code.RepoStatus {
	n.t.Helper()
	var status string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT status FROM code_repos WHERE id = ?`, id).Scan(&status)
	}); err != nil {
		n.t.Fatalf("read %s's status: %v", id, err)
	}
	return code.RepoStatus(status)
}

// holding is a Holds reader that answers held for the keys it holds — and
// remembers every page it was asked.
type holding struct {
	held  map[string]bool
	asked [][]string
}

func (h *holding) Holds(_ context.Context, keys []string) (map[string]bool, error) {
	h.asked = append(h.asked, slices.Clone(keys))
	out := map[string]bool{}
	for _, k := range keys {
		if h.held[k] {
			out[k] = true
		}
	}
	return out, nil
}

// A RENAMED REPOSITORY'S OLD NAME IS RELEASED ONCE ITS REDIRECT HAS ENDED, and
// not before: the pass lists the redirects whose window ended by its clock and
// releases each with one record, after which the old name names nothing and is
// free.
//
// Mutation: list every redirect whatever its end — the early pass writes a
// release, red.
func TestRetentionReleasesARedirectPastItsWindow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "old-name", unitOwned("eng"))
	n.lands(lcRename(lcPerson, id, "new-name"))
	until := n.resolved("old-name").RedirectUntil
	var p purged
	early := passOver(n.store, n.reader, &p)
	early.Now = func() time.Time { return until.Add(-time.Minute) }
	if report, err := early.Pass(t.Context()); err != nil || report.Released != 0 {
		t.Fatalf("a pass before the redirect's end = %+v, %v, want nothing released", report, err)
	}
	if got := n.resolved("old-name"); got.ID != id {
		t.Fatalf("the old name answers %+v before its window ended", got)
	}
	// PAST IT: the pass's clock, and the release record's own broker instant.
	n.storedAt = func(uint64, time.Time) time.Time { return until.Add(time.Minute) }
	late := passOver(n.store, n.reader, &p)
	late.Now = func() time.Time { return until.Add(time.Minute) }
	report, err := late.Pass(t.Context())
	n.storedAt = nil
	if err != nil || report.Released != 1 {
		t.Fatalf("a pass past the redirect's end = %+v, %v, want the old name released", report, err)
	}
	if _, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "old-name", Fresh: stale}); !errors.Is(err,
		code.ErrRepoNotFound) {
		t.Fatalf("the released old name resolves: %v", err)
	}
	if got := n.resolved("new-name"); got.ID != id {
		t.Fatalf("the new name answers %+v", got)
	}
	// AND THE NAME IS FREE.
	n.made(lcPerson, "old-name", unitOwned("eng"))
}

// AN IDLE SCRATCH REPOSITORY IS EXPIRED — deleted, restorable for
// code.deleted_retention_days like any deletion — and one pushed to inside its
// idle window, or archived on purpose, is kept.
//
// Mutation: drop the status term from the listing and the decide — the
// archived one expires, red.
func TestRetentionExpiresAnIdleScratchRepository(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	idle := n.made(dev, "dev.idle", seatOwned("dev"))
	busy := n.made(dev, "dev.busy", seatOwned("dev"))
	archived := n.made(dev, "dev.kept", seatOwned("dev"))
	project := n.made(lcPerson, "project", unitOwned("eng"))
	n.lands(lcOn(code.LifecycleArchive, dev, archived))
	n.storedAt = func(uint64, time.Time) time.Time { return harnessAt.Add(-time.Hour) }
	n.push(busy, entry("push-busy", set("refs/heads/main", code.ZeroOID, oidOf("busy"))))
	n.storedAt = nil
	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil || report.Expired != 1 || len(report.Failed) != 0 {
		t.Fatalf("the pass = %+v, %v, want the idle one expired", report, err)
	}
	for id, want := range map[string]code.RepoStatus{idle: code.RepoStatusDeleted,
		busy: code.RepoStatusActive, archived: code.RepoStatusArchived, project: code.RepoStatusActive} {
		if got := n.statusOf(id); got != want {
			t.Errorf("%s is %s after the pass, want %s", id, got, want)
		}
	}
	// RESTORABLE, as any deletion is, by the seat that owns it.
	n.lands(lcOn(code.LifecycleRestore, dev, idle))
}

// A SCRATCH REPOSITORY A LIVE TURN HOLDS IS KEPT, and counted kept; once no
// turn holds it, the next pass expires it. The pass asks the holds once a page,
// for the page's ids.
//
// Mutation: ignore what the holds answer — both are expired, red.
func TestRetentionKeepsAScratchRepositoryALiveTurnHolds(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	held := n.made(dev, "dev.held", seatOwned("dev"))
	free := n.made(dev, "dev.free", seatOwned("dev"))
	var p purged
	pass := passOver(n.store, n.reader, &p)
	h := &holding{held: map[string]bool{held: true}}
	pass.Holds = h.Holds
	report, err := pass.Pass(t.Context())
	if err != nil || report.Expired != 1 || report.ScratchHeld != 1 {
		t.Fatalf("the pass = %+v, %v, want one expired and one held", report, err)
	}
	if len(h.asked) != 1 || !slices.Contains(h.asked[0], held) || !slices.Contains(h.asked[0], free) {
		t.Fatalf("the pass asked the holds %v, want one page naming both", h.asked)
	}
	if n.statusOf(held) != code.RepoStatusActive || n.statusOf(free) != code.RepoStatusDeleted {
		t.Fatalf("the held one is %s and the free one %s", n.statusOf(held), n.statusOf(free))
	}
	pass.Holds = holdsNone
	if report, err = pass.Pass(t.Context()); err != nil || report.Expired != 1 {
		t.Fatalf("the pass once nothing holds it = %+v, %v, want it expired", report, err)
	}
}

// A HOLD THAT CANNOT BE READ EXPIRES NOTHING: an unknown is never "nobody
// holds it". The step ends for the pass, says why on the report, and warns.
//
// Mutation: read the error as nothing held — both are expired, red.
func TestAnUnreadHoldSkipsTheScratchExpiry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	a := n.made(dev, "dev.a", seatOwned("dev"))
	b := n.made(dev, "dev.b", seatOwned("dev"))
	logs := &lineLog{}
	var p purged
	pass := passOver(n.store, n.reader, &p)
	pass.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	pass.Holds = func(context.Context, []string) (map[string]bool, error) {
		return nil, errors.New("seat holder node-b did not answer inside 2s")
	}
	report, err := pass.Pass(t.Context())
	if err != nil || report.Expired != 0 || !strings.Contains(report.ScratchExpiryUnread, "node-b") {
		t.Fatalf("the pass = %+v, %v, want nothing expired and the unread holds named", report, err)
	}
	if n.statusOf(a) != code.RepoStatusActive || n.statusOf(b) != code.RepoStatusActive {
		t.Fatal("a repository whose holds were unread was expired")
	}
	if warned := logs.lines("code_scratch_expiry_skipped"); len(warned) != 1 || warned[0]["level"] != "WARN" {
		t.Fatalf("the skip was logged %v, want one WARN line", warned)
	}
}

// HELD REPOSITORIES DO NOT STALL THE EXPIRY: a held repository stays active and
// idle, so a listing read from the top again would hand every page the same
// held ones and never reach the rest. Keyset, the pass reads past them.
//
// Mutation: drop the keyset term from the listing — every page is the same 256
// held repositories, and none is expired, red.
func TestHeldRepositoriesDoNotStallTheScratchExpiry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.scratch.ScratchMaxPerSeat = config.CodeScratchMaxPerSeatCeiling
	dev := agent("dev")
	oldest := harnessAt.Add(-30 * 24 * time.Hour)
	ids := make([]string, 0, 300)
	for i := range 300 {
		n.storedAt = func(uint64, time.Time) time.Time { return oldest.Add(time.Duration(i) * time.Second) }
		ids = append(ids, n.made(dev, fmt.Sprintf("dev.s%03d", i), seatOwned("dev")))
	}
	n.storedAt = nil
	h := &holding{held: map[string]bool{}}
	for _, id := range ids[:256] {
		h.held[id] = true
	}
	var p purged
	pass := passOver(n.store, n.reader, &p)
	pass.Holds = h.Holds
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	report, err := pass.Pass(ctx)
	if err != nil || report.Expired != 44 || report.ScratchHeld != 256 {
		t.Fatalf("the pass = %+v, %v, want the 44 unheld expired past the 256 held", report, err)
	}
}

// A HOLD RECORDED AFTER THE PASS READ ITS HOLDS STOPS THE EXPIRY. The pass
// fixes its cutoff and its page's position before it asks; a turn that takes
// its hold after that — too late for the pass to see it in memory or on a row
// — records it on the repository's subject, and the expiry's decide refuses a
// repository with any record past its page.
//
// Mutation: drop the version term from the expiry's decide — the repository is
// deleted under the turn, red.
func TestAHoldRecordedAfterThePassReadItsHoldsStopsTheExpiry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	id := n.made(dev, "dev.raced", seatOwned("dev"))
	seen := n.resolved("dev.raced").Position
	var p purged
	pass := passOver(n.store, n.reader, &p)
	pass.Holds = func(ctx context.Context, _ []string) (map[string]bool, error) {
		got, err := n.store.Lifecycle(ctx, lcHold(dev, id, "turn-raced", seen))
		if err != nil || got.Outcome != statelog.OutcomeApplied || got.Position.IsZero() {
			t.Errorf("the hold taken while the pass decides = %+v, %v, want it recorded", got, err)
		}
		return map[string]bool{}, nil
	}
	report, err := pass.Pass(t.Context())
	if err != nil || report.Expired != 0 {
		t.Fatalf("the pass = %+v, %v, want nothing expired", report, err)
	}
	if got := n.statusOf(id); got != code.RepoStatusActive {
		t.Fatalf("the repository is %s: an expiry decided under a hold it could not see", got)
	}
}

// AN EXPIRY THAT LANDS FIRST ANSWERS THE HOLD DELETED, before the turn reads
// anything, and the hold writes nothing: whichever of the two the broker
// orders first decides the other.
//
// Mutation: let the hold's decide record whatever the status — a record
// follows the delete, red.
func TestAnExpiryThatLandsFirstAnswersTheHoldDeleted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	id := n.made(dev, "dev.gone", seatOwned("dev"))
	seen := n.resolved("dev.gone").Position
	var p purged
	if report, err := passOver(n.store, n.reader, &p).Pass(t.Context()); err != nil || report.Expired != 1 {
		t.Fatalf("the pass = %+v, %v, want the repository expired", report, err)
	}
	end := n.end()
	got := n.life(lcHold(dev, id, "turn-late", seen))
	if got.Refusal != code.LifecycleRepoDeleted || !errors.Is(got.Err(), code.ErrRepoNotFound) {
		t.Fatalf("a hold after the expiry = %+v, want repo_deleted", got)
	}
	if n.end() != end {
		t.Fatal("a hold on an expired repository wrote a record")
	}
}

// THE CUTOFF IS FIXED BEFORE THE HOLDS ARE READ, and it is the expiry's only
// clock: the pass takes it once, before its page, and every decide judges the
// repository's idleness by it — never by the deciding node's own clock, which
// has moved on by the time the decide runs. A repository whose idle time
// reaches code.scratch.idle_days only after the pass fixed its cutoff is past
// no in-flight pass's cutoff, which is what lets a hold taken on it then go
// unrecorded.
//
// Mutation: judge idleness by the decide's own clock — the repository is
// deleted, red.
func TestTheCutoffIsFixedBeforeTheHoldsAreRead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	// THE PASS'S CLOCK AN HOUR BEHIND THE STORE'S, which decides at
	// harnessAt.
	passNow := harnessAt.Add(-time.Hour)
	idle := n.scratch.ScratchIdle
	n.storedAt = func(uint64, time.Time) time.Time { return passNow.Add(-idle).Add(30 * time.Minute) }
	id := n.made(dev, "dev.edge", seatOwned("dev"))
	n.storedAt = nil
	cutoff := passNow.Add(-idle)
	listed, view, err := code.IdleScratchView(t.Context(), n.reader, cutoff, 256)
	if err != nil {
		t.Fatalf("list the idle scratch repositories: %v", err)
	}
	if slices.Contains(listed, id) {
		t.Fatalf("a repository active after the cutoff was listed: %v", listed)
	}
	if expired, err := n.store.ExpireScratch(t.Context(), id, view); err != nil || expired {
		t.Fatalf("an expiry at the pass's cutoff = %v, %v, want the repository kept", expired, err)
	}
	var p purged
	pass := passOver(n.store, n.reader, &p)
	pass.Now = func() time.Time { return passNow }
	if report, err := pass.Pass(t.Context()); err != nil || report.Expired != 0 {
		t.Fatalf("the pass = %+v, %v, want nothing expired", report, err)
	}
	if got := n.statusOf(id); got != code.RepoStatusActive {
		t.Fatalf("the repository is %s", got)
	}
}

// A HOLD RECORD MOVES NO ACTIVITY: its apply moves the repository's version —
// what orders it against an expiry — and never its idle clock or its last push,
// since a hold is a turn's dependency and not work in the repository. A
// repository hold on a project, or on an archived scratch repository, is
// answered with no record: neither expires while so.
//
// Mutation: move last_activity_at in the hold's apply — red.
func TestAHoldRecordMovesNoActivity(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	id := n.made(dev, "dev.still", seatOwned("dev"))
	before := n.activityOf(id)
	got := n.lands(lcHold(dev, id, "turn-1", n.resolved("dev.still").Position))
	if got.Position.IsZero() {
		t.Fatalf("a hold on an active scratch repository wrote no record: %+v", got)
	}
	after := n.activityOf(id)
	if after.lastActivity != before.lastActivity || after.lastPush != before.lastPush ||
		after.version <= before.version {
		t.Fatalf("the hold moved %+v to %+v, want only the version moved", before, after)
	}
	project := n.made(lcPerson, "proj", unitOwned("eng"))
	archived := n.made(dev, "dev.arch", seatOwned("dev"))
	n.lands(lcOn(code.LifecycleArchive, dev, archived))
	end := n.end()
	for name, id := range map[string]string{"proj": project, "dev.arch": archived} {
		if got := n.lands(lcHold(dev, id, "turn-1", n.resolved(name).Position)); !got.Position.IsZero() {
			t.Errorf("a hold on %s wrote a record at %s", name, got.Position)
		}
	}
	if n.end() != end {
		t.Fatal("a hold on a repository no expiry takes wrote a record")
	}
}

// A HOLD IS DECIDED AT THE READ IT ORDERS: it is published at the turn's own
// read of the repository, so a data node behind that read — the router may ask
// one, since a request carries this node's own writes' floor and never a
// read's — answers that it is behind, and the router asks the next, rather than
// refuse from rows older than what the turn has just read.
//
// Mutation: publish the hold without its session floor — the node behind
// answers repo_deleted for a repository the turn has just read active, red.
func TestAHoldIsDecidedAtTheReadItOrders(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	a.chart, b.chart = testChart(), testChart()
	dev := agent("dev")
	id := a.made(dev, "dev.back", seatOwned("dev"))
	a.lands(lcOn(code.LifecycleDelete, dev, id))
	b.drain()
	b.hold()
	seen := a.lands(lcOn(code.LifecycleRestore, dev, id)).Position
	end := a.end()
	if _, err := b.store.Lifecycle(t.Context(), lcHold(dev, id, "turn-b", seen)); !errors.Is(err,
		code.ErrNotServing) {
		t.Fatalf("a hold asked of a node behind the turn's read = %v, want ErrNotServing", err)
	}
	if a.end() != end {
		t.Fatal("the node behind the turn's read wrote a record")
	}
	if got := a.lands(lcHold(dev, id, "turn-a", seen)); got.Position.IsZero() {
		t.Fatalf("the hold asked of a node past the read wrote no record: %+v", got)
	}
}

// A HOLD NEEDS THE SEAT TO READ IT: a seat holds only what it may read, and a
// person holds nothing — no turn runs as a person, so a person's hold is no
// request at all.
//
// Mutation: drop the grant check — the outsider's hold is recorded, red.
func TestAHoldNeedsTheSeatToReadIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	req := lcCreate(agent("dev"), "dev.unit", seatOwned("dev"))
	req.Create.Visibility = code.VisibilityUnit
	id := n.lands(req).Repo.ID
	seen := n.resolved("dev.unit").Position
	end := n.end()
	if got := n.life(lcHold(agent("vp-sales"), id, "turn-x", seen)); got.Refusal != code.LifecycleNotPermitted {
		t.Fatalf("an outsider's hold on a unit repository = %+v, want not_permitted", got)
	}
	if _, err := n.store.Lifecycle(t.Context(), lcHold(lcPerson, id, "turn-y", seen)); !errors.Is(err,
		code.ErrInvalid) {
		t.Fatalf("a person's hold = %v, want ErrInvalid", err)
	}
	if n.end() != end {
		t.Fatal("a refused hold wrote a record")
	}
}

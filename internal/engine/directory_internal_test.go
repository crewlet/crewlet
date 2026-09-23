package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// A SUSPENSION WITHDRAWS A SEAT'S CONTACT IDENTITIES WITHIN ONE APPLY, with no
// org-chart record anywhere.
//
// The whole path, from the identity applier the register builds to the
// registry every inbound delivery resolves through: one status record is
// applied and committed, the applier's post-commit signal wakes this node's
// directory trigger, and the registry it rebuilds — for the SAME company,
// because nothing about the company moved — no longer attributes the
// founder's Slack account to their seat.
//
// THE CONTROL is the registry every node built before the directory existed:
// the org view alone. It keeps routing to the suspended founder, which is the
// defect, and it is built here from the very company the rebuilt registry
// answers for.
func TestASuspensionWithdrawsContactIdentitiesWithinOneApply(t *testing.T) {
	t.Parallel()
	rig := newDirectoryRig(t)
	founder := rig.bindFounder(t)
	rig.awaitRouting(t, true, "an active founder's account")

	rig.setStage(t, founder, iam.StageSuspended)
	rig.awaitRouting(t, false, "a suspended founder's account")

	if why, ok := rig.engine.Registry().Withholding(founderSeat); !ok ||
		why != notify.WithheldSuspended {
		t.Errorf("the registry reports the founder's seat as %q/%v, want suspended",
			why, ok)
	}
	if !rig.engine.indexes(rig.company) {
		t.Error("the rebuild indexed a different company: a suspension moves " +
			"nothing in the org, so the registry must answer for the same one")
	}

	// THE CONTROL: the org view alone still routes to them.
	chartOnly := notify.NewRegistry(rig.company.Org)
	chartOnly.ReconcileHumanContacts(rig.company.Org, rig.engine.resolver().LookupOK,
		notify.Standing{})
	if p, ok := chartOnly.ByExternalID("slack", founderSlack); !ok || p.Handle != founderSeat {
		t.Fatalf("the org-view-only registry does not route the founder (%+v), "+
			"so the withdrawal above proves nothing", p)
	}
}

// REINSTATING THEM RESTORES THEIR IDENTITIES, through the same trigger and
// again with no chart record: the withdrawal is a reading of the directory,
// not a state the registry was left in.
func TestUnsuspendingRestoresThem(t *testing.T) {
	t.Parallel()
	rig := newDirectoryRig(t)
	founder := rig.bindFounder(t)
	rig.awaitRouting(t, true, "an active founder's account")

	rig.setStage(t, founder, iam.StageSuspended)
	rig.awaitRouting(t, false, "a suspended founder's account")

	rig.setStage(t, founder, iam.StageActive)
	rig.awaitRouting(t, true, "a reinstated founder's account")
	if _, ok := rig.engine.Registry().Withholding(founderSeat); ok {
		t.Error("a reinstated seat is still reported withheld")
	}
	// AND THE OTHER HUMAN SEAT WAS NEVER TOUCHED, which is what a WHOLE
	// rebuild buys: a diff against a fresh registry would have dropped
	// every identity the suspension did not name.
	if _, ok := rig.engine.Registry().ByExternalID("slack", colleagueSlack); !ok {
		t.Error("a rebuild lost the colleague's identity it had nothing to do with")
	}
}

// EVERY NODE READS THE DIRECTORY ITS ROLES GIVE IT — decided by the wiring a
// running node goes through, not asserted of a hand-built engine.
//
// A node that runs the identity domain reads its OWN rows, gated on its own
// applier's position, and answers the fleet's question for the nodes that
// cannot. A seats-only satellite runs no identity domain, so its copy of the
// tables is empty: it must neither read that copy ("nobody holds any seat") nor
// fall back to the chart, since it consumes deliveries and runs seats like
// every other node. It asks the fleet. Alone in its fleet, with nobody running
// the domain and nothing ever written to its log, the fleet's answer is the
// empty directory — a CONSULTED reading that withholds nothing.
func TestEachNodeReadsTheDirectoryItsRolesGiveIt(t *testing.T) {
	t.Parallel()

	t.Run("a node running the identity domain reads its own rows", func(t *testing.T) {
		t.Parallel()
		e := bootDirectoryNode(t, nil)
		dir, at := e.partyDirectory()
		if _, own := dir.(iamDirectory); !own || at == nil {
			t.Fatalf("the directory is %T (position gate %v), want this node's "+
				"own rows gated on its own applier", dir, at != nil)
		}
		// AND IT ANSWERS FOR THE NODES THAT HOLD NONE.
		request, err := json.Marshal(holdersRequest{Version: holdersProtocol})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		replies, err := e.backends.Queue.Ask(ctx, topics.IamHolders, request, 1)
		if err != nil || len(replies) != 1 {
			t.Fatalf("asked the fleet's directory: %d replies, %v — a node "+
				"running the domain must answer it", len(replies), err)
		}
	})

	t.Run("a seats-only node asks the fleet", func(t *testing.T) {
		t.Parallel()
		e := bootDirectoryNode(t, []string{"seats"})
		if e.IAM() != nil {
			t.Fatal("a seats-only node opened the identity domain, so this case " +
				"is not about the node it names")
		}
		dir, at := e.partyDirectory()
		if _, fleet := dir.(*fleetDirectory); !fleet || at != nil {
			t.Fatalf("the directory is %T (position gate %v), want the fleet's, "+
				"asked on every net tick", dir, at != nil)
		}
		reg := e.Registry()
		if !reg.Standing().Consulted() || reg.Standing().Unread() {
			t.Errorf("the registry was built from %+v, want the fleet's reading",
				reg.Standing())
		}
		if _, ok := reg.ByExternalID("slack", founderSlack); !ok {
			t.Error("a fleet whose directory has nobody bound withheld a seat")
		}
	})
}

// AN ENGINE WITH NO NATIVE RUNTIME IS CHART-ONLY, and a signal changes nothing.
//
// That is `crewlet validate`, and a test: nothing started a directory, so the
// registry consults none and the trigger has nothing to rebuild from — not
// even the pointer, which is what a reader holding the live registry compares.
func TestAnEngineWithNoNativeRuntimeIsChartOnly(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	company := directoryCompany(t, e)
	e.refreshParties(t.Context(), company)
	reg := e.Registry()
	if reg.Standing().Consulted() {
		t.Error("an engine with no directory built its registry from a reading")
	}
	chartOnly := notify.NewRegistry(company.Org)
	chartOnly.ReconcileHumanContacts(company.Org, e.resolver().LookupOK, notify.Standing{})
	want, got := chartOnly.Identities("slack"), reg.Identities("slack")
	if len(want) == 0 || fmt.Sprint(want) != fmt.Sprint(got) {
		t.Errorf("slack identities = %v, want the chart-only set %v", got, want)
	}
	e.nudgeDirectory()
	e.refreshDirectory(t.Context(), true)
	if e.Registry() != reg {
		t.Error("the directory trigger rebuilt a registry on an engine with no directory")
	}
}

// THE TWO REBUILD TRIGGERS SWAP WHOLE REGISTRIES, never a half-built one and
// never a diff, however they interleave.
//
// A published company (the chart view's trigger) and a directory signal both
// rebuild one pointer, and the second rebuilds for the SAME company the first
// built for. Run together under the race detector, with a reader resolving the
// seat the directory never touches on every registry it can see: that seat
// vanishing even once is a registry published half-built or patched rather
// than rebuilt. At the end the registry answers for the LAST company published
// and the LAST reading taken — the rebuild that read the directory first must
// never be the one that lands last.
func TestTheTwoRebuildTriggersSwapWholeRegistries(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	var suspended atomic.Bool
	e.useDirectory(directoryFunc(func(context.Context) ([]notify.Holder, error) {
		stage := iam.StageActive
		if suspended.Load() {
			stage = iam.StageSuspended
		}
		return []notify.Holder{
			{Seat: founderSeat, Stage: stage},
			{Seat: colleagueSeat, Stage: iam.StageActive},
		}, nil
	}), nil)

	first := directoryCompany(t, e)
	e.refreshParties(t.Context(), first)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, ok := e.Registry().ByExternalID("slack", colleagueSlack); !ok {
				t.Error("a registry was published without the colleague's " +
					"identity: half-built, or patched rather than rebuilt")
				return
			}
		}
	})

	const rounds = 50
	companies := make([]*Company, rounds)
	for i := range companies {
		companies[i] = directoryCompany(t, e)
	}
	var triggers sync.WaitGroup
	triggers.Go(func() {
		for _, c := range companies {
			e.refreshParties(t.Context(), c)
		}
	})
	triggers.Go(func() {
		for i := range rounds {
			suspended.Store(i%2 == 0)
			e.refreshDirectory(t.Context(), true)
		}
	})
	triggers.Wait()
	close(stop)
	readers.Wait()

	// THE LAST WORD FROM EACH: the final company, and a final reading.
	suspended.Store(true)
	e.refreshDirectory(t.Context(), true)
	if !e.indexes(companies[rounds-1]) {
		t.Error("the registry does not answer for the last company published")
	}
	if _, ok := e.Registry().ByExternalID("slack", founderSlack); ok {
		t.Error("the registry routes a founder the last reading suspended")
	}
	suspended.Store(false)
	e.refreshDirectory(t.Context(), true)
	if _, ok := e.Registry().ByExternalID("slack", founderSlack); !ok {
		t.Error("the registry withholds a founder the last reading reinstated")
	}
}

// AN UNREADABLE DIRECTORY CARRIES THE LAST READING FORWARD.
//
// Neither alternative is safe: chart-only hands every suspended person's seat
// back to the chart for the length of a store blip, and withholding every human
// seat silences the company over the same blip. What the node last KNEW is the
// reading the live registry was built from — including across a newly
// published company, which is where a stale reading would otherwise be lost.
func TestAnUnreadableDirectoryCarriesTheLastReadingForward(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	var failing atomic.Bool
	e.useDirectory(directoryFunc(func(context.Context) ([]notify.Holder, error) {
		if failing.Load() {
			return nil, sql.ErrConnDone
		}
		return []notify.Holder{{Seat: founderSeat, Stage: iam.StageSuspended}}, nil
	}), nil)

	e.refreshParties(t.Context(), directoryCompany(t, e))
	if _, ok := e.Registry().ByExternalID("slack", founderSlack); ok {
		t.Fatal("precondition: a suspended founder routes")
	}

	failing.Store(true)
	e.refreshParties(t.Context(), directoryCompany(t, e))
	if _, ok := e.Registry().ByExternalID("slack", founderSlack); ok {
		t.Error("a directory that could not be read handed the suspended " +
			"founder's seat back to the chart")
	}
	if _, ok := e.Registry().ByExternalID("slack", colleagueSlack); !ok {
		t.Error("a directory that could not be read withheld a seat nobody suspended")
	}
	// AND THE NET IS OWED A RETRY, which the periodic tick honours however
	// still the applier's position is.
	e.notify.rebuilding.Lock()
	owed := e.notify.directoryFailed
	e.notify.rebuilding.Unlock()
	if !owed {
		t.Error("an unreadable directory left no retry owed")
	}
}

// A NODE THAT HAS NEVER READ ITS DIRECTORY FAILS CLOSED.
//
// The first registry is built at boot, with no live registry to carry a
// reading from. What it carried used to be the zero Standing — chart-only —
// so a directory unreadable at that moment registered every suspended,
// retired and removed holder's accounts until the net's re-read landed. It now
// withholds every human seat instead, and the net's first successful read is
// what routes them: the control is that very read, which must restore the
// seats a reading does not withhold.
func TestAnUnreadableFirstReadingWithholdsEveryHumanSeat(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	var failing atomic.Bool
	failing.Store(true)
	e.useDirectory(directoryFunc(func(context.Context) ([]notify.Holder, error) {
		if failing.Load() {
			return nil, sql.ErrConnDone
		}
		return []notify.Holder{{Seat: founderSeat, Stage: iam.StageSuspended}}, nil
	}), nil)

	company := directoryCompany(t, e)
	e.refreshParties(t.Context(), company)
	reg := e.Registry()
	for _, id := range []string{founderSlack, colleagueSlack} {
		if p, ok := reg.ByExternalID("slack", id); ok {
			t.Errorf("a node that never read its directory routes %s to %q — the "+
				"chart-only answer, which hands a suspended holder their seat", id,
				p.Handle)
		}
	}
	if why, ok := reg.Withholding(colleagueSeat); !ok || why != notify.WithheldUnread {
		t.Errorf("the colleague's seat reports %q/%v, want %q", why, ok,
			notify.WithheldUnread)
	}
	if _, ok := reg.ByHandle("ceo"); !ok {
		t.Error("an unread directory took an agent seat out of the registry")
	}

	// A SECOND UNREADABLE PASS keeps it closed, even for a newly published
	// company: an unread reading carried forward is still an unread one.
	e.refreshParties(t.Context(), directoryCompany(t, e))
	if _, ok := e.Registry().ByExternalID("slack", colleagueSlack); ok {
		t.Error("a second unreadable pass reopened contact routing")
	}

	// THE CONTROL: the net's first successful read routes what the reading
	// does not withhold, and withholds what it does.
	failing.Store(false)
	e.refreshDirectory(t.Context(), false)
	if _, ok := e.Registry().ByExternalID("slack", colleagueSlack); !ok {
		t.Error("the first successful reading did not route the colleague")
	}
	if _, ok := e.Registry().ByExternalID("slack", founderSlack); ok {
		t.Error("the first successful reading routed a suspended founder")
	}
}

// The fixture: a company with two human seats and a directory rig over a real
// replicated estate, driven through the identity applier the register builds.

const (
	founderSeat    = "dana-founder"
	founderSlack   = "U0FOUNDER"
	colleagueSeat  = "priya-shah"
	colleagueSlack = "U0PRIYA"
)

// directoryCompany is a fresh company with two human seats, each declaring a
// Slack account.
func directoryCompany(t *testing.T, e *Engine) *Company {
	t.Helper()
	company, err := NewCompanyWith(directoryConfig(t), e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	return company
}

// bootDirectoryNode is a whole node — [New] and every stage of its boot — with
// the directory company, under the roles given (nil for all three).
func bootDirectoryNode(t *testing.T, roles []string) *Engine {
	t.Helper()
	return bootDirectoryNodeWith(t, roles, nil)
}

// bootDirectoryNodeWith is [bootDirectoryNode] with the node's Tier A amended
// before it boots, for a case about what a node reads from its own
// configuration.
func bootDirectoryNodeWith(t *testing.T, roles []string,
	amend func(*config.Bootstrap)) *Engine {

	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	b.Node.Roles = roles
	if amend != nil {
		amend(&b)
	}
	// A RECORDER, as `crewlet run` always hands one: a node booted without
	// one silently drops every gauge a case might read.
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: directoryConfig(t),
		Metrics: rec})
	if err != nil {
		t.Fatalf("boot a node with roles %v: %v", roles, err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// directoryConfig is the company document behind [directoryCompany].
func directoryConfig(t *testing.T) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Dana Founder
    handle: ` + founderSeat + `
    kind: human
    contact: {slack_user_id: ` + founderSlack + `}
  - name: Priya Shah
    handle: ` + colleagueSeat + `
    kind: human
    contact: {slack_user_id: ` + colleagueSlack + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// directoryFunc adapts a function to the notify seam.
type directoryFunc func(context.Context) ([]notify.Holder, error)

func (f directoryFunc) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	return f(ctx)
}

// directoryRig is one node: an engine running its directory trigger over a
// real replicated estate, and the identity applier THE REGISTER builds for it.
type directoryRig struct {
	engine  *Engine
	company *Company
	db      *store.DB
	applier statelog.Applier
	seq     uint64
}

func newDirectoryRig(t *testing.T) *directoryRig {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	log, err := statelogtest.LocalReader(iamdomain.Domain{}, db.Replicated(),
		statelog.Position{})
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{DB: db, Log: log})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	e.useDirectory(iamDirectory{reader: reader}, reader.At)

	// THROUGH THE REGISTER, so the signal this case depends on is the one
	// a running node wires rather than one the test hands in.
	entry, ok := registrationFor(iamdomain.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no identity domain")
	}
	applier, err := entry.NewApplier(&stateLog{
		nodeID: "node-a", applyHooks: e.applyHooks(),
	})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var loop sync.WaitGroup
	loop.Go(func() { e.watchDirectory(ctx) })
	t.Cleanup(func() {
		cancel()
		loop.Wait()
	})

	company := directoryCompany(t, e)
	e.refreshParties(t.Context(), company)
	return &directoryRig{engine: e, company: company, db: db, applier: applier}
}

// bindFounder enrols the founder, active, and binds them to their seat — the
// state every case starts from — answering their person id.
func (r *directoryRig) bindFounder(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	claim, err := iamdomain.EncodeClaim(iamdomain.Claim{
		V: iamdomain.DocumentVersion, Person: id})
	if err != nil {
		t.Fatal(err)
	}
	r.apply(t, iamdomain.SeatSubject(founderSeat), iamdomain.OpClaim, id, claim)
	person, err := iamdomain.EncodePerson(iamdomain.Person{
		V: iamdomain.DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive})
	if err != nil {
		t.Fatal(err)
	}
	r.apply(t, iamdomain.PersonSubject(id), iamdomain.OpEnrol, id, person)
	return id
}

// setStage applies ONE status record — the whole of a suspension.
func (r *directoryRig) setStage(t *testing.T, id string, stage iam.Stage) {
	t.Helper()
	change, err := iamdomain.EncodeStatus(iamdomain.StatusChange{
		V: iamdomain.DocumentVersion, Stage: stage})
	if err != nil {
		t.Fatal(err)
	}
	r.apply(t, iamdomain.PersonSubject(id), iamdomain.OpStatus, id, change)
}

// apply commits one record through the applier and runs its post-commit half,
// exactly as the framework's loop does.
func (r *directoryRig) apply(t *testing.T, subject iamdomain.Subject,
	op iamdomain.OpKind, person string, mutation []byte) {
	t.Helper()
	r.seq++
	rec := iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: fmt.Sprintf("op-%d", r.seq),
			Subject: subject, Op: op, Writer: "node-a",
			Scope: iamdomain.PeopleScope(person),
		},
		Mutation: mutation, Person: person,
		Actor: "ana.admin", ActorKind: iam.KindPerson,
	}
	payload, err := iamdomain.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(subject.Kind),
			Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
			Op:      string(op), OpID: rec.OpID, Writer: "node-a",
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: r.seq,
		},
		Payload:  payload,
		StoredAt: time.Unix(1_700_000_000+int64(r.seq), 0).UTC(),
	}
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, gated, err := r.applier.Gated(t.Context(), tx, record); err != nil || gated {
			return fmt.Errorf("gated=%v: %w", gated, err)
		}
		_, err := r.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: record.StoredAt, StoredAt: record.StoredAt})
		return err
	}); err != nil {
		t.Fatalf("apply %s %s: %v", op, subject, err)
	}
	r.applier.Committed(t.Context())
}

// awaitRouting waits for this node's registry to route — or stop routing — the
// founder's Slack account, which the directory trigger does on its own
// goroutine after the commit.
func (r *directoryRig) awaitRouting(t *testing.T, routes bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, ok := r.engine.Registry().ByExternalID("slack", founderSlack)
		if ok == routes {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s routes: %v, want %v, ten seconds after the commit", what,
				ok, routes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

package engine

import (
	"context"
	"database/sql"
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
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A SUSPENSION WITHDRAWS A SEAT'S CONTACT IDENTITIES WITHIN ONE APPLY, with no
// configuration change: one status record committed through the applier the
// register builds signals the directory trigger, and the registry it rebuilds,
// for the SAME company, no longer attributes the founder's Slack account to
// their seat. THE CONTROL, the org view alone built from that same company,
// still routes to the suspended founder.
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
	chartOnly := notify.NewRegistry(rig.company.Org, nil)
	chartOnly.ReconcileHumanContacts(rig.company.Org, rig.engine.resolver().LookupOK,
		notify.Standing{})
	if p, ok := chartOnly.ByExternalID("slack", founderSlack); !ok || p.Handle != founderSeat {
		t.Fatalf("the org-view-only registry does not route the founder (%+v), "+
			"so the withdrawal above proves nothing", p)
	}
}

// REINSTATING THEM RESTORES THEIR IDENTITIES through the same trigger: the
// withdrawal is a reading of the directory, not a state the registry was left in.
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
	// AND THE OTHER HUMAN SEAT WAS NEVER TOUCHED, which a WHOLE rebuild buys
	// and a diff against a fresh registry would not.
	if _, ok := rig.engine.Registry().ByExternalID("slack", colleagueSlack); !ok {
		t.Error("a rebuild lost the colleague's identity it had nothing to do with")
	}
}

// EVERY DATA NODE READS ITS OWN DIRECTORY, WHATEVER ITS ROLES, through the
// wiring a running node boots with: its OWN rows, gated on its own applier's
// position. The node serving no API matters most: it consumes inbound
// deliveries too, so a chart-built registry there would attribute a suspended
// person's messages to their seat. Mutation: narrow the identity domain to the
// nodes that serve the API and the second case opens no reader.
func TestEveryDataNodeReadsItsOwnDirectoryWhateverItsRoles(t *testing.T) {
	t.Parallel()
	for name, roles := range map[string][]string{
		"a node declaring no roles":  nil,
		"a data node serving no API": {"data", "seats", "workers"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := bootDirectoryNode(t, roles)
			if e.IAM() == nil {
				t.Fatal("the node opened no identity domain")
			}
			dir, at := e.partyDirectory()
			if _, own := dir.(iamDirectory); !own || at == nil {
				t.Fatalf("the directory is %T (position gate %v), want this "+
					"node's own rows gated on its own applier", dir, at != nil)
			}
			if reg := e.Registry(); !reg.Standing().Consulted() || reg.Standing().Unread() {
				t.Errorf("the registry was built from %+v, want this node's "+
					"own reading", reg.Standing())
			}
		})
	}
}

// A NODE WITHOUT `data` READS A DATA NODE'S DIRECTORY, through the estate
// router, re-read on the registry's timer with no position to gate on: it runs
// no applier (ADR-0025), and a chart-built registry would route a suspended
// person's word to their seat. Mutation: hand it the local reader's shape, or a
// position gate, and this fails.
//
// This calls the hand-over's body on a rig, since booting such a node needs a
// leaf link only internal/e2e stands up; that [New] calls it, end to end, is
// internal/e2e's TestAStatelessNodesPartyRegistryReadsTheDirectory.
func TestANodeWithoutDataReadsTheDirectoryThroughTheRouter(t *testing.T) {
	t.Parallel()
	e, _ := statelessOver(t)
	e.directoryNudge = make(chan struct{}, 1)
	e.startRoutedDirectory(t.Context())
	t.Cleanup(e.routedDirectory.end)
	dir, at := e.partyDirectory()
	if _, routed := dir.(routedIdentity); !routed || at != nil {
		t.Fatalf("the directory is %T (position gate %v), want a data node's, "+
			"read through the router with no position of this node's", dir, at != nil)
	}
}

// AN ENGINE HANDED NO DIRECTORY IS CHART-ONLY, and a signal changes nothing,
// not even the pointer a reader holding the live registry compares. Only
// `crewlet validate` and a test are in this posture, never an engine [New]
// builds.
func TestAnEngineHandedNoDirectoryIsChartOnly(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	company := directoryCompany(t, e)
	e.refreshParties(t.Context(), company)
	reg := e.Registry()
	if reg.Standing().Consulted() {
		t.Error("an engine with no directory built its registry from a reading")
	}
	chartOnly := notify.NewRegistry(company.Org, nil)
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
// never a diff, however they interleave. A publish and a directory signal run
// together under the race detector while a reader resolves the seat the
// directory never touches: its vanishing once is a registry published
// half-built or patched. At the end the registry answers for the LAST company
// published and the LAST reading, never the rebuild that read first.
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

// AN UNREADABLE DIRECTORY CARRIES THE LAST READING FORWARD
// ([Engine.readStandingLocked]), across a newly published company too, where a
// stale reading would otherwise be lost.
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
	// AND THE NET IS OWED A RETRY, however still the applier's position is.
	e.notify.rebuilding.Lock()
	owed := e.notify.directoryFailed
	e.notify.rebuilding.Unlock()
	if !owed {
		t.Error("an unreadable directory left no retry owed")
	}
}

// A NODE THAT HAS NEVER READ ITS DIRECTORY FAILS CLOSED: the boot registry,
// with nothing to carry, withholds every human seat rather than going
// chart-only. The control is the net's first successful read, which must route
// the seats a reading does not withhold.
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
	// company: an unread reading carried forward is still unread.
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
// before it boots.
func bootDirectoryNodeWith(t *testing.T, roles []string,
	amend func(*config.Bootstrap)) *Engine {

	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	b.Node.Roles = roles
	if amend != nil {
		amend(&b)
	}
	// A RECORDER, as `crewlet run` hands one: without it a node drops every
	// gauge a case might read.
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
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{DB: db.Replicated().Reader()})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	e.useDirectory(iamDirectory{reader: reader}, reader.At)

	// THROUGH THE REGISTER, so the signal is the one a running node wires.
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

// bindFounder enrols the founder, active and bound to their seat, in one
// directory record, answering their person id.
func (r *directoryRig) bindFounder(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	enrolled, err := iamdomain.EncodeEnrolled(iamdomain.Enrolled{
		V: iamdomain.DocumentVersion,
		Person: iamdomain.Person{V: iamdomain.DocumentVersion,
			Kind: iam.KindPerson, Stage: iam.StageActive},
		Holds: iamdomain.Identifiers{SeatID: founderSeat},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.apply(t, iamdomain.DirectorySubject(), iamdomain.OpEnrol, id, enrolled)
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

// awaitRouting waits for this node's registry to route, or stop routing, the
// founder's Slack account: the trigger rebuilds on its own goroutine.
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

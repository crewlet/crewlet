package engine

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// heardMoves keeps what the credential listener was handed.
type heardMoves struct {
	mu    sync.Mutex
	moved []iamdomain.Moved
}

func (h *heardMoves) hear(m iamdomain.Moved) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.moved = append(h.moved, m)
}

// take is everything heard since the last call, and forgets it.
func (h *heardMoves) take() []iamdomain.Moved {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.moved
	h.moved = nil
	return out
}

// WHOSE CREDENTIALS AN IDENTITY BATCH MOVED REACHES THE LISTENER, THROUGH THE
// REGISTER, and a batch that moved none does not. The listener decides open
// dashboard sockets, and every internal/iamdomain case hands its own hook, so
// nothing else would notice a register that built the applier with none: the
// applier here is the one a running node builds. A sign-in is the control: it
// moves nobody's credential and must not reach the listener, or every sign-in
// reaches open sockets it did not touch.
func TestAnIdentityBatchReachesTheCredentialListener(t *testing.T) {
	t.Parallel()
	rig := newDirectoryRig(t)
	heard := &heardMoves{}
	rig.engine.SetOnIdentityMoved(heard.hear)

	founder := rig.bindFounder(t)
	if got := heard.take(); !slices.ContainsFunc(got, func(m iamdomain.Moved) bool {
		return slices.Contains(m.People, founder)
	}) {
		t.Fatalf("an enrolment and a bind reached the listener as %+v, want the "+
			"founder named", got)
	}

	lineage := uuid.NewString()
	session, err := iamdomain.EncodeSession(iamdomain.Session{
		V: iamdomain.DocumentVersion, Person: founder,
		AbsoluteExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.apply(t, iamdomain.SessionSubject(lineage), iamdomain.OpOpen, founder, session)
	if got := heard.take(); len(got) != 0 {
		t.Fatalf("a sign-in reached the credential listener as %+v: every sign-in "+
			"would reach open sockets it did not touch", got)
	}

	rig.apply(t, iamdomain.SessionSubject(lineage), iamdomain.OpClose, founder, session)
	if got := heard.take(); len(got) != 1 || !slices.Equal(got[0].Sessions, []string{lineage}) {
		t.Fatalf("a sign-out reached the listener as %+v, want its lineage", got)
	}

	rig.setStage(t, founder, iam.StageSuspended)
	if got := heard.take(); len(got) != 1 || !slices.Equal(got[0].People, []string{founder}) {
		t.Fatalf("a suspension reached the listener as %+v, want the founder", got)
	}
}

// A RECORD THE IDENTITY RUNNER RETAINS NAMES EVERYONE TO THE LISTENER, through
// the applier the register builds: the framework finds the retention hook by a
// type assertion, so a register that wrapped the applier or dropped the hook
// would compile, pass every iamdomain case, and leave a retained revocation
// reaching no open socket. It moves no seat: contact routing is rebuilt from
// rows, and a retained record wrote none.
func TestARetainedIdentityRecordReachesTheCredentialListener(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	entry, ok := registrationFor(iamdomain.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no identity domain")
	}
	applier, err := entry.NewApplier(&stateLog{nodeID: "node-a", applyHooks: e.applyHooks()})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	hook, ok := applier.(statelog.RetentionHook)
	if !ok {
		t.Fatalf("the identity applier the register builds (%T) does not take the "+
			"framework's retention hook, so a record this node retains reaches "+
			"no open socket", applier)
	}

	hook.Retained(t.Context(), 1)
	applier.Committed(t.Context())
	if got := heard.take(); len(got) != 1 || !got[0].Everyone {
		t.Fatalf("a batch that retained a record reached the listener as %+v, "+
			"want everyone", got)
	}
	select {
	case <-e.directoryNudge:
		t.Error("a batch that retained a record rebuilt the party registry, " +
			"which is rebuilt from rows a retained record never wrote")
	default:
	}
}

// AN ESTATE REPLACED WITH NO BATCH TO SAY SO NAMES EVERYONE TO THE LISTENER:
// after an adoption, or a reopen on a failed join's file, every revocation the
// donor applied arrived without an apply, so no committed batch names it, and a
// socket on a session one of them ended would serve on until it closed.
func TestAReplacedEstateNamesEveryone(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	s := &stateLog{applyHooks: e.applyHooks()}

	s.estateReplaced()
	got := heard.take()
	if len(got) != 1 || !got[0].Everyone {
		t.Fatalf("a replaced estate reached the listener as %+v, want everyone", got)
	}
	select {
	case <-e.directoryNudge:
	default:
		t.Error("a replaced estate did not rebuild the party registry: its rows " +
			"are a peer's, and a suspension the peer applied reaches contact " +
			"routing only at the next periodic re-read")
	}
}

// AN ESTATE REPLACED UNDER THE CHAT INDEX IS INDEXED AGAIN FROM ITS START: the
// index walks FORWARD over the transcript's versions, so a row replaced behind
// the walk is never reread, and what it indexed from the replaced copy would
// answer searches as from a caught-up index. The state log's own signal (raised
// by both an adoption and a reopen on a failed join's file) is asked, over an
// index caught up first: one that was not would read as not ready anyway.
func TestAReplacedEstateRebuildsTheChatIndex(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	index := search.NewChatIndexer(db, db.Replicated().Reader())
	for range 16 {
		worked, err := index.Sweep(t.Context(), time.Now())
		if err != nil {
			t.Fatalf("sweep the chat index: %v", err)
		}
		if worked == 0 {
			break
		}
	}
	if ready, err := index.Ready(t.Context()); err != nil || !ready {
		t.Fatalf("the premise: the chat index over an empty transcript has not "+
			"caught up (ready %v, %v)", ready, err)
	}

	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	e.native.Store(&native{chatIndex: index})
	s := &stateLog{applyHooks: e.applyHooks()}
	s.estateReplaced()
	if ready, err := index.Ready(t.Context()); err != nil || ready {
		t.Fatalf("the chat index still answers as caught up (%v, %v) over an estate "+
			"that was replaced under it, so every search reads what it indexed "+
			"from the copy that is gone", ready, err)
	}
}

// A REPLACED ESTATE REBUILDS THE INDEX OF CHANGES' WORDS, as it rebuilds the
// chat index, or a document indexed from the replaced copy answers a listing
// narrowed by words indefinitely.
// Mutation: drop the change index's call from estateReplaced.
func TestAReplacedEstateRebuildsTheChangeIndex(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	index := search.NewChangeIndexer(db, db.Replicated().Reader(), gitlet.Domain{})
	for range 16 {
		worked, err := index.Sweep(t.Context(), time.Now())
		if err != nil {
			t.Fatalf("sweep the change index: %v", err)
		}
		if worked == 0 {
			break
		}
	}
	held, err := index.Holding(t.Context(), search.ChangeQuery{Terms: []string{"login"}, Limit: 1})
	if err != nil || held.Rebuilding {
		t.Fatalf("the premise: the change index over an empty estate is rebuilding (%+v, %v)", held, err)
	}

	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	e.native.Store(&native{changeIndex: index})
	s := &stateLog{applyHooks: e.applyHooks()}
	s.estateReplaced()
	if held, err = index.Holding(t.Context(), search.ChangeQuery{Terms: []string{"login"}, Limit: 1}); err != nil ||
		!held.Rebuilding {
		t.Fatalf("the change index still answers as built (%+v, %v) over an estate that was replaced "+
			"under it", held, err)
	}
}

// AN ADOPTION NAMES EVERYONE TO THE LISTENER once the donor's rows are in place
// and the appliers run over them: every revocation a peer applied while this
// node was below the floor arrives inside the donor's file through no apply, so
// nothing else would say which open socket's credential one of them ended.
func TestAnAdoptionNamesEveryone(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, rows)
	standUpDonor(t, e, q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())

	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	if err := e.rejoin(s.run, s); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if got := heard.take(); !slices.ContainsFunc(got,
		func(m iamdomain.Moved) bool { return m.Everyone }) {
		t.Fatalf("an adoption told the credential listener %+v, want everyone: "+
			"its rows are a donor's", got)
	}
}

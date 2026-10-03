package engine

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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
// REGISTER, and a batch that moved none does not.
//
// The listener is what decides an open dashboard socket again — a handshake
// decision alone leaves a revoked session's tab receiving the company's state
// for as long as it stays open — and nothing would notice a register that
// built the identity applier with no hook: every case in internal/iamdomain
// hands its own. So the applier here is the one a running node builds, and the
// listener the one the API registers. A sign-in is the control: it is the bulk
// of this log's traffic, moves nobody's credential, and must reach the
// listener not at all, or every sign-in in the company re-decides every socket.
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
			"would decide every open socket again", got)
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

// AN ESTATE REPLACED WITH NO BATCH TO SAY SO DECIDES EVERY CREDENTIAL AGAIN.
//
// An adoption puts a donor's rows in place and a reopened estate may be the
// one a failed join installed: every revocation the donor applied while this
// node was too far behind to follow arrives without an apply, so no committed
// batch will ever name it. Told nothing, a socket opened on a session one of
// those records ended goes on serving it until it closes on its own.
func TestAReplacedEstateDecidesEveryCredentialAgain(t *testing.T) {
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

// AN ADOPTION DECIDES EVERY CREDENTIAL AGAIN, once the donor's rows are in
// place and the appliers are running over them.
//
// The node was below the floor: every revocation a peer applied in that time
// arrives inside the donor's file and through no apply of this node's, so
// nothing else would ever say which open socket's credential one of them
// ended.
func TestAnAdoptionDecidesEveryCredentialAgain(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", e.StateLogHydrated)
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, rows)
	standUpDonor(t, q, rows, statelog.Position{
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

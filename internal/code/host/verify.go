package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A VERIFY IS AN OPERATION THAT OUTLIVES ITS REQUEST. An operator's check of
// this node's cache of a repository reads every byte of it — a hash of every
// pack, then git's connectivity check over the whole history — and with a
// repair rebuilds and checks again: tens of seconds a gigabyte, minutes for a
// large repository. Answered in one request, the check outlived every
// balancer's read timeout in front of the API (nginx's default is sixty
// seconds), and the timeout's close cancelled the request and with it the
// check and any rebuild in flight — so a large repository could never be
// verified, or repaired, through a default proxy, each retry cut at the same
// point. So [Server.StartVerify] decides at once everything it can decide
// without reading the repository — the name, the copy this node holds, a
// slot of the maintenance lane and the memory the check needs, each a
// refusal its caller is told at once — and then runs the check on a goroutine
// of the HOST'S lifetime, never the request's: the caller is handed the
// verification's id, and [Server.Verification] answers it, running or ended,
// while the node keeps it.
//
// One verification a repository at a time on a node: a second asking for the
// same check is handed the one running, and one asking for the other
// (a repair beside a check, or a check beside a repair) is refused naming it
// ([VerifyRunningError]).

// VerifyKept is how long an ended verification is kept for its caller to read:
// an hour, so a client that lost its connection while the check ran — a
// laptop asleep, a proxy restarted — comes back for the answer rather than
// read every byte of the repository again, while what a report says of a
// cache that pushes keep moving is still recent. Memory is not the bound: a
// node keeps one verification a repository, the latest, so it holds at most
// one report for each repository it was asked about within the hour. A
// restart keeps none — a report is a check of a cache this process held.
const VerifyKept = time.Hour

// VerifyState is where a verification is: one of [VerifyStates].
type VerifyState string

const (
	// VerifyRunning is a check still running.
	VerifyRunning VerifyState = "running"
	// VerifyDone is a check that ended, Report what it found.
	VerifyDone VerifyState = "done"
	// VerifyFailed is a check that could not end — Err says why — and Report
	// what it found before it failed.
	VerifyFailed VerifyState = "failed"
)

// VerifyStates is every [VerifyState].
var VerifyStates = []VerifyState{VerifyRunning, VerifyDone, VerifyFailed}

// Valid reports whether s is one of [VerifyStates].
func (s VerifyState) Valid() bool { return slices.Contains(VerifyStates, s) }

// Verification is one check of this node's cache of a repository, as
// [Server.StartVerify] began it and [Server.Verification] answers it: a copy,
// true at the instant it was taken.
type Verification struct {
	// ID names the verification on this node.
	ID uuid.UUID
	// Repo is the repository's name and RepoID its id.
	Repo, RepoID string
	// Repair: a cache found damaged is rebuilt and checked again.
	Repair bool
	// State is where it is; Started and Ended (zero while it runs) when.
	State          VerifyState
	Started, Ended time.Time
	// Report is what the check found: once it is done, everything; once it
	// failed, what it found before it did, the repository named whenever it
	// was known.
	Report cache.VerifyReport
	// Err is why a failed verification failed, and nil otherwise:
	// ErrStopping wrapping what the cut left when the host's stop cut it
	// short.
	Err error
}

// VerifyRunningError is a verification refused because one of the same
// repository is already running on this node with the other repair setting:
// Running is that one, which its caller can read until it ends.
type VerifyRunningError struct {
	Running Verification
}

func (e *VerifyRunningError) Error() string {
	return fmt.Sprintf("host: a verify of %s (repair %t) is already running on this node as %s; "+
		"read it, and ask again once it has ended", e.Running.Repo, e.Running.Repair, e.Running.ID)
}

// verifications is a node's verifications: the latest of each repository,
// by its id, kept until VerifyKept after it ended or until the next verify
// of the same repository begins.
type verifications struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*Verification
	latest map[string]uuid.UUID // a repository's id → its latest verification
}

// sweep drops every verification that ended more than VerifyKept before
// now; mu is held. Run at every StartVerify and Verification, so what a node
// keeps needs no goroutine of its own to age.
func (v *verifications) sweep(now time.Time) {
	for id, got := range v.byID {
		if got.State != VerifyRunning && now.Sub(got.Ended) > VerifyKept {
			delete(v.byID, id)
			if v.latest[got.RepoID] == id {
				delete(v.latest, got.RepoID)
			}
		}
	}
}

// StartVerify begins a check of this node's cache of the repository named
// name, rebuilding it when repair finds it damaged (cache.Verification.Run),
// and answers the verification RUNNING — whose end [Server.Verification]
// reads. What it refuses, it refuses at once, having read nothing of the
// repository:
//
//   - code.ErrRepoNotFound when no repository is declared under the name —
//     read stale, and again through the barrier when not found, since a
//     refusal is never a stale answer — or the grammar refuses it;
//   - cache.ErrNotHeld when this node holds no copy of it: a check builds
//     none, since that is a whole download of the repository;
//   - gitproc.ErrBusy when the maintenance lane has no free slot or the
//     background work leaves the check's memory no room, and
//     cache.ErrNoMemory when the node's memory cannot hold the check at all;
//   - [*VerifyRunningError] when a verify of the repository with the other
//     repair setting is running here; one with the same setting is answered
//     as the verification running, its caller sharing it;
//   - ErrStopping on a stopping host, and whatever the resolve's read
//     refused with.
//
// The check runs under the host's lifetime alone: its caller ending its
// request ends nothing, and the host's Stop ends it, the verification then
// failed with ErrStopping wrapping what the cut left.
func (s *Server) StartVerify(ctx context.Context, name string, repair bool) (Verification, error) {
	if !gitref.ValidRepoName(name) {
		return Verification{}, code.ErrRepoNotFound
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return Verification{}, ErrStopping
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	started := false
	defer func() {
		if !started {
			s.inflight.Done()
		}
	}()

	ref, err := s.resolveVerified(ctx, name)
	if err != nil {
		return Verification{}, err
	}
	s.verifies.mu.Lock()
	defer s.verifies.mu.Unlock()
	now := s.now()
	s.verifies.sweep(now)
	if id, ok := s.verifies.latest[ref.ID]; ok && s.verifies.byID[id].State == VerifyRunning {
		running := *s.verifies.byID[id]
		if running.Repair != repair {
			return Verification{}, &VerifyRunningError{Running: running}
		}
		return running, nil
	}
	// ADMITTED UNDER THE REGISTRY'S LOCK, so two asking at once for one
	// repository are one verification rather than two admitted beside each
	// other; the admission waits for nothing (cache.AdmitVerify).
	checked, err := s.o.Cache.AdmitVerify(ref.ID)
	if err != nil {
		return Verification{}, err
	}
	v := &Verification{ID: uuid.Must(uuid.NewV7()), Repo: ref.Name, RepoID: ref.ID, Repair: repair,
		State: VerifyRunning, Started: now}
	if previous, ok := s.verifies.latest[ref.ID]; ok {
		delete(s.verifies.byID, previous)
	}
	if s.verifies.byID == nil {
		s.verifies.byID, s.verifies.latest = map[uuid.UUID]*Verification{}, map[string]uuid.UUID{}
	}
	s.verifies.byID[v.ID], s.verifies.latest[ref.ID] = v, v.ID
	started = true
	go s.runVerify(v.ID, checked, repair)
	return *v, nil
}

// resolveVerified is the repository a verify names, under ctx joined to the
// host's lifetime — a Stop reaches the read — and ErrStopping, wrapping what
// the cut left, when the stop cut it short rather than its caller.
func (s *Server) resolveVerified(ctx context.Context, name string) (code.RepoRef, error) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The host's lifetime ends the read too: Stop must reach it.
	defer context.AfterFunc(s.life, cancel)()
	ref, err := s.o.Estate.Resolve(work, code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if errors.Is(err, code.ErrRepoNotFound) {
		ref, err = s.o.Estate.Resolve(work, code.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	if err != nil && s.life.Err() != nil && ctx.Err() == nil {
		return code.RepoRef{}, fmt.Errorf("%w: %w", ErrStopping, err)
	}
	return ref, err
}

// runVerify is one verification's check, on a goroutine of the host's
// lifetime that StartVerify counted among the work Stop waits for, and the
// record of how it ended.
func (s *Server) runVerify(id uuid.UUID, checked *cache.Verification, repair bool) {
	defer s.inflight.Done()
	defer checked.Release()
	report, err := checked.Run(s.life, repair) //nolint:contextcheck // the host's lifetime, never a request's: see the file's doc
	if err != nil && s.life.Err() != nil {
		err = fmt.Errorf("%w: %w", ErrStopping, err)
	}
	s.verifies.mu.Lock()
	v := s.verifies.byID[id]
	if v != nil {
		v.Ended, v.Report, v.Err, v.State = s.now(), report, err, VerifyDone
		if err != nil {
			v.State = VerifyFailed
		}
	}
	s.verifies.mu.Unlock()
	if err != nil {
		s.log.Warn("code_verify_failed", "repo", report.Name, "verify", id, "repair", repair,
			"problems", len(report.Problems), "error", err)
		return
	}
	s.log.Info("code_verify_finished", "repo", report.Name, "verify", id, "repair", repair,
		"packs", report.Packs, "problems", len(report.Problems), "rebuilt", report.Rebuilt)
}

// Verification answers the verification id of the repository named name,
// running or ended, while this node keeps it: until VerifyKept after it
// ended, or until the next verify of the repository begins, and never across
// a restart. False for one this node does not keep, or one of another
// repository.
func (s *Server) Verification(name string, id uuid.UUID) (Verification, bool) {
	s.verifies.mu.Lock()
	defer s.verifies.mu.Unlock()
	s.verifies.sweep(s.now())
	v, ok := s.verifies.byID[id]
	if !ok || v.Repo != name {
		return Verification{}, false
	}
	return *v, true
}

package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A VERIFY OUTLIVES ITS REQUEST: it reads every byte of a repository's cache
// (every pack's hash, then git's connectivity check) and may repair and check
// again, minutes for a large repository (cache.BenchmarkVerify): past a
// balancer's read timeout (nginx's default is 60 s), whose close would cancel
// a request-bound check at the same point on every retry. So
// [Server.StartVerify] refuses at once what needs no read of the repository
// (the name, the copy held, a maintenance slot, the check's memory), then runs
// the check on a goroutine of the HOST'S lifetime and hands back an id that
// [Server.Verification] answers while the node keeps it.
//
// One verification a repository at a time on a node: one with the same repair
// setting joins the running one; the other is refused ([VerifyRunningError]).

// VerifyKept is how long an ended verification is kept for its caller to read:
// an hour, so a client that lost its connection while the check ran comes
// back for the answer rather than read the repository again, while a report
// of a cache pushes keep moving is still recent. Memory is not the bound: a
// node keeps only each repository's latest. A restart keeps none, since a
// report checked a cache this process held.
const VerifyKept = time.Hour

// VerifyHold is how long the request that begins a verify waits for the check
// to end (AwaitVerification), so a small copy's check is answered in that one
// request rather than after a wait the client must guess.
//
// A QUARTER OF A SECOND answers at once a copy of up to some 25 MiB
// (cache.BenchmarkVerify: about 9.4 ms a MiB), most of a company's
// repositories, and delays a longer check's answer by only that, far inside
// every balancer's read timeout. With no hold, a sweep's client slept about
// ten times each small check before reading it, the node's slot idle.
const VerifyHold = 250 * time.Millisecond

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
	// Repair: what the check finds is repaired — in place, or by a rebuild
	// (cache.VerifyRun.Repair) — and the copy checked again.
	Repair bool
	// State is where it is; Started and Ended (zero while it runs) when.
	State          VerifyState
	Started, Ended time.Time
	// Report is what the check found: everything once done; once failed,
	// what it found before, the repository named whenever it was known.
	Report cache.VerifyReport
	// Err is why a failed verification failed, else nil: ErrStopping
	// wrapping what the cut left when the host's stop cut it short.
	Err error
	// Deadline is, while a check pass runs, when this node ends it: the
	// pass's bound (cache.CheckBudget over what the copy holds) from its
	// start. Zero while the copy is brought to the rows it is checked against
	// or repaired (each object-store read bounded on its own:
	// objstore.ReadBudget, objstore.Stall), and once ended.
	Deadline time.Time

	// ended is closed once the verification has ended (AwaitVerification).
	ended chan struct{}
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
//
// AGED FROM THE FRONT OF A QUEUE, never by a walk: ended holds ids in the
// order they ended (each Ended stamped under mu as it is appended), so the
// expired ones are a prefix, and a sweep looks at them and the first one
// still kept. A walk of the registry at every StartVerify and read, under the
// lock every check's end takes, made a sweep of a node's repositories
// quadratic in them (TestAVerifySweepLooksOnlyAtWhatItAges). An id a newer
// verification replaced is gone from byID and passed over.
type verifications struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*Verification
	latest map[string]uuid.UUID // a repository's id → its latest verification
	ended  []uuid.UUID          // every ended verification, the oldest end first
}

// begin keeps v as its repository's latest verification, dropping the one it
// replaces; mu is held.
func (r *verifications) begin(v *Verification) {
	if r.byID == nil {
		r.byID, r.latest = map[uuid.UUID]*Verification{}, map[string]uuid.UUID{}
	}
	if previous, ok := r.latest[v.RepoID]; ok {
		delete(r.byID, previous)
	}
	r.byID[v.ID], r.latest[v.RepoID] = v, v.ID
}

// end records that the verification id ended at, set filling in how; mu is
// held. One a newer verification of its repository already replaced is
// nothing to record.
func (r *verifications) end(id uuid.UUID, at time.Time, set func(*Verification)) {
	v := r.byID[id]
	if v == nil {
		return
	}
	v.Ended = at
	set(v)
	r.ended = append(r.ended, id)
}

// sweep drops every verification that ended more than VerifyKept before
// now, answering how many it looked at (those dropped or passed over, and the
// first one still kept); mu is held. It runs at every StartVerify and read,
// so aging needs no goroutine of its own.
func (r *verifications) sweep(now time.Time) int {
	looked := 0
	for len(r.ended) > 0 {
		looked++
		id := r.ended[0]
		v, ok := r.byID[id]
		if ok && now.Sub(v.Ended) <= VerifyKept {
			break
		}
		r.ended = r.ended[1:]
		if ok {
			delete(r.byID, id)
			if r.latest[v.RepoID] == id {
				delete(r.latest, v.RepoID)
			}
		}
	}
	return looked
}

// StartVerify begins a check of this node's cache of the repository name,
// repairing what it finds when repair is set (cache.Verification.Run), and
// answers it RUNNING for [Server.Verification] to read. It refuses at once,
// having read nothing of the repository: gitlet.ErrRepoNotFound (read stale,
// then through the barrier, since a refusal is never stale); cache.ErrNotHeld
// (a check builds no copy, a whole download); gitproc.ErrBusy or
// cache.ErrNoMemory; [*VerifyRunningError] beside a running verify of the
// other repair setting (the same setting shares it); ErrStopping. The caller's
// request ending ends nothing; Stop fails the check with ErrStopping. A floor
// (a code-log position) is what the copy is resolved at and brought to, with
// no barrier; none appends one at the head, so a sweep names its listing's
// position for each repository rather than append a barrier a repository.
func (s *Server) StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (
	Verification, error) {
	if !gitref.ValidRepoName(name) {
		return Verification{}, gitlet.ErrRepoNotFound
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

	if !floor.IsZero() && floor.Stream != topics.CodeLogStream {
		return Verification{}, fmt.Errorf("%w: a verify's floor is on %s, and a repository's rows are on %s",
			gitlet.ErrInvalid, floor.Stream, topics.CodeLogStream)
	}
	ref, err := s.resolveVerified(ctx, name, floor)
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
	// repository are one verification; the admission waits for nothing
	// (cache.AdmitVerify).
	checked, err := s.o.Cache.AdmitVerify(ref.ID)
	if err != nil {
		return Verification{}, err
	}
	v := &Verification{ID: uuid.Must(uuid.NewV7()), Repo: ref.Name, RepoID: ref.ID, Repair: repair,
		State: VerifyRunning, Started: now, ended: make(chan struct{})}
	s.verifies.begin(v)
	started = true
	fresh := statelog.Freshness{Level: statelog.ReadLinearizable}
	if !floor.IsZero() {
		fresh = statelog.FloorAt(statelog.ReadSession, floor)
	}
	go s.runVerify(v.ID, v.ended, checked, cache.VerifyRun{Repair: repair, Fresh: fresh})
	return *v, nil
}

// resolveVerified is the repository a verify names, read stale at the
// caller's floor, under ctx joined to the host's lifetime so Stop reaches the
// read; ErrStopping, wrapping what the cut left, when the stop rather than
// the caller cut it short.
func (s *Server) resolveVerified(ctx context.Context, name string, floor statelog.Position) (gitlet.RepoRef, error) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The host's lifetime ends the read too: Stop must reach it.
	defer context.AfterFunc(s.life, cancel)()
	ref, err := s.o.Estate.Resolve(work, gitlet.ResolveQuery{Name: name,
		Fresh: statelog.FloorAt(statelog.ReadStale, floor)})
	if errors.Is(err, gitlet.ErrRepoNotFound) {
		ref, err = s.o.Estate.Resolve(work, gitlet.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	if err != nil && s.life.Err() != nil && ctx.Err() == nil {
		return gitlet.RepoRef{}, fmt.Errorf("%w: %w", ErrStopping, err)
	}
	return ref, err
}

// runVerify runs one verification's check on a goroutine of the host's
// lifetime, counted in the work Stop waits for, records how it ended, then
// closes ended. how carries repair and freshness; Checking is set here.
//
// WHAT THE CHECK HELD IS LET GO BEFORE ITS END IS RECORDED: a caller reading
// it done asks for the next at once, and finding this slot still held would
// be refused busy for BusyRetryAfter, ten seconds a repository of a sweep.
func (s *Server) runVerify(id uuid.UUID, ended chan struct{}, checked *cache.Verification, how cache.VerifyRun) {
	defer s.inflight.Done()
	// Each check pass publishes its deadline, so a caller reading the
	// verification waits by the node's own bound.
	checking := func(bound time.Duration) {
		s.verifies.mu.Lock()
		defer s.verifies.mu.Unlock()
		if v := s.verifies.byID[id]; v != nil {
			v.Deadline = time.Time{}
			if bound > 0 {
				v.Deadline = s.now().Add(bound)
			}
		}
	}
	how.Checking = checking
	//nolint:contextcheck // the host's lifetime, never a request's: see the file's doc
	report, err := checked.Run(s.life, how)
	checked.Release()
	if err != nil && s.life.Err() != nil {
		err = fmt.Errorf("%w: %w", ErrStopping, err)
	}
	s.verifies.mu.Lock()
	s.verifies.end(id, s.now(), func(v *Verification) {
		v.Report, v.Err, v.State, v.Deadline = report, err, VerifyDone, time.Time{}
		if err != nil {
			v.State = VerifyFailed
		}
	})
	s.verifies.mu.Unlock()
	close(ended)
	if err != nil {
		s.log.Warn("code_verify_failed", "repo", report.Name, "verify", id, "repair", how.Repair,
			"repaired_by_sync", len(report.RepairedBySync), "problems", len(report.Problems), "error", err)
		return
	}
	s.log.Info("code_verify_finished", "repo", report.Name, "verify", id, "repair", how.Repair,
		"packs", report.Packs, "repaired_by_sync", len(report.RepairedBySync), "problems", len(report.Problems),
		"repaired_in_place", report.RepairedInPlace, "rebuilt", report.Rebuilt)
}

// AwaitVerification answers the verification id of the repository named name
// once it has ended, or as it stands when ctx (the beginning request, held for
// VerifyHold) ends first. False for one this node does not keep, or one of
// another repository.
func (s *Server) AwaitVerification(ctx context.Context, name string, id uuid.UUID) (Verification, bool) {
	s.verifies.mu.Lock()
	v, ok := s.verifies.byID[id]
	if !ok || v.Repo != name {
		s.verifies.mu.Unlock()
		return Verification{}, false
	}
	ended := v.ended
	s.verifies.mu.Unlock()
	select {
	case <-ended:
	case <-ctx.Done():
	}
	return s.Verification(name, id)
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

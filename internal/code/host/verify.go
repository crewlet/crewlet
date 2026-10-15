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
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A VERIFY IS AN OPERATION THAT OUTLIVES ITS REQUEST. An operator's check of
// this node's cache of a repository reads every byte of it — a hash of every
// pack, then git's connectivity check over the whole history — and with a
// repair rebuilds and checks again: about nine seconds a gigabyte measured
// (cache.BenchmarkVerify, the connectivity check most of it), twice that with
// a repair, minutes for a large repository. Answered in one request, the check
// outlived every
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

// VerifyHold is how long the request that begins a verify may wait for the
// check to end, so a check of a small copy is answered in the one request
// that asked for it (AwaitVerification) rather than read again after a
// wait the client has to guess.
//
// A QUARTER OF A SECOND. cache.BenchmarkVerify measured a whole check at
// 9.5 ms over five pushes of 150 KB and 9.4 ms a mebibyte over 100 MB, so
// a quarter of a second answers at once a copy of up to some 25 MiB — most
// of a company's repositories — while a check that runs longer is answered
// running a quarter of a second later than it would have been, far inside
// every balancer's read timeout. Answered at once instead, each small
// repository of a sweep cost its client a sleep about ten times the check
// before its first read, the node's slot idle meanwhile.
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
	// Deadline is, while it checks the copy, when this node ends that check
	// by: the bound of the check pass running (cache.CheckBudget over what
	// the copy holds), counted from the pass's start. Zero while it brings
	// the copy to the rows it is checked against first, or rebuilds it for
	// a repair — whose reads of the object store are bounded each on its
	// own (objstore.ReadBudget, objstore.ReadStall), as every sync's are —
	// and once it has ended.
	Deadline time.Time

	// ended is closed once the verification has ended: what
	// AwaitVerification waits on.
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
// AGED FROM THE FRONT OF A QUEUE, never by a walk: ended holds every
// verification in the order it ended — the order of its Ended, each stamped
// under mu as it is appended — so the ones past VerifyKept are a prefix of
// it, and a sweep looks at them and at the first one still kept, and at
// nothing else. A sweep of every repository a node holds keeps a
// verification for each, and aged by a walk of the whole registry at every
// StartVerify and every read of one, under the lock every check's end takes
// too, that sweep cost the node a number of map visits quadratic in what it
// held — some 10^8 at ten thousand repositories. An id a newer verification
// of its repository replaced is no longer in byID, and is passed over when
// the queue reaches it.
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
// now, answering how many it looked at — those it dropped or passed over,
// and the first one still kept; mu is held. Run at every StartVerify and
// every read of a verification, so what a node keeps needs no goroutine of
// its own to age.
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
//
// AT A FLOOR ITS CALLER NAMES, or at the log's head. A floor — a position on
// the code log the caller read at — is what the repository is resolved at
// and its copy brought to before it is checked, with no barrier: a copy
// already past it is checked as it is. With none, the copy is brought to the
// log's head, which appends a barrier to the code log — single-flighted only
// among requests in flight together, so an operator's sweep of every
// repository one at a time appended one a repository: ten thousand a sweep
// at config.CodeMaxRepos, on every git node it ran on, a log whose census
// counts a day's barriers as its people's git reads. A sweep reads its
// listing at the head once and names that listing's position for each.
func (s *Server) StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (
	Verification, error) {
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

	if !floor.IsZero() && floor.Stream != topics.CodeLogStream {
		return Verification{}, fmt.Errorf("%w: a verify's floor is on %s, and a repository's rows are on %s",
			code.ErrInvalid, floor.Stream, topics.CodeLogStream)
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
	// repository are one verification rather than two admitted beside each
	// other; the admission waits for nothing (cache.AdmitVerify).
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
		fresh = statelog.Freshness{Level: statelog.ReadSession, MinPosition: floor}
	}
	go s.runVerify(v.ID, v.ended, checked, cache.VerifyRun{Repair: repair, Fresh: fresh})
	return *v, nil
}

// resolveVerified is the repository a verify names — read stale, at the
// caller's floor where it names one — under ctx joined to the host's
// lifetime — a Stop reaches the read — and ErrStopping, wrapping what the cut
// left, when the stop cut it short rather than its caller.
func (s *Server) resolveVerified(ctx context.Context, name string, floor statelog.Position) (code.RepoRef, error) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The host's lifetime ends the read too: Stop must reach it.
	defer context.AfterFunc(s.life, cancel)()
	ref, err := s.o.Estate.Resolve(work, code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale, MinPosition: floor}})
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
// record of how it ended — ended closed once that is recorded. how carries
// its repair and its freshness; its Checking is this function's own.
//
// WHAT THE CHECK HELD IS LET GO BEFORE ITS END IS RECORDED: a caller that
// reads the verification done — or is answered it by the request that began
// it — asks for the next one at once, and finding this one's maintenance slot
// still held it would be refused busy and told to wait BusyRetryAfter, ten
// seconds of a sweep's for each repository, for a slot freed microseconds
// later.
func (s *Server) runVerify(id uuid.UUID, ended chan struct{}, checked *cache.Verification, how cache.VerifyRun) {
	defer s.inflight.Done()
	// WHEN THE NODE ENDS WHAT IT IS DOING is said as each check pass begins,
	// so a caller reading the verification waits by the node's own bound.
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
			"problems", len(report.Problems), "error", err)
		return
	}
	s.log.Info("code_verify_finished", "repo", report.Name, "verify", id, "repair", how.Repair,
		"packs", report.Packs, "problems", len(report.Problems), "rebuilt", report.Rebuilt)
}

// AwaitVerification answers the verification id of the repository named name
// once it has ended, or as it stands when ctx ends first — the request that
// began it, held for VerifyHold, so a small check is answered where it was
// asked. False for one this node does not keep, or one of another
// repository.
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

package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// requestBeside is what an engine write's request holds beside its payload at
// the longest the engine stamps every other field of it: the run's work item
// and model at code.MaxProvenanceBytes of a character JSON spells in six
// bytes, a branch at the 255 bytes a ref may be of the same, a seat's handle
// at iam.MaxLogin, an operation id at statelog.MaxCallerOpIDBytes, the field
// names, the request's own fields and its signed frame — some 6.8 KiB as
// measured (TestAnEngineWriteTheRuleAdmitsCrossesTheWireAtItsBound: 6 908
// bytes beside a commit_files' payload, 6 945 beside a revert's), held to
// SIXTEEN, over twice that: what a slot of a git node's writes' lane holds of
// a write's request is the payload's bound and this beside it.
const requestBeside = 16 << 10

// sizer is a broker registration that records the size of every request each
// of its subjects was sent, as the serving node's broker client holds it.
type sizer struct {
	q *memory.Queue

	mu    sync.Mutex
	sizes map[string][]int
}

func (s *sizer) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	return s.q.Serve(ctx, subject, answers, func(ctx context.Context, raw []byte) ([]byte, error) {
		s.mu.Lock()
		if s.sizes == nil {
			s.sizes = map[string][]int{}
		}
		s.sizes[subject] = append(s.sizes[subject], len(raw))
		s.mu.Unlock()
		return h(ctx, raw)
	})
}

// sent is the size of every request subject was sent, in order.
func (s *sizer) sent(subject string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sizes[subject])
}

// largestWrite is the largest engine write of kind — a commit_files or a
// revert — that code.EngineWrite.Validate admits: its payload's message of
// '<', which JSON spells in six bytes, filled to the bound exactly, and every
// other field at the longest the engine stamps it. The rule is the judge of
// "largest": the write is admitted, and the same write one '<' longer is
// refused.
func largestWrite(t *testing.T, kind code.EngineWriteKind) code.EngineWrite {
	t.Helper()
	widest := strings.Repeat("&", code.MaxProvenanceBytes)
	w := code.EngineWrite{Kind: kind, RepoID: repoID(7),
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: strings.Repeat("s", iam.MaxLogin),
			Run: code.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d", WorkBackend: widest,
				WorkKey: widest, Model: widest}},
		OpID: statelog.NewOpID(time.Now(), strings.Repeat("n", statelog.MaxCallerOpIDBytes-37)),
		Fresh: statelog.Freshness{Level: statelog.ReadSession,
			Floors: []statelog.Position{{Stream: topics.CodeLogStream, Generation: math.MaxUint32, Seq: math.MaxUint64}}},
		Ref:          "refs/heads/" + strings.Repeat("&", gitref.MaxRefNameBytes-len("refs/heads/")),
		ExpectedHead: "1111111111111111111111111111111111111111",
	}
	room := code.MaxCommitPayloadBytes
	switch kind {
	case code.WriteCommitFiles:
		one := "one\n"
		w.Files = []code.FileOp{{Op: code.FileOpPut, Path: "README.md", Content: &one}}
		encoded, err := json.Marshal(w.Files)
		if err != nil {
			t.Fatal(err)
		}
		room -= len(encoded)
	case code.WriteRevert:
		w.Commit = "2222222222222222222222222222222222222222"
	default:
		t.Fatalf("no largest %s", kind)
	}
	// The message JSON spells in room bytes exactly: as many '<' as fit
	// beside its quotes, and the rest in plain bytes.
	n := (room - 2) / 6
	w.Message = strings.Repeat("<", n) + strings.Repeat("x", room-2-6*n)
	if err := w.Validate(); err != nil {
		t.Fatalf("the largest %s, at the payload's bound as JSON spells it: %v, want it admitted", kind, err)
	}
	past := w
	past.Message = "<" + w.Message
	if err := past.Validate(); !errors.Is(err, code.ErrInvalid) {
		t.Fatalf("the %s one '<' past the payload's bound as JSON spells it: %v, want code.ErrInvalid — "+
			"the rule measures what crosses the wire", kind, err)
	}
	return w
}

// AN ENGINE WRITE THE ONE RULE ADMITS CROSSES THE WIRE, ITS REQUEST THE
// PAYLOAD'S BOUND AND A FEW KIBIBYTES BESIDE IT: the largest write
// code.EngineWrite.Validate admits — a commit_files' or a revert's, its
// message of '<', which JSON spells in six bytes, filled to the bound and one
// '<' more refused, every other field at the longest the engine stamps — is
// carried to a peer git node and built, its request as that node's broker
// client holds it under queue.MaxPayloadBytes and within requestBeside of
// code.MaxCommitPayloadBytes: what a slot of a git node's writes' lane is
// sized by. Measured by its length, a commit_files' message admitted at the
// bound crossed at six times it, and a revert's message at whatever the
// transport would carry — past it, refused to every peer by the transport
// while the home's own seat had it built in process.
//
// Mutation: measure the message by its length (code.EngineWrite.Validate) —
// the one-'<'-past writes are admitted, red.
func TestAnEngineWriteTheRuleAdmitsCrossesTheWireAtItsBound(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	node := &fakeNode{name: "git-a", units: chartOf("git-a")}
	f.nodes["git-a"] = node
	sized := &sizer{q: f.start(t)}
	stop, err := Serve(t.Context(), sized, ServerOptions{Self: "git-a", Local: node,
		Git: gitHalf{node: &fakeGit{name: "git-a"}}, Seams: node.seams(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.git.set(func(p *fakePlacement) { p.nodes = []string{"git-a"} })

	writes := laneGitWrites.subject("git-a")
	for _, kind := range []code.EngineWriteKind{code.WriteCommitFiles, code.WriteRevert} {
		res, err := f.client.Code().GitWrite(t.Context(), largestWrite(t, kind))
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the largest %s the rule admits, asked of a peer git node = (%+v, %v), want it built", kind,
				res, err)
		}
		sent := sized.sent(writes)
		if len(sent) == 0 {
			t.Fatalf("the largest %s reached git-a on no request of its writes' lane", kind)
		}
		size := sent[len(sent)-1]
		t.Logf("the largest %s's request is %d bytes: %d beside its payload", kind, size,
			size-code.MaxCommitPayloadBytes)
		if size > queue.MaxPayloadBytes || size > code.MaxCommitPayloadBytes+requestBeside {
			t.Errorf("the largest %s's request is %d bytes, want at most the payload's %d and %d beside it, "+
				"under the transport's %d", kind, size, code.MaxCommitPayloadBytes, requestBeside,
				queue.MaxPayloadBytes)
		}
	}
}

// heldGit is a git half that HOLDS every engine write it is asked until it is
// released — as a code host holds one through its sync, its build, its
// upload, its proposal and the wait for its push's row, the decoded write kept
// to the end — counting the writes it was asked and those it holds now.
type heldGit struct {
	release chan struct{}
	asked   atomic.Int64
	holding atomic.Int64
}

func (g *heldGit) Git() (GitServer, bool) { return g, true }

func (g *heldGit) Read(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, errors.New("this git half answers no read")
}

func (g *heldGit) MergePreview(context.Context, code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	return code.MergePreviewAnswer{}, errors.New("this git half answers no merge preview")
}

func (g *heldGit) Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	g.asked.Add(1)
	g.holding.Add(1)
	defer g.holding.Add(-1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return code.EngineWriteResult{}, &code.ErrGitUnready{Reason: code.UnreadyShare,
			Detail: "a held write's answer fell due before it was released"}
	}
	// THE DECODED WRITE, READ AFTER THE WAIT, is live through it — as the
	// code host's pipeline keeps it.
	return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, PushID: code.WritePushID(w.OpID),
		Commit:  fmt.Sprintf("%d operations, a message of %d bytes", len(w.Files), len(w.Message)),
		Command: code.Command{Ref: w.Ref, Old: w.ExpectedHead, Result: code.ResultOK}}, nil
}

// serveHeldGit serves git-a — a data node, as every git node is in this
// release — over the fleet's broker with half as its git half.
func serveHeldGit(t *testing.T, f *fleet, half *heldGit) {
	t.Helper()
	node := &fakeNode{name: "git-a", units: chartOf("git-a")}
	f.nodes["git-a"] = node
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{Self: "git-a", Local: node, Git: half,
		Seams: node.seams(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

// sealWrite is w as a peer asks it of a git node's writes' lane, signed: its
// request's id and its sealed bytes, and nothing else of it kept.
func sealWrite(w code.EngineWrite) (string, []byte, error) {
	args, err := json.Marshal(w)
	if err != nil {
		return "", nil, err
	}
	req := request{ID: signedask.NewID(), Op: opCodeGitWrite.spec.name, Args: args, From: "agent-9",
		Deadline: time.Now().Add(time.Minute)}
	sealed, err := fleetEnvelopes.sealRequest(req)
	return req.ID, sealed, err
}

// askSealed asks one sealed request of subject and reads its reply as the
// router does — verified, and bound to the request — or says why it cannot.
func askSealed(ctx context.Context, q *memory.Queue, subject, id string, sealed []byte) (reply, error) {
	replies, err := q.Ask(ctx, subject, sealed, 1)
	switch {
	case err != nil:
		return reply{}, err
	case len(replies) != 1:
		return reply{}, fmt.Errorf("nothing answered on %s", subject)
	}
	rep, why, err := fleetEnvelopes.openReply(replies[0], id)
	switch {
	case err != nil:
		return reply{}, err
	case why != "":
		return reply{}, fmt.Errorf("the reply on %s was not believed: %s", subject, why)
	}
	return rep, nil
}

// holdWrites has n writes, each asked of git-a's writes' lane by an asker of
// its own, held by half — waiting until it holds every one — and answers what
// each asker heard, once it is released, on the channel it returns, closed
// when every one has.
func holdWrites(t *testing.T, q *memory.Queue, half *heldGit, n int, write func(i int) code.EngineWrite) <-chan error {
	t.Helper()
	subject := laneGitWrites.subject("git-a")
	heard := make(chan error, n)
	var asks sync.WaitGroup
	for i := range n {
		asks.Add(1)
		go func() {
			defer asks.Done()
			id, sealed, err := sealWrite(write(i))
			if err != nil {
				heard <- err
				return
			}
			rep, err := askSealed(t.Context(), q, subject, id, sealed)
			switch {
			case err != nil:
				heard <- err
			case rep.Unserved != "" || rep.Err != nil:
				heard <- fmt.Errorf("a held write, once released, was answered %+v", rep)
			}
			// THE REQUEST AS DELIVERED IS HELD TO ITS ANSWER, as the broker
			// client's message is on a git node.
			runtime.KeepAlive(sealed)
		}()
	}
	go func() { asks.Wait(); close(heard) }()
	deadline := time.Now().Add(2 * time.Minute)
	for half.holding.Load() < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("git-a holds %d of the %d writes asked of it after two minutes", half.holding.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return heard
}

// A GIT WRITE PAST THE ONES A GIT NODE HOLDS IS ANSWERED AT ONCE: with all
// gitWritesHeld engine writes git-a holds for its peers held, the next — the
// largest write the one rule admits — is answered `behind`, at its share,
// naming the bound, its git half never asked, while every held write still
// holds its slot; and once they end, the next write is built. Left to wait
// for a slot it waited unseen in the broker client's buffer, which drops a
// request past its bytes — its asker hearing nothing for a whole write attempt
// and then taking the healthy node for a gone one.
//
// JUDGED BY WHAT WAS STILL HELD, never by a clock: the next write's asker
// waits up to a minute, and what proves it never waited for a slot is that no
// held write had let go of one when it was answered.
//
// Mutation: register the writes' lane gitWritesHeld wide and answer nothing at
// once — the next write waits for a slot until its asker gives up, red.
func TestAGitWritePastTheOnesANodeHoldsIsAnsweredAtOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	half := &heldGit{release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(half.release) }) }
	serveHeldGit(t, f, half)
	t.Cleanup(release)
	q := f.start(t)
	heard := holdWrites(t, q, half, gitWritesHeld, func(i int) code.EngineWrite { return writeOf(repoID(i)) })

	asked := half.asked.Load()
	id, sealed, err := sealWrite(largestWrite(t, code.WriteCommitFiles))
	if err != nil {
		t.Fatal(err)
	}
	next, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	begun := time.Now()
	rep, err := askSealed(next, q, laneGitWrites.subject("git-a"), id, sealed)
	took := time.Since(begun)
	if err != nil {
		t.Fatalf("with every write git-a holds held, the next = %v, want it answered at once", err)
	}
	t.Logf("the largest write past the held ones was answered in %v", took)
	if rep.Unserved != unservedBehind || rep.Unready != code.UnreadyShare ||
		!strings.Contains(rep.Detail, "gitWritesHeld") {
		t.Errorf("the write past the held ones was answered %+v, want behind, at its share, naming the bound", rep)
	}
	if n, held := half.asked.Load(), half.holding.Load(); n != asked || held != gitWritesHeld {
		t.Errorf("answering the write past the held ones, git-a's git half was asked %d writes more and holds %d, "+
			"want none more and all %d still held", n-asked, held, gitWritesHeld)
	}

	release()
	for err := range heard {
		t.Error(err)
	}
	id, sealed, err = sealWrite(writeOf(repoID(gitWritesHeld)))
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := askSealed(t.Context(), q, laneGitWrites.subject("git-a"), id, sealed); err != nil ||
		rep.Unserved != "" || rep.Err != nil {
		t.Errorf("a write once the held ones ended = (%+v, %v), want it built", rep, err)
	}
}

// heldBeside bounds what a held write's slot holds beside its three copies of
// the payload: each copy's other fields (requestBeside at their longest), the
// allocator's rounding of each copy — a large object — up to its next 8 KiB
// page, the slot's context, timer and channels (here its asker's too, which
// in a fleet are on another node) and its replay-guard entry. At most some
// 88 KiB as measured under -race (TestWhatAHeldGitWriteHolds: 2.96 and 3.09
// payloads a slot over two runs, the heap between its two readings moving by
// some 130 KiB a slot from one run to the next), held to a QUARTER OF A
// PAYLOAD: room for what is beside the copies and that noise, and none for a
// fourth.
const heldBeside = code.MaxCommitPayloadBytes / 4

// WHAT A HELD GIT WRITE HOLDS IS MEASURED, not assumed: a git node holds, in
// each slot of its writes' lane a peer's write takes, the write's request as
// its broker client delivered it, the request as admitted — its arguments
// copied out of it — and the write decoded, which its git half keeps through
// the sync, the build, the upload, the proposal and the wait for the push's
// row. At the payload's bound, in plain bytes — which JSON spells as long as
// their text, so the decoded copy is as large as the encoding: the most a
// write at the bound holds — a slot holds three payloads and at most
// heldBeside beside them: what the writes' lane's doc states of a full lane
// ([lane.answers]).
//
// NOT PARALLEL, since it reads the heap: no other case allocates beside it.
//
// Mutation: keep a fourth copy of a held write's arguments in its slot — a
// slot passes three payloads and heldBeside, red.
func TestWhatAHeldGitWriteHolds(t *testing.T) {
	const slots = 16
	f := newFleet(t)
	half := &heldGit{release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(half.release) }) }
	serveHeldGit(t, f, half)
	t.Cleanup(release)
	q := f.start(t)
	// THE CONTENT THAT FILLS THE BOUND, beside the rest of one write's
	// files and its message, in plain bytes.
	empty := ""
	base := writeOf(repoID(0))
	base.Files = []code.FileOp{{Op: code.FileOpPut, Path: "README.md", Content: &empty}}
	files, err := json.Marshal(base.Files)
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(base.Message)
	if err != nil {
		t.Fatal(err)
	}
	fill := code.MaxCommitPayloadBytes - len(files) - len(message)
	plain := func(i int) code.EngineWrite {
		w := writeOf(repoID(i))
		content := strings.Repeat("x", fill)
		w.Files = []code.FileOp{{Op: code.FileOpPut, Path: "README.md", Content: &content}}
		return w
	}
	if err := plain(0).Validate(); err != nil {
		t.Fatalf("a write at the payload's bound in plain bytes: %v, want it admitted", err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	heard := holdWrites(t, q, half, slots, plain)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	release()
	for err := range heard {
		t.Error(err)
	}

	slot := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / slots
	t.Logf("a held write at the payload's bound holds %d bytes of the heap, %.2f payloads", slot,
		float64(slot)/code.MaxCommitPayloadBytes)
	if limit := int64(3*code.MaxCommitPayloadBytes + heldBeside); slot > limit {
		t.Errorf("a held write at the payload's bound holds %d bytes of the heap, past the %d of three payloads "+
			"— its request as delivered, as admitted, and decoded — and heldBeside beside them", slot, limit)
	}
}

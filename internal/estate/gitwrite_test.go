package estate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
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

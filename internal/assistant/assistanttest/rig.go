package assistanttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
)

// Rig is one suite's assistant: a real [assistant.Service] over this
// package's fakes, a person, and a conversation of theirs.
type Rig struct {
	t            testing.TB
	Svc          *assistant.Service
	Clock        *Clock
	Store        *Store
	Model        *Model
	Dispatcher   *Dispatcher
	Revoker      *Revoker
	Audit        *Audit
	Person       iam.Principal
	Conversation uuid.UUID
}

// RigOptions vary one rig.
type RigOptions struct {
	Rounds   []Round
	Tools    []catalog.Tool
	Handler  func(exec.Request) exec.Response
	Budget   Budget
	Models   *Models
	Off      bool
	Proposer assistant.Proposer
	// Company replaces the rig's company, for a suite about one the
	// assistant cannot run on.
	Company assistant.Company
}

// NewRig builds a rig and closes its service when the suite ends. Its person is
// grace, holding the tracker, on the human seat grace; ada is a second human
// seat.
func NewRig(t testing.TB, o RigOptions) *Rig {
	t.Helper()
	clock := NewClock()
	r := &Rig{t: t, Clock: clock, Store: NewStore(clock.Now), Model: NewModel(o.Rounds...),
		Dispatcher: &Dispatcher{Handler: o.Handler}, Revoker: &Revoker{}, Audit: &Audit{},
		Person:       Person("grace", "grace", iam.GrantTracker),
		Conversation: ID("conversation/one"),
	}
	models := Models{Model: r.Model, Key: "assistant"}
	if o.Models != nil {
		models = *o.Models
	}
	company := o.Company
	if company == nil {
		company = Company(t, !o.Off, "grace", "ada")
	}
	svc, err := assistant.New(assistant.Deps{
		Node: "node-a", Company: company,
		Models: models, Budget: o.Budget, Catalogue: Catalogue(o.Tools),
		Cards:       Cards{Title: "People", Columns: []string{"name"}},
		Transcripts: r.Store, Records: r.Store, Dispatcher: r.Dispatcher, Revoker: r.Revoker,
		Audit: r.Audit, Metrics: Metrics{}, Proposer: o.Proposer, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	r.Svc = svc
	r.Store.Seed(r.Person.ID, r.Conversation, "")
	return r
}

// Grant is a fresh read grant for one question, valid for five minutes, with
// a sentinel token planted to prove it never reaches anything.
func (r *Rig) Grant(name string) Grant {
	return Grant{GrantID: ID("grant/" + name), ExpiresAt: r.Clock.Now().Add(5 * time.Minute),
		Token: "cwl_ast_SENTINEL_" + name}
}

// Key is a stable operation key for one question, in the shape
// internal/api/opkey scopes one to a person.
func Key(name string) string { return "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12.k" + name }

// Ask starts a turn of the rig's person under key name.
func (r *Rig) Ask(name, text string, page *wire.PageContext) (wire.TurnAccepted, error) {
	r.t.Helper()
	return r.Svc.Start(context.Background(), assistant.TurnStart{Person: r.Person,
		Conversation: r.Conversation, Key: Key(name), Input: text, Page: page,
		Read: r.Grant(name)})
}

// Collector is a stream as a suite reads it, refusing a frame the protocol
// would.
type Collector struct {
	mu     sync.Mutex
	frames []wire.Frame
}

var _ assistant.FrameWriter = (*Collector)(nil)

func (c *Collector) Frame(fr wire.Frame) error {
	if err := fr.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, fr)
	return nil
}

func (c *Collector) Keepalive() error { return nil }

// Frames is every frame drawn.
func (c *Collector) Frames() []wire.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Frame(nil), c.frames...)
}

// Follow reads a turn's stream from after seq after until it ends.
func (r *Rig) Follow(turn uuid.UUID, after uint64) []wire.Frame {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Collector{}
	if err := r.Svc.Stream(ctx, r.Person.ID, r.Conversation, turn, after, c); err != nil {
		r.t.Fatalf("stream: %v", err)
	}
	if ctx.Err() != nil {
		r.t.Fatal("the stream never ended")
	}
	return c.Frames()
}

// Last is the final frame of kind in frames, or nil.
func Last(frames []wire.Frame, kind wire.EventKind) *wire.Frame {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Kind == kind {
			return &frames[i]
		}
	}
	return nil
}

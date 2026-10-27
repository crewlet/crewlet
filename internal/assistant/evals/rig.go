package evals

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Rig is one assistant: a real [assistant.Service] over this package's
// stand-ins (or the App's own dispatcher and delegations), a person, and a
// conversation of theirs. Suites wrap it in assistanttest.Rig; an evaluation
// case runs on it directly.
type Rig struct {
	Svc          *assistant.Service
	Clock        *Clock
	Store        *Store
	Model        *Model
	Dispatcher   *Dispatcher
	Revoker      *Revoker
	Audit        *Audit
	Person       iam.Principal
	Conversation uuid.UUID
	// Files is the store the rig's service keeps files in, nil for none.
	Files assistant.Files
	// sessions is where the rig's turns serve their tools to the scripted
	// model's CLI, removed by Close.
	sessions *toolsession.Host
}

// RigOptions vary one rig.
type RigOptions struct {
	Rounds  []Round
	Tools   []catalog.Tool
	Handler func(exec.Request) exec.Response
	Budget  Budget
	Models  *Models
	Off     bool
	// Company replaces the rig's company, for a suite about one the
	// assistant cannot run on.
	Company assistant.Company
	// Acts are the writes the rig's person may propose.
	Acts []string
	// Catalogue replaces the fixed Tools: catalog.Curated offers each
	// person the curated table as a running node does.
	Catalogue assistant.Catalogue
	// Person replaces grace, who holds Worklet on the seat grace.
	Person *iam.Principal
	// Dispatcher and Revoker replace the recording fakes with the real
	// grant machinery (internal/api/auth's Dispatcher and Delegations), for
	// a suite that drives the App's own handler; the rig's Dispatcher and
	// Revoker are then nil, and its questions carry a grant derived through
	// the App's guard ([Rig.AskWith]).
	Dispatcher exec.Dispatcher
	Revoker    assistant.Revoker
	// Node is the rig's node id; empty is "node-a".
	Node string
	// Store is shared with another rig, for a suite about two nodes of one
	// fleet reading one replicated estate; nil is a store of the rig's own.
	Store *Store
	// Remote and Rate are the rig's fleet half and the fleet's turn
	// counter; nil serves this node's own turns and counts them here.
	Remote assistant.Remote
	Rate   assistant.TurnRate
	// Files is the assistant's file store: attachments, delivered files and
	// the files a draft change names; nil keeps none.
	Files assistant.Files
	// Deep runs deep work; nil offers none. A rig given one and no Files
	// keeps its files in a [Files] of its own.
	Deep assistant.DeepWork
}

// Assemble builds a rig over o. Its person is grace, holding Worklet, on
// the human seat grace, unless o names another; o.Company is required. The
// one construction every suite (through assistanttest.NewRig) and every
// evaluation case share, so a case runs on exactly the service a suite does.
func Assemble(o RigOptions) (*Rig, error) {
	if o.Company == nil {
		return nil, errors.New("evals: RigOptions.Company is required")
	}
	clock := NewClock()
	store := o.Store
	if store == nil {
		store = NewStore(clock.Now)
	}
	node := o.Node
	if node == "" {
		node = "node-a"
	}
	r := &Rig{Clock: clock, Store: store, Model: NewModel(o.Rounds...),
		Audit: &Audit{}, Person: Person("grace", "grace", iam.GrantWorklet),
		Conversation: ID("conversation/one"),
	}
	if o.Person != nil {
		r.Person = *o.Person
	}
	dispatcher := o.Dispatcher
	if dispatcher == nil {
		r.Dispatcher = &Dispatcher{Handler: o.Handler}
		dispatcher = r.Dispatcher
	}
	revoker := o.Revoker
	if revoker == nil {
		r.Revoker = &Revoker{}
		revoker = r.Revoker
	}
	models := Models{Model: r.Model, Key: "assistant"}
	if o.Models != nil {
		models = *o.Models
	}
	files := o.Files
	if files == nil && o.Deep != nil {
		files = NewFiles(clock.Now)
	}
	r.Files = files
	var offered assistant.Catalogue = Catalogue(o.Tools)
	if o.Catalogue != nil {
		offered = o.Catalogue
	}
	sessions, err := toolsession.NewHost()
	if err != nil {
		return nil, err
	}
	r.sessions = sessions
	svc, err := assistant.New(assistant.Deps{
		Sessions: sessions,
		Node:     node, Company: o.Company, Remote: o.Remote, Rate: o.Rate, Files: files, Deep: o.Deep,
		Acts:   func(context.Context, iam.Principal) ([]string, error) { return o.Acts, nil },
		Models: models, Budget: o.Budget, Catalogue: offered,
		Transcripts: r.Store, Records: r.Store, Dispatcher: dispatcher, Revoker: revoker,
		Audit: r.Audit, Metrics: Metrics{}, Now: clock.Now,
	})
	if err != nil {
		_ = sessions.Close()
		return nil, err
	}
	r.Svc = svc
	r.Store.Seed(r.Person.ID, r.Conversation, "")
	return r, nil
}

// Close stops every turn the rig's service runs, within ten seconds of its
// own: a run whose ctx was cancelled (a suite that ended, an operator's
// interrupt) still stops its turns, so the shutdown is detached from ctx's
// cancellation and keeps only its values.
func (r *Rig) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := r.Svc.Close(ctx)
	if r.sessions != nil {
		err = errors.Join(err, r.sessions.Close())
	}
	return err
}

// Grant is a fresh read grant for one question, valid for five minutes, with
// a sentinel token planted to prove it never reaches anything.
func (r *Rig) Grant(name string) Grant {
	return Grant{GrantID: ID("grant/" + name), ExpiresAt: r.Clock.Now().Add(5 * time.Minute),
		Token: "cwl_ast_SENTINEL_" + name}
}

// Ask starts a turn of the rig's person under key name, on a fake read grant.
func (r *Rig) Ask(name, text string, page *wire.PageContext) (wire.TurnAccepted, error) {
	return r.AskWith(name, text, page, r.Grant(name))
}

// AskWith starts a turn of the rig's person under key name on the read grant
// given: a real one derived through the App's guard, when the rig sends
// through the App's own dispatcher.
func (r *Rig) AskWith(name, text string, page *wire.PageContext, read exec.Grant) (
	wire.TurnAccepted, error) {

	return r.Svc.Start(context.Background(), assistant.TurnStart{Person: r.Person,
		Conversation: r.Conversation, Key: Key(name), Input: text, Page: page, Read: read})
}

// Brief asks for a brief ([wire.TurnBrief]) of page under key name, on a fake
// read grant.
func (r *Rig) Brief(name, text string, page *wire.PageContext) (wire.TurnAccepted, error) {
	return r.Svc.Start(context.Background(), assistant.TurnStart{Person: r.Person,
		Conversation: r.Conversation, Key: Key(name), Input: text, Page: page,
		Read: r.Grant(name), Mode: wire.TurnBrief})
}

// Collector is a stream as a suite reads it, refusing a frame the protocol
// would.
type Collector struct {
	mu     sync.Mutex
	frames []wire.Frame
}

var _ assistant.FrameWriter = (*Collector)(nil)

// Frame implements [assistant.FrameWriter].
func (c *Collector) Frame(fr wire.Frame) error {
	if err := fr.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, fr)
	return nil
}

// Keepalive implements [assistant.FrameWriter].
func (c *Collector) Keepalive() error { return nil }

// Frames is every frame drawn.
func (c *Collector) Frames() []wire.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Frame(nil), c.frames...)
}

// Await reads a turn's stream from after seq after until it ends, or fails
// when it has not ended within patience or ctx ends first.
func (r *Rig) Await(ctx context.Context, turn uuid.UUID, after uint64, patience time.Duration) (
	[]wire.Frame, error) {

	ctx, cancel := context.WithTimeout(ctx, patience)
	defer cancel()
	c := &Collector{}
	if err := r.Svc.Stream(ctx, r.Person.ID, r.Conversation, turn, after, c); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("the stream never ended within %s", patience)
	}
	return c.Frames(), nil
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

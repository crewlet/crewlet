package cdp

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// AttachOptions is what [AutoAttach] does with each target it holds.
type AttachOptions struct {
	// Filter is the parent's Target.setAutoAttach filter: which types of
	// target it attaches. Nil or empty is Chromium's own default, every
	// type but "browser" and "tab": an empty filter is sent as none at all.
	// One that takes a tab is refused (AutoAttach says why).
	Filter []TargetFilterEntry
	// ChildFilter is every attached child's filter, in turn: which types a
	// page, a frame or a worker attaches of its own. Nil or empty is Chromium's
	// default without a service worker: every type but "browser", "tab" and
	// "service_worker". A filter that attaches nothing says so in an entry,
	// {Exclude: true}, since an empty one is sent as none at all, which is
	// Chromium's default.
	//
	// It is apart from Filter because Chromium announces a service worker at the
	// browser's level and again at every page, and a target must be held by ONE
	// session: held by two it starts only once both resume it (measured on both
	// pins). So Filter alone takes it, and AutoAttach refuses a ChildFilter that
	// does. A shared worker is announced at the browser's level alone.
	ChildFilter []TargetFilterEntry
	// Setup prepares one target as it is attached: the caller's request gate,
	// bindings and scripts. It SENDS them with [Send], in the order Chromium is
	// to apply them, and returns what it sent; the attacher resumes the target
	// behind them and only then waits for the answers. Setup never waits for one:
	// a held page with no renderer yet (a noopener popup) answers only what the
	// browser serves until resumed, so a wait would run to [CommandTimeout].
	// attached.WaitingForDebugger says whether Chromium holds the target; one
	// already running answers at once, so Setup may wait on it, and refuses it
	// by returning an error. Setup never makes a held target open a window (see
	// hold). It runs per child on its own goroutine, an error or nil Answer
	// stops the attacher, and it never calls [Attacher.Close] or [Attacher.Done]
	// (both wait for it). Nil sets up nothing.
	Setup func(ctx context.Context, child *Session, attached TargetAttachedToTargetEvent) ([]Answer, error)
}

// announcedTwice is every target type Chromium announces at more than one
// level, which a ChildFilter never takes (AttachOptions.ChildFilter): a
// service worker, at the browser's and again at every page it serves.
// Measured on both pinned Chromiums on 2026-11-11, auto-attach on the
// browser's session and on a page's each taking every type: the page's
// service worker was announced on both, its shared worker on the browser's
// alone.
var announcedTwice = []string{"service_worker"}

// childDefault is the filter a nil or empty ChildFilter sends: Chromium's own
// default — every type but "browser" and "tab" — without the types it
// announces at more than one level.
var childDefault = []TargetFilterEntry{
	{Type: "service_worker", Exclude: true},
	{Type: "browser", Exclude: true},
	{Type: "tab", Exclude: true},
	{},
}

// takes reports whether filter attaches a target of kind once it is sent, as
// Chromium reads the message carrying it: by its first entry that names kind
// or names no type, a target no entry matches left out. Nil or empty is sent
// as no filter at all (targetSetAutoAttachParams.Filter is omitempty), which
// is Chromium's default, every type but "browser" and "tab".
func takes(filter []TargetFilterEntry, kind string) bool {
	if len(filter) == 0 {
		return kind != "browser" && kind != "tab"
	}
	for _, e := range filter {
		if e.Type == "" || e.Type == kind {
			return !e.Exclude
		}
	}
	return false
}

// Attacher resumes no new target of a session — and, recursively, of each
// of those, an out-of-process frame's frames and a page's or a worker's
// workers — until the caller has set it up. It is [AutoAttach]'s, whose doc
// says which targets Chromium holds until then.
type Attacher struct {
	c           *Conn
	o           AttachOptions
	childFilter []TargetFilterEntry // o.ChildFilter, or childDefault for nil or empty
	cancel      context.CancelCauseFunc
	wg          sync.WaitGroup
	done        chan struct{}

	mu  sync.Mutex
	err error
}

// AutoAttach turns auto-attach on for parent, Chromium told to hold each new
// target at its start, and resumes none until o.Setup has prepared it. Per
// target it runs Setup, turns auto-attach on for the child with o.ChildFilter,
// resumes the child behind what Setup sent, then waits for Setup's answers. A
// failed Setup, or a child auto-attach Chromium refuses, leaves its target held
// and is [Attacher.Err]: the attacher stops.
//
// Held on both pins (TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp): a page,
// an out-of-process frame and any worker started once auto-attach is on. Not
// held: a target already running (refuse it in Setup) and a tab created with an
// address, announced held yet run before Setup. Why only held targets are
// resumed: hold. Goroutines end with ctx, the connection, a failed Setup or Close.
func AutoAttach(ctx context.Context, parent *Session, o AttachOptions) (*Attacher, error) {
	if takes(o.Filter, "tab") {
		// With tabs taken Chromium announces each tab's page unheld, so it
		// would never be resumed; it also attached a page's prerender held
		// before it had a renderer, where taking pages prerenders nothing.
		return nil, errors.New("cdp: AttachOptions.Filter takes tab targets: Chromium announces the page in a " +
			"tab unheld, so the attacher would never resume it and it would not run; take pages, as Chromium's " +
			"default does")
	}
	childFilter := o.ChildFilter
	if len(childFilter) == 0 {
		childFilter = childDefault
	}
	for _, kind := range announcedTwice {
		if takes(childFilter, kind) {
			return nil, fmt.Errorf("cdp: AttachOptions.ChildFilter takes %s targets, which Chromium announces at "+
				"more than one level: each page one serves would hold it, and it would start only once every one "+
				"of them resumed it; take it in Filter, on the browser's session, and leave it out of ChildFilter",
				kind)
		}
	}
	actx, cancel := context.WithCancelCause(ctx)
	a := &Attacher{c: parent.c, o: o, childFilter: childFilter, cancel: cancel, done: make(chan struct{})}
	err := a.watch(actx, parent, o.Filter)
	go func() {
		a.wg.Wait()
		close(a.done)
	}()
	if err != nil {
		cancel(err)
		<-a.done
		return nil, err
	}
	return a, nil
}

// Done is closed once the attacher has stopped and every goroutine it started
// has ended.
func (a *Attacher) Done() <-chan struct{} { return a.done }

// Err is the attacher's first failure, or nil: a Setup that failed, a
// child's own auto-attach Chromium refused, or an announcement the attacher
// could not read — each leaving a target held, never resumed — or, once the
// target was on its way, a refusal of a command its Setup sent or of its
// resume.
func (a *Attacher) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// Close stops the attacher and waits for its goroutines to end, a Setup still
// running among them, which its context's end tells to return — so a Setup
// never calls Close, and stops the attacher by returning an error instead. A
// target it was holding stays held, and the stop is no [Attacher.Err]. It is
// idempotent.
func (a *Attacher) Close() {
	a.cancel(ErrClosed)
	<-a.done
}

// watch listens on s for the targets it attaches, then turns auto-attach on
// for s with filter: listening first, so no announcement is missed.
func (a *Attacher) watch(ctx context.Context, s *Session, filter []TargetFilterEntry) error {
	l := s.Listen(TargetAttachedToTarget)
	a.wg.Add(1)
	go a.announcements(ctx, l)
	_, err := Call(ctx, s, targetSetAutoAttach, targetSetAutoAttachParams{
		AutoAttach:             true,
		WaitForDebuggerOnStart: true,
		Flatten:                true,
		Filter:                 filter,
	})
	return err
}

// announcements takes each target s attaches and holds it, until the
// attacher stops or s ends.
func (a *Attacher) announcements(ctx context.Context, l *Listener) {
	defer a.wg.Done()
	defer l.Close()
	for {
		m, err := l.Next(ctx)
		if err != nil {
			return
		}
		// An announcement the attacher cannot read, or one naming no session
		// or no target, holds a target it cannot reach to resume: as a target
		// whose Setup failed, it stays held, and stops the attacher.
		e, _, err := TargetAttachedToTarget.Of(m)
		if err == nil && (e.SessionID == "" || e.TargetInfo.TargetID == "") {
			err = fmt.Errorf("cdp: %s names no session or no target", TargetAttachedToTarget.name)
		}
		if err != nil {
			a.fail(ctx, err)
			return
		}
		// The session the reader registered before it delivered this. Ended
		// already, it detached before it could be held: there is nothing to
		// resume, nor for Setup to prepare — hold's guards would make the
		// failure of a Setup run on it no failure anyway, so this only spares
		// Setup a session that can no longer answer.
		child := a.c.Session(e.SessionID)
		if child.Err() != nil {
			continue
		}
		a.wg.Add(1)
		go a.hold(ctx, child, e)
	}
}

// hold sets one target up, turns auto-attach on for it, resumes it if
// Chromium holds it, and then waits for what its Setup sent. A child that
// ends on the way — its target closed or crashed — leaves nothing to resume
// and is no failure of the attacher's.
func (a *Attacher) hold(ctx context.Context, child *Session, attached TargetAttachedToTargetEvent) {
	defer a.wg.Done()
	info := attached.TargetInfo
	var sent []Answer
	if a.o.Setup != nil {
		var err error
		if sent, err = a.o.Setup(ctx, child, attached); err != nil {
			if child.Err() == nil {
				a.fail(ctx, fmt.Errorf("cdp: setting up %s target %s: %w", info.Type, info.TargetID, err))
			}
			return
		}
	}
	// Waited for before the resume, so that what the child starts is held
	// too. Chromium answers it while it holds the child, a popup with no
	// renderer yet included (AttachOptions.Setup). A target type that serves
	// no Target domain attaches nothing of its own: Chromium's
	// method-not-found is no failure.
	if err := a.watch(ctx, child, a.childFilter); err != nil && !notServed(err) && child.Err() == nil {
		a.fail(ctx, fmt.Errorf("cdp: turning auto-attach on for %s target %s: %w", info.Type, info.TargetID, err))
		return
	}
	// Only a target Chromium holds is resumed, never one already running. A
	// resume releases a popup held inside window.open: one on the popup's own
	// session, one on its OPENER'S (a running page's popup, held three seconds,
	// ran at once), and one on any page sharing the opener's renderer (with
	// --renderer-process-limit=1, a resumed about:blank tab released another
	// tab's popup before its Setup was done). So a caller never opens a window
	// from an about:blank tab, and creates tabs on about:blank and navigates
	// once Setup's answers are in: a tab created with an address ran its first
	// script before Setup, and its resume released popups it had opened. A popup
	// with no opener is held until a resume reaches its own session.
	var resumed *Pending[struct{}]
	if attached.WaitingForDebugger {
		resumed = Send(ctx, child, runtimeRunIfWaitingForDebugger, struct{}{})
	}
	for _, answer := range sent {
		err := errNilAnswer
		if answer != nil {
			err = answer.failure(ctx)
		}
		// A nil answer is the caller's fault, not the target's, so it stops
		// the attacher even when the child has ended meanwhile.
		if errors.Is(err, errNilAnswer) || (err != nil && child.Err() == nil) {
			a.fail(ctx, fmt.Errorf("cdp: setting up %s target %s: %w", info.Type, info.TargetID, err))
			return
		}
	}
	if resumed == nil {
		return
	}
	// A target type that serves no Runtime domain runs no script to hold:
	// Chromium's method-not-found is no failure.
	if _, err := resumed.Wait(ctx); err != nil && !notServed(err) && child.Err() == nil {
		a.fail(ctx, fmt.Errorf("cdp: resuming %s target %s: %w", info.Type, info.TargetID, err))
	}
}

// fail records the attacher's first failure and stops it. ctx is the
// attacher's own, which every goroutine it starts is handed.
func (a *Attacher) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		// Stopped already, and what failed was only the stop.
		return
	}
	a.mu.Lock()
	if a.err == nil {
		a.err = err
	}
	a.mu.Unlock()
	a.cancel(err)
}

// notServed reports whether err is Chromium's refusal of a method the target
// does not serve.
func notServed(err error) bool {
	var p *ProtocolError
	return errors.As(err, &p) && p.Code == MethodNotFound
}

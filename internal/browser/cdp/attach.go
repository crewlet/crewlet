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
	// ChildFilter is every attached child's filter, in turn: which types
	// a page, a frame or a worker attaches of its own. Nil or empty is
	// Chromium's default without a service worker: every type but
	// "browser", "tab" and "service_worker". A filter that attaches nothing
	// says so in an entry, {Exclude: true}, since an empty one would be
	// sent as none at all, which is Chromium's default.
	//
	// It is apart from Filter because Chromium announces a service worker
	// at more than one level — at the browser's and again at every page it
	// serves — and a target must be held by ONE session: held by two, it is
	// set up by each and starts only once both have resumed it (measured on
	// both pinned Chromiums: a command to a service worker two sessions held
	// went unanswered for as long as both held it, one to a service worker a
	// single session held was answered at once, and with a page's own
	// auto-attach taking it, it was announced twice). So a service worker is
	// taken at the parent's level alone, by Filter, which on the browser's
	// session is one session however many pages it serves: AutoAttach
	// refuses a ChildFilter that takes one, which would hold it once for
	// each page it serves, and a nil or empty ChildFilter leaves it out. A
	// shared worker is announced at the browser's level alone, and a type
	// neither filter takes is never attached, and runs unheld.
	ChildFilter []TargetFilterEntry
	// Setup prepares one target as it is attached: the caller's request
	// gate, bindings and scripts. It SENDS them, with [Send], in the order
	// Chromium is to apply them, and returns what it sent: the attacher
	// resumes a target Chromium holds behind those commands, and waits for
	// their answers only then. Setup waits for none of them itself while its
	// target is held, because a page Chromium holds before its renderer
	// exists — a popup opened with no opener, by window.open's noopener or a
	// link's target=_blank — answers only what the browser serves until it
	// has resumed: held for three seconds on both pinned Chromiums, such a
	// popup answered Fetch.enable and Target.setAutoAttach, and its Runtime,
	// Page, Network and Emulation commands only once resumed. A Setup that
	// waited on one would hold its target until [CommandTimeout], and fail.
	// What Setup sent is in place before its target runs a line: on both
	// pins such a popup ran its first script with the binding and the script
	// its Setup had sent. A refusal among the answers is the attacher's
	// [Attacher.Err], its target running by then without what was refused.
	//
	// attached is Chromium's announcement of the target. Its
	// WaitingForDebugger says whether Chromium holds it: it does for a
	// target it starts once auto-attach is on, which runs no line before what
	// Setup sent, and does not for one already running when auto-attach
	// reached it, which Setup prepares as it runs — it answers at once, so
	// Setup may wait on it — and refuses, by returning an error, when the
	// caller must not drive such a target. AutoAttach says which targets are
	// which, and names the one kind announced as held that is not.
	//
	// Setup never makes a target Chromium holds open a window: the popup
	// would be held inside its opener's window.open, and the attacher's
	// resume of the opener would release it before its own setup
	// (AutoAttach). Setup runs for each child on a goroutine of its own,
	// concurrently across children, with the attacher's context. To stop the
	// attacher, it returns an error, which leaves its target held. It never
	// calls [Attacher.Close] or waits on [Attacher.Done]: both wait for every
	// goroutine the attacher started, Setup's own among them, and so would
	// wait for ever. Nil sets up nothing. Every Answer it hands back is what
	// [Send] returned: a nil one stops the attacher, naming the target, as an
	// error would.
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

// AutoAttach turns auto-attach on for parent, with Chromium told to hold each
// new target at its start, and resumes no target it attaches until o.Setup
// has prepared it. For each one Chromium announces it runs Setup, then turns
// auto-attach on for the child with o.ChildFilter (nil or empty: Chromium's
// default without a service worker) — the recursion — and once Chromium has
// answered that, resumes the child (Runtime.runIfWaitingForDebugger) behind
// what Setup sent, and only then waits for Setup's answers. So no target
// Chromium holds runs a line before its setup and its own auto-attach are in
// place. A Setup that fails, or a child's own auto-attach Chromium refuses,
// leaves its target held, never resumed, and is the attacher's
// [Attacher.Err]: the attacher stops, and whoever holds the session ends it.
//
// What Chromium holds is each target of these kinds that it starts once
// auto-attach is on: a page — a popup window.open or a link opens, of its
// opener's site or another, with an opener or without one, or a tab
// Target.createTarget opens on about:blank — an out-of-process frame, and a
// worker of any kind, a frame's own and a worker's own among them. On both
// pinned Chromiums, with every Setup made to take two seconds, none of them
// ran a line before its setup (TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp
// drives every kind named here). Any other kind of target was not measured,
// and is not known to be held. Two of the ways Chromium holds a page were
// measured, and the attacher keeps to both:
//
//   - A popup window.open opens with an opener is held inside that
//     window.open, which returns to the opener's script once the popup is
//     released. A resume on the popup's own session releases it, and so does
//     one on its OPENER'S: on both pins a running page's popup, held for
//     three seconds, ran at once when the page was sent
//     Runtime.runIfWaitingForDebugger, before any setup of its own, while
//     resuming the page's dedicated, shared or service worker released
//     nothing. So the attacher resumes only a target Chromium holds, which
//     has run nothing and so opened nothing, never one already running; the
//     resume is this package's alone, unexported, so nothing else sends one;
//     and nothing makes a held target open a window before it is resumed
//     (AttachOptions.Setup). A resume can release a popup through another page
//     that shares its opener's renderer, not only through the opener's own
//     session: on both pins, with Chromium run with
//     --renderer-process-limit=1, the attacher's resume of a tab created on
//     about:blank, held and set up in a second, released a popup that a
//     running about:blank tab held inside window.open, before the popup's own
//     six-second Setup was done. When the opener was on a site, its popup
//     waited for its own resume even so, the site's renderer not shared with
//     the about:blank tab, and so did both when Chromium ran by default, each
//     tab with a renderer of its own. So a caller never opens a window from a
//     tab still on about:blank.
//   - A popup with no opener, and a tab Target.createTarget opens on
//     about:blank, are held by the browser until a resume reaches their own
//     session: on both pins a popup with no opener stayed held while the page
//     that opened it was resumed, and a navigation sent to a held tab waited
//     for the tab's resume. Such a popup has no renderer while it is held,
//     which is why Setup sends rather than calls.
//
// Two kinds are not held:
//
//   - A target already running when auto-attach reaches it — a tab a pool
//     already had open, the browser's first tab once it has started, and
//     their frames and workers — is attached as it runs, announced with
//     WaitingForDebugger false, and its setup comes after whatever it has
//     run. A caller that must not drive such a target refuses it in Setup,
//     or closes it and opens a tab of its own. The attacher never resumes
//     it, so a popup it opens is held until that popup's own setup.
//   - A tab Target.createTarget opens with an address is announced held, with
//     WaitingForDebugger true, yet on both pins it ran its first script
//     before a Setup that took two seconds. Announced held, it is resumed
//     once its Setup is done, and that resume releases a popup it has opened
//     meanwhile: on both pins such a tab's popup ran as the tab's two-second
//     Setup ended, before a Setup of its own that took four. So the caller
//     creates its tabs on about:blank, and navigates each once its Setup's
//     answers are in.
//
// AutoAttach refuses a Filter that takes tabs, sending nothing. Chromium then
// announces the page in each tab unheld (WaitingForDebugger false), and the
// attacher resumes only what Chromium says it holds: on both pins, with
// auto-attach taking tabs, a page opened from a running tab was announced so
// under its tab, which was held and resumed, and the page never ran. Taking
// tabs also had Chromium attach a page's prerender, under its tab, held
// before it had a renderer, where taking pages, as Chromium's default and the
// zero AttachOptions do, a page's speculation rules prerender nothing: on
// both pins the page they named was never requested.
//
// The attacher's goroutines end with ctx, with the parent's connection, with
// a Setup that failed, or with [Attacher.Close]. AutoAttach itself returns
// once Chromium has answered the parent's Target.setAutoAttach, and refuses,
// sending nothing, options whose ChildFilter takes a type Chromium announces
// at more than one level (AttachOptions.ChildFilter says why).
func AutoAttach(ctx context.Context, parent *Session, o AttachOptions) (*Attacher, error) {
	if takes(o.Filter, "tab") {
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
	// Only a target Chromium holds is resumed: one already running needs no
	// resume, and a resume of a page releases every popup it holds, before
	// that popup's own setup (AutoAttach).
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

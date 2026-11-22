package executor

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// What the layers above ask a harness instead of its name: each an optional
// capability of the harness, read through the runner (the sandbox layer's,
// [Runner.ThreadTraits]) or looked up by the harness's name (the engine's,
// [Window], [SessionTraits]), so a harness that states none reads as zero.

// threadHarness is a [mainThreadHarness] stating what its read settles.
type threadHarness interface {
	threadTraits() sandbox.ThreadTraits
}

// ThreadTraits is what the runner's harness's main-thread read settles
// ([sandbox.ThreadTraits]): zero for a harness that reads no thread, but
// Receipts for one that confirms off its session record ([recordConfirmer]).
func (r *Runner) ThreadTraits() sandbox.ThreadTraits {
	traits := sandbox.ThreadTraits{}
	if _, reads := r.harness.(mainThreadHarness); reads {
		if h, ok := r.harness.(threadHarness); ok {
			traits = h.threadTraits()
		}
		traits.Reads = true
	}
	if _, records := r.harness.(recordConfirmer); records {
		traits.Receipts = true
	}
	return traits
}

// notesOffExplainer is a [noteReader] saying what in a box can switch its
// reading of notes off.
type notesOffExplainer interface{ notesOffWhy() string }

// NotesOffWhy is what can switch the runner's harness's reading of notes off,
// "" for a harness that says nothing ([sandbox] warns with it).
func (r *Runner) NotesOffWhy() string {
	if h, ok := r.harness.(notesOffExplainer); ok {
		return h.notesOffWhy()
	}
	return ""
}

// SignIn is how a member signs its harness in, as far as the route it reaches
// its model by differs: harness-neutral, so the executor reads no config. The
// zero value is a mode this build does not know, judged as the least window.
type SignIn string

const (
	// SignInFirstParty means the entry hands the harness first-party credentials
	// alone (a login, a key, or the host's own environment).
	SignInFirstParty SignIn = "first-party"
	// SignInOwnEnv means the entry signs the harness in by its own env.
	SignInOwnEnv SignIn = "own-env"
)

// SignIns is every [SignIn] this build knows.
var SignIns = []SignIn{SignInFirstParty, SignInOwnEnv}

// Valid reports whether this build knows s.
func (s SignIn) Valid() bool { return slices.Contains(SignIns, s) }

// WindowAsk is what a launch knows of the member a window is asked for
// ([Window]).
type WindowAsk struct {
	// Model is the model argument the member hands its harness.
	Model string
	// SignIn is how the member signs its harness in.
	SignIn SignIn
	// Env is the member's own env, read only where Exact is set.
	Env map[string]string
	// Exact reads the member's platform off its env, as the launch floor
	// does; unset, a mode signing in by env is held to the least window, as a
	// relaunch's telling is.
	Exact bool
}

// windowHarness is a harness that compacts by its run's window
// ([sandbox.FirstRequestWindow]).
type windowHarness interface {
	Window(ask WindowAsk) sandbox.ExecutorWindow
	PinnedWindow(model string) int
}

// Window is the window a run of ask's member on harness is pinned to and held
// to, and false for a harness that compacts by no window or one this build
// does not drive.
func Window(harness string, ask WindowAsk) (sandbox.ExecutorWindow, bool) {
	if h, ok := harnessNamed(harness).(windowHarness); ok {
		return h.Window(ask), true
	}
	return sandbox.ExecutorWindow{}, false
}

// PinnedWindow is the window the runner's harness pins a run on model to, 0
// for a harness that pins none.
func (r *Runner) PinnedWindow(model string) int {
	if h, ok := r.harness.(windowHarness); ok {
		return h.PinnedWindow(model)
	}
	return 0
}

// sessionJudge is a harness whose continued sessions a relaunch judges.
type sessionJudge interface {
	sessionTraits() sandbox.SessionTraits
}

// SessionTraits is what a relaunch reads of the harness named harness
// ([sandbox.SessionTraits]), zero for one that states none or this build does
// not drive.
func SessionTraits(harness string) sandbox.SessionTraits {
	if h, ok := harnessNamed(harness).(sessionJudge); ok {
		return h.sessionTraits()
	}
	return sandbox.SessionTraits{}
}

// harnessNamed is the harness named name, nil for one this build does not
// drive.
func harnessNamed(name string) Harness {
	for _, h := range Registered() {
		if h.Name() == name {
			return h
		}
	}
	return nil
}

// artifactHolder is a harness whose runs leave files of their own in a box
// ([Runner.ClearArtifacts]): files removed one by one, dirs whole.
type artifactHolder interface {
	artifacts(p Paths) (files, dirs []string)
}

// boxPointHarness is a harness that compacts at a point only its box can say
// ([sandbox.FirstRequestBox]).
type boxPointHarness interface {
	readBoxPoint(ctx context.Context, box sandbox.Sandbox, ask sandbox.PointAsk) (sandbox.BoxPoint, error)
}

var _ sandbox.PointReader = (*Runner)(nil)

// ReadBoxPoint reads, in box, where the runner's harness compacts a continued
// session's first request ([sandbox.PointReader]), charged at the harness's own
// rate ([sandbox.SessionTraits.Charge]); a mark for a harness that reads none.
func (r *Runner) ReadBoxPoint(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.PointAsk) (sandbox.BoxPoint, error) {

	h, ok := r.harness.(boxPointHarness)
	if !ok {
		return sandbox.BoxPoint{Mark: sandbox.PointUnread,
			Why: r.harness.Name() + " compacts at no point its box says"}, nil
	}
	point, err := h.readBoxPoint(ctx, box, ask)
	point.Charge = SessionTraits(r.harness.Name()).Charge
	return point, err
}

// StartsFresh reports whether the runner's harness continues no earlier
// session ([resumer]), so every launch on it starts one of its own.
func (r *Runner) StartsFresh() bool {
	c, ok := r.harness.(resumer)
	return !ok || !c.Resumes()
}

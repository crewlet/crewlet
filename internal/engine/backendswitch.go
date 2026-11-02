package engine

import (
	"context"
	"strconv"

	"github.com/crewlet/crewlet/internal/config"
)

// A native backend changes at a restart, and the node says so.
//
// # Why a switch is not live
//
// A node's native runtime — Worklet's, Doclet's and Chatlet's read and write
// sides over the core's logs, the code host's committer and git surface,
// the index, the feeds — is built ONCE per process ([Engine.startNative], or
// [Engine.startRemote] on a node without `data`), from whichever company
// started it, and it follows the fleet's logs rather than a revision. The
// logs themselves are the core's and run from boot whatever the company says
// (core.go). Tearing a half down or bringing one up at an apply would rebuild
// the index and the feeds and re-run every boot reconcile on a configuration
// edit; so a revision that turns a native half on or off activates normally,
// reports `ok`, and changes nothing here until the process restarts.
//
// # Why it is said, once per activation
//
// That was always the rule, and nothing said so: a revision that moved
// `tracker.backend` from worklet to jira left every node serving Worklet's
// tools with no word anywhere, and one that turned Doclet on on a node built
// on Confluence served no page tool, with nothing saying why. [Engine.reconcileNative] runs once per apply and an apply once
// per activation per node, so each activation that still asks for a switch
// says it once — a re-activation of the same document is somebody asking
// again — and a restart's reapply compares against the runtime that restart
// built from it, which agrees, so it says nothing. That last clause rests on
// `crewlet run` booting a node on the revision its store holds rather than on
// its -company file, and holds for every node that held the revision when it
// restarted; one that did not (down at the activation, or a data node joining
// with an empty store) boots on the company it had, says it once more at its
// first apply, and agrees after its next restart.

// builtWith is the native-backend choices a runtime was built with: the
// company that started it, never a later revision. A runtime is built once
// per process ([Engine.startNative], [Engine.startRemote]), so these are what
// it serves until a restart, whatever an activation says — and
// [backendSwitches] is how an activation that says otherwise is told so.
//
// THE BACKEND VALUES, not whether each is native, so the line can name what
// the node was started on; the comparison reads only their native-ness.
type builtWith struct {
	tracker   config.TrackerBackend
	knowledge config.KnowledgeBackend
	chat      config.ChatBackend
	// code is `code.native`, which is already whether the half is the
	// engine's own: the code host has no vendor to name.
	code bool
}

// builtFrom is what a runtime started for c is built with — the one reading
// both kinds of runtime take, so the halves a runtime holds and the choices it
// reports cannot disagree.
func builtFrom(c *config.Company) builtWith {
	return builtWith{
		tracker: c.TrackerBackendFor(), knowledge: c.KnowledgeBackendFor(),
		chat: c.ChatBackendFor(), code: c.Code.Native,
	}
}

// runsTracker, runsWiki, runsChat and runsCode are the halves a runtime built
// this way holds: Worklet, Doclet, Chatlet and the code host.
func (b builtWith) runsTracker() bool { return b.tracker == config.TrackerWorklet }
func (b builtWith) runsWiki() bool    { return b.knowledge == config.KnowledgeDoclet }
func (b builtWith) runsChat() bool    { return b.chat == config.ChatChatlet }
func (b builtWith) runsCode() bool    { return b.code }

// backendSwitch is one native-backend choice an activated revision makes that
// the running runtime was not built with.
type backendSwitch struct {
	// Field is the configuration path the revision moved: tracker.backend,
	// knowledge.backend, chat.backend or code.native.
	Field string
	// Half is what the runtime builds natively for that field, as a person
	// reads it: "Worklet", "Doclet", "Chatlet", "the code host".
	Half string
	// Running is the field's value the runtime was built with — what this
	// node serves until a restart.
	Running string
	// Activated is the value the revision says.
	Activated string
	// Holds is whether the runtime holds the engine's own half: true for a
	// half the revision turned off, which the node keeps serving; false for
	// one it turned on, which the node has nothing to serve with.
	Holds bool
}

// detail is the sentence an operator reads, which differs by direction: a
// half the runtime holds goes on being served, and one it does not cannot be.
func (s backendSwitch) detail() string {
	meanwhile := "with " + s.Half + " (" + s.Field + ": " + s.Running +
		") and keeps serving it"
	if !s.Holds {
		meanwhile = "without " + s.Half + " (" + s.Field + ": " +
			s.Running + ") and cannot serve it"
	}
	return "this node's native runtime was built " + meanwhile + " until the process " +
		"restarts; restart each node (one at a time is fine) to serve " + s.Field +
		": " + s.Activated
}

// backendSwitches is every choice c makes that a runtime built with b does not
// serve: a choice of WHETHER a half is native, since that is what the runtime
// was built around. Moving between two vendors (jira to none) is not one — a
// vendor's parser is rebuilt live by its reconciler — and neither is any other
// field. In a fixed order: tracker, then knowledge, then chat, then the code
// host — whose one field is already whether the half is native, so any change
// of it is one.
//
// PURE, so the rule is tested without an engine; [Engine.sayBackendSwitches]
// says each one.
func backendSwitches(b builtWith, c *config.Company) []backendSwitch {
	var out []backendSwitch
	if tracker := c.TrackerBackendFor(); b.runsTracker() != (tracker == config.TrackerWorklet) {
		out = append(out, backendSwitch{
			Field: "tracker.backend", Half: "Worklet",
			Running: string(b.tracker), Activated: string(tracker), Holds: b.runsTracker(),
		})
	}
	if knowledge := c.KnowledgeBackendFor(); b.runsWiki() != (knowledge == config.KnowledgeDoclet) {
		out = append(out, backendSwitch{
			Field: "knowledge.backend", Half: "Doclet",
			Running: string(b.knowledge), Activated: string(knowledge), Holds: b.runsWiki(),
		})
	}
	if chat := c.ChatBackendFor(); b.runsChat() != (chat == config.ChatChatlet) {
		out = append(out, backendSwitch{
			Field: "chat.backend", Half: "Chatlet",
			Running: string(b.chat), Activated: string(chat), Holds: b.runsChat(),
		})
	}
	if b.runsCode() != c.Code.Native {
		out = append(out, backendSwitch{
			Field: "code.native", Half: "the code host",
			Running: strconv.FormatBool(b.code), Activated: strconv.FormatBool(c.Code.Native),
			Holds: b.runsCode(),
		})
	}
	return out
}

// runtimeBuilt is what this node's native runtime was built with, and whether
// it runs one at all — a data node's or a stateless node's, whichever it is.
//
// THE ONE READING of whether a runtime exists that [Engine.reconcileNative]
// guards on, so the line below it and the parsers it swaps are judged by one
// answer rather than two.
func (e *Engine) runtimeBuilt() (builtWith, bool) {
	if n := e.native.Load(); n != nil {
		return n.built, true
	}
	if r := e.remote.Load(); r != nil {
		return r.built, true
	}
	return builtWith{}, false
}

// sayBackendSwitches warns, once, about every native-backend choice c makes
// that a runtime built with built does not serve.
//
// Only ever said over a runtime that EXISTS: its caller returns first on a
// node running none. Nor could such a node have anything to say — an apply
// starts the runtime from its own company at its `native` stage
// ([Engine.startNativeFor]), whatever backends that company names, before the
// stage that says this, so the first runtime an apply meets always agrees with
// it.
func (e *Engine) sayBackendSwitches(ctx context.Context, built builtWith, c *Company) {
	for _, s := range backendSwitches(built, c.Config) {
		log.WarnContext(ctx, "native_backend_needs_restart", "field", s.Field,
			"running", s.Running, "activated", s.Activated, "detail", s.detail())
	}
}

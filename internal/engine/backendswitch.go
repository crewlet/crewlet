package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// A native backend changes at a restart, and the node says so.
//
// The native runtime (Worklet, Doclet, Chatlet, Gitlet, the index, the feeds)
// is built ONCE per process ([Engine.startNative], [Engine.startRemote]) and
// follows the core's logs, run from boot whatever the company says (core.go):
// rebuilding it at an apply would rebuild the index and feeds and re-run every
// boot reconcile on a config edit. So a revision turning a native half on or
// off activates `ok`, changes nothing until a restart, and is warned about by
// [Engine.reconcileNative] at each activation asking for it (a re-activation
// is somebody asking again). A restart's reapply agrees with the runtime it
// built, since `crewlet run` boots on the store's revision; a node that missed
// the revision boots on its old company and says it once more at first apply.

// builtWith is the native-backend choices a runtime was built with: the
// company that started it, never a later revision, since a runtime is built
// once per process; [backendSwitches] tells an activation that differs. It
// holds the backend values, not their native-ness, so the line can name what
// the node was started on.
type builtWith struct {
	tracker   config.TrackerBackend
	knowledge config.KnowledgeBackend
	chat      config.ChatBackend
	code      config.CodeBackend
}

// builtFrom is what a runtime started for c is built with — the one reading
// both kinds of runtime take, so the halves a runtime holds and the choices it
// reports cannot disagree.
func builtFrom(c *config.Company) builtWith {
	return builtWith{
		tracker: c.TrackerBackendFor(), knowledge: c.KnowledgeBackendFor(),
		chat: c.ChatBackendFor(), code: c.CodeBackendFor(),
	}
}

// runsTracker, runsWiki, runsChat and runsCode are the halves a runtime built
// this way holds: Worklet, Doclet, Chatlet and Gitlet.
func (b builtWith) runsTracker() bool { return b.tracker == config.TrackerWorklet }
func (b builtWith) runsWiki() bool    { return b.knowledge == config.KnowledgeDoclet }
func (b builtWith) runsChat() bool    { return b.chat == config.ChatChatlet }
func (b builtWith) runsCode() bool    { return b.code == config.CodeGitlet }

// backendSwitch is one native-backend choice an activated revision makes that
// the running runtime was not built with.
type backendSwitch struct {
	// Field is the configuration path the revision moved: tracker.backend,
	// knowledge.backend, chat.backend or code.backend.
	Field string
	// Half is what the runtime builds natively for that field, as a person
	// reads it: "Worklet", "Doclet", "Chatlet", "Gitlet".
	Half string
	// Running is the value the runtime was built with, served until a restart.
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
// serve: a change of WHETHER a half is native, the one thing the runtime was
// built around. Moving between two vendors (jira to none) is not one, since a
// vendor's parser is rebuilt live by its reconciler. In a fixed order:
// tracker, knowledge, chat, then Gitlet, whose axis names no vendor, so any
// change of it is one. PURE, so the rule is tested without an engine;
// [Engine.sayBackendSwitches] says each.
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
	if code := c.CodeBackendFor(); b.runsCode() != (code == config.CodeGitlet) {
		out = append(out, backendSwitch{
			Field: "code.backend", Half: "Gitlet",
			Running: string(b.code), Activated: string(code), Holds: b.runsCode(),
		})
	}
	return out
}

// service is the switch's field as the org projection's `services` names it:
// tracker, knowledge, chat or code: the field without its `.backend`.
func (s backendSwitch) service() string {
	return strings.TrimSuffix(s.Field, ".backend")
}

// AwaitingRestart is which of Crewlet's own services c turns ON that this
// node's native runtime was built without, as the org projection's `services`
// names them (tracker, knowledge, chat, code), in that order; empty on a node
// serving everything c asks for, and on one running no runtime yet, whose
// first apply builds it from the company it meets.
//
// Only the turned-on half of [backendSwitches]: a service turned off is still
// served until the restart, while one turned on has nothing to answer with, so
// the API names these on the org projection and refuses their questions with
// `native_backend_needs_restart`. Read OF c, never of the current epoch again,
// so the services a caller reports and those awaiting a restart cannot
// straddle a publish.
func (e *Engine) AwaitingRestart(c *config.Company) []string {
	built, runs := e.runtimeBuilt()
	if !runs || c == nil {
		return nil
	}
	return awaitingRestart(built, c)
}

// awaitingRestart is [Engine.AwaitingRestart] over a runtime built with b.
// PURE, so the rule is tested without an engine.
func awaitingRestart(b builtWith, c *config.Company) []string {
	var out []string
	for _, s := range backendSwitches(b, c) {
		if !s.Holds {
			out = append(out, s.service())
		}
	}
	return out
}

// runtimeBuilt is what this node's native runtime was built with, and whether
// it runs one at all (a data node's or a stateless node's). It is the one
// reading [Engine.reconcileNative] guards on, so the line and the parsers it
// swaps are judged by one answer.
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
// that a runtime built with built does not serve. Its caller says it only over
// a runtime that EXISTS: an apply starts a missing runtime from its own
// company at its `native` stage ([Engine.startNativeFor]), before this stage,
// so the first runtime an apply meets always agrees with it.
func (e *Engine) sayBackendSwitches(ctx context.Context, built builtWith, c *Company) {
	for _, s := range backendSwitches(built, c.Config) {
		log.WarnContext(ctx, "native_backend_needs_restart", "field", s.Field,
			"running", s.Running, "activated", s.Activated, "detail", s.detail())
	}
}

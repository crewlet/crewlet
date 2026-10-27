package engine

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
)

// THE CODE READ TOOLS' ENGINE WIRING: the same deps for a seat's registry and
// for the operator catalogue, over the code host through this node's estate
// router — its own copy of the code log where it serves, a data node's
// otherwise, and a repository's home git node for its content — and the
// address a person's git clones from.

// CodeToolDeps are the code read tools' dependencies on this node, or the zero
// value (which serves none of them) where this node's RUNNING runtime serves
// no code host.
//
// [OperatorCode]'S ANSWER, the one the code host's REST reads get, and through
// it [Engine.nativeHalves]' — so a seat's code tools and a person's `/code/`
// reads on one node are offered on exactly the same terms. None for a company
// that does not host code, whose tools would answer every call that no
// repository is served; none for one that turned `code.native` on until this
// node restarts into it, where `/code/` answers `native_backend_needs_restart`
// too; and still the tools for one that turned it off, until that restart,
// since the runtime goes on serving its repositories as git and `/code/` do.
// WHATEVER THIS NODE'S ROLES: a node holding no copy of the code log and no
// `git` role reaches both through its router.
//
// EXPORTED for the operator surface, which builds its catalogue outside this
// package for every call; a seat's registry takes the value at every apply
// ([Engine.equip]), after the runtime the apply starts.
func CodeToolDeps(e *Engine) builtin.CodeDeps {
	if e == nil {
		return builtin.CodeDeps{}
	}
	seam, ok := OperatorCode(e)
	if !ok {
		return builtin.CodeDeps{}
	}
	return builtin.CodeDeps{Code: seam, CloneURL: e.cloneURL}
}

// cloneURL is the address a person's git clones repository name from: the
// deployment's `api.external_url` — the listener /git/ is served on — and the
// one path a repository is served at, or empty where the deployment names no
// address, which no answer then invents.
func (e *Engine) cloneURL(name string) string {
	if e.externalBase == "" {
		return ""
	}
	return e.externalBase + auth.GitRepoPath(name)
}

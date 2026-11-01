package engine

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
)

// THE CODE TOOLS' ENGINE WIRING: the same deps for a seat's registry and for
// the operator catalogue, over the code host through this node's estate router
// — its own copy of the code log where it serves, a data node's otherwise, and
// a repository's home git node for its content and its engine-built writes —
// the address a person's git clones from, and whoever a turn is acting for
// with the chart that decides them (the requester ceiling, the artifacts tools'
// own: [turnRequesters] and [ChartAuthorityOf]).

// CodeToolDeps are the code tools' dependencies on this node, or the zero value
// (which serves none of them) where this node's RUNNING runtime serves no code
// host: the reads, the writes, and the seam a seat's turn holds a scratch
// repository through — the router's one code seam behind all three, so a node
// that serves a seat's reads serves its writes and keeps its holds on the same
// terms.
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
	// THE CEILING'S HALVES BESIDE THE CODE HOST, read per call like the
	// chart every seat's authority decision reads: a seat's tools are cloned
	// into its lease and an apply does not rebuild the clone, so a value
	// holding this epoch's chart would judge a requester by an organisation
	// a later activation replaced.
	//
	// AND THE WRITES AND THE HOLDS, over the same seam: an engine write is
	// asked of the repository's home git node and a lifecycle request — a
	// tool's, or a turn's hold record on a scratch repository it named —
	// of a data node, each through the router a read takes. The holder is
	// wired whether or not a seat writes, since a seat that only reads
	// still holds what it reads; an operator's call has no turn and holds
	// nothing whatever is wired here.
	deps := builtin.CodeDeps{Code: seam, Writer: seam, Holder: seam, CloneURL: e.cloneURL,
		Requesters: turnRequesters{engine: e}, Chart: ChartAuthorityOf(e)}
	// AND THE OBJECT STORE A CHECK'S KEPT LOG IS READ FROM, where this node
	// has one: the log a failed run kept is one object the fleet shares,
	// read a page at a time by code_get_check_failures.
	if objects := e.ObjectStore(); objects != nil {
		deps.Logs = objects
	}
	// AND THE TRACKER'S READ OF ONE TASK, where the company runs the native
	// tracker: a change write naming a native work item by its key is
	// resolved to the item's identity through the very read the work tools
	// resolve a key with, on this node's router — none elsewhere, where a
	// key names no item this engine keeps.
	//
	// AND ITS WRITER FOR ONE ACTOR, which a merged change's step two closes
	// its work item through as whoever merged it: the router's, a person's
	// credential or a seat's turn alike ([remoteActor]).
	if halves, ok := e.trackerHalves(); ok {
		deps.Tasks = halves.reader
		deps.Closer = func(actor builtin.Actor) builtin.WorkWriter { return halves.as(actor) }
	}
	return deps
}

// CodeReaderDeps are `code_repo_readers`' dependencies on this node — who read
// a `visibility: unit` repository, from the usage domain through this node's
// estate router (whichever data node's usage copy serves), and the company's
// clock a range is cut on, read per call ([Engine.Zone]) — or the zero value,
// which serves no tool, on [CodeToolDeps]' terms: where this node's running
// runtime serves no code host, the tool is not offered beside the code reads it
// stands with.
//
// EXPORTED for the operator surface, which builds its catalogue for every call.
func CodeReaderDeps(e *Engine) builtin.ReadersDeps {
	if e == nil {
		return builtin.ReadersDeps{}
	}
	if _, ok := OperatorCode(e); !ok {
		return builtin.ReadersDeps{}
	}
	return builtin.ReadersDeps{Readers: e.router.Usage(), Zone: e.Zone}
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

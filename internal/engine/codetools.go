package engine

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// THE CODE TOOLS' ENGINE WIRING: one set of deps for a seat's registry and the
// operator catalogue, over Gitlet through this node's estate router, with the
// clone address and the requester ceiling ([turnRequesters], [ChartAuthorityOf]).

// CodeToolDeps are the code tools' dependencies on this node — the reads, the
// writes and the scratch-repository holds, all over the router's one code seam
// so a node serving a seat's reads serves its writes on the same terms — or
// the zero value, serving none, where [OperatorCode] answers none. That is the
// answer Gitlet's REST reads and [Engine.nativeHalves] get, so a seat's tools
// and a person's `/gitlet/` reads are offered alike: none for a company hosting
// no code, or one that turned Gitlet on until this node restarts into it; still
// the tools for one that turned it off, until that restart. A node with no copy
// of the code log and no `gitlet` role reaches both through its router.
//
// EXPORTED for the operator surface, which builds its catalogue per call; a
// seat's registry takes it at every apply ([Engine.equip]), after the runtime.
func CodeToolDeps(e *Engine) builtin.CodeDeps {
	if e == nil {
		return builtin.CodeDeps{}
	}
	seam, ok := OperatorCode(e)
	if !ok {
		return builtin.CodeDeps{}
	}
	// THE CEILING'S CHART IS READ PER CALL: a seat's tools are cloned into its
	// lease and an apply does not rebuild the clone, so a captured chart would
	// judge a requester by an organisation a later activation replaced.
	//
	// The holder is wired whether or not a seat writes, since a read holds
	// too; an operator's call has no turn and holds nothing. Placement (the
	// router's view of the Gitlet nodes, per call) answers a person's purge,
	// and the search is the engine's own scatter ([Engine.CodeSearch]), so a
	// seat's tool and a person's route search on the same terms.
	deps := builtin.CodeDeps{Code: seam, Writer: seam, Holder: seam, CloneURL: e.cloneURL,
		Requesters: turnRequesters{engine: e}, Chart: ChartAuthorityOf(e), Searcher: e,
		Placement: codePlacement{router: e.router, limits: e.codeLimits},
		// Mentions read the chart current at the write ([LiveMentions]), for
		// the ceiling's reason.
		Mentions: LiveMentions(e)}
	// A failed check's kept log is one shared object, read a page at a time
	// by code_get_check_failures.
	if objects := e.ObjectStore(); objects != nil {
		deps.Logs = objects
	}
	// Only on the native tracker: the read that resolves a work item key a
	// change names (the work tools' own), and a writer per actor, through
	// which a merged change closes its item as whoever merged it
	// ([remoteActor]).
	if halves, ok := e.trackerHalves(); ok {
		deps.Tasks = halves.reader
		deps.Closer = func(actor builtin.Actor) builtin.WorkWriter { return halves.as(actor) }
	}
	return deps
}

// CodeReaderDeps are `code_repo_readers`' dependencies on this node — who read
// a `visibility: unit` repository, from the usage domain over the estate
// router, and the company's clock a range is cut on ([Engine.Zone]) — or the
// zero value, serving no tool, on [CodeToolDeps]' terms.
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

// cloneURL is the address a person's git clones repository name from:
// `api.external_url` (the listener /git/ is on) plus the repository's path, or
// empty where the deployment names no address, which no answer invents.
func (e *Engine) cloneURL(name string) string {
	if e.externalBase == "" {
		return ""
	}
	return e.externalBase + auth.GitRepoPath(name)
}

// codePlacement is where a repository's work is placed, as a purge's answer
// names it (builtin.CodePlacement): its maintainer, by cache.PlacementCharge
// (the maintainers' own loops' figure, so a node with no cache of its own
// names the node they pick), and every live node serving git in the
// repository's order, each of which may hold a copy.
type codePlacement struct {
	router *estate.Router
	limits func() gitlet.Limits
}

func (p codePlacement) Maintainer(repoID string, liveObjects int64) (string, error) {
	charge, err := cache.PlacementCharge(liveObjects, p.limits())
	if err != nil {
		return "", err
	}
	return p.router.Maintainer(repoID, charge)
}

func (p codePlacement) GitletNodes(repoID string) ([]string, error) { return p.router.GitOrder(repoID) }

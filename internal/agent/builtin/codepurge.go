package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE PURGE: code_purge_unreachable, a person's request that a repository's
// objects no ref reaches be dropped — the leaked secret force-pushed away, the
// history a deleted branch left — which the repository's MAINTAINER serves by
// expunging them (internal/code/compact), with no grace for what a ref drop
// left: a purge is asked because something must go now.
//
// # A person's, and a maintainer's
//
// The authority table holds it to a person with a fresh proof of who they
// are (authz.ActionCodePurgeUnreachable: human-only, step-up), so no seat is
// offered it and no model's say-so erases what the company's code held; and
// the tool holds it to the repository's grant, maintain or above, read once
// the repository is — every person maintains every repository (ADR-0055), so
// what that refuses is a principal of a kind a future grant ranks lower. A
// credential bound to nobody purges nothing ([code.UnboundDetail]): the
// request names the person who asked, for its attribution, and nobody is no
// one a purge may be answered to.
//
// # One request, however often it is asked
//
// The request's id is the call's operation (m3-w1's rule, [opIDFor]): the act
// transport's key, a turn-less assistant's `op_id`, so the same call made
// again — after an `unknown`, or a lost answer — is the same request, which
// the repository's committer's ledger collapses and answers from its own
// record. A request asked while another is open WIDENS that one to itself
// (internal/code's ExpungeRequest): the repository holds ONE open purge,
// served once nothing from before the latest request is left.
//
// # What the answer says still holds the objects
//
// A request is answered once its record lands, long before its maintainer
// serves it, so the answer says what a person needs to know about a purge
// that is not done: the request, where its record landed, the git node that
// will serve it ([CodePlacement]), and what still holds the objects after it
// is served — every git node's cache until it syncs past the commit, every
// backup taken before the commit for as long as it is kept, and the object
// store for [code.RetireGrace] after the commit, until the retired rows'
// sweep and the store's next collection take them. The repository's detail
// (code_get_repo's `expunge`) says when the commit landed.

// CodePurgeUnreachableTool is the purge's name: its authority row's action.
const CodePurgeUnreachableTool = string(authz.ActionCodePurgeUnreachable)

// PurgeWrites are the purge, in [CodeWrites] after M4's and M5's writes and
// before the import ([ImportWrites]): M6's first write.
func PurgeWrites() []string { return []string{CodePurgeUnreachableTool} }

// purgeVerb is the verb a purge's operation is named by (m3-w1's op rule).
const purgeVerb = "code-purge"

// CodePlacement is where a repository's work is placed, as the purge's answer
// names it — estate.Router through the engine, declared here by its consumer:
// the git node that maintains a repository of liveObjects live objects, and
// every live git node in the repository's order, each of which may hold a
// copy. Each error is UNKNOWN, never "nobody".
type CodePlacement interface {
	Maintainer(repoID string, liveObjects int64) (string, error)
	GitNodes(repoID string) ([]string, error)
}

// purgeAnswer is a purge's answer.
type purgeAnswer struct {
	Repo string `json:"repo"`
	// Request is the repository's open purge from this request's record on —
	// its own id, or "" where the request was served at once (a repository
	// holding no pack has nothing a cycle could drop).
	Request  string           `json:"request"`
	Outcome  statelog.Outcome `json:"outcome"`
	Position string           `json:"position,omitempty"`
	// Maintainer is the git node that serves the request, as this node's
	// view of the git nodes places the repository's maintenance now; "" with
	// Unplaced saying why where that view could not be read.
	Maintainer string     `json:"maintainer,omitempty"`
	Unplaced   string     `json:"unplaced,omitempty"`
	Holds      purgeHolds `json:"holds"`
	OpID       string     `json:"op_id,omitempty"`
}

// purgeHolds is what still holds a purged object once the purge is served.
type purgeHolds struct {
	// GitNodes are the live git nodes, each of which may hold a copy of the
	// repository: a cache keeps the objects until it syncs past the commit.
	GitNodes []string `json:"git_nodes"`
	// BackupsBefore and StoreUntil are each the sentence of what holds them:
	// the backups taken before the commit, and the object store until a
	// grace after it.
	BackupsBefore string `json:"backups_before"`
	StoreUntil    string `json:"store_until"`
}

// The holds' two sentences, the same for every purge.
var (
	purgeBackupsBefore = "every backup taken before the purge commits keeps the purged objects for as long as " +
		"the backup is kept; code_get_repo's expunge.expunged_at is the commit's instant"
	purgeStoreUntil = fmt.Sprintf("the object store keeps them for %d hours after the purge commits, until "+
		"the retired packs' sweep and the store's next collection take them", int(code.RetireGrace.Hours()))
)

// codePurgeUnreachable is the purge.
type codePurgeUnreachable struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codePurgeUnreachable)(nil)

func (t *codePurgeUnreachable) Name() string { return CodePurgeUnreachableTool }

func (t *codePurgeUnreachable) Description() string {
	return "Purge a repository's unreachable objects — what no branch or tag reaches, such as a secret " +
		"force-pushed away — on its maintainer, with no grace. A person's alone; a request is answered once " +
		"recorded, and asked again it is the same request."
}

func (t *codePurgeUnreachable) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type":       "object",
		"properties": map[string]any{"repo": codeRepoArg()},
		"required":   []string{"repo"},
	})
}

func (t *codePurgeUnreachable) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codePurgeUnreachable) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	if c.asker.Kind != code.PrincipalOperator || c.asker.Seat == "" {
		// THE TABLE'S RULE AGAIN, here where the request is formed: a seat
		// is refused before the call reaches a tool, and a request a seat
		// could send would be refused by every committer.
		return refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, tool+": a purge is a person's, "+
			"through a credential bound to their human seat."), nil
	}
	name, refusal := repoArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	actor, bound := t.deps.bindOperation(c.actor, tool, args)
	if bound != "" {
		return failed(bound), nil
	}
	c.actor = actor
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	if g := code.GrantOf(c.asker, repo, nil); g.Access < code.AccessMaintain {
		return refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, fmt.Sprintf("%s purged nothing of %s: "+
			"a purge is a maintainer's, and you hold %s on it.", tool, repo.Name, g.Access)), nil
	}
	op := opIDFor(actor, tool, purgeVerb, repo.ID, args)
	p := code.ExpungeProposal{Request: &code.ExpungeRequest{RepoID: repo.ID, RequestID: op, By: c.asker}}
	if err := p.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.Expunge(ctx, p)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, c, repo, op, err), nil
	}
	if res.Refusal != "" {
		return purgeRefused(ctx, tool, repo.Name, res, actor), nil
	}
	switch res.Outcome {
	case statelog.OutcomeUnknown:
		return unknownWrite(actor, tool, fmt.Sprintf("the purge of %s was recorded", repo.Name), op, false,
			unknownNext(false, sameCall(actor, tool), fmt.Sprintf("Read the repository's expunge with %s",
				CodeGetRepoTool), "")), nil
	case statelog.OutcomeApplied, statelog.OutcomePending:
	default:
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a purge with outcome %q and no "+
			"refusal", res.Outcome), fmt.Sprintf("%s could not tell what became of the request: %s.", tool,
			faultSaid)), nil
	}
	out := purgeAnswer{Repo: repo.Name, Request: res.Request, Outcome: res.Outcome,
		Position: positionText(res.Position), OpID: actor.Operation,
		Holds: purgeHolds{BackupsBefore: purgeBackupsBefore, StoreUntil: purgeStoreUntil}}
	t.place(repo, &out)
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// place names, in out, the git node that serves the purge and the git nodes
// that may hold a copy, from this node's view of the git nodes — a view that
// could not be read is said, never answered as nobody: the request is
// recorded either way.
func (t *codePurgeUnreachable) place(repo code.RepoRef, out *purgeAnswer) {
	if t.deps.Placement == nil {
		out.Unplaced = "this node names no git placement"
		return
	}
	var unread []string
	maintainer, err := t.deps.Placement.Maintainer(repo.ID, repo.LiveObjects)
	if err != nil {
		unread = append(unread, "the maintainer: "+err.Error())
	}
	nodes, err := t.deps.Placement.GitNodes(repo.ID)
	if err != nil {
		unread = append(unread, "the git nodes: "+err.Error())
	}
	out.Maintainer, out.Holds.GitNodes = maintainer, nodes
	if len(unread) > 0 {
		out.Unplaced = "could not read " + strings.Join(unread, "; ")
	}
}

// purgeRefused is a purge the committer refused with no record, in its class
// ([pushClass]: the repository's states share a push's): a repository gone
// NOT_FOUND, one being imported or a committer that could not take it now
// UNAVAILABLE — the same call again being the same request — and anything
// else the engine's own fault, since nothing about a request but the
// repository is judged.
func purgeRefused(ctx context.Context, tool, repo string, res code.ExpungeResult, actor Actor) tools.Result {
	cause := writeRefusalError(string(res.Refusal), res.Detail)
	class, known := pushClass(res.Refusal)
	if !known || class != tools.RefusalNotFound && class != tools.RefusalUnavailable {
		return faulted(ctx, tool, cause, fmt.Sprintf("%s could not ask for the purge of %s: the code host "+
			"refused it %s, which no request is — %s.", tool, repo, res.Refusal, faultSaid))
	}
	said := fmt.Sprintf("%s asked nothing of %s: %s (%s).", tool, repo, res.Detail, res.Refusal)
	if class == tools.RefusalUnavailable {
		if again := sameCall(actor, tool); again != "" {
			said += " Nothing was recorded: " + again + " — it is the same request."
		} else {
			said += " Nothing was recorded; try again shortly."
		}
	}
	return refusedBy(class, cause, said)
}

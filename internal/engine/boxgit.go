package engine

import (
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// launchBoxGit is the box git environment of each launch of turn turnID's
// executor ([sandbox.LaunchRequest.RunEnv]), minted per launch by
// [Engine.boxGitEnv] over run, the run environment the launch assembled; nil
// where this node gives a box no git: it has no [sandbox.BoxGitURLVar], or it
// serves no code host now.
//
// SERVED IS [OperatorCode]'s ANSWER, the seam every `code_checkout` of the run
// resolves through ([CodeToolDeps]), so a box is given its git exactly where a
// checkout can grant it a repository. Never [Engine.CodeUnserved], which is
// asked only once the code host was found absent and says why: it has no
// answer for a node that serves code, so a launch gated on it being nil was
// given no git on every node, and each checkout it then made cloned from an
// empty base.
func (e *Engine) launchBoxGit(seat *org.Role, turnID string, item *types.WorkItem,
	run map[string]string) func(launchID string) (map[string]string, error) {

	if _, served := OperatorCode(e); e.boxGitURL == "" || !served {
		return nil
	}
	base := e.boxGitURL
	return func(launchID string) (map[string]string, error) {
		return e.boxGitEnv(seat, base, host.BoxRunRef{TurnID: turnID, LaunchID: launchID}, item, run)
	}
}

// boxGitEnv is the engine's git environment for ONE LAUNCH of a run given
// repositories on the code host (ADR-0086), over run, the run environment the
// launch assembled from the operator's configuration: the run's credential for
// this launch — a token naming the turn and the launch id the store just
// minted, so a job a relaunch superseded is refused — the code host's base,
// git's configuration through the environment with the operator's own entries
// kept and the engine's appended ([sandbox.BoxGit]), and the values the coding
// clone's commit hook writes as trailers, naming this launch and the work item
// the launching turn is charged to (nil is none).
//
// THE SEAT'S COMMIT IDENTITY IS THE ENGINE'S ONE RULE for a seat's commits —
// its role's name and email as written, else its handle on the identity
// domain ([codeSigner]) — so a commit the box makes names the seat as one the
// engine builds does. Refused where the run environment's own git
// configuration has a hole git refuses, and where the trailers' values are no
// run's (an engine bug the launch abandons on).
func (e *Engine) boxGitEnv(seat *org.Role, base string, ref host.BoxRunRef, item *types.WorkItem,
	run map[string]string) (map[string]string, error) {

	sig, err := codeSigner{e: e}.Sign(code.Principal{Kind: code.PrincipalAgent, Seat: seat.Handle()})
	if err != nil {
		return nil, fmt.Errorf("engine: the commit identity of seat %s's coding run: %w", seat.Handle(), err)
	}
	if e.boxTokens == nil {
		return nil, fmt.Errorf("engine: this node mints no coding box's credential")
	}
	token := e.boxTokens.Mint(ref)
	if token == "" {
		return nil, fmt.Errorf("engine: no credential can be minted for launch %s of run %s", ref.LaunchID,
			ref.TurnID)
	}
	env, err := sandbox.BoxGit{Base: base, User: host.BoxUser, Token: token, Name: sig.AuthorName,
		Email: sig.AuthorEmail}.Env(run)
	if err != nil {
		return nil, err
	}
	trailers, err := sandbox.TrailerEnv(seat.Handle(), ref.LaunchID, item)
	if err != nil {
		return nil, err
	}
	maps.Copy(env, trailers)
	return env, nil
}

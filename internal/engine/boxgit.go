package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// launchBoxGit is the box git environment of each launch of turn turnID's
// executor ([sandbox.LaunchRequest.RunEnv]), minted per launch by
// [Engine.boxGitEnv] over run, the run environment the launch assembled; nil
// where this node gives a box no git: it has no [sandbox.BoxGitURLVar], or it
// does not serve Gitlet now.
//
// SERVED IS [OperatorCode]'s ANSWER, the seam every `code_checkout` of the run
// resolves through ([CodeToolDeps]), so a box is given its git exactly where a
// checkout can grant it a repository. Never [Engine.CodeUnserved], which is
// asked only once Gitlet was found absent and says why: it has no
// answer for a node that serves code, so a launch gated on it being nil was
// given no git on every node, and each checkout it then made cloned from an
// empty base.
func (e *Engine) launchBoxGit(seat *org.Role, turnID string,
	run map[string]string) func(launchID string) (map[string]string, error) {

	if _, served := OperatorCode(e); e.boxGitURL == "" || !served {
		return nil
	}
	base := e.boxGitURL
	return func(launchID string) (map[string]string, error) {
		return e.boxGitEnv(seat, base, host.BoxRunRef{TurnID: turnID, LaunchID: launchID}, run)
	}
}

// boxGitEnv is the engine's git environment for ONE LAUNCH of a run on a node
// that serves Gitlet, whatever the run goes on to check out
// (ADR-0093), over run, the run environment the launch assembled from the
// operator's configuration: the run's credential for this launch — a token
// naming the turn and the launch id the store just minted, so a job a
// relaunch superseded is refused — Gitlet's base, git's configuration
// through the environment with the operator's own entries kept and the
// engine's appended ([sandbox.BoxGit]). A commit's trailers are no part of
// it: they come from the run's attribution hook, which the harness's launch
// writes from [sandbox.CommitAttribution], and that hook is the only place
// they are spelled, because a value here would name them twice.
//
// THE SEAT'S COMMIT IDENTITY IS THE ENGINE'S ONE RULE for a seat's commits —
// its role's name and email as written, else its handle on the identity
// domain ([codeSigner]) — so a commit the box makes names the seat as one the
// engine builds does. Refused where the run environment's own git
// configuration has a hole git refuses.
func (e *Engine) boxGitEnv(seat *org.Role, base string, ref host.BoxRunRef,
	run map[string]string) (map[string]string, error) {

	sig, err := codeSigner{e: e}.Sign(gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: seat.Handle()})
	if err != nil {
		return nil, fmt.Errorf("engine: the commit identity of seat %s's executor: %w", seat.Handle(), err)
	}
	if e.boxTokens == nil {
		return nil, fmt.Errorf("engine: this node mints no coding box's credential")
	}
	token := e.boxTokens.Mint(ref)
	if token == "" {
		return nil, fmt.Errorf("engine: no credential can be minted for launch %s of run %s", ref.LaunchID,
			ref.TurnID)
	}
	return sandbox.BoxGit{Base: base, User: host.BoxUser, Token: token, Name: sig.AuthorName,
		Email: sig.AuthorEmail}.Env(run)
}

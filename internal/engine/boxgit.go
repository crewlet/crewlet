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
// [Engine.boxGitEnv] over run, the run environment the launch assembled, for
// the engine host's name in the box the launch makes; nil where this node
// gives a box no git: no [sandbox.BoxGitURLVar], or Gitlet not served now.
//
// SERVED IS [OperatorCode]'s ANSWER, the seam every `code_checkout` of the run
// resolves through ([CodeToolDeps]), so a box gets git exactly where a checkout
// can grant it a repository. Never [Engine.CodeUnserved]: it answers only once
// Gitlet was found absent, so gating on it gives no box git on any node and
// every checkout clones from an empty base.
func (e *Engine) launchBoxGit(seat *org.Role, turnID string,
	run map[string]string) func(launchID string, engineHost sandbox.EngineHost) (sandbox.LaunchEnv, error) {

	if _, served := OperatorCode(e); e.boxGitURL == "" || !served {
		return nil
	}
	base := e.boxGitURL
	return func(launchID string, engineHost sandbox.EngineHost) (sandbox.LaunchEnv, error) {
		return e.boxGitEnv(seat, base, engineHost, host.BoxRunRef{TurnID: turnID, LaunchID: launchID}, run)
	}
}

// boxGitEnv is the engine's git environment for ONE LAUNCH of a run on a node
// serving Gitlet, whatever the run checks out (ADR-0093), over run, the run
// environment the launch assembled: a token naming the turn and the launch id
// the store just minted (so a job a relaunch superseded is refused), which the
// launch writes into the box rather than any variable ([sandbox.LaunchEnv]),
// and Gitlet's base and git's configuration as a box behind engineHost dials
// them ([sandbox.BoxGit]). No commit trailers: the run's attribution hook
// ([sandbox.CommitAttribution]) is the one place they are spelled.
//
// The commit identity is [codeSigner]'s, the engine's one rule for a seat's
// commits, so the box's commits name the seat as the engine's do. Refused
// where the run environment's git configuration has a hole git refuses.
func (e *Engine) boxGitEnv(seat *org.Role, base string, engineHost sandbox.EngineHost, ref host.BoxRunRef,
	run map[string]string) (sandbox.LaunchEnv, error) {

	sig, err := codeSigner{e: e}.Sign(gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: seat.Handle()})
	if err != nil {
		return sandbox.LaunchEnv{}, fmt.Errorf("engine: the commit identity of seat %s's executor: %w",
			seat.Handle(), err)
	}
	if e.boxTokens == nil {
		return sandbox.LaunchEnv{}, fmt.Errorf("engine: this node mints no coding box's credential")
	}
	token, err := e.boxTokens.Mint(ref)
	if err != nil {
		return sandbox.LaunchEnv{}, fmt.Errorf("engine: no credential can be minted for launch %s of run %s: %w",
			ref.LaunchID, ref.TurnID, err)
	}
	return sandbox.BoxGit{Base: base, EngineHost: engineHost, User: host.BoxUser, Token: token,
		Name: sig.AuthorName, Email: sig.AuthorEmail}.Launch(run)
}

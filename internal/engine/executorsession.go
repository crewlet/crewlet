package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S SESSIONS, KEPT, AND EVERY TURN'S END RECORDED (ADR-0073), as
// the engine wires them into the sandbox runtime (internal/sandbox's
// session.go has the mechanism).
//
// # Through the router, wherever the seat runs
//
// A seat's box is wherever its seat is held, a seats-only node included, and
// only a data node publishes. So the coordinator records every capture and every
// end through the ESTATE ROUTER's writes ([Engine.executorLog]) — never a
// data node's own writer, which a seats-only node does not have — and they are
// the engine's alone: no package serving a person or a model reaches them
// (internal/estate's gate).
//
// # What the engine adds
//
// The company's word on keeping sessions, off the revision the FLEET runs
// ([executorKeep.Policy]); the secrets a capture scrubs by value
// ([executorKeep.Secrets]); the instant a turn's operations are minted at, the
// rule every operation id of a turn follows ([executorKeep.MintedAt]); the
// sealed upload ([executorKeep.Put]); and, at every way a turn ends, the
// account of how it ended ([Engine.endExecutorTurn]) — the turn's decision,
// whether its reply reached the party waiting, who stopped it, what lost it.

// executorKeep is the engine's part of keeping a run's harness session
// ([sandbox.SessionKeep]).
type executorKeep struct{ engine *Engine }

var _ sandbox.SessionKeep = executorKeep{}

// Policy is the company's word on keeping sessions, off the revision the fleet's
// activation pointer names ([Engine.executorPolicy]). A node with no object
// store keeps nothing, whatever the company says: there is nowhere to keep a
// session, and a capture refused for it on every launch would say only that.
func (k executorKeep) Policy(ctx context.Context) (sandbox.SessionPolicy, error) {
	if k.engine.Objects() == nil {
		return sandbox.SessionPolicy{}, nil
	}
	return k.engine.executorPolicy(ctx)
}

// executorPolicy is `turn_engine.conversation_session`'s switch and per-seat
// budget, as the revision the fleet's activation pointer names states them
// ([Engine.fleetCompany]), or why this node cannot know them.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.executorRetention]'s
// reason: the budget a capture carries decides which ended sessions every node
// releases, so a node serving a revision the fleet did not take must not decide
// it. A capture that cannot read it is refused, and its stamp says why.
func (e *Engine) executorPolicy(ctx context.Context) (sandbox.SessionPolicy, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return sandbox.SessionPolicy{}, err
	}
	if company.Config == nil {
		return sandbox.SessionPolicy{Keep: true, SeatBudget: config.DefaultSessionBytesPerSeat}, nil
	}
	session := company.Config.TurnEngine.ConversationSession
	return sandbox.SessionPolicy{Keep: session.KeepsSessions(), SeatBudget: session.SessionBudget()}, nil
}

// Secrets is every value the engine put by value in run's box that is a secret
// ([Engine.runSecrets]).
func (k executorKeep) Secrets(_ context.Context, run sandbox.PendingRun) []string {
	return k.engine.runSecrets(k.engine.Company(), run)
}

// MintedAt is the instant a turn's derived operations are minted at: its
// work's start, or the instant the turn rebased onto where that start is past
// the operation ledger's horizon — [rebaseFor], the one rule every operation id
// a turn derives follows, judged at this attempt's clock.
func (k executorKeep) MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error) {
	identity := builtin.Actor{TurnID: turnID, WorkKey: workKey, WorkSince: workSince}
	rebased, err := rebaseFor(ctx, k.engine.rebases(), identity, time.Now().UTC())
	if err != nil {
		return time.Time{}, err
	}
	identity.RebasedTo = rebased
	return identity.OperationSince(), nil
}

// Put seals archive under the fleet keyring as it streams and uploads it into
// an object the store mints ([Engine.ExecutorSessions]). It closes archive on
// every path, as the session store does.
func (k executorKeep) Put(ctx context.Context, handle, turn string,
	archive io.ReadCloser) (executordomain.SealedObject, error) {

	store, err := k.engine.ExecutorSessions()
	if err == nil && store == nil {
		err = errors.New("this node has no object store to keep a session in")
	}
	if err != nil {
		_ = archive.Close()
		return executordomain.SealedObject{}, err
	}
	return store.Put(ctx, handle, turn, archive)
}

// runSecrets is every value the engine put by value in run's box that is a
// secret, for the scrub its session is kept through: every value of the seat's
// run environment and setup files that a reference resolved (an operator's
// declared token — a value written in the document is the document's, and no
// secret), the sign-in in the environment of the entry the run launched on,
// and every string its login's files hold. Read off the company this node runs
// now, as the run's environment was off the one it launched under.
//
// NOT THE ISSUED TOKENS: a seat's GitHub App token is minted per launch and
// renewed as the run works, and reading it again would mint another, never the
// one the box holds. Those, and the run's bridge token, are caught by their
// shapes, which redact knows — the scrub's second pass, over every file.
func (e *Engine) runSecrets(c *Company, run sandbox.PendingRun) []string {
	if c == nil {
		return nil
	}
	resolver := e.resolver()
	var out []string
	fromReference := func(raw string) {
		if len(resolver.Missing(raw)) > 0 {
			return
		}
		if value := resolver.Value(raw); value != raw {
			out = append(out, value)
		}
	}
	var defaults []sandbox.SetupStep
	if c.catalogue != nil {
		defaults = c.catalogue.DefaultSetup()
	}
	settings := seatSandbox(c, run.AgentHandle)
	for _, step := range boxSetup(defaults, settings) {
		for _, raw := range step.Env {
			fromReference(raw)
		}
		for _, body := range step.Files {
			if content, unresolved := secrets.ReadContent(body, resolver.LookupOK); unresolved == "" && content != body {
				out = append(out, content)
			}
		}
	}
	if settings != nil {
		for _, raw := range settings.Env {
			fromReference(raw)
		}
	}
	agent := runHarness(c, run.Launch.ProviderKey)
	if agent == nil {
		return out
	}
	for name, value := range agent.SandboxEnv() {
		if entrySignInName(agent, name) {
			out = append(out, value)
		}
	}
	logins := make([]string, 0, 2)
	for _, path := range agent.SandboxCredentials() {
		logins = append(logins, path)
	}
	if doc := agent.SandboxCredentialDocument(); doc != "" {
		logins = append(logins, doc)
	}
	for _, path := range logins {
		raw, err := os.ReadFile(path) //nolint:gosec // the entry's own login, at the path its harness keeps it
		if err != nil {
			continue
		}
		out = append(out, executor.LoginValues(raw)...)
	}
	return out
}

// runHarness is the harness entry a run launched on, nil where the company no
// longer configures it as one.
func runHarness(c *Company, providerKey string) *harness.Provider {
	if c == nil || c.Models == nil || providerKey == "" {
		return nil
	}
	provider, ok := c.Models.Provider(providerKey)
	if !ok {
		return nil
	}
	agent, _ := provider.(*harness.Provider)
	return agent
}

// endExecutorTurn records on the executor log how a turn that launched an
// executor ended ([sandbox.Coordinator.EndTurn]), on a node whose sandbox
// runtime is up; nothing on one whose is not, which launched nothing.
func (e *Engine) endExecutorTurn(ctx context.Context, end sandbox.TurnEnd) {
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	rt.coordinator.EndTurn(ctx, end)
}

// runTurnEnd is the end of the turn run's row belongs to, ending as ending.
func runTurnEnd(run sandbox.PendingRun, ending executordomain.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID,
		ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Ask: run.TaskDescription, Ending: ending}
}

// telTurnEnd is the end of the turn tel describes, ending as ending: for the
// frame that launched the turn's executor, which ends a turn whose run was
// stopped before the turn parked on it.
func telTurnEnd(tel turnTelemetry, ending executordomain.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: tel.handle, TurnID: tel.runID, ConversationKey: tel.convKey,
		WorkKey: tel.workKey, WorkSince: tel.workSince, Ask: tel.title, Ending: ending}
}

// endingOf is how a turn whose loop returned res ended, as its end records it:
// FAILED where a guard ended it or its decision was a failure, COMPLETED
// otherwise — and the turn's own decision and breach, never the reviewer's last
// word, which still reads `done` after the loop overrode it; whether its reply
// reached the party waiting, and on what; what the reviewer said landed; what
// stopped a blocked round. answered is the engine's own answer to a colleague's
// ask, where the turn owed one.
func endingOf(res turn.Result, reply turn.Reply, answered colleagueAnswer) executordomain.Ending {
	ending := executordomain.Ending{Outcome: executordomain.OutcomeCompleted, Decision: string(res.Decision),
		Delivered: res.Delivered, DeliveredTo: replySurface(reply)}
	if res.Breach != nil || res.Decision == phase.Failed {
		ending.Outcome = executordomain.OutcomeFailed
	}
	if res.Breach != nil {
		ending.Failure = breachAccount(res.Breach)
	}
	if res.LastReview != nil {
		ending.CompletedWork = res.LastReview.CompletedWork
	}
	if w := res.LastWork; w != nil && w.Outcome == turn.OutcomeBlocked {
		ending.BlockedOn = w.Evidence
	}
	answered.onto(&ending)
	return ending
}

// breachAccount is the end's account of the guard that ended a turn.
func breachAccount(b *turn.Breach) string {
	account := "the turn stopped on the engine's " + string(b.Kind) + " guard"
	if detail := strings.TrimSpace(b.Detail); detail != "" {
		account += ": " + detail
	}
	return account
}

// replySurface is where the party waiting on a turn waits, as its end names it:
// the surface a tool reply is owed on, `a2a` for the colleague the engine
// answers, nothing where nobody waits.
func replySurface(reply turn.Reply) string {
	switch reply.Kind {
	case turn.ReplyTool:
		return reply.Surface
	case turn.ReplyEngine:
		return replyA2A
	}
	return ""
}

// replyA2A is the surface a colleague's ask is answered on.
const replyA2A = "a2a"

// stoppedEnding is how a turn a person stopped ended: stopped, by whom and why
// — the reason in their own words, which the next turn is told fenced as theirs
// (the ending's field is declared somebody else's) — beside whatever the turn's
// loop had decided and delivered by then.
func stoppedEnding(res turn.Result, reply turn.Reply, stop error, answered colleagueAnswer) executordomain.Ending {
	ending := endingOf(res, reply, answered)
	ending.Outcome, ending.Failure = executordomain.OutcomeStopped, ""
	if pause, ok := stopOf(stop); ok {
		ending.StoppedBy, ending.StopReason = pause.By, pause.Reason
	}
	return ending
}

// runDelivered is whether the party waiting on run's turn has its answer, as
// the run's own record of its bridged calls says — [turn.Answered], the rule a
// turn's loop answers it by, over the calls of the round the run was in — for
// an ending no frame of the turn reaches: a loss, a stop.
func (e *Engine) runDelivered(c *Company, run sandbox.PendingRun) (turn.Reply, bool) {
	reply := turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return reply, false
	}
	calls := make([]ledger.Call, 0, len(run.BridgeCalls))
	for _, call := range run.BridgeCalls {
		calls = append(calls, ledger.Call{Name: call.Name, Failed: call.Failed})
	}
	return reply, turn.Answered(calls, seatSurface(e.seatRegistry(c, run.AgentHandle)), reply)
}

// seatSurface is the delivery surface a seat's calls are judged against, as its
// registry holds it: which of its tools reach somebody, and where.
func seatSurface(reg *tools.Registry) turn.Surface {
	if reg == nil {
		return turn.Surface{}
	}
	u := reg.Snapshot()
	return turn.Surface{Catalogue: u.Names(), Deliveries: u.Deliveries(), KnownReads: u.KnownReads(),
		KnownOpenWorld: u.KnownOpenWorld(), Discoverable: u.MCPNames()}
}

// runEnding is how a run's turn ended where the coordinator's ending, not the
// turn's loop, ended it: lost, or stopped by a person — whether its reply
// reached anybody read off the run's own calls, and the engine's answer to a
// colleague's ask beside it.
func (e *Engine) runEnding(c *Company, run sandbox.PendingRun, answered colleagueAnswer) executordomain.Ending {
	reply, delivered := e.runDelivered(c, run)
	ending := executordomain.Ending{Delivered: delivered, DeliveredTo: replySurface(reply)}
	switch {
	case run.Stop != nil:
		ending.Outcome = executordomain.OutcomeStopped
		ending.StoppedBy, ending.StopReason = run.Stop.By, run.Stop.Reason
	default:
		ending.Outcome = executordomain.OutcomeLost
		if run.Ending != nil {
			ending.Failure = redact.Secrets(fmt.Sprintf("the executor's run was lost (%s): %s",
				run.Ending.Reason, run.Ending.Detail))
		}
	}
	answered.onto(&ending)
	return ending
}

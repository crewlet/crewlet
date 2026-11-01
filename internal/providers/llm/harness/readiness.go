package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// WHETHER THE NEXT CALL CAN BE ANSWERED, known without making one: what a
// caller that asks on a person's behalf (the Dashboard Assistant) checks before
// it takes their question, so a model that is signed out or spent is said up
// front, in its own words, rather than after the person has asked and waited.
//
// # Only what the entry already holds
//
// [Provider.Unready] never starts a process and never calls a model. It reads
// three things the entry already has:
//
//   - its sit-out ([Provider.bench]): a spent plan, until the reset its CLI
//     named;
//   - what its own last call met: a CLI that reported its login expired or
//     missing, or a CLI that could not be started at all;
//   - its sign-in as `crewlet llm list` reads it ([Provider.signIn]): nothing
//     the engine hands the CLI signs it in.
//
// # A verdict that cannot lock the entry out
//
// A login is not benched (bench.go): calls keep reaching the CLI, so the next
// seat to call finds a mended login. A caller that REFUSES on this verdict
// breaks that loop, since the refusal is what stops the call that would clear
// it. So a verdict lifts on its own three ways: a call through the entry
// answers (a seat's, on the same entry); the login files change (`crewlet llm
// login` from another process); or [readinessMemory] passes, after which the
// entry is worth one call again and the verdict waits for what that call
// meets. The last is what keeps a wrong verdict bounded: the sign-in reading
// cannot see a credential a CLI keeps where no profile names it, which is the
// doctor's reason to let an answered smoke test overrule it.

// readinessMemory is how long a verdict holds without new evidence: two of the
// dashboard's reads of `/viewer` (every five minutes), so the person who opens
// the assistant after a failure is told before they ask. Past it the entry is
// tried again, which costs one CLI start that exits at once if the login is
// still missing; a wrong verdict keeps the entry from its callers for no
// longer than this.
const readinessMemory = 10 * time.Minute

// readiness is the sign-in's memory of what its calls met, held in its [Plan]
// and guarded by the plan's mutex.
type readiness struct {
	// verdict is the failure the next call is expected to meet, nil for
	// none; since is when it was established and stamp the login files as
	// they were then.
	verdict error
	since   time.Time
	stamp   string
	// retry is a verdict that lifted with nothing tried since: the entry is
	// owed one call before the sign-in reading may hold it again.
	retry bool
	// answered is a call through this entry that answered: whatever the
	// sign-in reading says, something signs it in.
	answered bool
}

// Unready is the failure this entry's next call would meet as far as it knows
// without making one, or nil when nothing says it would fail: a spent plan
// ([llm.KindRateLimit] with its [llm.Error.RetryAt]), a login its CLI last
// reported expired or missing, or nothing that signs the CLI in
// ([llm.KindAuth]), or a CLI that could not be started ([llm.KindFatal]
// wrapping [llm.ErrNotRunnable]). See readiness.go for what lifts each.
func (p *Provider) Unready(ctx context.Context) error {
	if err := p.SittingOut(); err != nil {
		return err
	}
	stamp := p.LoginStamp()
	missing := p.missingSignIn(ctx)
	now := p.now()

	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	r := &plan.ready
	if r.verdict != nil && (r.stamp != stamp || now.Sub(r.since) >= readinessMemory) {
		r.verdict, r.retry = nil, true
	}
	if r.verdict == nil && missing != nil && !r.retry && !r.answered {
		r.verdict, r.since, r.stamp = missing, now, stamp
	}
	return r.verdict
}

// remember records what a call met, for [Provider.Unready]: an answer clears
// every verdict, and a login the CLI refused or a CLI that could not start
// sets one. Anything else (a timeout, a spent plan the bench already holds, a
// CLI that ran and failed) says nothing about the next call's sign-in and
// leaves the memory as it was.
func (p *Provider) remember(err error) {
	var stamp string
	verdict := err != nil && (llm.KindOf(err) == llm.KindAuth || errors.Is(err, llm.ErrNotRunnable))
	if verdict {
		stamp = p.LoginStamp()
	}
	now := p.now()
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	switch {
	case err == nil:
		plan.ready.verdict, plan.ready.retry, plan.ready.answered = nil, false, true
	case verdict:
		plan.ready.verdict, plan.ready.since, plan.ready.stamp, plan.ready.retry = err, now, stamp, false
		// WHAT A PERSON IS SHOWN, not only what the next call is told: the
		// refusal is the sign-in's last failure until an answer outdates it
		// ([PlanState.LastFailure]).
		if plan.lastFailure == nil || !now.Before(plan.lastFailure.At) {
			plan.lastFailure = &Failure{Kind: llm.KindOf(err), At: now.UTC(),
				Said: boundSaid(firstLine(failureSaid(err)))}
		}
	}
}

// failureSaid is what a classified failure says, without the classification's
// own prefix: the vendor's sentence where the CLI gave one.
func failureSaid(err error) string {
	var failure *llm.Error
	if errors.As(err, &failure) && failure.Err != nil {
		return failure.Err.Error()
	}
	return err.Error()
}

// missingSignIn is the classified failure of an entry nothing signs in, or nil:
// no route [Provider.signIn] can see, on a profile that declares the routes
// its CLI signs in by, or a credential reference the operator set and this
// process could not resolve. Two profiles are not judged on configuration
// alone: one that declares no route at all (the engine cannot see how its CLI
// signs in), and one that reads its provider's key from the environment
// ([harnessprofile.Profile.EnvSignIn]) with nothing configured, which may front an
// endpoint that takes no key.
func (p *Provider) missingSignIn(ctx context.Context) error {
	s := p.signIn(ctx)
	switch {
	case len(s.sources) > 0:
		return nil
	case s.unreadable != nil:
		// A shared login that cannot be read is not judged either: whether
		// its files sign the CLI in is unknown rather than no
		// ([signIn.unreadable]), and the call itself reports why it was
		// refused.
		return nil //nolint:nilerr // unknown is not a verdict, see above
	}
	if len(s.unresolved) == 0 && (p.profile.EnvSignIn || !p.declaresSignIn()) {
		return nil
	}
	return p.fail(llm.KindAuth, fmt.Errorf("no sign-in: no credential files in %s, and "+
		"nothing in the environment the CLI is given authenticates it", p.ws.CredentialsDir()))
}

// declaresSignIn reports whether the profile names any route [Provider.signIn]
// reads: credential files, a credential store, a token or a key variable.
func (p *Provider) declaresSignIn() bool {
	prof := p.profile
	return len(prof.CredentialPaths) > 0 || prof.CredentialStore != nil ||
		prof.TokenEnv != "" || prof.APIKeyEnv != "" || len(prof.SignInEnv) > 0
}

// LoginStamp identifies the entry's login files as they are now: each file the
// shared login may hold ([Workspace.loginNames]) that is there, by its name,
// size and modification time, so a login written or removed by another
// process reads as a change without reading a credential.
func (p *Provider) LoginStamp() string {
	var b strings.Builder
	for _, name := range p.ws.loginNames() {
		path := p.ws.sharedPath(name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}

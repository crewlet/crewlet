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

// WHETHER THE NEXT CALL CAN BE ANSWERED, known without making one, so a caller
// asking for a person (the Dashboard Assistant) can say a signed-out or spent
// model up front. [Provider.Unready] starts no process and calls no model: it
// reads the entry's sit-out ([Provider.bench]), what its last call met and its
// sign-in as `crewlet llm list` reads it ([Provider.signIn]).
//
// A verdict must not lock the entry out: a login is not benched (bench.go), and
// a refusing caller stops the call that would find a mended login. So it lifts
// when a call through the entry answers, the login files change, or
// [readinessMemory] passes, after which one call is owed. The last bounds a
// wrong verdict: the sign-in reading cannot see a credential kept where no
// profile names it, so the doctor lets an answered smoke test overrule.

// readinessMemory is how long a verdict holds without new evidence: two of the
// dashboard's reads of `/viewer` (every five minutes), so the person who opens
// the assistant after a failure is told before they ask. Past it the entry is
// tried again, which costs one harness start that exits at once if the login is
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
// ([llm.KindRateLimit] with its [llm.Error.RetryAt]), a login its harness last
// reported expired or missing, or nothing that signs the harness in
// ([llm.KindAuth]), or a harness that could not be started ([llm.KindFatal]
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

// KnownUnready is the verdict [Provider.Unready] holds now, resolving no
// sign-in: what an LLM fleet's pick reads, which makes no call and waits on
// nothing. Nil where no verdict holds, or the login changed or the memory
// passed since it was reached.
func (p *Provider) KnownUnready() error {
	stamp := p.LoginStamp()
	now := p.now()
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	r := &plan.ready
	if r.verdict == nil || r.stamp != stamp || now.Sub(r.since) >= readinessMemory {
		return nil
	}
	return r.verdict
}

// remember records what a call met, for [Provider.Unready]: an answer clears
// every verdict, and a login the harness refused or a harness that could not start
// sets one. Anything else (a timeout, a spent plan the bench already holds, a
// harness that ran and failed) says nothing about the next call's sign-in and
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
// own prefix: the vendor's sentence where the harness gave one.
func failureSaid(err error) string {
	var failure *llm.Error
	if errors.As(err, &failure) && failure.Err != nil {
		return failure.Err.Error()
	}
	return err.Error()
}

// missingSignIn is the classified failure of an entry nothing signs in, or nil:
// no route [Provider.signIn] can see, on a profile that declares the routes
// its harness signs in by, or a credential reference the operator set and this
// process could not resolve. Two profiles are not judged on configuration
// alone: one that declares no route at all (the engine cannot see how its harness
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
		// its files sign the harness in is unknown rather than no
		// ([signIn.unreadable]), and the call itself reports why it was
		// refused.
		return nil //nolint:nilerr // unknown is not a verdict, see above
	}
	if len(s.unresolved) == 0 && (p.profile.EnvSignIn || !p.declaresSignIn()) {
		return nil
	}
	return p.fail(llm.KindAuth, fmt.Errorf("no sign-in: no credential files in %s, and "+
		"nothing in the environment the harness is given authenticates it", p.ws.CredentialsDir()))
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

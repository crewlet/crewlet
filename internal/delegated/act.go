package delegated

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

// Act is what an ask is for, as the engine set it on a question it composed
// from structured data (ADR-0125): its kind, the option whose choice approves
// it, and the digest of exactly what that choice approves. An ask carrying
// none is [KindOther].
type Act struct {
	Kind    Kind   `json:"kind"`
	Approve string `json:"approve"`
	Digest  string `json:"digest"`
	// Host is an ssh_operate act's server, by its id.
	Host string `json:"host,omitempty"`
}

// digestHex is a SHA-256 in hex, compiled at first use.
var digestHex = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{64}$`) })

// Validate holds an act to a delegable kind, an approving option, a digest,
// and a host for an ssh_operate act.
func (a Act) Validate() error {
	switch {
	case !a.Kind.Delegable():
		return fmt.Errorf("an act's kind is one a person's choice can approve, never %q", a.Kind)
	case a.Approve == "":
		return fmt.Errorf("an act names the option whose choice approves it")
	case !digestHex().MatchString(a.Digest):
		return fmt.Errorf("an act carries the SHA-256 of what it approves, in hex")
	case a.Kind == KindSSHOperate && a.Host == "":
		return fmt.Errorf("an %s act names its server", KindSSHOperate)
	}
	return nil
}

type actKey struct{}

// WithAct is ctx carrying the act the engine composed for the ask a tool is
// about to post: the one way an act reaches an ask.
func WithAct(ctx context.Context, a Act) context.Context { return context.WithValue(ctx, actKey{}, a) }

// ActOf is the act ctx carries, false for none.
func ActOf(ctx context.Context) (Act, bool) {
	a, ok := ctx.Value(actKey{}).(Act)
	return a, ok
}

type approvalKey struct{}

// WithApproval is ctx carrying that its person may approve kind now, at the
// proof that kind's approval asks: decided by the caller before an answer is
// posted, and read where the answer's choice is checked against its ask.
func WithApproval(ctx context.Context, kind Kind) context.Context {
	return context.WithValue(ctx, approvalKey{}, kind)
}

// Approves reports whether ctx carries an approval of kind.
func Approves(ctx context.Context, kind Kind) bool {
	k, ok := ctx.Value(approvalKey{}).(Kind)
	return ok && k == kind
}

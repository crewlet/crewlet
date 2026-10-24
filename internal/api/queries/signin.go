// How a person signs in on this node, for the administrators who answer for it.

package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// SignInPolicy is this node's sign-in posture: whether a second factor is
// required, the password floor, and how long a session and a proof of
// identity last.
//
// TIER A, AND THIS NODE'S. Every value here is read from `api.auth` in the
// node's own file, which no apply changes and no other node shares: a fleet
// whose nodes disagree is a legal state, as the admin posture beside it is,
// and a session lives by the rules of the node that minted it. So the answer
// says which node it describes, and a screen that shows it names that node
// rather than presenting the reading as the company's.
//
// POSTURE, NOT A SECRET: nothing here is a credential, a token or a name, and
// two of the five are already public on `/auth/config`, where a sign-in form
// reads them before anybody has signed in. The lifetimes are not, because
// that route is unguarded and says only what a form needs; they are served
// here, to administrators, because an administrator is who answers "how long
// do sessions last here" and the only other place it was written was the
// file on the host.
//
// EVERY DURATION IN WHOLE SECONDS, the unit every other duration on this
// surface is spelled in, each the value the engine ENFORCES: a setting the
// file leaves out is answered as its default, never as zero.
type SignInPolicy struct {
	// SecondFactor is `required` or `optional`, as [config.APIAuth.SecondFactor]
	// reads it: unset is required.
	SecondFactor iam.SecondFactor `json:"second_factor"`
	// MinPasswordLength is the floor every password is held to, as
	// [config.APIAuth.Passwords] reads it.
	MinPasswordLength int `json:"min_password_length"`
	// SessionAbsoluteSeconds is the longest a session lives from sign-in,
	// however it is used (`api.auth.session.absolute`).
	SessionAbsoluteSeconds int64 `json:"session_absolute_seconds"`
	// SessionIdleSeconds is how long a session lives unused before it must
	// sign in again. A constant of the engine ([session.Idle]) rather than a
	// setting, because it rides inside every signed cookie and two nodes
	// disagreeing would hand one browser two expiries.
	SessionIdleSeconds int64 `json:"session_idle_seconds"`
	// StepUpSeconds is how long a proof of identity authorises a sensitive
	// gesture before it is asked for again (`api.auth.session.step_up`).
	StepUpSeconds int64 `json:"step_up_seconds"`
}

// SignInPolicyOf is the posture a node's `api.auth` block enforces, every
// value read through the accessor its enforcer reads, so this answer and the
// sign-in path cannot disagree about a default.
func SignInPolicyOf(auth config.APIAuth) SignInPolicy {
	return SignInPolicy{
		SecondFactor:           auth.SecondFactor(),
		MinPasswordLength:      auth.Passwords(),
		SessionAbsoluteSeconds: seconds(auth.Session.Absolute()),
		SessionIdleSeconds:     seconds(session.Idle),
		StepUpSeconds:          seconds(auth.Session.StepUp()),
	}
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// SignInPolicyAnswer is what `sign_in_policy` answers: this node's posture
// and the node it describes.
type SignInPolicyAnswer struct {
	// Node is the node whose Tier A this is ([Sources.NodeID]).
	Node   string       `json:"node"`
	Policy SignInPolicy `json:"policy"`
}

// signInPolicy answers this node's [SignInPolicy], beside the node it
// describes.
func (s Sources) signInPolicy(context.Context, Params) (any, error) {
	return SignInPolicyAnswer{Node: s.NodeID, Policy: *s.SignIn}, nil
}

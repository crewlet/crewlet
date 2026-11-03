package googlecloud

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// SetClock replaces the time a client reads and waits by, so a test's
// three-minute bound passes in no time.
func SetClock(c *Client, now func() time.Time, sleep func(context.Context, time.Duration) error) {
	c.clock = clock{now: now, sleep: sleep}
}

// AwaitObserved is the one read-back wait, on a test's clock.
func AwaitObserved(
	ctx context.Context, now func() time.Time, sleep func(context.Context, time.Duration) error,
	what string, check func() (bool, error),
) (time.Duration, error) {
	return clock{now: now, sleep: sleep}.awaitObserved(ctx, readBack(what), check)
}

// ReadBacks is every kind of read that follows a write.
func ReadBacks() []string {
	return []string{string(readCreated), string(readGrantable), string(readDelegated), string(readBindings),
		string(readDisabled), string(readEnabled), string(readDeleted), string(readLastWrites)}
}

// ObserveBudget is the bound every read-back waits through.
const ObserveBudget = observeBudget

// ConditionRoles is the roles a level condition lists, false for any other
// expression.
func ConditionRoles(expression string) ([]string, bool) { return conditionRoles(expression) }

// AccountNotYet reports Google's answer for an account not yet grantable.
func AccountNotYet(err error, email string) bool { return accountNotYet(err, email) }

// Grant and Revoke are the policy edits a write makes.
func Grant(pol *Policy, who string, roles []string) bool  { return grant(pol, who, roles) }
func Revoke(pol *Policy, who string, roles []string) bool { return revoke(pol, who, roles) }

// HeldBy is the roles pol grants who: unconditional, then conditional.
func HeldBy(pol Policy, who string) ([]string, []string) {
	h := heldBy(pol, who)
	return h.plain, h.conditional
}

// Never and CredentialRule are the two patterns the level table is held to.
func Never(permission string) bool          { return never.MatchString(permission) }
func CredentialRule(permission string) bool { return credential.MatchString(permission) }

// Barred is what a level's role holds that the level may not.
func Barred(level string, permissions []string) []string { return barred(level, permissions) }

// PredefinedRoles is every predefined role a level grants, by that level.
func PredefinedRoles() map[string]string { return predefinedRoles() }

// Reject is how the loop reads a failure: the credential's, or a fault.
func Reject(err error) error { return reject(err) }

// Seat-facing names a test asserts on.
func DisplayName(company, handle string) string { return displayName(company, handle) }
func Description(company, handle string) string { return description(company, handle) }

// OpenAccess opens the registered access vendor with a transport pointed at a
// fake and a clock, which the production Open cannot be handed.
func OpenAccess(
	vendor seataccess.Vendor, in seataccess.OpenInput, hc *http.Client,
	now func() time.Time, sleep func(context.Context, time.Duration) error,
) (seataccess.Session, error) {
	return vendor.(accessVendor).open(in, hc, &clock{now: now, sleep: sleep})
}

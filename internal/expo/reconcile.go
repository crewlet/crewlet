package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/integration"
)

// Options is what one pass over the company's half of Expo needs: the
// provisioner token and the account. Each agent's own robot is the per-seat
// access sync's ([accessVendor]), which a setup pass runs beside this one.
type Options struct {
	// Client carries the provisioner token.
	Client *Client

	// Account is the account name, resolved.
	Account string
}

// Result is what one pass found. It writes nothing at Expo.
type Result struct {
	// Viewer is who the provisioner token acts as.
	Viewer Actor

	// Account is the account, where the token could see it.
	Account Account

	// AccountMissing is an account name the token sees no account under.
	AccountMissing bool

	// Permissions are what the token's actor holds on the account.
	Permissions []Permission

	// Apps is how many projects the account has.
	Apps int

	name string
}

// Reconcile reads the provisioner token and the account and reports what an
// operator has to act on. A token Expo refuses is an error wrapping
// [integration.ErrCredentialRejected]: nothing else a pass reports could be
// trusted over it.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("expo: no client")
	}
	if opts.Account == "" {
		return nil, integration.ErrNotConfigured
	}
	in, err := inspect(ctx, opts.Client, opts.Account, nil)
	res := &Result{Viewer: in.viewer, Account: in.account, Permissions: in.perms, Apps: in.apps,
		name: opts.Account}
	if err == nil {
		return res, nil
	}
	st, staged := errors.AsType[*stageError](err)
	status := Status(err)
	if staged && st.stage == stageAccount && (status == http.StatusNotFound || status == http.StatusForbidden) {
		res.AccountMissing = true
		return res, nil
	}
	return nil, reconcileError(err)
}

// reconcileError is a failed read as the loop reads it: a refusal is the
// credential's, anything else a fault it retries.
func reconcileError(err error) error {
	where := stageViewer.name
	if st, ok := errors.AsType[*stageError](err); ok {
		where = st.stage.name
	}
	rejected := integration.Reject(err, Status(err))
	return fmt.Errorf("expo: the provisioner token in integrations.expo.provisioning.access_token "+
		"%s reading %s: %w", integration.Refusal(rejected), where, rejected)
}

// The subjects a company finding names: the fields that clear it.
const (
	subjectToken   = "integrations.expo.provisioning.access_token"
	subjectAccount = "integrations.expo.provisioning.account_name"
)

// Findings is what the pass saw, in the shared vocabulary. Each agent's own
// robot is reported per seat by the access sync.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	switch {
	case r.AccountMissing:
		out = append(out, integration.Finding{
			Kind: integration.FindingUnknownTier, Subject: subjectAccount,
			Title: "Account not found",
			Detail: fmt.Sprintf("Expo shows the provisioner token no account named %q, so no "+
				"agent's robot user can be created", r.name),
			Remedy: "Correct integrations.expo.provisioning.account_name, or replace the " +
				"provisioner token with one from a member of that account",
			ActionURL: AccessTokensURL(r.name),
		})
	case !administers(r.Permissions):
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Subject: subjectToken,
			Title: "Provisioner token lacks the Admin role",
			Detail: fmt.Sprintf("The provisioner token acts as %s, which holds %s on %s, and "+
				"creating, changing and deleting robot users needs the Admin role",
				actorWords(r.Viewer), permissionWords(r.Permissions), r.Account.Name),
			Remedy: "Give it the Admin role on the account at Expo, or replace the token " +
				"with one whose user or robot has it",
			ActionURL: MembersURL(r.name),
		})
	}
	return out
}

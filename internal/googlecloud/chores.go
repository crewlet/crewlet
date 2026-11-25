package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/words"
)

// EnablerRoleID is the custom role the Cloud Shell block makes in the identity
// project and grants the provisioner, so Crewlet turns a Google product's APIs
// on itself and the block never changes when a product is turned on.
const EnablerRoleID = "crewletApiEnabler"

// EnablerPermission is all [EnablerRoleID] holds: reading what is on, and the
// operation an enable answers, come with Service Usage Viewer. Service Usage
// Admin would add turning an API off and changing quotas.
const EnablerPermission = "serviceusage.services.enable"

// What the company reader says of itself in the console; an account by its
// name that says otherwise is not Crewlet's, and is never adopted.
const (
	readerDisplayName = "Crewlet growth reader"
	readerDescription = "Reads growth figures for the whole company, for Crewlet"
)

// subjectReader names the company reader in a finding.
const subjectReader = "the company reader"

// ConvergeGrowthProducts does the identity project's chores for the Google
// growth products the company turns on: their APIs on, and the company's
// reader made and trusting the company's federated subject alone. A
// provisioner the block has not let turn APIs on is one finding, raised
// whether or not a product waits on it, so a company connected before is told
// to run the block again once. A fault Google or the context answers is the
// error; every refusal is a finding.
func ConvergeGrowthProducts(
	ctx context.Context, c *Client, company string, products []googleauth.Product,
) ([]integration.Finding, error) {
	may, err := MayEnableAPIs(ctx, c)
	if err != nil {
		return chore(ctx, c, "read whether the provisioner may turn APIs on", "apis", err)
	}
	var findings []integration.Finding
	if !may {
		findings = append(findings, RunBlockAgain(c.IdentityProject()))
	}
	if len(products) == 0 {
		return findings, nil
	}
	if may {
		enabled, eerr := enableAPIs(ctx, c, products)
		if eerr != nil {
			return nil, eerr
		}
		findings = append(findings, enabled...)
	}
	reader, err := holdReader(ctx, c, company)
	if err != nil {
		return nil, err
	}
	return append(findings, reader...), nil
}

// MayEnableAPIs reports whether the provisioner holds [EnablerPermission] on
// the identity project.
func MayEnableAPIs(ctx context.Context, c *Client) (bool, error) {
	held, err := c.TestProjectPermissions(ctx, c.IdentityProject(), []string{EnablerPermission})
	return slices.Contains(held, EnablerPermission), err
}

// RunBlockAgain is the one finding of a connection whose block predates
// [EnablerRoleID]: running the current block once grants it.
func RunBlockAgain(identity string) integration.Finding {
	return integration.Finding{
		Kind: integration.FindingApprovalRequired, Subject: subjectIdentity,
		Title: "Run the Cloud Shell block again, once",
		Detail: "Crewlet now turns on each Google product's APIs and makes the company reader itself, and the block " +
			"that connected " + identity + " did not let it",
		Remedy: "Open Settings, copy the Cloud Shell block and run it once. Turning on another Google product " +
			"then needs no block",
	}
}

// MissingAPIs is what of product's APIs is not on in the identity project,
// sorted.
func MissingAPIs(ctx context.Context, c *Client, products ...googleauth.Product) ([]string, error) {
	enabled, err := c.EnabledServices(ctx, c.IdentityProject())
	if err != nil {
		return nil, err
	}
	var off []string
	for _, p := range products {
		for _, api := range p.APIs {
			if !enabled[api] && !slices.Contains(off, api) {
				off = append(off, api)
			}
		}
	}
	slices.Sort(off)
	return off, nil
}

// enableAPIs turns on what products need and is off, and waits until it reads
// back on.
func enableAPIs(ctx context.Context, c *Client, products []googleauth.Product) ([]integration.Finding, error) {
	off, err := MissingAPIs(ctx, c, products...)
	if err != nil {
		return chore(ctx, c, "read the APIs on "+c.IdentityProject(), "apis", err)
	}
	if len(off) == 0 {
		return nil, nil
	}
	if err := c.EnableServices(ctx, c.IdentityProject(), off); err != nil {
		return chore(ctx, c, "turn on "+words.And(unquoted(off)), "apis", err)
	}
	if _, err := awaitObserved(ctx, c.clock(), readAPIsOn, func() (bool, error) {
		still, rerr := MissingAPIs(ctx, c, products...)
		return rerr != nil || len(still) == 0, rerr
	}); err != nil {
		return chore(ctx, c, "read back the APIs it turned on", "apis", err)
	}
	return nil, nil
}

// holdReader makes the company's reader where it is missing and lets the
// company's federated subject alone act as it. ONLY CREWLET'S: an account by
// its name that does not say what Crewlet writes on it is never adopted.
func holdReader(ctx context.Context, c *Client, company string) ([]integration.Finding, error) {
	id := googleauth.ReaderAccountID(company)
	email := googleauth.AccountEmail(id, c.IdentityProject())
	sa, found, err := c.ServiceAccount(ctx, email)
	if err != nil {
		return chore(ctx, c, "read "+email, subjectReader, err)
	}
	if !found {
		sa, err = c.CreateServiceAccount(ctx, id, readerDisplayName, readerDescription)
		if googleauth.Status(err) == http.StatusConflict {
			sa, found, err = c.ServiceAccount(ctx, email)
			if err == nil && !found {
				err = &Settling{What: string(readCreated), Reason: ReasonCreating}
			}
		}
		if err != nil {
			return chore(ctx, c, "make "+email, subjectReader, err)
		}
		if _, werr := awaitObserved(ctx, c.clock(), readCreated, func() (bool, error) {
			_, found, rerr := c.ServiceAccount(ctx, email)
			return found || (rerr != nil && !gone(rerr)), rerr
		}); werr != nil {
			return chore(ctx, c, "read back "+email, subjectReader, werr)
		}
	}
	switch {
	case sa.Description != readerDescription:
		return []integration.Finding{{
			Kind: integration.FindingIdentityFailed, Subject: subjectReader,
			Detail: "a service account " + email + " already exists, and it is not one Crewlet made",
			Remedy: "Nothing is adopted by its name. Delete it in the identity project if nobody uses it; " +
				"Crewlet makes the reader on its next pass",
			ActionURL: projectURL(ServiceAccountsURL, c.IdentityProject()),
		}}, nil
	case sa.Disabled:
		return []integration.Finding{{
			Kind: integration.FindingApprovalRequired, Subject: subjectReader,
			Detail:    "somebody turned " + email + " off at Google, and Crewlet never turns it back on unasked",
			Remedy:    "Enable it in the identity project, or turn the company's Google growth products off",
			ActionURL: projectURL(ServiceAccountsURL, c.IdentityProject()),
		}}, nil
	}
	principal := googleauth.Principal(c.auth.Provider(), company)
	trusts := func() (bool, error) {
		pol, rerr := c.AccountPolicy(ctx, email)
		return rerr == nil && slices.Contains(heldBy(pol, principal).plain, workloadIdentityUser), rerr
	}
	ok, err := trusts()
	if err != nil {
		return chore(ctx, c, "read who may act as "+email, subjectReader, err)
	}
	if ok {
		return nil, nil
	}
	if err := c.EditAccountPolicy(ctx, email, func(p *Policy) bool {
		return grant(p, principal, []string{workloadIdentityUser})
	}); err != nil {
		return chore(ctx, c, "let Crewlet act as "+email, subjectReader, err)
	}
	if _, err := awaitObserved(ctx, c.clock(), readDelegated, func() (bool, error) {
		ok, rerr := trusts()
		return ok || rerr != nil, rerr
	}); err != nil {
		return chore(ctx, c, "read back who may act as "+email, subjectReader, err)
	}
	return nil, nil
}

// workloadIdentityUser lets a federated principal act as an account.
const workloadIdentityUser = "roles/iam.workloadIdentityUser"

// chore is the answer to a chore's step doing that failed with err: the error
// where a later pass clears it, a pending finding while Google settles, and
// otherwise the refusal with what to check.
func chore(ctx context.Context, c *Client, doing, subject string, err error) ([]integration.Finding, error) {
	if googleauth.Passes(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil {
		return nil, fmt.Errorf("googlecloud: %s: %w", doing, err)
	}
	if reason, settling := SettlingReason(err); settling {
		return []integration.Finding{{Kind: integration.FindingGrantPending, Subject: subject,
			Detail: reason + ": Crewlet could not yet " + doing}}, nil
	}
	return []integration.Finding{{
		Kind: integration.FindingIdentityFailed, Subject: subject,
		Detail:    "Crewlet could not " + doing + ": " + err.Error(),
		Remedy:    "Run the Cloud Shell block again so the provisioner holds its roles on " + c.IdentityProject(),
		ActionURL: CloudShellURL,
	}}, nil
}

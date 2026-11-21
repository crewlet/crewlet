package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
)

// A seat's growth identity is the second service account the engine makes for
// it in the identity project, the one Google's growth products know the seat
// as (internal/googleauth). It is made and turned off here, by the
// provisioner's own steps, because they are the Google Cloud seat account's
// steps: create and read back, grant the provisioner Token Creator on it alone,
// disable, enable. Its record is the core's ([coord.GoogleIdentity]), written
// only by [ConvergeGrowthIdentities], and ALWAYS BEFORE the step at Google it
// names, so a pass cut short leaves an account Crewlet knows is its own.

// GrowthSeat is one agent seat as the Google identity pass reads it: whether
// any of its Google growth lanes is on.
type GrowthSeat struct {
	Handle string
	On     bool
}

// growthDisplayName is what the console lists a growth identity as: the seat
// account's name ([displayName]) with " growth", so the orphan check reads
// both alike.
func growthDisplayName(company, handle string) string {
	return displayName(company, strings.TrimSpace(handle)+" growth")
}

// growthDescription is what a growth identity says of itself in the console.
func growthDescription(company, handle string) string {
	return cut("Managed by Crewlet: agent "+strings.TrimSpace(handle)+" of "+strings.TrimSpace(company)+
		" in Google's growth products, never Google Cloud. Do not edit.", descriptionMax)
}

// ConvergeGrowthIdentities holds each seat's growth identity, recorded as
// held, to the OR of its growth lanes: made when the first turns on, turned
// off when the last turns off or the seat is removed, kept rather than deleted
// so its roles in the products survive should the seat return (a person
// deletes it in the identity project). A seat's failure is its finding; a
// fault Google or the store answers is joined into the error once every other
// seat has converged. NEVER ADOPTED BY NAME: only an account whose making the
// record shows was Crewlet's own is taken as Crewlet's, and one somebody
// turned off at Google is never turned back on unasked.
func ConvergeGrowthIdentities(
	ctx context.Context, c *Client, company string, seats []GrowthSeat, held []coord.GoogleIdentity,
	store coord.GoogleIdentities,
) ([]integration.Finding, error) {
	records := make(map[string]coord.GoogleIdentity, len(held))
	for _, rec := range held {
		records[rec.Handle] = rec
	}
	on := make(map[string]bool, len(seats))
	var order []string
	for _, seat := range seats {
		on[seat.Handle] = seat.On
		order = append(order, seat.Handle)
	}
	for _, rec := range held {
		if _, listed := on[rec.Handle]; !listed {
			order = append(order, rec.Handle)
		}
	}
	g := growthPass{client: c, company: company, store: store}
	var findings []integration.Finding
	var faults []error
	for _, handle := range order {
		rec, has := records[handle]
		var f *integration.Finding
		var err error
		switch {
		case on[handle] && (!has || !rec.Made()):
			f, err = g.create(ctx, handle, rec, has)
		case on[handle]:
			f, err = g.keepOn(ctx, rec)
		case has && rec.Made():
			f, err = g.keepOff(ctx, rec)
		case has:
			f, err = g.settleIntent(ctx, rec)
		}
		switch {
		case errors.Is(err, errStale):
			// ANOTHER PASS WROTE THE RECORD MEANWHILE: the next pass reads it.
		case err != nil:
			faults = append(faults, err)
		case f != nil:
			findings = append(findings, *f)
		}
	}
	return findings, errors.Join(faults...)
}

// growthPass is one pass's work on the growth identities.
type growthPass struct {
	client  *Client
	company string
	store   coord.GoogleIdentities
}

// errStale is a record write another pass moved under this one.
var errStale = errors.New("googlecloud: a growth identity changed under this pass")

// subject names a seat's growth identity in a finding.
func subject(handle string) string { return handle + "'s Google growth identity" }

// refusal is a seat's finding that the step doing failed with err, or err
// itself where a wait clears it.
func (g growthPass) refusal(handle, doing string, err error) (*integration.Finding, error) {
	if googleauth.Passes(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("googlecloud: %s: %w", subject(handle), err)
	}
	if _, settling := SettlingReason(err); settling {
		return &integration.Finding{Kind: integration.FindingIdentityMissing, Subject: subject(handle),
			Detail: "Google is still settling Crewlet's change to " + subject(handle)}, nil
	}
	return &integration.Finding{
		Kind: integration.FindingIdentityFailed, Subject: subject(handle),
		Detail:    "Crewlet could not " + doing + ": " + err.Error(),
		Remedy:    "Check the provisioner's Service Account Admin role on " + g.client.IdentityProject(),
		ActionURL: projectURL(ServiceAccountsURL, g.client.IdentityProject()),
	}, nil
}

// write stores rec at its version, or creates it where it is new.
func (g growthPass) write(ctx context.Context, rec coord.GoogleIdentity, isNew bool) (coord.GoogleIdentity, error) {
	var stored coord.GoogleIdentity
	var ok bool
	var err error
	if isNew {
		stored, ok, err = g.store.CreateGoogleIdentity(ctx, rec)
	} else {
		stored, ok, err = g.store.UpdateGoogleIdentity(ctx, rec)
	}
	switch {
	case err != nil:
		return coord.GoogleIdentity{}, fmt.Errorf("googlecloud: record %s: %w", subject(rec.Handle), err)
	case !ok:
		return coord.GoogleIdentity{}, errStale
	}
	return stored, nil
}

// create makes the seat's growth identity. The intent is recorded first, by
// the account's email, so a 409 for that email afterwards is this pass's own
// earlier attempt, read back by email and recorded by its unique ID, and
// never a stranger's account taken over.
func (g growthPass) create(
	ctx context.Context, handle string, rec coord.GoogleIdentity, has bool,
) (*integration.Finding, error) {
	email := googleauth.AccountEmail(googleauth.GrowthAccountID(g.company, handle), g.client.IdentityProject())
	ours := has && rec.Email == email
	if !ours {
		intent := coord.GoogleIdentity{Handle: handle, Email: email, CreatedAt: g.client.clock().Now(),
			Version: rec.Version}
		var err error
		if rec, err = g.write(ctx, intent, !has); err != nil {
			return nil, err
		}
	}
	id, _, _ := strings.Cut(email, "@")
	sa, err := g.client.CreateServiceAccount(ctx, id, growthDisplayName(g.company, handle),
		growthDescription(g.company, handle))
	if googleauth.Status(err) == http.StatusConflict {
		// OURS ONLY WHERE AN EARLIER PASS RECORDED THE INTENT and the account
		// says what Crewlet writes on it; else a stranger's, and the intent
		// this pass wrote goes.
		var found bool
		if ours {
			sa, found, err = g.client.ServiceAccount(ctx, email)
			if err != nil {
				return g.refusal(handle, "read back "+email, err)
			}
		}
		if !found || sa.Description != growthDescription(g.company, handle) {
			if _, derr := g.store.DeleteGoogleIdentity(ctx, handle, rec.Version); derr != nil {
				return nil, fmt.Errorf("googlecloud: forget %s: %w", subject(handle), derr)
			}
			return &integration.Finding{
				Kind: integration.FindingIdentityFailed, Subject: subject(handle),
				Detail: "a service account " + email + " already exists, and Crewlet has no record of making it",
				Remedy: "Nothing is adopted by its name. Delete it in the identity project if nobody uses it; " +
					"Crewlet makes a new one on its next pass",
				ActionURL: projectURL(ServiceAccountsURL, g.client.IdentityProject()),
			}, nil
		}
	} else if err != nil {
		return g.refusal(handle, "make "+email, err)
	}
	rec.UniqueID, rec.Email = sa.UniqueID, sa.Email
	if rec, err = g.write(ctx, rec, false); err != nil {
		return nil, err
	}
	if _, werr := awaitObserved(ctx, g.client.clock(), readCreated, func() (bool, error) {
		_, found, rerr := g.client.ServiceAccount(ctx, sa.UniqueID)
		return found || (rerr != nil && !gone(rerr)), rerr
	}); werr != nil {
		return g.refusal(handle, "read back "+email, werr)
	}
	return g.delegate(ctx, rec)
}

// settleIntent finishes an off seat's intent to make its identity: an account
// Google made under the recorded email is recorded and turned off, and an
// intent Google never acted on is forgotten.
func (g growthPass) settleIntent(ctx context.Context, rec coord.GoogleIdentity) (*integration.Finding, error) {
	sa, found, err := g.client.ServiceAccount(ctx, rec.Email)
	switch {
	case err != nil:
		return g.refusal(rec.Handle, "read "+rec.Email, err)
	case !found:
		if _, derr := g.store.DeleteGoogleIdentity(ctx, rec.Handle, rec.Version); derr != nil {
			return nil, fmt.Errorf("googlecloud: forget %s: %w", subject(rec.Handle), derr)
		}
		return nil, nil
	}
	rec.UniqueID = sa.UniqueID
	if rec, err = g.write(ctx, rec, false); err != nil {
		return nil, err
	}
	return g.keepOff(ctx, rec)
}

// keepOn holds an on seat's recorded identity on: delegated, enabled where
// the engine turned it off, and still at Google. One gone is forgotten, so
// the next pass makes it again.
func (g growthPass) keepOn(ctx context.Context, rec coord.GoogleIdentity) (*integration.Finding, error) {
	sa, found, err := g.client.ServiceAccount(ctx, rec.UniqueID)
	switch {
	case err != nil:
		return g.refusal(rec.Handle, "read "+rec.Email, err)
	case !found:
		if _, derr := g.store.DeleteGoogleIdentity(ctx, rec.Handle, rec.Version); derr != nil {
			return nil, fmt.Errorf("googlecloud: forget %s: %w", subject(rec.Handle), derr)
		}
		return &integration.Finding{Kind: integration.FindingIdentityMissing, Subject: subject(rec.Handle),
			Detail: rec.Email + " is gone from the identity project; Crewlet makes the seat a new one"}, nil
	case sa.Disabled && !rec.Disabled:
		return &integration.Finding{
			Kind: integration.FindingApprovalRequired, Subject: subject(rec.Handle),
			Detail:    "somebody turned " + rec.Email + " off at Google, and Crewlet never turns it back on unasked",
			Remedy:    "Enable it in the identity project, or turn the agent's Google growth products off",
			ActionURL: projectURL(ServiceAccountsURL, g.client.IdentityProject()),
		}, nil
	case sa.Disabled:
		if eerr := g.client.EnableServiceAccount(ctx, rec.UniqueID); eerr != nil {
			return g.refusal(rec.Handle, "turn "+rec.Email+" back on", eerr)
		}
		if werr := awaitDisabled(ctx, g.client, rec.UniqueID, false); werr != nil {
			return g.refusal(rec.Handle, "turn "+rec.Email+" back on", werr)
		}
	}
	if rec.Disabled {
		// RECORDED ONCE GOOGLE SHOWS IT ON: a pass cut short before this
		// finds the account on and the record off, and enables it again.
		rec.Disabled = false
		if rec, err = g.write(ctx, rec, false); err != nil {
			return nil, err
		}
	}
	return g.delegate(ctx, rec)
}

// delegate holds the provisioner's Token Creator on the identity alone, which
// every token waits on, and records it. It is read each pass, since a person
// can take it off at Google, and written and read back only where missing.
func (g growthPass) delegate(ctx context.Context, rec coord.GoogleIdentity) (*integration.Finding, error) {
	prov := member(g.client.Provisioner())
	pol, err := g.client.AccountPolicy(ctx, rec.Email)
	if err != nil {
		return g.refusal(rec.Handle, "read who may act as "+rec.Email, err)
	}
	if !slices.Contains(heldBy(pol, prov).plain, tokenCreator) {
		if _, werr := awaitObserved(ctx, g.client.clock(), readDelegated, func() (bool, error) {
			eerr := g.client.EditAccountPolicy(ctx, rec.Email, func(p *Policy) bool {
				return grant(p, prov, []string{tokenCreator})
			})
			switch googleauth.Status(eerr) {
			case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
				return false, eerr
			}
			return true, eerr
		}); werr != nil {
			return g.refusal(rec.Handle, "let the provisioner act as "+rec.Email, werr)
		}
		if _, werr := awaitObserved(ctx, g.client.clock(), readDelegated, func() (bool, error) {
			read, rerr := g.client.AccountPolicy(ctx, rec.Email)
			return rerr != nil || slices.Contains(heldBy(read, prov).plain, tokenCreator), rerr
		}); werr != nil {
			return g.refusal(rec.Handle, "read back the grant on "+rec.Email, werr)
		}
	}
	if rec.Delegated {
		return nil, nil
	}
	rec.Delegated = true
	_, err = g.write(ctx, rec, false)
	return nil, err
}

// keepOff holds an off or removed seat's identity off. The record says so
// FIRST, so a pass cut short after Google turned it off never reads the
// engine's own disable as a person's. Its roles in the products stay where
// people put them, so turning a lane on again restores the seat's access.
func (g growthPass) keepOff(ctx context.Context, rec coord.GoogleIdentity) (*integration.Finding, error) {
	if !rec.Disabled {
		rec.Disabled = true
		var err error
		if rec, err = g.write(ctx, rec, false); err != nil {
			return nil, err
		}
	}
	sa, found, err := g.client.ServiceAccount(ctx, rec.UniqueID)
	switch {
	case err != nil:
		return g.refusal(rec.Handle, "read "+rec.Email, err)
	case !found:
		if _, derr := g.store.DeleteGoogleIdentity(ctx, rec.Handle, rec.Version); derr != nil {
			return nil, fmt.Errorf("googlecloud: forget %s: %w", subject(rec.Handle), derr)
		}
		return nil, nil
	case sa.Disabled:
		return nil, nil
	}
	if derr := g.client.DisableServiceAccount(ctx, rec.UniqueID); derr != nil {
		return g.refusal(rec.Handle, "turn "+rec.Email+" off", derr)
	}
	if werr := awaitDisabled(ctx, g.client, rec.UniqueID, true); werr != nil {
		return g.refusal(rec.Handle, "turn "+rec.Email+" off", werr)
	}
	return nil, nil
}

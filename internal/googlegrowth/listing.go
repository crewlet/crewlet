package googlegrowth

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Listed is one entry a product's listing found (an Analytics property, a
// Search Console site, a Play app): its ID as the company document writes
// it, its name, and what holds it.
type Listed struct {
	ID string
	// Name is the product's name for it, "" where it shows none; Parent is
	// what holds it as the product names it ("accounts/123") and
	// ParentName that holder's name, both "" where nothing does.
	Name, Parent, ParentName string
	// Roles are the role IDs the listing shows the caller holds there, and
	// Read whether it shows them at all.
	Roles []string
	Read  bool
}

// Lister is one listing call: as the company's reader for a picker, or as a
// seat's identity for its check.
type Lister struct {
	Company *config.Company
	Token   string
	Send    *googleauth.Transport
}

// Listing is every entry a product lists, by ID.
type Listing map[string]Listed

// listingOf is listed keyed by ID.
func listingOf(listed []Listed) Listing {
	out := Listing{}
	for _, l := range listed {
		out[l.ID] = l
	}
	return out
}

// readerListing is what the company's reader sees at the product, and why
// nothing could be listed in a person's words ("" where it was). A product
// with no listing lists nothing and says nothing.
func (v SeatAccess) readerListing(ctx context.Context, c *config.Company, tokens seataccess.IssuedTokens,
	send *googleauth.Transport) ([]Listed, string) {
	if v.List == nil || tokens == nil {
		return nil, ""
	}
	entries := v.Identity().Entries
	token, err := tokens.ReaderToken(ctx, v.Product)
	stage, _ := errors.AsType[*googleauth.StageError](err)
	switch status := googleauth.Status(err); {
	case errors.Is(err, seataccess.ErrNotIssued):
		return nil, "Connect Google Cloud so Crewlet's company reader can list your " + entries + "."
	case stage != nil && stage.Stage == googleauth.StageFederation:
		return nil, "Crewlet cannot sign in through the Google Cloud connection, so it cannot list your " +
			entries + "; Google Cloud's page says why."
	case stage != nil || status == http.StatusForbidden || status == http.StatusNotFound:
		return nil, "Crewlet cannot act as the company reader yet, so it cannot list your " + entries + "."
	case err != nil:
		return nil, "Google did not answer the company reader's sign-in, so your " + entries +
			" are not listed now; Crewlet asks again shortly."
	}
	listed, err := v.List(ctx, Lister{Company: c, Token: token, Send: send})
	return listed, listingRefused(err, v.Name, entries, "the company reader")
}

// listingRefused is why a listing as who found nothing, in a person's words;
// "" where it answered.
func listingRefused(err error, product, entries, who string) string {
	switch status := googleauth.Status(err); {
	case err == nil:
		return ""
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return product + " does not let " + who + " list your " + entries + " yet."
	}
	return product + " did not answer the listing, so your " + entries + " are not listed now; Crewlet asks " +
		"again shortly."
}

// options is the lane's offer with what the company's reader lists.
func (v SeatAccess) options(ctx context.Context, lane seataccess.Lane, c *config.Company,
	tokens seataccess.IssuedTokens, send *googleauth.Transport) seataccess.Options {
	out := seataccess.Options{Lane: lane, Levels: v.Levels(), ScopeTyped: v.Typed}
	listed, why := v.readerListing(ctx, c, tokens, send)
	return listedOptions(out, v.List != nil, listed, why)
}

// GrantOptions is a consent product's lane offer (AdMob, AdSense): its levels
// and what the company's own sign-in lists, which every seat's calls go as.
func GrantOptions(ctx context.Context, g Granted, tokens seataccess.IssuedTokens) seataccess.Options {
	out := seataccess.Options{Lane: g.Lane, Levels: g.Levels, ScopeTyped: g.Typed}
	if tokens == nil {
		return out
	}
	token, err := tokens.AdminToken(ctx, g.Product)
	switch {
	case errors.Is(err, seataccess.ErrNotIssued):
		return listedOptions(out, true, nil, "Sign in with Google on "+g.Name+"'s Settings tab so Crewlet can "+
			"list your "+g.Entries+".")
	case err != nil:
		return listedOptions(out, true, nil, "Google did not answer the company's sign-in for "+g.Name+", so "+
			"your "+g.Entries+" are not listed now; Crewlet asks again shortly.")
	}
	listed, err := g.List(ctx, Lister{Token: token, Send: googleauth.NewTransport(g.HTTP)})
	return listedOptions(out, true, listed, listingRefused(err, g.Name, g.Entries, "the company's sign-in"))
}

// Granted is a consent product's lane, as [GrantOptions] lists it.
type Granted struct {
	Product integration.Kind
	Lane    seataccess.Lane
	Levels  []seataccess.Level
	// Name is the product's, Entries what a seat's scope holds ("publisher
	// accounts") and Typed the shape of one typed.
	Name, Entries, Typed string
	List                 func(ctx context.Context, in Lister) ([]Listed, error)
	// HTTP is the transport Google is reached through; nil in production.
	HTTP *http.Client
}

// listedOptions is out with listed as a picker offers it: each entry by its
// name, grouped under what holds it, and why nothing was listed where
// nothing could be. lists is whether the product lists at all.
func listedOptions(out seataccess.Options, lists bool, listed []Listed, why string) seataccess.Options {
	out.ScopeUnlisted = why
	for _, l := range listed {
		out.Scope = append(out.Scope, l.ID)
		if l.Name != "" && l.Name != l.ID {
			if out.ScopeNames == nil {
				out.ScopeNames = map[string]string{}
			}
			out.ScopeNames[l.ID] = l.Name
		}
		if group := firstNonEmpty(l.ParentName, l.Parent); group != "" {
			if out.ScopeGroups == nil {
				out.ScopeGroups = map[string]string{}
			}
			out.ScopeGroups[l.ID] = group
		}
	}
	out.ScopeLive = lists && why == ""
	return out
}

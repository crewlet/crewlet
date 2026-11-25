package seataccess

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/integration"
)

// An agent asks for one grant just in time with request_access (ADR-0118):
// the engine stores the request, posts it as a decision whose grant options
// carry the request's id, and a person answers by previewing and saving the
// grant on the Agents tab. The stored request is what the save builds the
// grant from, so nothing the agent wrote but its reason reaches the grant.

// GrantRequest is one agent's request for one grant, as the engine stored it
// when the agent asked.
type GrantRequest struct {
	// ID is what the request's options carry ([GrantOption]).
	ID string `json:"id"`

	// Kind, Lane and Handle are where the grant would be held.
	Kind   integration.Kind `json:"kind"`
	Lane   Lane             `json:"lane"`
	Handle string           `json:"handle"`

	// Permission and Scope are the grant asked for, checked against the
	// lane's live catalogue when the agent asked and again when it is saved.
	Permission string   `json:"permission"`
	Scope      []string `json:"scope,omitempty"`

	// Hours is how long the temporary option grants it for.
	Hours int `json:"hours"`

	// Reason is the agent's own words for why, kept as the grant's reason.
	Reason string `json:"reason" untrusted:"fence"`

	// Target is what the one change asked for changes (a bucket), which a
	// permission granted for one change needs ([GrantEntry.OneChange]).
	Target string `json:"target,omitempty" untrusted:"name"`

	// Item is the work item the request was asked on, Ask the person it was
	// asked of, AskedAt when.
	Item    string    `json:"item"`
	Ask     string    `json:"ask"`
	AskedAt time.Time `json:"asked_at"`
}

// RequestTTL is how long an unanswered request is kept: a month, past which
// the change it was asked for has moved on. Its question stays in the Inbox,
// and choosing a grant there says the request has lapsed.
const RequestTTL = 30 * 24 * time.Hour

// grantOptionPrefix begins a request's two grant options: the engine's
// reserved prefix, so no author's decision can carry one, and "r", which no
// key of the access catalogue begins with.
const grantOptionPrefix = decision.ReservedOptionPrefix + "r"

// GrantOptionTemporary and GrantOptionPermanent are a request's two grant
// options: `grant-r<id>-t` grants it for the request's hours, `grant-r<id>-p`
// for good. Nineteen characters, inside a decision's option id.
func GrantOptionTemporary(id string) string { return grantOptionPrefix + id + "-t" }

// GrantOptionPermanent is [GrantOptionTemporary]'s permanent twin.
func GrantOptionPermanent(id string) string { return grantOptionPrefix + id + "-p" }

// grantOption is the shape of a request's grant option.
var grantOption = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(grantOptionPrefix) + `([a-z0-9]{10})-([tp])$`)
})

// GrantOption is the request a decision's option grants and whether for good,
// or false for any other option.
func GrantOption(option string) (id string, permanent bool, ok bool) {
	m := grantOption().FindStringSubmatch(option)
	if m == nil {
		return "", false, false
	}
	return m[1], m[2] == "p", true
}

// Validate refuses a request the engine could not have written.
func (r GrantRequest) Validate() error {
	switch {
	case !grantOption().MatchString(GrantOptionTemporary(r.ID)):
		return fmt.Errorf("seataccess: %q is not a request id", r.ID)
	case !r.Kind.Valid() || !r.Lane.Valid() || r.Lane.Kind() != r.Kind:
		return fmt.Errorf("seataccess: request %s names no lane of a kind this build manages", r.ID)
	case strings.TrimSpace(r.Handle) == "" || strings.TrimSpace(r.Permission) == "":
		return fmt.Errorf("seataccess: request %s names no seat or no permission", r.ID)
	case r.Hours < MinGrantHours || r.Hours > MaxGrantHours:
		return fmt.Errorf("seataccess: request %s lasts %d hours, outside %d to %d", r.ID, r.Hours,
			MinGrantHours, MaxGrantHours)
	case r.Target != "" && !config.SeatGrantTarget(r.Target):
		return fmt.Errorf("seataccess: request %s names a target that is not one resource's name", r.ID)
	case r.StoredBytes() > coord.MaxGrantRequestBytes:
		return fmt.Errorf("seataccess: request %s is %d bytes as stored, past the %d one request holds: "+
			"name fewer scopes, or say why in fewer words", r.ID, r.StoredBytes(), coord.MaxGrantRequestBytes)
	}
	return nil
}

// StoredBytes is how many bytes the request is stored as, the one bound on its
// size ([coord.MaxGrantRequestBytes]): its fields are an agent's words and a
// catalogue's names, held together rather than each.
func (r GrantRequest) StoredBytes() int {
	raw, err := json.Marshal(r)
	if err != nil {
		return math.MaxInt32
	}
	return len(raw)
}

// Grant is the grant a person's choice of option makes from the request at
// now: for its hours, or for good.
func (r GrantRequest) Grant(permanent bool, now time.Time) Grant {
	g := Grant{Permission: r.Permission, Scope: normalSet(r.Scope), Reason: r.Reason, Request: r.ID, Target: r.Target}
	if !permanent {
		g.ExpiresAt = now.UTC().Add(time.Duration(r.Hours) * time.Hour)
	}
	return g
}

// ErrNoRequest is a request id the store does not hold: never asked, already
// granted, or lapsed ([RequestTTL]).
var ErrNoRequest = errors.New("seataccess: no such request")

// GrantRefused is a chosen grant that cannot be saved as asked, with why.
type GrantRefused struct{ Reason string }

func (e *GrantRefused) Error() string { return "seataccess: " + e.Reason }

// CoordRequests is the agents' grant requests on the fleet's coordination
// store: the whole company, now, since a request asked on the node holding the
// seat is answered on whichever node serves the person.
type CoordRequests struct{ store coord.GrantRequests }

// NewCoordRequests wraps the fleet's request records, refusing a nil backend
// for [NewCoordLedger]'s reason.
func NewCoordRequests(store coord.GrantRequests) (*CoordRequests, error) {
	if store == nil {
		return nil, errors.New("seataccess: NewCoordRequests needs the fleet's coordination backend")
	}
	return &CoordRequests{store: store}, nil
}

// Request is one stored request, or [ErrNoRequest].
func (c *CoordRequests) Request(ctx context.Context, id string) (GrantRequest, error) {
	raw, found, err := c.store.GrantRequest(ctx, id)
	if err != nil {
		return GrantRequest{}, fmt.Errorf("seataccess: read request %s: %w", id, err)
	}
	if !found {
		return GrantRequest{}, ErrNoRequest
	}
	var r GrantRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return GrantRequest{}, fmt.Errorf("seataccess: request %s is unreadable: %w", id, err)
	}
	r.ID = id
	if err := r.Validate(); err != nil {
		return GrantRequest{}, err
	}
	return r, nil
}

// Open is every readable request not past [RequestTTL], oldest first; one it
// cannot read is left for [CoordRequests.Sweep].
func (c *CoordRequests) Open(ctx context.Context, now time.Time) ([]GrantRequest, error) {
	all, err := c.store.GrantRequests(ctx)
	if err != nil {
		return nil, fmt.Errorf("seataccess: read the requests: %w", err)
	}
	var out []GrantRequest
	for id, raw := range all {
		var r GrantRequest
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		r.ID = id
		if r.Validate() != nil || now.Sub(r.AskedAt) >= RequestTTL {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b GrantRequest) int {
		return cmp.Or(a.AskedAt.Compare(b.AskedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Put stores a request, once validated.
func (c *CoordRequests) Put(ctx context.Context, r GrantRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("seataccess: encode request %s: %w", r.ID, err)
	}
	if err := c.store.PutGrantRequest(ctx, r.ID, raw); err != nil {
		return fmt.Errorf("seataccess: store request %s: %w", r.ID, err)
	}
	return nil
}

// Drop forgets a request: granted, declined or lapsed. Dropping one that is
// not there is the outcome asked for.
func (c *CoordRequests) Drop(ctx context.Context, id string) error {
	if err := c.store.DeleteGrantRequest(ctx, id); err != nil {
		return fmt.Errorf("seataccess: drop request %s: %w", id, err)
	}
	return nil
}

// Sweep drops every request asked more than [RequestTTL] before now, and
// every one this build cannot read, which no answer could use.
func (c *CoordRequests) Sweep(ctx context.Context, now time.Time) error {
	all, err := c.store.GrantRequests(ctx)
	if err != nil {
		return fmt.Errorf("seataccess: read the requests: %w", err)
	}
	var errs []error
	for id, raw := range all {
		var r GrantRequest
		if json.Unmarshal(raw, &r) == nil && now.Sub(r.AskedAt) < RequestTTL {
			continue
		}
		if err := c.store.DeleteGrantRequest(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

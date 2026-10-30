package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("artifactsapi")

// WHO A LISTING IS FILTERED FOR, AND WHO HOLDS ONE ARTIFACT.
//
// A listing is filtered in SQL by the viewer's relations (their record, the
// units they sit in or below, the agents they lead) and then decided row by
// row by `get_artifact`, so the filter keeps a page from holding rows nobody
// may see and the authority table still decides each one. The relations are
// the CHART'S, asked through the same [authz.Chart] every decision asks: the
// roster only says which units and seats exist, never who sits or leads where,
// so a membership the chart answers and one a listing assumes cannot differ.

// Roster is the running company's units and seats as the chart holds them
// today: what a viewer's relations are enumerated over.
type Roster interface {
	// Units is every unit key in the company.
	Units(ctx context.Context) ([]string, error)

	// Seats is every seat in the company, human and agent.
	Seats(ctx context.Context) ([]RosterSeat, error)
}

// RosterSeat is one seat of the roster.
type RosterSeat struct {
	Handle string
	Agent  bool
}

// ErrNoRoster is a node that holds no running company to enumerate: it is
// still booting or has never been handed one, which clears.
var ErrNoRoster = errors.New("artifactsapi: this node runs no company yet, so it " +
	"cannot tell which units and seats a viewer relates to")

// ErrUnknownAudience is a share naming a seat or a unit the company does not
// hold: well formed, and nobody's.
var ErrUnknownAudience = errors.New("artifactsapi: nobody holds that audience")

// knownAudience refuses an audience a NEW share could not reach: one a grant
// may not name ([artifacts.Audience.ValidateGrant], a login named as a seat
// included), and a seat or a unit the company's roster does not hold. The
// agents' share tool asks the running org the same question
// (internal/agent/builtin), so a share is refused alike from either side.
//
// AN ERROR READING THE ROSTER IS NOT A REFUSAL: the caller answers it as
// unavailable rather than telling a person their colleague does not exist.
func (g Gate) knownAudience(ctx context.Context, a artifacts.Audience) error {
	if err := a.ValidateGrant(); err != nil {
		return err
	}
	switch a.Kind {
	case artifacts.AudienceSeat:
		seats, err := g.Roster.Seats(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(seats, func(s RosterSeat) bool { return s.Handle == a.ID }) {
			return fmt.Errorf("%w: the company has no seat %q; share with a seat the "+
				"org chart holds", ErrUnknownAudience, a.ID)
		}
	case artifacts.AudienceUnit:
		units, err := g.Roster.Units(ctx)
		if err != nil {
			return err
		}
		if !slices.Contains(units, a.ID) {
			return fmt.Errorf("%w: the company has no unit %q; share with a unit the "+
				"org chart holds", ErrUnknownAudience, a.ID)
		}
	}
	return nil
}

// viewerFor is the listing filter for principal p: their record, the units the
// chart seats them in (or below), and the agent seats it says they lead. An
// error is a chart or roster this node could not read, which a listing answers
// unknown for rather than listing less.
func (g Gate) viewerFor(ctx context.Context, p iam.Principal) (artifactsdomain.Viewer, error) {
	v := artifactsdomain.Viewer{Record: iam.RecordOwner(p)}
	actor := chartActor(p)
	if actor == "" {
		// A CALLER THE CHART CANNOT NAME relates to nothing a unit or a
		// lead reaches.
		return v, nil
	}
	units, err := g.Roster.Units(ctx)
	if err != nil {
		return artifactsdomain.Viewer{}, err
	}
	for _, unit := range units {
		member, err := g.Chart.MemberOf(ctx, actor, unit) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return artifactsdomain.Viewer{}, fmt.Errorf("artifactsapi: does %s sit in %s: %w",
				actor, unit, err)
		}
		if member {
			v.Units = append(v.Units, unit)
		}
	}
	seats, err := g.Roster.Seats(ctx)
	if err != nil {
		return artifactsdomain.Viewer{}, err
	}
	for _, seat := range seats {
		if !seat.Agent || seat.Handle == actor {
			continue
		}
		leads, err := g.Chart.Leads(ctx, actor, seat.Handle)
		if err != nil {
			return artifactsdomain.Viewer{}, fmt.Errorf("artifactsapi: does %s lead %s: %w",
				actor, seat.Handle, err)
		}
		if leads {
			v.LedOwners = append(v.LedOwners, seat.Handle)
		}
	}
	return v, nil
}

// chartActor is the name the chart is asked about p by: their seat, and their
// login where they hold none (which the chart seats nowhere). The authority
// table asks the chart by exactly this name, so a relation a listing assumes
// and one a decision reaches are one answer.
func chartActor(p iam.Principal) string {
	if p.Seat != "" {
		return p.Seat
	}
	return p.Login
}

// Visible keeps the rows of a listing the viewer may be shown, each decided by
// `get_artifact` on a relation ([authz.Visible]): the admin path shows no row,
// so a listing never surfaces somebody's private artifact by an override
// nobody recorded.
func (g Gate) Visible(ctx context.Context, p iam.Principal, items []artifactsdomain.ListItem) (
	[]artifactsdomain.ListItem, error) {

	now := g.decidedAt(ctx)
	out := make([]artifactsdomain.ListItem, 0, len(items))
	for _, item := range items {
		shown, err := authz.Visible(ctx, p, g.object(item.Head), g.Chart, now)
		if err != nil {
			return nil, err
		}
		if shown {
			out = append(out, item)
		}
	}
	return out, nil
}

// List answers one page of the artifacts p can see.
func (g Gate) List(ctx context.Context, reads Lister, p iam.Principal, q artifactsdomain.ListQuery) (
	ListPage, error) {

	v, err := g.viewerFor(ctx, p)
	if err != nil {
		return ListPage{}, err
	}
	page, err := reads.List(ctx, v, q)
	if err != nil {
		return ListPage{}, err
	}
	shown, err := g.Visible(ctx, p, page.Contents)
	if err != nil {
		return ListPage{}, err
	}
	out := ListPage{Prefix: page.Prefix, Delimiter: page.Delimiter,
		Contents: make([]ListItem, 0, len(shown)), CommonPrefixes: page.CommonPrefixes,
		IsTruncated: page.IsTruncated, NextContinuationToken: page.NextContinuationToken,
		Total: page.Total, Level: string(page.Level), Position: page.Position, Complete: page.Complete}
	for _, item := range shown {
		role, err := g.shownRole(ctx, p, item.Head)
		if err != nil {
			return ListPage{}, err
		}
		out.Contents = append(out.Contents, ListItem{
			Artifact: shapeHead(item.Head, role, false),
			Pinned:   item.Pinned, OpenedAt: item.OpenedAt, FolderPath: item.FolderPath,
		})
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	return out, nil
}

// shownRole is the role a row of a listing is shaped by: the read row's
// standing, a lower bound where the chart could not settle it, which shows
// less rather than more.
func (g Gate) shownRole(ctx context.Context, p iam.Principal, head artifactsdomain.Head) (
	artifacts.Role, error) {

	d := authz.Decide(ctx, p, authz.ActionArtifactRead, g.object(head), g.Chart, g.decidedAt(ctx))
	if d.Unknown() {
		return "", d.Err
	}
	return d.Standing.Role, nil
}

// Lister is the artifacts estate's listing.
type Lister interface {
	List(ctx context.Context, v artifactsdomain.Viewer, q artifactsdomain.ListQuery) (
		artifactsdomain.ListPage, error)
}

// ListPage is one page of a listing as a viewer is shown it, in S3's
// ListObjectsV2 terms (artifactsdomain.ListPage says what each means).
type ListPage struct {
	Prefix         string                         `json:"prefix"`
	Delimiter      string                         `json:"delimiter,omitempty"`
	Contents       []ListItem                     `json:"contents"`
	CommonPrefixes []artifactsdomain.CommonPrefix `json:"common_prefixes,omitempty"`

	// KeyCount is what this page shows, contents and common prefixes: a row
	// the authority table withheld is not counted.
	KeyCount              int    `json:"key_count"`
	IsTruncated           bool   `json:"is_truncated"`
	NextContinuationToken string `json:"next_continuation_token,omitempty"`

	// Total is how many artifacts the query matched in the estate's own
	// filter, which decides every relation the authority table does; a row
	// the table still withheld is counted and not listed.
	Total    int               `json:"total"`
	Level    string            `json:"read_level"`
	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// ListItem is one artifact of a listing.
type ListItem struct {
	Artifact
	Pinned   bool      `json:"pinned,omitempty"`
	OpenedAt time.Time `json:"opened_at"`

	// FolderPath is the folder it is filed in, as the viewer sees it.
	FolderPath string `json:"folder_path,omitempty"`
}

// ---- effective access ------------------------------------------------- //

// Access is who holds an artifact and through what: the share dialog's list.
type Access struct {
	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	// Role is the asking caller's own role.
	Role artifacts.Role `json:"role"`

	// Shares is every share with the seats each unit share reaches today,
	// shown to the owner and to editors; nil below.
	Shares []ShareAccess `json:"shares,omitempty"`

	// Leads are the seats that lead an agent owner, each holding owner on
	// its artifacts (the lead arm, D12); nil for a person's artifact, and
	// below editor.
	Leads []string `json:"leads,omitempty"`

	// ShareCount is how many shares the artifact carries, which a viewer
	// and a commenter are shown in place of the list.
	ShareCount int `json:"share_count"`
}

// ShareAccess is one share and whom it reaches.
type ShareAccess struct {
	artifacts.Share

	// Members are the seats a unit share reaches today: the unit's and
	// every unit's below it. Empty for a seat or company share.
	Members []string `json:"members,omitempty"`

	// Invalid marks a share that reaches nobody ([artifacts.Audience.Reaches]):
	// a login named as a seat, which a build before the grant rule wrote. It
	// is listed rather than hidden, so its owner sees it and removes it.
	Invalid bool `json:"invalid,omitempty"`
}

// access is the effective access on a, as the caller a admitted is shown it.
func (g Gate) access(ctx context.Context, a Admission) (Access, error) {
	head := a.Head
	out := Access{Owner: head.Owner, OwnerKind: head.OwnerKind, Role: a.Role(),
		ShareCount: len(head.Shares)}
	if !a.Role().Includes(artifacts.RoleEditor) {
		return out, nil
	}
	var seats []RosterSeat
	needSeats := head.OwnerKind == iam.ActorAgent || slices.ContainsFunc(head.Shares,
		func(s artifacts.Share) bool { return s.Audience.Kind == artifacts.AudienceUnit })
	if needSeats {
		var err error
		if seats, err = g.Roster.Seats(ctx); err != nil {
			return Access{}, err
		}
	}
	out.Shares = make([]ShareAccess, 0, len(head.Shares))
	for _, s := range head.Shares {
		entry := ShareAccess{Share: s, Invalid: !s.Audience.Reaches()}
		if s.Audience.Kind == artifacts.AudienceUnit {
			for _, seat := range seats {
				member, err := g.Chart.MemberOf(ctx, seat.Handle, s.Audience.ID)
				if err != nil {
					return Access{}, err
				}
				if member {
					entry.Members = append(entry.Members, seat.Handle)
				}
			}
		}
		out.Shares = append(out.Shares, entry)
	}
	if head.OwnerKind == iam.ActorAgent {
		for _, seat := range seats {
			if seat.Handle == head.Owner {
				continue
			}
			leads, err := g.Chart.Leads(ctx, seat.Handle, head.Owner)
			if err != nil {
				return Access{}, err
			}
			if leads {
				out.Leads = append(out.Leads, seat.Handle)
			}
		}
	}
	return out, nil
}

// Access answers the effective access on artifact id for p.
func (g Gate) Access(ctx context.Context, p iam.Principal, id artifacts.ID) (Access, error) {
	a, err := g.Read(ctx, p, id, 0)
	if err != nil {
		return Access{}, err
	}
	return g.access(ctx, a)
}

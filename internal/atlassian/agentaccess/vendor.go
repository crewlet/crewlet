// Package agentaccess is Atlassian's half of per-seat access (design
// sections 4 and 5.2, D8): one service account per agent seat in the
// organization, granted to Jira and Confluence as two lanes, each with the
// site's own roles in the projects or spaces of the seat's scope, and one API
// token whose scopes are the union of the lanes the seat is on.
//
// # Why a package of its own
//
// It needs the organization's admin APIs (internal/atlassian) and both
// products' site APIs (internal/jira, internal/confluence), and the jira
// package already imports internal/atlassian for the seat credential reads.
// Living inside internal/atlassian would make that a cycle.
//
// # What Atlassian does not offer, and what this does instead
//
// Atlassian documents no route that removes ONE product's access from a
// service account: the grant itself is the undocumented console invite, and
// the platform-role revoke is site-wide. So a lane turned off has its
// project roles or space access removed and the account's token re-minted
// without that product's scopes (revoked outright when the seat is on
// neither product). The product grant stays, and the status says product
// access is one grant for every agent.
//
// Atlassian also lists a token without its scopes, so the lanes a token
// carries are written into its label ([tokenLabel]), which is what makes a
// lane's on or off readable from the vendor every pass.
package agentaccess

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/seataccess"
)

func init() { seataccess.Register(Vendor{}) }

// The credentials sealed per seat: the API token, and the address Atlassian
// assigned the account, without which its products refuse the token (they
// authenticate base64(address:token)). Declared beside the reads that use
// them ([atlassian.CredentialOf]).
const (
	FieldToken = atlassian.FieldToken
	FieldEmail = atlassian.FieldEmail
)

// Vendor implements [seataccess.Vendor] for Atlassian.
type Vendor struct{}

// Kind implements [seataccess.Vendor].
func (Vendor) Kind() integration.Kind { return integration.KindAtlassian }

// Catalog implements [seataccess.Vendor]. No tool binding: Crewlet's own
// tracker and knowledge tools read the seat's credential directly.
func (Vendor) Catalog() seataccess.Catalog {
	lane := func(l seataccess.Lane) seataccess.LaneSpec {
		return seataccess.LaneSpec{
			Lane: l, Levels: true, LiveLevels: true, Scoped: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true,
		}
	}
	return seataccess.Catalog{
		Kind:        integration.KindAtlassian,
		Credentials: []string{FieldToken, FieldEmail},
		Lanes:       []seataccess.LaneSpec{lane(seataccess.LaneJira), lane(seataccess.LaneConfluence)},
	}
}

// product is what differs between the two lanes.
type product struct {
	lane  seataccess.Lane
	name  string // the product's own name, for a step's text
	grant string // the product key a grant names
	where string // what its scope lists: "projects" or "spaces"
}

var (
	productJira = product{
		lane: seataccess.LaneJira, name: "Jira", grant: atlassian.GrantJira, where: "projects",
	}
	productConfluence = product{
		lane: seataccess.LaneConfluence, name: "Confluence", grant: atlassian.GrantConfluence,
		where: "spaces",
	}
)

// productOf is the product a lane grants, and false for a lane this vendor
// does not have.
func productOf(lane seataccess.Lane) (product, bool) {
	switch lane {
	case seataccess.LaneJira:
		return productJira, true
	case seataccess.LaneConfluence:
		return productConfluence, true
	}
	return product{}, false
}

// access names a stored level as the product names it: a role from the
// lane's Options, Confluence's space permissions by their own names, and an id
// the product no longer has marked as missing.
func (p product) access(level string, opts seataccess.Options) string {
	level = strings.TrimSpace(level)
	if level == "" {
		return "no " + p.name + " access"
	}
	if strings.Contains(level, ":") {
		// IN CONFLUENCE'S OWN ORDER, View first, as its screen lists them.
		ids := strings.Fields(level)
		var names []string
		for _, op := range confluence.Operations {
			if slices.Contains(ids, op.ID()) {
				names = append(names, op.Name)
			}
		}
		for _, id := range ids {
			if _, ok := confluence.OperationByID(id); !ok {
				names = append(names, id+" (not found at "+p.name+")")
			}
		}
		return "the space permissions " + strings.Join(names, ", ")
	}
	kind := "project role"
	if p.lane == seataccess.LaneConfluence {
		kind = "space role"
	}
	if named, _, ok := opts.Level(level); ok {
		return "the " + named.Name + " " + kind
	}
	return "the " + kind + " " + level + " (not found at " + p.name + ")"
}

// action is one step of one op: how it is drawn and how it is taken. The
// same list backs [Vendor.Steps] and [session.Apply], so the step a preview
// shows and the step a sync takes at the same index cannot differ.
type action struct {
	verb  seataccess.StepVerb
	text  string
	note  string
	apply func(s *session, ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error)
}

// actions is the steps one op takes for one seat's lane, in order. PURE: the
// lane's Options change the words, never which steps there are.
func actions(seat seataccess.SeatRef, p product, op seataccess.Op, want, have seataccess.Desired,
	opts seataccess.Options,
) []action {
	where := "every one of the company's " + p.where
	if len(want.Scope) > 0 {
		where = strings.Join(want.Scope, ", ")
	}
	access := p.access(want.Level, opts)
	grant := action{
		verb:  seataccess.VerbGrant,
		text:  "Grant it " + p.name + " access",
		note:  "Product access is one grant, the same for every agent; what it may do is the role below.",
		apply: (*session).grant,
	}
	roles := action{
		verb:  seataccess.VerbGrant,
		text:  "Give it " + access + " in " + where,
		apply: (*session).converge,
	}
	mint := action{
		verb:  seataccess.VerbMint,
		text:  "Mint its API token with the scopes of the products it is on, where its token lacks " + p.name + "'s",
		apply: (*session).mintIfShort,
	}
	retire := action{
		verb:  seataccess.VerbRevoke,
		text:  "Revoke any earlier token Crewlet minted on it",
		apply: (*session).retire,
	}
	switch op {
	case seataccess.OpCreate:
		return []action{{
			verb:  seataccess.VerbCreate,
			text:  "Create an Atlassian service account for " + seatName(seat),
			note:  "One account serves Jira and Confluence; it is created once.",
			apply: (*session).create,
		}, grant, roles, mint, retire}
	case seataccess.OpEnable:
		return []action{grant, roles, mint, retire}
	case seataccess.OpLevel:
		roles.verb = seataccess.VerbRerole
		roles.text = "Change it from " + p.access(have.Level, opts) + " to " + access + " in " + where
		return []action{roles}
	case seataccess.OpScope:
		roles.verb = seataccess.VerbScope
		roles.text = "Give it " + access + " in " + where + " and nowhere else"
		return []action{roles}
	case seataccess.OpMint:
		mint.apply = (*session).mintAlways
		return []action{mint, retire}
	case seataccess.OpRotate:
		return []action{{
			verb:  seataccess.VerbRotate,
			text:  "Mint a new API token with the scopes of the products it is on",
			apply: (*session).mintAlways,
		}, {
			verb:  seataccess.VerbRevoke,
			text:  "Revoke its previous token",
			apply: (*session).retire,
		}}
	case seataccess.OpDisable:
		return []action{{
			verb: seataccess.VerbDisable,
			text: "Remove its " + p.name + " roles",
			note: "Atlassian offers no route to remove one product's access from a " +
				"service account, so the account keeps its " + p.name + " product " +
				"access and loses its roles and its token's " + p.name + " scopes.",
			apply: (*session).disable,
		}, {
			verb:  seataccess.VerbMint,
			text:  "Replace its API token with one without " + p.name + "'s scopes, where it is still on the other product",
			apply: (*session).narrow,
		}, retire}
	case seataccess.OpRevoke:
		return []action{{
			verb:  seataccess.VerbRevoke,
			text:  "Revoke its API token, now that it is on neither Jira nor Confluence",
			note:  "The account keeps its product access and is kept; remove it to delete it.",
			apply: (*session).revokeAll,
		}}
	case seataccess.OpRemove:
		return []action{{
			verb:  seataccess.VerbRemove,
			text:  "Delete the service account",
			note:  "Allowed only when it is off on both Jira and Confluence: one account serves both.",
			apply: (*session).remove,
		}}
	}
	return nil
}

func seatName(seat seataccess.SeatRef) string {
	if strings.TrimSpace(seat.Name) != "" {
		return seat.Name
	}
	return seat.Handle
}

// Steps implements [seataccess.Vendor], naming what it can without the
// lane's Options. See [Vendor.StepsFor].
func (v Vendor) Steps(
	seat seataccess.SeatRef, lane seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	return v.StepsFor(seataccess.StepsInput{Seat: seat, Lane: lane, Op: op, Want: want, Have: have})
}

// StepsFor implements [seataccess.NamedSteps]: the steps, with every role and
// permission named as the product names it.
func (Vendor) StepsFor(in seataccess.StepsInput) []seataccess.Step {
	p, ok := productOf(in.Lane)
	if !ok {
		return nil
	}
	op := in.Op
	drawn := actions(in.Seat, p, op, in.Want, in.Have.Access(), in.Options)
	out := make([]seataccess.Step, 0, len(drawn))
	for i, a := range drawn {
		out = append(out, seataccess.Step{
			Op: op, Verb: a.verb, Text: a.text, Note: a.note,
			By: seataccess.ByCrewlet, Index: i,
		})
	}
	return out
}

// Open implements [seataccess.Vendor].
//
// The organization's key creates and grants accounts and mints their tokens;
// the company's Jira and Confluence credentials (an administrator's, as each
// product block already requires) manage project roles and space access. A
// product whose block is absent leaves its lane unmanageable, which its steps
// refuse with the remedy rather than the whole session failing.
func (Vendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("atlassian: no company document: %w", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Atlassian
	if cfg == nil {
		return nil, fmt.Errorf("atlassian: the company does not provision Atlassian accounts: %w",
			integration.ErrNotConfigured)
	}
	value := strings.TrimSpace
	if in.Env != nil {
		value = func(v string) string { return strings.TrimSpace(in.Env.Value(v)) }
	}
	key, orgID := strings.TrimSpace(in.Operator), value(cfg.OrgID)
	if key == "" {
		key = value(cfg.APIKey)
	}
	if key == "" || orgID == "" {
		return nil, fmt.Errorf("atlassian: integrations.atlassian.org_id and api_key "+
			"did not both resolve: %w", integration.ErrNotConfigured)
	}
	s := &session{
		org: atlassian.NewClient(atlassian.ClientOptions{}), key: key, orgID: orgID,
		seats:  seatBlocks(in.Config),
		jiraIn: jira.ProjectsOf(in.Org), confIn: confluence.SpacesOf(in.Org),
		cache: newCache(),
		now:   func() time.Time { return time.Now().UTC() },
	}
	declared := cfg.Declared()
	if j := in.Config.Integrations.Jira; j != nil {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: j.ResolvedBaseURL(value), Email: value(j.Email), Token: value(j.Token),
			Declared: declared,
		})
		if err == nil {
			s.jira = client
		} else {
			s.jiraMissing = err.Error()
		}
	}
	if c := in.Config.Integrations.Confluence; c != nil {
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: c.ResolvedBaseURL(value), Email: value(c.Email), Token: value(c.Token),
			Declared: declared,
		})
		if err == nil {
			s.conf = client
		} else {
			s.confMissing = err.Error()
		}
	}
	return s, nil
}

// lanes is which products a seat's document blocks turn on.
type lanes struct{ jira, confluence bool }

func (l lanes) on(lane seataccess.Lane) bool {
	if lane == seataccess.LaneJira {
		return l.jira
	}
	return l.confluence
}

func (l lanes) without(lane seataccess.Lane) lanes {
	if lane == seataccess.LaneJira {
		l.jira = false
	} else {
		l.confluence = false
	}
	return l
}

func (l lanes) any() bool { return l.jira || l.confluence }

// seatDoc is one seat's two blocks in the document.
type seatDoc struct {
	jira       *config.RoleJira
	confluence *config.RoleConfluence
}

func (d seatDoc) lanes() lanes {
	out := lanes{}
	if d.jira != nil {
		out.jira = config.SeatAccessOn(d.jira.Enabled)
	}
	if d.confluence != nil {
		out.confluence = config.SeatAccessOn(d.confluence.Enabled)
	}
	return out
}

// seatBlocks is each agent seat's Jira and Confluence blocks, by handle.
func seatBlocks(cfg *config.Company) map[string]seatDoc {
	out := map[string]seatDoc{}
	for role := range cfg.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() {
			continue
		}
		in := role.Integrations
		if in.Jira != nil || in.Confluence != nil {
			out[seat.Handle()] = seatDoc{jira: in.Jira, confluence: in.Confluence}
		}
	}
	return out
}

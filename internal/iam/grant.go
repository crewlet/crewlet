package iam

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// Grant is one permission a principal carries: ADMIN, or ONE APP.
//
// A CLOSED SET OF SEVEN (ADR-0033), each constant below: whether somebody RUNS
// the company and which apps they USE. A team lead's reach and a share's are
// RELATIONS internal/authz asks about, never a grant. THE ZERO IS INVALID: a
// gate nobody filled in holds it, and a valid zero would ship it "granted".
//
// A NAME THIS BUILD DOES NOT KNOW IS KEPT AND IGNORED: it arrives in a row from
// a newer peer, stays there, [Grant.Valid] is false, [Principal.Can] never
// matches it and [Expand] carries it through. A writer may REMOVE it, never ADD
// it (internal/iamdomain's MayConfer); Tier A's configuration refuses it.
type Grant string

const (
	// GrantAdmin runs the company and the engine: the company document,
	// integrations, models, budgets, credential store, identity directory, the
	// deployment's controls, the audit trail and the purges. It INCLUDES EVERY
	// APP, through [Expand].
	//
	// IT IS HOST ACCESS, ONE permission by decision. The configuration it writes
	// runs code (an `mcp_servers` command on every engine host, `mcp_env`, model
	// keys, `run_in: direct`), so a holder can run anything there and read the
	// keyring; a split "runs code" permission would be this one's whole reach.
	//
	// A UNIT'S LEAD changes the seats and units inside it without this
	// (internal/authz's subtree rule), every field but a credential's.
	GrantAdmin Grant = "admin"

	// GrantWorklet uses Worklet: every Worklet read and write, a project's
	// files, the views and the catalogue, and the holder's own inbox, queue
	// and person view, plus a report's, as their lead, which internal/authz
	// decides on the relation.
	GrantWorklet Grant = "worklet"

	// GrantDoclet uses Doclet: reading, searching, writing, saving and
	// commenting on the company's own pages. A page is an ADDRESS other pages
	// and prompts resolve, so a write here changes what every seat reads next
	// turn, which is why the tool-skill pages, injected into every seat's
	// prompt, are [GrantAdmin]'s and not this.
	GrantDoclet Grant = "doclet"

	// GrantArtifacts authors artifacts: creating one, publishing a version,
	// renaming it, adding an asset, changing its datastore's schema, and
	// trashing and restoring what one owns. It is the permission a founder
	// withholds from somebody who should not publish script that runs in
	// colleagues' browsers.
	//
	// IT GATES AUTHORING ONLY, never participation. Somebody without it
	// opens, comments on and fills in every artifact shared with them,
	// because a share is a RELATION (internal/authz's shared class) and on
	// a participation verb the relation alone decides; an invitation that
	// ticks nothing still leaves a person able to answer the review sheet a
	// colleague sent them.
	GrantArtifacts Grant = "artifacts"

	// GrantChatlet uses Chatlet: reading and searching the rooms its
	// holder may see, posting, replying, reacting, opening a conversation,
	// making a room, moving their own membership and keeping their read
	// state.
	//
	// WHICH ROOM IS NEVER THIS PERMISSION'S. Public and unit rooms are every
	// holder's; a private room, group or direct conversation is MEMBERSHIP
	// (internal/chatlet's Visible), an administrator's included. It is spoken in
	// as a SEAT: a credential acting as itself holds it and still says nothing
	// (internal/authz's seated rows). A company on a vendor's chat
	// (`chat.backend: vendor`) is given nothing by it.
	GrantChatlet Grant = "chatlet"

	// GrantGitlet uses Gitlet, the engine's own git host: reading, searching,
	// cloning, fetching and pushing, and every write a repository takes,
	// through every door (routes, git transport, code tools, dashboard, the
	// code half of the inbox; ADR-0110).
	//
	// WHICH REPOSITORY, AND WHAT ON IT, IS NEVER THIS PERMISSION'S. Behind it,
	// ADR-0088's derivation decides: a person holding it maintains every
	// repository, a credential bound to no seat reads every one and never pushes
	// (ADR-0085), and a seat holds the lower of its `code.access` tier and its
	// chart relation. A holder without it is refused at the authority table
	// before any repository is read. Who read a repository and the nodes' caches
	// are [GrantAdmin]'s.
	GrantGitlet Grant = "gitlet"

	// GrantAtlas uses Atlas, the map of everything the company runs on. A
	// holder reads all of it, private addresses included, so it is OPT-IN
	// ([OptInApps]) and an administrator holds it through [Expand] (ADR-0127).
	GrantAtlas Grant = "atlas"
)

// AllGrants are every permission this build knows, in declaration order.
//
// Named AllGrants rather than Grants because [Principal.Grants] is the field a
// reader meets first, and one name for the company's whole vocabulary and for
// one principal's slice of it is a sentence that reads wrong either way.
var AllGrants = []Grant{
	GrantAdmin, GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet, GrantGitlet, GrantAtlas,
}

// Apps are the permissions that open one APP each, THE ONE PLACE AN APP IS
// DECLARED: an agent seat holds exactly these, [Expand] adds them to an
// administrator, the dashboard's picker locks them under Admin, internal/authz's
// walks hold each to a verb a person can take, and the live socket's audience
// reads it. The dashboard's copy is held to this one by a gate.
//
// APPENDED, NEVER INSERTED: the order is what [Expand] stores in a row, so a
// new app goes last and no stored row moves.
var Apps = []Grant{GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet, GrantGitlet, GrantAtlas}

// OptInApps are the apps a person holds only once somebody gives them: never by
// default on an invitation, a create or a picker, and always through admin.
// Holding one is a decision about the person rather than the company's tools.
var OptInApps = []Grant{GrantAtlas}

// DefaultApps are what a person's invitation or create carries when nothing
// names their access: every app but the opt-in ones, in [Apps] order, read by
// the CLI and, through a copy a gate holds to it, the dashboard's picker.
var DefaultApps = slices.DeleteFunc(slices.Clone(Apps), func(g Grant) bool {
	return slices.Contains(OptInApps, g)
})

// Valid reports whether a permission off the wire is one this build knows.
//
// An unknown name is an ordinary event on a rolling upgrade — a newer peer
// writes one this build has never heard of — so this answers false and
// NOTHING ELSE HAPPENS: see [Principal.Can] for what that buys and
// [Principal.Validate] for what it deliberately does not cost.
func (g Grant) Valid() bool { return slices.Contains(AllGrants, g) }

// NotAGrant is a name this build does not know as a permission, as every
// surface refusing one tells it: the name, and every permission there is,
// read off [AllGrants] so a permission the vocabulary gains is named without
// anybody remembering where.
//
// ONE SENTENCE for Tier A's token grants, an identity record's conferral and
// the identity directory's routes, which said it three ways: two in words,
// and one hand-joined ("Use one of admin, worklet, …"), which answered first
// on /iam so the other two never reached anybody there.
func NotAGrant(g Grant) string {
	return fmt.Sprintf("%q isn't a permission. Use %s", g, words.Or(AllGrants))
}

// IsApp reports whether g opens one app, rather than running the company.
func (g Grant) IsApp() bool { return slices.Contains(Apps, g) }

// Expand is set as it is compared: set's own members, plus every app when set
// holds [GrantAdmin].
//
// EVERY COMPARISON OF PERMISSIONS GOES THROUGH IT ([Principal.Can], the admin
// posture, a token's mint, what a writer may confer, an invitation's bound,
// Tier A's token check), so "admin includes every app" is one rule, not six.
//
// A FRESH SLICE THAT NEVER ALIASES set, so appending never edits a row the
// caller read. It keeps set's members in order (unknown names and duplicates
// too), then, for admin, adds each [Apps] member not held, in [Apps] order.
func Expand(set []Grant) []Grant {
	out := slices.Clone(set)
	if out == nil {
		out = []Grant{}
	}
	if !slices.Contains(set, GrantAdmin) {
		return out
	}
	for _, app := range Apps {
		if !slices.Contains(out, app) {
			out = append(out, app)
		}
	}
	return out
}

// AppsOf is the apps a set of permissions lets its holder use: every app admin
// includes where the set holds admin ([Expand]), and never admin itself, which
// no turn's ceiling carries, nor a name this build does not know, which grants
// nothing. In [Expand]'s order, a fresh slice that never aliases set.
//
// ONE RULE FOR EVERY REQUESTER A TURN IS WORKED FOR (ADR-0109): the person who
// holds a seat, and a credential acting as no seat, both as the node running
// the turn resolves them and, for the credential, as the node that took its
// write recorded them ([ActorApps]).
func AppsOf(set []Grant) []Grant {
	return slices.DeleteFunc(Expand(set), func(g Grant) bool { return !g.IsApp() })
}

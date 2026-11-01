package iam

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// Grant is one permission a principal carries: ADMIN, or ONE APP.
//
// A CLOSED SET OF FIVE (ADR-0033), and the set is the whole point. Eleven
// grants carved the engine by surface (a read of the board, a read of the
// trail, a read of the document, a write of work, a write of pages, the
// credential store twice, the deployment, the directory, `sandbox:run`) and a
// company had to compose those eleven into the three or four things it meant
// to say about a person. Nobody could: an operator inviting a teammate faced a
// list of capabilities that each opened part of a screen, and a person handed
// none of the right ones landed on an engine that showed them nothing at all.
// What a company actually says about somebody is whether they RUN it, and
// which of its apps they USE, so that is the vocabulary:
//
//   - [GrantAdmin] runs the company and the engine, and includes every app;
//   - [GrantWorklet] uses Worklet, read and write;
//   - [GrantDoclet] uses Doclet, read and write;
//   - [GrantArtifacts] authors artifacts: creates them, publishes their
//     versions and shapes their data;
//   - [GrantChatlet] uses Chatlet, read and write.
//
// Everything else a person sees (the org chart, the agents' status, spend,
// schedules) is EVERYBODY's, and asks no permission at all; what a team lead
// may do inside their own team, and what a share lets somebody do on one
// artifact, are RELATIONS internal/authz asks about, never a grant.
//
// THE ZERO IS INVALID, and that is load-bearing rather than decorative: a gate
// somebody forgot to fill in holds the zero Grant, and a zero that validated
// would ship that gate as "granted".
//
// A NAME THIS BUILD DOES NOT KNOW IS KEPT AND IGNORED. One arrives in a
// replicated row — a person's, an invitation's, a machine token's — from a
// newer peer during a rolling upgrade, or from an older estate this build
// renamed out of the vocabulary. It stays in the row, [Grant.Valid] answers
// false for it, [Principal.Can] never matches it and [Expand] carries it
// through unchanged. A writer may REMOVE it from a row and never ADD it
// (internal/iamdomain's MayConfer enforces that at the record), and Tier A's
// configuration refuses it outright, naming [AllGrants], because a file an
// operator wrote is a typo rather than an upgrade.
type Grant string

const (
	// GrantAdmin runs the company and the engine: the company document,
	// its integrations, models, settings and budgets, the workspace
	// catalogue, the tool-skill pages, the credential store, the identity
	// directory, the deployment's own controls, the company-wide audit
	// trail and the purges. And it INCLUDES EVERY APP, through [Expand], so
	// an administrator is never told they lack a permission an app opens.
	//
	// IT IS HOST ACCESS, and it is ONE permission by decision rather than by
	// oversight. The configuration it writes runs code: an `mcp_servers`
	// entry is a command every engine host executes, a seat's `mcp_env` and
	// its per-phase model keys (`llm_*`, a `cli-agent` provider among them)
	// choose what the seat's children run and which credentials they are
	// handed, a model entry's `run_in: direct` runs the seat's executor, a
	// fully permissioned coding agent, on the engine host, and a seat's
	// worker grants decide which tools a worker runs with. So a holder can
	// run anything on every engine host and read
	// whatever a process there can — the keyring included — and every
	// other permission is transitively theirs. A split permission for "the
	// part that runs code" was weighed and declined: it would be the whole
	// of this one's practical reach under a second name. It is conferred
	// the way shell on those hosts is.
	//
	// A UNIT'S LEAD changes the seats and units inside it without this
	// permission (internal/authz's subtree rule), every field but a
	// credential's; what lies outside every lead's subtree, every setting
	// and every credential the chart names are this permission's alone.
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
	// WHICH ROOM IS NEVER THIS PERMISSION'S. A public room and a unit's room
	// are every holder's, and a private room, a group and a direct
	// conversation are their MEMBERSHIP (internal/chatlet's Visible) — an
	// administrator's included, who reads no conversation they are not in.
	// And it is spoken in as a SEAT: a credential acting as itself holds it
	// and still says nothing, because a room renders a speaker by seat
	// (internal/authz's seated rows). A company whose chat is a vendor's
	// (`chat.backend: vendor`) is given nothing by it, as the tracker app
	// gives a company on Jira nothing.
	GrantChatlet Grant = "chatlet"
)

// AllGrants are every permission this build knows, in declaration order.
//
// Named AllGrants rather than Grants because [Principal.Grants] is the field a
// reader meets first, and one name for the company's whole vocabulary and for
// one principal's slice of it is a sentence that reads wrong either way.
var AllGrants = []Grant{GrantAdmin, GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet}

// Apps are the permissions that open one APP each, and THE ONE PLACE AN APP IS
// DECLARED.
//
// Everything that has to know which permissions are apps reads this list: an
// agent seat holds exactly these (it uses the company's apps and never runs
// the company), [Expand] adds them to an administrator, the dashboard's access
// picker ticks and locks them under Admin, and internal/authz's walks hold
// every one of them to a verb a person can take; the live socket's audience
// reads it too, so an app added here reaches every reader with no second edit.
// The dashboard's own copy is held to this one by a gate, so an app added here
// and nowhere else fails the build rather than shipping a permission no screen
// offers.
//
// APPENDED, NEVER INSERTED: the order is what [Expand] stores in a row and
// what every list of an administrator's permissions reads in, so a new app
// goes last and no stored row moves.
var Apps = []Grant{GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet}

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
// EVERY COMPARISON OF PERMISSIONS GOES THROUGH IT — [Principal.Can], a node's
// admin posture ([AdminPosture.Effective]), a machine token's mint against its
// owner, what a writer may confer, an invitation's bound and Tier A's token
// check — because "admin includes every app" stated at each of those would be
// six copies of one rule, and the day one of them forgot, an administrator
// would be refused a page by the one comparison that did.
//
// A FRESH SLICE THAT NEVER ALIASES set, so a caller that appends to the answer
// never edits a row it read. It holds set's own members in their order —
// unknown names kept, duplicates kept as given, because what a row says is the
// row's business — and then, when set holds admin, every member of [Apps] set
// does not already hold, in [Apps] order. It adds nothing otherwise, and
// Expand(Expand(s)) equals Expand(s).
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

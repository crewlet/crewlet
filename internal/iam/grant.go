package iam

import "slices"

// Access is the read/write classification a route, a query or a tool declares.
//
// A NAMED STRING RATHER THAN A BOOL, and the zero is invalid, because a bool
// here has two readings that drift apart — `write` reads as "may write" at a
// gate and "is a write" at a route table — and because a bool's zero is a
// valid setting, which is exactly what the tree's zero-value rule refuses: a
// surface somebody forgot to classify would ship as a read.
//
// It is DECLARED, never derived from an HTTP method here. internal/api/auth
// already owns that mapping (GET/HEAD/OPTIONS), and a second copy of it in a
// package with no HTTP in it is how the two come to disagree about a verb.
type Access string

const (
	// AccessRead changes nothing and starts nothing. It is the class a
	// narrow service account is cut down to: a deliberately public read
	// surface is a credential holding read grants and nothing else, which
	// is listable, revocable and present in the audit log — none of which
	// the `allow_anonymous_read` posture this replaced could be.
	AccessRead Access = "read"

	// AccessWrite changes something, starts something, or hands out a
	// value that cannot be taken back.
	AccessWrite Access = "write"
)

// Accesses are the two.
var Accesses = []Access{AccessRead, AccessWrite}

// Valid reports whether an access off the wire is one this build knows.
func (a Access) Valid() bool { return slices.Contains(Accesses, a) }

// Grant is one capability a principal carries.
//
// A CLOSED SET OF ELEVEN, drawn from the surfaces this engine actually has to
// authorize — internal/api's route table, configapi, secretsapi, setupapi and
// the builtin tool set — rather than from a taxonomy invented to look tidy.
// Each constant below says which surface it covers and why it is not folded
// into its neighbour.
//
// The wire strings are `<domain>:<verb>`, but [Grant.Access] does NOT split
// them: `fleet:operate` has no verb a split could classify, and a classifier
// that works for nine values out of ten is a classifier that fails silently on
// the tenth. The table is the answer.
//
// THE ZERO IS INVALID, and that is load-bearing rather than decorative: a gate
// somebody forgot to fill in holds the zero Grant, and a zero that validated
// would ship that gate as "granted".
type Grant string

const (
	// GrantStateRead reads what the company is DOING: the board, the
	// pages, the roster, the org chart, the fleet, budgets, schedules —
	// every named read route in internal/api/rest.go and the socket
	// frames built from the same functions. The ordinary dashboard read.
	GrantStateRead Grant = "state:read"

	// GrantAuditRead reads the RECORD of what happened, whoever or
	// whatever it happened to: /events, /agents/{id}/memory and the turn
	// frames on /ws/stream, which carry full prompts, tool arguments and
	// diary entries — and the identity estate's own trail, /iam/audit,
	// beside them. Separate from [GrantStateRead] because showing
	// somebody the board and showing them every prompt an agent was ever
	// given are not one decision.
	//
	// ONE GRANT OVER BOTH TRAILS rather than a transcript read and an
	// audit read, because they have one audience and one question — what
	// did this company do, and who asked it to — and the two answers are
	// the same answer read from two tables: an agent's turn names the
	// principal that woke it, and a sign-in names the session every later
	// turn carried. A reader holding one and refused the other could
	// establish neither half of that.
	GrantAuditRead Grant = "audit:read"

	// GrantConfigRead reads the company document. Its own grant, and not
	// [GrantStateRead], for the reason /config is guarded even for reads:
	// the document is the org chart, every integration and the SHAPE of
	// every credential the company holds, which is a map of what to
	// attack. The same reason covers /setup's and /secrets' listings —
	// the names of the credentials a company has NOT configured are worth
	// as much as the configuration.
	GrantConfigRead Grant = "config:read"

	// GrantSecretRead reveals a stored credential's VALUE — the one
	// /secrets route that answers with one, which takes an explicit
	// ?reveal=true and logs the access. Split from [GrantSecretWrite]
	// because rotating a credential and reading one out are opposite
	// risks: an automation that reseals keys nightly needs the write and
	// must never hold the read.
	GrantSecretRead Grant = "secrets:read"

	// GrantWorkWrite files and moves work: create, update, comment, merge
	// and the project facets a writer may declare — the tracker half of
	// both /operator and a seat's own builtins. It is the tracker's
	// `operator` author kind made into a capability, and it stays
	// separate from [GrantKnowledgeWrite] because a company routinely
	// wants an automation that files bugs and may not edit the handbook.
	GrantWorkWrite Grant = "work:write"

	// GrantKnowledgeWrite authors the company's own pages: write_page,
	// save_page and comment_on_page, and the same tools offered over
	// /operator. A page is an ADDRESS other pages and prompts resolve, so
	// a write here changes what every seat reads next turn.
	GrantKnowledgeWrite Grant = "knowledge:write"

	// GrantConfigWrite changes the company: PATCH /config and the epoch
	// activation that follows, which rebuilds every seat's tools,
	// providers and MCP children — and the org chart's structure, its
	// runtime half and the relations authority is derived from.
	//
	// IT IS HOST ACCESS, and it is ONE grant by decision rather than by
	// oversight. The configuration it writes runs code: an `mcp_servers`
	// entry is a command every engine host executes, a seat's `mcp_env`
	// and its per-phase model keys (`llm_*`, a `cli-agent` provider among
	// them) choose what the seat's children run and which credentials they
	// are handed, a `sandbox` cell of `run_in: self` runs a coding agent on
	// the engine host, and a seat's worker grants decide which tools a
	// worker runs with. So a holder can run anything on every engine host
	// and read whatever a process there can — the keyring included — and
	// every grant below is transitively theirs. A split grant for "the
	// part that runs code" was weighed and declined: it would be the whole
	// of this grant's practical reach under a second name, and a ceiling
	// that withheld it would withhold every settings edit with it. It is
	// conferred the way shell on those hosts is.
	//
	// /setup gets NO GRANT OF ITS OWN, deliberately. It performs no write
	// of its own — a credential goes through the store /secrets serves
	// and a pointer through the merge and compare-and-set /config
	// performs — so connecting an integration is exactly this grant plus
	// [GrantSecretWrite]. A third grant beside them would be a second
	// answer to one question, and the day the two answers differ is the
	// day one of them stops being consulted.
	GrantConfigWrite Grant = "config:write"

	// GrantSecretWrite seals, rotates and deletes the fleet's
	// credentials, and re-keys the store: everything /secrets serves bar
	// the reveal. A write here is irreversible in the direction that
	// matters — a deleted credential is gone from every node at once.
	GrantSecretWrite Grant = "secrets:write"

	// GrantFleetOperate is the node's own controls, which change no
	// company data and can stop the company dead: POST /backup (which
	// copies every credential and every seat's memory to a path the
	// caller names), the retention floor, the capacity window, the
	// maintenance gestures, evict and readmit, and POST /budgets/reset —
	// and the purges, a work item's and a page's, which nothing undoes.
	// One grant rather than one per gesture because they share a blast
	// radius and an audience — whoever runs the deployment — and
	// splitting them would invite a gate that holds all of them but one.
	//
	// IT IS HALF OF TWO MORE, each asked beside a second grant because it
	// is the deployment's reach over somebody else's subject: an object's
	// REMOVAL from the org chart takes it with [GrantConfigWrite] — the
	// one structural change nothing undoes, the address tombstoned for
	// ever and a seat's mailbox, lease and diary gone with it, which is a
	// purge's reach over the company's own structure — and ending every
	// session in the company takes it with [GrantPeopleManage].
	//
	// AND IT IS THE ADMIN PATH over the relation classes internal/authz
	// decides somebody's WORK by — a colleague's queue, a project's
	// policy — though not over an org chart object's prose, which is
	// [GrantConfigWrite]'s.
	GrantFleetOperate Grant = "fleet:operate"

	// GrantPeopleManage is authority over PERSON ROWS: inviting somebody,
	// changing what they carry, suspending them, revoking their sessions,
	// resetting a second factor, removing them.
	//
	// THE GRANT THAT CAN GRANT, and its own grant rather than a part of
	// [GrantConfigWrite] so that a DIRECTORY change is its own gesture:
	// changing the company document rebuilds every seat's tools and
	// providers, changing a person's row decides who may do that tomorrow,
	// and the two are reviewed by different people for different reasons.
	// The person who onboards a team has no business editing `mcp_servers`.
	//
	// IT IS NOT A BOUND ON A config:write HOLDER, and must not be read as
	// one. That grant is host access ([GrantConfigWrite]): a holder can run
	// code on every engine host and reach whatever a process there can,
	// the keyring and the store this grant writes to included. What the
	// separation buys is that the ORDINARY path — the one a review, an
	// audit row and a revocation all watch — puts directory changes in
	// somebody else's hands, not that the other path is closed.
	//
	// IT IS ALSO THE ONE GRANT THAT BOUNDS ITSELF. A caller may not
	// confer a grant they do not hold — on anybody, themselves included —
	// so holding this does not reach past whatever else the holder
	// carries. internal/iamdomain enforces that at the RECORD, because a
	// record can be published by a CLI, a duty and a migration, none of
	// which passes through a route.
	GrantPeopleManage Grant = "people:manage"

	// GrantSandboxRun starts a detached coding run and holds the per-run
	// credential the MCP bridge mints for the box. The one grant that
	// puts generated code on a machine and hands it a seat's whole tool
	// surface, which is why it is not folded into [GrantWorkWrite] even
	// though a coding run is usually how work gets done.
	GrantSandboxRun Grant = "sandbox:run"
)

// AllGrants are the eleven, in the order the constants declare them.
//
// Named AllGrants rather than Grants because [Principal.Grants] is the field a
// reader meets first, and one name for the company's whole vocabulary and for
// one principal's slice of it is a sentence that reads wrong either way.
var AllGrants = []Grant{
	GrantStateRead,
	GrantAuditRead,
	GrantConfigRead,
	GrantSecretRead,
	GrantWorkWrite,
	GrantKnowledgeWrite,
	GrantConfigWrite,
	GrantSecretWrite,
	GrantFleetOperate,
	GrantPeopleManage,
	GrantSandboxRun,
}

// PersonPresentGrants are the grants whose gestures need a PERSON present, and
// which a machine token therefore never carries, whatever its owner holds:
// revealing a credential's value, and deciding who may do anything at all.
//
// [GrantConfigWrite] IS DELIBERATELY NOT HERE, although it is host access and
// strictly stronger than either: applying a configuration from a pipeline —
// `crewlet config import` in a deploy job — is exactly the work a machine token
// exists for, and a token that could not carry it would send that pipeline back
// to a Tier A token, which answers to no person and ends only when somebody
// edits the deployment's own configuration. So a token may be minted with it,
// and minting one is handing a pipeline shell on every engine host: its owner
// decides that at the mint, and the token's row says what it carries. This list
// is for the gestures a pipeline has no business making at all, not a ranking
// of how dangerous a grant is.
//
// HERE, IN THE LEAF, because two packages that cannot see each other both have
// to enforce it: internal/iamdomain refuses them at a token's mint, and
// internal/iam/credential drops them from what a token carries on every
// request. The second is not redundant — a rule that held only at the mint
// would be a rule a row from anywhere else could step round. They are one of
// the two locks between a token and the sensitive window ([Recency]); the other
// is that the request guard never proves a token for that window at all, which
// is what closes the gestures a person makes about THEMSELVES on no grant. A
// token is what an attacker holding a pipeline's environment already has.
var PersonPresentGrants = []Grant{GrantSecretRead, GrantPeopleManage}

// grantAccess classifies every grant. A map rather than a string split, for
// the reason [Grant] gives; and every member of [AllGrants] appears, which
// grant_test.go asserts in both directions so a new grant cannot be added
// without a reader deciding what class it is in.
var grantAccess = map[Grant]Access{
	GrantStateRead:      AccessRead,
	GrantAuditRead:      AccessRead,
	GrantConfigRead:     AccessRead,
	GrantSecretRead:     AccessRead,
	GrantWorkWrite:      AccessWrite,
	GrantKnowledgeWrite: AccessWrite,
	GrantConfigWrite:    AccessWrite,
	GrantSecretWrite:    AccessWrite,
	GrantFleetOperate:   AccessWrite,
	GrantPeopleManage:   AccessWrite,
	GrantSandboxRun:     AccessWrite,
}

// Valid reports whether a grant off the wire is one this build knows.
//
// An unknown grant is an ordinary event on a rolling upgrade — a newer peer
// writes a capability string this build has never heard of — so this answers
// false and NOTHING ELSE HAPPENS: see [Principal.Can] for what that buys and
// [Principal.Validate] for what it deliberately does not cost.
func (g Grant) Valid() bool { return slices.Contains(AllGrants, g) }

// Access reports whether this grant covers reading or changing.
//
// An unknown grant has no access class — this build cannot know what a
// capability it has never heard of would open — so it answers the zero
// [Access], which is invalid and which every gate must therefore refuse rather
// than read as "read".
func (g Grant) Access() Access { return grantAccess[g] }

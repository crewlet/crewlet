// Package stream is the dashboard's live channel: one WebSocket per tab,
// carrying the RESULT of applying each event to the server's projection.
//
// The fan-out is separated from the transport, and the split is structural
// rather than stylistic. [Hub] holds the clients, the per-client queue and the
// backpressure rule, and no WebSocket is reachable from it — so "a slow tab
// loses its oldest envelope rather than stalling every other tab" is a property
// a test can assert directly, instead of one inferred from a socket that
// happened not to fall over.
//
// # What a client is, and what decides its bytes
//
// A [Client] carries two things beyond its queue: the [FramePosture] it is
// being served in, and the ROOMS it is in. The posture decides the BYTES (one
// encode serves every client in the same posture) and a room decides the
// AUDIENCE for the kinds routed to rooms. A room is a seat's (the sockets whose
// watch of that seat was allowed, for `inbox_changed`) or an artifact's (the
// sockets whose join of it was allowed, for what moves on the artifact, its
// datastore and its presence), and ONE KEYED INDEX holds both ([RoomKey]), so
// one rule, "a targeted frame with no room is dropped, never broadcast", covers
// every targeted kind. Everything else about the fan-out follows from those two
// and from the total route table below.
//
// # Who may be a recipient for a seat
//
// The hub INDEXES a watch and decides nothing about it; the socket decides it
// ([watching]), the way the `work_inbox` question decides a read of the same
// seat's inbox — its holder, whoever leads it, an admin, each with the
// tracker — through the
// same three-valued chart seam, and re-decides it whenever the socket's
// credential is decided again (lifetime.go).
// The seat's room carries `inbox_changed`, which says whose inbox moved, when
// and why; an index anyone could write to would be a way to follow somebody
// else's inbox that the question itself refuses.
//
// # Who may be in an artifact's room
//
// Decided like reading the artifact, and decided again whenever access to it
// can have narrowed: see rooms.go.
//
// AND IT IS RESOLVED BEFORE IT IS DECIDED, through [iam.OwnerOf] and the same
// identity directory that question reads: a frame is pushed to the name a
// person's notices are kept under — their seat, when the directory binds them
// to one — so a watch naming their login is installed on that seat, and the
// directory is asked only once the watch has been decided as far as it can
// be without the record.
//
// # An open socket's credential
//
// A socket is authenticated at its handshake and ended by what ends its
// credential, never by a timer of its own: an identity move naming it is
// decided by the guard ([Service.CredentialsMoved]), a move naming nobody
// closes every socket [CloseUndecided] for the handshake to decide, a
// published company is decided in memory, and the credential's own end by a
// timer. lifetime.go states every case and what each close code asks of the
// client.
package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/tokens"
)

var log = logging.Get("api.stream")

// writeTimeout bounds ONE frame written to a client.
//
// A TCP peer that vanishes without a FIN — a closed laptop, a dropped NAT
// entry, a mobile handoff — leaves a Write blocked until the kernel gives up,
// which is minutes with default keepalives. Until then the socket's goroutine,
// its queue and its hub registration stay live, so a page nobody is reading
// holds a slot on every broadcast.
//
// THIRTY SECONDS, and it is deliberately generous: [QueueDepth] below decides
// that a slow tab must not be disconnected, so this exists to tell GONE from
// SLOW and nothing else. A tighter deadline would sever a mobile tab
// mid-snapshot and contradict that decision.
const writeTimeout = 30 * time.Second

// QueueDepth is how many frames one client may have waiting.
//
// Drop-OLDEST past it, never block. A slow tab must not stall the publish path
// or any other tab, and it must not be disconnected either: the reconnect flow
// refetches a snapshot, so dropping is recoverable and a disconnect is a
// visible failure for a reader who did nothing wrong.
//
// Oldest rather than newest, because what a dashboard shows is the CURRENT
// state: the newest frame is the one that makes the screen right, and
// dropping it to keep an older one would leave the tab further behind than
// doing nothing.
const QueueDepth = 512

// Kind is what a frame from the socket is: a push, one of the two answers to a
// query, or the pong.
//
// A NAMED TYPE, so the one place a kind is spelled is a constant below and
// every frame is built from one — and the route, need and posture tables are
// keyed by it, so a kind spelled as a bare string somewhere else does not
// compile. The dashboard's `PushKind` union is its copy of exactly these
// constants — `contract/wire.ts`, held both ways by
// `TestTheDashboardKnowsExactlyThePushKindsTheEngineSends`, which reads every
// constant of this type from this package's source — and the socket's dispatch
// falls silently through a kind that union does not name. So a kind is frozen
// against RENAMING once a dashboard renders it: a renamed kind is a broken
// client, not a refactor. A NEW kind is additive — the client's dispatch
// ignores a kind it does not know, which is what lets the engine add one
// before the screen that renders it. Every one of them has an entry on
// [routes]; see TestEveryPushKindHasARoute.
type Kind string

// The push kinds.
const (
	KindSnapshot  Kind = "snapshot"
	KindEvent     Kind = "event"
	KindAgents    Kind = "agents"
	KindSeats     Kind = "seats"
	KindSandboxes Kind = "sandboxes"
	KindTokens    Kind = "tokens"
	KindBudget    Kind = "budget"
	KindSchedules Kind = "schedules"
	KindOrg       Kind = "org"
	KindTools     Kind = "tools"
	KindHealth    Kind = "health"

	// KindInboxChanged says one seat's inbox moved, and it is routed to an
	// AUDIENCE rather than to everyone: it carries the seat in
	// [Envelope.Seat] and reaches only the clients in that seat's room,
	// the ones that asked to watch it (see [Hub.Watch]) and were allowed to
	// (see [watching]).
	//
	// IT IS HOW A PERSON LEARNS THEY HAVE WORK. The tracker's applier, on
	// every node serving the API — each holds the estate, since ingress
	// requires the `data` role — says whose inbox each committed batch
	// moved, and each node pushes that to its OWN sockets through
	// [Service.InboxChanged]: every such node applies every record, so no
	// node forwards to another. The
	// payload is an [InboxChange]: identifiers and a count, never content.
	KindInboxChanged Kind = "inbox_changed"

	// KindArtifactChanged says one artifact moved: a version published, its
	// displayed fields or a label, its shares, its assets, the trash, a
	// transfer or a purge. Routed to the artifact's room, and a HINT: the
	// artifact, what moved, the head version and the record's position,
	// never content, so a screen re-reads through the questions that decide
	// what it may see. Pushed by every data node from its own applier
	// ([Service.ArtifactsMoved]).
	KindArtifactChanged Kind = "artifact_changed"

	// KindArtifactData is what a committed datastore batch changed, routed
	// to the artifact's room and SHAPED PER READER: a document in an
	// `own_rows`, `editors` or `private` collection reaches only the sockets
	// its rule admits, and the changed documents travel inline while the
	// frame stays within [artifacts.PushFrameMaxBytes] and as their keys
	// with a re-read hint past it ([Service.ArtifactDataMoved]).
	KindArtifactData Kind = "artifact_data"

	// KindArtifactPresence is a room's presence fan-out: the members it now
	// presents that are new or changed, the sessions it no longer presents,
	// and for a large room the counts beside its most recent fifty
	// (internal/presence). Routed to the artifact's room; told, never
	// stored.
	KindArtifactPresence Kind = "artifact_presence"

	// KindResult and KindError answer one query, correlated by the
	// client-minted id it was asked under.
	KindResult Kind = "result"
	KindError  Kind = "error"

	KindPong Kind = "pong"
)

// routes is how each kind reaches the clients it is for. See [Route].
//
// BESIDE THE KINDS, and total over them: a kind declared above with no entry
// here has no route at all, which [RouteOf] answers as the invalid zero Route
// and TestEveryPushKindHasARoute fails the build over.
var routes = map[Kind]Route{
	// The company's own state. What one tab sees, every tab sees.
	KindSnapshot:  RouteDirect,
	KindEvent:     RouteBroadcast,
	KindAgents:    RouteBroadcast,
	KindSeats:     RouteBroadcast,
	KindSandboxes: RouteBroadcast,
	KindTokens:    RouteBroadcast,
	KindBudget:    RouteBroadcast,
	KindSchedules: RouteBroadcast,
	KindOrg:       RouteBroadcast,
	KindTools:     RouteBroadcast,
	KindHealth:    RouteBroadcast,

	// One room's audience: a seat's, or an artifact's.
	KindInboxChanged:     RouteRoom,
	KindArtifactChanged:  RouteRoom,
	KindArtifactData:     RouteRoom,
	KindArtifactPresence: RouteRoom,

	// The company's own chat, each frame to the viewers its room admits —
	// decided by the chat hub per recipient, never fanned out by route.
	KindChatPosted:            RouteViewer,
	KindChatEdited:            RouteViewer,
	KindChatDeleted:           RouteViewer,
	KindChatReacted:           RouteViewer,
	KindChatRoomChanged:       RouteViewer,
	KindChatMembershipChanged: RouteViewer,
	KindChatCursorMoved:       RouteViewer,
	KindChatPresence:          RouteViewer,

	// The answers, to the one client that asked.
	KindResult: RouteDirect,
	KindError:  RouteDirect,
	KindPong:   RouteDirect,
}

// Need is who may read a push kind that says something about the COMPANY.
//
// A NAMED STRING rather than a grant, because most of what the socket carries
// asks no permission at all: the overview — the roster, what each seat is
// doing, the org chart, spend, schedules, the tools, health — is everybody's
// (ADR-0033). What a need says is whether the kind is everybody's, admin's,
// or one app's — and an app's need IS the app's own name ([NeedFor]), so the
// set of needs is derived from [iam.Apps] rather than kept beside it: an app
// declared there is a need here, and an audience meets it, with no edit to
// this package.
type Need string

const (
	// NeedEverybody is the overview's: any principal the guard resolved
	// whose stage may act — which every open socket is, since the handshake
	// admits nobody else.
	NeedEverybody Need = "everybody"

	// NeedAdmin is the company-wide audit trail's: an event is the
	// `events` question's row, which carries a phase's prompts and its
	// response for every seat at once.
	NeedAdmin Need = "admin"

	// NeedWorklet is Worklet's: an `inbox_changed` frame says whose inbox
	// moved, which is what the `work_inbox` question it tells a screen to
	// ask again answers.
	NeedWorklet = Need(iam.GrantWorklet)

	// NeedArtifact is an artifact room's: decided PER ARTIFACT, by the
	// reader's role on it, which no app answers. A frame of such a kind
	// reaches only the sockets in the artifact's room, and a socket is in
	// it only because its join was decided like reading the artifact
	// (`get_artifact`) and stays decided ([rooms]); any resolved reader may
	// be in a room, so no grant stands in front of the kind itself. It is
	// NO APP'S NEED, the `artifacts` app's included ([NeedFor]): that app
	// gates authoring an artifact and never reading one.
	NeedArtifact Need = "artifact"
	// NeedChatlet is Chatlet's: every chat frame — a committed record, a
	// person's own read state, a room's presence — is the chat questions'
	// rows, which their holders alone ask. It is the reader's half of
	// what admits a chat frame; WHICH room is the chat hub's per-recipient
	// decision on top of it.
	NeedChatlet = Need(iam.GrantChatlet)
)

// NeedFor is the need of a kind one app's holders may read.
func NeedFor(app iam.Grant) Need { return Need(app) }

// Needs is the closed set: the three that are no app's, then one per app in
// [iam.Apps]' order.
var Needs = append([]Need{NeedEverybody, NeedAdmin, NeedArtifact}, appNeeds()...)

// appNeeds is one need per app.
func appNeeds() []Need {
	out := make([]Need, 0, len(iam.Apps))
	for _, app := range iam.Apps {
		out = append(out, NeedFor(app))
	}
	return out
}

// app is the app this need is, or false for one that is not an app's.
func (n Need) app() (iam.Grant, bool) {
	app := iam.Grant(n)
	return app, app.IsApp()
}

// Valid reports whether n is a need this build knows.
func (n Need) Valid() bool { return slices.Contains(Needs, n) }

// needs is who may read each kind that says something about the COMPANY.
//
// BESIDE THE ROUTES and total over the kinds together with [answers]: a route
// says who a frame is addressed to, and this says who may read it at all.
//
// EACH KIND IS DECLARED AS ITS QUESTION IS, so no fact is reachable by
// choosing a channel: an event is the `events` query's row, so admin's; an
// `inbox_changed` frame the tracker's; everything else is the overview the
// questions beside it answer to everybody. What of the overview a reader
// receives is decided per payload — a seat's live call is masked to its
// status for a reader its trail is not open to — by [Shaped].
//
// A kind in neither table is received by NOBODY — see [Audience.Receives].
var needs = map[Kind]Need{
	KindSnapshot:     NeedEverybody,
	KindEvent:        NeedAdmin,
	KindAgents:       NeedEverybody,
	KindSeats:        NeedEverybody,
	KindSandboxes:    NeedEverybody,
	KindTokens:       NeedEverybody,
	KindBudget:       NeedEverybody,
	KindSchedules:    NeedEverybody,
	KindOrg:          NeedEverybody,
	KindTools:        NeedEverybody,
	KindHealth:       NeedEverybody,
	KindInboxChanged: NeedWorklet,

	KindArtifactChanged:  NeedArtifact,
	KindArtifactData:     NeedArtifact,
	KindArtifactPresence: NeedArtifact,

	KindChatPosted:            NeedChatlet,
	KindChatEdited:            NeedChatlet,
	KindChatDeleted:           NeedChatlet,
	KindChatReacted:           NeedChatlet,
	KindChatRoomChanged:       NeedChatlet,
	KindChatMembershipChanged: NeedChatlet,
	KindChatCursorMoved:       NeedChatlet,
	KindChatPresence:          NeedChatlet,
}

// NeedOf is who may read kind, and false for a kind that says nothing about
// the company — an answer to a socket's own exchange — or one this build does
// not declare.
func NeedOf(kind Kind) (Need, bool) {
	need, gated := needs[kind]
	return need, gated
}

// Grants are the permissions any one of which admits a reader to a kind of
// this need: what a refusal names. The overview names none, since nothing the
// reader could be given would change an answer they already have, and an
// artifact room names none, since what admits a reader there is a share.
func (n Need) Grants() []iam.Grant {
	if n == NeedAdmin {
		return []iam.Grant{iam.GrantAdmin}
	}
	if app, ok := n.app(); ok {
		return []iam.Grant{app}
	}
	return nil
}

// answers are the kinds that say nothing about the company, only about this
// socket's own exchange — which is why no need stands in front of them. A
// query's result or refusal was already decided by the posture its question is
// registered under, and deciding it again here would be a second opinion that
// could only ever disagree; a pong carries nothing at all.
var answers = map[Kind]bool{
	KindResult: true,
	KindError:  true,
	KindPong:   true,
}

// Audience is what one reader may receive: whether they are an admin, which
// apps they use, their own record, which seats' bodies (what a seat is
// saying, as opposed to what it is doing) are theirs to read, and the
// principal it was described for.
//
// ITS ZERO VALUE RECEIVES NO COMPANY FACT AT ALL, because it was resolved for
// nobody: the closed end and the only safe one, since a reader nobody
// described is not one this node knows may see anything.
//
// A VALUE, compared whole by [Audience.Equal], because an open socket keeps
// the audience it was opened with and a published company that would give it
// another closes it for the reconnect to build the new one.
//
// # The apps are a set read from iam.Apps, never a field per app
//
// It held one boolean per app and a switch over them in [Audience.Has], so an
// app added to the vocabulary was an app no socket reader held until somebody
// remembered a second edit here, in a switch nothing compared with the list.
// The set is whatever [iam.Apps] declares, filled through [iam.Principal.Can]
// (an administrator through every app), so the audience can never disagree
// with the authority table about which apps a reader uses.
//
// # And the principal, for what is decided per object
//
// An artifact is decided by the reader's ROLE on it, which no app answers:
// whether a socket may join an artifact's room, and which of its rows a push
// may carry to it, are the authority table's question about this principal
// and that artifact ([authz.StandingOn]). So the audience keeps who it was
// described for ([Audience.Principal]) rather than a second, narrower copy of
// the facts a role is decided from.
type Audience struct {
	resolved bool
	admin    bool

	// apps are the apps this reader uses, in [iam.Apps]' order — every one
	// for an administrator. A SET DERIVED FROM THE APPS rather than a field
	// per app, so the next app is one line in internal/iam and not five
	// edits here, any one of which forgotten is an audience that silently
	// never receives that app's frames or never compares unequal.
	apps   []iam.Grant
	record string

	// body is the agent ids of the seats whose body this reader may read,
	// sorted. Unread for an admin, who reads every body whatever it holds.
	body []string

	// principal is who this audience was described for. Never handed out
	// shared: [Audience.Principal] copies its grants.
	principal iam.Principal
}

// NewAudience is the audience principal p describes, given the agent ids of
// the seats `audit.seat.read` admits p to. An admin reads every body whatever
// body holds; anybody else reads exactly those.
//
// [Service.AudienceFor] is how the engine builds one — it decides body from
// the authority table, seat by seat. This constructor is the value half, for
// a caller that already holds the decision.
func NewAudience(p iam.Principal, body []string) Audience {
	p.Grants = slices.Clone(p.Grants)
	a := Audience{resolved: true, admin: p.Can(iam.GrantAdmin),
		record: iam.RecordOwner(p), principal: p}
	for _, app := range iam.Apps {
		if p.Can(app) {
			a.apps = append(a.apps, app)
		}
	}
	if !a.admin {
		a.body = slices.Compact(slices.Sorted(slices.Values(body)))
	}
	return a
}

// Receives reports whether this audience may read a frame of kind.
func (a Audience) Receives(kind Kind) bool {
	if answers[kind] {
		return true
	}
	if !a.resolved {
		return false
	}
	need, declared := needs[kind]
	switch {
	case !declared:
		return false
	case need == NeedEverybody:
		return true
	case need == NeedAdmin:
		return a.admin
	case need == NeedArtifact:
		// THE ROOM DECIDES, by the reader's role on the artifact: a
		// socket reaches a frame of this need only through a join that
		// was allowed.
		return true
	}
	if app, ok := need.app(); ok {
		return a.Has(app)
	}
	return false
}

// Admin reports whether this audience is an administrator's — the question an
// org projection asks to decide which variant of itself a reader receives.
func (a Audience) Admin() bool { return a.resolved && a.admin }

// Has reports whether this audience may use app — an administrator through
// every app. A permission that is not an app (admin itself, or a name this
// build does not know) is no app anybody uses: [Audience.Admin] is the
// question about admin.
func (a Audience) Has(app iam.Grant) bool {
	// THE ADMINISTRATOR'S APPS ARE ALREADY IN THE SET — [NewAudience] asks
	// [iam.Principal.Can], which expands admin to every app — so an
	// administrator is not a case here, and a name that is no app is no
	// audience's, theirs included.
	return a.resolved && app.IsApp() && slices.Contains(a.apps, app)
}

// Principal is who this audience was described for, with its own copy of the
// grants, so a caller that appends to them cannot reach this audience or any
// other client's. The zero principal for the zero audience, which every class
// of the authority table refuses.
func (a Audience) Principal() iam.Principal {
	p := a.principal
	p.Grants = slices.Clone(p.Grants)
	return p
}

// Body reports whether this audience may read the body of the seat with
// agentID: an administrator any seat's, anybody else the seats their trail
// reaches.
func (a Audience) Body(agentID string) bool {
	if !a.resolved {
		return false
	}
	if a.admin {
		return true
	}
	_, found := slices.BinarySearch(a.body, agentID)
	return agentID != "" && found
}

// Record is the reader's own record ([iam.RecordOwner]).
func (a Audience) Record() string { return a.record }

// Reach is what this audience may see of the seat with agentID's live state.
func (a Audience) Reach(agentID string) livestate.Reach {
	return livestate.Reach{Body: a.Body(agentID), Tracker: a.Has(iam.GrantWorklet),
		Config: a.resolved && a.admin, Record: a.record}
}

// Equal reports whether two audiences receive exactly the same.
//
// WHO THE PRINCIPAL IS COUNTS, beside what it may read: an artifact room is
// decided for the principal, so a reader re-bound to another seat, or re-kinded
// by its binding, receives differently even where every app and body is the
// same. What does NOT count is how recently it proved who it is: that moves on
// every step-up and changes nothing a socket is sent.
func (a Audience) Equal(b Audience) bool {
	return a.resolved == b.resolved && a.admin == b.admin &&
		slices.Equal(a.apps, b.apps) && a.record == b.record &&
		(a.admin || slices.Equal(a.body, b.body)) &&
		a.principal.ID == b.principal.ID && a.principal.Kind == b.principal.Kind &&
		a.principal.Login == b.principal.Login && a.principal.Seat == b.principal.Seat &&
		a.principal.Stage == b.principal.Stage
}

// variant is the name of what this audience receives of a payload masked by
// reach — whether it is an administrator's, which apps it uses, and which
// seats' bodies it reads — so two audiences given one variant receive
// byte-identical frames and a broadcast encodes once per variant rather than
// once per client. withRecord adds the reader's own record, for a payload that
// shows something to a person by who they are (a coding run's question).
//
// THE APPS BY NAME, in [iam.Apps] order, so an app added to the vocabulary
// splits the variants it should with no edit here.
//
// THE BODY SET AS A DIGEST, so a variant name stays short however many seats a
// lead leads; two different sets colliding would hand one reader another's
// variant, which is why it is SHA-256 rather than a cheaper hash.
func (a Audience) variant(withRecord bool) string {
	switch {
	case !a.resolved:
		return "nobody"
	case a.admin:
		return "admin"
	}
	apps := make([]string, 0, len(a.apps))
	for _, app := range a.apps {
		apps = append(apps, string(app))
	}
	sum := sha256.Sum256([]byte(strings.Join(a.body, "\x00")))
	name := fmt.Sprintf("apps=%s;body=%s", strings.Join(apps, ","),
		hex.EncodeToString(sum[:]))
	if withRecord {
		name += ";record=" + a.record
	}
	return name
}

// Shaped is a push payload whose CONTENT depends on what its reader may read,
// beyond whether they may read its kind at all.
//
// The need table ([needs]) decides WHETHER an audience receives a kind; a
// shaped payload decides WHAT of it. The roster is everybody's, and a seat's
// live call inside it is its body only to an administrator and the seat's
// lead; the org tree is everybody's, and a seat's resolved model chain and
// tool sources inside it are derived from settings only an administrator
// reads — so one kind carries several shapes, and a reader is handed the one
// its audience describes.
//
// The decision travels WITH THE PAYLOAD rather than being a second table here,
// because the payload's type is the one place that knows which of its fields
// is which: a table of kinds could only say "shaped", and would have to trust
// every producer of the kind to have built the narrow shape.
type Shaped interface {
	// For is the payload as an audience may receive it, and the name of
	// that variant. Two audiences given one variant name receive
	// byte-identical payloads, which is what lets a broadcast encode once
	// per variant and posture rather than once per client.
	For(Audience) (variant string, data any)
}

// RoomShaped is a push payload to an artifact's room whose CONTENT depends on
// the reader's ROLE on the artifact, beyond what [Shaped] decides from the
// audience: a datastore delta in an `own_rows`, `editors` or `private`
// collection reaches only the readers its rule admits. The hub hands it each
// member's [Reader], the role being what that member's join decided.
type RoomShaped interface {
	// ForReader is the payload as one reader may receive it, and the name
	// of that variant: two readers given one name receive byte-identical
	// payloads, which is what lets a push encode once per variant. deliver
	// is false for a reader the push has nothing for, who is sent nothing.
	ForReader(Reader) (variant string, data any, deliver bool)
}

// Reader is one member of an artifact's room, as a push to it is shaped.
type Reader struct {
	Audience Audience
	Role     artifacts.Role
}

// ShapeFor is data as audience may receive it — the payload itself when it is
// not [Shaped] — for a REST mirror of a push, which answers one reader the
// shape the push hands them.
func ShapeFor(data any, audience Audience) any {
	_, shaped := shapeFor(data, audience)
	return shaped
}

// shapeFor is data as audience may receive it, and its variant's name — the
// payload itself, unnamed, when it is not [Shaped].
func shapeFor(data any, audience Audience) (string, any) {
	if shaped, ok := data.(Shaped); ok {
		return shaped.For(audience)
	}
	return "", data
}

// AgentRows are `agents` or `seats` rows — roster rows with the live overlay
// merged onto them, or a push's overlay rows — as a [Shaped] payload: each row
// masked to the reach its reader has over the seat it names.
type AgentRows []map[string]any

// For masks every row for audience.
func (rows AgentRows) For(audience Audience) (string, any) {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		id, _ := row["agent_id"].(string)
		out = append(out, livestate.MaskRow(row, audience.Reach(id)))
	}
	return audience.variant(false), out
}

// SpendRollup is the live spend breakdown as the `tokens` push carries it: a
// [Shaped] payload, whole for an admin and without its per-entry breakdown for
// everybody else ([tokens.Rollup.WithoutEntries]), which is how the `tokens`
// question answers too. The variant is named after which of the two it is,
// `admin` or `overview`, as the org's is.
type SpendRollup tokens.Rollup

// For is the rollup as audience may read it.
func (r SpendRollup) For(audience Audience) (string, any) {
	if audience.Admin() {
		return "admin", tokens.Rollup(r)
	}
	return "overview", tokens.Rollup(r).WithoutEntries()
}

// SandboxRows are the in-flight coding runs as a [Shaped] payload: each masked
// to the reach its reader has over the seat that launched it.
type SandboxRows []livestate.SandboxEntry

// For masks every run for audience. THE READER'S RECORD NAMES THE VARIANT only
// while some run is parked on a question, because only a question is shown to
// somebody by who they are; every other push is shared by every reader with
// the same reach.
func (rows SandboxRows) For(audience Audience) (string, any) {
	out := make([]livestate.SandboxEntry, 0, len(rows))
	asks := false
	for _, entry := range rows {
		asks = asks || entry.Question != ""
		out = append(out, entry.For(audience.Reach(entry.AgentID)))
	}
	return audience.variant(asks), out
}

// Envelope is one server-to-client frame.
//
// ID, What and Error are omitted on a push and present on a query answer, which
// is what lets one type carry both directions of the protocol without a client
// having to know which shape to expect from which kind.
//
// Error is an [httpjson.Code] rather than a string, and that is the whole of
// what makes "the socket and REST cannot call one refusal two things" a
// compile-time fact rather than a convention: a frame can only carry a value
// from the engine's one refusal vocabulary. The query-answer subset is named
// in socket.go.
type Envelope struct {
	Kind  Kind          `json:"kind"`
	Data  any           `json:"data,omitempty"`
	TS    string        `json:"ts,omitempty"`
	ID    int64         `json:"id,omitempty"`
	What  string        `json:"what,omitempty"`
	Error httpjson.Code `json:"error,omitempty"`

	// Seat names which seat a seat room's frame is about ([PushSeat]).
	//
	// ON THE WIRE, because a client watching a seat has to be able to tell
	// two seats' frames apart if it ever watches more than one, and a
	// routing key the receiver cannot read is a key only the sender can
	// debug. Omitted on every other kind, so no existing frame changes
	// shape.
	Seat string `json:"seat,omitempty"`

	// Artifact names the artifact whose room an `error` frame withdraws or
	// refuses: a join refused, or a room taken away because the reader's
	// access to the artifact narrowed. Omitted on every other frame; an
	// artifact room's pushes name their artifact in their own payload.
	Artifact string `json:"artifact,omitempty"`

	// room is the room a [RouteRoom] frame is addressed to, and is the key
	// the fan-out routes on. NOT ON THE WIRE: the payload and [Envelope.Seat]
	// already say what the frame is about, and the key is the hub's own
	// namespacing of them. Set only by the constructors that build such a
	// frame, so a room frame cannot be built without one.
	room RoomKey

	// *Refused is a refusal on AUTHORITY's machine-readable half, on an
	// `unauthorized` error frame and nowhere else: the rule's reason, and
	// the grants any one of which would have admitted the caller —
	// FLATTENED into the frame beside `error`, under the keys the REST
	// envelope answers the same refusal with ([authz.DetailReason],
	// [authz.DetailGrants]). The socket used to drop both, so a question a
	// REST caller was refused with `{error, reason, grants}` came back over
	// the socket as a bare code, and the dashboard — whose only data
	// channel this is — could not tell a missing grant from a missing
	// relation. Nil on every other frame, which omits both keys.
	*Refused

	// Detail is the refusal's own sentence, written FOR the caller: a
	// `bad_params` refusal's (see [BadParamsError]) — which names the
	// parameter to change and the values it accepts — or an `unavailable`
	// one's state-log refusal words ([Unavailable.Detail]). ONE KEY for
	// both, as the REST envelope carries both under `detail`; set through
	// [Envelope.unavailable] on the second, so the two halves of an
	// `unavailable` frame cannot be built apart. Omitted on every other
	// frame — a query failure's own text can carry a database path, and
	// none of the rest has a reader. Both setters write it through
	// [httpjson.Sentence], as the REST envelope writes its own, so the two
	// transports carry the same sentence.
	Detail string `json:"detail,omitempty"`

	// *Unavailable is an `unavailable` query error frame's refusal code
	// and hint — the state log's code, and how long to wait before asking
	// this node again, zero when waiting changes nothing — FLATTENED beside
	// `error` exactly as a REST 503 carries them, with its words on
	// [Envelope.Detail]. Without it a client had one code for a node
	// catching up and a node that will refuse the same read until an
	// operator acts, and could only poll both. Nil on every other frame,
	// which omits both keys.
	*Unavailable
}

// unavailable sets an `unavailable` frame's refusal, its hint and its words
// together: the words travel on [Envelope.Detail], which the refusal's struct
// does not carry onto the wire itself, so setting the struct alone would send
// a refusal code with none of the words that explain it.
func (e *Envelope) unavailable(u Unavailable) {
	e.Unavailable = &u
	e.Detail = httpjson.Sentence(u.Detail)
}

// Refused is the reason and the grants an `unauthorized` frame carries, and
// the role an artifact's refusal for a role names.
//
// GRANTS IS NEVER OMITTED from a refusal that has one, because an EMPTY list is
// an answer — no capability would admit this caller, and what the deciding
// rule asks is something else, which the reason names — where an absent key
// would read as a frame that did not say. ROLE IS, on every refusal but the
// one for a role below the verb's: it is the role to ask an artifact's owner
// for, which no other refusal has.
type Refused struct {
	Reason string   `json:"reason"`
	Grants []string `json:"grants"`
	Role   string   `json:"role,omitempty"`
}

// Push builds a broadcast envelope stamped now.
func Push(kind Kind, data any, now time.Time) Envelope {
	return Envelope{Kind: kind, Data: data, TS: now.UTC().Format(time.RFC3339Nano)}
}

// PushSeat builds an envelope addressed to one seat's watchers: its room.
//
// Its own constructor rather than a field a caller sets on [Push]'s result,
// because the room is the ADDRESS for a [RouteRoom] kind: a frame of such a
// kind that reached [Hub.Broadcast] without one would be dropped, and a
// constructor that cannot produce one is how that stops being a runtime
// discovery.
func PushSeat(kind Kind, seat string, data any, now time.Time) Envelope {
	env := Push(kind, data, now)
	env.Seat = seat
	env.room = SeatRoom(seat)
	return env
}

// PushArtifact builds an envelope addressed to the sockets in one artifact's
// room, for [PushSeat]'s reason.
func PushArtifact(kind Kind, id artifacts.ID, data any, now time.Time) Envelope {
	env := Push(kind, data, now)
	env.room = ArtifactRoom(id)
	return env
}

// Room is the room a frame is addressed to, empty for one that is not
// room-routed.
func (e Envelope) Room() RoomKey { return e.room }

// Client is one connected dashboard.
type Client struct {
	// out carries encoded frames to the socket's own writer goroutine.
	// Buffered to QueueDepth; a send that would block drops the oldest
	// instead.
	//
	// FRAMES RATHER THAN ENVELOPES, which is the whole of the fan-out
	// rewrite: the bytes are marshalled once per posture by the
	// broadcaster and shared, where every writer used to marshal the same
	// envelope again for itself.
	out chan *Frame

	mu      sync.Mutex
	closed  bool
	dropped int

	// posture is how this client is being served. Set at [NewClient] and
	// moved by [Client.SetPosture]; read under the lock because the hub
	// writes it (a node changing posture) while a broadcast reads it.
	posture FramePosture

	// audience is what this client may receive. Set at [NewClient] from
	// the principal the socket was opened as, and never moved: a decision
	// that finds the principal's permissions changed, or a published
	// company that changes whose trail it reads, closes the socket, and the
	// reconnect is a new client (lifetime.go).
	audience Audience

	// rooms is every room this client is in, with the join that put it
	// there. The hub's index is the authority on which buckets the client
	// is in; this is the same fact kept beside the client, so a broadcast
	// can read a member's role, an unregister can find the buckets to
	// leave, and a decision taken about one join withdraws that join alone.
	// Written under the hub's lock and this one, read under this one.
	rooms map[RoomKey]*membership

	// joins counts every join this client has been through, a seat watch
	// cleared included, so a decision taken about ONE join can withdraw
	// that join and no later one: see [Hub.UnwatchIf] and [Hub.LeaveIf].
	joins uint64
}

// membership is one client's place in one room.
type membership struct {
	// join is which of the client's joins this is.
	join uint64

	// role is the client's role on the artifact an artifact room is for,
	// as its join (or the last decision since) found it: what a push is
	// shaped by. Empty in a seat's room.
	role artifacts.Role

	// session is the client's presence session in an artifact's room.
	session string

	// suspended means a decision about this membership is pending (the
	// artifact's access narrowed), and missed that a push it would have
	// received was withheld meanwhile ([withheldWhileSuspended]).
	suspended bool
	missed    bool
}

// withheldWhileSuspended reports whether a push of kind is held back from a
// member whose room is being decided again: the document contents of the
// datastore, which is what a reader whose share was just withdrawn must not
// receive in the moment before the decision lands. The room's other pushes
// say what moved and who is present, and keep flowing until the decision
// withdraws the room or keeps it.
func withheldWhileSuspended(kind Kind) bool { return kind == KindArtifactData }

// NewClient builds a client with an empty queue, served LIVE, that may
// receive what audience allows.
//
// Live rather than the zero posture, which is invalid by construction (see
// [FramePosture]): a client is a tab somebody just opened, and the node it
// reached is serving it unless something says otherwise. [Hub.Register] then
// moves it to the hub's current posture, which is the authority.
//
// THE AUDIENCE IS AN ARGUMENT rather than something set afterwards, so a
// client cannot exist for one instant on the hub's list with an audience
// nobody chose.
func NewClient(audience Audience) *Client {
	return &Client{out: make(chan *Frame, QueueDepth), posture: FrameLive,
		audience: audience, rooms: map[RoomKey]*membership{}}
}

// Receives reports whether this client may read a frame of kind.
func (c *Client) Receives(kind Kind) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.audience.Receives(kind)
}

// Audience is what this client may currently receive.
func (c *Client) Audience() Audience {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.audience
}

// Out is the channel a transport reads frames from. Closed by [Client.Close].
func (c *Client) Out() <-chan *Frame { return c.out }

// Posture is how this client is currently being served: the node's posture.
func (c *Client) Posture() FramePosture {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posture
}

// admits is the posture this client takes a push of kind under, and false
// where its audience's need or its posture refuses the kind — the two filters
// every push passes before it is shaped and encoded ([fanOut]), stated once so
// a producer choosing its recipients one by one can ask them FIRST: a client a
// frame will never reach is not worth a read to decide whether it may see it.
func (c *Client) admits(kind Kind) (FramePosture, bool) {
	// WHO MAY READ IT before what the posture delivers: a client the kind
	// is not declared for is not in its audience whatever the route says.
	// See [needs].
	if !c.Receives(kind) {
		return "", false
	}
	posture := c.Posture()
	if posture.index() < 0 || !posture.Delivers(kind) {
		return "", false
	}
	return posture, true
}

// SetPosture moves this client to p, reporting whether it took.
//
// AN INVALID POSTURE IS REFUSED AND THE CURRENT ONE KEPT. The zero value is
// the one a caller reaches by forgetting to decide, and the two ways to serve
// it are opposite — everything, or nothing — so taking it would either leak a
// degraded node's state or blank a healthy dashboard, with nothing on either
// path to say which happened.
func (c *Client) SetPosture(p FramePosture) bool {
	if !p.Valid() {
		log.Warn("stream_invalid_frame_posture", "posture", string(p),
			"hint", "the client keeps the posture it had; a frame posture is "+
				"live or degraded, and the zero value is not a posture")
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posture = p
	return true
}

// Seat is the seat this client is a recipient for, or empty.
func (c *Client) Seat() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seat, _ := c.seatLocked()
	return seat
}

// seatLocked is the seat whose room this client is in and that membership.
// Caller holds c.mu.
func (c *Client) seatLocked() (string, *membership) {
	for key, m := range c.rooms {
		if seat, ok := key.seat(); ok {
			return seat, m
		}
	}
	return "", nil
}

// Dropped reports how many frames this client has lost to backpressure.
//
// Reported rather than merely counted: a tab that is behind is a tab showing
// something other than the truth, and the number is the only evidence of it
// that survives the drop.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Reply encodes an envelope for THIS client alone and queues it.
//
// The [RouteDirect] path: a snapshot, a pong, a query's result or refusal.
// One client, so one encode, and nothing is shared — which is exactly why it
// is a different method from the broadcast path rather than a one-client call
// into it.
func (c *Client) Reply(env Envelope) {
	if !c.Receives(env.Kind) {
		// A WIRING FAULT, never a decision made here: every path that
		// replies with a company fact builds it for this client's own
		// audience first. Logged loudly because the alternative to
		// dropping it is sending it to somebody who may not read it.
		log.Error("stream_reply_withheld", "kind", env.Kind,
			"hint", "build the frame from the client's own audience; this "+
				"client is not among the readers the kind is declared for")
		return
	}
	if !c.Posture().Delivers(env.Kind) {
		return
	}
	// SHAPED FOR THIS CLIENT, as a broadcast of the same payload would be.
	_, env.Data = shapeFor(env.Data, c.Audience())
	frame, err := EncodeFrame(env)
	if err != nil {
		// A frame that cannot be encoded is this server's bug, not the
		// client's. Dropping it keeps the socket alive for every other
		// kind rather than tearing down a working dashboard over one
		// malformed answer.
		log.Error("stream_encode_failed", "kind", env.Kind, "error", err)
		return
	}
	c.send(frame)
}

// send queues a frame, dropping the oldest if the queue is full.
//
// Never blocks. The caller is the ingest path, shared by every client, so one
// slow reader blocking here would stop the whole fan-out.
//
// The lock is held ACROSS the channel operations, not merely around the closed
// check. Checking and then releasing leaves a window in which Close runs and
// the send lands on a closed channel — which panics, and takes the whole
// broadcast with it. The two ends genuinely race: the transport closes when its
// socket dies while the ingest path is fanning out. Holding it is cheap because
// nothing here blocks: every operation is a select with a default.
func (c *Client) send(frame *Frame) {
	if frame == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for {
		select {
		case c.out <- frame:
			return
		default:
		}
		// Full. Take one off the front and try again. The take can only
		// fail if the transport's reader emptied the queue in between,
		// and then the next send succeeds — so the loop always makes
		// progress. Looping rather than dropping the NEW frame is what
		// keeps the newest state on screen.
		select {
		case <-c.out:
			c.dropped++
		default:
		}
	}
}

// Close stops the client and releases its queue.
//
// Idempotent, because both ends can reach it: the transport closes when the
// socket dies, and the hub closes when it is shutting down.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.out)
}

// RoomKey names one room on the hub's index: a seat's, whose inbox frames reach
// the sockets watching it, or an artifact's, whose pushes reach the sockets in
// it. NAMESPACED, so no seat handle can name an artifact's room or the reverse,
// and one index answers both.
type RoomKey string

const (
	seatRoomPrefix     = "seat:"
	artifactRoomPrefix = "artifact:"
)

// SeatRoom is the room of one seat's watchers.
func SeatRoom(handle string) RoomKey { return RoomKey(seatRoomPrefix + handle) }

// ArtifactRoom is the room of the sockets in one artifact.
func ArtifactRoom(id artifacts.ID) RoomKey { return RoomKey(artifactRoomPrefix + id.String()) }

// seat is the handle a seat's room names, and false for any other room.
func (k RoomKey) seat() (string, bool) { return strings.CutPrefix(string(k), seatRoomPrefix) }

// Artifact is the artifact an artifact's room names, and false for any other
// room.
func (k RoomKey) Artifact() (artifacts.ID, bool) {
	raw, ok := strings.CutPrefix(string(k), artifactRoomPrefix)
	if !ok {
		return artifacts.ID{}, false
	}
	id, err := artifacts.ParseID(raw)
	return id, err == nil
}

// Hub is the set of connected clients, and the index that routes a frame to
// the ones it is for.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}

	// rooms is the routing index: which clients are in which room. A
	// SECOND MAP rather than a filter over clients, because a per-room frame
	// on a company with fifty tabs open would otherwise walk fifty clients
	// to find the few in it, on every inbox movement and every committed
	// datastore batch, and the walk's cost is paid by the publish path.
	//
	// A room with no members holds NO entry: an empty bucket left behind by
	// the last member leaving is a map that grows with the number of seats
	// and artifacts anyone has ever looked at.
	rooms map[RoomKey]map[*Client]struct{}

	// posture is the node's current frame posture, applied to every client
	// registered from now on and to every client already registered when
	// it moves. The node is what is degraded, not the tab — but the value
	// is carried per client all the same, because a client registers at an
	// arbitrary moment (including during a change) and a frame has to be
	// decided by one value read once, not by a second read that can move
	// between a fan-out's start and one client's send.
	posture FramePosture
}

// NewHub builds an empty hub, serving LIVE until told otherwise.
func NewHub() *Hub {
	return &Hub{
		clients: map[*Client]struct{}{},
		rooms:   map[RoomKey]map[*Client]struct{}{},
		posture: FrameLive,
	}
}

// Register adds a client to the fan-out, at the hub's current posture.
//
// BEFORE its snapshot is sent, deliberately. The window between registering and
// snapshotting delivers frames describing state the snapshot also carries —
// which is harmless, because the client dedupes streamed envelopes against the
// snapshot by event id. Registering AFTER would instead lose everything
// published in that window, which nothing recovers.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	posture := h.posture
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	c.SetPosture(posture)
}

// Unregister removes a client from every room and from the fan-out, and closes
// it.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	_, present := h.clients[c]
	delete(h.clients, c)
	c.mu.Lock()
	for key := range c.rooms {
		h.leaveLocked(c, key)
	}
	c.mu.Unlock()
	h.mu.Unlock()
	if present {
		if dropped := c.Dropped(); dropped > 0 {
			log.Info("stream_client_left_behind", "dropped", dropped,
				"hint", "the tab could not keep up and lost frames; its "+
					"reconnect refetches a snapshot")
		}
	}
	c.Close()
}

// joinLocked puts c in the room key with membership m, replacing whatever
// membership it held there, and answers the one replaced. Caller holds h.mu
// for writing and c.mu.
func (h *Hub) joinLocked(c *Client, key RoomKey, m *membership) *membership {
	prior := c.rooms[key]
	c.joins++
	m.join = c.joins
	c.rooms[key] = m
	members, ok := h.rooms[key]
	if !ok {
		members = map[*Client]struct{}{}
		h.rooms[key] = members
	}
	members[c] = struct{}{}
	return prior
}

// leaveLocked takes c out of the room key, answering the membership it held
// there or nil. Caller holds h.mu for writing and c.mu.
func (h *Hub) leaveLocked(c *Client, key RoomKey) *membership {
	m, ok := c.rooms[key]
	if !ok {
		return nil
	}
	delete(c.rooms, key)
	members := h.rooms[key]
	delete(members, c)
	if len(members) == 0 {
		delete(h.rooms, key)
	}
	return m
}

// Watch makes c a recipient for seat, replacing whatever it watched before.
//
// ONE SEAT AT A TIME, because a tab keeps one inbox current — the dashboard
// watches the viewer's own seat for as long as the tab is open — and a client
// that accumulated every seat it was ever pointed at would keep receiving
// frames nobody is reading. An empty seat clears the subscription.
//
// A client that is not registered is not indexed: the index is a subset of the
// fan-out, and a bucket holding a client the hub has already let go of would
// keep it alive and keep sending to its closed queue.
func (h *Hub) Watch(c *Client, seat string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, present := h.clients[c]; !present {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, _ := c.seatLocked(); current != "" {
		h.leaveLocked(c, SeatRoom(current))
	}
	if seat == "" {
		c.joins++
		return
	}
	h.joinLocked(c, SeatRoom(seat), &membership{})
}

// Watching is the seat c watches and which join that is, for a decision about
// it taken outside the hub's lock — see [Hub.UnwatchIf].
func (h *Hub) Watching(c *Client) (seat string, watch uint64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	seat, m := c.seatLocked()
	if m == nil {
		return "", c.joins
	}
	return seat, m.join
}

// UnwatchIf withdraws c's watch only if it is still the watch [Hub.Watching]
// named, reporting whether it withdrew anything.
//
// A COMPARE-AND-CLEAR, because the decision to withdraw is taken on another
// goroutine: deciding a credential again reads the watch, asks the authority
// table with no lock held, and by the time a refusal comes back the socket's
// own read loop may have installed an allowed watch for a DIFFERENT seat —
// which is exactly when the old one starts being refused, a rebind moving the
// viewer's seat. An unconditional clear there withdrew the new, allowed
// watch and told the tab it was refused, and the dashboard retries only a
// refusal that says "unavailable", so that tab heard nothing until its next
// socket.
func (h *Hub) UnwatchIf(c *Client, watch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	seat, m := c.seatLocked()
	if m == nil || m.join != watch {
		return false
	}
	h.leaveLocked(c, SeatRoom(seat))
	c.joins++
	return true
}

// Watchers reports how many clients are recipients for one seat.
func (h *Hub) Watchers(seat string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[SeatRoom(seat)])
}

// WatchedSeats is every seat at least one client watches, sorted — the
// audience of a frame that has to reach every watched seat at once
// ([Service.InboxReread]). The index holds no empty bucket, so a seat listed
// here has a watcher.
func (h *Hub) WatchedSeats() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var seats []string
	for key := range h.rooms {
		if seat, ok := key.seat(); ok {
			seats = append(seats, seat)
		}
	}
	slices.Sort(seats)
	return seats
}

// RoomMember is one client's place in an artifact's room, as a decision about
// it is taken outside the hub's lock: which join it is, the role it was last
// decided at and its presence session.
type RoomMember struct {
	Client  *Client
	Join    uint64
	Role    artifacts.Role
	Session string
}

// JoinArtifact puts c in id's room at role, under its presence session, and
// answers the join and the session of a membership it replaced (a socket
// joining an artifact it was already in), empty when it replaced none. False
// for a client the hub does not hold, which is put in no room.
func (h *Hub) JoinArtifact(c *Client, id artifacts.ID, role artifacts.Role,
	session string) (join uint64, replaced string, ok bool) {

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, present := h.clients[c]; !present {
		return 0, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &membership{role: role, session: session}
	if prior := h.joinLocked(c, ArtifactRoom(id), m); prior != nil {
		replaced = prior.session
	}
	return m.join, replaced, true
}

// LeaveArtifact takes c out of id's room, answering the presence session it
// held there, false when it was not in it.
func (h *Hub) LeaveArtifact(c *Client, id artifacts.ID) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := h.leaveLocked(c, ArtifactRoom(id))
	if m == nil {
		return "", false
	}
	return m.session, true
}

// LeaveArtifactIf takes c out of id's room only if it is still the join a
// decision was taken about, for [Hub.UnwatchIf]'s reason, answering the
// session it held there.
func (h *Hub) LeaveArtifactIf(c *Client, id artifacts.ID, join uint64) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	key := ArtifactRoom(id)
	if m := c.rooms[key]; m == nil || m.join != join {
		return "", false
	}
	m := h.leaveLocked(c, key)
	return m.session, true
}

// Session is c's presence session in id's room, false when it is not in it.
func (h *Hub) Session(c *Client, id artifacts.ID) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.rooms[ArtifactRoom(id)]
	if !ok {
		return "", false
	}
	return m.session, true
}

// Artifacts is every artifact room c is in, each as a decision about it is
// taken, sorted by artifact.
func (h *Hub) Artifacts(c *Client) map[artifacts.ID]RoomMember {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[artifacts.ID]RoomMember{}
	for key, m := range c.rooms {
		if id, ok := key.Artifact(); ok {
			out[id] = RoomMember{Client: c, Join: m.join, Role: m.role, Session: m.session}
		}
	}
	return out
}

// Members is every client in id's room.
func (h *Hub) Members(id artifacts.ID) []RoomMember {
	h.mu.RLock()
	defer h.mu.RUnlock()
	key := ArtifactRoom(id)
	out := make([]RoomMember, 0, len(h.rooms[key]))
	for c := range h.rooms[key] {
		c.mu.Lock()
		if m, ok := c.rooms[key]; ok {
			out = append(out, RoomMember{Client: c, Join: m.join, Role: m.role, Session: m.session})
		}
		c.mu.Unlock()
	}
	return out
}

// RoomsHeld is every artifact at least one client is in, sorted.
func (h *Hub) RoomsHeld() []artifacts.ID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []artifacts.ID
	for key := range h.rooms {
		if id, ok := key.Artifact(); ok {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b artifacts.ID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// Suspend marks every membership of id's room as pending a decision: until
// each is decided again ([Hub.Resume], [Hub.LeaveArtifactIf]) it receives no
// push [withheldWhileSuspended] withholds. Called on the applier's goroutine
// the moment a narrowing of the artifact's access lands, so no document
// reaches a reader whose share was withdrawn in the moment before the decision
// that removes them.
func (h *Hub) Suspend(id artifacts.ID) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	key := ArtifactRoom(id)
	for c := range h.rooms[key] {
		c.mu.Lock()
		if m, ok := c.rooms[key]; ok {
			m.suspended = true
		}
		c.mu.Unlock()
	}
}

// Hold suspends c's membership of id's room alone, if it is still the join
// that was decided: what a decision that could not be reached does to a
// member, so it receives no push [withheldWhileSuspended] withholds until a
// later decision resumes or withdraws it. It reports whether the membership
// was there to hold.
func (h *Hub) Hold(c *Client, id artifacts.ID, join uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.rooms[ArtifactRoom(id)]
	if m == nil || m.join != join {
		return false
	}
	m.suspended = true
	return true
}

// Resume lifts a suspension on c's membership of id's room, if it is still
// the join that was decided, at the role the decision found; missed reports
// that a push was withheld meanwhile, which the caller follows with a re-read
// hint.
func (h *Hub) Resume(c *Client, id artifacts.ID, join uint64, role artifacts.Role) (missed, ok bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.rooms[ArtifactRoom(id)]
	if m == nil || m.join != join {
		return false, false
	}
	missed = m.missed
	m.role, m.suspended, m.missed = role, false, false
	return missed, true
}

// SetPosture moves the hub and every client on it to p.
//
// The NODE is what changes posture, so this is where a change lands: one call
// per health tick rather than a read per client per frame, which would put a
// coordination round trip on the publish path.
func (h *Hub) SetPosture(p FramePosture) {
	if !p.Valid() {
		log.Warn("stream_invalid_frame_posture", "posture", string(p),
			"hint", "the hub keeps the posture it had")
		return
	}
	h.mu.Lock()
	h.posture = p
	targets := slices.Collect(maps.Keys(h.clients))
	h.mu.Unlock()

	for _, c := range targets {
		c.SetPosture(p)
	}
}

// Posture is the posture the hub is serving new clients at.
func (h *Hub) Posture() FramePosture {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.posture
}

// Clients reports how many are connected.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast encodes an envelope ONCE PER POSTURE AND VARIANT and hands the bytes
// to every client the kind's route names.
//
// # Once per posture, not once per client
//
// A client's posture is the only thing that decides its bytes, so two clients
// in the same posture must receive the byte-identical frame from one encode.
// With N dashboards open this used to be N marshals of identical JSON — on N
// goroutines, for every push — because the envelope travelled to each writer
// and each writer encoded it. The cache below is keyed by posture and payload
// variant, filled lazily, so a company with every tab in one posture pays
// exactly one encode for a plain payload.
//
// # The route decides the audience
//
// [RouteBroadcast] is every client; [RouteRoom] is the clients in the frame's
// room and nobody else. [RouteDirect] is REFUSED here: a result carries the
// correlation id of one client's question, and fanning it out would answer
// every other tab's question with it. [RouteViewer] is refused too: its
// recipients are chosen one by one by the chat hub, and a chat frame fanned
// out by route is a private room on every tab. A kind with no route at all is
// refused for the same reason: there is no safe default, and "everyone" is the
// unsafe one.
//
// Never blocks on any client: each client's own queue absorbs the difference,
// and one that has fallen behind loses its oldest frame rather than holding up
// the publish path.
func (h *Hub) Broadcast(env Envelope) {
	route := RouteOf(env.Kind)
	switch {
	case !route.Valid():
		log.Error("stream_unrouted_kind", "kind", env.Kind,
			"hint", "add the kind to internal/api/stream's route table; a push "+
				"with no route is dropped rather than sent to every client")
		return
	case route == RouteDirect:
		log.Error("stream_direct_kind_broadcast", "kind", env.Kind,
			"hint", "an answer belongs to the client that asked; use Client.Reply")
		return
	case route == RouteViewer:
		log.Error("stream_viewer_kind_broadcast", "kind", env.Kind,
			"hint", "a chat frame reaches the viewers its room admits, decided "+
				"one by one by the chat hub; broadcast, it is a private room "+
				"delivered to every open tab")
		return
	case route == RouteRoom && env.room == "":
		log.Error("stream_room_frame_without_a_room", "kind", env.Kind,
			"hint", "build it with PushSeat or PushArtifact; a room-routed frame "+
				"with no room has no audience and is never fanned out to everyone")
		return
	}

	h.mu.RLock()
	var targets []*Client
	if route == RouteRoom {
		targets = slices.Collect(maps.Keys(h.rooms[env.room]))
	} else {
		targets = slices.Collect(maps.Keys(h.clients))
	}
	h.mu.RUnlock()

	fanOut(targets, env)
}

// fanOut hands env to every target its need, its posture and its room admit,
// encoding it ONCE PER POSTURE AND PAYLOAD VARIANT: a plain payload has one
// variant, and a [Shaped] or [RoomShaped] one as many as the readers it tells
// apart.
//
// THE ONE PATH A PUSH LEAVES BY, whoever chose the targets — [Hub.Broadcast]
// from its route, and a producer that decides its recipients one by one from
// its own — so who may read a kind, what a posture delivers, how a payload is
// shaped and how often it is encoded are each decided in one place. A producer
// with a send loop of its own would be a second answer to all four.
func fanOut(targets []*Client, env Envelope) {
	type encoding struct {
		slot    int
		variant string
	}
	cache := map[encoding]*Frame{}
	for _, c := range targets {
		// WHO MAY READ IT AND WHAT ITS POSTURE DELIVERS, before how it is
		// encoded. See [Client.admits].
		posture, admitted := c.admits(env.Kind)
		if !admitted {
			continue
		}
		slot := posture.index()
		reader, member := c.reader(env.room, env.Kind)
		if !member {
			continue
		}
		variant, data, deliver := shapeForReader(env.Data, reader)
		if !deliver {
			continue
		}
		key := encoding{slot: slot, variant: variant}
		frame, encoded := cache[key]
		if !encoded {
			// STORED BEFORE IT IS CHECKED, so an envelope that cannot be
			// encoded costs one failed marshal for the whole posture and
			// variant rather than one per client. A nil frame is then
			// that encoding's answer and [Client.send] ignores it.
			shaped := env
			shaped.Data = data
			var err error
			frame, err = EncodeFrame(shaped)
			if err != nil {
				log.Error("stream_encode_failed", "kind", env.Kind, "error", err)
				frame = nil
			}
			cache[key] = frame
		}
		c.send(frame)
	}
}

// reader is who c is as a push to room is shaped, and false where c must not
// receive it: no longer in the room, or in it while its room is being decided
// again and the kind is one withheld meanwhile (which marks the push missed).
// For a push that is not room-routed, c's audience with no role.
func (c *Client) reader(room RoomKey, kind Kind) (Reader, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := Reader{Audience: c.audience}
	if room == "" {
		return r, true
	}
	m, ok := c.rooms[room]
	if !ok {
		return r, false
	}
	if m.suspended && withheldWhileSuspended(kind) {
		m.missed = true
		return r, false
	}
	r.Role = m.role
	return r, true
}

// shapeForReader is data as one reader may receive it: [RoomShaped] by their
// role, [Shaped] by their audience, and the payload itself otherwise.
func shapeForReader(data any, r Reader) (string, any, bool) {
	if shaped, ok := data.(RoomShaped); ok {
		return shaped.ForReader(r)
	}
	variant, shaped := shapeFor(data, r.Audience)
	return variant, shaped, true
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	targets := slices.Collect(maps.Keys(h.clients))
	h.clients = map[*Client]struct{}{}
	h.rooms = map[RoomKey]map[*Client]struct{}{}
	h.mu.Unlock()

	for _, c := range targets {
		c.Close()
	}
}

package estate

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- the knowledge base -------------------------------------------------- //

type listPagesArgs struct {
	Filter doclet.Filter
	Fresh  statelog.Freshness
}

var opListPages = define("pages.list", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a listPagesArgs) (doclet.Listing, error) {
		if b.Pages == nil {
			return doclet.Listing{}, errNoHalf
		}
		return b.Pages.List(ctx, a.Filter, a.Fresh)
	})

type getPageArgs struct {
	Ref   string
	Fresh statelog.Freshness
}

var opGetPage = define("pages.get", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a getPageArgs) (doclet.Detail, error) {
		if b.Pages == nil {
			return doclet.Detail{}, errNoHalf
		}
		return b.Pages.Get(ctx, a.Ref, a.Fresh)
	})

type skillPagesArgs struct {
	Container string
	Fresh     statelog.Freshness
}

var opSkillPages = define("pages.skill_pages", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]doclet.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type containersArgs struct {
	Fresh statelog.Freshness
}

var opContainers = define("pages.containers", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a containersArgs) ([]doclet.ContainerListing, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.Containers(ctx, a.Fresh)
	})

// A PAGE'S ACTIVITY, a container's, or the whole knowledge base's.
var opPageActivity = define("pages.activity", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q doclet.PageActivityQuery) (doclet.PageActivity, error) {
		if b.Pages == nil {
			return doclet.PageActivity{}, errNoHalf
		}
		return b.Pages.Activity(ctx, q)
	})

var opPageTurnsMoved = define("pages.turns_moved", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q doclet.TurnsMovedQuery) (map[string]bool, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.TurnsMovedPages(ctx, q)
	})

type revisionArgs struct {
	PageID  string
	Version int
	Fresh   statelog.Freshness
}

// aRevision is [PageReader.Revision]'s two answers, named.
type aRevision struct {
	Revision doclet.Revision
	Found    bool
}

var opRevision = define("pages.revision", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a revisionArgs) (aRevision, error) {
		if b.Pages == nil {
			return aRevision{}, errNoHalf
		}
		rev, found, err := b.Pages.Revision(ctx, a.PageID, a.Version, a.Fresh)
		return aRevision{Revision: rev, Found: found}, err
	})

// keyed reports a page write the caller made REPEATABLE: one whose actor
// carries the caller's own operation key ([doclet.Actor.OpKey]), from which the
// pages store derives the operation id — bound to what the write says — and a
// create its page's id, so a repeat under it is the same operation.
func keyed(actor doclet.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

// chatKeyed is [keyed] for a chat write ([chatlet.Actor.OpKey]): the chat store
// derives the operation id, and a message's or a new room's, from the key and
// what the gesture asks.
func chatKeyed(actor chatlet.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

// pageActorWrites is the check every page write is held to before it is asked
// anywhere ([op.refusedBeforeAsking]): that the actor its arguments hold may
// write ([actorWrites]).
func pageActorWrites(actor doclet.Actor) error {
	return actorWrites(actor.Handle, iam.ActorKind(actor.Kind), actor.WrittenFor)
}

// chatActorWrites is [pageActorWrites] for a chat write, every gesture whose
// arguments hold the actor it writes as. A reach writes nothing, and is held
// to nothing here.
func chatActorWrites(actor chatlet.Actor) error {
	return actorWrites(actor.Handle, iam.ActorKind(actor.Kind), actor.WrittenFor)
}

type createPageArgs struct {
	Actor doclet.Actor
	Page  doclet.NewPage
}

var opCreatePage = define("pages.create", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a createPageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Create(ctx, a.Actor, a.Page)
	}).repeatableWhen(func(a createPageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a createPageArgs) error { return pageActorWrites(a.Actor) })

type savePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Save   doclet.Save
}

var opSavePage = define("pages.save", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a savePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.SavePage(ctx, a.Actor, a.PageID, a.Save)
	}).repeatableWhen(func(a savePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a savePageArgs) error { return pageActorWrites(a.Actor) })

type renamePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Title  string
	Quiet  bool
}

var opRenamePage = define("pages.rename", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a renamePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Rename(ctx, a.Actor, a.PageID, a.Title, a.Quiet)
	}).repeatableWhen(func(a renamePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a renamePageArgs) error { return pageActorWrites(a.Actor) })

type commentArgs struct {
	Actor   doclet.Actor
	PageID  string
	Comment doclet.NewComment
}

// commented is a comment write's two answers, named.
type commented struct {
	Comment doclet.Comment
	Written doclet.Written
}

// A COMMENT IS REPEATABLE UNDER ITS CALLER'S KEY ([doclet.Actor.OpKey]), as
// every page write is: a seat's turn hands its call's own, a person's request
// its Idempotency-Key, and the store derives the comment's id from it, so a
// re-run or a retry posts once.
var opCommentPage = define("pages.comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a commentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.Comment(ctx, a.Actor, a.PageID, a.Comment)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a commentArgs) bool {
	return keyed(a.Actor)
}).refusedBeforeAsking(func(a commentArgs) error { return pageActorWrites(a.Actor) })

type editCommentArgs struct {
	Actor     doclet.Actor
	PageID    string
	CommentID string
	Body      string
}

var opEditComment = define("pages.edit_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a editCommentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.EditComment(ctx, a.Actor, a.PageID, a.CommentID, a.Body)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a editCommentArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a editCommentArgs) error { return pageActorWrites(a.Actor) })

// ---- the operator's page writes ------------------------------------------ //

// The knowledge base's writes no seat is handed, routed as the operator's
// tracker writes are. Each is a once-write made repeatable by the caller's key:
// unkeyed, a second trash is a second record and a second purge a refusal of
// its own first copy.

type pageRefArgs struct {
	Actor  doclet.Actor
	PageID string
}

var opTrashPage = define("pages.trash", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Trash(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a pageRefArgs) error { return pageActorWrites(a.Actor) })

var opRestorePage = define("pages.restore", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Restore(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a pageRefArgs) error { return pageActorWrites(a.Actor) })

type purgePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Reason string
}

var opPurgePage = define("pages.purge", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a purgePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Purge(ctx, a.Actor, a.PageID, a.Reason)
	}).repeatableWhen(func(a purgePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a purgePageArgs) error { return pageActorWrites(a.Actor) })

type removeCommentArgs struct {
	Actor     doclet.Actor
	PageID    string
	CommentID string

	// Authority is the caller's answer from the authority table — whether
	// it may take down a remark it did not write — decided on the asking
	// node, which holds the grants, and judged against the remark's author
	// in the serving node's own snapshot.
	Authority doclet.CommentAuthority
}

var opRemoveComment = define("pages.remove_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a removeCommentArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.RemoveComment(ctx, a.Actor, a.PageID, a.CommentID, a.Authority)
	}).repeatableWhen(func(a removeCommentArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a removeCommentArgs) error { return pageActorWrites(a.Actor) })

// knowledgeArgs is a search as it crosses. The seat is its HANDLE and the
// org is the serving node's own, for the reason the package doc gives:
// neither is a value, both are the current chart.
type knowledgeArgs struct {
	Text  string
	Seat  string
	Limit int

	// Mode is how to rank. Empty is hybrid — what a caller that names no
	// mode (the prefetch, a seat's search_knowledge tool) sends.
	Mode knowledge.Mode

	// Scoped says the caller searched with an org, which is what supplies
	// the read scope; false searches as nobody.
	Scoped bool

	// ExcludeAncestors keeps nil and empty APART, which the query tells
	// apart on purpose: nil takes the default exclusion, and an empty list
	// is a caller deliberately searching drafts. Omitted-if-empty would
	// collapse the second into the first.
	ExcludeAncestors []string
	Exclusion        bool

	// Prefetch says the search is a turn's knowledge prefetch: the serving
	// node coordinates the scan, so it is the node that times it, and the
	// budget it times it against is the turn start's ([knowledge.Query]).
	Prefetch bool
}

// query is the search a request names, against the serving node's own chart:
// the seat's role and the org whose read scope applies.
func (a knowledgeArgs) query(b Backend) knowledge.Query {
	q := knowledge.Query{Text: a.Text, Limit: a.Limit, Mode: a.Mode, Prefetch: a.Prefetch}
	if a.Exclusion {
		q.ExcludeAncestors = a.ExcludeAncestors
		if q.ExcludeAncestors == nil {
			q.ExcludeAncestors = []string{}
		}
	}
	if b.Seat != nil {
		seat, o := b.Seat(a.Seat)
		if a.Seat != "" {
			q.Seat = seat
		}
		if a.Scoped {
			q.Org = o
		}
	}
	return q
}

// THE KNOWLEDGE SEARCH READS THE INDEX OVER THE KNOWLEDGE BASE'S ROWS, so it
// runs only on a copy whose knowledge base is in service — and it carries no
// floor, for [opWorkSearch]'s reason: the index is one no log position
// describes.
var opKnowledgeSearch = define("knowledge.search", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a knowledgeArgs) (knowledge.Result, error) {
		if b.Knowledge == nil {
			return knowledge.Result{}, errNoHalf
		}
		return b.Knowledge.Answer(ctx, a.query(b))
	}).floorless()

// WHETHER THE INDEX IS STILL BUILDING, which is what turns an empty search into
// "still indexing" — asked, like the search, of the index over the knowledge
// base's rows rather than of the log.
var opKnowledgeBuilding = define("knowledge.building", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	}).floorless()

// ---- the usage domain's reads ------------------------------------------- //

// THE USAGE DOMAIN'S READS cross like every other domain's, so a node whose
// usage copy is out of service is answered from a peer's (ADR-0079).
// FLOORLESS: no write this node makes lands on the usage log (every node
// publishes its own days from its own event log), and its rows are compacted,
// each replaced whole.
//
// FIVE ARE PAGED when a PEER answers ([UsageReader], [allPages]); this node's
// own answer is the whole read ([Backend.pageBytes]).

// usagePageBytes is the most a page of a routed usage read carries before it
// is cut at the next record's edge: A QUARTER OF THE MESSAGE
// ([queue.MaxPayloadBytes]). A page may run one group past it (a record's rows
// are never split, usage.Group); a record is one message, under 1 MiB at its
// caps (usage.TestAFullSeatDayRecordFitsOneMessage), and as read rows a few
// times that, which the other three quarters hold
// (TestAUsagePageFitsOneMessage). A reply past the message is refused, never
// dropped ([server.encodeReply]).
const usagePageBytes = queue.MaxPayloadBytes / 4

// usagePageArgs is one page of a paged usage read: the read's query, and the
// group the page before it ended on — empty for the first.
type usagePageArgs[Q any] struct {
	Query Q
	After usage.Group
}

var opUsageSpend = define("usage.spend", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.SpendRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.SpendRow]{}, errNoHalf
		}
		return b.Usage.SpendPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsageSeatTurns = define("usage.seat_turns", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.TurnsRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.TurnsRow]{}, errNoHalf
		}
		return b.Usage.SeatTurnsPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsagePersonSpend = define("usage.person_spend", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.PersonQuery]) (usage.Page[usage.PersonRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.PersonRow]{}, errNoHalf
		}
		return b.Usage.PersonSpendPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsageSeatDays = define("usage.seat_days", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.SeatDay], error) {
		if b.Usage == nil {
			return usage.Page[usage.SeatDay]{}, errNoHalf
		}
		return b.Usage.SeatDaysPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

// scheduleRunsPage is [UsageReader.ScheduleRuns]'s two answers, named.
type scheduleRunsPage struct {
	Runs []usage.ScheduleRun
	More bool
}

var opUsageScheduleRuns = define("usage.schedule_runs", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.ScheduleRunsQuery) (scheduleRunsPage, error) {
		if b.Usage == nil {
			return scheduleRunsPage{}, errNoHalf
		}
		runs, more, err := b.Usage.ScheduleRuns(ctx, q)
		return scheduleRunsPage{Runs: runs, More: more}, err
	}).floorless()

var opUsagePageReads = define("usage.page_reads", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.PageReadsQuery]) (usage.Page[usage.PageRead], error) {
		if b.Usage == nil {
			return usage.Page[usage.PageRead]{}, errNoHalf
		}
		return b.Usage.PageReadsPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

// readsElidedArgs is a range of company days, both inclusive.
type readsElidedArgs struct {
	From, To string
}

var opUsageReadsElided = define("usage.reads_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readsElidedArgs) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.ReadsElided(ctx, a.From, a.To)
	}).floorless()

// A SERVER'S TOOL CALLS ARE ONE ANSWER, NOT PAGED: one row per (tool, seat)
// summed across nodes and days, so a longer window adds no row. At a couple of
// hundred bytes a row, one message ([queue.MaxPayloadBytes]) holds some forty
// thousand: a hundred tools called by four hundred seats. A reply past it is
// refused, never dropped ([server.encodeReply]).
var opUsageServerToolCalls = define("usage.server_tool_calls", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.ServerToolCallsQuery) ([]usage.ServerToolCallsRow, error) {
		if b.Usage == nil {
			return nil, errNoHalf
		}
		return b.Usage.ServerToolCalls(ctx, q)
	}).floorless()

// A SEAT'S TURNS BY WAKE AND OUTCOME are one answer, one row per (seat, wake,
// disposition, outcome) over the range, every node and day summed.
var opUsageTurnLedger = define("usage.turn_ledger", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.SpendQuery) ([]usage.TurnLedgerRow, error) {
		if b.Usage == nil {
			return nil, errNoHalf
		}
		return b.Usage.TurnLedger(ctx, q)
	}).floorless()

var opUsageToolsElided = define("usage.tools_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readsElidedArgs) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.ToolsElided(ctx, a.From, a.To)
	}).floorless()

// repoReadsArgs is one page of a repository's readers: the read's query, the
// reader the page before it ended on — empty for the first — and the bytes its
// ASKER cuts a page at.
type repoReadsArgs struct {
	Query  usage.RepoReadsQuery
	After  usage.Group
	Budget int
}

// A REPOSITORY'S READERS ARE PAGED BY THEIR ASKER: a person walks them a page
// per request (a tool answer, a REST page), so the page is the budget the
// asker names, bounded by the wire's ([usagePageBytes]) where that is smaller.
var opUsageRepoReads = define("usage.repo_reads", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a repoReadsArgs) (usage.Page[usage.RepoReader], error) {
		if b.Usage == nil {
			return usage.Page[usage.RepoReader]{}, errNoHalf
		}
		budget := a.Budget
		if b.pageBytes > 0 && (budget <= 0 || budget > b.pageBytes) {
			budget = b.pageBytes
		}
		return b.Usage.RepoReadsPage(ctx, a.Query, a.After, budget)
	}).floorless()

var opUsageRepoReadsElided = define("usage.repo_reads_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.RepoReadsQuery) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.RepoReadsElided(ctx, q)
	}).floorless()

// linkedFromArgs names the page whose backlinks are asked for, by its id.
type linkedFromArgs struct {
	PageID string
}

// A PAGE'S BACKLINKS READ BOTH CORPORA, pages and tasks (a task's linked-page
// relations included), so they run only where this node's knowledge base AND
// tracker copies are in service ([op.alsoReads], ADR-0079): otherwise a peer's
// page would be merged with links from rows this node no longer answers for.
// No floor, for [opKnowledgeSearch]'s reason.
var opLinkedFrom = define("knowledge.linked_from", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a linkedFromArgs) (search.Backlinks, error) {
		if b.Backlinks == nil {
			return search.Backlinks{}, errNoHalf
		}
		return b.Backlinks.LinkedFrom(ctx, a.PageID)
	}).floorless().alsoReads(trackerDomain)

// ---- the identity directory ---------------------------------------------- //

// A node holding no data runs seats, which ask the directory whose record a
// login names ([iam.OwnerOf]) and who holds each human seat (the party
// registry's routing, an agent's roster); serving requests, it also asks for a
// presented machine token's row ([TokenFacts]). It holds no replicated estate
// (ADR-0025), so each is asked of a data node, floored on the identity log.

// Binding is the part of a directory row a binding question reads: who holds
// the login, their kind and enrolment stage, and the seat the row binds.
//
// NOTHING ELSE CROSSES. [iamdomain.Sighting] also carries credential verifiers
// and grants; the asking node decides nothing on either (a credential is
// verified only where it is presented), and a verifier on the broker is of use
// only to an observer. This type makes the narrowing a property of the wire,
// not of every caller's restraint. Only [opRequester]'s answer carries grants,
// since the asker decides a turn's ceiling on them, and never a verifier.
type Binding struct {
	ID    string
	Kind  iam.Kind
	Stage iam.Stage
	Login string
	Seat  string
}

// bindingOf is the binding a directory row holds.
func bindingOf(seen iamdomain.Sighting) Binding {
	return Binding{ID: seen.ID, Kind: seen.Kind, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat}
}

type bindingArgs struct {
	Login string
}

// bound is a binding question's answer: the row, and how far the serving node
// could vouch for it — read in ONE snapshot there, for
// [iamdomain.Reader.PersonByLoginVouched]'s reason.
type bound struct {
	Binding Binding
	Vouch   iamdomain.Vouch
}

// WHOSE RECORD A LOGIN NAMES is decided on the asking node, against its own
// chart and its own Tier A tokens — this answers the directory's half: the row
// and the facts a verdict is formed from, never the verdict. A login nobody
// holds is the zero binding and no error, as the directory answers it.
var opIdentityBinding = define("identity.binding", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a bindingArgs) (bound, error) {
		if b.Identity == nil {
			return bound{}, errNoHalf
		}
		seen, vouch, err := b.Identity.PersonByLoginVouched(ctx, a.Login)
		if err != nil {
			return bound{}, err
		}
		return bound{Binding: bindingOf(seen), Vouch: vouch}, nil
	})

// WHO HOLDS EACH HUMAN SEAT, read whole in one snapshot — the party registry is
// rebuilt whole from it, never patched. An error is UNKNOWN and crosses as one:
// an empty answer is a real reading ("nobody is bound to anything"), and an
// asker that read an outage as one would hand every suspended person's seat
// back to the chart.
var opSeatHolders = define("identity.seat_holders", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) ([]iamdomain.SeatHolder, error) {
		if b.Identity == nil {
			return nil, errNoHalf
		}
		return b.Identity.SeatHolders(ctx)
	})

// seatArgs names the one seat a holder question is about.
type seatArgs struct {
	Seat string
}

// heldRequester is who holds one seat, with the holder's grants, and whether
// anybody does: the directory's three-valued answer with its error crossing
// apart, as every error does.
type heldRequester struct {
	Requester iamdomain.Requester
	Held      bool
}

// WHO HOLDS ONE HUMAN SEAT, AND WITH WHAT GRANTS: [opSeatHolders]' answer for
// one seat, plus grants, in two seeks and one decode
// ([iamdomain.Reader.Requester]). A turn composes the person it acts for from
// it, up to once per call of a tool that decides for somebody (the requester
// ceiling), so its cost must not grow with the company's people. An error is
// UNKNOWN and crosses as one, never "nobody holds it", which a turn would read
// as a seat the chart alone speaks for.
//
// A NEW NAME, NEVER A RESHAPED ANSWER: a node that does not know it leaves it
// unserved and the asker moves on, so an answer without grants is never read
// as a person holding none or every app; a change to its meaning takes
// another name.
var opRequester = define("identity.requester", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a seatArgs) (heldRequester, error) {
		if b.Identity == nil {
			return heldRequester{}, errNoHalf
		}
		requester, held, err := b.Identity.Requester(ctx, a.Seat)
		if err != nil {
			return heldRequester{}, err
		}
		return heldRequester{Requester: requester, Held: held}, nil
	})

// WHAT A PRESENTED MACHINE TOKEN IS CHECKED AGAINST, read in one snapshot (the
// credential, its owner, how far this node vouches for both) and answered
// NARROWED: the verifier withheld, a MAC over it and the asker's nonce in its
// place, under the key the asker named ([TokenFacts]). The asking node, which
// holds the presented secret, decides ([credential.CheckToken]). An error is
// UNKNOWN, never "no such token", which would 401 a sound credential.
//
// A KEY THIS NODE DOES NOT HOLD RUNS NOTHING, as a request signed under one
// does ([Envelopes]): the router asks a node that holds it, and the key is
// checked before the row is read.
var opIdentityMachineToken = define("identity.machine_token", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a machineTokenArgs) (TokenFacts, error) {
		if b.Identity == nil {
			return TokenFacts{}, errNoHalf
		}
		key, held := b.verifiers.key(a.KeyID)
		if !held {
			return TokenFacts{}, declined{cause: fmt.Errorf("it holds no secrets.keys "+
				"entry %q to answer a machine token's verifier under (it holds %s)",
				a.KeyID, words.And(b.verifiers.held()))}
		}
		row, err := b.Identity.MachineToken(ctx, a.ID)
		if err != nil {
			return TokenFacts{}, err
		}
		return factsOf(row, key, a.Nonce), nil
	})

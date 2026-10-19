package authz_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// chart is a fixture hierarchy: who leads whom, and who leads what.
type chart struct {
	leads      map[[2]string]bool
	projects   map[[2]string]bool
	units      map[[2]string]bool
	containers map[[2]string]bool
	members    map[[2]string]bool
	err        error
}

func (c chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.leads[[2]string{actor, subject}], nil
}

func (c chart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.projects[[2]string{actor, project}], nil
}

// LeadsUnit answers the unit relation, which is DELIBERATELY A THIRD MAP
// rather than the project one read with a different key: the fixture is what
// catches a rule asking the wrong relation, and a fake that folded the two
// would agree with any rule that asked either.
func (c chart) LeadsUnit(_ context.Context, actor, unit string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.units[[2]string{actor, unit}], nil
}

// LeadsContainer answers the page-container relation, a FOURTH map for
// [chart.LeadsUnit]'s reason.
func (c chart) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.containers[[2]string{actor, container}], nil
}

// LeadsAnyone answers whether actor leads anybody, READ OFF THE LEADS MAP
// rather than a map of its own: it is the question "is there a subject
// [chart.Leads] answers true for", and a fixture holding a second answer to it
// could disagree with the first.
func (c chart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	for pair, led := range c.leads {
		if led && pair[0] == actor {
			return true, nil
		}
	}
	return false, nil
}

// MemberOf answers unit membership, a FIFTH map: sitting in a unit is not
// leading it, and a fake that read either for the other would agree with a
// rule that asked the wrong one.
func (c chart) MemberOf(_ context.Context, actor, unit string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.members[[2]string{actor, unit}], nil
}

// nimbus is the fixture every case below decides against: the CTO leads the
// SRE, leads the PLATFORM project, and leads the `sre` unit, in which the SRE
// sits (the CTO does not: leading a unit is not sitting in it).
func nimbus() chart {
	return chart{
		leads:      map[[2]string]bool{{"cto", "sre"}: true},
		projects:   map[[2]string]bool{{"cto", "PLATFORM"}: true},
		units:      map[[2]string]bool{{"cto", "sre"}: true},
		containers: map[[2]string]bool{{"cto", "RUNBOOKS"}: true},
		members:    map[[2]string]bool{{"sre", "sre"}: true},
	}
}

// decidedAt is the instant every case here is decided at.
//
// THE WALL CLOCK AT START-UP rather than a fixed date, because the cases that
// go through [authz.ContextGuard] are decided at the request's own instant and
// a proof fixed in 2026 would be stale against it.
var decidedAt = time.Now()

// proved gives a principal a proof of identity a minute old: inside the
// step-up window, so every case that is not about the step-up decides on its
// rule alone — the step-up has cases of its own in recency_test.go.
func proved(p iam.Principal) iam.Principal {
	at := decidedAt.Add(-time.Minute)
	p.ReauthAt = at.Add(time.Hour)
	return p
}

// person is a human principal with these grants, who proved who they are a
// minute ago.
func person(login string, grants ...iam.Grant) iam.Principal {
	return proved(iam.Principal{ID: uuid.New(), Login: login, Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: grants})
}

// machine is a credential acting as itself — a pipeline's service account the
// identity directory binds to no seat — with these grants. Fresh by
// construction, as every credential with nobody at a keyboard is.
func machine(login string, grants ...iam.Grant) iam.Principal {
	return proved(iam.Principal{ID: uuid.New(), Login: login, Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: grants})
}

// seat is an agent principal acting as a seat.
// personLeading is a HUMAN the chart knows by a seat handle, which is what
// somebody signed in and bound to a seat is: [authz.actorOf] asks the chart by
// the seat, and the human-only bar reads the KIND.
func personLeading(handle string, grants ...iam.Grant) iam.Principal {
	p := seat(handle, grants...)
	p.Kind = iam.KindPerson
	return proved(p)
}

func seat(handle string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindSeat,
		Seat: handle, Stage: iam.StageActive, Grants: grants}
}

// THE WHOLE TABLE, ONE CASE PER ROW OF IT.
//
// Written as a table because the rules ARE a table: what makes this suite
// worth anything is that a reader can see sixteen classes decided side by side
// and spot the row whose answer is wrong for its neighbours.
func TestTheAuthorityTableDecidesEveryClass(t *testing.T) {
	t.Parallel()
	worklet, doclet, chat, admin := iam.GrantWorklet, iam.GrantDoclet,
		iam.GrantChatlet, iam.GrantAdmin
	for _, c := range []struct {
		name    string
		p       iam.Principal
		action  authz.Action
		object  authz.Object
		allowed bool
		reason  authz.Reason
	}{
		// --- everybody ------------------------------------------------ //
		{"anybody looks a colleague up", person("jane.doe"),
			authz.ActionColleagueRead, authz.Object{Kind: authz.KindPerson},
			true, authz.ReasonEverybody},
		{"anybody reads who holds each seat", person("jane.doe"),
			authz.ActionSeatsRead, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonEverybody},
		{"anybody reaches an item's thread", person("jane.doe"),
			authz.ActionWorkThread, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonEverybody},

		// --- one app -------------------------------------------------- //
		{"Worklet reads the board", person("jane.doe", worklet),
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"a seat holding the apps reads a page", seat("sre", iam.Apps...),
			authz.ActionPageRead, authz.Object{Kind: authz.KindPage},
			true, authz.ReasonGrant},
		{"admin reads the board through the apps it includes",
			person("jane.doe", admin), authz.ActionWorkList,
			authz.Object{Kind: authz.KindTask}, true, authz.ReasonGrant},
		// AND THE OTHER APP DOES NOT. The whole value of one permission per
		// app is that a company can hand out one: a Doclet holder reads no
		// board and a Worklet holder no page.
		{"Doclet alone reads no board", person("ana.writer", doclet),
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},
		{"Worklet alone reads no page", person("ci.release", worklet),
			authz.ActionPageRead, authz.Object{Kind: authz.KindPage},
			false, authz.ReasonNoGrant},
		{"the overview alone reads no board", person("jane.doe"),
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},
		// A PROJECT'S FILES ARE WORKLET'S, read and kept on it and on
		// nothing narrower, never on Doclet.
		{"Worklet reads a project's file", seat("sre", worklet),
			authz.ActionFileRead,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		{"Worklet keeps a file in a project", seat("sre", worklet),
			authz.ActionFileWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		{"Doclet does not", seat("sre", doclet), authz.ActionFileWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonNoGrant},
		{"Worklet files an item", seat("sre", worklet),
			authz.ActionWorkCreate, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"Worklet does not author a page", seat("sre", worklet),
			authz.ActionPageCreate, authz.Object{Kind: authz.KindPage},
			false, authz.ReasonNoGrant},
		{"Doclet authors a page", seat("sre", doclet),
			authz.ActionPageCreate, authz.Object{Kind: authz.KindPage},
			true, authz.ReasonGrant},

		// --- any app --------------------------------------------------- //
		{"a person using Doclet asks it", personLeading("sam", doclet),
			authz.ActionKnowledgeAnswer, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonGrant},
		{"so does a person using Worklet", personLeading("sam", worklet),
			authz.ActionKnowledgeAnswer, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonGrant},
		{"somebody using neither has nothing to search", personLeading("sam"),
			authz.ActionKnowledgeAnswer, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},
		// AND CHAT IS NOT ONE OF THE APPS IT DRAWS ON: the answer searches
		// the pages and the board, never a room, so a person using chat alone
		// has nothing to search either.
		{"a person using chat alone has nothing to search", personLeading("sam", chat),
			authz.ActionKnowledgeAnswer, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},
		{"a seat never does — it has a search and a model of its own",
			seat("sre", iam.Apps...), authz.ActionKnowledgeAnswer,
			authz.Object{Kind: authz.KindCompany}, false, authz.ReasonSeatRefused},
		// AND NEITHER DOES A CREDENTIAL ACTING AS ITSELF, whatever it holds:
		// the answer runs on the model of the seat its asker acts as, and it
		// acts as none.
		{"a credential bound to no seat has no model to answer on",
			machine("ci:release", admin), authz.ActionKnowledgeAnswer,
			authz.Object{Kind: authz.KindCompany}, false, authz.ReasonUnseated},
		{"nor does a person the directory binds to none", person("jane.doe", admin),
			authz.ActionKnowledgeAnswer, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonUnseated},

		// --- the chat -------------------------------------------------- //
		{"chat posts in a room", personLeading("sam", chat), authz.ActionChatPost,
			authz.Object{Kind: authz.KindChannel}, true, authz.ReasonGrant},
		{"a seat holding the apps posts too", seat("sre", iam.Apps...),
			authz.ActionChatPost, authz.Object{Kind: authz.KindChannel},
			true, authz.ReasonGrant},
		{"Worklet alone reads no room", personLeading("sam", worklet),
			authz.ActionChatRead, authz.Object{Kind: authz.KindChannel},
			false, authz.ReasonNoGrant},
		{"a credential acting as itself says nothing, whatever it holds",
			machine("ci:release", admin), authz.ActionChatPost,
			authz.Object{Kind: authz.KindChannel}, false, authz.ReasonUnseated},
		{"a room is a person's to make", personLeading("sam", chat),
			authz.ActionChatCreate, authz.Object{Kind: authz.KindChannel},
			true, authz.ReasonGrant},
		{"and never a seat's", seat("sre", iam.Apps...), authz.ActionChatCreate,
			authz.Object{Kind: authz.KindChannel}, false, authz.ReasonSeatRefused},
		{"a message's author edits it", personLeading("sam", chat), authz.ActionChatEdit,
			authz.Object{Kind: authz.KindMessage, Author: "sam"}, true, authz.ReasonAuthor},
		{"a colleague does not", personLeading("ana", chat), authz.ActionChatEdit,
			authz.Object{Kind: authz.KindMessage, Author: "sam"}, false, authz.ReasonNotAuthor},
		{"admin takes a message down unread", personLeading("ops", admin),
			authz.ActionChatDelete, authz.Object{Kind: authz.KindMessage, Unresolved: true},
			true, authz.ReasonGrant},
		{"a unit room's lead archives it", personLeading("cto", chat),
			authz.ActionChatArchive, authz.Object{Kind: authz.KindChannel,
				Container: "sre", Author: "sam"}, true, authz.ReasonLead},
		{"its creator does", personLeading("sam", chat), authz.ActionChatArchive,
			authz.Object{Kind: authz.KindChannel, Author: "sam"}, true, authz.ReasonAuthor},
		{"a colleague does not archive a named room", personLeading("ana", chat),
			authz.ActionChatArchive, authz.Object{Kind: authz.KindChannel, Author: "sam"},
			false, authz.ReasonNotAuthor},
		{"nor the lead of a unit it is not", personLeading("cto", chat),
			authz.ActionChatArchive, authz.Object{Kind: authz.KindChannel,
				Container: "web", Author: "sam"}, false, authz.ReasonNotLead},
		{"a person marks their own read state", personLeading("sam", chat),
			authz.ActionChatMark, authz.Object{Kind: authz.KindPerson, Owner: "sam"},
			true, authz.ReasonSelf},

		// --- the person asked ------------------------------------------- //
		{"the person an open ask names reads the item", personLeading("sre"),
			authz.ActionWorkRead,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Asked: "sre"},
			true, authz.ReasonAsked},
		{"and answers on it", personLeading("sre"), authz.ActionWorkComment,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Asked: "sre"},
			true, authz.ReasonAsked},
		{"somebody else is not let in by another's ask", personLeading("cto"),
			authz.ActionWorkRead,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Asked: "sre"},
			false, authz.ReasonNoGrant},
		{"Worklet reads it whoever is asked", person("jane.doe", worklet),
			authz.ActionWorkRead,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Asked: "sre"},
			true, authz.ReasonGrant},

		// --- the caller's own working state -------------------------- //
		{"a seat reads its own episodes", seat("sre"),
			authz.ActionEpisodesQuery, authz.Object{Kind: authz.KindPerson},
			true, authz.ReasonSelf},
		{"a seat writes its own diary with no permission at all", seat("sre"),
			authz.ActionMemoryPersist, authz.Object{Kind: authz.KindPerson},
			true, authz.ReasonSelf},
		{"and never somebody else's", seat("sre"),
			authz.ActionMemoryPersist,
			authz.Object{Kind: authz.KindPerson, Owner: "cto"},
			false, authz.ReasonNotSelf},
		// NOT EVEN THE ADMIN PATH, which is what makes this its own class:
		// somebody reading a seat's memory is a different verb with a rule
		// of its own, and admitting it here too would be a second answer to
		// one question.
		{"admin does not reach a seat's diary",
			person("jane.doe", admin), authz.ActionMemoryPersist,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},

		// --- an agent's own -------------------------------------------- //
		{"a seat asks a colleague holding no permission", seat("sre"),
			authz.ActionColleagueAsk, authz.Object{Kind: authz.KindPerson, Owner: "cto"},
			true, authz.ReasonEverybody},
		{"a seat starts a coding run holding no permission", seat("sre"),
			authz.ActionSandboxRun, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonEverybody},
		{"a person holding admin does neither", person("jane.doe", admin),
			authz.ActionSandboxRun, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonSeatOnly},

		// --- own record ---------------------------------------------- //
		{"a seat marks its own inbox", seat("sre", worklet),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonSelf},
		{"but not without Worklet", seat("sre"),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNoGrant},
		{"a lead does not mark a report's inbox", seat("cto", worklet),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"admin unsticks a queue", person("jane.doe", admin),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonGrant},
		{"an unnamed owner is refused", seat("sre", worklet),
			authz.ActionInboxMark, authz.Object{Kind: authz.KindPerson},
			false, authz.ReasonUnnamed},

		// --- own or lead --------------------------------------------- //
		{"a seat sets its own priorities", seat("sre", worklet),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonSelf},
		{"a lead sets a report's priorities", seat("cto", worklet),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},
		// A LEAD WHO DOES NOT USE THE TRACKER keeps no queue in it, theirs
		// or a report's.
		{"a lead without Worklet does not", seat("cto"),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNoGrant},
		{"a colleague does not", seat("design", worklet),
			authz.ActionPrioritiesSet, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},

		// --- container ------------------------------------------------ //
		{"a project's lead writes its policy", seat("cto", worklet),
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("sre", worklet),
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"admin does", person("jane.doe", admin),
			authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},
		{"an unnamed container is refused", seat("cto", worklet),
			authz.ActionProjectPolicy, authz.Object{Kind: authz.KindProject},
			false, authz.ReasonUnnamed},
		// BUT THE TOOL ITSELF IS THE APP: adding a label is open to
		// everybody who uses Worklet, and gating the whole verb as the
		// container's refused a colleague the one facet internal/worklet
		// grants them.
		{"a colleague declares a label", seat("sre", worklet),
			authz.ActionProjectWrite,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonGrant},

		// --- tidy ------------------------------------------------------ //
		//
		// A PAGE CONTAINER IS A THIRD KEY CLASS, and the fixture holds it
		// apart from the other two: the CTO leads the RUNBOOKS container and
		// neither the project nor the unit map answers for it. A rule that
		// asked the project relation with a container key refused the lead
		// of that very container on every company that does not spell its
		// `space:` and its `project:` the same.
		{"a container's lead renames a page in it", seat("cto", doclet),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "RUNBOOKS"},
			true, authz.ReasonLead},
		{"a colleague does not", seat("sre", doclet),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "RUNBOOKS"},
			false, authz.ReasonNotLead},
		{"and the project relation is not asked for it", seat("cto", doclet),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindContainer, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		// AND WHOEVER WROTE IT TIDIES IT, whoever leads its container.
		{"the page's creator renames it", seat("sre", doclet),
			authz.ActionPageRename,
			authz.Object{Kind: authz.KindPage, Container: "RUNBOOKS", Author: "sre"},
			true, authz.ReasonAuthor},
		{"the page's creator trashes it", personLeading("sre", doclet),
			authz.ActionPageTrash,
			authz.Object{Kind: authz.KindPage, Container: "RUNBOOKS", Author: "sre"},
			true, authz.ReasonAuthor},
		{"an item's reporter removes it", seat("sre", worklet),
			authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Author: "sre"},
			true, authz.ReasonAuthor},
		{"and restores it", seat("sre", worklet),
			authz.ActionWorkRestore,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Author: "sre"},
			true, authz.ReasonAuthor},
		{"the container's lead removes an item", seat("cto", worklet),
			authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague who may file may not remove another's",
			seat("sre", worklet), authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Author: "cto"},
			false, authz.ReasonNotLead},

		// --- destructive ---------------------------------------------- //
		{"the project's lead moves an item out", seat("cto", worklet),
			authz.ActionWorkMove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"},
			true, authz.ReasonLead},
		// A MOVE HAS NO AUTHOR ARM: carrying work out of a project is the
		// project's lead's, whoever filed it.
		{"the reporter does not", seat("sre", worklet), authz.ActionWorkMove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Author: "sre"},
			false, authz.ReasonNotLead},
		{"the project's lead removes a file", seat("cto", worklet),
			authz.ActionFileRemove,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague who may write a file may not remove one",
			seat("sre", worklet), authz.ActionFileRemove,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonNotLead},

		// --- saved views ---------------------------------------------- //
		//
		// ONE VERB, TWO AUTHORITIES, picked by what the view IS. The
		// payload decides and the caller cannot choose which question
		// is asked — see [authz.ClassSavedView].
		{"a seat saves its own personal view", seat("sre", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, Owner: "sre"},
			true, authz.ReasonSelf},
		{"a lead does not rearrange a report's own strip", seat("cto", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"a project's lead saves a shared view on it", seat("cto", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindProject,
				Container: "PLATFORM"},
			true, authz.ReasonLead},
		{"a colleague does not share one there", seat("sre", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindProject,
				Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"a unit's lead shares one on the unit", seat("cto", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindUnit,
				Container: "sre"},
			true, authz.ReasonLead},
		{"a seat shares one on its own page", seat("sre", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindPerson,
				Container: "sre"},
			true, authz.ReasonSelf},
		{"and their lead does too", seat("cto", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindPerson,
				Container: "sre"},
			true, authz.ReasonLead},
		// THE WORKSPACE TAB IS THE ADMIN PATH ALONE: it lands in front
		// of every person in the company and nobody leads it.
		{"nobody leads the workspace", seat("cto", worklet),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindCompany,
				Container: "workspace"},
			false, authz.ReasonNotLead},
		{"so admin saves a workspace tab", person("jane.doe", admin),
			authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, ContainerKind: authz.KindCompany,
				Container: "workspace"},
			true, authz.ReasonGrant},
		{"a view naming neither is refused", seat("cto", worklet),
			authz.ActionViewSave, authz.Object{Kind: authz.KindView},
			false, authz.ReasonUnnamed},

		// --- the org chart's subtree ---------------------------------- //
		//
		// A UNIT IS ITS OWN RELATION, and the fixture holds it apart from
		// the project one: the CTO leads the `sre` UNIT and the PLATFORM
		// PROJECT, and neither key answers the other's map.
		//
		// A PERSON BOUND TO THE SEAT, because that is who writes the chart:
		// it is a /config write, no seat's tool reaches it, and every write
		// asks for a recent proof of identity no seat can give. NO APP: the
		// chart is the company's shape, not one of its apps.
		{"a unit's lead writes inside it", personLeading("cto"),
			authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			true, authz.ReasonLead},
		{"a colleague in it does not", personLeading("sre", iam.Apps...),
			authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			false, authz.ReasonNotLead},
		{"a unit key is not a project key", personLeading("cto"),
			authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Container: "PLATFORM"},
			false, authz.ReasonNotLead},
		{"the company root is nobody's subtree", personLeading("cto"),
			authz.ActionOrgWrite, authz.Object{Kind: authz.KindUnit},
			false, authz.ReasonNotLead},
		{"admin writes anywhere", person("jane.doe", admin), authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit}, true, authz.ReasonGrant},
		{"an agent never writes the chart, whatever it holds",
			seat("cto", admin), authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			false, authz.ReasonSeatRefused},
		{"a lead reads inside their subtree", personLeading("cto"),
			authz.ActionOrgRead,
			authz.Object{Kind: authz.KindUnit, Container: "sre"},
			true, authz.ReasonLead},
		{"admin reads anywhere", person("jane.doe", admin), authz.ActionOrgRead,
			authz.Object{Kind: authz.KindUnit}, true, authz.ReasonGrant},
		// A WRITE NOBODY HAS READ YET passes the route for anybody a unit
		// could name as its lead, and nobody else: the body decides.
		{"a person bound to a seat passes the route before the body",
			personLeading("sre"), authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Unresolved: true},
			true, authz.ReasonLead},
		{"somebody bound to no seat leads nothing", person("jane.doe"),
			authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Unresolved: true},
			false, authz.ReasonNotLead},

		// --- purge ----------------------------------------------------- //
		{"admin purges", person("jane.doe", admin), authz.ActionWorkPurge,
			authz.Object{Kind: authz.KindTask}, true, authz.ReasonGrant},
		{"a seat holding admin does not", seat("sre", admin), authz.ActionWorkPurge,
			authz.Object{Kind: authz.KindTask}, false, authz.ReasonSeatRefused},
		// THE ONE TIDY AN AUTHOR DOES NOT TAKE: nothing undoes it.
		{"an item's reporter does not purge it", person("jane.doe", worklet),
			authz.ActionWorkPurge,
			authz.Object{Kind: authz.KindTask, Author: "jane.doe"},
			false, authz.ReasonNoGrant},
		// AND THE HUMAN-ONLY BAR COMPOSES WITH A RELATION rather than only
		// with admin, which is why it is a field on the row and not a class:
		// a project's own lead archives it, and an agent that leads one does
		// not — a project nobody can file into again is a company decision.
		{"an agent leading a project does not archive it", seat("cto", worklet),
			authz.ActionProjectArchive,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			false, authz.ReasonSeatRefused},
		{"a person who leads it does", personLeading("cto", worklet),
			authz.ActionProjectArchive,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"},
			true, authz.ReasonLead},

		// --- authored --------------------------------------------------- //
		{"the author edits their comment", person("jane.doe", doclet),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			true, authz.ReasonAuthor},
		{"somebody else does not", person("sam.rey", doclet),
			authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			false, authz.ReasonNotAuthor},
		{"nor does the author who no longer uses Doclet",
			person("jane.doe"), authz.ActionPageCommentEdit,
			authz.Object{Kind: authz.KindPage, Author: "jane.doe"},
			false, authz.ReasonNoGrant},
		// A WORK ITEM'S REMARK IS DECIDED BY THE SAME CLASS, and the seat a
		// person is bound to is their authorship there: a comment a bound
		// person wrote is recorded under their SEAT handle.
		{"a bound person edits the remark their seat wrote",
			personLeading("sre", worklet), authz.ActionWorkCommentEdit,
			authz.Object{Kind: authz.KindTask, Author: "sre"},
			true, authz.ReasonAuthor},
		{"and a colleague does not", personLeading("cto", worklet),
			authz.ActionWorkCommentEdit,
			authz.Object{Kind: authz.KindTask, Author: "sre"},
			false, authz.ReasonNotAuthor},

		// --- a place on a board ----------------------------------------- //
		{"Worklet moves a card", person("jane.doe", worklet),
			authz.ActionWorkPlace, authz.Object{Kind: authz.KindTask},
			true, authz.ReasonGrant},
		{"Doclet alone does not", person("jane.doe", doclet),
			authz.ActionWorkPlace, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonNoGrant},
		// AND NEVER A SEAT, however it is granted: a board is a person's
		// arrangement.
		{"a seat does not rearrange a board", seat("sre", worklet),
			authz.ActionWorkPlace, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonSeatRefused},

		// --- whether a seat works ---------------------------------------- //
		// ITS LEAD PAUSES IT, and nobody who merely uses the company's
		// apps: a rule that asked for one would let any colleague stop any
		// seat. No app: a lead who uses none still runs their team.
		{"a lead pauses their report", personLeading("cto"),
			authz.ActionSeatPause, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},
		{"a colleague holding every app does not", person("jane.doe", iam.Apps...),
			authz.ActionSeatPause, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"admin resumes any seat", person("jane.doe", admin),
			authz.ActionSeatResume, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonGrant},
		{"a seat resumes nobody, itself included", seat("sre", admin),
			authz.ActionSeatResume, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonSeatRefused},
		{"a lead steers their report's turn", personLeading("cto"),
			authz.ActionTurnSteer, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},
		{"and a colleague does not", personLeading("sam"),
			authz.ActionTurnSteer, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},
		// THE REQUESTER ANSWERS A RUN THROUGH THE SELF ARM: the tool asks
		// about the requester's own seat when that is who is calling.
		{"the requester answers the run they asked for", personLeading("sam"),
			authz.ActionRunAnswer, authz.Object{Kind: authz.KindPerson, Owner: "sam"},
			true, authz.ReasonSelf},
		{"the run's seat's lead answers it", personLeading("cto"),
			authz.ActionRunAnswer, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},

		// --- one seat's trail ---------------------------------------------- //
		{"a lead reads their agent's trail", personLeading("cto"),
			authz.ActionSeatTrailRead, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonLead},
		{"a colleague holding every app does not",
			personLeading("sam", iam.Apps...), authz.ActionSeatTrailRead,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			false, authz.ReasonNotSelf},
		{"admin reads any seat's", person("jane.doe", admin),
			authz.ActionSeatTrailRead, authz.Object{Kind: authz.KindPerson, Owner: "sre"},
			true, authz.ReasonGrant},
		// AND THE COMPANY-WIDE TRAIL IS ADMIN'S, whoever leads whom.
		{"a lead does not read the whole company's trail", personLeading("cto"),
			authz.ActionAuditRead, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},

		// --- running the company ----------------------------------------- //
		{"admin patches the company", person("jane.doe", admin),
			authz.ActionConfigWrite, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonGrant},
		{"every app together does not", person("jane.doe", iam.Apps...),
			authz.ActionConfigWrite, authz.Object{Kind: authz.KindCompany},
			false, authz.ReasonNoGrant},
		{"admin reveals a credential at a keyboard", person("jane.doe", admin),
			authz.ActionSecretReveal, authz.Object{Kind: authz.KindCompany},
			true, authz.ReasonGrant},
		{"a tool skill page is admin's, not Doclet's",
			person("jane.doe", doclet), authz.ActionSkillPageWrite,
			authz.Object{Kind: authz.KindPage}, false, authz.ReasonNoGrant},

		// --- shared ----------------------------------------------------- //
		//
		// AN ARTIFACT IS ITS SHARES': the SRE sits in the `sre` unit, so a
		// unit share admits them at its role, and nothing else does.
		{"a unit share admits who sits in the unit", personLeading("sre"),
			authz.ActionArtifactRead, authz.Object{Kind: authz.KindArtifact, Owner: "jane",
				Shares: []artifacts.Share{{Audience: artifacts.Audience{
					Kind: artifacts.AudienceUnit, ID: "sre"}, Role: artifacts.RoleViewer}}},
			true, authz.ReasonShared},
		{"a lead of the unit who sits elsewhere is not in it", personLeading("cto"),
			authz.ActionArtifactRead, authz.Object{Kind: authz.KindArtifact, Owner: "jane",
				Shares: []artifacts.Share{{Audience: artifacts.Audience{
					Kind: artifacts.AudienceUnit, ID: "sre"}, Role: artifacts.RoleViewer}}},
			false, authz.ReasonNotShared},

		// --- stage --------------------------------------------------------- //
		{"an invited principal may do nothing yet",
			iam.Principal{ID: uuid.New(), Login: "new.hire", Kind: iam.KindPerson,
				Stage: iam.StageInvited, Grants: []iam.Grant{admin}},
			authz.ActionWorkList, authz.Object{Kind: authz.KindTask},
			false, authz.ReasonStage},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), c.p, c.action, c.object, nimbus(), decidedAt)
			if d.Unknown() {
				t.Fatalf("decided UNKNOWN: %v", d.Err)
			}
			if d.Allowed != c.allowed {
				t.Errorf("allowed = %v, want %v (reason %q)",
					d.Allowed, c.allowed, d.Reason)
			}
			if d.Reason != c.reason {
				t.Errorf("reason = %q, want %q", d.Reason, c.reason)
			}
		})
	}
}

// AND FLIPPING THE CHART INVERTS EVERY ANSWER THAT TURNS ON IT.
//
// The control for the table above. Without it every lead case could be
// passing because the rule ignores the chart entirely and answers from the
// permission, which is exactly the collapse that made `Lead` false for every
// operator in the surface this replaces.
func TestFlippingTheChartInvertsEveryLeadAnswer(t *testing.T) {
	t.Parallel()
	inverted := chart{
		leads:    map[[2]string]bool{{"sre", "cto"}: true},
		projects: map[[2]string]bool{{"sre", "PLATFORM"}: true},
	}
	for _, c := range []struct {
		name   string
		actor  string
		action authz.Action
		object authz.Object
	}{
		{"priorities", "cto", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", "cto", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", "cto", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
		{"a file's removal", "cto", authz.ActionFileRemove,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"a seat's trail", "cto", authz.ActionSeatTrailRead,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			actor := seat(c.actor, iam.GrantWorklet)
			was := authz.Decide(t.Context(), actor, c.action, c.object, nimbus(), decidedAt)
			is := authz.Decide(t.Context(), actor, c.action, c.object, inverted, decidedAt)
			if !was.Allowed {
				t.Fatalf("the fixture does not allow this to begin with: %q", was.Reason)
			}
			if is.Allowed {
				t.Errorf("still allowed with the lead relation reversed, so the "+
					"rule is not reading the chart at all (reason %q)", is.Reason)
			}
		})
	}
}

// A CHART THAT COULD NOT ANSWER IS UNKNOWN, NEVER A REFUSAL.
//
// The seam this replaces opened with `if c == nil || c.Org == nil { return
// false }`, so a node that was booting, applying a revision or simply behind
// told every lead in the company that they lead nobody. It reads as a
// permissions bug and the node reports itself healthy throughout. An error
// here has to reach the caller as its own outcome, or the surface answers 403
// and sends somebody to ask for an authority they already hold.
func TestAChartReadErrorIsUnknownNotARefusal(t *testing.T) {
	t.Parallel()
	behind := errors.New("this node has not applied the running company")
	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
	}{
		{"priorities", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"a tidy", authz.ActionPageTrash,
			authz.Object{Kind: authz.KindPage, Container: "RUNBOOKS"}},
		{"a unit room's archive", authz.ActionChatArchive,
			authz.Object{Kind: authz.KindChannel, Container: "sre", Author: "sam"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// A PERSON BOUND TO THEIR SEAT, holding every app: a seat would
			// be refused the human-only rows before any chart is read.
			d := authz.Decide(t.Context(), personLeading("cto", iam.Apps...), c.action,
				c.object, chart{err: behind}, decidedAt)
			if !d.Unknown() {
				t.Fatalf("a chart that could not answer decided %v (%q), which "+
					"a surface renders as a refusal", d.Allowed, d.Reason)
			}
			if !errors.Is(d.Err, behind) {
				t.Errorf("err = %v, want the chart's own", d.Err)
			}
			if d.Allowed {
				t.Error("Allowed is true beside an error, which a caller " +
					"reading it first would honour")
			}
		})
	}
}

// AND THE ADMIN PATH DOES NOT NEED THE CHART.
//
// The control for the case above, and the reason admin is checked first: an
// administrator must not be told "I cannot tell" by a node that is merely
// lagging. Without this, closing the hole above would have opened a worse one
// — every administrator locked out whenever the chart is unavailable.
func TestTheAdminPathDecidesWithNoChartAtAll(t *testing.T) {
	t.Parallel()
	admin := person("jane.doe", iam.GrantAdmin)
	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
	}{
		{"priorities", authz.ActionPrioritiesSet,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}},
		{"project policy", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}},
		{"removal", authz.ActionWorkRemove,
			authz.Object{Kind: authz.KindTask, Container: "PLATFORM"}},
		{"the chart", authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit, Container: "sre"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), admin, c.action, c.object, authz.NoChart{}, decidedAt)
			if d.Unknown() {
				t.Fatalf("the admin path asked a chart it does not need: %v", d.Err)
			}
			if !d.Allowed || d.Reason != authz.ReasonGrant {
				t.Errorf("allowed = %v (%q), want admin to decide",
					d.Allowed, d.Reason)
			}
		})
	}
}

// THE SUBTREE'S ADMIN PATH IS ADMIN, AND IT NEEDS NO CHART.
//
// `Decision.Grants` is what a person refused reads as the remedy, so a lead
// refused a unit they do not lead is told admin — for a read as for a write —
// and never an app, which would change nothing. And the admin path is decided
// before the relation, so a node that cannot read its org never tells an
// administrator that it cannot tell, while a lead on the same node is told
// exactly that.
//
// Mutation: name the row's app on the refusal and every grant assertion here
// goes red; ask the chart before admin and the administrator's decision turns
// unknown.
func TestTheSubtreesAdminPathIsAdmin(t *testing.T) {
	t.Parallel()
	object := authz.Object{Kind: authz.KindUnit, Container: "elsewhere"}
	want := []iam.Grant{iam.GrantAdmin}
	for _, action := range []authz.Action{authz.ActionOrgWrite, authz.ActionOrgRead} {
		refused := authz.Decide(t.Context(), personLeading("cto", iam.Apps...), action,
			object, nimbus(), decidedAt)
		if refused.Allowed || !slices.Equal(refused.Grants, want) {
			t.Errorf("%s: a lead refused a unit they do not lead: allowed %v "+
				"naming %v, want a refusal naming %v", action, refused.Allowed,
				refused.Grants, want)
		}
		admitted := authz.Decide(t.Context(), person("jane.doe", iam.GrantAdmin), action,
			object, authz.NoChart{}, decidedAt)
		if admitted.Unknown() || !admitted.Allowed {
			t.Errorf("%s: admin on a node with no chart: allowed %v, err %v — the "+
				"admin path needs no chart", action, admitted.Allowed, admitted.Err)
		}
		blind := authz.Decide(t.Context(), personLeading("cto"), action, object,
			chart{err: authz.ErrNoChart}, decidedAt)
		if !blind.Unknown() {
			t.Errorf("%s: a lead on a node that cannot read its chart decided "+
				"%+v, want unknown", action, blind)
		}
	}
}

// A VERB WITH NO RULE IS REFUSED, AND SAYS IT IS THIS BUILD'S MISTAKE.
//
// The alternative is a default class, which is how a new verb ships ungated —
// and ships looking correct. The completeness walk keeps it out of
// production; this is what happens if it ever gets there.
func TestAVerbWithNoRuleIsRefused(t *testing.T) {
	t.Parallel()
	d := authz.Decide(t.Context(), person("jane.doe", iam.GrantAdmin),
		authz.Action("delete_the_company"), authz.Object{Kind: authz.KindCompany},
		nimbus(), decidedAt)
	if d.Allowed {
		t.Fatal("a verb this build has no rule for was allowed")
	}
	if d.Reason != authz.ReasonUnknownAction {
		t.Errorf("reason = %q, want the unknown-action refusal", d.Reason)
	}
	// AND IT IS NOT UNKNOWN. An error here would make this "ask me again",
	// so a verb with no rule would answer 503 to a request that can never
	// succeed — on every retry, for ever.
	if d.Unknown() {
		t.Errorf("a verb with no rule decided UNKNOWN (%v), which a surface "+
			"renders as a retry rather than as a refusal", d.Err)
	}
}

// A REFUSAL NAMES THE SMALLEST PERMISSIONS THAT WOULD HAVE ADMITTED THE CALLER.
//
// [authz.Decision.Grants] is what a surface puts in front of a person refused
// ("this needs Worklet"), so it has to be the decision's own account of what
// it consulted, never a guess made beside it. Held here against the decision
// itself, over every verb, every kind of object and every permission: for each
// refusal of a principal carrying nothing, the permissions it names are
// precisely the MINIMAL ones that, carried alone, turn the same question into
// an admit on the permission — every one that does, less any whose [iam.Expand]
// already holds another that does (admin is never named beside an app it
// includes). Both directions, because each fails differently: naming a
// permission that would not have helped sends somebody to ask for authority
// that changes nothing, leaving one out tells them there is no way in when
// there is, and naming admin beside the app tells them to ask for running the
// company when using one app would do.
//
// TWO OBJECTS PER KIND, because several rules refuse an object missing its
// field BEFORE they consult any permission — no permission would change that
// answer, so it names none — and an object with every field filled is the one
// that reaches the relation and the admin path behind it. The chart answers
// every relation with a firm no, so a lead relation never admits anybody and
// never hides a permission.
func TestARefusalNamesExactlyTheGrantsThatWouldHaveAdmittedIt(t *testing.T) {
	t.Parallel()
	// BOUND AND UNBOUND, because a verb made as a seat refuses the second
	// before any permission is weighed — naming none, which this holds too —
	// and decided only unbound, every such row's own grants would go
	// unchecked.
	for _, holder := range []func(...iam.Grant) iam.Principal{
		func(g ...iam.Grant) iam.Principal { return person("jane.doe", g...) },
		func(g ...iam.Grant) iam.Principal { return personLeading("jane", g...) },
	} {
		for _, a := range authz.Actions() {
			for _, kind := range authz.ObjectKinds {
				for _, object := range []authz.Object{
					{Kind: kind},
					{Kind: kind, ID: "x", Owner: "somebody.else", Author: "somebody.else",
						Container: "ELSEWHERE", ContainerKind: authz.KindProject},
				} {
					refusalNamesItsCover(t, holder, a, object)
				}
			}
		}
	}
}

// refusalNamesItsCover is one case of the walk above: holder with nothing is
// decided, and a refusal must name exactly the minimal cover of the grants
// that, held alone, admit.
func refusalNamesItsCover(t *testing.T, holder func(...iam.Grant) iam.Principal,
	a authz.Action, object authz.Object) {

	t.Helper()
	nobody := holder()
	d := authz.Decide(t.Context(), nobody, a, object, chart{}, decidedAt)
	if d.Unknown() {
		t.Fatalf("%s on %+v was undecidable against a chart that answers: %v",
			a, object, d.Err)
	}
	if d.Allowed {
		return
	}
	var admits []iam.Grant
	for _, g := range iam.AllGrants {
		got := authz.Decide(t.Context(), holder(g), a, object, chart{}, decidedAt)
		if got.Allowed && got.Reason == authz.ReasonGrant {
			admits = append(admits, g)
		}
	}
	named := slices.Clone(d.Grants)
	minimal := minimalCover(admits)
	slices.Sort(named)
	slices.Sort(minimal)
	if !slices.Equal(named, minimal) {
		t.Errorf("%s (seat %q) on %+v refused (%s) naming %v; the smallest "+
			"permissions that would have admitted it alone are %v",
			a, nobody.Seat, object, d.Reason, named, minimal)
	}
}

// minimalCover is admits less every permission whose expansion already holds
// another member of admits: admin, wherever an app it includes admits too.
func minimalCover(admits []iam.Grant) []iam.Grant {
	var out []iam.Grant
	for _, g := range admits {
		covered := false
		for _, h := range admits {
			if h != g && slices.Contains(iam.Expand([]iam.Grant{g}), h) {
				covered = true
			}
		}
		if !covered {
			out = append(out, g)
		}
	}
	return out
}

// A NAME NOBODY HAS RESOLVED IS DECIDED BEFORE THE DIRECTORY IS ASKED, and a
// caller the directory's answer could never admit learns nothing from it.
//
// Asked after the lookup, a stranger naming a login nobody holds was refused
// on the name as typed while a held one this node could not resolve answered
// 503 carrying the seat it was bound to — so which logins exist, and whose
// seat each holds, was readable off the difference by anybody. Decided first:
// admin and the caller's own login admit as they would on any record; a class
// with no lead path, and a caller who leads nobody, are refused with EXACTLY
// the decision somebody's seat they do not lead gets, so the refusal cannot
// say whether a lookup ever happened; and a caller who leads somebody is told
// to resolve the name and decide again on the record.
func TestANameNobodyResolvedIsDecidedBeforeTheDirectoryIsAsked(t *testing.T) {
	t.Parallel()
	unresolved := func(login string) authz.Object {
		return authz.Object{Kind: authz.KindPerson, Owner: login, Unresolved: true}
	}
	resolvedTo := func(seat string) authz.Object {
		return authz.Object{Kind: authz.KindPerson, Owner: seat}
	}
	stranger := person("pat.nobody", iam.GrantWorklet)
	lead := personLeading("cto", iam.GrantWorklet)
	admin := person("ops.admin", iam.GrantAdmin)
	blind := chart{err: errors.New("the running org is not built yet")}

	for _, c := range []struct {
		name   string
		p      iam.Principal
		action authz.Action
		chart  chart
		want   string // allowed, refused, resolve, unknown
	}{
		{"admin, on a queue", admin, authz.ActionPersonRead, nimbus(), "allowed"},
		{"admin, on a mark", admin, authz.ActionInboxMark, nimbus(), "allowed"},
		{"the caller's own login", stranger, authz.ActionPersonRead, nimbus(), "allowed"},
		{"a stranger, on a queue", stranger, authz.ActionPersonRead, nimbus(), "refused"},
		{"a stranger, on a mark", stranger, authz.ActionInboxMark, nimbus(), "refused"},
		// A MARK HAS NO LEAD PATH, so leading somebody changes nothing
		// and the chart is never asked — not even a chart that cannot say.
		{"a lead, on a mark", lead, authz.ActionInboxMark, blind, "refused"},
		{"a lead, on a queue", lead, authz.ActionPersonRead, nimbus(), "resolve"},
		{"a stranger, on a chart that cannot say", stranger, authz.ActionPersonRead,
			blind, "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			login := "someone.else"
			if c.name == "the caller's own login" {
				login = c.p.Login
			}
			d := authz.Decide(t.Context(), c.p, c.action, unresolved(login),
				c.chart, decidedAt)
			if got := outcome(d); got != c.want {
				t.Fatalf("decided %s (%+v), want %s", got, d, c.want)
			}
			if c.want != "refused" {
				return
			}
			// THE SAME REFUSAL SOMEBODY'S SEAT THEY DO NOT LEAD GETS,
			// reason and grants alike: anything else is the refusal
			// saying whether the name was looked up.
			onSeat := authz.Decide(t.Context(), c.p, c.action, resolvedTo("sre-2"),
				nimbus(), decidedAt)
			if d.Reason != onSeat.Reason || !slices.Equal(d.Grants, onSeat.Grants) {
				t.Errorf("refused before the lookup as %s %v, and on a seat they "+
					"do not lead as %s %v — the two must be one answer",
					d.Reason, d.Grants, onSeat.Reason, onSeat.Grants)
			}
		})
	}
}

// A CALLER WITHOUT THE ROW'S APP IS REFUSED BEFORE ANYTHING IS READ.
//
// [authz.ErrUnresolved] tells a surface to look something up — a login in the
// directory, a row in the tracker — and decide again. A caller the looked-up
// answer could never admit, because they do not use the app the verb belongs
// to, must not be sent there: they are refused on the spot, naming the app,
// the same answer whatever the lookup would have said. The control is the same
// caller holding the app, who IS sent to resolve. Mutation: let withApp pass
// an unresolved decision through and every first assertion goes red.
func TestACallerWithoutTheAppIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
		app    iam.Grant
	}{
		{"a lead naming a report's queue", authz.ActionPersonRead,
			authz.Object{Kind: authz.KindPerson, Owner: "someone.else", Unresolved: true},
			iam.GrantWorklet},
		{"a page nobody has read", authz.ActionPageTrash,
			authz.Object{Kind: authz.KindPage, Unresolved: true}, iam.GrantDoclet},
		{"an item nobody has read", authz.ActionWorkMove,
			authz.Object{Kind: authz.KindTask, Unresolved: true}, iam.GrantWorklet},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			without := authz.Decide(t.Context(), personLeading("cto"), c.action,
				c.object, nimbus(), decidedAt)
			if without.Unknown() || without.Allowed ||
				!slices.Equal(without.Grants, []iam.Grant{c.app}) {
				t.Errorf("without the app: %+v, want refused naming [%s]", without, c.app)
			}
			with := authz.Decide(t.Context(), personLeading("cto", c.app), c.action,
				c.object, nimbus(), decidedAt)
			if !errors.Is(with.Err, authz.ErrUnresolved) {
				t.Errorf("with the app: %+v, want told to resolve", with)
			}
		})
	}
}

// READING AND KEEPING A DAY BOTH TAKE THE TRACKER.
//
// A person's notices name the company's tasks and their queue lists them, so
// their day IS the tracker's: reading it, marking it, pinning views over it and
// ordering it each take the app beside the relation, and somebody who does not
// use Worklet has no day there to keep. Without the app on the rows one
// question had two answers — the query surface asks the app of every work
// question it registers, so the queue a REST route refused would have been
// answered over the act transport. Mutation: drop the app from the mark's row
// and its case is admitted.
func TestReadingAndKeepingADayBothTakeWorklet(t *testing.T) {
	t.Parallel()
	own := authz.Object{Kind: authz.KindPerson, Owner: "erin.ng"}
	theirs := authz.Object{Kind: authz.KindPerson, Owner: "someone.else"}
	worklet := []iam.Grant{iam.GrantWorklet}
	for _, c := range []struct {
		name   string
		p      iam.Principal
		action authz.Action
		object authz.Object
		admit  bool
		grants []iam.Grant
	}{
		{"own inbox, holding nothing", person("erin.ng"), authz.ActionInboxRead, own,
			false, worklet},
		{"own queue, holding Doclet", person("erin.ng", iam.GrantDoclet),
			authz.ActionMyWork, own, false, worklet},
		{"own record, holding nothing", person("erin.ng"), authz.ActionPersonRead, own,
			false, worklet},
		{"marking own inbox, holding nothing", person("erin.ng"), authz.ActionInboxMark,
			own, false, worklet},
		{"pinning own views, holding nothing", person("erin.ng"), authz.ActionPinsSet,
			own, false, worklet},
		{"ordering own queue, holding nothing", person("erin.ng"),
			authz.ActionPrioritiesSet, own, false, worklet},
		// THE CONTROLS: the app opens every one of them, and admin opens
		// somebody else's through the apps it includes.
		{"own inbox, holding Worklet", person("erin.ng", iam.GrantWorklet),
			authz.ActionInboxRead, own, true, nil},
		{"marking own inbox, holding Worklet", person("erin.ng", iam.GrantWorklet),
			authz.ActionInboxMark, own, true, nil},
		{"admin on somebody else's", person("ops.admin", iam.GrantAdmin),
			authz.ActionInboxRead, theirs, true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := authz.Decide(t.Context(), c.p, c.action, c.object, nimbus(), decidedAt)
			if d.Unknown() || d.Allowed != c.admit {
				t.Fatalf("decided %+v, want admitted=%v", d, c.admit)
			}
			if !c.admit && !slices.Equal(d.Grants, c.grants) {
				t.Errorf("refused naming %v, want %v", d.Grants, c.grants)
			}
		})
	}
}

// outcome names a decision's arm for the cases that compare arms.
func outcome(d authz.Decision) string {
	switch {
	case errors.Is(d.Err, authz.ErrUnresolved):
		return "resolve"
	case d.Unknown():
		return "unknown"
	case d.Allowed:
		return "allowed"
	}
	return "refused"
}

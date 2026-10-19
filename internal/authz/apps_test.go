package authz_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY APP IS NAMED BY A ROW A PERSON CAN TAKE.
//
// An app no row names is a permission an administrator can tick on somebody's
// row that opens nothing at all, which reads to that person as a broken
// screen. Walked from [iam.Apps] rather than from the table, so an app added
// to the vocabulary fails here until a verb belongs to it — and a row
// that is SEAT-ONLY does not count, because no person ever takes it.
// Mutation: drop the knowledge app from every page row and this goes red.
func TestEveryAppIsNamedByARow(t *testing.T) {
	t.Parallel()
	named := map[iam.Grant]bool{}
	for _, a := range authz.Actions() {
		app, _ := authz.AppOf(a)
		seatOnly, _ := authz.SeatOnlyOf(a)
		if app == "" || seatOnly {
			continue
		}
		if !app.IsApp() {
			t.Errorf("%s belongs to %q, which is not one of %v", a, app, iam.Apps)
		}
		named[app] = true
	}
	for _, app := range iam.Apps {
		if !named[app] {
			t.Errorf("no verb a person can take belongs to %q, so holding it "+
				"opens nothing", app)
		}
	}
	if _, known := authz.AppOf("no.such.verb"); known {
		t.Error("AppOf knows a verb the table has no row for")
	}
}

// EVERY APP ROW ADMITS A HOLDER AND REFUSES A NON-HOLDER, NAMING THE APP.
//
// Walked over EVERY row that names an app, on an object and a chart that
// satisfy whatever relation the row asks — the caller's own record, a comment
// they wrote, a project and a page container they lead — so the app is the
// ONLY thing left to decide: the same principal is admitted holding it and
// refused without it, and the refusal names exactly the app (never admin,
// which would change the answer too but is not the smallest thing to ask for).
// That is what holds the app on a RELATION row, which the relation alone would
// otherwise answer. Mutation: return the relation's decision from withApp
// untouched and every relation row's second half goes red.
func TestEveryAppRowAdmitsAHolderAndRefusesANonHolder(t *testing.T) {
	t.Parallel()
	walked := 0
	for _, a := range authz.Actions() {
		app, _ := authz.AppOf(a)
		if app == "" {
			continue
		}
		walked++
		// THE CTO leads the PLATFORM project and the RUNBOOKS container in
		// the fixture, and the object names them as its owner and author
		// besides, so every relation class admits them.
		object := authz.Object{Kind: authz.KindProject, Owner: "cto", Author: "cto",
			Container: "PLATFORM", ContainerKind: authz.KindProject, Asked: ""}
		switch app {
		case iam.GrantDoclet:
			object = authz.Object{Kind: authz.KindPage, Owner: "cto", Author: "cto",
				Container: "RUNBOOKS"}
		case iam.GrantArtifacts:
			// AN ARTIFACT THE CTO OWNS, which holds every role.
			object = authz.Object{Kind: authz.KindArtifact, Owner: "cto"}
		}
		// A UNIT'S ROOM the CTO leads the unit of, and a message they said.
		if app == iam.GrantChatlet {
			object = authz.Object{Kind: authz.KindChannel, Owner: "cto", Author: "cto",
				Container: "sre"}
		}
		holder := authz.Decide(t.Context(), personLeading("cto", app), a, object,
			nimbus(), decidedAt)
		if holder.Unknown() || !holder.Allowed {
			t.Errorf("%s: a holder of %s with every relation decided %+v, want "+
				"admitted", a, app, holder)
		}
		without := authz.Decide(t.Context(), personLeading("cto"), a, object,
			nimbus(), decidedAt)
		if without.Unknown() || without.Allowed ||
			!slices.Equal(without.Grants, []iam.Grant{app}) {
			t.Errorf("%s: the same caller without %s decided %+v, want refused "+
				"naming exactly [%s]", a, app, without, app)
		}
	}
	if walked == 0 {
		t.Fatal("no row names an app, so this walk certifies nothing")
	}
}

// AN ANY-APP ROW NAMES ITS OWN SET OF APPS, AND NO OTHER ROW NAMES ONE.
//
// [authz.ClassAnyApp] admits on any ONE of its row's apps ([authz.AnyOf]) and
// on nothing else, so a holder of an app the gesture draws nothing from is
// refused, and the refusal names exactly the set. Every such row names at
// least two apps — one would be [authz.ClassApp] spelt another way — each a
// real app, none twice; and a row of any other class names none, since no
// other class reads it and a set there would be a decision nobody consults.
// Walked as a person bound to a seat, so a seated row is decided on its class.
//
// THE OUTSIDE-THE-SET HALF is the case the class used to get wrong, iterating
// [iam.Apps]: chat is an app no any-app row draws on, and that reading admitted
// a holder of chat alone to a search of the tracker and the knowledge base — a
// search of nothing they may read. The walk fails if no app falls outside
// every set, because the half is then vacuous and certifies nothing.
// Mutation: decide the class over iam.Apps again, and the chat holder goes
// red.
func TestAnAnyAppRowNamesItsOwnSetOfApps(t *testing.T) {
	t.Parallel()
	walked, outside := 0, 0
	for _, a := range authz.Actions() {
		class, _ := authz.ClassOf(a)
		set, known := authz.AnyOf(a)
		if !known {
			t.Fatalf("AnyOf does not know %s, which the table has a row for", a)
		}
		if class != authz.ClassAnyApp {
			if len(set) != 0 {
				t.Errorf("%s is decided by %s and names the any-app set %v, "+
					"which nothing reads", a, class, set)
			}
			continue
		}
		walked++
		if len(set) < 2 {
			t.Errorf("%s is any-app over %v: fewer than two apps is one app "+
				"spelt another way", a, set)
		}
		for i, app := range set {
			if !app.IsApp() || slices.Contains(set[:i], app) {
				t.Errorf("%s names %q in its set %v, which is not one more app",
					a, app, set)
			}
		}
		object := authz.Object{Kind: authz.KindCompany}
		nobody := authz.Decide(t.Context(), personLeading("sam"), a, object,
			nimbus(), decidedAt)
		if nobody.Allowed || nobody.Reason != authz.ReasonNoGrant ||
			!slices.Equal(nobody.Grants, set) {
			t.Errorf("%s: a caller holding nothing decided %+v, want refused "+
				"naming exactly %v", a, nobody, set)
		}
		for _, app := range iam.Apps {
			d := authz.Decide(t.Context(), personLeading("sam", app), a, object,
				nimbus(), decidedAt)
			want := slices.Contains(set, app)
			if !want {
				outside++
			}
			if d.Allowed != want {
				t.Errorf("%s: a caller holding %s alone decided %+v, want "+
					"admitted = %t (the set is %v)", a, app, d, want, set)
			}
		}
	}
	if walked == 0 {
		t.Fatal("no row is any-app, so this walk certifies nothing")
	}
	if outside == 0 {
		t.Fatal("every app is in every any-app row's set, so the half that " +
			"refuses an app outside it certifies nothing")
	}
	if _, known := authz.AnyOf("no.such.verb"); known {
		t.Error("AnyOf knows a verb the table has no row for")
	}
}

// THE ASKED ARM ADMITS ONLY THE PERSON ASKED, AND ONLY TO READ AND ANSWER.
//
// Somebody a colleague put a question to reads the item and answers it without
// the tracker. Everything about that is narrow, and each narrowing is a case:
// it is the person the ask NAMES (by the seat they hold, or the login they act
// under) and nobody else; an object nobody has read is "read it and decide
// again", never an admission; and it reaches exactly two verbs — walked over
// the whole table, no other row admits on being asked. The controls are a
// tracker holder, admitted whoever is asked, and the person asked on an item
// whose ask names somebody else. Mutation: admit on any non-empty Asked and
// the stranger is let in.
func TestTheAskedArmAdmitsOnlyThePersonAsked(t *testing.T) {
	t.Parallel()
	asked := personLeading("sre")
	item := func(asked string) authz.Object {
		return authz.Object{Kind: authz.KindTask, Container: "PLATFORM", Asked: asked}
	}
	for _, a := range []authz.Action{authz.ActionWorkRead, authz.ActionWorkComment} {
		for _, c := range []struct {
			name   string
			p      iam.Principal
			object authz.Object
			want   authz.Reason
			admit  bool
		}{
			{"the seat asked", asked, item("sre"), authz.ReasonAsked, true},
			{"the login asked", person("jane.doe"), item("jane.doe"),
				authz.ReasonAsked, true},
			{"somebody another ask names", asked, item("cto"), authz.ReasonNoGrant, false},
			{"nobody asked", asked, item(""), authz.ReasonNoGrant, false},
			{"a Worklet holder", person("ana.ops", iam.GrantWorklet), item("cto"),
				authz.ReasonGrant, true},
		} {
			d := authz.Decide(t.Context(), c.p, a, c.object, nimbus(), decidedAt)
			if d.Unknown() || d.Allowed != c.admit || d.Reason != c.want {
				t.Errorf("%s, %s: decided %+v, want admitted=%v for %s", a, c.name,
					d, c.admit, c.want)
			}
			if !c.admit && !slices.Equal(d.Grants, []iam.Grant{iam.GrantWorklet}) {
				t.Errorf("%s, %s: refused naming %v, want [worklet]", a, c.name, d.Grants)
			}
		}
		unread := authz.Decide(t.Context(), asked, a,
			authz.Object{Kind: authz.KindTask, Unresolved: true}, nimbus(), decidedAt)
		if !errors.Is(unread.Err, authz.ErrUnresolved) {
			t.Errorf("%s on an item nobody has read decided %+v, want told to "+
				"read it", a, unread)
		}
	}
	for _, a := range authz.Actions() {
		d := authz.Decide(t.Context(), asked, a, item("sre"), nimbus(), decidedAt)
		if d.Reason == authz.ReasonAsked && a != authz.ActionWorkRead &&
			a != authz.ActionWorkComment {
			t.Errorf("%s admitted the person asked on being asked; only reading "+
				"the item and answering on it may", a)
		}
	}
}

// AN AUTHOR TIDIES THEIR OWN WORK ONLY WITH ITS APP.
//
// A page's creator trashes, restores and renames it and an item's reporter
// removes and restores it, whoever leads its container — but the app still
// governs: somebody who no longer uses Doclet does not tidy pages
// in it. And the author arm is the AUTHOR's: a colleague tidies nobody else's
// work. Mutation: check the author arm after the container relation and the
// author outside the lead's project is refused.
func TestAnAuthorTidiesOnlyWithTheApp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		action authz.Action
		object authz.Object
		app    iam.Grant
	}{
		{authz.ActionPageTrash, authz.Object{Kind: authz.KindPage,
			Container: "RUNBOOKS", Author: "sre"}, iam.GrantDoclet},
		{authz.ActionPageRestore, authz.Object{Kind: authz.KindPage,
			Container: "RUNBOOKS", Author: "sre"}, iam.GrantDoclet},
		{authz.ActionPageRename, authz.Object{Kind: authz.KindPage,
			Container: "RUNBOOKS", Author: "sre"}, iam.GrantDoclet},
		{authz.ActionWorkRemove, authz.Object{Kind: authz.KindTask,
			Container: "PLATFORM", Author: "sre"}, iam.GrantWorklet},
		{authz.ActionWorkRestore, authz.Object{Kind: authz.KindTask,
			Container: "PLATFORM", Author: "sre"}, iam.GrantWorklet},
		// A ROOM'S CREATOR archives it, whoever leads its unit.
		{authz.ActionChatArchive, authz.Object{Kind: authz.KindChannel,
			Container: "sre", Author: "sre"}, iam.GrantChatlet},
	} {
		with := authz.Decide(t.Context(), personLeading("sre", c.app), c.action,
			c.object, nimbus(), decidedAt)
		if !with.Allowed || with.Reason != authz.ReasonAuthor {
			t.Errorf("%s: the author holding %s decided %+v, want admitted as "+
				"author", c.action, c.app, with)
		}
		without := authz.Decide(t.Context(), personLeading("sre"), c.action,
			c.object, nimbus(), decidedAt)
		if without.Allowed || !slices.Equal(without.Grants, []iam.Grant{c.app}) {
			t.Errorf("%s: the author without %s decided %+v, want refused naming it",
				c.action, c.app, without)
		}
		other := authz.Decide(t.Context(), personLeading("sam", c.app), c.action,
			c.object, nimbus(), decidedAt)
		if other.Allowed {
			t.Errorf("%s: a colleague tidied somebody else's work (%s)", c.action,
				other.Reason)
		}
	}
}

// AN UNRESOLVED TIDY ASKS FOR THE ROW.
//
// A surface that offers a verb before it has read anything — the operator's
// list of what a person may do — names no container and no author, and both
// decide a tidy. Answered as a refusal, every one of these verbs was missing
// from a project lead's list (a container nobody named refuses as unnamed for
// anybody but admin), so a lead's restore control was drawn disabled. Answered
// [authz.ErrUnresolved] for a caller holding the app — "read the row and ask
// again" — the verb is offered and decided on the row. A caller without the
// app is refused there and then, and admin is admitted. Walked over every
// TIDY, CONTAINER-REMOVAL and MOVE row. Mutation: drop the unresolved arm from
// the tidy class and its rows answer unnamed.
func TestAnUnresolvedTidyAsksForTheRow(t *testing.T) {
	t.Parallel()
	walked := 0
	for _, a := range authz.Actions() {
		class, _ := authz.ClassOf(a)
		if class != authz.ClassTidy && class != authz.ClassDestructive {
			continue
		}
		walked++
		app, _ := authz.AppOf(a)
		object := authz.Object{Kind: authz.KindTask, Unresolved: true}
		switch app {
		case iam.GrantDoclet:
			object.Kind = authz.KindPage
		case iam.GrantChatlet:
			object.Kind = authz.KindChannel
		}
		holder := authz.Decide(t.Context(), personLeading("cto", app), a, object,
			nimbus(), decidedAt)
		if !errors.Is(holder.Err, authz.ErrUnresolved) {
			t.Errorf("%s: a holder of %s on a row nobody has read decided %+v, "+
				"want told to read it", a, app, holder)
		}
		without := authz.Decide(t.Context(), personLeading("cto"), a, object,
			nimbus(), decidedAt)
		if without.Unknown() || without.Allowed {
			t.Errorf("%s: a caller without %s decided %+v, want refused", a, app, without)
		}
		// BOUND TO A SEAT, as every person is, so a row made as one is
		// decided on its class.
		admin := authz.Decide(t.Context(), personLeading("ops-admin", iam.GrantAdmin), a,
			object, nimbus(), decidedAt)
		if !admin.Allowed {
			t.Errorf("%s: admin decided %+v, want admitted", a, admin)
		}
	}
	if walked == 0 {
		t.Fatal("no tidy or destructive row exists, so this walk certifies nothing")
	}
}

// AN UNREAD REMARK IS ADMIN'S, OR READ AND DECIDED AGAIN.
//
// An authored verb asked before anybody has read the remark — a person's
// surface asking what to offer, a moderator about to take a message down in a
// room they are not in — names no author. Admin is admitted there and then:
// the admin path needs no author, and a moderator who had to read a private
// room's message to learn who wrote it could not delete it at all. A caller
// holding the app is told to read the remark and decide again on its author
// ([authz.ErrUnresolved]); one without it is refused naming the app before
// anything is read; and an author left empty on a remark somebody DID read is
// still unnamed. Walked over every AUTHORED row. Mutation: drop the unresolved
// arm and the holder is refused unnamed, so no person's surface offers an
// edit, and admin is refused a moderation it may make.
func TestAnUnreadRemarkIsAdminsOrReadFirst(t *testing.T) {
	t.Parallel()
	walked := 0
	for _, a := range authz.Actions() {
		if class, _ := authz.ClassOf(a); class != authz.ClassAuthored {
			continue
		}
		walked++
		app, _ := authz.AppOf(a)
		kind := authz.KindTask
		switch app {
		case iam.GrantDoclet:
			kind = authz.KindPage
		case iam.GrantChatlet:
			kind = authz.KindMessage
		}
		unread := authz.Object{Kind: kind, Unresolved: true}
		holder := authz.Decide(t.Context(), personLeading("sam", app), a, unread,
			nimbus(), decidedAt)
		if !errors.Is(holder.Err, authz.ErrUnresolved) {
			t.Errorf("%s: a holder of %s on a remark nobody has read decided %+v, "+
				"want told to read it", a, app, holder)
		}
		without := authz.Decide(t.Context(), personLeading("sam"), a, unread,
			nimbus(), decidedAt)
		if without.Unknown() || without.Allowed ||
			!slices.Equal(without.Grants, []iam.Grant{app}) {
			t.Errorf("%s: a caller without %s decided %+v, want refused naming it",
				a, app, without)
		}
		admin := authz.Decide(t.Context(), personLeading("ops-admin", iam.GrantAdmin), a,
			unread, nimbus(), decidedAt)
		if !admin.Allowed || admin.Reason != authz.ReasonGrant {
			t.Errorf("%s: admin on an unread remark decided %+v, want admitted", a, admin)
		}
		read := authz.Decide(t.Context(), personLeading("sam", app), a,
			authz.Object{Kind: kind}, nimbus(), decidedAt)
		if read.Allowed || read.Reason != authz.ReasonUnnamed {
			t.Errorf("%s: a remark read and naming no author decided %+v, want "+
				"unnamed", a, read)
		}
	}
	if walked == 0 {
		t.Fatal("no authored row exists, so this walk certifies nothing")
	}
}

// SHOWS FOLLOWS THE KIND'S APP, AND APPFOR NAMES ONLY APPS.
//
// The one filter every cross-app answer runs its rows through: a tracker row
// to a tracker holder, a page to a knowledge holder, a row of no app to
// everybody — admin seeing everything through the apps it includes. Held over
// every kind, so a kind added later is decided here rather than shown to
// everybody by default; and every app is some kind's, or no row of it could
// ever be filtered, except an app that gates AUTHORING a kind decided per
// object, which filters no row by design (its rows are [authz.Visible]'s, held
// by the walk below). Mutation: map a view to no app and the tracker-less
// reader is shown it.
func TestShowsFollowsTheKindsApp(t *testing.T) {
	t.Parallel()
	// THE ARTIFACTS APP GATES AUTHORING AND NEVER READING: an artifact and
	// a folder of the artifact tree are each shown by their shares, so no
	// kind belongs to it.
	authoringOnly := map[iam.Grant]bool{iam.GrantArtifacts: true}
	used := map[iam.Grant]bool{}
	for _, k := range authz.ObjectKinds {
		app := authz.AppFor(k)
		if app != "" && !app.IsApp() {
			t.Errorf("%s belongs to %q, which is not an app", k, app)
		}
		used[app] = true
		for _, p := range []iam.Principal{
			person("jane.doe"),
			person("jane.doe", iam.GrantWorklet),
			person("jane.doe", iam.GrantDoclet),
			person("jane.doe", iam.GrantArtifacts),
			person("jane.doe", iam.GrantChatlet),
			person("jane.doe", iam.GrantAdmin),
		} {
			want := k != authz.KindArtifact && k != authz.KindArtifactFolder &&
				(app == "" || p.Can(app))
			if got := authz.Shows(p, k); got != want {
				t.Errorf("Shows(%v, %s) = %v, want %v", p.Grants, k, got, want)
			}
		}
	}
	for _, app := range iam.Apps {
		if !used[app] && !authoringOnly[app] {
			t.Errorf("no kind of object belongs to %q, so no row of it is ever "+
				"filtered", app)
		}
	}
	for app := range authoringOnly {
		if used[app] {
			t.Errorf("%q gates authoring only, and a kind belongs to it, so a "+
				"row of that kind is shown to whoever holds it", app)
		}
	}
	for k, want := range map[authz.ObjectKind]iam.Grant{
		authz.KindTask: iam.GrantWorklet, authz.KindProject: iam.GrantWorklet,
		authz.KindView: iam.GrantWorklet, authz.KindPage: iam.GrantDoclet,
		authz.KindContainer: iam.GrantDoclet, authz.KindPerson: "",
		authz.KindUnit: "", authz.KindCompany: "", authz.KindArtifact: "",
		authz.KindChannel: iam.GrantChatlet, authz.KindMessage: iam.GrantChatlet,
	} {
		if got := authz.AppFor(k); got != want {
			t.Errorf("AppFor(%s) = %q, want %q", k, got, want)
		}
	}
}

// A CHART THAT CANNOT ANSWER CANNOT ADMIT A CALLER THE APP REFUSES.
//
// Every arm of a row that names an app needs the app — the owner's, a lead's
// and an author's beside their relation, and the admin path through
// [iam.Expand], which is decided before the chart — so for a caller holding
// no app the answer is no whatever the chart would have said. Answered
// unknown it was a 503 telling them to come back to a refusal, for as long as
// the node could not read its chart (applying a revision, or running none).
// Walked over EVERY row that names an app, on an object somebody else owns,
// wrote and leads, resolved and not; the one unknown a caller without the app
// may still be answered is the asked arm's "read the item", because being
// asked admits without the app. The named cases pin the refusal to exactly
// the app, and the CONTROL is the same calls from a caller holding it, for
// whom the chart's answer IS the decision and so stays unknown. Mutation:
// convert only an [authz.ErrUnresolved] in withApp, as it once did, and every
// relation row reading the chart goes red.
func TestAChartThatCannotAnswerCannotAdmitACallerWithoutTheApp(t *testing.T) {
	t.Parallel()
	down := chart{err: errors.New("the chart could not be read")}
	walked := 0
	for _, a := range authz.Actions() {
		app, _ := authz.AppOf(a)
		if app == "" {
			continue
		}
		class, _ := authz.ClassOf(a)
		for _, unresolved := range []bool{false, true} {
			object := authz.Object{Kind: authz.KindProject, Owner: "sre", Author: "sre",
				Container: "PLATFORM", ContainerKind: authz.KindProject, Unresolved: unresolved}
			if app == iam.GrantDoclet {
				object = authz.Object{Kind: authz.KindPage, Owner: "sre", Author: "sre",
					Container: "RUNBOOKS", Unresolved: unresolved}
			}
			if app == iam.GrantChatlet {
				object = authz.Object{Kind: authz.KindChannel, Owner: "sre", Author: "sre",
					Container: "sre", Unresolved: unresolved}
			}
			d := authz.Decide(t.Context(), personLeading("cto"), a, object, down, decidedAt)
			walked++
			if unresolved && class == authz.ClassAppOrAsked && errors.Is(d.Err, authz.ErrUnresolved) {
				continue
			}
			if d.Allowed || d.Unknown() {
				t.Errorf("%s (unresolved %v): a caller without %s on a chart that "+
					"cannot answer decided %+v, want refused", a, unresolved, app, d)
			}
		}
	}
	if walked == 0 {
		t.Fatal("no row names an app, so this walk certifies nothing")
	}

	for _, c := range []struct {
		name   string
		action authz.Action
		object authz.Object
		app    iam.Grant
	}{
		{"a report's record", authz.ActionPersonRead,
			authz.Object{Kind: authz.KindPerson, Owner: "sre"}, iam.GrantWorklet},
		{"a record nobody has resolved", authz.ActionPersonRead,
			authz.Object{Kind: authz.KindPerson, Owner: "sre.person", Unresolved: true},
			iam.GrantWorklet},
		{"a project's policy", authz.ActionProjectPolicy,
			authz.Object{Kind: authz.KindProject, Container: "PLATFORM",
				ContainerKind: authz.KindProject}, iam.GrantWorklet},
		{"a page in a container", authz.ActionPageTrash,
			authz.Object{Kind: authz.KindPage, Author: "sre", Container: "RUNBOOKS"},
			iam.GrantDoclet},
		{"a project's shared view", authz.ActionViewSave,
			authz.Object{Kind: authz.KindView, Container: "PLATFORM",
				ContainerKind: authz.KindProject}, iam.GrantWorklet},
		{"a unit's room", authz.ActionChatArchive,
			authz.Object{Kind: authz.KindChannel, Author: "sre", Container: "sre"},
			iam.GrantChatlet},
	} {
		without := authz.Decide(t.Context(), personLeading("cto"), c.action, c.object, down,
			decidedAt)
		if without.Unknown() || without.Allowed || without.Reason != authz.ReasonNoGrant ||
			!slices.Equal(without.Grants, []iam.Grant{c.app}) {
			t.Errorf("%s: a caller without %s decided %+v, want refused no_grant "+
				"naming exactly [%s]", c.name, c.app, without, c.app)
		}
		holding := authz.Decide(t.Context(), personLeading("cto", c.app), c.action, c.object,
			down, decidedAt)
		if !holding.Unknown() {
			t.Errorf("%s: a caller holding %s decided %+v on a chart that cannot "+
				"answer, want unknown — the chart is their decision", c.name, c.app, holding)
		}
	}
}

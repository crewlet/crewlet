package authz

import "github.com/crewlet/crewlet/internal/iam"

// AppFor is the app a kind of object belongs to: a task, a project and a saved
// view are the tracker's, a page and a page container the knowledge base's,
// and every other kind — a person, a unit, the company — belongs to no app.
//
// THE ONE PER-APP HELPER, beside [Shows]. A cross-app answer — the company
// feed, the decisions a person is waiting on, a page's links back to the work
// that cites it, a seat's current task — carries rows of more than one kind
// to readers who may use only one of the apps, and each of those surfaces
// asking "which app is this row?" for itself is how one of them comes to show
// a task to somebody without the tracker. Asked here, the answer is one
// function over the same kinds the authority table decides on.
func AppFor(k ObjectKind) iam.Grant {
	switch k {
	case KindTask, KindProject, KindView:
		return iam.GrantTracker
	case KindPage, KindContainer:
		return iam.GrantKnowledge
	}
	return ""
}

// Shows reports whether p may be shown a row of kind k in an answer that
// mixes apps: a row that belongs to no app is everybody's, and a row of an
// app is shown to whoever holds the app — an administrator through
// [iam.Expand].
//
// A FILTER, NEVER A REFUSAL. A cross-app answer drops the rows its reader may
// not see and answers the rest, because refusing the whole answer over one row
// would withhold the rows they may see — and refusing a filter they asked for
// by name would confirm what exists behind it.
func Shows(p iam.Principal, k ObjectKind) bool {
	app := AppFor(k)
	return app == "" || p.Can(app)
}

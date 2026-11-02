package authz

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// AppFor is the app a kind of object belongs to: a task, a project and a saved
// view are the tracker's, a page and a page container the knowledge base's, a
// chat room and a message the chat's, and every other kind (a person, a unit,
// the company, an artifact) belongs to no app.
//
// AN ARTIFACT BELONGS TO NO APP and is not everybody's either: it is decided
// per object, by its shares ([ClassShared]), so the `artifacts` app gates
// authoring one and never reading one. See [Shows] and [Visible].
//
// THE ONE PER-APP HELPER, beside [Shows]. A cross-app answer — the company
// feed, the decisions a person is waiting on, a page's links back to the work
// that cites it, a seat's current task — carries rows of more than one kind
// to readers who may use only one of the apps, and each of those surfaces
// asking "which app is this row?" for itself is how one of them comes to show
// a task to somebody without Worklet. Asked here, the answer is one
// function over the same kinds the authority table decides on.
func AppFor(k ObjectKind) iam.Grant {
	switch k {
	case KindTask, KindProject, KindView:
		return iam.GrantWorklet
	case KindPage, KindContainer:
		return iam.GrantDoclet
	case KindChannel, KindMessage:
		return iam.GrantChatlet
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
//
// A KIND DECIDED PER OBJECT IS NEVER SHOWN BY ITS KIND: an artifact belongs to
// no app, and "no app" here means everybody's, so a kind-level filter would
// show every private artifact to every reader. Its rows go through [Visible],
// which reads the row.
func Shows(p iam.Principal, k ObjectKind) bool {
	if perObject(k) {
		return false
	}
	app := AppFor(k)
	return app == "" || p.Can(app)
}

// Visible reports whether p may be shown object o in an answer that mixes
// kinds or lists many objects: [Shows] for every kind decided by its app, and
// for an artifact, whether reading it admits p on a RELATION.
//
// THE ADMIN PATH SHOWS NO ROW HERE. An administrator may open any artifact,
// and that opening is an override the engine records; a listing, a feed or a
// search that surfaced every private artifact by the same override would
// disclose titles and existence nobody recorded. So an artifact is shown to an
// administrator exactly as it is to anybody else: owned, shared with them, or
// led through the agent that owns it.
//
// THREE-VALUED, because an artifact's relation may ask the chart: an error is
// a row this node cannot place, which a list answers unknown for rather than
// silently dropping (a share the reader holds would vanish whenever the node
// was behind).
func Visible(ctx context.Context, p iam.Principal, o Object, chart Chart,
	now time.Time) (bool, error) {

	if !perObject(o.Kind) {
		return Shows(p, o.Kind), nil
	}
	d := Decide(ctx, p, ActionArtifactRead, o, chart, now)
	if d.Unknown() {
		return false, d.Err
	}
	return d.Allowed && !d.Elevated, nil
}

// perObject reports a kind decided per object rather than per app: an
// artifact, and a folder of the artifact tree.
func perObject(k ObjectKind) bool { return k == KindArtifact || k == KindArtifactFolder }

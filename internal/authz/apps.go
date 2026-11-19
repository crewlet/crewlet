package authz

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// AppFor is the app a kind of object belongs to: a task, a project and a saved
// view are the tracker's, a page and a page container the knowledge base's, a
// chat room and a message the chat's, a repository and a change Gitlet's, an
// atlas entry Atlas's, and every other kind belongs to no app.
//
// AN ARTIFACT BELONGS TO NO APP and is not everybody's either: it is decided
// per object, by its shares ([ClassShared]), so the `artifacts` app gates
// authoring one, never reading. See [Shows] and [Visible].
//
// THE ONE PER-APP HELPER, beside [Shows]: a surface answering "which app is
// this row?" itself is how one comes to show a task to somebody without Worklet.
func AppFor(k ObjectKind) iam.Grant {
	switch k {
	case KindTask, KindProject, KindView:
		return iam.GrantWorklet
	case KindPage, KindContainer:
		return iam.GrantDoclet
	case KindChannel, KindMessage:
		return iam.GrantChatlet
	case KindRepository, KindChange:
		return iam.GrantGitlet
	case KindAtlasEntry:
		return iam.GrantAtlas
	}
	return ""
}

// Shows reports whether p may be shown a row of kind k in an answer that
// mixes apps: a row that belongs to no app is everybody's, and a row of an
// app is shown to whoever holds the app — an administrator through
// [iam.Expand].
//
// A FILTER, NEVER A REFUSAL. A cross-app answer drops the rows its reader may
// not see and answers the rest: refusing all over one row withholds rows they
// may see, and refusing a filter asked for by name confirms what is behind it.
//
// A KIND DECIDED PER OBJECT IS NEVER SHOWN BY ITS KIND: "no app" here means
// everybody's, so a kind-level filter would show every private artifact to
// every reader. Those rows go through [Visible], which reads the row.
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
// THE ADMIN PATH SHOWS NO ROW HERE. Opening any artifact is an override the
// engine records; a listing surfacing every private artifact by it would
// disclose titles and existence nobody recorded. An administrator sees an
// artifact as anybody does: owned, shared, or led through its owning agent.
//
// THREE-VALUED, because a relation may ask the chart: an error is a row this
// node cannot place, which a list answers unknown for rather than silently
// dropping (a held share would vanish whenever the node was behind).
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

package authz_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// requester is the ceiling of a turn the principal p woke.
func requester(p iam.Principal) authz.Ceiling {
	return authz.Ceiling{Kind: authz.CeilingRequester, Principal: p}
}

// AN AGENT NEVER DISCLOSES TO, OR WRITES FOR, A REQUESTER WHO COULD NOT.
//
// The confused deputy, as a table. The agent `ops-bot` may read and fill in a
// board shared with it; Mallory, who may not open it, messages the agent. Every
// verb the agent takes in her turn is decided again for her: a read of the
// board she cannot open is refused ([authz.ReasonCeiling], naming nothing),
// and so is a write into it. A requester who may take the verb is no ceiling
// at all, and a turn nobody woke is bounded by the agent alone. Mutation: skip
// the requester's decision and Mallory reads the board.
func TestAnAgentNeverDisclosesToOrWritesForARequesterWhoCouldNot(t *testing.T) {
	t.Parallel()
	bot := seat("ops-bot", iam.Apps...)
	mallory := personLeading("mallory", iam.Apps...)
	viewer := personLeading("sre")
	board := artifact("jane", seatShare("ops-bot", artifacts.RoleContributor),
		seatShare("sre", artifacts.RoleContributor))
	for _, c := range []struct {
		name    string
		ceiling authz.Ceiling
		action  authz.Action
		admit   bool
	}{
		{"nobody woke it, reading", authz.Ceiling{Kind: authz.CeilingNone},
			authz.ActionArtifactRead, true},
		{"Mallory woke it, reading", requester(mallory), authz.ActionArtifactRead, false},
		{"Mallory woke it, reading the data", requester(mallory),
			authz.ActionArtifactDataRead, false},
		{"Mallory woke it, writing the data", requester(mallory),
			authz.ActionArtifactDataWrite, false},
		{"a colleague it is shared with woke it, reading", requester(viewer),
			authz.ActionArtifactRead, true},
		{"and writing the data", requester(viewer), authz.ActionArtifactDataWrite, true},
		{"a stranger woke it, reading", authz.Ceiling{Kind: authz.CeilingStranger},
			authz.ActionArtifactRead, false},
		{"a ceiling nobody filled in", authz.Ceiling{}, authz.ActionArtifactRead, false},
	} {
		d := authz.DecideWithin(t.Context(), bot, c.ceiling, c.action, board, nimbus(),
			decidedAt)
		if d.Unknown() || d.Allowed != c.admit {
			t.Errorf("%s: decided %+v, want admitted=%v", c.name, d, c.admit)
		}
		if !c.admit && (d.Reason != authz.ReasonCeiling || len(d.Grants) != 0) {
			t.Errorf("%s: refused %+v, want the ceiling naming nothing", c.name, d)
		}
	}
}

// ANYBODY WHO CAN MESSAGE AN AGENT STEERS ITS EDITS TO ITS OWN WORK, AND
// NOTHING ELSE OF IT.
//
// On the agent's own artifact, a requester who cannot open it may still have
// the agent publish a fix, rename it, add an asset, reshape or fill in its
// data, and create a new one, as they may ask an agent to change any other
// work it owns. They may NOT have it read the artifact back to them, nor
// share it with them, which is the same read one step removed. A stranger
// steers the same edits and reaches nothing else. On somebody else's
// artifact nothing is steered. Mutation: spare every verb on the agent's own
// artifact, or none, and this goes red.
func TestAnAgentsOwnEditsAreSteeredAndItsReadsAreNot(t *testing.T) {
	t.Parallel()
	bot := seat("ops-bot", iam.Apps...)
	own := agentOwned("ops-bot")
	theirs := artifact("jane", seatShare("ops-bot", artifacts.RoleContributor))
	mallory := requester(personLeading("mallory"))
	stranger := authz.Ceiling{Kind: authz.CeilingStranger}
	for _, c := range []struct {
		name    string
		action  authz.Action
		object  authz.Object
		ceiling authz.Ceiling
		admit   bool
	}{
		{"publishing a fix", authz.ActionArtifactPublish, own, mallory, true},
		{"renaming it", authz.ActionArtifactUpdate, own, mallory, true},
		{"adding an asset", authz.ActionArtifactAsset, own, mallory, true},
		{"reshaping its data", authz.ActionArtifactSchema, own, mallory, true},
		{"filling in its data", authz.ActionArtifactDataWrite, own, mallory, true},
		{"creating one of its own", authz.ActionArtifactCreate,
			authz.Object{Kind: authz.KindArtifact, Owner: "ops-bot"}, mallory, true},
		{"a stranger steering an edit", authz.ActionArtifactPublish, own, stranger, true},
		{"reading it back", authz.ActionArtifactRead, own, mallory, false},
		{"reading its data back", authz.ActionArtifactDataQuery, own, mallory, false},
		{"sharing it", authz.ActionArtifactShare, own, mallory, false},
		{"unsharing it", authz.ActionArtifactUnshare, own, mallory, false},
		{"a stranger reading it", authz.ActionArtifactRead, own, stranger, false},
		{"writing somebody else's", authz.ActionArtifactDataWrite, theirs, mallory, false},
		{"a stranger writing somebody else's", authz.ActionArtifactDataWrite, theirs,
			stranger, false},
	} {
		d := authz.DecideWithin(t.Context(), bot, c.ceiling, c.action, c.object, nimbus(),
			decidedAt)
		if d.Unknown() || d.Allowed != c.admit {
			t.Errorf("%s: decided %+v, want admitted=%v", c.name, d, c.admit)
		}
	}
}

// THE AGENT'S OWN REFUSAL IS THE ANSWER, AND A REQUESTER NOBODY CAN DECIDE IS
// UNKNOWN.
//
// The ceiling only ever narrows: a verb the agent itself may not take is
// refused on the agent's own rule, whoever woke it, so the refusal names what
// the agent lacks. And a requester whose relation the chart cannot answer is
// "this node cannot tell", never a refusal. Mutation: decide the requester
// first and the agent's own refusal reads as the ceiling.
func TestTheCeilingOnlyNarrowsAndCannotTellIsNotNo(t *testing.T) {
	t.Parallel()
	bot := seat("ops-bot", iam.Apps...)
	lead := requester(personLeading("cto", iam.Apps...))
	d := authz.DecideWithin(t.Context(), bot, lead, authz.ActionArtifactTrash,
		agentOwned("ops-bot"), nimbus(), decidedAt)
	if d.Allowed || d.Reason != authz.ReasonSeatRefused {
		t.Errorf("an agent trashing its own artifact for its lead decided %+v, want "+
			"refused as a seat", d)
	}
	d = authz.DecideWithin(t.Context(), bot, lead, authz.ActionArtifactRead, artifact("jane"),
		nimbus(), decidedAt)
	if !d.NotFound() {
		t.Errorf("an agent reading what it holds no relation to decided %+v, want not "+
			"found on its own rule", d)
	}

	// THE SRE'S UNIT IS WHAT ADMITS THEM, and a chart that cannot answer
	// cannot say. The agent's own seat share needs no chart.
	board := artifact("jane", seatShare("ops-bot", artifacts.RoleViewer),
		unitShare("sre", artifacts.RoleViewer))
	behind := errors.New("behind")
	d = authz.DecideWithin(t.Context(), bot, requester(personLeading("sre")),
		authz.ActionArtifactRead, board, chart{err: behind}, decidedAt)
	if !errors.Is(d.Err, behind) {
		t.Errorf("a requester the chart cannot place decided %+v, want unknown", d)
	}
	if !authz.CeilingNone.Valid() || authz.CeilingKind("").Valid() {
		t.Error("the ceiling kinds do not validate as declared")
	}
}

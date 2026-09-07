// Package integration is the app-neutral half of keeping an external
// surface in line with the company.
//
// # Why this exists before any third-party app sits on it
//
// Six integration packages already know how to bring their surface into line:
// slack, mattermost, jira, github, gitlab and (from this change) datadog.
// What none of them had was a way to SAY WHERE THEY GOT TO. Each returned a
// bag of its own nouns (Created, Rotated, Kept, Pending, Failed, Notes), each
// was called once from its own CLI subcommand, and nothing anywhere turned
// one of those bags into "this integration is waiting on you to install an
// app". So the engine's own dashboard query says, in its doc comment, that it
// NEVER INFERS HEALTH: there was nothing to infer it from.
//
// This package is that missing half, and it is built the way internal/notify
// was built, before any third-party app sat on it. A spine grown after its
// first integration has that integration welded into it, and the five bespoke
// classifiers this replaces are what that looks like: written once per
// integration, they had already drifted on the question that matters most
// (see [Classify]).
//
// # The three things it owns
//
//   - The VOCABULARY a pass reports in: [Phase], [Actor], [Finding] and the
//     [Report] they fold into. One vocabulary for every surface, because a
//     founder asking "is my Slack working" and "is my GitLab working" is
//     asking one question, and answering it in two third-party apps' words makes them
//     compare two things that were never the same shape.
//   - The CADENCE the next pass runs on ([Schedule]), which is derived from
//     WHO has to act rather than from what failed. Work the engine or a
//     third-party app is doing comes back quickly; work a person owes is re-checked on
//     their schedule, not ours.
//   - The LOOP ([Worker]), a fleet singleton that runs each registered
//     reconciler when it is due and records what it found.
//
// # What it deliberately does not own
//
// TEARDOWN. The control plane this was ported from has a tenant who
// disconnects an integration, and a loop that then removes the identities it
// provisioned. An engine has no tenant: an operator who deletes the `gitlab:`
// block from the company document has said what the engine should stop
// talking to, NOT that fifteen service accounts and everything attributable
// to them should be destroyed. So a removed block makes this package forget
// its state and nothing else; decommissioning accounts stays an explicit
// gesture on the integration's own subcommand, where the operator types the flag
// and reads what it is about to delete.
package integration

import "slices"

// Kind names an external surface the engine converges.
//
// The values are the SAME STRINGS the dashboard's integrations query already
// keys its rows on (internal/api/queries/company.go), because the two answer
// halves of one question and a reader joining them by hand is how a row ends
// up carrying another third-party app's status.
//
// There is deliberately no kind for the Forge relay. It is a delivery path
// for Jira and Confluence Cloud rather than a surface of its own: nothing is
// provisioned into it, so a reconcile would have nothing to converge.
type Kind string

// The surfaces this build reconciles. These values are STORED, in the
// fleet's coordination store and on the wire, so a rename is a migration
// rather than an edit.
const (
	KindSlack      Kind = "slack"
	KindMattermost Kind = "mattermost"
	KindJira       Kind = "jira"
	KindConfluence Kind = "confluence"
	KindGitHub     Kind = "github"
	KindGitLab     Kind = "gitlab"
	KindDatadog    Kind = "datadog"

	// KindAtlassian is the ORGANIZATION, not a third product. Jira and
	// Confluence are sites this engine reads and writes as an account; this
	// is where the account itself is created, which no site API can do. It
	// reconciles identities and never ingests anything, so it has no
	// inbound route of its own.
	KindAtlassian Kind = "atlassian"
)

// Kinds is every surface this package knows, in the order an operator reads
// them: chat first, then the tracker and its wiki, then the code hosts, then
// observability. A stable order so a status listing does not reshuffle
// between two reads of the same fleet.
var Kinds = []Kind{
	KindSlack, KindMattermost, KindJira, KindConfluence,
	KindGitHub, KindGitLab, KindDatadog, KindAtlassian,
}

// Valid reports whether k is a surface this build knows.
//
// A value off the wire is a VALUE rather than a panic: the coordination store
// holds state written by whichever build ran the last pass, and a rolling
// upgrade puts a peer's newer kind in front of an older reader. An unknown
// kind is skipped by the worker and rendered as-is by the API, which is the
// only honest thing either can do with a surface it cannot converge.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// String makes a Kind printable without a conversion at every log site.
func (k Kind) String() string { return string(k) }

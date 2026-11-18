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
// # The four things it owns
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
//   - The DISCONNECT: [AskTeardown] records that somebody asked for a surface
//     to be taken away, [Worker.tearDown] drives the vendor's
//     [Disconnector] until it succeeds, and [ObserveTeardown] folds the
//     result. It is here rather than beside the reconcile because a
//     disconnecting row IS a row — the same record, read by the same screen,
//     with the same rule about what a phase means — and the transition that
//     was written inline at the API set three of its fields and forgot three.
//
// # The tool roll-up, the certification suite and the owed look
//
// [Rollup] is decided here, one state per tool, because a screen that
// re-derived it kept a second copy of the phase order and drifted.
// [integrationtest] is the certification suite: one set of cases every
// reconciler passes, because a promise about what a pass does to somebody's
// workspace that nothing checks is the worst shape a rule can take. A teardown
// is recorded under WHO ASKED ([State.RequestedBy]), since the loop finishes
// it minutes later on whichever node holds the duty; without it the revision
// that drops the block was recorded as the loop's own, not the person's who
// forced the disconnect. A forced look at a held surface stays owed
// ([Worker.visit]): an apply's sweep or a refresh that finds another writer
// holding the lease records nothing and re-arms the look for the next tick,
// because the ask was spent on the way there and dropping it left the surface
// on its settled cadence.
//
// # What a disconnect is, and what it is NOT
//
// It is a REQUEST, made once, by a person who pressed a button and answered a
// question about the accounts. That distinction is the whole of the design and
// this file used to state it as "teardown is deliberately not owned here",
// which had stopped being true and read as though an edit could destroy
// accounts.
//
// It cannot. The control plane this was ported from has a tenant who
// disconnects an integration, and a loop that then removes the identities it
// provisioned. An engine has no tenant: an operator who deletes the `gitlab:`
// block from the company document has said what the engine should stop
// talking to, NOT that fifteen service accounts and everything attributable
// to them should be destroyed. So a removed block still makes this package
// forget its state and nothing else — that path runs through
// [Store.ForgetIntegration] and touches no third-party app. Removing accounts
// needs [State.RemoveSeats], which only [AskTeardown] sets and only from a
// question somebody was asked; the integration's own subcommand is the other
// way, where the operator types the flag and reads what it is about to delete.
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

	// KindCloudflare provisions one account API token per agent and, in this
	// build, ingests nothing.
	KindCloudflare Kind = "cloudflare"

	// KindScaleway provisions one IAM application, policy and API key per
	// agent and, in this build, ingests nothing.
	KindScaleway Kind = "scaleway"

	// KindExpo provisions one robot user per agent and, in this build,
	// ingests nothing.
	KindExpo Kind = "expo"

	// KindDigitalOcean checks and keeps one personal access token per agent,
	// which a person creates and pastes, and ingests nothing.
	KindDigitalOcean Kind = "digitalocean"

	// KindAWS provisions one IAM role per agent and, in this build, ingests
	// nothing.
	KindAWS Kind = "aws"

	// KindGoogleCloud provisions one service account per agent and, in this
	// build, ingests nothing.
	KindGoogleCloud Kind = "google_cloud"

	// KindSSH signs short-lived certificates for agents to reach the
	// company's servers with and ingests nothing.
	KindSSH Kind = "ssh"

	// KindAtlassian is the ORGANIZATION, not a third product. Jira and
	// Confluence are sites this engine reads and writes as an account; this
	// is where the account itself is created, which no site API can do. It
	// reconciles identities and never ingests anything, so it has no
	// inbound route of its own.
	KindAtlassian Kind = "atlassian"
)

// Kinds is every surface this package knows, in the order an operator reads
// them: chat first, then the tracker and its wiki, then the code hosts, then
// observability, the edge, app delivery, the cloud and servers. A stable order
// so a status listing does not reshuffle between two reads of the same fleet.
var Kinds = []Kind{
	KindSlack, KindMattermost, KindJira, KindConfluence,
	KindGitHub, KindGitLab, KindDatadog, KindCloudflare, KindExpo, KindDigitalOcean,
	KindScaleway, KindAWS, KindGoogleCloud, KindSSH, KindAtlassian,
}

// ConvergeOrder is the order the reconcile loop VISITS surfaces in, which is
// not the order an operator reads them.
//
// ONE REAL DEPENDENCY: Atlassian is where an agent's service account is
// CREATED, and Jira and Confluence check for that account with the credential
// it minted. In reading order both products run before the pass that creates
// what they look for, and over a reconnect report `401 Action required` at
// the third-party app until Atlassian's own pass has run.
//
// A separate value rather than a reordering of [Kinds], because the orders
// answer different questions and would drift. A test pins them to the same
// SET, so a surface added to one and forgotten in the other is caught.
var ConvergeOrder = []Kind{
	// THE ACCOUNTS FIRST, then the products that authenticate as them.
	KindAtlassian, KindJira, KindConfluence,
	KindSlack, KindMattermost,
	KindGitHub, KindGitLab, KindDatadog, KindCloudflare, KindExpo, KindDigitalOcean,
	KindScaleway, KindAWS, KindGoogleCloud, KindSSH,
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

// Ingress says WHO KEEPS a surface's inbound address pointing at this
// deployment.
//
// A company's public base URL moves, and what happens next depends on who
// wrote the address down at the third-party app. That is what makes
// [State.Endpoint] mean anything: on a surface the engine re-registers it
// repairs itself within a tick; on one a person typed into a settings page it
// is the only warning that deliveries now go nowhere.
//
// Getting it wrong is silent: an address only a person can move, stamped as
// though a pass had moved it, reports a healthy integration while every
// delivery lands where nothing answers.
type Ingress string

const (
	// IngressNone is a surface with no inbound address at all: it is
	// reached over a connection this engine opens, or it ingests nothing.
	// A base URL change cannot break it, so a moved address is not a
	// finding and no address is recorded.
	IngressNone Ingress = "none"

	// IngressEngine is a surface whose hook a pass registers and
	// re-registers. A moved base is converged on the next tick, and the
	// address recorded on each pass is where that registration points.
	IngressEngine Ingress = "engine"

	// IngressOperator is a surface whose address a PERSON pasted into the
	// third-party app's own settings, because the third-party app offers no
	// way for this engine to write it. Nothing converges it: the recorded
	// address is whatever it was at the last setup, and when the base moves
	// away from it only the operator can put it right.
	IngressOperator Ingress = "operator"
)

// Ingress reports who maintains this surface's inbound address.
//
// GitHub, GitLab, Jira, Confluence and Datadog expose an API for registering
// their delivery address, which each pass uses. Slack's Request URL is a
// settings-page field with no write API, so it holds whatever a person last
// typed. Mattermost is dialled out over a websocket and Atlassian only
// provisions identities, so neither has an inbound address to go stale.
//
// DATADOG IS ENGINE-OWNED: its webhook definition is writable through the
// organization credential pair the block carries for identities, which config
// validation requires of an enabled block.
func (k Kind) Ingress() Ingress {
	switch k {
	case KindGitHub, KindGitLab, KindJira, KindConfluence, KindDatadog:
		return IngressEngine
	case KindSlack:
		return IngressOperator
	default:
		return IngressNone
	}
}

// Ingests reports whether anything ever arrives FROM this surface.
//
// NOT THE SAME QUESTION AS [Kind.Ingress], which answers None for two very
// different surfaces: Mattermost has no address because the engine DIALS OUT,
// then receives everything said in its team; Atlassian only CREATES accounts
// (Jira and Confluence have their own webhook and parser). So "no parser turns
// deliveries into work" warns about Mattermost and means nothing for Atlassian.
// Cloudflare, Expo, DigitalOcean, Scaleway, AWS, Google Cloud and SSH are
// likewise: credentials there are minted, pasted or issued, and nothing they
// send is routed to a seat.
func (k Kind) Ingests() bool {
	return k != KindAtlassian && k != KindCloudflare && k != KindExpo && k != KindDigitalOcean &&
		k != KindScaleway && k != KindAWS && k != KindGoogleCloud && k != KindSSH
}

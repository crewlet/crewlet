package datadog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Options is what one pass over the company's half of Datadog needs.
//
// THE COMPANY'S HALF ONLY: the organization credentials, the inbound webhook
// and the advisory about accounts nothing manages. Each agent's own account is
// the per-seat access sync's ([seataccess.Vendor], this package's
// accessvendor.go), which a setup pass runs beside this one.
type Options struct {
	// Client talks to the company's region.
	Client *Client

	// Config is the company's datadog block.
	Config *config.Datadog

	// Creds is the organization credential pair, resolved.
	Creds Credentials

	// ReadOnly is a check: the pass reads and reports, and writes nothing
	// at Datadog. A missing or misdirected webhook is reported rather than
	// registered, which is the fact a check exists to surface.
	ReadOnly bool

	// WebhookBase is where Datadog reaches this deployment
	// ([config.API.WebhookBase]: the public listener's address where
	// `api.public` is set), or empty to skip registering the inbound
	// webhook at Datadog.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), which a pass with no address names as
	// the setting to fill in. Empty is [setup.BaseKey]'s.
	WebhookBaseKey string

	// WebhookToken is the resolved shared token the definition carries and
	// the route checks. Empty skips registration for the same reason an
	// empty base does: a definition that cannot deliver is worse than none.
	WebhookToken string

	// Managed are the agent accounts the per-seat access sync manages, each
	// a Datadog user id the access ledger records or the address a document
	// seat's account is created under ([AccountEmail]).
	//
	// THE ORPHAN ADVISORY NEVER REPORTS THEM. This pass manages no agent's
	// account, so it cannot know on its own which accounts are; judged
	// against nothing, it would call every agent's live account an orphan.
	// What is managed is what the ledger and the document say, so that is
	// what is passed here.
	Managed []string
}

// Result is what one pass found.
type Result struct {
	// Org is the organization the credentials authenticate as, which is
	// the one thing an operator can check to see they connected the
	// account they meant to.
	Org Org

	// Webhook is what this pass did to the inbound definition at Datadog,
	// nil where the pass never reached it.
	Webhook *WebhookResult

	// UsersURL is this organization's user administration at Datadog, or ""
	// when the pass could not name one: where a person settles what only
	// Datadog can.
	UsersURL string

	// Orphaned are the enabled service accounts at this company's own email
	// domain that nothing manages ([Options.Managed]).
	//
	// REPORTED AND NEVER TOUCHED, which is the whole of it. A live
	// organization accumulates these: an account somebody made by hand at
	// this engine's `.invalid` domain, or one an engine made before it kept
	// a record of its accounts. Every one is an identity somebody has to
	// decide about, and nothing else names them: no seat's row mentions
	// them, and the card read Ready over an organization full of them.
	//
	// Not removed, for the reason the disconnect gives at length: an account
	// is a colleague at that app with history attached, and deleting one
	// because a handle changed is not a decision a timer makes. It is an
	// ADVISORY — [integration.FindingRegistrationOrphaned] is phase-ready,
	// owed to an admin — so it says so without holding the surface out of
	// Ready.
	Orphaned []User

	// Notes are the caveats, such as a webhook this pass could not register.
	Notes []string
}

// appKeyName is what every application key this engine mints is called.
//
// IT IS THE ONLY THING THAT ATTRIBUTES ONE. Datadog serves a key's value
// exactly once and its listing carries an id and a name, so the name is all a
// later pass has to tell a key this engine wrote from one somebody else put on
// the account: whether the account still holds the seat's credential, which
// keys a rotation retires, and which a mint for a seat holding nothing
// replaces. It is a constant rather than a literal at the mint because a
// reader matching a different string than the mint wrote would find nothing,
// for ever, with no symptom but a seat that stays stuck.
const appKeyName = "crewlet"

// Reconcile brings the company's half of Datadog in line: it verifies the
// organization credentials, keeps the inbound webhook registered, and reports
// accounts at this engine's own domain that nothing manages.
//
// It CREATES, CHANGES AND DISABLES NO AGENT'S ACCOUNT: that is the per-seat
// access sync's, which reads every account back on each pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS OBSERVED NOTHING, AND IT SAYS SO HERE RATHER THAN BY
	// HAPPENING TO MAKE A NETWORK CALL FIRST.
	//
	// [integration.Reconciler]'s contract — and the clause
	// integrationtest drives for it — is that a cancelled pass raises
	// rather than answering with findings, because an error is a fault the
	// loop retries and an empty findings list is a statement that
	// everything is fine. This pass satisfied that only INCIDENTALLY: the
	// credential probe below is a network call, so a dead context failed
	// it. That left the clause resting on two things it should not rest
	// on. The first is the transport honouring cancellation, which is a
	// property of whatever [Client] was built with rather than of this
	// package. The second is worse and is reachable today: the arm above
	// the probe returns [integration.ErrNotConfigured], which the loop
	// reads as "forget this surface's status row" — so a node draining
	// during shutdown, against a company mid-edit, would delete the
	// fleet's Datadog status on its way out.
	//
	// And when the probe DID answer for it, it answered wrongly. The
	// sentence it produces names the organization credential pair and says
	// it "could not be verified", so a pass cancelled by a node shutting
	// down sent an operator to look at a key that was never asked about.
	// [integration.Refusal]'s own doc is about exactly that class of
	// mistake.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("datadog: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return nil, integration.ErrNotConfigured
	}
	if opts.Creds.APIKey == "" || opts.Creds.AppKey == "" {
		// A FINDING'S WORTH OF FACT, reported as an error because the
		// pass observed nothing: both keys are needed together, and
		// Datadog's refusal for a missing one names neither.
		return nil, errors.New(
			"datadog: both the API key and the application key are needed; " +
				"Datadog refuses a write carrying only one and its message " +
				"names neither")
	}

	org, err := opts.Client.VerifyCredentials(ctx, opts.Creds)
	if err != nil {
		// THE VERB FOLLOWS THE CLASSIFICATION. Saying refused whatever
		// happened turns this build's own mistake into an accusation
		// against the operator's key: see [integration.Refusal].
		rejected := integration.Reject(err, Status(err))
		return nil, fmt.Errorf(
			"datadog: the organization credential pair in "+
				"integrations.datadog.provisioning %s, so nothing "+
				"else this run reports would be trustworthy: %w",
			integration.Refusal(rejected), rejected)
	}
	res := &Result{Org: org, UsersURL: opts.Client.UsersURL()}

	// THE INBOUND HALF FIRST, and before the early return below. A company
	// with no seats to give identities to still wants its alerts to
	// arrive, and the webhook is the only thing that makes them.
	hook := ensureWebhook(ctx, opts)
	res.Webhook = &hook
	if hook.Blocked != nil {
		res.Notes = append(res.Notes, hook.Blocked.Detail)
	}

	// AND WHAT THE ORGANIZATION HOLDS THAT NOTHING MANAGES. Read off one
	// listing at this company's own domain.
	existing, err := opts.Client.ListServiceAccounts(ctx, opts.Creds, emailDomainOf(opts.Config))
	if err != nil {
		return nil, fmt.Errorf("datadog: list the service accounts: %w",
			integration.Reject(err, Status(err)))
	}
	res.Orphaned = orphanedAccounts(existing, opts.Managed, emailDomainOf(opts.Config))
	return res, nil
}

// orphanedAccounts is every service account at this company's own domain that
// no seat in the plan claims.
//
// # Which accounts count as this engine's
//
// The domain is the first half and it is not enough on its own: a company may
// point `email_domain` at a real domain it owns, where its own people have
// addresses. So a `.invalid` domain — the default, and reserved by RFC 2606
// precisely because nothing can deliver to it — is taken as conclusive, since
// no person has a mailbox there and a service account at one was made by a
// provisioner. Anywhere else the `crewlet-` prefix is the marker.
//
// THE PREFIX THIS ENGINE NO LONGER WRITES is deliberately not listed. The
// accounts a live organization actually holds came from whatever scheme was
// current when they were made, and enumerating past ones would be a list that
// grows for ever and is wrong the moment somebody edits `email_domain`. The
// `.invalid` clause catches every one of them without naming any.
//
// An account is claimed by its id where the access ledger records one, and by
// the address a document seat's account is created under otherwise: a seat
// whose account the sync has not adopted yet is still a seat somebody is
// managing, and reporting its account as orphaned would tell an operator to
// clean up the identity of a working agent. See [Options.Managed].
//
// # A disabled one is already where the advisory is asking it to get to
//
// The remedy this reports under is "disable or delete the ones you do not
// want", so an account that is ALREADY DISABLED has reached the end state and
// reporting it asks for work somebody has done. Worse, it was mostly this
// engine's own: a `remove_seats` disconnect DISABLES the accounts it removes,
// so the ordinary reconnect-with-a-smaller-roster cycle turned every correct
// teardown into a row on the card — measured on one organization, 36 rows,
// all of them inert.
//
// Skipping them also makes the advisory's own instruction work. Before this,
// an operator who read the note and disabled an account watched the finding
// come back unchanged on the next tick, because the only state the filter
// looked at was the address; the only thing that cleared it was a delete. Now
// either half of "disable or delete" does.
//
// THE ENABLED ONES ARE THE WHOLE FINDING and they still report. That is the
// account that can still act — a changed handle's old identity, live,
// holding whatever it held, matching nothing any pass will ever ask for
// again.
func orphanedAccounts(existing []User, managed []string, domain string) []User {
	claimed := make(map[string]bool, len(managed))
	for _, account := range managed {
		claimed[strings.ToLower(strings.TrimSpace(account))] = true
	}
	var out []User
	for _, user := range existing {
		if user.Disabled {
			continue
		}
		if claimed[strings.ToLower(strings.TrimSpace(user.Email))] ||
			claimed[strings.ToLower(strings.TrimSpace(user.ID))] {
			continue
		}
		if !madeHere(user.Email, domain) {
			continue
		}
		out = append(out, user)
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.Email, b.Email) })
	return out
}

// madeHere reports an address only a provisioner of this company could have
// created. See [orphanedAccounts].
func madeHere(email, domain string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	local, host := email[:at], strings.TrimSpace(email[at+1:])
	if !strings.EqualFold(host, strings.TrimSpace(domain)) {
		return false
	}
	if strings.HasSuffix(strings.ToLower(host), ".invalid") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(local), "crewlet-")
}

func emailDomainOf(cfg *config.Datadog) string {
	if cfg == nil {
		return DefaultEmailDomain
	}
	return emailDomain(cfg.Provisioning)
}

// Findings is what a pass observed, in the shared vocabulary.
//
// The pass reports what it SAW rather than what it did, and every entry here
// is a thing an operator can act on. An agent's own account is reported by
// the access sync, per seat.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	if hook := r.Webhook; hook != nil {
		switch {
		case hook.Err != nil:
			// INGRESS, NOT IDENTITY. A definition that could not be
			// written is every alert this company has going nowhere,
			// which is a different thing from one agent lacking an
			// account and is owned by whoever holds the Datadog keys.
			out = append(out, integration.Finding{
				Kind:    integration.FindingIngressBlocked,
				Subject: hook.Name,
				Detail:  hook.Err.Error(),
			})
		case hook.Blocked != nil:
			// AND A DEFINITION THAT WAS NEVER ATTEMPTED IS THE SAME
			// OUTAGE, reached through this deployment's own
			// configuration rather than through Datadog refusing a
			// write. It was a note nothing read, so a company with no
			// public base URL or an unresolved webhook token had its
			// Datadog surface classified Ready over an inbound path
			// that did not exist — see [Unregistered].
			out = append(out, integration.Finding{
				Kind:    hook.Blocked.Kind,
				Subject: hook.Blocked.Subject,
				Detail:  hook.Blocked.Detail,
			})
		}
	}
	// AND THE ACCOUNTS NOTHING CLAIMS, said once.
	//
	// ONE FINDING rather than one per account, because there is nothing
	// per-account to do differently: the decision is the same for all of
	// them and is one person's, and 36 rows of it — measured — would bury
	// every other finding on the card. The addresses are in the detail,
	// which is where a list an operator acts on belongs.
	//
	// AN ADVISORY, so it does not hold the surface out of Ready.
	// [integration.FindingRegistrationOrphaned]'s verdict is phase-ready
	// and admin-owed: nothing is broken, and nothing this engine runs will
	// ever change it, so reporting it as a wait would leave somebody
	// watching a retry that has nothing to retry.
	if len(r.Orphaned) > 0 {
		addresses := make([]string, 0, len(r.Orphaned))
		for _, user := range r.Orphaned {
			addresses = append(addresses, user.Email)
		}
		// THE COUNT IN THE SENTENCE AND THE ADDRESSES IN THE LIST. The
		// detail is the card's one-line status and it is capped: written
		// inline, thirty-six addresses were a 500-character wall cut off
		// mid-address. [integration.Count] takes the number from the same
		// slice the list is, so the two cannot disagree.
		out = append(out, integration.Finding{
			Kind:    integration.FindingRegistrationOrphaned,
			Subject: "datadog service accounts",
			// "…matching no seat", a PARTICIPLE, so the sentence reads
			// correctly at one account and at thirty-six. A relative
			// clause needs "matches" or "match" and would put a second
			// number agreement in a sentence that already has one.
			Detail: "Crewlet made " +
				integration.Count(len(addresses), "enabled service account") +
				" at this company's own email domain matching no seat it " +
				"provisions for — a removed seat or a changed handle",
			Remedy: "Disable or delete the ones you do not want, at Datadog. " +
				"Nothing here removes them: an account is a colleague there, " +
				"with history attached.",
			Subjects: addresses,
		})
	}
	return out
}

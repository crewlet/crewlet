package scaleway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Options is what one pass over the company's half of Scaleway needs: the
// provisioner key and the agents' applications the access ledger records.
// Each agent's own application is the per-seat access sync's
// ([accessVendor]), which a setup pass runs beside this one.
type Options struct {
	// Client carries the provisioner key for the company's organization,
	// and AccessKey names that key.
	Client    *Client
	AccessKey string

	// Company is the company's name, which every agent application's name
	// carries ([ApplicationName]).
	Company string

	// Seats are the agents' applications the access ledger records, by
	// seat handle.
	Seats map[string]Seat

	// AtlasApplicationID is the atlas application the engine recorded, ""
	// where it made none: never an orphan, and what tells a stray one apart.
	AtlasApplicationID string

	// InUse are the levels a seat that is on holds ([seataccess.LevelsInUse]):
	// a level the catalogue cannot grant is a problem only for one of them.
	InUse []string

	// Now is the clock the expiry warning is judged by; nil is the wall
	// clock.
	Now func() time.Time
}

// Seat is one agent application the access ledger records.
type Seat struct {
	// ApplicationID is the application, and Name what the ledger recorded
	// it under, which the engine's policy on it carries.
	ApplicationID string
	Name          string

	// AccessKey is the access key sealed for the seat, "" where none is.
	AccessKey string
}

// Result is what one pass found. It writes nothing at Scaleway.
type Result struct {
	key         APIKey
	grants      grants
	keysRefused bool
	gaps        []seataccess.LevelGap
	inUse       []string

	// foreign are the policies on agents' applications the engine did not
	// write, and extraKeys the keys besides each agent's sealed one, each as
	// "<handle>: <name>".
	foreign   []string
	extraKeys []string

	// orphaned are the applications named for this company's agents that the
	// ledger does not record: reported, never touched, since a name is no
	// proof the engine made one.
	orphaned []Application

	// strays are the applications under the atlas application's name that the
	// engine did not record: reported, never touched.
	strays []Application

	now time.Time
}

// Reconcile reads the provisioner key, the organization and every agent
// application the ledger records, and reports what an operator has to act
// on. A refused key, or one that is a user's or another organization's, is
// an error wrapping [integration.ErrCredentialRejected]: nothing else a pass
// reports could be trusted over it.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("scaleway: no client")
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now()
	}
	in, err := inspect(ctx, opts.Client, strings.TrimSpace(opts.AccessKey), nil)
	if err != nil {
		return nil, reconcileError(err)
	}
	apps, err := opts.Client.Applications(ctx)
	if err != nil {
		return nil, reconcileError(&stageError{stage: stageApps, err: err})
	}
	res := &Result{
		key: in.key, grants: in.grants, keysRefused: in.keysRefused(), gaps: in.gaps,
		inUse: opts.InUse, now: now,
		orphaned: orphanedApplications(apps, opts.Seats, opts.Company, in.app.ID, opts.AtlasApplicationID),
		strays:   strayAtlasApplications(apps, opts.Company, strings.TrimSpace(opts.AtlasApplicationID)),
	}
	for _, handle := range slices.Sorted(maps.Keys(opts.Seats)) {
		seat := opts.Seats[handle]
		foreign, extra, err := strangers(ctx, opts.Client, opts.Company, handle, seat)
		if err != nil {
			return nil, reconcileError(&stageError{stage: stageSeat, err: err})
		}
		res.foreign = append(res.foreign, foreign...)
		res.extraKeys = append(res.extraKeys, extra...)
	}
	return res, nil
}

// strangers is what one agent's application holds that the engine did not put
// there: a policy under another name than the engine's, and a key besides the
// one sealed for the seat.
func strangers(ctx context.Context, c *Client, company, handle string, seat Seat) (policies, keys []string, err error) {
	held, err := c.PoliciesOf(ctx, seat.ApplicationID)
	if err != nil {
		return nil, nil, err
	}
	own := policyNameOf(seat.Name, company, handle)
	for _, p := range held {
		if p.Name != own {
			policies = append(policies, handle+": "+p.Name)
		}
	}
	live, err := c.APIKeysOf(ctx, seat.ApplicationID)
	if err != nil {
		return nil, nil, err
	}
	for _, k := range live {
		if k.AccessKey != seat.AccessKey {
			keys = append(keys, handle+": "+k.AccessKey)
		}
	}
	return policies, keys, nil
}

// orphanedApplications is every application named for this company's agents
// that is neither one the ledger records, the provisioner's own nor the atlas
// application the engine recorded.
func orphanedApplications(apps []Application, seats map[string]Seat, company, provisionerID, atlasID string) []Application {
	prefix := ApplicationName(company, "")
	managed := map[string]bool{}
	for _, s := range seats {
		managed[s.ApplicationID] = true
	}
	var out []Application
	for _, app := range apps {
		if app.ID == provisionerID || app.ID == atlasID || managed[app.ID] || !strings.HasPrefix(app.Name, prefix) {
			continue
		}
		out = append(out, app)
	}
	slices.SortFunc(out, func(a, b Application) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// reconcileError is a failed read as the loop reads it: a refusal, or a key
// that is not an application's in the organization, is the credential's, and
// anything else a fault it retries.
func reconcileError(err error) error {
	if bearer, ok := errors.AsType[*bearerError](err); ok {
		return fmt.Errorf("scaleway: integrations.scaleway.provisioning.secret_key: %w: %s",
			integration.ErrCredentialRejected, bearer.Error())
	}
	where, needs := "the key", ""
	if st, ok := errors.AsType[*stageError](err); ok {
		where, needs = st.stage.name, st.stage.needs
	}
	rejected := reject(err)
	if errors.Is(rejected, integration.ErrCredentialRejected) && needs != "" {
		return fmt.Errorf("scaleway: the provisioner key in integrations.scaleway.provisioning.secret_key "+
			"was refused reading %s, which needs %s: %w", where, needs, rejected)
	}
	return fmt.Errorf("scaleway: the provisioner key in integrations.scaleway.provisioning.secret_key "+
		"%s reading %s: %w", integration.Refusal(rejected), where, rejected)
}

// subjectKey is the field that clears a finding about the provisioner key.
const subjectKey = "integrations.scaleway.provisioning.secret_key"

// Findings is what the pass saw, in the shared vocabulary. Each agent's own
// application is reported per seat by the access sync.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	if expires := r.key.Expires(); integration.ExpiresSoon(expires, r.now) {
		out = append(out, integration.Finding{
			Kind: integration.FindingCredentialExpiring, Subject: subjectKey,
			Title: "Provisioner key expires soon",
			Detail: "The provisioner key expires on " + expires.Format(time.DateOnly) +
				". From then on no pass can make, narrow, rotate or delete an agent's application, policy or key",
			Remedy:    "Create a new API key for the provisioner application and replace it on the Scaleway connection",
			ActionURL: APIKeysURL, ExpiresAt: expires,
		})
	}
	if missing := r.grants.missing; len(missing) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Subject: subjectKey,
			Title: "Provisioner is missing permission sets",
			Detail: "The provisioner application's policies do not grant " + words.And(missing) +
				" on the Organization, which Crewlet needs to manage the agents' applications, policies and keys",
			Remedy: "Add " + pronoun(len(missing)) + " to a rule at Organization scope on a policy attached " +
				"to the provisioner application",
			ActionURL: PoliciesURL, Subjects: slices.Clone(missing),
		})
	}
	if r.keysRefused {
		said := "Scaleway refuses the provisioner key reading itself by its access key"
		if cond := r.grants.keyCondition; cond != "" {
			said = "The rule granting IAMApplicationManager carries the condition " + cond
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Subject: subjectKey,
			Title: "Provisioner cannot manage the agents' keys",
			Detail: said + ". An API key has no attribute naming its application, so the condition refuses " +
				"reading, minting, rotating and deleting the agents' keys",
			Remedy: "Remove the condition. Limit the key by a short expiry instead and, on an engine with a " +
				"fixed egress address, a condition on request.ip",
			ActionURL: PoliciesURL,
		})
	}
	out = append(out, seataccess.GapFindings(r.gaps, r.inUse)...)
	if len(r.extraGrants()) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: subjectKey,
			Title:  "Provisioner holds more than it needs",
			Detail: "The provisioner application also holds " + words.And(r.extraGrants()) + ", which Crewlet does not need",
			Remedy: "Remove them from its policies, keeping IAMApplicationManager, IAMPolicyManager and " +
				"ProjectReadOnly on the Organization",
			ActionURL: PoliciesURL, Subjects: r.extraGrants(),
		})
	}
	if len(r.foreign) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: "scaleway agent policies",
			Title: "Policies Crewlet did not write",
			Detail: "Scaleway holds " + integration.Count(len(r.foreign), "policy") + " on agents' " +
				"applications that Crewlet did not write, so those agents may hold more than their level",
			Remedy:    "Review them at Scaleway and delete the ones the agents should not hold. Crewlet never deletes a policy it did not write",
			ActionURL: PoliciesURL, Subjects: slices.Clone(r.foreign),
		})
	}
	if len(r.extraKeys) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: "scaleway agent keys",
			Title: "API keys beside an agent's own",
			Detail: "Scaleway holds " + integration.Count(len(r.extraKeys), "API key") + " on agents' " +
				"applications besides the one sealed for each agent, and each works until it expires",
			Remedy: "Delete the ones nobody uses at Scaleway. The agent's next key rotation deletes the ones " +
				"Crewlet minted itself",
			ActionURL: APIKeysURL, Subjects: slices.Clone(r.extraKeys),
		})
	}
	if len(r.orphaned) > 0 {
		names := make([]string, 0, len(r.orphaned))
		for _, app := range r.orphaned {
			names = append(names, app.Name+" ("+app.ID+")")
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingRegistrationOrphaned, Subject: "scaleway agent applications",
			Title: "Agent applications with no seat",
			Detail: "Scaleway holds " + integration.Count(len(names), "IAM application") + " named for this " +
				"company's agents that Crewlet has no record of making",
			Remedy:    "Delete the ones nobody uses at Scaleway. Crewlet never deletes an application it has no record of",
			ActionURL: ApplicationsURL, Subjects: names,
		})
	}
	if len(r.strays) > 0 {
		names := make([]string, 0, len(r.strays))
		for _, app := range r.strays {
			names = append(names, app.Name+" ("+app.ID+")")
		}
		slices.Sort(names)
		out = append(out, integration.Finding{
			Kind: integration.FindingRegistrationOrphaned, Subject: "scaleway atlas applications",
			Title: "Atlas applications Crewlet has no record of",
			Detail: "Scaleway holds " + integration.Count(len(names), "IAM application") + " under the name " +
				"Crewlet gives the Atlas application that Crewlet has no record of making",
			Remedy:    "Delete the ones nobody uses at Scaleway. Crewlet never deletes an application it has no record of",
			ActionURL: ApplicationsURL, Subjects: names,
		})
	}
	return out
}

// extraGrants is the sets the provisioner holds beyond the three it needs.
func (r *Result) extraGrants() []string { return slices.Clone(r.grants.extra) }

// policyNameOf is the name the engine writes a seat's policy under: the name
// the ledger recorded the application under when the engine made it, so a
// rename at Scaleway never turns the engine's own policy into a stranger's,
// else the name it would make the application under now.
func policyNameOf(recorded, company, handle string) string {
	if name := strings.TrimSpace(recorded); name != "" {
		return name
	}
	return ApplicationName(company, handle)
}

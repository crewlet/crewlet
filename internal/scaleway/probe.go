package scaleway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// probeCallTimeout bounds one call of the check inside [setup.VerifyBudget].
// Scaleway answers these reads in well under a second, so five seconds is
// slack for a slow answer.
const probeCallTimeout = 5 * time.Second

// Probe checks a typed provisioner key against its organization before
// anything stores it, with reads only: the key is an IAM application's in the
// organization, the application's own policies grant the three sets the
// engine needs on the organization with no condition that refuses key
// operations, and the catalogue lists every set an agent level grants.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "secret_key" {
			return setup.Verdict{}, setup.NotCheckable("Scaleway", check.Field)
		}
		secret := strings.TrimSpace(check.Value("secret_key"))
		access := strings.TrimSpace(check.Value("access_key"))
		org := config.ScalewayID(check.Value("organization_id"))
		switch {
		case secret == "":
			return setup.Verdict{}, setup.Unusable("Enter the provisioner secret key to test it.")
		case !accessKeyPattern().MatchString(access):
			return setup.Verdict{}, setup.Unusable("Enter the provisioner key's access key, SCW followed " +
				"by 17 capital letters or digits: the secret key is checked as that key.")
		case !uuidPattern().MatchString(org):
			return setup.Verdict{}, setup.Unusable("Enter the Organization ID, a UUID from the " +
				"Organization settings: the key is checked against it.")
		}
		client, err := NewClient(ClientOptions{OrganizationID: org, SecretKey: secret, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, fmt.Errorf("scaleway: probe: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()
		in, err := inspect(ctx, client, access, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, probeCallTimeout)
		})
		if err != nil {
			return probeVerdict(ctx, err, secret), nil
		}
		if refusal, refused := in.refusal(); refused {
			return refusal, nil
		}
		// A LEVEL THE CATALOGUE CANNOT GRANT is unavailable, not a broken
		// key, unless it is the default one: then no agent can be given a
		// level at all.
		if i := slices.IndexFunc(in.gaps, func(g seataccess.LevelGap) bool {
			return g.Level == config.ScalewayLevelRead
		}); i >= 0 {
			return setup.Refused("The key works, but no agent can be given a level yet.", in.gaps[i].Reason+"."), nil
		}
		return in.works(time.Now().UTC()), nil
	}
}

// refusal is the verdict for a provisioner that cannot provision, and false
// for one that can: a set it lacks, or a condition refusing key operations.
func (in inspection) refusal() (setup.Verdict, bool) {
	if missing := in.grants.missing; len(missing) > 0 {
		app := in.app.Name
		if app == "" {
			app = "the application"
		}
		return setup.Refused("The key works, but its policy does not grant "+words.And(missing)+".",
			"Crewlet needs "+pronoun(len(missing))+" to manage the agents' applications, policies and keys.",
			"Add "+pronoun(len(missing))+" to a rule at Organization scope, on a policy attached to "+app+
				" itself."), true
	}
	if in.keysRefused() {
		said := "Scaleway refused the key reading itself by its access key."
		if cond := in.grants.keyCondition; cond != "" {
			said = "The rule granting IAMApplicationManager carries the condition " + cond + "."
		}
		return setup.Refused("The key works, but it cannot read, mint, rotate or delete the agents' keys.",
			said,
			"An API key has no attribute naming its application, so no condition can keep the "+
				"provisioner to the agents' keys.",
			"Remove the condition, and limit the key by a short expiry and, on an engine with a fixed "+
				"egress address, a condition on request.ip."), true
	}
	return setup.Verdict{}, false
}

// works is the Test button's verdict for a key that can provision: what it
// reaches, then each fact a person may act on. The key's reach is the form's
// caution, said once there, so it is not repeated here.
func (in inspection) works(now time.Time) setup.Verdict {
	notes := []string{fmt.Sprintf("Application: %s, in an Organization with %s.",
		in.app.Name, integration.Count(len(in.projects), "project"))}
	switch expires := in.key.Expires(); {
	case expires.IsZero():
		notes = append(notes, "It never expires. Give it an expiry of 30 days or less.")
	case integration.ExpiresSoon(expires, now):
		notes = append(notes, "Expires on "+expires.Format(time.DateOnly)+", soon: create its "+
			"replacement before then.")
	default:
		notes = append(notes, "Expires on "+expires.Format(time.DateOnly)+".")
	}
	if extra := in.grants.extra; len(extra) > 0 {
		notes = append(notes, "Its application also holds "+words.And(extra)+", which Crewlet does not need.")
	}
	if !in.grants.pinned() {
		notes = append(notes, "No rule limits it to a source address. On an engine with a fixed egress "+
			"address, add a condition on request.ip to its rule.")
	}
	for _, cond := range in.grants.conditions {
		if cond != in.grants.keyCondition {
			notes = append(notes, "A rule on it carries the condition "+cond+".")
		}
	}
	for _, g := range in.gaps {
		notes = append(notes, g.Reason+".")
	}
	return setup.Works("Works. The key can manage the agents' applications, policies and keys.", notes...)
}

// probeVerdict is the verdict for a failed read: a refusal names the read and
// the set it needs, and everything else is [setup.Judge]'s.
func probeVerdict(ctx context.Context, err error, secret string) setup.Verdict {
	if bearer, ok := errors.AsType[*bearerError](err); ok {
		return setup.Refused(bearer.said+".", bearer.remedy+".")
	}
	name := "the key"
	st, staged := errors.AsType[*stageError](err)
	if staged {
		name = st.stage.name
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("Scaleway did not answer in time while checking " + name + ".")
	}
	if Status(err) == http.StatusUnauthorized {
		return setup.Refused("Scaleway does not accept this secret key.",
			"It is mistyped, expired or deleted. Paste the secret key shown when the API key was created.")
	}
	if staged && st.stage == stageKey && Status(err) == http.StatusNotFound {
		return setup.Refused("The secret key works, but no API key in the Organization has this access key.",
			"Paste the access key shown beside the secret key.")
	}
	reason := ""
	api, isAPI := errors.AsType[*APIError](err)
	if isAPI && api.Detail != noDetail {
		reason = api.Detail
	}
	// THE ADDRESS IS FIXED (APIBase), so a 4xx other than a rate limit is
	// Scaleway refusing what was sent, never an address that is wrong.
	refused := isAPI && api.Status >= http.StatusBadRequest && api.Status < http.StatusInternalServerError &&
		api.Status != http.StatusTooManyRequests
	verdict := setup.Judge(setup.Failure{App: "Scaleway", Err: err, Status: Status(err), Refused: refused}, "")
	if verdict.Outcome != setup.VerifyRefused {
		return verdict
	}
	summary, needs := "Scaleway refused the key.", ""
	if staged {
		summary = "Scaleway refused the key reading " + st.stage.name + "."
		if st.stage.needs != "" {
			needs = "Reading it needs " + st.stage.needs + " on the Organization."
		}
	}
	return setup.Refused(summary, needs, setup.SaidBy("Scaleway", reason, secret))
}

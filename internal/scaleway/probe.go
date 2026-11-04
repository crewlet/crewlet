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
		case !accessKeyPattern.MatchString(access):
			return setup.Verdict{}, setup.Unusable("Enter the provisioner key's access key, SCW followed " +
				"by 17 capital letters or digits: the secret key is checked as that key.")
		case !uuidPattern.MatchString(org):
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
		if refusal := in.refusal(); refusal != "" {
			return setup.Refused(refusal), nil
		}
		// A LEVEL THE CATALOGUE CANNOT GRANT is unavailable, not a broken
		// key, unless it is the default one: then no agent can be given a
		// level at all.
		if i := slices.IndexFunc(in.gaps, func(g seataccess.LevelGap) bool {
			return g.Level == config.ScalewayLevelRead
		}); i >= 0 {
			return setup.Refused("The key works, but no agent can be given a level yet. " + in.gaps[i].Reason + "."), nil
		}
		return setup.Works(in.worksDetail(time.Now().UTC())), nil
	}
}

// refusal is the sentence for a provisioner that cannot provision, "" for one
// that can: a set it lacks, or a condition refusing key operations.
func (in inspection) refusal() string {
	if missing := in.grants.missing; len(missing) > 0 {
		return "The key works, but its application's policies do not grant " + words.And(missing) +
			" on the Organization, which Crewlet needs to manage the agents' applications, policies " +
			"and keys. Add " + pronoun(len(missing)) + " to a rule at Organization scope on a policy " +
			"attached to the application itself."
	}
	if in.keysRefused() {
		said := "Scaleway refused the key reading itself by its access key"
		if cond := in.grants.keyCondition; cond != "" {
			said = "The rule granting IAMApplicationManager carries the condition " + cond
		}
		return "The key works, but " + lowerFirst(said) + ", so Crewlet cannot read, mint, rotate or " +
			"delete the agents' keys under it. An API key has no attribute naming its application, so " +
			"no condition can keep the provisioner to the agents' keys. Remove the condition, and limit " +
			"the key by a short expiry and, on an engine with a fixed egress address, a condition on " +
			"request.ip."
	}
	return ""
}

// worksDetail is the Test button's sentence for a key that can provision: what
// it reaches, plainly, then each note a person may act on.
func (in inspection) worksDetail(now time.Time) string {
	parts := []string{fmt.Sprintf("Works. The key belongs to the IAM application %s, which can manage "+
		"the agents' applications, policies and keys, and the Organization has %s",
		in.app.Name, integration.Count(len(in.projects), "project"))}
	parts = append(parts, "This key can mint keys for, and grant anything to, any application in the "+
		"Organization, so give it a short expiry and rotate it")
	switch expires := in.key.Expires(); {
	case expires.IsZero():
		parts = append(parts, "It never expires")
	case integration.ExpiresSoon(expires, now):
		parts = append(parts, "It expires on "+expires.Format(time.DateOnly)+", soon: create its "+
			"replacement before then")
	default:
		parts = append(parts, "It expires on "+expires.Format(time.DateOnly))
	}
	if extra := in.grants.extra; len(extra) > 0 {
		parts = append(parts, "Its application also holds "+words.And(extra)+", which Crewlet does not need")
	}
	if !in.grants.pinned() {
		parts = append(parts, "No rule limits it to a source address. On an engine with a fixed egress "+
			"address, add a condition on request.ip to its rule")
	}
	for _, cond := range in.grants.conditions {
		if cond != in.grants.keyCondition {
			parts = append(parts, "A rule on it carries the condition "+cond)
		}
	}
	for _, g := range in.gaps {
		parts = append(parts, g.Reason)
	}
	return strings.Join(parts, ". ") + "."
}

// lowerFirst is s with its first letter lowered, for a clause inside a
// sentence.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// probeVerdict is the verdict for a failed read: a refusal names the read and
// the set it needs, and everything else is [setup.Judge]'s.
func probeVerdict(ctx context.Context, err error, secret string) setup.Verdict {
	if bearer, ok := errors.AsType[*bearerError](err); ok {
		return setup.Refused(bearer.said + ".")
	}
	name := "the key"
	st, staged := errors.AsType[*stageError](err)
	if staged {
		name = st.stage.name
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Scaleway did not answer in " +
			"time while checking " + name + ". Nothing was stored."}
	}
	if Status(err) == http.StatusUnauthorized {
		return setup.Refused("Scaleway does not accept this secret key: it is mistyped, expired or " +
			"deleted. Paste the secret key shown when the API key was created.")
	}
	if staged && st.stage == stageKey && Status(err) == http.StatusNotFound {
		return setup.Refused("The secret key works, but Scaleway has no API key with this access key " +
			"in the Organization. Paste the access key shown beside the secret key.")
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
	sentence := "Scaleway refused the key"
	if staged {
		sentence = "Scaleway refused the key reading " + st.stage.name
		if st.stage.needs != "" {
			sentence += ", which needs " + st.stage.needs + " on the Organization"
		}
	}
	if said := setup.Sayable(reason, secret); said != "" {
		sentence += ": " + strings.TrimRight(said, ".")
	}
	return setup.Refused(sentence + ".")
}

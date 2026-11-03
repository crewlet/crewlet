package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// identityAPIs are what the identity project must have enabled: accounts,
// impersonation, federation, the project reads, and the quota project of the
// provisioner's own calls.
//
//nolint:gochecknoglobals // an immutable list, not state
var identityAPIs = []string{
	"iam.googleapis.com", "iamcredentials.googleapis.com", "sts.googleapis.com",
	"cloudresourcemanager.googleapis.com", "serviceusage.googleapis.com",
}

// accountAdmin is what Service Account Admin lets the provisioner do with the
// agents' accounts in the identity project, every one a step it takes.
//
//nolint:gochecknoglobals // an immutable list, not state
var accountAdmin = []string{
	"iam.serviceAccounts.create", "iam.serviceAccounts.delete", "iam.serviceAccounts.disable",
	"iam.serviceAccounts.enable", "iam.serviceAccounts.get", "iam.serviceAccounts.getIamPolicy",
	"iam.serviceAccounts.list", "iam.serviceAccounts.setIamPolicy",
}

// defaultAccountQuota is Google's default Service Account Count per project,
// which the Test button reports the identity project's accounts against.
const defaultAccountQuota = 100

// Probe checks the connection with reads only, before or after it is stored:
// federation, impersonation, the identity project's APIs and the provisioner's
// grants there, every project prepared for Crewlet, foreign accounts in the
// identity project, and its account count. Each failure is a sentence and its
// fix.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "provider" {
			return setup.Verdict{}, setup.NotCheckable("Google Cloud", check.Field)
		}
		value := func(field string) string {
			if check.Value == nil {
				return ""
			}
			return check.Value(field)
		}
		return probeCheck(ctx, check.Company, value, httpClient, nil)
	}
}

// probeCheck is the Test button's check on clk (nil is the wall clock), and an
// error only for inputs no check can be made with.
func probeCheck(
	ctx context.Context, company string, value func(string) string, hc *http.Client, clk *clock,
) (setup.Verdict, error) {
	email, provider, key := strings.TrimSpace(value("provisioner")), strings.TrimSpace(value("provider")),
		strings.TrimSpace(value("federation_key"))
	switch {
	case email == "":
		return setup.Verdict{}, setup.Unusable("Enter the provisioner service account's email to test the connection.")
	case key == "":
		return setup.Verdict{}, setup.Unusable("Save the provisioner first: that mints the federation key the " +
			"provider holds the public half of.")
	case provider == "":
		return setup.Verdict{}, setup.Unusable("Paste the provider name the Cloud Shell block prints to test it.")
	}
	client, err := NewClient(ClientOptions{
		Company: company, Provisioner: email, Provider: provider, FederationKey: key, HTTP: hc,
	})
	if err != nil {
		return setup.Verdict{}, setup.Unusable(strings.TrimPrefix(err.Error(), "googlecloud: ") + ".")
	}
	if clk != nil {
		client.clock = *clk
	}
	ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
	defer cancel()
	if _, err := client.token(ctx); err != nil {
		return reachRefusal(ctx, client, company, err), nil
	}
	identity := client.IdentityProject()
	enabled, err := client.EnabledServices(ctx, identity)
	if err != nil {
		return readRefusal(ctx, err, "the APIs enabled on "+identity,
			"Grant the provisioner Service Usage Viewer on "+identity+" (the Cloud Shell block does)"), nil
	}
	if missing := absent(identityAPIs, enabled); len(missing) > 0 {
		return setup.Refused("Crewlet reaches the provisioner, but " + identity + " does not have " +
			words.And(missing) + " enabled. Enable " + pronoun(len(missing)) + " on the identity project's " +
			"APIs page, then test again."), nil
	}
	held, err := client.TestProjectPermissions(ctx, identity, accountAdmin)
	if err != nil {
		return readRefusal(ctx, err, "its own permissions on "+identity, ""), nil
	}
	if missing := absent(accountAdmin, set(held)); len(missing) > 0 {
		return setup.Refused("Crewlet reaches the provisioner, but it cannot manage service accounts in " +
			identity + " (it lacks " + words.And(missing) + "). Grant it Service Account Admin on that " +
			"project, then test again."), nil
	}
	accounts, err := client.ServiceAccounts(ctx)
	if err != nil {
		return readRefusal(ctx, err, "the accounts of "+identity, ""), nil
	}
	found, err := discover(ctx, client)
	if err != nil {
		return readRefusal(ctx, err, "the projects it can read",
			"Grant the provisioner its three Project IAM Admin conditions on each project agents may use"), nil
	}
	return setup.Works(worksDetail(client, accounts, found)), nil
}

// reachRefusal is the sentence for a provisioner Crewlet cannot reach, and
// the fix for the half of the setup that refused.
func reachRefusal(ctx context.Context, c *Client, company string, err error) setup.Verdict {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Google did not answer in time while " +
			"Crewlet signed in as the provisioner. Nothing was stored."}
	}
	st, staged := errors.AsType[*stageError](err)
	said := ""
	if api, ok := errors.AsType[*APIError](err); ok {
		said = ": " + strings.TrimRight(setup.Sayable(api.Message), ".")
	}
	switch {
	case staged && st.stage == stageFederation:
		return setup.Refused("Google's Security Token Service refused Crewlet's federation token" + said + ". Run the " +
			"Cloud Shell block again so the provider holds Crewlet's current public key and accepts the " +
			"subject " + Subject(company) + ", and check the provider name is the one it printed.")
	case staged && st.stage == stageImpersonation:
		return setup.Refused("Federation works, but Crewlet cannot act as " + c.Provisioner() + said + ". Grant " +
			Principal(c.provider, company) + " Workload Identity User on that service account (the Cloud " +
			"Shell block does), and enable the IAM Service Account Credentials API on " + c.IdentityProject() + ".")
	default:
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Crewlet could not sign in as the " +
			"provisioner: " + err.Error() + ". Nothing was stored."}
	}
}

// readRefusal is the sentence for a read the provisioner was refused or that
// failed, with fix where the read names one.
func readRefusal(ctx context.Context, err error, what, fix string) setup.Verdict {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: "Google did not answer in time while " +
			"Crewlet read " + what + ". Nothing was stored."}
	}
	sentence := "Crewlet reaches the provisioner, but could not read " + what
	api, ok := errors.AsType[*APIError](err)
	if !ok || api.Status >= http.StatusInternalServerError || api.Status == http.StatusTooManyRequests {
		return setup.Verdict{Outcome: setup.VerifyUnreachable, Detail: sentence + ": " + err.Error() +
			". Nothing was stored."}
	}
	if said := setup.Sayable(api.Message); said != "" {
		sentence += ": " + strings.TrimRight(said, ".")
	}
	if api.Status == http.StatusForbidden && fix != "" {
		return setup.Refused(sentence + ". " + fix + ", then test again.")
	}
	return setup.Refused(sentence + ".")
}

// worksDetail is the Test button's sentence for a connection that works: what
// it reaches, then each note a person may act on.
func worksDetail(c *Client, accounts []ServiceAccount, found discovery) string {
	identity := c.IdentityProject()
	parts := []string{"Works. Crewlet acts as " + c.Provisioner() + " through federation, with no key, " +
		"and makes the agents' service accounts in " + identity}
	offered, names := found.offered()
	if len(offered) == 0 {
		parts = append(parts, "No project is prepared for Crewlet yet, so no agent can reach anything: run "+
			"the Prepare a project block on each project agents may use, then test again")
	} else {
		listed := make([]words.Said, 0, len(offered))
		for _, id := range offered {
			listed = append(listed, words.Said(cmpName(names[id], id)))
		}
		parts = append(parts, fmt.Sprintf("Agents may be given %s: %s",
			integration.Count(len(offered), "project"), words.And(listed)))
	}
	for _, p := range found {
		if !p.Offered() {
			parts = append(parts, p.ID+" is not offered: "+strings.Join(p.Problems, "; "))
		}
	}
	var foreign []string
	for _, a := range accounts {
		if a.Email != c.Provisioner() && !strings.HasPrefix(a.Email, accountPrefix) {
			foreign = append(foreign, a.Email)
		}
	}
	if len(foreign) > 0 {
		slices.Sort(foreign)
		parts = append(parts, "The provisioner can also act as "+words.And(foreign)+", which "+identity+
			" should not hold: move "+pronoun(len(foreign))+" to another project")
	}
	parts = append(parts, fmt.Sprintf("%s holds %d of Google's default quota of %d service accounts",
		identity, len(accounts), defaultAccountQuota))
	return strings.Join(parts, ". ") + "."
}

// cmpName is a project as a sentence names it: its display name and its ID,
// or its ID alone.
func cmpName(name, id string) string {
	if name == "" || name == id {
		return id
	}
	return name + " (" + id + ")"
}

// absent is what of want has is not in, in want's order.
func absent(want []string, has map[string]bool) []string {
	var out []string
	for _, w := range want {
		if !has[w] {
			out = append(out, w)
		}
	}
	return out
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// pronoun is "it" for one and "them" for more.
func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

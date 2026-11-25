package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googleauth"
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

// probeParts are the parts of the Test, in the order it runs them, which a
// refusal lists up to the one that failed ([setup.Parts]).
//
//nolint:gochecknoglobals // an immutable list, not state
var probeParts = []string{
	"Federation", "Acting as the provisioner", "Identity project's APIs", "Provisioner's grants",
	"Identity project's accounts", "Prepared projects",
}

// The index of each of [probeParts].
const (
	atFederation = iota
	atImpersonation
	atAPIs
	atGrants
	atAccounts
	atProjects
)

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
	ctx context.Context, company string, value func(string) string, hc *http.Client, clk *googleauth.Clock,
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
	client, err := NewClient(googleauth.Options{
		Company: company, Provisioner: email, Provider: provider, FederationKey: key, HTTP: hc,
	})
	if err != nil {
		return setup.Verdict{}, setup.Unusable(googleauth.Sentence(err) + ".")
	}
	if clk != nil {
		client.setClock(*clk)
	}
	ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
	defer cancel()
	if _, err = client.auth.FederatedToken(ctx, client.Provisioner()); err != nil {
		return reachRefusal(ctx, client, company, err), nil
	}
	identity := client.IdentityProject()
	enabled, err := client.EnabledServices(ctx, identity)
	if err != nil {
		return readRefusal(ctx, err, atAPIs, "the APIs enabled on "+identity,
			"Grant the provisioner Service Usage Viewer on "+identity+" (the Cloud Shell block does)"), nil
	}
	if missing := absent(identityAPIs, enabled); len(missing) > 0 {
		return setup.Refused("Crewlet reaches the provisioner, but "+identity+" lacks an API it needs.",
			append(setup.Parts(probeParts, atAPIs),
				setup.Fact{Label: "Not enabled", Value: words.And(unquoted(missing))},
				setup.Warning{Action: "Enable " + pronoun(len(missing)) + " on the identity project's APIs page, " +
					"or run the Cloud Shell block again, then test again."})...), nil
	}
	held, err := client.TestProjectPermissions(ctx, identity, accountAdmin)
	if err != nil {
		return readRefusal(ctx, err, atGrants, "its own permissions on "+identity, adminFix(identity)), nil
	}
	if missing := absent(accountAdmin, set(held)); len(missing) > 0 {
		return setup.Refused("The provisioner cannot manage service accounts in "+identity+".",
			append(setup.Parts(probeParts, atGrants),
				setup.Fact{Label: "It lacks", Value: words.And(unquoted(missing))},
				setup.Warning{Action: "Grant it Service Account Admin on that project, or run the Cloud Shell block " +
					"again, then test again."})...), nil
	}
	accounts, err := client.ServiceAccounts(ctx)
	if err != nil {
		return readRefusal(ctx, err, atAccounts, "the accounts of "+identity, adminFix(identity)), nil
	}
	found, err := discover(ctx, client)
	if err != nil {
		return readRefusal(ctx, err, atProjects, "its projects",
			"Grant the provisioner its three Project IAM Admin conditions on each project agents may use"), nil
	}
	return works(client, accounts, found), nil
}

// adminFix is the fix for a read Service Account Admin on identity allows.
func adminFix(identity string) string {
	return "Grant the provisioner Service Account Admin on " + identity + " (the Cloud Shell block does)"
}

// reachRefusal is the verdict for a provisioner Crewlet cannot reach, and
// the fix for the half of the setup that refused.
func reachRefusal(ctx context.Context, c *Client, company string, err error) setup.Verdict {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("Google did not answer in time while Crewlet signed in as the provisioner.")
	}
	st, staged := errors.AsType[*googleauth.StageError](err)
	var said setup.Fact
	if api, ok := errors.AsType[*googleauth.APIError](err); ok {
		said = setup.SaidBy("Google", api.Message)
	}
	switch {
	case staged && st.Stage == googleauth.StageFederation:
		return setup.Refused("Google's Security Token Service refused Crewlet's federation token.",
			append(setup.Parts(probeParts, atFederation), said,
				setup.Warning{Action: "Run the Cloud Shell block again so the provider holds Crewlet's current public " +
					"key and accepts the subject `" + googleauth.Subject(company) + "`. Check the provider name is the one " +
					"the block printed."})...)
	case staged && st.Stage == googleauth.StageImpersonation:
		return setup.Refused("Federation works, but Crewlet cannot act as "+c.Provisioner()+".",
			append(setup.Parts(probeParts, atImpersonation), said,
				setup.Warning{Action: "Grant `" + googleauth.Principal(c.auth.Provider(), company) + "` Workload Identity " +
					"User on that service account. The Cloud Shell block does. Enable the IAM Service Account " +
					"Credentials API on " + c.IdentityProject() + "."})...)
	default:
		return setup.Unreachable("Crewlet could not sign in as the provisioner.", setup.ErrorOf(err))
	}
}

// readRefusal is the verdict for a read the provisioner was refused or that
// failed at part, with fix where the read names one. A read Google did not
// answer fails no part: only the parts before it are said.
func readRefusal(ctx context.Context, err error, part int, what, fix string) setup.Verdict {
	passed := setup.Parts(probeParts[:part], -1)
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("Google did not answer in time while Crewlet read "+what+".", passed...)
	}
	summary := "Crewlet reaches the provisioner, but could not read " + what + "."
	api, ok := errors.AsType[*googleauth.APIError](err)
	if !ok || api.Status >= http.StatusInternalServerError || api.Status == http.StatusTooManyRequests {
		return setup.Unreachable(summary, append(passed, setup.ErrorOf(err))...)
	}
	failed := append(setup.Parts(probeParts, part), setup.SaidBy("Google", api.Message))
	if api.Status == http.StatusForbidden && fix != "" {
		return setup.Refused(summary, append(failed, setup.Warning{Action: fix + ", then test again."})...)
	}
	return setup.Refused(summary, failed...)
}

// works is the Test button's verdict for a connection that works: who it acts
// as and what it reaches, then each risk with what to do about it.
func works(c *Client, accounts []ServiceAccount, found discovery) setup.Verdict {
	identity := c.IdentityProject()
	details := []setup.Detail{
		setup.Fact{Label: "Acts as", Value: c.Provisioner()},
		setup.Fact{Label: "Agent accounts in", Value: identity},
	}
	offered, names := found.offered()
	if len(offered) == 0 {
		details = append(details, setup.Warning{
			Title:  "No project is prepared for Crewlet yet, so no agent can reach anything.",
			Action: "Run the Prepare a project block on each project agents may use, then test again.",
		})
	} else {
		listed := make([]words.Said, 0, len(offered))
		for _, id := range offered {
			listed = append(listed, words.Said(cmpName(names[id], id)))
		}
		details = append(details, setup.Fact{Label: "Agents may use", Value: words.And(listed)})
	}
	for _, p := range found {
		for _, problem := range p.Problems {
			details = append(details, setup.Warning{
				Title: p.ID + " is not offered: " + problem.said + ".", Action: problem.fix + ".",
			})
		}
	}
	var foreign []string
	for _, a := range accounts {
		if !crewletsOwn(a.Email, c.Provisioner()) {
			foreign = append(foreign, a.Email)
		}
	}
	if len(foreign) > 0 {
		slices.Sort(foreign)
		details = append(details, setup.Warning{
			Title:  "The provisioner can also act as " + words.And(foreign) + ", which " + identity + " should not hold.",
			Action: "Move " + pronoun(len(foreign)) + " to another project.",
		})
	}
	details = append(details, setup.Fact{
		Label: "Service accounts",
		Value: fmt.Sprintf("%d of Google's default quota of %d", len(accounts), defaultAccountQuota),
	})
	return setup.Works("Works. Crewlet acts as the provisioner through federation, with no key.", details...)
}

// unquoted is names as a fact's value says them: a value is not a sentence, so
// nothing needs setting apart from the words around it.
func unquoted(names []string) []words.Said {
	out := make([]words.Said, 0, len(names))
	for _, name := range names {
		out = append(out, words.Said(name))
	}
	return out
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

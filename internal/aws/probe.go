package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// rerun is the fix for every way the account's stack differs from this
// engine's: the CloudShell block creates or updates it in place.
const rerun = "Run the CloudShell block on the AWS connection again to update the stack, then test again. " +
	"Do not edit Crewlet's roles or policies by hand."

// Probe checks the connection with reads only, before or after it is stored:
// the provisioner's sign-in, every Crewlet policy and role against what this
// engine renders, the stack's version, the identity split and the account's
// role quota. Each failure is a sentence and its fix.
//
// hc is nil in production; a test hands one that reaches a fake.
func Probe(hc *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != "connection" {
			return setup.Verdict{}, setup.NotCheckable("AWS", check.Field)
		}
		value := func(field string) string {
			if check.Value == nil {
				return ""
			}
			return strings.TrimSpace(check.Value(field))
		}
		prov := config.AWSProvisioning{Slug: value("slug"), HomeRegion: value("home_region"), Regions: check.Regions,
			CertificateAuthority: value("certificate_authority"), Connection: value("connection")}
		return probeCheck(ctx, prov, func(conn *Connection) *Client { return NewClient(conn, hc) })
	}
}

// probeCheck is the Test button's check through the client build makes, and
// an error only for inputs no check can be made with.
func probeCheck(ctx context.Context, prov config.AWSProvisioning, build func(*Connection) *Client) (setup.Verdict, error) {
	switch {
	case prov.CertificateAuthority == "":
		return setup.Verdict{}, setup.Unusable("Save the short name and the home Region first: that mints the " +
			"sign-in certificate the stack holds.")
	case prov.Connection == "":
		return setup.Verdict{}, setup.Unusable("Paste the stack output the CloudShell block prints to test it.")
	}
	conn, err := NewConnection(&prov)
	if err != nil {
		return setup.Verdict{}, setup.Unusable(sentence(strings.TrimPrefix(err.Error(), "aws: ")))
	}
	ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
	defer cancel()
	report, err := build(conn).CheckStack(ctx, prov.RegionsOrDefault())
	if err != nil {
		return checkRefusal(ctx, conn, err), nil
	}
	return verdictOf(conn, report), nil
}

// verdictOf is the Test button's verdict for a stack it could read.
func verdictOf(conn *Connection, r StackReport) setup.Verdict {
	switch {
	case r.Outdated() && r.Version == "":
		return setup.Refused("The stack carries no version, where this engine expects version "+StackVersion+".",
			rerun)
	case r.Outdated():
		return setup.Refused(fmt.Sprintf("The stack is out of date: version %s, where this engine expects %s.",
			r.Version, StackVersion), rerun)
	case len(r.Split) > 0:
		return setup.Refused("Crewlet signs in, but one identity can become another.",
			append(sentences(r.Split), rerun)...)
	case len(r.Drift) > 0:
		return setup.Refused("Crewlet signs in, but the stack is not the one this engine expects.",
			append(sentences(r.Drift), rerun)...)
	}
	notes := []string{"Signs in as " + conn.resolve(provisionerName) + ", through IAM Roles Anywhere."}
	if r.RolesQuota > 0 {
		notes = append(notes, fmt.Sprintf("%d of the account's %d IAM roles are in use.", r.Roles, r.RolesQuota))
		if free := r.RolesQuota - r.Roles; free < 20 {
			notes = append(notes, fmt.Sprintf("Only %d more can be made, one per agent: ask AWS to raise the "+
				"IAM roles quota before adding agents.", free))
		}
	}
	return setup.Works("Works. Crewlet signs in, and every policy and role matches this version.", notes...)
}

// checkRefusal is the verdict for a check that could not read the stack.
func checkRefusal(ctx context.Context, conn *Connection, err error) setup.Verdict {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return setup.Unreachable("AWS did not answer in time while Crewlet read the stack.")
	}
	var api *APIError
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != http.StatusTooManyRequests {
		return setup.Refused("AWS refused Crewlet's sign-in as "+conn.resolve(provisionerName)+".",
			setup.SaidBy("AWS", api.Message),
			"The stack's trust anchor must hold this engine's certificate, and the output must be this stack's.",
			rerun)
	}
	if status := Status(err); status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return setup.Refused("Crewlet signs in, but could not read the stack.", setup.SaidBy("AWS", err.Error()), rerun)
	}
	return setup.Unreachable("Crewlet could not read the stack.", sentence(err.Error()))
}

// sentences is each of ss as a sentence, for the notes under a refusal.
func sentences(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, sentence(s))
	}
	return out
}

// sentence is s as a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

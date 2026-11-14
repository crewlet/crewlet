package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// rerun is the fix for every way the account's stack differs from this
// engine's: the CloudShell block creates or updates it in place.
const rerun = "Run the CloudShell block on the AWS connection again to update the stack, then test again. " +
	"Do not edit Crewlet's roles or policies by hand."

// atlasRerun is the fix for an Atlas reader the stack or its output lacks.
const atlasRerun = "Run the CloudShell block on the AWS connection again and paste its last line, then test again. " +
	"Agents are not affected."

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
			CertificateAuthority: value("certificate_authority"), Connection: value("connection"),
			AtlasRead: value("atlas_read") == "true"}
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
	report, err := build(conn).CheckStack(ctx, prov.RegionsOrDefault(), prov.AtlasRead)
	if err != nil {
		return checkRefusal(ctx, conn, err), nil
	}
	return verdictOf(conn, report), nil
}

// verdictOf is the Test button's verdict for a stack it could read.
func verdictOf(conn *Connection, r StackReport) setup.Verdict {
	fix := setup.Warning{Action: rerun}
	switch {
	case r.Outdated() && r.Version == "":
		return setup.Refused("The stack carries no version, where this engine expects version "+StackVersion+".", fix)
	case r.Outdated():
		return setup.Refused(fmt.Sprintf("The stack is out of date: version %s, where this engine expects %s.",
			r.Version, StackVersion), fix)
	case len(r.Split) > 0:
		details := make([]setup.Detail, 0, len(r.Split))
		for _, split := range r.Split {
			details = append(details, setup.Warning{Title: sentence(split), Action: rerun})
		}
		return setup.Refused("Crewlet signs in, but one identity can become another.", details...)
	case len(r.Drift) > 0:
		// EVERY OBJECT THAT DIFFERS HAS THE ONE FIX, so they are facts under one
		// action rather than a dozen warnings saying the same thing.
		details := make([]setup.Detail, 0, len(r.Drift)+1)
		for _, drift := range r.Drift {
			details = append(details, setup.Fact{Label: "Differs", Value: strings.TrimRight(drift, ".")})
		}
		return setup.Refused("Crewlet signs in, but the stack is not the one this engine expects.",
			append(details, fix)...)
	}
	details := []setup.Detail{
		setup.Fact{Label: "Signs in as", Value: conn.resolve(provisionerName)},
		setup.Fact{Label: "Through", Value: "IAM Roles Anywhere"},
	}
	if r.RolesQuota > 0 {
		details = append(details, setup.Fact{Label: "IAM roles", Value: fmt.Sprintf("%d of %d in use", r.Roles, r.RolesQuota)})
		if free := r.RolesQuota - r.Roles; free < 20 {
			details = append(details, setup.Warning{
				Title:  "Only " + integration.Count(free, "more IAM role") + " can be made, one per agent.",
				Action: "Ask AWS to raise the IAM roles quota before adding agents.",
			})
		}
	}
	// THE ATLAS'S ROLE NEVER REFUSES THE CONNECTION: agents work without it,
	// so what it lacks is said beside a stack that works.
	for _, problem := range r.Atlas {
		details = append(details, setup.Warning{Title: sentence(problem), Action: atlasRerun})
	}
	return setup.Works("Works. Crewlet signs in, and every policy and role matches this version.", details...)
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
			setup.Warning{
				Title:  "The stack's trust anchor must hold this engine's certificate, and the output must be this stack's.",
				Action: rerun,
			})
	}
	if status := Status(err); status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return setup.Refused("Crewlet signs in, but could not read the stack.",
			setup.SaidBy("AWS", err.Error()), setup.Warning{Action: rerun})
	}
	return setup.Unreachable("Crewlet could not read the stack.", setup.ErrorOf(err))
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

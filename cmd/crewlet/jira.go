package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
)

// `crewlet jira provision` — the Atlassian tracker's reconcile.
//
// # It reports far more than it changes, and that is Jira's shape
//
// The other vendor commands are mostly WRITES: they create accounts and mint
// the credentials the seats authenticate with. Jira issues neither — a Cloud
// API token is created by the person it belongs to, and a Data Center
// personal access token can only be minted for the calling user — so a
// command that offered to provision accounts would be printing instructions
// dressed as actions.
//
// What it does instead is answer the three questions that are otherwise
// invisible until an issue reaches nobody: which account each seat's
// credential authenticates as, whether every project the org names exists
// and agrees about its lead, and whether the inbound webhook is registered.

func runJiraProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("jira provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhook")
	recreate := fs.Bool("recreate-webhook", false,
		"delete and remake the webhook to mint a fresh secret; this "+
			"invalidates the secret every other deployment of this company holds")
	dryRun := fs.Bool("dry-run", false,
		"read and report, and register nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet jira provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-recreate-webhook] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Jira
	if cfg == nil {
		return errors.New(
			"jira: the company config has no integrations.jira block, so " +
				"there is no instance to reconcile")
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}

	ctx := context.Background()
	env, closeEnv, err := companyResolver(ctx, *sinks.bootstrap, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

	resolved := config.Jira{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	base := resolved.BaseURL()
	if base == "" {
		return fmt.Errorf(
			"jira: neither integrations.jira.url (%q) nor cloud_id (%q) "+
				"resolved to anything", cfg.URL, cfg.CloudID)
	}
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return fmt.Errorf(
			"jira: integrations.jira.token (%q) resolved empty — the org "+
				"account is what this run reads the instance with", cfg.Token)
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return err
	}

	// THE SINK IS OPENED FOR A REAL RUN ONLY. A dry run reads the instance
	// and registers nothing, so it has nothing to record — and opening the
	// secret store to write nothing would prompt for a passphrase on a
	// command that promised to touch nothing.
	opts := jira.Options{
		Client: client, Config: cfg, Org: organization,
		Value:           env.Value,
		RecreateWebhook: *recreate,
	}
	if *dryRun {
		fmt.Fprintln(stdout,
			"-dry-run: reading the instance; no webhook will be registered.")
	} else {
		opts.WebhookBase = *publicURL
		sink, closeSink, openErr := sinks.open(ctx, stdout)
		if openErr != nil {
			return openErr
		}
		defer closeSink()
		opts.Sink = sink
	}

	res, err := jira.Reconcile(ctx, opts)
	if res != nil {
		printJiraResult(stdout, res)
	}
	return err
}

// printJiraResult renders what the run found.
//
// THE SEATS COME FIRST because that is the finding an operator acts on: a
// seat with no account id receives nothing, and nothing else in the engine
// says so — its inbound routing is simply silent.
func printJiraResult(w io.Writer, res *jira.Result) {
	fmt.Fprintf(w, "\n%s instance, org account %s.\n",
		string(res.Deployment), res.Account)

	fmt.Fprintf(w, "\n%d of %d seat(s) can receive Jira events:\n",
		res.Routing(), len(res.Seats))
	for _, seat := range res.Seats {
		where := seat.Project
		if where == "" {
			where = "-"
		}
		if seat.Routes() {
			fmt.Fprintf(w, "  %-16s %-10s %s\n", seat.Handle, where, seat.Account)
			continue
		}
		fmt.Fprintf(w, "  %-16s %-10s NO ACCOUNT — %s\n", seat.Handle, where, seat.Reason)
	}

	if len(res.Projects) > 0 {
		fmt.Fprintf(w, "\n%d project(s) named by the org chart:\n", len(res.Projects))
		for _, p := range res.Projects {
			switch {
			case !p.Exists:
				fmt.Fprintf(w, "  %-10s NOT ON THIS INSTANCE — %s\n", p.Key, p.Detail)
			case p.Agrees():
				fmt.Fprintf(w, "  %-10s %s (lead %s)\n", p.Key, p.Name, p.OrgLead)
			default:
				// REPORTED, NEVER FAILED. A Jira lead who is not a seat
				// here is an ordinary arrangement — a human manager owns
				// the project and an agent triages it — so the two ideas
				// of ownership are printed side by side and the operator
				// decides whether they should agree.
				fmt.Fprintf(w, "  %-10s %s (org lead %s, Jira lead %s)\n",
					p.Key, p.Name, orDash(p.OrgLead), orDash(jiraLeadLabel(p)))
			}
		}
	}

	if res.Hooked != "" {
		fmt.Fprintf(w, "\nWebhook registered at %s.\n", res.Hooked)
	}
	printNotes(w, res.Notes)
}

func jiraLeadLabel(p jira.ProjectCheck) string {
	if p.JiraLeadHandle != "" {
		return p.JiraLeadHandle
	}
	if p.JiraLeadName != "" {
		return p.JiraLeadName
	}
	return p.JiraLead
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

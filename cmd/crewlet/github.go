package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
)

// `crewlet github provision` — the hosted code host's reconcile.
//
// # It reports more than it changes, and that is GitHub's shape
//
// The self-hosted code host's command beside this one CREATES a service
// account per seat and mints its token. GitHub offers neither: there is no
// API that creates a user, and the API that once minted a token on somebody
// else's behalf was withdrawn in 2020. A command that offered to provision
// accounts would be printing instructions dressed as actions.
//
// So it answers the question that is otherwise invisible until an event
// reaches nobody — which account each seat's credential authenticates as —
// and does the one write GitHub does allow: registering the inbound
// webhooks, on the organization where the credential may and on each named
// repository where it may not.

func runGitHubProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("github provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhooks")
	recreate := fs.Bool("recreate-webhooks", false,
		"delete and remake every webhook to mint a fresh secret; this "+
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
			"usage: crewlet github provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-recreate-webhooks] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.GitHub
	if cfg == nil {
		return errors.New(
			"github: the company config has no integrations.github block, so " +
				"there is nothing to reconcile")
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

	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		// THE ORG CREDENTIAL IS OPTIONAL FOR THE ENGINE AND REQUIRED
		// HERE, and the difference is what each one does with it. The
		// engine reads participant lists and degrades without them; this
		// command registers webhooks, and there is no degraded form of
		// that — a run with no credential could only report the seat
		// logins it resolved and leave the deployment delivering nothing.
		return fmt.Errorf(
			"github: integrations.github.token (%q) resolved empty — this run "+
				"registers the webhooks with it, and a token that can do that "+
				"needs admin on each repository (or admin:org_hook for one "+
				"organization-level hook)", cfg.Token)
	}
	client, err := github.NewClient(github.ClientOptions{
		APIBase: resolved.APIBase(), WebBase: resolved.WebURL(), Token: token,
	})
	if err != nil {
		return err
	}

	// THE SINK IS OPENED FOR A REAL RUN ONLY. A dry run reads GitHub and
	// registers nothing, so it has nothing to record — and opening a sink
	// is not free: the -env-file one CREATES the file at 0600, and the
	// -secret-store one probes the store's lock and may reach a running
	// node's API. A command that promised to touch nothing must not.
	opts := github.Options{
		Client: client, Config: cfg, Org: organization,
		Value:            env.Value,
		RecreateWebhooks: *recreate,
	}
	if *dryRun {
		fmt.Fprintln(stdout,
			"-dry-run: reading GitHub; no webhook will be registered.")
	} else {
		opts.WebhookBase = *publicURL
		sink, closeSink, openErr := sinks.open(ctx, stdout)
		if openErr != nil {
			return openErr
		}
		defer closeSink()
		opts.Sink = sink
	}

	res, err := github.Reconcile(ctx, opts)
	if res != nil {
		printGitHubResult(stdout, res)
	}
	return err
}

// printGitHubResult renders what the run found.
//
// THE SEATS COME FIRST because that is the finding an operator acts on: a
// seat with no login receives nothing, and nothing else in the engine says
// so — its inbound routing is simply silent.
func printGitHubResult(w io.Writer, res *github.Result) {
	fmt.Fprintf(w, "\nAuthenticated as %s.\n", res.Login)

	fmt.Fprintf(w, "\n%d of %d seat(s) can receive GitHub events:\n",
		res.Routing(), len(res.Seats))
	for _, seat := range res.Seats {
		if seat.Routes() {
			fmt.Fprintf(w, "  %-16s %s\n", seat.Handle, seat.Login)
			continue
		}
		fmt.Fprintf(w, "  %-16s NO ACCOUNT — %s\n", seat.Handle, seat.Reason)
	}

	if len(res.Hooks) > 0 {
		fmt.Fprintf(w, "\n%d webhook target(s):\n", len(res.Hooks))
		for _, hook := range res.Hooks {
			switch {
			case hook.Hooked() && hook.Created:
				fmt.Fprintf(w, "  %-28s registered at %s\n", hook.Target, hook.URL)
			case hook.Hooked():
				fmt.Fprintf(w, "  %-28s already pointing at %s\n", hook.Target, hook.URL)
			default:
				fmt.Fprintf(w, "  %-28s NOT HOOKED — %s\n",
					hook.Target, orDash(hook.Detail))
			}
		}
	}
	printNotes(w, res.Notes)
}

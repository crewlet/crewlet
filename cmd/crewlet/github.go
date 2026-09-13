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
		"this deployment's public base URL, for registering the webhooks; "+
			"defaults to integrations.public_base_url")
	recreate := fs.Bool("recreate-webhooks", false,
		"delete and remake every webhook to mint a fresh secret; this "+
			"invalidates the secret every other deployment of this company holds")
	dryRun := fs.Bool("dry-run", false,
		"read and report, and register nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
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
	// registers nothing, so it has nothing to record — and opening the
	// secret store to write nothing would prompt for a passphrase on a
	// command that promised to touch nothing.
	opts := github.Options{
		Client: client, Config: cfg, Org: organization,
		Value:            env.Value,
		RecreateWebhooks: *recreate,
	}
	if *dryRun {
		fmt.Fprintln(stdout,
			"-dry-run: reading GitHub; no webhook will be registered.")
	} else {
		opts.WebhookBase = webhookBase(*publicURL, &company.Integrations, env.LookupOK)
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
// seat this run could not resolve receives nothing, and nothing else in the
// engine says so — its inbound routing is simply silent.
//
// THREE OUTCOMES, NOT TWO, and the third is why this was rewritten. A seat
// that names no personal access token is not broken: on the current design it
// acts through its OWN GitHub App, and that is the shape a new company has.
// Printed as a binary "can / cannot receive events" it read as
// "0 of 3 seat(s) can receive GitHub events" over three healthy agents,
// followed by three NO ACCOUNT lines — the same false alarm that
// [github.Result.Findings] was fixed to stop reporting, relocated to the
// command line. The counts follow [github.SeatIdentity]'s own vocabulary now,
// so the two cannot drift again.
func printGitHubResult(w io.Writer, res *github.Result) {
	fmt.Fprintf(w, "\nAuthenticated as %s.\n", res.Login)

	var unclaimed, refused []github.SeatIdentity
	for _, seat := range res.Seats {
		switch {
		case seat.Routes():
		case seat.Refused():
			refused = append(refused, seat)
		default:
			unclaimed = append(unclaimed, seat)
		}
	}

	fmt.Fprintf(w, "\n%d of %d seat(s) hold a token of their own:\n",
		res.Routing(), len(res.Seats))
	for _, seat := range res.Seats {
		if seat.Routes() {
			fmt.Fprintf(w, "  %-16s %s\n", seat.Handle, seat.Login)
		}
	}
	if len(unclaimed) > 0 {
		// NOT A PROBLEM, and said in those words. Each of these acts
		// through its own app; listing them under a heading that implies
		// a fault is what this rewrite removes.
		fmt.Fprintf(w, "\n%d seat(s) act through their own GitHub App:\n", len(unclaimed))
		for _, seat := range unclaimed {
			fmt.Fprintf(w, "  %-16s no personal access token, which is expected\n", seat.Handle)
		}
	}
	if len(refused) > 0 {
		fmt.Fprintf(w, "\n%d seat(s) name a credential this run could not resolve:\n", len(refused))
		for _, seat := range refused {
			fmt.Fprintf(w, "  %-16s REFUSED — %s\n", seat.Handle, seat.Reason)
		}
	}

	if len(res.Hooks) > 0 {
		fmt.Fprintf(w, "\n%d webhook target(s):\n", len(res.Hooks))
		for _, hook := range res.Hooks {
			switch {
			case hook.Hooked() && hook.Created:
				fmt.Fprintf(w, "  %-28s registered at %s\n", hook.Target, hook.URL)
			case hook.Hooked():
				fmt.Fprintf(w, "  %-28s already pointing at %s\n", hook.Target, hook.URL)
			case !hook.Blocks():
				// NOTHING TO HOOK, not a refusal. Printed as NOT HOOKED
				// it reads as a problem and sends somebody to fix a
				// repository that is finished.
				fmt.Fprintf(w, "  %-28s skipped — %s\n",
					hook.Target, orDash(hook.Detail))
			default:
				fmt.Fprintf(w, "  %-28s NOT HOOKED — %s\n",
					hook.Target, orDash(hook.Detail))
			}
		}
	}
	printNotes(w, res.Notes)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
)

// `crewlet confluence import` — publishing authored markdown into spaces.
//
// The knowledge base's one write command, and it writes only what an
// operator asked it to: a directory tree of markdown, one space per
// directory, plus the tool skills the files themselves declare.

func runConfluenceImport(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)
	directory, args := splitSubject(args)

	fs := flag.NewFlagSet("confluence import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := bootstrapFlag(fs)
	space := fs.String("space", "",
		"the space tool-skill pages are published into; empty reads "+
			"CREWLET_TOOL_SKILLS_SPACE, then knowledge.skills_container")
	prune := fs.Bool("prune", false,
		"delete published tool-skill pages whose key no local file publishes")
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, directory, given := twoPositionals(fs, companyPath, directory)
	if given != 2 {
		fmt.Fprintln(stderr,
			"usage: crewlet confluence import <company.yaml> <directory> "+
				"[-space KEY] [-prune] [-dry-run]")
		return errors.New("name exactly one company document and one directory")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Confluence
	if cfg == nil {
		return errors.New(
			"confluence: the company config has no integrations.confluence " +
				"block, so there is no instance to publish into")
	}
	skillsSpace := skillsContainer(*space, "CREWLET_TOOL_SKILLS_SPACE", company.SkillsContainerKey())
	plan, err := confluence.Walk(directory, skillsSpace)
	if err != nil {
		return err
	}
	printConfluencePlan(stdout, plan)
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was written or deleted.")
		return nil
	}
	if len(plan.Items) == 0 {
		return nil
	}

	ctx := context.Background()
	env, closeEnv, err := companyResolver(ctx, *bootstrapPath, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

	resolved := config.Confluence{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: resolved.BaseURL(), Email: env.Value(cfg.Email),
		Token: env.Value(cfg.Token),
	})
	if err != nil {
		return err
	}
	res, err := confluence.Publish(ctx, confluence.PublishOptions{
		Client: client, Plan: plan, Prune: *prune, SkillsSpace: skillsSpace,
	})
	if res != nil {
		printConfluenceResult(stdout, res)
	}
	if err != nil {
		return err
	}
	if len(res.Failed) > 0 {
		// NON-ZERO, because a run that published forty pages and could
		// not publish two is not a success — and the two are exactly the
		// pages an agent will later fail to find.
		return fmt.Errorf("confluence: %d page(s) could not be written", len(res.Failed))
	}
	return nil
}

func printConfluencePlan(w io.Writer, plan *confluence.Plan) {
	if len(plan.Items) == 0 {
		printNotes(w, plan.Notes)
		return
	}
	fmt.Fprintf(w, "%d page(s) across %d space(s):\n",
		len(plan.Items), len(plan.Spaces()))
	for _, item := range plan.Items {
		kind := "doc"
		if item.Skill {
			kind = "skill"
		}
		fmt.Fprintf(w, "  %-6s %-10s %s\n", kind, item.Space, item.Title)
	}
	printNotes(w, plan.Notes)
}

func printConfluenceResult(w io.Writer, res *confluence.PublishResult) {
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "\nCreated %d page(s):\n  %s\n",
			len(res.Created), strings.Join(res.Created, "\n  "))
	}
	if len(res.Updated) > 0 {
		fmt.Fprintf(w, "Updated %d page(s):\n  %s\n",
			len(res.Updated), strings.Join(res.Updated, "\n  "))
	}
	if len(res.Pruned) > 0 {
		fmt.Fprintf(w, "Deleted %d orphaned skill page(s):\n  %s\n",
			len(res.Pruned), strings.Join(res.Pruned, "\n  "))
	}
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, "\n%d page(s) FAILED:\n  %s\n",
			len(res.Failed), strings.Join(res.Failed, "\n  "))
	}
	printNotes(w, res.Notes)
}

// `crewlet confluence resync` — the knowledge base's read-only diagnostic.
//
// It exists because the tool-skill registry is populated by a full walk of
// one space at boot, and a skill that does not load is INVISIBLE — the only
// symptom is guidance that never appears in a Plan prompt. This runs the engine's own
// walk against a THROWAWAY registry and prints what it found, so an operator
// can see what the next boot will see without restarting anything.
//
// It deliberately does NOT reach into a running engine. Applying a change
// there is the engine's own job, on the next boot or the next webhook.
//
// SKILLS-ONLY, and that is not an omission: knowledge docs are searched live
// at query time and never loaded into a registry, so for them there is
// nothing to resync.
func runConfluenceResync(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("confluence resync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	space := fs.String("space", "",
		"the skills space key; empty takes knowledge.skills_container")
	bootstrapPath := bootstrapFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet confluence resync <company.yaml> [-space KEY]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Confluence
	if cfg == nil {
		return errors.New(
			"confluence: the company config has no integrations.confluence " +
				"block, so there is no instance to read")
	}

	ctx := context.Background()
	env, closeEnv, err := companyResolver(ctx, *bootstrapPath, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

	resolved := config.Confluence{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: resolved.BaseURL(), Email: env.Value(cfg.Email),
		Token: env.Value(cfg.Token),
	})
	if err != nil {
		return err
	}

	key := skillsContainer(*space, "CREWLET_TOOL_SKILLS_SPACE", company.SkillsContainerKey())
	if key == "" {
		return errors.New(
			"confluence: this company has no tool-skills container — " +
				"knowledge.skills_container is set to the empty string, which " +
				"turns tool skills off. Pass -space KEY to read one anyway, " +
				"or remove that setting")
	}
	pages, err := confluence.SkillPages(ctx, client, key)
	if err != nil {
		return err
	}
	loaded, report := skills.Admit(pages)
	fmt.Fprintf(stdout, "%s holds %d page(s): %d skill(s), %d ordinary page(s).\n",
		key, report.Pages, len(loaded), report.Ordinary)
	for _, skill := range loaded {
		fmt.Fprintf(stdout, "  %-28s %s\n", skill.Key, skill.Title)
	}
	if len(report.Undecodable) > 0 {
		// A PAGE THAT LOOKS LIKE A SKILL AND DOES NOT PARSE is the case
		// worth printing: somebody wrote a trigger and got the rest wrong,
		// and the only other symptom is guidance that never appears.
		fmt.Fprintf(stdout, "\n%d page(s) declare a trigger and did not parse:\n",
			len(report.Undecodable))
		for _, title := range report.Undecodable {
			fmt.Fprintf(stdout, "  - %s\n", title)
		}
		return fmt.Errorf("%d page(s) could not be decoded", len(report.Undecodable))
	}
	return nil
}

// `crewlet confluence provision` — registering the inbound hooks.
//
// The knowledge base's second write command, and the only one that writes to
// the instance's administration rather than to its content. On Data Center
// it registers one signed hook; on Cloud one token-bearing hook per event,
// because Cloud signs nothing and names no event in what it delivers.

func runConfluenceProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("confluence provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the hooks; "+
			"defaults to integrations.public_base_url")
	recreate := fs.Bool("recreate-webhooks", false,
		"delete and remake every hook to mint a fresh token or secret; this "+
			"invalidates the value every other deployment of this company holds")
	dryRun := fs.Bool("dry-run", false,
		"read and report, and register nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet confluence provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-recreate-webhooks] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Confluence
	if cfg == nil {
		return errors.New(
			"confluence: the company config has no integrations.confluence " +
				"block, so there is no instance to reconcile")
	}

	ctx := context.Background()
	env, closeEnv, err := companyResolver(ctx, *sinks.bootstrap, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

	resolved := config.Confluence{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	base := resolved.BaseURL()
	if base == "" {
		return fmt.Errorf(
			"confluence: neither integrations.confluence.url (%q) nor cloud_id "+
				"(%q) resolved to anything", cfg.URL, cfg.CloudID)
	}
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return fmt.Errorf(
			"confluence: integrations.confluence.token (%q) resolved empty; the "+
				"org account is what this run reads the instance with", cfg.Token)
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return err
	}

	// THE SINK IS OPENED FOR A REAL RUN ONLY, for the reason every sibling
	// states: a dry run registers nothing and has nothing to record.
	opts := confluence.Options{
		Client: client, Config: cfg, Value: env.Value, Recreate: *recreate,
	}
	if *dryRun {
		fmt.Fprintln(stdout,
			"-dry-run: reading the instance; no hook will be registered.")
	} else {
		opts.WebhookBase = webhookBase(*publicURL, &company.Integrations, env.LookupOK)
		sink, closeSink, openErr := sinks.open(ctx, stdout)
		if openErr != nil {
			return openErr
		}
		defer closeSink()
		opts.Sink = sink
	}

	res, err := confluence.Reconcile(ctx, opts)
	if res != nil {
		printConfluenceHooks(stdout, res)
	}
	return err
}

// printConfluenceHooks renders what the run found.
func printConfluenceHooks(w io.Writer, res *confluence.Result) {
	fmt.Fprintf(w, "\n%s instance, org account %s.\n", string(res.Deployment), res.Account)
	if len(res.Hooks) > 0 {
		fmt.Fprintf(w, "\n%d hook(s):\n", len(res.Hooks))
		for _, hook := range res.Hooks {
			state := "converged"
			switch {
			case !hook.Hooked():
				// A REACHABLE BRANCH NOW. Every failure used to return
				// an error, so the reconcile never yielded a hook
				// without a URL and this printed nothing ever; a refused
				// event is recorded and the walk continues, so one
				// bad event no longer hides the other seven. orDash for
				// the same reason the GitHub printer has it: an empty
				// detail would render as a dangling colon.
				state = "NOT registered: " + orDash(hook.Detail)
			case hook.Created:
				state = "created"
			}
			// THE TOKEN IS NOT PRINTED. The Cloud target carries it in the
			// query, and a run's stdout is the one place an operator pastes
			// into a ticket. The path is what tells them where it points.
			fmt.Fprintf(w, "  %-16s %s\n", hook.Event, redactQuery(hook.URL)+"  "+state)
		}
	}
	if len(res.Notes) > 0 {
		fmt.Fprintf(w, "\n%d note(s):\n", len(res.Notes))
		for _, note := range res.Notes {
			fmt.Fprintf(w, "  - %s\n", note)
		}
	}
}

// redactQuery drops a URL's query string, which on the Cloud route is the
// credential.
func redactQuery(raw string) string {
	if i := strings.Index(raw, "?"); i >= 0 {
		return raw[:i] + "?token=…"
	}
	return raw
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

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
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	for companyPath == "" || directory == "" {
		if len(tail) == 0 {
			break
		}
		if companyPath == "" {
			companyPath, tail = tail[0], tail[1:]
			continue
		}
		directory, tail = tail[0], tail[1:]
	}
	if companyPath == "" || directory == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet confluence import <company.yaml> <directory> [-dry-run]")
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
	plan, err := confluence.Walk(directory, cfg.SkillsSpaceKey())
	if err != nil {
		return err
	}
	printConfluencePlan(stdout, plan)
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was written.")
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
	res, err := confluence.Publish(ctx, confluence.PublishOptions{Client: client, Plan: plan})
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
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, "\n%d page(s) FAILED:\n  %s\n",
			len(res.Failed), strings.Join(res.Failed, "\n  "))
	}
	printNotes(w, res.Notes)
}

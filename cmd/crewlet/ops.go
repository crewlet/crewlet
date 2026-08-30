package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// The two operator commands that read and write the store directly.
//
// Both exist because a node does these things implicitly and a deployment
// sometimes needs them explicitly: migrations run at every open, and budget
// counters are written by every turn — so neither is a gap in the engine.
// What they are is a way to do them WITHOUT starting one.

// runMigrate is `crewlet migrate`.
//
// # A node migrates on its own, so why this exists
//
// Rolling out N nodes at once means N processes opening the same database
// and racing to apply the same files. That race is safe — one transaction
// per file, the version row written inside it — but it is not what an
// operator wants to watch during a deploy, and a failure mid-rollout is a
// fleet in two schema states. Migrating once, deliberately, before anything
// starts, makes the outcome one thing that either worked or did not.
func runMigrate(args []string, stdout, stderr io.Writer) error {
	bootstrapPath, args := splitSubject(args)

	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store")
	check := fs.Bool("check", false,
		"report what is pending and apply nothing; exits non-zero when there is any")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if tail := fs.Args(); bootstrapPath == "" && len(tail) == 1 {
		bootstrapPath = tail[0]
	} else if len(tail) > 0 && bootstrapPath != "" {
		fmt.Fprintln(stderr, "usage: crewlet migrate [<config.yaml>] [-check]")
		return errors.New("name at most one config document")
	}
	if bootstrapPath == "" {
		bootstrapPath = *configPath
	}

	// TIER A RESOLVES FROM THE ENVIRONMENT ALONE — this file carries the
	// store's address, so a resolver reaching the store would have Tier A
	// reading from the thing it describes.
	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return err
	}
	opts := store.Options{
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout: time.Duration(
			boot.Store.BusyTimeoutSeconds * float64(time.Second)),
	}
	ctx := context.Background()

	applied, pending, err := store.Pending(ctx, boot.Store.Path, opts)
	if err != nil {
		// READING IS ALSO A SECOND PROCESS ON THE FILE, and until Pending
		// took the lock this was the one path that did not say so: -check
		// reported the schema of a live engine's database, and only the
		// apply below was refused. The remedy differs from the apply's,
		// though — nothing here would change the file, so an operator
		// wanting the answer has a route that does not involve stopping
		// the company.
		return engineHoldsTheStore(err, bootstrapPath,
			"Stop `crewlet run` on this node and re-run. A running engine has "+
				"already applied every migration this binary carries, so a node "+
				"that is up is a node with nothing pending.")
	}
	if *check {
		fmt.Fprintf(stdout, "%s: %d applied, %d pending\n",
			boot.Store.Path, len(applied), len(pending))
		for _, file := range pending {
			fmt.Fprintf(stdout, "  pending  %s\n", file)
		}
		if len(pending) > 0 {
			// NON-ZERO, because this is what a deploy gate calls: a
			// command that reported pending work and exited 0 would be
			// a gate that never stops anything.
			return fmt.Errorf("%d migration(s) pending", len(pending))
		}
		return nil
	}
	if len(pending) == 0 {
		fmt.Fprintf(stdout, "%s is up to date (%d migration(s) applied).\n",
			boot.Store.Path, len(applied))
		return nil
	}

	// OPENING IS WHAT MIGRATES. There is deliberately no second code path
	// that applies files: a migrator the engine does not use is one that
	// can disagree with it about what "applied" means.
	db, err := store.Open(ctx, boot.Store.Path, opts)
	if err != nil {
		// NO ROUTE AROUND THIS ONE, and that is correct: migrating the
		// schema under a live engine is what the lock exists to prevent,
		// not an inconvenience it imposes. The remedy is the only one.
		return engineHoldsTheStore(err, bootstrapPath,
			"Stop `crewlet run` on this node and re-run — a schema change "+
				"under a live engine is exactly what the lock prevents.")
	}
	defer func() { _ = db.Close() }()

	now, err := db.AppliedMigrations(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: applied %d migration(s).\n",
		boot.Store.Path, len(now)-len(applied))
	for _, file := range pending {
		fmt.Fprintf(stdout, "  applied  %s\n", file)
	}
	return nil
}

// runBudgets is `crewlet budgets`.
//
// # Why this talks to a NODE and not to a file
//
// The token counter is FLEET state: it lives in the coordination store so
// that a company's cap is one number rather than one per node. On the default
// topology that store is the engine's own embedded broker, which means there
// is nothing on disk this command could open — and, worse, that opening it
// anyway would be dangerous rather than merely useless: a second JetStream
// server on the same store directory is ACCEPTED rather than refused
// (measured), so two writers would corrupt the counter instead of contending
// for it.
//
// So `show` reads the same answer the dashboard renders, and `reset` posts to
// the one route that writes it. Both take the running node's address, which
// defaults to what this node's own Tier A config says it binds — so the
// common case is still `crewlet budgets show` beside the config file.
func runBudgets(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "show":
		return budgetsShow(rest, stdout, stderr)
	case "reset":
		return budgetsReset(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr,
			"usage: crewlet budgets show|reset [<config.yaml>] [-url] [-token] [-scope]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown budgets command %q", sub)
	}
}

// budgetsShow prints what each scope has spent.
func budgetsShow(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "budgets show", stderr, nil)
	if err != nil {
		return err
	}
	var answer struct {
		Durable bool `json:"durable"`
		Org     struct {
			MaxTokens        int    `json:"max_tokens"`
			DurableUsed      int    `json:"durable_used"`
			DurableUpdatedAt string `json:"durable_updated_at"`
		} `json:"org"`
		Seats []struct {
			Handle           string `json:"handle"`
			AgentID          string `json:"agent_id"`
			MaxTokens        int    `json:"max_tokens"`
			DurableUsed      int    `json:"durable_used"`
			DurableUpdatedAt string `json:"durable_updated_at"`
		} `json:"seats"`
	}
	if err := client.get(context.Background(), "/query/budgets", &answer); err != nil {
		return err
	}
	if !answer.Durable {
		// NOT ZERO. "Nobody could look" and "nothing was spent" are
		// different facts and only one of them is a measurement — the
		// same distinction the query surface draws with this flag.
		return errors.New("the node could not read the counter; " +
			"its `durable` flag is false, so nothing here can be stated")
	}
	fmt.Fprintf(stdout, "%-32s %12s %12s  %s\n", "SCOPE", "USED", "CAP", "LAST CHARGED")
	fmt.Fprintf(stdout, "%-32s %12d %12s  %s\n", "org", answer.Org.DurableUsed,
		capOrDash(answer.Org.MaxTokens), dashIfEmpty(answer.Org.DurableUpdatedAt))
	for _, seat := range answer.Seats {
		if seat.DurableUsed == 0 && seat.MaxTokens == 0 {
			// A seat that has spent nothing under no cap has nothing
			// to report, and printing a permanent zero for every
			// seat in a large company buries the ones that matter.
			continue
		}
		fmt.Fprintf(stdout, "%-32s %12d %12s  %s\n",
			seat.Handle, seat.DurableUsed, capOrDash(seat.MaxTokens),
			dashIfEmpty(seat.DurableUpdatedAt))
	}
	return nil
}

func capOrDash(limit int) string {
	if limit <= 0 {
		// `token_budget: 0` is how an operator says "no ceiling", so a
		// literal 0 in this column would read as the opposite.
		return "unlimited"
	}
	return strconv.Itoa(limit)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// budgetsReset zeroes the counters.
//
// # It is never a schedule
//
// A budget is a ceiling for the life of a deployment, and a counter that
// rolled itself over would silently re-arm a company somebody had stopped on
// purpose. So this is an operator action, and it names what it cleared.
func budgetsReset(args []string, stdout, stderr io.Writer) error {
	var scope *string
	client, err := nodeClientFor(args, "budgets reset", stderr, func(fs *flag.FlagSet) {
		scope = fs.String("scope", "",
			"reset only this scope (org, or agent:<id>); empty resets every scope")
	})
	if err != nil {
		return err
	}
	path := "/budgets/reset"
	if *scope != "" {
		path += "?scope=" + url.QueryEscape(*scope)
	}
	var answer struct {
		Cleared int      `json:"cleared"`
		Scopes  []string `json:"scopes"`
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	if answer.Cleared == 0 {
		fmt.Fprintln(stdout, "Nothing to reset: no counter matched.")
		return nil
	}
	// The report NAMES what was cleared. A count alone leaves an operator
	// unable to tell "reset the seat I meant" from "reset a scope that was
	// already empty".
	sort.Strings(answer.Scopes)
	fmt.Fprintf(stdout, "Reset %d scope(s): %s\n",
		answer.Cleared, strings.Join(answer.Scopes, ", "))
	return nil
}

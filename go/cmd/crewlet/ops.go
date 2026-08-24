package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
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
		Driver:       store.Driver(boot.Store.Driver),
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout: time.Duration(
			boot.Store.BusyTimeoutSeconds * float64(time.Second)),
	}
	ctx := context.Background()

	applied, pending, err := store.Pending(ctx, boot.Store.Path, opts)
	if err != nil {
		return err
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
		return err
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
func runBudgets(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "show":
		return budgetsShow(rest, stdout, stderr)
	case "reset":
		return budgetsReset(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet budgets show|reset [<config.yaml>]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown budgets command %q", sub)
	}
}

// budgetsShow prints what each scope has spent.
func budgetsShow(args []string, stdout, stderr io.Writer) error {
	db, close, err := openStoreFor(args, "budgets show", stderr, nil)
	if err != nil {
		return err
	}
	defer close()

	usage, err := db.Budgets().List(context.Background())
	if err != nil {
		return err
	}
	if len(usage) == 0 {
		// NOT ZERO. A company that has spent nothing and a company whose
		// counters were reset are the same row-less state, and printing
		// "0" for scopes that do not exist would invent seats.
		fmt.Fprintln(stdout, "No scope has spent anything yet.")
		return nil
	}
	fmt.Fprintf(stdout, "%-32s %12s  %s\n", "SCOPE", "USED", "LAST CHARGED")
	for _, u := range usage {
		fmt.Fprintf(stdout, "%-32s %12d  %s\n",
			u.Scope, u.Used, u.UpdatedAt.UTC().Format(time.RFC3339))
	}
	return nil
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
	db, close, err := openStoreFor(args, "budgets reset", stderr, func(fs *flag.FlagSet) {
		scope = fs.String("scope", "",
			"reset only this scope (org, or agent:<id>); empty resets every scope")
	})
	if err != nil {
		return err
	}
	defer close()

	ctx := context.Background()
	// READ FIRST, so the report names what was cleared. A count alone
	// leaves an operator unable to tell "reset the seat I meant" from
	// "reset a scope that was already empty".
	before, err := db.Budgets().List(ctx)
	if err != nil {
		return err
	}
	cleared := clearedScopes(before, *scope)

	n, err := db.Budgets().Reset(ctx, *scope)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(stdout, "Nothing to reset: no counter matched.")
		return nil
	}
	fmt.Fprintf(stdout, "Reset %d scope(s): %s\n", n, strings.Join(cleared, ", "))
	return nil
}

// clearedScopes names the counters a reset will remove.
func clearedScopes(usage []store.Usage, scope string) []string {
	var out []string
	for _, u := range usage {
		if scope == "" || u.Scope == scope {
			out = append(out, u.Scope)
		}
	}
	sort.Strings(out)
	return out
}

// openStoreFor is the shared "one config argument, then open the store"
// preamble the operator commands share.
func openStoreFor(args []string, name string, stderr io.Writer, extra func(*flag.FlagSet)) (*store.DB, func(), error) {
	bootstrapPath, args := splitSubject(args)

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if tail := fs.Args(); bootstrapPath == "" && len(tail) == 1 {
		bootstrapPath = tail[0]
	} else if len(tail) > 0 {
		fmt.Fprintf(stderr, "usage: crewlet %s [<config.yaml>]\n", name)
		return nil, nil, errors.New("name at most one config document")
	}
	if bootstrapPath == "" {
		bootstrapPath = *configPath
	}

	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(context.Background(), boot.Store.Path, store.Options{
		Driver:       store.Driver(boot.Store.Driver),
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout: time.Duration(
			boot.Store.BusyTimeoutSeconds * float64(time.Second)),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

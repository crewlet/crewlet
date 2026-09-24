package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/store"
)

// Two operator commands for state a node also keeps on its own: `migrate`,
// which applies the store's schema without starting a node, and `budgets`,
// which reads and resets the fleet's token counter through a running one.

// runMigrate is `crewlet migrate`.
//
// # A node migrates on its own, so why this exists
//
// Every process that opens a node's store migrates both of its files, so this
// is never required. What it adds is the migration as a step of its own,
// before the node starts and with its own exit status, and `-check`: the
// deploy gate that reports what this binary would apply, and applies nothing.
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
	// THE SAME SHAPE AS `run`: the document is named once. `crewlet
	// migrate a.yaml b.yaml` puts both names in the tail with no subject,
	// and resolving that to either of them — or to neither, and so to
	// ./crewlet.yaml — would migrate a store the operator did not mean,
	// for real unless -check was given. So a count past one is refused.
	bootstrapPath, given := onePositional(fs, bootstrapPath)
	if given > 1 {
		fmt.Fprintln(stderr, "usage: crewlet migrate [<config.yaml>] [-check]")
		return errors.New("name at most one config document")
	}
	if bootstrapPath != "" {
		if isFlagSet(fs, "config") {
			return errors.New(
				"the config document is named twice, as a positional argument " +
					"and as -config; they would have to agree and nothing " +
					"checks that they do")
		}
	} else {
		bootstrapPath = *configPath
	}

	// TIER A RESOLVES FROM THE ENVIRONMENT ALONE — this file carries the
	// store's address, so a resolver reaching the store would have Tier A
	// reading from the thing it describes.
	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return err
	}
	opts := engine.StoreOptions(boot)
	ctx := context.Background()

	schemas, err := store.Pending(ctx, boot.Store.Path, opts)
	if err != nil {
		// READING IS ALSO A SECOND PROCESS ON THE FILE. -check applies
		// nothing, but it reads the store's files, and the lock admits one
		// process at a time — so against a running engine the remedy is the
		// apply's: stop it, and run the check again.
		return engineHoldsTheStore(err, bootstrapPath,
			"Stop `crewlet run` on this node and re-run the check: it reads "+
				"the store's files, which the running engine holds.")
	}
	// BOTH ESTATES, ALWAYS BOTH. A node is two databases with two
	// independent sequences, and a report that named one of them would be
	// a deploy gate that passes while the other is behind.
	var waiting int
	for _, sch := range schemas {
		waiting += len(sch.Pending)
	}
	if *check {
		for _, sch := range schemas {
			fmt.Fprintf(stdout, "%s (%s): %d applied, %d pending\n",
				sch.Path, sch.Estate, len(sch.Applied), len(sch.Pending))
			for _, file := range sch.Pending {
				fmt.Fprintf(stdout, "  pending  %s\n", file)
			}
		}
		if waiting > 0 {
			// NON-ZERO, because this is what a deploy gate calls: a
			// command that reported pending work and exited 0 would be
			// a gate that never stops anything.
			return fmt.Errorf("%d migration(s) pending", waiting)
		}
		return nil
	}
	if waiting == 0 {
		for _, sch := range schemas {
			fmt.Fprintf(stdout, "%s (%s) is up to date (%d migration(s) applied).\n",
				sch.Path, sch.Estate, len(sch.Applied))
		}
		return nil
	}

	// OPENING IS WHAT MIGRATES. There is deliberately no second code path
	// that applies files: a migrator the engine does not use is one that
	// can disagree with it about what "applied" means. One Open brings up
	// both estates, so both sequences run here.
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

	for _, sch := range schemas {
		handle := db
		if sch.Estate == store.EstateReplicated {
			handle = db.Replicated()
		}
		now, err := handle.AppliedMigrations(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s (%s): applied %d migration(s).\n",
			sch.Path, sch.Estate, len(now)-len(sch.Applied))
		for _, file := range sch.Pending {
			fmt.Fprintf(stdout, "  applied  %s\n", file)
		}
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
			RefusedAt        string `json:"refused_at"`
		} `json:"org"`
		Seats []struct {
			Handle           string `json:"handle"`
			AgentID          string `json:"agent_id"`
			MaxTokens        int    `json:"max_tokens"`
			DurableUsed      int    `json:"durable_used"`
			DurableUpdatedAt string `json:"durable_updated_at"`
			RefusedAt        string `json:"refused_at"`
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
	// USED AGAINST CAP says whether a scope is out: the engine sends no
	// round for a scope at or past its cap, and it counts a round the cap
	// refused, so a refusal leaves USED past CAP. LAST REFUSED says when the
	// cap last turned a round away, which the two numbers cannot — and it is
	// a date, not a state: it stands until the scope next admits a charge,
	// so after a revision raises the cap it is still set on a scope with
	// room.
	const row = "%-32s %12s %12s  %-30s  %s\n"
	fmt.Fprintf(stdout, row, "SCOPE", "USED", "CAP", "LAST CHARGED", "LAST REFUSED")
	fmt.Fprintf(stdout, row, "org", strconv.Itoa(answer.Org.DurableUsed),
		capOrDash(answer.Org.MaxTokens), dashIfEmpty(answer.Org.DurableUpdatedAt),
		dashIfEmpty(answer.Org.RefusedAt))
	for _, seat := range answer.Seats {
		if seat.DurableUsed == 0 && seat.MaxTokens == 0 {
			// A seat that has spent nothing under no cap has nothing
			// to report, and printing a permanent zero for every
			// seat in a large company buries the ones that matter.
			continue
		}
		fmt.Fprintf(stdout, row,
			seat.Handle, strconv.Itoa(seat.DurableUsed), capOrDash(seat.MaxTokens),
			dashIfEmpty(seat.DurableUpdatedAt), dashIfEmpty(seat.RefusedAt))
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
	slices.Sort(answer.Scopes)
	fmt.Fprintf(stdout, "Reset %d scope(s): %s\n",
		answer.Cleared, strings.Join(answer.Scopes, ", "))
	return nil
}

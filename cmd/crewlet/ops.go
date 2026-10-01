package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// Two operator commands for what a node otherwise does implicitly: migrations
// run at every open, and the budget counters are charged by every turn — so
// neither is a gap in the engine. `migrate` applies the store's schema WITHOUT
// starting one; `budgets` reads the fleet's counters, which live in no file,
// through the node that is running.

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
	// THE SAME SHAPE AS `run`, because the failure is the same and worse
	// here. The refusal used to be conjoined with `bootstrapPath != ""`,
	// which made it unreachable on the exact input it was written for:
	// `crewlet migrate a.yaml b.yaml` puts both names in the tail with no
	// subject, so neither branch fired and the command silently migrated
	// the database named by ./crewlet.yaml — a database the operator never
	// named, and without -check it migrates it for real.
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
	opts := store.Options{
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout:  boot.Store.BusyTimeout(),
	}
	ctx := context.Background()

	schemas, err := store.Pending(ctx, boot.Store.Path, opts)
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
// The token counters are FLEET state: they live in the coordination store so
// that a company's cap is one number rather than one per node. On the default
// topology that store is the engine's own embedded broker, which means there
// is nothing on disk this command could open — and, worse, that opening it
// anyway would be dangerous rather than merely useless: a second JetStream
// server on the same store directory is ACCEPTED rather than refused
// (measured), so two writers would corrupt the counters instead of contending
// for them.
//
// So `show` reads the same answer the dashboard renders. It takes the running
// node's address, which defaults to what this node's own Tier A config says it
// binds — so the common case is still `crewlet budgets show` beside the config
// file.
//
// There is no `reset`, and no route for one (ADR-0019). Each ceiling is per
// calendar window, and a window's allowance comes back when the window turns
// over; room before then is made by raising the ceiling, which is a config
// change like any other.
func runBudgets(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "show":
		return budgetsShow(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr,
			"usage: crewlet budgets show [<config.yaml>] [-url] [-token]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown budgets command %q", sub)
	}
}

// budgetWindow is one calendar window of a scope, as the budgets answer states
// it.
type budgetWindow struct {
	Period    string `json:"period"`
	Window    string `json:"window"`
	ResetsAt  string `json:"resets_at"`
	Used      int    `json:"used"`
	Limit     *int   `json:"limit"`
	RefusedAt string `json:"refused_at"`
	State     string `json:"state"`
}

// budgetsShow prints what each scope has spent, window by window.
func budgetsShow(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "budgets show", stderr, nil)
	if err != nil {
		return err
	}
	var answer struct {
		Timezone string `json:"timezone"`
		Durable  bool   `json:"durable"`
		Org      struct {
			Windows []budgetWindow `json:"windows"`
		} `json:"org"`
		Seats []struct {
			Handle  string         `json:"handle"`
			Windows []budgetWindow `json:"windows"`
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
	// THE CLOCK FIRST: every window below is cut on it, and a reader in
	// another zone would otherwise read "2026-09-23" as their own day.
	fmt.Fprintf(stdout, "Windows on the company clock: %s\n\n", dashIfEmpty(answer.Timezone))
	// STATE is the engine's own judgement (ok, near, refusing), and
	// REFUSING SINCE is the gate's record of saying no: a refused charge
	// increments nothing, so a seat charged in rounds stalls short of its
	// ceiling and USED against LIMIT alone would read as headroom.
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCOPE\tPERIOD\tWINDOW\tUSED\tLIMIT\tSTATE\tRESETS AT\tREFUSING SINCE")
	rows := func(scope string, windows []budgetWindow) {
		for _, win := range windows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				scope, win.Period, win.Window, win.Used, limitOrUnlimited(win.Limit),
				win.State, dashIfEmpty(win.ResetsAt), dashIfEmpty(win.RefusedAt))
		}
	}
	rows("org", answer.Org.Windows)
	for _, seat := range answer.Seats {
		if !worthPrinting(seat.Windows) {
			// A seat that has spent nothing under no ceiling has
			// nothing to report, and printing three permanent zeroes
			// for every seat in a large company buries the ones that
			// matter.
			continue
		}
		rows(seat.Handle, seat.Windows)
	}
	return w.Flush()
}

// worthPrinting reports whether a seat has spent anything or is capped in any
// window.
func worthPrinting(windows []budgetWindow) bool {
	for _, win := range windows {
		if win.Used > 0 || win.Limit != nil {
			return true
		}
	}
	return false
}

// limitOrUnlimited is a window's ceiling, or "unlimited" where the answer
// states none: an uncapped window carries no `limit` at all, and a blank or a
// 0 in this column would read as the opposite of what it means.
func limitOrUnlimited(limit *int) string {
	if limit == nil {
		return "unlimited"
	}
	return strconv.Itoa(*limit)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

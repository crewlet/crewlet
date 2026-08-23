// Command crewlet is the engine binary.
//
// One binary is the whole product: it runs agents, serves the API and
// dashboard, and — in the default solo topology — embeds its own broker and
// database, so a company runs with no external services at all. What a node
// does is a config value (node.roles), not a different command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "crewlet:", err)
		os.Exit(1)
	}
}

// run is main's testable body: it takes its arguments and streams rather
// than reaching for globals, so the CLI surface can be exercised in tests.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "crewlet %s", version.String())
		if rev := version.Revision(); rev != "" {
			fmt.Fprintf(stdout, " (%s)", rev)
		}
		fmt.Fprintln(stdout)
		return nil
	case "help", "--help", "-h":
		usage(stdout)
		return flag.ErrHelp
	case "run":
		return runEngine(rest, stderr)
	case "validate":
		return validateConfigs(rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s — an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
  crewlet validate [flags]    Check both config tiers without starting anything
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q)
`, version.String(), defaultBootstrapPath, defaultCompanyPath)
}

// The two tiers are separate files because they answer to separate people. A
// node's broker address and store path are an operator's; the org chart and
// the model behind each seat are the company's.
//
// Tier B is read from a FILE here, which is not where it will finally live: a
// company config is versioned in the store and activated by epoch, and the
// engine is meant to read the active revision rather than a path. That needs
// the config plane — revisions, the activation pointer, per-node apply status
// — which is a separate piece of work. Until it lands, the file IS the active
// revision, and -company becomes the import path rather than the run path when
// it does.
const (
	defaultBootstrapPath = "crewlet.yaml"
	defaultCompanyPath   = "company.yaml"
)

// configFlags is the pair every config-reading command takes.
type configFlags struct {
	bootstrap *string
	company   *string
}

func addConfigFlags(fs *flag.FlagSet) configFlags {
	return configFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's broker, store and API"),
		company: fs.String("company", defaultCompanyPath,
			"Tier B config: the company's org, providers and integrations"),
	}
}

// load reads both tiers, reporting EVERY problem it can rather than the first.
//
// Both, not just the first to fail: an operator fixing a broker URL only to be
// told about their org chart on the next boot has been made to pay twice for
// one edit. It is the same rule each tier's own validator follows internally.
func (c configFlags) load() (*config.Bootstrap, *config.Company, error) {
	// Environment-only resolution for Tier A, which is not a default but a
	// rule: Tier A carries the store's address and the keys that open it,
	// so a resolver reaching the secret store would have Tier A reading
	// from the store it is describing.
	boot, bootErr := config.LoadBootstrap(*c.bootstrap, config.EnvOnly())
	company, companyErr := config.LoadCompany(*c.company)
	if err := errors.Join(bootErr, companyErr); err != nil {
		return nil, nil, err
	}
	return boot, company, nil
}

func validateConfigs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}
	// Building the epoch is the rest of the check, and it reaches nothing:
	// no broker, no store, no provider is dialled. It is what catches the
	// problems a schema cannot — a seat whose llm names no configured
	// provider, a role reporting to a unit that does not exist.
	epoch, err := engine.NewCompany(company)
	if err != nil {
		return err
	}
	// Both slot types are read straight off the config: ParseBootstrap
	// decodes over DefaultBootstrap, so a file that names neither comes
	// back carrying the defaults rather than empty strings. A helper here
	// naming them again would be a second copy of the defaults, and the
	// two would eventually disagree about what an unset slot does.
	fmt.Fprintf(stdout, "%s: %d agent seats, %d LLM providers, stream %q, coordination %q\n",
		company.Name, len(epoch.Seats()), len(epoch.Models.Keys()),
		boot.Stream.Type, boot.Coordination.Type)
	return nil
}

func runEngine(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logging.Configure(logging.ParseLevel(*logLevel), logging.ParseFormat(*logFormat), stderr)
	log := logging.Get("cli")

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}

	// The engine owns the process signals exclusively — one handler per
	// signal — because a graceful drain is the difference between a
	// restart that resumes cleanly and one that redelivers half-finished
	// turns. Nothing else in the process may install a handler.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("engine_starting", "version", version.String(), "company", company.Name)
	e, err := engine.New(ctx, engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		return err
	}
	if err := e.Start(ctx); err != nil {
		// Everything the engine opened comes down with it. Returning
		// without this leaves a broker, a store and a set of held seat
		// leases behind — and the leases are the expensive half, because
		// a peer cannot take those seats until they lapse at the TTL.
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	<-ctx.Done()

	// Detached from the signal context, which is already cancelled — that
	// is what woke us. The drain waits for in-flight turns, bounded only
	// by the context it is given, so handing it this one would abandon
	// every turn already running and make the shutdown the opposite of
	// graceful.
	//
	// The bound belongs to whatever supervises the process: a container
	// runtime's kill grace, an operator's second interrupt. Both already
	// have one and can see things this process cannot.
	log.Info("engine_draining")
	e.Stop(context.WithoutCancel(ctx))
	return nil
}

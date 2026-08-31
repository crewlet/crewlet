// Command crewlet is the engine binary.
//
// One binary is the whole product: it runs agents, serves the API and
// dashboard, and — in the default solo topology — embeds its own broker and
// database, so a company runs with no external services at all. What a node
// does is a config value (node.roles), not a different command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"

	"gopkg.in/yaml.v3"
)

func main() {
	// WHERE THIS PROCESS'S LOGS GO, decided by the one function that owns
	// the process. Everything below adjusts only the level and the format
	// (logging.SetVerbosity), because a command that installed a
	// destination would be installing whatever writer its caller handed
	// it — which under `go test` is one test's buffer, shared with every
	// other test in the binary.
	//
	// Not redundant with logging's own init default: this is the
	// statement of intent, and an init that changed would otherwise
	// change the CLI silently.
	logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr)
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// errSilent has already said everything it has to say, on stdout,
		// in the shape its caller asked for. Printing a second copy on
		// stderr is what makes a machine consumer's log unreadable.
		if errors.Is(err, errSilent) {
			os.Exit(1)
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

	// THE OPERATOR COMMANDS ARE QUIET BY DEFAULT. They open a store, which
	// logs a migration line per schema file and an open line per call —
	// noise on a one-shot command whose stdout is meant to be piped, read
	// or diffed. `run` configures its own level from its own flag, and
	// nothing here silences a WARNING.
	//
	// QUIET IS A DEFAULT, NOT A CEILING. A half-applied migration or a
	// deploy gate that failed is exactly the run whose detail an operator
	// needs, and pinning every non-`run` command at warn with no override
	// left them nothing to turn up. $CREWLET_LOG_LEVEL is the escape
	// hatch, and it is an environment variable rather than a flag on nine
	// commands because it belongs to the INVOCATION rather than to any one
	// of them — a CI step exports it once and every command it runs
	// answers.
	if cmd != "run" {
		// THE LEVEL, NOT THE DESTINATION. This used to install `stderr`
		// as the process-wide sink, which is wrong twice over: `run`
		// takes that writer so it can be TESTED, and a function that
		// mutates process state out of one of its own arguments cannot
		// be called twice concurrently. Under `go test` it was 29
		// parallel tests pointing the global at their own buffers — a
		// data race, and one test's log lines landing in another's
		// output. See [logging.Configure].
		logging.SetVerbosity(operatorLogLevel(), operatorLogFormat())
		warnUnrecognisedLogNames(logging.Get("cli"), "environment",
			"CREWLET_LOG_LEVEL", os.Getenv("CREWLET_LOG_LEVEL"),
			"CREWLET_LOG_FORMAT", os.Getenv("CREWLET_LOG_FORMAT"))
	}

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
	case "schema":
		return emitSchema(rest, stdout, stderr)
	case "secrets":
		return runSecrets(rest, stdout, stderr)
	case "config":
		return runConfig(rest, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "budgets":
		return runBudgets(rest, stdout, stderr)
	case "backup":
		return runBackup(rest, stdout, stderr)
	case "llm":
		return runLLM(rest, stdout, stderr)
	case "gitlab", "github", "jira", "slack", "confluence", "mattermost":
		return runIntegration(cmd, rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s — an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
                              -roles ingress|seats|workers narrows what this node does
  crewlet validate [flags]    Check both config tiers without starting anything
  crewlet schema [tier]       Print a tier's JSON Schema (company by default)
  crewlet migrate [config]    Apply pending schema migrations (-check reports only)
  crewlet budgets <cmd>       Show or reset the durable token counters
  crewlet backup -dir PATH    Copy this node's store and stream estate, through
                              the running engine, to a path on ITS host
  crewlet secrets <cmd>       Read and rotate the encrypted secret store
  crewlet config <cmd>        Import, inspect and activate company revisions
  crewlet llm <cmd>           Log in, verify and export the subscription CLI backends
  crewlet gitlab <cmd>        Reconcile the company's seats into a GitLab instance
  crewlet github <cmd>        Report a GitHub deployment's seat accounts and hook it
  crewlet jira <cmd>          Report a Jira instance's seat accounts and projects
  crewlet slack <cmd>         Create, update and install one Slack app per seat
  crewlet confluence <cmd>    Publish authored markdown and tool skills into spaces
  crewlet mattermost <cmd>    Reconcile a Mattermost team, and diagnose one
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q).
            A seed: it is imported into the store when the store does not
            already hold it, and a running node serves the store.
`, version.String(), defaultBootstrapPath, defaultCompanyPath)
}

// The two tiers are separate files because they answer to separate people. A
// node's broker address and store path are an operator's; the org chart and
// the model behind each seat are the company's.
//
// Tier A is read from its file on every boot and never from anywhere else —
// it is what tells the process where its store and broker are, so it cannot
// come from them.
//
// Tier B is different: -company names a SEED. A running node serves the
// revision the activation pointer names, and the file is imported into the
// store when the store does not already hold it (see reconcile.go). That is
// what makes a PUT /config on one node reach every other, and what makes an
// operator's edit to the file still take effect.
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
	if err := errors.Join(nameTheNeighbour(*c.bootstrap, bootErr), companyErr); err != nil {
		return nil, nil, err
	}
	return boot, company, nil
}

// nameTheNeighbour adds the one hint that answers the commonest first-run
// failure.
//
// This repository's own quickstart, its example file and half its
// documentation have called the Tier A document `config.yaml`, while the
// binary's default is `crewlet.yaml`. An operator who followed the guide gets
// "no such file" about a name they never typed, with their file sitting right
// there. Naming it costs one stat and saves the whole diagnosis.
//
// A HINT, not a fallback: silently loading a file the operator did not ask
// for is how a node boots from the wrong document on a machine that has both.
func nameTheNeighbour(path string, err error) error {
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	neighbour := filepath.Join(filepath.Dir(path), "config.yaml")
	if filepath.Clean(neighbour) == filepath.Clean(path) {
		return err
	}
	if _, statErr := os.Stat(neighbour); statErr != nil {
		return err
	}
	return fmt.Errorf("%w\n  (%s is there — this build's Tier A default is %q; "+
		"pass it as `crewlet run %s` or with -config)",
		err, neighbour, defaultBootstrapPath, neighbour)
}

// Tier is which config document a validation is about.
//
// A NAMED TYPE with a closed set, because the value decides which validator
// runs and an unrecognised one must be a refusal rather than a silent fall
// through to "company" — a Tier A file validated as Tier B fails on every
// field it has, and the operator reads a wall of nonsense.
type Tier string

const (
	// TierAuto picks by reading the document — see [detectTier].
	TierAuto Tier = "auto"
	// TierCompany is Tier B: the company's org, providers, integrations.
	TierCompany Tier = "company"
	// TierBootstrap is Tier A: this node's broker, store and API.
	TierBootstrap Tier = "bootstrap"
)

// tiers is the closed set, for the flag's error and for validation.
var tiers = []Tier{TierAuto, TierCompany, TierBootstrap}

func (t Tier) Valid() bool {
	return slices.Contains(tiers, t)
}

// detectTier reads a document and says which tier it is.
//
// # By the keys it has, not by its filename
//
// A filename is a convention an operator can break and routinely does — and
// the one thing this must get right is the case where they named the file
// something else. The two tiers share no top-level key at all, which is what
// makes the test cheap and unambiguous: `name` and `agents` belong to a
// company, `node`, `stream`, `store` and `coordination` to a bootstrap.
//
// An UNDECIDABLE document is an error naming -tier, never a guess. Guessing
// wrong reports every field of the file as invalid, and an operator reading
// that has no way to tell it from a genuinely broken document.
func detectTier(raw []byte) (Tier, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// NOT AN ERROR HERE: whichever validator runs will report the
		// parse failure with its own line numbers. Company is the
		// likelier document by far, so it is the one that gets to speak.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return TierCompany, nil
	}
	company := 0
	bootstrap := 0
	for key := range doc {
		switch key {
		case "name", "agents", "providers", "integrations", "knowledge", "learning":
			company++
		case "node", "stream", "store", "coordination", "api", "secrets", "logging":
			bootstrap++
		}
	}
	switch {
	case company > bootstrap:
		return TierCompany, nil
	case bootstrap > company:
		return TierBootstrap, nil
	default:
		return "", fmt.Errorf(
			"cannot tell which tier this document is: it carries %d key(s) "+
				"only a company has and %d only a bootstrap has. Name it with "+
				"-tier company or -tier bootstrap", company, bootstrap)
	}
}

// validation is what one `crewlet validate` run found.
//
// THE SHAPE IS THE JSON, so the two output modes cannot drift: the text
// renderer reads this struct too, rather than being a second pass over the
// same data that eventually disagrees with it.
type validation struct {
	Valid   bool              `json:"valid"`
	Tier    Tier              `json:"tier"`
	File    string            `json:"file,omitempty"`
	Errors  []validationError `json:"errors"`
	Summary map[string]any    `json:"summary,omitempty"`
}

// validationError is one problem, with the parts an authoring loop needs to
// jump to the field and decide what to do.
type validationError struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func faultsOf(err error) []validationError {
	faults := config.Faults(err)
	out := make([]validationError, 0, len(faults))
	for _, f := range faults {
		out = append(out, validationError{
			Path: f.Path, Type: f.KindName(), Message: f.Detail,
		})
	}
	return out
}

// validateConfigs is `crewlet validate`.
//
// # Two shapes, because it has two callers
//
// An OPERATOR names one file and reads a sentence. An AUTHORING LOOP — a
// model editing a config until it is valid — names one file and reads
// -json, where every problem carries the exact path to the field. Those are
// the same command because they are the same question, and a loop that had
// to parse prose would converge on whatever the prose happened to say.
//
// The two-flag form (-config plus -company) checks BOTH tiers at once, which
// is what a CI step wants.
func validateConfigs(args []string, stdout, stderr io.Writer) error {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	tier := fs.String("tier", string(TierAuto),
		"which document a positional file is: auto, company or bootstrap")
	asJSON := fs.Bool("json", false,
		"emit {valid, tier, errors:[{path,type,message}], summary} instead of prose")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// LEFTOVERS ARE REFUSED, not ignored. Go's flag package stops at the
	// first non-flag token, so `crewlet validate company.yaml -json` used
	// to parse ZERO flags, discard both tokens and validate ./crewlet.yaml
	// and ./company.yaml instead — printing a success line about files it
	// never opened. A fix loop reading that converges on nothing.
	tail := fs.Args()
	if file == "" && len(tail) == 1 {
		file, tail = tail[0], nil
	}
	if len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet validate [<file.yaml>] [-tier auto|company|bootstrap] [-json]\n"+
				"   or: crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]")
		return errors.New("name at most one document")
	}
	if !Tier(*tier).Valid() {
		return fmt.Errorf("-tier %q is not one of %s", *tier, tierNames())
	}
	if file != "" {
		return validateOne(file, Tier(*tier), *asJSON, stdout)
	}
	return validateBoth(cfg, *asJSON, stdout)
}

func tierNames() string {
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, string(t))
	}
	return strings.Join(out, ", ")
}

// validateOne checks the single document an operator named.
func validateOne(file string, tier Tier, asJSON bool, stdout io.Writer) error {
	res := validation{File: file, Tier: tier, Errors: []validationError{}}
	raw, err := os.ReadFile(file)
	if err != nil {
		res.Errors = faultsOf(err)
		return report(stdout, res, asJSON)
	}
	if tier == TierAuto {
		detected, detErr := detectTier(raw)
		if detErr != nil {
			res.Errors = faultsOf(detErr)
			return report(stdout, res, asJSON)
		}
		res.Tier = detected
	}

	if res.Tier == TierBootstrap {
		// TIER A RESOLVES FROM THE ENVIRONMENT ALONE, which is not a
		// default but a rule: it carries the store's address and the keys
		// that open it, so a resolver reaching the secret store would have
		// Tier A reading from the store it is describing.
		boot, bootErr := config.LoadBootstrap(file, config.EnvOnly())
		if bootErr != nil {
			res.Errors = faultsOf(bootErr)
			return report(stdout, res, asJSON)
		}
		res.Valid = true
		res.Summary = map[string]any{
			"stream": boot.Stream.Type, "coordination": boot.Coordination.Type,
			"store": boot.Store.Path, "roles": boot.Node.Roles,
		}
		return report(stdout, res, asJSON)
	}

	company, companyErr := config.LoadCompany(file)
	if companyErr != nil {
		res.Errors = faultsOf(companyErr)
		return report(stdout, res, asJSON)
	}
	// Building the epoch is the rest of the check, and it reaches nothing:
	// no broker, no store, no provider is dialled. It is what catches the
	// problems a schema cannot — a seat whose llm names no configured
	// provider, a role reporting to a unit that does not exist.
	epoch, epochErr := engine.NewCompany(company)
	if epochErr != nil {
		res.Errors = faultsOf(epochErr)
		return report(stdout, res, asJSON)
	}
	res.Valid = true
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(epoch.Seats()),
		"llm_providers": len(epoch.Models.Keys()),
	}
	return report(stdout, res, asJSON)
}

// validateBoth is the two-flag form: check a Tier A and a Tier B together.
//
// BOTH, not just the first to fail: an operator fixing a broker URL only to be
// told about their org chart on the next boot has been made to pay twice for
// one edit. It is the same rule each tier's own validator follows internally.
func validateBoth(cfg configFlags, asJSON bool, stdout io.Writer) error {
	res := validation{Tier: TierAuto, Errors: []validationError{}}
	boot, company, err := cfg.load()
	if err != nil {
		res.Errors = faultsOf(err)
		return report(stdout, res, asJSON)
	}
	epoch, err := engine.NewCompany(company)
	if err != nil {
		res.Errors = faultsOf(err)
		return report(stdout, res, asJSON)
	}
	res.Valid = true
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(epoch.Seats()),
		"llm_providers": len(epoch.Models.Keys()),
		"stream":        boot.Stream.Type, "coordination": boot.Coordination.Type,
	}
	return report(stdout, res, asJSON)
}

// report renders a validation and returns the run's exit disposition.
//
// A FAILED VALIDATION IS A NON-ZERO EXIT IN BOTH MODES. The -json caller is a
// CI step as often as it is a model, and `crewlet validate x.yaml -json ||
// exit 1` has to be able to fail — a mode that printed {"valid": false} and
// exited 0 would pass every gate built on it.
func report(stdout io.Writer, res validation, asJSON bool) error {
	if asJSON {
		raw, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(raw))
		if res.Valid {
			return nil
		}
		// SILENT, because the payload already carries every problem and a
		// second copy on stderr is what makes a JSON consumer's log
		// unreadable.
		return errSilent
	}
	if !res.Valid {
		var b strings.Builder
		for _, e := range res.Errors {
			if e.Path != "" {
				b.WriteString("\n  " + e.Path + ": " + e.Message)
				continue
			}
			b.WriteString("\n  " + e.Message)
		}
		return errors.New(strings.TrimPrefix(b.String(), "\n  "))
	}
	fmt.Fprintln(stdout, summaryLine(res))
	return nil
}

// summaryLine is the one prose line a successful validation prints.
func summaryLine(res validation) string {
	if name, ok := res.Summary["company"].(string); ok {
		line := fmt.Sprintf("%s: %d agent seats, %d LLM providers",
			name, res.Summary["seats"], res.Summary["llm_providers"])
		if stream, both := res.Summary["stream"]; both {
			line += fmt.Sprintf(", stream %q, coordination %q",
				stream, res.Summary["coordination"])
		}
		return line
	}
	return fmt.Sprintf("%s: stream %q, coordination %q, store %q, roles %v",
		res.File, res.Summary["stream"], res.Summary["coordination"],
		res.Summary["store"], res.Summary["roles"])
}

// errSilent asks the caller to exit non-zero without printing anything more.
var errSilent = errors.New("")

func runEngine(args []string, stderr io.Writer) error {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "console",
		"log format: console (columns and colour for a person), text or json")
	debug := fs.Bool("debug", false, "shorthand for -log-level debug")
	roles := fs.String("roles", "",
		"what this node runs, overriding node.roles: ingress, seats, workers")
	apiHost := fs.String("api-host", "", "bind address, overriding api.host")
	apiPort := fs.Int("api-port", -1,
		"bind port, overriding api.port; 0 serves no HTTP at all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// THE POSITIONAL IS THE TIER A PATH, and a leftover is REFUSED rather
	// than ignored. Go's flag package stops at the first non-flag token,
	// so `crewlet run /etc/crewlet.yaml -debug` used to parse no flags at
	// all, discard the path, and boot from ./crewlet.yaml — or from
	// nothing — without ever mentioning the file the operator named.
	tail := fs.Args()
	if file == "" && len(tail) == 1 {
		file, tail = tail[0], nil
	}
	if len(tail) > 0 {
		fmt.Fprintln(stderr, "usage: crewlet run [<config.yaml>] "+
			"[-company <company.yaml>] [-roles …] [-api-host …] [-api-port …]")
		return errors.New("name at most one config document")
	}
	if file != "" {
		if isFlagSet(fs, "config") {
			return errors.New(
				"the config document is named twice, as a positional argument " +
					"and as -config; they would have to agree and nothing " +
					"checks that they do")
		}
		*cfg.bootstrap = file
	}

	// THE FLAGS ALONE, FIRST, because the file has not been read yet and
	// reading it is exactly what `-debug` is most often turned on to watch:
	// an unresolved ${VAR}, a refused enum, a path that is not there. The
	// second call below re-applies this on top of the file's own settings.
	//
	// The level and format this invocation asked for, on the process's own
	// sink — see the note in [run] for why the writer is not this
	// command's to install.
	logging.SetVerbosity(flagLogLevel(*logLevel, *debug), logging.ParseFormat(*logFormat))
	log := logging.Get("cli")
	warnUnrecognisedLogNames(log, "flag", "-log-level", *logLevel, "-log-format", *logFormat)

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}
	// AND NOW THE FILE, which is what makes `debug: true` in Tier A mean
	// anything. It was a declared field nothing ever read: the quickstart
	// tells an operator to write it and the deployment guide says it
	// "raises the log level to DEBUG", and for the life of the field it
	// did nothing at all. Lines emitted BEFORE this point came out under
	// the flags alone, which is the best a process can do about a file it
	// has not opened yet.
	logging.SetVerbosity(logSettings(boot, fs, *logLevel, *logFormat, *debug))
	// THE FLAGS OVERRIDE THE FILE, and are applied AFTER it loads so a
	// validation failure names the file's own value rather than one the
	// command line put there. Each is applied only when it was actually
	// given: an unset -api-port is not "port 0", which would serve no
	// HTTP at all and make every integration go deaf.
	if err = overrideNode(boot, fs, *roles, *apiHost, *apiPort); err != nil {
		return err
	}

	// The engine owns the process signals exclusively — one handler per
	// signal — because a graceful drain is the difference between a
	// restart that resumes cleanly and one that redelivers half-finished
	// turns. Nothing else in the process may install a handler.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// TRACING BEFORE THE ENGINE, because engine.New starts subscriptions
	// and registers the publish listener, and every one of those can open a
	// span. A provider installed after them would leave the boot — the one
	// stretch an operator most often wants a trace of — reporting nothing.
	//
	// The flush is DEFERRED rather than written at each return. There are
	// six ways out of this function, five of them boot failures, and a
	// deferred call registered here runs after all of them AND after the
	// drain below, which is exactly the ordering the batch processor needs:
	// the spans the drain itself emits are in the buffer when it flushes.
	// Writing it at each return is how five of the six quietly stop doing it.
	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return err
	}
	flushTraces, err := tracing.Configure(ctx, tracing.Options{
		NodeID:  nodeID,
		Version: version.String(),
	})
	if err != nil {
		return err
	}
	defer func() {
		if flushErr := flushTraces(ctx); flushErr != nil {
			// Losing telemetry is not worth failing a shutdown that has
			// otherwise succeeded — the drain has already released the
			// seats and closed the backends by the time this runs.
			log.WarnContext(ctx, "trace_flush_failed", "error", flushErr)
		}
	}()

	log.InfoContext(ctx, "engine_starting", "version", version.String(), "company", company.Name)
	e, err := engine.New(ctx, engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		return err
	}

	// ONE keyring for the process. The reconciler opens revisions with it
	// and the config surface seals them with it; two ciphers over one
	// store would mean a revision written by this node is one it cannot
	// read back.
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return fmt.Errorf("secrets keyring: %w", err)
	}

	// THE STORE IS AUTHORITATIVE AT RUNTIME; the file is a seed. Both
	// halves matter: without the seed a first run has nothing to activate
	// and the node serves a company no peer can see, and without the store
	// a PUT /config on one node would be invisible to every other.
	//
	// Converged BEFORE Start, so seats are claimed under the epoch this
	// node will actually serve rather than under the file's and then moved.
	reconciler, err := startReconciler(ctx, e, boot, company, cipher, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// THE POSTURE REACHES PEERS from here, because the reconciler owns it
	// and is only built once the engine exists. Set before Start, so the
	// first heartbeat already carries it.
	e.SetPosture(func(ctx context.Context) string {
		return string(reconciler.Posture(ctx))
	})

	// AND THE GATE THAT ACTS ON IT. Reporting the posture is not applying
	// it: without this the inbound edge, the scheduler and every seat's
	// own inbox admitted work whatever this node had concluded, so a
	// `shed` reached the dashboard and the readiness probe and changed
	// nothing else. Set beside the reporter and before Start, so the
	// refusal is live from the first delivery this node could take.
	e.SetAdmits(reconciler.Admits)

	// MERGED: one process is both engine and API, sharing one broker and
	// one store. The API half is what makes the node reachable at all —
	// every inbound webhook arrives through it — so an engine that ran
	// without it would hold seats and hear nothing.
	//
	// BOUND BEFORE THE ENGINE STARTS, and the order is a fix rather than a
	// preference. Starting the engine first means claiming seats first, and
	// a seat is not claimed until its per-role MCP children are up — one
	// subprocess per server per seat, each a spawn and a handshake and a
	// tools/list. On the Nimbus example that is 21 children, and the whole
	// inbound edge — dashboard, REST, every vendor's webhook — was dark for
	// as long as they took. Measured at 37 seconds with four seats and every
	// vendor failing FAST; a company whose vendors actually answer takes
	// minutes, and it scales with seats times servers.
	//
	// Nothing here needs a started engine: the node exists, /health and
	// /ready report honestly that it holds no seats yet, and a webhook that
	// arrives in the window is retained rather than dropped because the
	// mailboxes are created before any claiming — see Node.Start.
	surface, err := serveAPI(ctx, boot, e, reconciler, cipher, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	if err = e.Start(ctx); err != nil {
		// Everything the engine opened comes down with it. Returning
		// without this leaves a broker, a store and a set of held seat
		// leases behind — and the leases are the expensive half, because
		// a peer cannot take those seats until they lapse at the TTL.
		//
		// The listener goes first, for the same reason it does in the
		// ordinary shutdown below: it is already accepting, and a node
		// whose engine failed to start must stop answering before it
		// gives up whatever it holds.
		if surface != nil {
			surface.stop(context.WithoutCancel(ctx), log)
		}
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// The poll loop, started only once the node is serving: a reconcile
	// that landed mid-boot would apply an epoch to a node that has not
	// claimed anything yet, which is work with nowhere to go.
	go reconciler.Run(ctx)

	<-ctx.Done()

	// HAND THE SIGNALS BACK before draining, so a SECOND interrupt kills
	// the process. Until this runs, signal.NotifyContext is still the
	// installed handler and has already cancelled its context, so every
	// further SIGINT is swallowed — leaving an operator watching a drain
	// they cannot abort from the terminal they started it in, with SIGKILL
	// from somewhere else as the only way out.
	//
	// That escape hatch is what makes the unbounded drain below safe to
	// offer: the bound belongs to whoever supervises the process, and for
	// an operator at a terminal the second press IS their bound.
	stop()

	if surface != nil {
		surface.stop(context.WithoutCancel(ctx), log)
	}

	// Detached from the signal context, which is already cancelled — that
	// is what woke us. The drain waits for in-flight turns, bounded only
	// by the context it is given, so handing it this one would abandon
	// every turn already running and make the shutdown the opposite of
	// graceful.
	//
	// The bound belongs to whatever supervises the process: a container
	// runtime's kill grace, or the operator's second interrupt that the
	// stop() above just re-armed. Both already have one and can see
	// things this process cannot.
	log.InfoContext(ctx, "engine_draining")
	e.Stop(context.WithoutCancel(ctx))
	return nil
}

// httpSurface is the API half of a merged node.
type httpSurface struct {
	app       *api.App
	server    *http.Server
	projector *observe.Projector
}

// stop shuts the HTTP surface down before the engine drains.
//
// BEFORE, deliberately: the drain waits for in-flight turns, and a listener
// still accepting webhooks during it would keep minting new ones. Closing the
// door first is what makes the drain converge.
func (s *httpSurface) stop(ctx context.Context, log *slog.Logger) {
	shutdown, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	if err := s.server.Shutdown(shutdown); err != nil {
		// A listener that would not close is not a reason to skip the
		// drain: the seats are the expensive thing to strand.
		log.WarnContext(ctx, "api_shutdown_failed", "error", err)
	}
	// After the listener, so no socket can be reading the projection while
	// its feed is torn down, and before the engine drains, so the drain's
	// own turns are not projected onto a page nobody can reach.
	s.projector.Stop(shutdown)
	s.app.Stop()
}

// apiShutdownGrace bounds how long the listener waits for in-flight REQUESTS.
//
// Requests, not turns. A REST call is a read against the local store and a
// webhook is a signature check plus a publish — both are milliseconds — so this
// only ever covers a client that stopped reading its own response. The turns
// are the engine's drain to wait for, and that one is bounded by the process
// supervisor rather than by a constant here.
const apiShutdownGrace = 5 * time.Second

// serveAPI binds the HTTP surface, or reports that this node serves none.
// companySecrets reads the verification material out of the engine's CURRENT
// epoch, on every request.
//
// Not captured once: a config reload replaces the epoch, and a receiver holding
// the old one would keep rejecting deliveries signed with a rotated secret —
// a failure that looks exactly like an attack and resolves only on restart.
// companyConfig is the engine's CURRENT company document, or nil.
func companyConfig(e *engine.Engine) *config.Company {
	if company := e.Company(); company != nil {
		return company.Config
	}
	return nil
}

func companySecrets(e *engine.Engine) webhooks.Secrets { return e.WebhookSecrets() }

func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, cipher secrets.Cipher, log *slog.Logger,
) (*httpSurface, error) {
	if boot.API.Port == 0 {
		// A real posture: a worker-only node runs no dashboard, no REST
		// API and no webhook endpoint. Saying so is the point — an
		// operator who expected an integration to work should learn it
		// here rather than from a webhook that never arrives.
		log.WarnContext(ctx, "api_disabled",
			"hint", "api.port is 0, so this node serves no dashboard, no REST "+
				"API and no webhook endpoint; every integration is deaf here")
		return nil, nil
	}

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("api: node identity: %w", err)
	}
	// ONE config surface, shared by the REST routes and the socket
	// queries. Sealing and opening with the SAME keyring the reconciler
	// applies through: two ciphers over one store would mean a revision
	// written here is one no node can read.
	configSurface := configapi.New(configapi.Options{
		Store: e.Backends().Store, Cipher: cipher,
		// THE POINTER, without which the write routes have nothing to
		// activate against. It was missing, so every /config write on
		// this binary reached a nil plane — the live-edit path that is
		// the whole of Tier B.
		Plane: e.Backends().Fleet,
		// And the nudge, so an operator's change lands on every node in
		// milliseconds rather than at the next reconcile poll.
		Queue: e.Backends().Queue,
	})
	// The fleet's secret store, sealed with the SAME keyring — a value
	// written here is one this node and every peer opens with the key
	// their Tier A names, and a second cipher would make a rotation
	// readable only on the node that served the request.
	secretSurface := secretsapi.New(secretsapi.Options{
		Fleet: e.Backends().Fleet, Cipher: cipher,
		ActiveKeyID: boot.Secrets.ActiveKeyID,
	})

	app := api.New(api.Options{
		Bootstrap: boot,
		Runtime:   engineRuntime{engine: e, reconciler: reconciler},
		// THE ENGINE'S OWN RECEIVER, not a second one built here. In a
		// merged process the API verifies tokens the engine minted, and
		// two receivers would sign with two per-process keys unless a
		// keyring happened to be configured.
		OtelReceiver: e.OtelReceiver(),
		QueueBackend: e.Backends().Queue.Backend(),
		// The read surface answers from this node's OWN store. A
		// question it has no source for comes back unknown rather than
		// empty, which is the difference between "this node has no
		// event log" and "the company has done nothing".
		Sources: queries.Sources{
			Events: e.Backends().Store.Events(),
			// Read through the ENGINE's epoch rather than a captured
			// company: an apply replaces it, and a screen bound to the
			// one this process booted on would describe a company that
			// is no longer running.
			Company:       func() *config.Company { return companyConfig(e) },
			Coord:         e.Backends().Coord,
			Plane:         e.Backends().Fleet,
			Runs:          sqlledger.New(e.Backends().Store.SQL()),
			Conversations: ledgerstore.NewConversations(e.Backends().Store),
			Diary:         learning.NewDiary(e.Backends().Store),
			Episodes:      learning.NewEpisodes(e.Backends().Store),
			Config:        configSurface,
			Budget:        e.Backends().Fleet,
			// The DURABLE record of detached coding runs. Read rather
			// than projected: a run parked on a person's question can
			// wait days, and the live projection sweeps long before
			// that — so the states that most need somebody were the
			// ones least likely to be on screen.
			//
			// The FLEET's record, so the screen shows every node's
			// runs rather than this one's. A run is recovered by
			// whichever node owns its seat, which is exactly why a
			// per-node read drew a dashboard that disagreed with
			// itself depending on which node answered.
			Sandbox: sandbox.NewCoordStore(e.Backends().Fleet),
			NodeID:  nodeID,
		},
		// The inbound edge. It republishes onto THIS node's queue and
		// dedupes through the FLEET'S coordination store, which is what
		// makes a delivery that lands on any node wake the seat's owner
		// exactly once — a vendor retrying reaches whichever node the
		// load balancer picks, so a claim only this node could see would
		// suppress nothing.
		// The WRITE half of the counter, for POST /budgets/reset. On the
		// default topology the coordination store is this engine's own
		// embedded broker, so a node that is running is the only thing
		// that can reach it — which is why the reset is a route and not
		// only a CLI subcommand.
		Budgets: e.Backends().Fleet,
		// Both estates a node holds, reachable only from inside it: the
		// store is locked to this process and the broker binds no
		// socket. See internal/backup.
		Backup: backup.New(backup.Options{
			Store:  e.Backends().Store,
			Conn:   e.Backends().Conn(),
			NodeID: boot.Node.ID,
		}),
		Config:  configSurface,
		Secrets: secretSurface,
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return companySecrets(e) },
			Publisher: e.Backends().Queue,
			Claims:    e.Backends().Fleet,
		},
	})
	// CONFIGURED by construction. The engine only exists because a company
	// config parsed, validated and built an epoch, so by the time this
	// runs a company is active — and a node that never said so would be
	// permanently unready, which takes every working node out of a load
	// balancer's rotation.
	//
	// It stays true for the life of the process: an apply that FAILS
	// leaves the node serving the previous epoch, which is a configured
	// node. What a failed apply changes is the POSTURE, and /ready reads
	// that — the two answer different questions, and collapsing them would
	// take a correctly-serving node out of rotation for being behind.
	app.SetConfigured(true)
	app.Start(ctx)

	// The other half of the observability pipeline. The engine already
	// persists what THIS node publishes (see engine.Backends); this is what
	// puts the whole company's events on this node's dashboard, and it is a
	// broadcast subscription because a browser attached here must see turns
	// that ran anywhere — see observe.Projector.
	//
	// Started before the listener binds, so a socket cannot open onto a
	// projection that is not yet being fed.
	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err = projector.Start(ctx); err != nil {
		app.Stop()
		return nil, err
	}

	addr := net.JoinHostPort(boot.API.Host, strconv.Itoa(boot.API.Port))
	// Through a ListenConfig so a shutdown signal arriving while the bind
	// is in flight aborts it, rather than leaving a listener nobody will
	// serve from — the bind can block on a DNS lookup for the host.
	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", addr)
	if err != nil {
		app.Stop()
		return nil, fmt.Errorf("api: bind %s: %w", addr, err)
	}
	server := &http.Server{
		Handler: app,
		// A read that never completes holds a connection open forever,
		// and the listener is the one surface an unauthenticated client
		// can reach.
		ReadHeaderTimeout: apiReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.ErrorContext(ctx, "api_serve_failed", "error", err)
		}
	}()

	log.InfoContext(ctx, "api_listening", "addr", listener.Addr().String(),
		"anonymous_read", app.Guard().AnonymousRead(),
		"tokens", app.Guard().Tokens())
	if app.Guard().AnonymousRead() && !auth.BindIsLoopback(boot.API.Host) {
		// Stated rather than assumed. The read surface carries LLM
		// transcripts, diary entries and the whole event stream, and on a
		// bind anything else can reach that is a decision somebody may
		// not have made deliberately.
		log.WarnContext(ctx, "api_anonymous_read_on_a_reachable_bind",
			"host", boot.API.Host,
			"hint", "reads serve without a token on an address other machines "+
				"can reach; set api.auth.allow_anonymous_read to false to close them")
	}
	// THE CONFIG-DERIVED SURFACES, re-sent whenever an apply changes them.
	//
	// The roster, the org tree and the tool catalogue all come from the
	// company document, so no event will ever correct them: a revision
	// that adds, renames or removes a role produces nothing a projection
	// could learn from, and an overlay merge cannot express a deletion at
	// all. Without this an open dashboard renders the company it connected
	// to until someone reloads.
	//
	// Registered after the app exists, which is the whole reason it is a
	// setter — see Engine.SetOnApplied.
	e.SetOnApplied(func(context.Context) {
		app.Stream().Broadcast("seats", app.Stream().Roster())
		app.Stream().Broadcast("org", app.Stream().Org())
		app.Stream().Broadcast("tools", app.Stream().Tools())
		app.Stream().Broadcast("schedules", app.Stream().Schedules())
	})

	return &httpSurface{app: app, server: server, projector: projector}, nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers.
//
// Ten seconds is generous for a header on any real network and short enough
// that a connection opened and left silent — the cheapest denial there is
// against a listener — costs one slot for ten seconds rather than for ever.
const apiReadHeaderTimeout = 10 * time.Second

// engineRuntime answers the questions only a co-located engine can.
type engineRuntime struct {
	engine     *engine.Engine
	reconciler *engine.Reconciler
}

// Tools is the catalogue this node serves, for the dashboard's tool screen.
//
// THE EPOCH'S SHARED CATALOGUE, not a seat's. A per-role MCP server gives each
// seat its own child and its own registry, so there is no single "the tools" a
// company has — and picking one seat's would render a catalogue that is right
// for one row of the agent screen and wrong for the rest. The shared surface
// is the one every seat has, which is the honest answer to "what does this
// company run".
func (r engineRuntime) Tools() []api.ToolInfo {
	company := r.engine.Company()
	if company == nil || company.Tools == nil {
		return nil
	}
	entries := company.Tools.List()
	out := make([]api.ToolInfo, 0, len(entries))
	for _, entry := range entries {
		source := "builtin"
		if server, ok := entry.FromMCP(); ok {
			source = server
		}
		out = append(out, api.ToolInfo{
			Name:        entry.Name(),
			Description: entry.Tool.Description(),
			Source:      source,
		})
	}
	return out
}

func (r engineRuntime) Snapshot() api.RuntimeState {
	host := r.engine.Node().Host()
	state := api.RuntimeState{
		InFlight:     r.engine.Backends().Queue.InFlightCount(),
		ShuttingDown: host.Draining(),
		Seats:        host.Held(),
		StartedAt:    r.engine.StartedAt().Format(time.RFC3339),
		// Which integrations have a PARSER, which is the only thing that
		// makes a verified delivery reach an agent. Read from the notify
		// service rather than from a list kept here: a hand-maintained
		// one is exactly what drifts, and it would drift towards
		// claiming more than the build does.
		RoutedSources: r.engine.RoutedSources(),
		// And which of them could actually verify a delivery, from the
		// RESOLVED secrets rather than from the config text. Same reason:
		// a list of what the document names would claim more than this
		// process can do.
		VerifiableSources: r.engine.VerifiableSources(),
	}
	if r.reconciler != nil {
		// Read live, on every probe, rather than cached: a cached
		// posture is a node that reports healthy through the whole
		// window in which it stopped being so — and this is the ONLY
		// place an operator can see why a node left rotation, since
		// /ready answers a bare 503 either way and "draining" and
		// "cannot apply epoch 41" call for opposite responses.
		state.Posture = string(r.reconciler.Posture(context.Background()))
		state.AppliedEpoch = r.reconciler.Applied()
	}
	return state
}

// emitSchema writes a tier's JSON Schema.
//
// # Why the CLI carries this at all
//
// The schema is not read by the engine — the Go types in `internal/config`
// are the validator, and the schema is a SUBSET of them. It exists for the
// people and tools that author a config without running one: the
// `# yaml-language-server: $schema=` modeline at the top of every shipped
// example, a CI linter, an assistant writing a config from scratch.
//
// It is generated rather than hand-written for the reason every generated
// artifact is: the two would diverge, and a schema that red-underlines a
// config the engine would happily run teaches authors to ignore it. The
// checked-in copies under `schema/` are regenerated by this command and
// compared in CI, so a config field added without one is a failing build
// rather than a stale file nobody opens.
func emitSchema(args []string, stdout, stderr io.Writer) error {
	// THE TIER COMES OFF THE FRONT, before the flags are parsed. Go's flag
	// package stops at the first non-flag argument, so parsing first would
	// leave `-o` in the positional tail on the natural spelling —
	// `crewlet schema company -o path` — and silently write to stdout.
	name, args := splitSubject(args)
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// EXACTLY ONE tier, wherever it appeared. The tail after flag parsing
	// holds the trailing form (`schema -o path company`) and anything left
	// over in either form — a second tier, a typo — which must be an error
	// rather than a silently ignored argument.
	tail := fs.Args()
	if name == "" && len(tail) == 1 {
		name, tail = tail[0], nil
	}
	if len(tail) > 0 {
		fmt.Fprintf(stderr, "usage: crewlet schema [%s|%s] [-o path]\n",
			config.TierCompany, config.TierBootstrap)
		return errors.New("name at most one tier")
	}
	if name == "" {
		// THE COMPANY TIER IS THE DEFAULT because it is the one an author
		// edits: Tier A is a handful of URLs an operator writes once,
		// while Tier B is the org chart, the providers and every
		// integration — which is what an editor's completion is for.
		name = string(config.TierCompany)
	}

	tier := config.Tier(name)
	body, err := config.Schema(tier)
	if err != nil {
		return err
	}
	// A TRAILING NEWLINE, because the file is checked in and compared: a
	// generator that emits none leaves every editor and every `git diff`
	// reporting a change nobody made.
	body = append(body, '\n')

	if *out == "" {
		_, err = stdout.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%s tier, %d bytes)\n", *out, tier, len(body))
	return nil
}

// splitSubject peels a leading positional argument off a subcommand's args.
//
// Returns "" when the first argument is a flag, which is what lets both
// orders work: `schema company -o x` and `schema -o x company`.
func splitSubject(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// flagLogLevel is the level the command line alone asks for.
//
// The SHORTHAND WINS, because writing both is an operator asking for debug
// in the loudest way available to them.
func flagLogLevel(logLevel string, debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return logging.ParseLevel(logLevel)
}

// warnUnrecognisedLogNames says so when a level or format name did not
// resolve to what it spells.
//
// # The fallback stays; the silence does not
//
// Both of these fail SOFT on purpose — a misspelled log level must never be
// why a company will not boot, and a CI step must never lose a migration to
// a typo in an export. But a soft failure nobody is told about is exactly
// how `debug: true` went its whole life doing nothing: the operator sees
// behaviour they did not ask for, with nothing anywhere pointing at why.
// Warning costs one line on the runs that have a mistake in them and nothing
// at all on the runs that do not.
//
// It logs rather than returning an error because the FILE is where a bad
// value is refused — see config.Logging.validate. A flag and an environment
// variable belong to one invocation and may not fail it.
func warnUnrecognisedLogNames(log *slog.Logger, source,
	levelField, level, formatField, format string,
) {
	if resolved, ok := logging.ParseLevelName(level); !ok {
		log.Warn("log_level_unrecognised", "source", source, "field", levelField,
			"value", level, "using", resolved.String(),
			"want", strings.Join(levelNames(), ", "))
	}
	if resolved, ok := logging.ParseFormatName(format); !ok {
		log.Warn("log_format_unrecognised", "source", source, "field", formatField,
			"value", format, "using", string(resolved),
			"want", strings.Join(formatNames(), ", "))
	}
}

// levelNames and formatNames render the closed sets for the message above,
// FROM the sets themselves — a hand-written list here would tell an operator
// to write a value the next release stopped accepting.
func levelNames() []string {
	out := make([]string, 0, len(logging.Levels))
	for _, l := range logging.Levels {
		out = append(out, string(l))
	}
	return out
}

func formatNames() []string {
	out := make([]string, 0, len(logging.Formats))
	for _, f := range logging.Formats {
		out = append(out, string(f))
	}
	return out
}

// logSettings resolves how loud `crewlet run` is, and in what shape, from
// the two places that get to say: the Tier A file and this command line.
//
// # The command line wins, but only where it actually spoke
//
// A flag carries its default whether or not anyone typed it, so applying
// `*logLevel` unconditionally would pin every node at info and make
// `logging.level: warn` in the file dead on arrival — the same class of bug
// as the `debug:` field this function exists to give a meaning. isFlagSet is
// what separates "the operator asked for info" from "nobody said anything",
// and it is the same idiom [overrideNode] uses for the three node overrides.
//
// `-debug` only ever RAISES: it is the shorthand for asking for debug, not a
// switch that turns the file's own setting off. An operator who wants a
// `debug: true` file quieter for one run says so with `-log-level info`.
func logSettings(boot *config.Bootstrap, fs *flag.FlagSet,
	logLevel, logFormat string, debug bool,
) (slog.Level, logging.Format) {
	level, format := boot.LogSettings()
	if isFlagSet(fs, "log-level") {
		level = logging.ParseLevel(logLevel)
	}
	if isFlagSet(fs, "log-format") {
		format = logging.ParseFormat(logFormat)
	}
	if isFlagSet(fs, "debug") && debug {
		level = slog.LevelDebug
	}
	return level, format
}

// overrideNode applies the run flags that override Tier A.
//
// # Why these three and not a general mechanism
//
// They are the fields whose right value depends on WHERE the process is
// running rather than on what the company is: which job this node does in a
// fleet, and where its HTTP surface binds. Everything else in Tier A is a
// property of the deployment that belongs in the file, where it can be
// reviewed — a flag for each would be a second configuration surface with no
// history and no validation story.
func overrideNode(boot *config.Bootstrap, fs *flag.FlagSet,
	roles, apiHost string, apiPort int,
) error {
	if isFlagSet(fs, "roles") {
		names := splitRoles(roles)
		if len(names) == 0 {
			return errors.New("-roles was given with no role names")
		}
		// THROUGH THE SAME PARSER the file goes through, which fails
		// CLOSED: an unknown name is an error rather than a skipped
		// entry, because a typo like `-roles seat` would otherwise
		// produce a node that runs nothing and reports itself healthy.
		// The flag is applied after the file has loaded, so this is the
		// only thing that validates it.
		if _, err := placement.ParseRoles(names); err != nil {
			return fmt.Errorf("-roles: %w", err)
		}
		boot.Node.Roles = names
	}
	if isFlagSet(fs, "api-host") {
		boot.API.Host = apiHost
	}
	if isFlagSet(fs, "api-port") {
		if apiPort < 0 || apiPort > 65535 {
			return fmt.Errorf("-api-port must be 0 (no HTTP surface) or "+
				"a port 1..65535, got %d", apiPort)
		}
		boot.API.Port = apiPort
	}
	return nil
}

// splitRoles reads a comma-separated role list, ignoring blank entries so a
// trailing comma is not a role named "".
func splitRoles(value string) []string {
	var out []string
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// operatorLogLevel is the level every non-`run` command logs at.
//
// Warn unless $CREWLET_LOG_LEVEL names another. A TYPO RESOLVES TO WARN —
// this command's own default — rather than failing or drifting to info: a bad
// log level must never be why an operator cannot run a migration, and it must
// not quietly change the default either. `run` applies the same
// never-fail rule to its own flag; only the fallback differs, because its
// default is info.
//
// Recognised against the closed set explicitly, because logging.ParseLevel
// cannot report "I did not recognise that" — it answers info for everything
// it does not know, which is the right answer for `run` and the wrong one
// here.
func operatorLogLevel() slog.Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CREWLET_LOG_LEVEL")))
	switch raw {
	case "debug", "info", "warn", "warning", "error":
		return logging.ParseLevel(raw)
	default:
		return slog.LevelWarn
	}
}

// operatorLogFormat is the shape every non-`run` command logs in.
//
// Console, so a migration that fails on a laptop reads like one, unless
// $CREWLET_LOG_FORMAT names another. It is the sibling of
// $CREWLET_LOG_LEVEL and exists for the same reason: these commands take no
// logging flags, and the CI step that ships a `crewlet migrate` run's output
// to a collector needs `json` from it exactly as much as `crewlet run` does.
// Without it the only lever over these commands' output was $NO_COLOR, which
// says nothing about the shape.
//
// A name this build does not know resolves to console — logging.ParseFormat
// applies the same never-fail rule to a format that operatorLogLevel applies
// to a level.
func operatorLogFormat() logging.Format {
	return logging.ParseFormat(os.Getenv("CREWLET_LOG_FORMAT"))
}

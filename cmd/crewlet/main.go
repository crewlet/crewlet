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
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/insights"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/research"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/speech"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/static"

	"gopkg.in/yaml.v3"
)

func main() {
	// THE PROC-RECEIVE HOOK RUNS FIRST: the engine's git starts this binary as
	// `crewlet gitlet hook proc-receive` inside a push, where stdout is git's
	// protocol and stderr the pusher's terminal, so no CLI setup output may
	// reach either. host.HookArgs recognises host.HookCommand's spelling.
	if args, ok := host.HookArgs(os.Args[1:]); ok {
		os.Exit(host.RunHook(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	// The destination is decided here, by the one function that owns the
	// process. Everything below adjusts only level and format
	// (logging.SetVerbosity): a command installing a destination would install
	// its caller's writer, which under `go test` is one test's buffer shared
	// by the whole binary. Not redundant with logging's init default: this
	// states the intent, so a changed init cannot change the CLI silently.
	logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr)
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// errSilent has already reported on stdout in the shape the caller
		// asked for; a second copy on stderr garbles a machine consumer's log.
		code := 1
		var exit *exitError
		if errors.As(err, &exit) {
			code = exit.code
		}
		if errors.Is(err, errSilent) {
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, "crewlet:", err)
		os.Exit(code)
	}
}

// exitError ends a command with a status of its own, for a script to tell a
// result the command reports from a failure to ask.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// run is main's testable body: arguments and streams are parameters, and it
// runs the invocation against this process's environment ([runIn]).
func run(args []string, stdout, stderr io.Writer) error {
	return runIn(invocation{environ: config.EnvSource{}}, args, stdout, stderr)
}

// invocation is what one run of a command is handed rather than reads from
// the process ([runIn]).
type invocation struct {
	// environ is where a node-talking command reads its credential
	// ([nodeTokenOrEmpty]) and a provisioning command the operator's
	// ([operatorCredential]).
	environ config.Source
}

// runIn is [run] against a handed invocation. The binary passes the
// process's environment, so the rule (`CREWLET_API_TOKEN` and nothing else)
// is unchanged; it is a parameter because a parallel suite cannot give each
// command a credential through the process's variable, and reading the
// process's would read what the machine exports. The engine is handed its
// environment likewise ([engine.Options.Environment]). The `CREWLET_LOG_*`
// knobs configure the process's one logger and are read from the process.
func runIn(inv invocation, args []string, stdout, stderr io.Writer) (err error) {
	// A stored-value read that failed part way stops the command with a panic:
	// the lookup has no error to return and its only other answer, unset, mints
	// over a working secret ([readFailed]). Answered here as the command's
	// failure; any other panic is a bug and goes on.
	defer func() {
		if r := recover(); r != nil {
			failed, ok := r.(readFailed)
			if !ok {
				panic(r)
			}
			err = failed
		}
	}()
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	environ := inv.environ

	// Operator commands are quiet by default: opening a store logs a migration
	// line per schema file, noise on a one-shot command whose stdout is piped
	// or diffed. Quiet is a default, not a ceiling ($CREWLET_LOG_LEVEL, an
	// environment variable because it belongs to the invocation), and no
	// WARNING is silenced. `run` sets its own level from its flag. The relay is
	// excluded too: its stdout IS the MCP protocol stream (internal/mcprelay).
	if cmd != "run" && cmd != mcprelay.Command {
		// The level, not the destination: installing `stderr` as the
		// process-wide sink races between parallel tests pointing the global at
		// their own buffers. See [logging.Configure].
		logging.SetVerbosity(operatorLogLevel(), operatorLogFormat())
		// The file opens before the first log line below, so a misspelled
		// $CREWLET_LOG_LEVEL lands in it. $CREWLET_LOG_FILE exists because
		// these commands take no logging flags and a CI step wants a
		// `crewlet migrate` in its node's durable record; rotation caps stay
		// in a deployment's file. Stderr stays on: a one-shot command whose
		// output vanished into a file would surprise a person at a terminal.
		detach, err := attachLogFile(config.LogFileSettings{
			Open: logging.FileOptions{Path: os.Getenv("CREWLET_LOG_FILE")},
		}, true, os.Stderr)
		if err != nil {
			return err
		}
		defer detach()
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
		return runEngine(rest, stderr, bindTCP)
	case "validate":
		return validateConfigs(rest, stdout, stderr)
	case "schema":
		return emitSchema(rest, stdout, stderr)
	case "secrets":
		return runSecrets(environ, rest, stdout, stderr)
	case "config":
		return runConfig(environ, rest, stdout, stderr)
	case "iam":
		return runIAM(environ, rest, os.Stdin, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "budgets":
		return runBudgets(environ, rest, stdout, stderr)
	case "backup":
		return runBackup(environ, rest, stdout, stderr)
	case "retention":
		return runRetention(environ, rest, stdout, stderr)
	case "worklet":
		return runWorklet(environ, rest, stdout, stderr)
	case "objects":
		return runObjects(environ, rest, stdout, stderr)
	case "gitlet":
		return runGitlet(environ, rest, stdout, stderr)
	case "fleet":
		return runFleet(environ, rest, stdout, stderr)
	case "feeds":
		return runFeeds(environ, rest, stdout, stderr)
	case "doctor":
		return runDoctor(environ, rest, stdout, stderr)
	case "seats":
		return runSeats(environ, rest, stdout, stderr)
	case "artifacts":
		return runArtifacts(environ, rest, stdout, stderr)
	case "chatlet":
		return runChatlet(environ, rest, stdout, stderr)
	case "llm":
		return runLLM(environ, rest, os.Stdin, stdout, stderr)
	case "search":
		return runSearch(rest, stdout, stderr)
	case "assistant":
		return runAssistant(environ, rest, stdout, stderr)
	case "mcp-relay":
		// The literal is mcprelay.Command; a case is read as the command list
		// ([TestTheRelayIsDispatchedUnderItsOwnName] holds the two together).
		return mcprelay.Run(rest, os.Stdin, stdout, stderr)
	case "speech":
		return runSpeech(rest, stdout, stderr)
	case "speech-worker":
		// The literal is speech.WorkerCommand, held together as mcp-relay's.
		return speech.RunWorker(rest, os.Stdin, stdout, stderr)
	case "gitlab", "github", "jira", "slack", "confluence", "mattermost":
		return runIntegration(inv, cmd, rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s: an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
                              -roles data|ingress|seats|workers narrows what this node does
  crewlet validate [flags]    Check both config tiers, or one document (a repository's
                              .crewlet/checks.yaml among them), without starting anything
  crewlet schema [tier]       Print a tier's JSON Schema (company by default),
                              or the issue report catalogue
  crewlet migrate [config]    Apply pending schema migrations (-check reports only)
  crewlet budgets show        Show the durable token counters: each scope's day,
                              week and month, its ceiling and its state
  crewlet backup -dir PATH    Copy this node's store and stream estate, through
                              the running engine, to a path on ITS host
  crewlet retention <cmd>     What the state log is holding, why it is not
                              shrinking, and the gestures that change it
  crewlet worklet <cmd>       The gestures on Worklet's work items that belong to
                              a person: purge, which destroys a task and every
                              row it produced and which nothing undoes
  crewlet objects status      Where the company's files are kept, and what the
                              object store's collector last found missing
  crewlet objects migrate     Move the company's files to an S3 bucket: copy on a
                              live fleet, then -final in maintenance mode
  crewlet gitlet <cmd>        Gitlet: status, verify a node's cache,
                              import a repository the node fetches (or -local, pushed
                              from this machine), export a bundle, purge-unreachable
                              (credential is git's helper)
  crewlet fleet broker <cmd>  The fleet broker's members, as the nodes advertise them
                              and as its metadata group counts them: list, and
                              remove a member that is gone for good
  crewlet feeds <cmd>         The wake feeds' dead letters, records no node could
                              read: dead-letters lists them, and discard gives
                              one's wake up for good
  crewlet doctor              The setup advisor's findings from a running node:
                              classes, subjects by ordinal and counts (-words
                              adds names and reasons); exits 2 when one blocks
  crewlet seats <cmd>         Pause a seat (-stop also ends its running turn and run)
                              or resume it: its holder, its lead, or an admin
  crewlet artifacts <cmd>     List, publish, version, share, trash and purge the
                              company's artifacts, as the token's principal
  crewlet chatlet <cmd>       Read, post and search the company's chat as your
                              seat. Admins can also delete messages and set how
                              long a room keeps them
  crewlet secrets <cmd>       Read and rotate the encrypted secret store
  crewlet config <cmd>        Import, inspect and activate company revisions
  crewlet iam <cmd>           People, seats, invitations, permissions,
                              sessions and tokens
  crewlet llm <cmd>           Log in, verify and export the harnesses every model
                              runs through
  crewlet search eval         Measure the semantic search against the exact scan,
                              on the vectors a store file actually holds
  crewlet assistant eval      Run evaluation cases through Ask Crewlet,
                              on their own scripts or against a candidate model
  crewlet mcp-relay SOCKET    Not for people: the MCP server a seat's harness
                              starts, relaying its stdio to the engine's tools
  crewlet speech prepare -dir DIR
                              Download and verify the dictation recognizer into
                              DIR, for dictation.recognizer_dir or an image
  crewlet speech-worker       Not for people: the dictation recognizer the engine
                              runs as a child process
  crewlet gitlab <cmd>        Register a GitLab group's webhook and its signing secret
  crewlet github <cmd>        Report a GitHub deployment's seat accounts and hook it
  crewlet jira <cmd>          Report a Jira instance's seat accounts and projects
  crewlet slack <cmd>         Retired: each agent's Slack app is the reconcile loop's
  crewlet confluence <cmd>    Register the inbound hooks; publish markdown and tool skills into spaces
  crewlet mattermost <cmd>    Diagnose a Mattermost install
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q).
            A BOOTSTRAP seed: imported only where no company exists yet
            (none in this node's store and none the fleet runs). Otherwise it
            is ignored, loudly, and the node runs the company that exists,
            so a restart never reverts a live change. Not read at all where
            Tier A sets api.auth.company_writers.
  -import-company
            Tier B to make active NOW, over whatever the fleet is running.
            The deliberate "this file is the company again" gesture.
            Refused where Tier A sets api.auth.company_writers.
            To change a RUNNING fleet with no restart at all, use
            "crewlet config import", which goes through the node's API.

Environment:
  %s
            The token every command that talks to a running node sends,
            reads included, to whichever node its -api names: one of the
            node's api.auth.tokens values, or an access token from "crewlet
            iam token" (whose -login form signs in instead and reads none).
            It's never a flag (flags end up in shell history) and never
            read from the config file.
`, version.String(), defaultBootstrapPath, defaultCompanyPath, apiTokenEnv)
}

// The tiers are separate files because they answer to separate people: the
// broker and store path are an operator's, the org chart and models the
// company's. Tier A is read from its file on every boot: it locates the
// store. Tier B is two flags, since a running node serves the revision the
// fleet's activation pointer names and a file only gets a document INTO it:
//
//	-company        BOOTSTRAP: imported only where no company exists yet.
//	-import-company OVERRIDE: make this file the active revision now.
//
// Override as the default would let a stale company.yaml silently win over a
// live edit; bootstrap's ignore is loud (see reconcile.go).
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

// tierBSeed is the Tier B document a `crewlet run` carries, and what it is
// for: the store is authoritative, so a file only gets a document INTO it.
// Override says whether it bootstraps a company that does not exist yet or
// replaces what the fleet runs. Company is nil when there is no file.
//
// It is the seed as the command line resolved it, not what it does: that is
// [decideSeed]'s, whose [seedPlan] is all the boot and the import accept.
type tierBSeed struct {
	Path     string
	Override bool
	Company  *config.Company

	// Managed is a -company seed this deployment's Tier A rules out
	// (`api.auth.company_writers`, ADR-0030): Path names the file, which
	// was never read, and Company is nil. See [managedSeed].
	Managed bool
}

// loadForRun reads Tier A, and Tier B as the seed `run` will reconcile,
// reporting every problem it can rather than the first, as [configFlags.load]
// does.
func (c configFlags) loadForRun(ctx context.Context, set *flag.FlagSet, importPath string) (
	*config.Bootstrap, tierBSeed, error,
) {
	// See [configFlags.load] for why Tier A resolves environment-only.
	boot, bootErr := config.LoadBootstrap(*c.bootstrap, config.EnvOnly())
	bootErr = nameTheNeighbour(*c.bootstrap, bootErr)

	// A company.yaml copied from `GET /config` carries a mask for every inline
	// credential, so it is restored against the fleet's revision this node's
	// store last saw, as `crewlet config import` does ([config.LoadCompanyToRun],
	// [knownRevision]). Read only for a file with a mask, never from a scratch
	// store (the previous run's rows, deleted as the engine opens it), and as
	// the stored form: a newer peer may have written it.
	var prior config.PriorRevision
	if bootErr == nil && !boot.Store.Scratch {
		stored := storeAt(*c.bootstrap)
		prior = func() (*config.Company, error) {
			state, held, err := stored(ctx)
			// The claim goes back at once: the engine's boot decision is the
			// one that holds the store ([heldStore]).
			defer held.release()
			if err != nil || state.known == nil {
				return nil, err
			}
			if state.known.unopened != nil {
				return nil, state.known.unopened
			}
			company, err := config.DecodeCompany(state.known.document)
			if err != nil {
				return nil, fmt.Errorf("parse revision %s, the company this node knows: %w",
					state.known.revision.ID, err)
			}
			return company, nil
		}
	}
	// The seed resolves against the Tier A it runs under ([managedSeed]: a
	// managed deployment reads none); if Tier A failed it resolves alone so
	// both documents' problems report at once.
	seedBoot := boot
	if bootErr != nil {
		seedBoot = nil
	}
	seed, seedErr := c.resolveSeed(set, importPath, seedBoot, prior)
	// Tier A comes back even when the seed failed: `crewlet run` opens the log
	// file Tier A names, so a malformed company.yaml is recorded there. A nil
	// Bootstrap beside an error means Tier A itself failed.
	if bootErr != nil {
		return nil, tierBSeed{}, errors.Join(bootErr, seedErr)
	}
	return boot, seed, seedErr
}

// resolveSeed decides which Tier B file this invocation carries and why,
// restored against prior where it is a redacted read. boot is the loaded
// Tier A, or nil when that failed ([managedSeed]).
func (c configFlags) resolveSeed(set *flag.FlagSet, importPath string,
	boot *config.Bootstrap, prior config.PriorRevision) (tierBSeed, error) {

	named := strings.TrimSpace(importPath)
	if named != "" && isFlagSet(set, "company") {
		// Refused, not resolved by precedence: the flags ask for opposite
		// things, so any winner silently guesses about the flag that
		// overwrites a running company.
		return tierBSeed{}, errors.New(
			"-company and -import-company both name a Tier B document, and " +
				"they mean opposite things: -company bootstraps a company " +
				"that does not exist yet, -import-company replaces what the " +
				"fleet is running. Pass one")
	}
	if boot != nil && boot.API.Auth.CompanyManaged() {
		return managedSeed(boot, *c.company, isFlagSet(set, "company"), named)
	}
	// Runnable rules only ([config.LoadCompanyToRun]); where the file is
	// imported, seedCompany holds it to the admission rules first.
	if named != "" {
		company, err := config.LoadCompanyToRun(named, prior)
		if err != nil {
			return tierBSeed{}, err
		}
		return tierBSeed{Path: named, Override: true, Company: company}, nil
	}

	path := *c.company
	company, err := config.LoadCompanyToRun(path, prior)
	if err == nil {
		return tierBSeed{Path: path, Company: company}, nil
	}
	// An absent default is no error: the store is authoritative, and
	// bootstrapping by `crewlet run` then PUT /config needs a binary that
	// starts without a document. An absent named file is a typo, and booting
	// past it runs something other than what the operator named.
	if isFlagSet(set, "company") || !errors.Is(err, fs.ErrNotExist) {
		return tierBSeed{}, err
	}
	return tierBSeed{Path: path}, nil
}

// nameTheNeighbour hints at the commonest first-run failure: the bundled
// example (`examples/nimbus.config.yaml`) is named `config.yaml`, but the
// default Tier A name is `crewlet.yaml`, so the operator gets "no such file"
// about a name they never typed. A hint, not a fallback: silently loading a
// file not asked for boots the wrong document on a machine that has both.
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
	return fmt.Errorf("%w\n  (%s is there, and this build's Tier A default is %q; "+
		"pass it as `crewlet run %s` or with -config)",
		err, neighbour, defaultBootstrapPath, neighbour)
}

// Tier is which document a validation is about: a config tier or a
// repository's checks file. The set is closed: an unrecognised value must be
// refused, not fall through to "company", which would fail every field of a
// Tier A file.
type Tier string

const (
	// TierAuto picks by reading the document — see [detectTier].
	TierAuto Tier = "auto"
	// TierCompany is Tier B: the company's org, providers, integrations.
	TierCompany Tier = "company"
	// TierBootstrap is Tier A: this node's broker, store and API.
	TierBootstrap Tier = "bootstrap"
	// TierChecks is a repository's `.crewlet/checks.yaml` (gitlet.ParseChecks),
	// checkable before pushing so a typo is found on a laptop.
	TierChecks Tier = "checks"
)

// tiers is the closed set, for the flag's error and for validation.
var tiers = []Tier{TierAuto, TierCompany, TierBootstrap, TierChecks}

func (t Tier) Valid() bool {
	return slices.Contains(tiers, t)
}

// detectTier says which tier a document is, from the config structs' unique
// keys (config.CompanyKeys / config.BootstrapKeys), since operators break
// filename conventions and a restated list drifts.
//
// A checks file is `checks:` beside no key either tier owns
// (TestTheTierVocabulariesShareNoKey); a typo'd key beside it is refused by
// its strict reading. An undecidable document is an error naming -tier, never
// a guess, which would report every field invalid.
func detectTier(raw []byte) (Tier, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// Not an error here: the validator that runs reports the parse
		// failure with line numbers, and Company is the likelier document.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return TierCompany, nil
	}
	company := 0
	bootstrap := 0
	companyKeys, bootstrapKeys := config.CompanyKeys(), config.BootstrapKeys()
	for key := range doc {
		switch {
		case slices.Contains(companyKeys, key):
			company++
		case slices.Contains(bootstrapKeys, key):
			bootstrap++
		}
	}
	_, checks := doc[checksFileKey]
	switch {
	case company > bootstrap:
		return TierCompany, nil
	case bootstrap > company:
		return TierBootstrap, nil
	case company == 0 && checks:
		return TierChecks, nil
	default:
		return "", fmt.Errorf(
			"cannot tell which tier this document is: it carries %d key(s) "+
				"only a company has and %d only a bootstrap has. Name it with "+
				"-tier company, -tier bootstrap or -tier checks", company, bootstrap)
	}
}

// checksFileKey is the one key a repository's checks file holds, which no
// config tier has.
const checksFileKey = "checks"

// validation is what one `crewlet validate` run found. Its shape is the JSON,
// and the text renderer reads this struct too, so the two modes cannot drift.
// Problems and warnings are [config.Problem] and [config.Warning], the shape
// the configuration API reports too.
type validation struct {
	Valid    bool             `json:"valid"`
	Tier     Tier             `json:"tier"`
	File     string           `json:"file,omitempty"`
	Problems []config.Problem `json:"problems"`

	// Warnings are valid configurations worth knowing about before applying
	// (a declined fsync's window, a trim that never advances, a dangling
	// reference). Their own list, not flagged problems: the exit code turns on
	// Problems alone, since a warning that could fail a build gets suppressed.
	Warnings []config.Warning `json:"warnings"`

	Summary map[string]any `json:"summary,omitempty"`
}

// newValidation is a run with nothing found; the lists are empty, not null.
func newValidation(tier Tier, file string) validation {
	return validation{Tier: tier, File: file, Problems: []config.Problem{}, Warnings: []config.Warning{}}
}

// validateConfigs is `crewlet validate`. An operator names one file and reads
// a sentence; an authoring loop (a model editing until valid) reads -json,
// where every problem carries its field's exact path, so it never parses
// prose. The two-flag form (-config plus -company) checks both tiers at once,
// for CI.
func validateConfigs(args []string, stdout, stderr io.Writer) error {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	tier := fs.String("tier", string(TierAuto),
		"which document a positional file is: auto, company, bootstrap or checks (a repository's "+
			gitlet.ChecksFilePath+")")
	asJSON := fs.Bool("json", false,
		"emit {valid, tier, file, problems, warnings, summary} instead of prose")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Leftovers are refused: flag parsing stops at the first non-flag token,
	// so `crewlet validate company.yaml -json` would discard both and print
	// success about ./crewlet.yaml and ./company.yaml instead.
	file, given := onePositional(fs, file)
	if given > 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet validate [<file.yaml>] [-tier auto|company|bootstrap|checks] [-json]\n"+
				"   or: crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]")
		return errors.New("name at most one document")
	}
	if !Tier(*tier).Valid() {
		return fmt.Errorf("-tier %q is not one of %s", *tier, words.Or(tiers))
	}
	if file != "" {
		return validateOne(file, Tier(*tier), *asJSON, stdout)
	}
	return validateBoth(cfg, *asJSON, stdout)
}

// validateOne checks the single document an operator named.
func validateOne(file string, tier Tier, asJSON bool, stdout io.Writer) error {
	res := newValidation(tier, file)
	raw, err := os.ReadFile(file)
	if err != nil {
		res.Problems = config.Problems(err)
		return report(stdout, res, asJSON)
	}
	if tier == TierAuto {
		detected, detErr := detectTier(raw)
		if detErr != nil {
			res.Problems = config.Problems(detErr)
			return report(stdout, res, asJSON)
		}
		res.Tier = detected
	}

	if res.Tier == TierChecks {
		// Read as the home Gitlet node reads it: one parser, so a file that
		// passes here is one Gitlet runs, faults located at field and line.
		checks, checksErr := gitlet.ParseChecks(raw)
		if checksErr != nil {
			res.Problems = config.Problems(checksErr)
			return report(stdout, res, asJSON)
		}
		names := make([]string, len(checks.Checks))
		for i, c := range checks.Checks {
			names[i] = c.Name
		}
		res.Valid = true
		res.Summary = map[string]any{"checks": len(names), "names": names}
		return report(stdout, res, asJSON)
	}

	if res.Tier == TierBootstrap {
		// Tier A resolves from the environment alone, as a rule: it holds
		// the store's address and keys, so a resolver reaching the secret
		// store would read from the store Tier A describes.
		boot, bootErr := config.ParseBootstrap(raw, config.EnvOnly())
		if bootErr != nil {
			res.Problems = config.Problems(bootErr)
			return report(stdout, res, asJSON)
		}
		res.Valid = true
		// Tier A's warnings are no refusals, but here they can still change
		// the decision before deploy; `crewlet run` logs the same list at
		// start ([logBootstrapAdvisories]). Appended to the empty list, not
		// assigned: [config.Bootstrap.Warnings] is nil when empty, and the
		// payload promises an array.
		res.Warnings = append(res.Warnings, engine.BootstrapWarnings(boot)...)
		res.Summary = map[string]any{
			"stream": boot.Stream.Type, "coordination": boot.Coordination.Type,
			"store": boot.Store.Path, "roles": boot.Node.Roles,
		}
		return report(stdout, res, asJSON)
	}

	company, warnings, companyErr := checkCompany(raw)
	res.Warnings = warnings
	if companyErr != nil {
		res.Problems = config.Problems(companyErr)
		return report(stdout, res, asJSON)
	}
	res.Valid = true
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(company.epoch.Seats()),
		"llm_providers": len(company.epoch.Models.Keys()),
	}
	return report(stdout, res, asJSON)
}

// checkedCompany is a company document that passed every check, with the
// epoch built from it.
type checkedCompany struct {
	*config.Company
	epoch *engine.Company
}

// checkCompany holds a Tier B document to every rule a submitted one is held
// to, then builds its epoch, which dials nothing and catches what a schema
// cannot (a seat whose llm names no configured provider).
//
// A document that failed but parsed reports its references alone, so a
// misspelled lead shows in the same pass. The admission half of
// [config.Company.Warnings] and the advisories stay off that path (an
// admission rule is already a problem, and advisories are noise beside a
// refusal); a document that held up carries the whole set.
func checkCompany(raw []byte) (*checkedCompany, []config.Warning, error) {
	company, err := config.ParseCompanyDocument(raw)
	if err != nil {
		return nil, []config.Warning{}, err
	}
	warnings := company.ReferenceWarnings()
	if err = errors.Join(company.Validate(), company.ValidateHost(os.Geteuid())); err != nil {
		return nil, warnings, err
	}
	epoch, err := engine.NewCompany(company)
	if err != nil {
		return nil, warnings, err
	}
	return &checkedCompany{Company: company, epoch: epoch}, company.Warnings(), nil
}

// validateBoth is the two-flag form: both tiers, reporting both's problems,
// not just the first to fail.
func validateBoth(cfg configFlags, asJSON bool, stdout io.Writer) error {
	res := newValidation(TierAuto, "")
	// Environment-only resolution for Tier A: see validateOne.
	boot, bootErr := config.LoadBootstrap(*cfg.bootstrap, config.EnvOnly())
	bootErr = nameTheNeighbour(*cfg.bootstrap, bootErr)

	var company *checkedCompany
	raw, companyErr := os.ReadFile(*cfg.company)
	if companyErr == nil {
		company, res.Warnings, companyErr = checkCompany(raw)
	}
	if err := errors.Join(bootErr, companyErr); err != nil {
		res.Problems = config.Problems(err)
		return report(stdout, res, asJSON)
	}
	// No rule needs both documents: every node runs its logs from boot, so
	// the in-memory-stream rule is Tier A's alone ([config.Stream.Durable]).
	res.Valid = true
	// Both tiers' warnings. The company's are already here (checkCompany
	// puts them there so a refused document reports them), so Tier A's go in
	// front, the order the files are named in, and the warnings neither tier
	// raises alone ([config.TierWarnings]) between. Inserted, not appended
	// onto Tier A's nil list: `append(nil)` of nothing stays nil, and the
	// payload promises arrays.
	res.Warnings = slices.Insert(res.Warnings, 0,
		slices.Concat(engine.BootstrapWarnings(boot),
			config.TierWarnings(boot, company.Company))...)
	// What only the two together say of this host: a Tier A bind no container
	// box can reach, for a company running code work in one. Judged for the
	// validating host, which a single-node install runs.
	res.Warnings = append(res.Warnings,
		engine.ContainerReachWarnings(boot, company.Company, runtime.GOOS)...)
	// And every line an integration's own credential replaces in an agent's box.
	res.Warnings = append(res.Warnings, engine.BoxCredentialWarnings(company.Company)...)
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(company.epoch.Seats()),
		"llm_providers": len(company.epoch.Models.Keys()),
		"stream":        boot.Stream.Type, "coordination": boot.Coordination.Type,
	}
	return report(stdout, res, asJSON)
}

// report renders a validation and returns the run's exit disposition. A
// failed validation exits non-zero in both modes, since a CI gate on -json
// would pass if {"valid": false} exited 0. A warning never fails it: the
// engine runs a document with a dangling reference, and refusing one would
// refuse a company assembled in pieces.
func report(stdout io.Writer, res validation, asJSON bool) error {
	if asJSON {
		if err := printJSON(stdout, res); err != nil {
			return err
		}
		if res.Valid {
			return nil
		}
		// Silent: the payload already carries every problem.
		return errSilent
	}
	// Warnings go on stdout, before the outcome: they are part of what the
	// run found, and read whichever way validation went.
	for _, w := range res.Warnings {
		fmt.Fprintln(stdout, warningLine(w))
	}
	if !res.Valid {
		return errors.New(strings.Join(problemLines(res.Problems), "\n  "))
	}
	fmt.Fprintln(stdout, summaryLine(res))
	return nil
}

// problemLines is the problems as lines an operator reads: one per message,
// led by every path it was placed at, so a duplicate shared by several
// entities is one paragraph, not one per entity.
func problemLines(problems []config.Problem) []string {
	var order []string
	paths := map[string][]string{}
	for _, p := range problems {
		if _, seen := paths[p.Message]; !seen {
			order = append(order, p.Message)
		}
		if p.Path != "" {
			paths[p.Message] = append(paths[p.Message], p.Path)
		} else if paths[p.Message] == nil {
			paths[p.Message] = []string{}
		}
	}
	lines := make([]string, len(order))
	for i, message := range order {
		lines[i] = prose(strings.Join(paths[message], ", "), message)
	}
	return lines
}

// prose is a message led by where it is, unless it already opens with that;
// the paths are what the operator can search their file for.
func prose(where, message string) string {
	if where == "" || strings.HasPrefix(message, where+": ") {
		return message
	}
	return where + ": " + message
}

// warningLine is one warning led by its kind: a dangling reference is broken,
// an advisory a valid choice reporting its cost, an admission rule a stored
// revision a new write would refuse. Under one `warning:` the broken one,
// rarer than advisories, gets skimmed past.
func warningLine(w config.Warning) string {
	if kind := warningKind(w.Kind); kind != "" {
		return "warning (" + kind + "): " + prose(w.Path, w.Message)
	}
	return "warning: " + prose(w.Path, w.Message)
}

// warningKind is a [config.Warning]'s kind in words. An unclassified kind
// renders as its wire value, never a blank pair of brackets.
func warningKind(kind string) string {
	switch kind {
	case config.WarningDanglingReference:
		return "dangling reference"
	case config.WarningAdmission:
		return "admission"
	case config.WarningAdvisory:
		return "advisory"
	default:
		return kind
	}
}

// summaryLine is the one prose line a successful validation prints.
func summaryLine(res validation) string {
	if names, ok := res.Summary["names"].([]string); ok && res.Tier == TierChecks {
		switch len(names) {
		case 0:
			return fmt.Sprintf("%s: no checks", res.File)
		case 1:
			return fmt.Sprintf("%s: 1 check (%s)", res.File, names[0])
		}
		return fmt.Sprintf("%s: %d checks (%s)", res.File, len(names), strings.Join(names, ", "))
	}
	if name, ok := res.Summary["company"].(string); ok {
		line := fmt.Sprintf("%s: %d agent seats, %d LLM providers",
			name, res.Summary["seats"], res.Summary["llm_providers"])
		if stream, both := res.Summary["stream"]; both {
			line += fmt.Sprintf(", stream %q, coordination %q",
				stream, res.Summary["coordination"])
		}
		return line
	}
	// The prose carries every field the JSON does: most people run without -json.
	return fmt.Sprintf("%s: stream %q, coordination %q, store %q, roles %v",
		res.File, res.Summary["stream"], res.Summary["coordination"],
		res.Summary["store"], res.Summary["roles"])
}

// errSilent asks the caller to exit non-zero without printing anything more.
var errSilent = errors.New("")

// runEngine is `crewlet run`. bind opens every listener before the boot
// ([bindListeners]): [bindTCP], or a socket a test already holds ([bindFunc]).
func runEngine(args []string, stderr io.Writer, bind bindFunc) (err error) {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "console",
		"log format: console (columns and colour for a person), text or json")
	debug := fs.Bool("debug", false, "shorthand for -log-level debug")
	logFile := fs.String("log-file", "",
		"also write the log to this file, overriding logging.file.path; "+
			"an empty value writes no file")
	roles := fs.String("roles", "",
		"what this node runs, overriding node.roles: ingress, seats, workers")
	mode := fs.String("mode", string(statelog.ModeNormal),
		"normal (default), maintenance or seal. The two maintenance modes start "+
			"this node's broker and NO publisher, for a stream capacity change; "+
			"every node of the fleet has to be in the same one. See "+
			"`crewlet retention set-capacity`")
	apiHost := fs.String("api-host", "", "bind address, overriding api.host")
	apiPort := fs.Int("api-port", -1,
		"bind port, overriding api.port; 0 serves no HTTP at all")
	// On run's own set, not addConfigFlags: `validate` has nothing to
	// override, and a flag that parses and means nothing is a defect.
	importCompany := fs.String("import-company", "",
		"Tier B config to make the active revision NOW, over whatever the "+
			"fleet is running; -company only bootstraps a company that does "+
			"not exist yet")
	// A flag, never a config field: a field reaches production by being
	// copied into an image. See auth.NewDevPrincipal for the two refusals.
	devPrincipal := fs.String("dev-principal", "",
		"Development only: treat every request without a token as this "+
			"login, with every app (and admin when api.auth.admin is "+
			"serve). Only works on a development build bound to loopback")
	// Assigned, not declared: err is NAMED so a deferred recorder can log a
	// boot failure before the log file closes, and a `:=` would shadow it.
	if err = fs.Parse(args); err != nil {
		return err
	}
	// The positional is the Tier A path, and a leftover is refused: flag
	// parsing stops at the first non-flag token, so `crewlet run
	// /etc/crewlet.yaml -debug` would discard the path and boot from
	// ./crewlet.yaml without mentioning it.
	file, given := onePositional(fs, file)
	if given > 1 {
		// Every flag run registers bar -config (the positional, whose second
		// naming is the error): the only synopsis an operator sees on wrong
		// arguments, and a flag missing from it is one they do not reach for.
		fmt.Fprintln(stderr, "usage: crewlet run [<config.yaml>] "+
			"[-company <company.yaml> | -import-company <company.yaml>] "+
			"[-log-level …] [-log-format …] [-log-file …] [-debug] "+
			"[-roles …] [-mode …] [-api-host …] [-api-port …] "+
			"[-dev-principal …]")
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

	// The flags alone, first: reading the file is what `-debug` is most often
	// turned on to watch. The second call below layers the file's settings;
	// the writer is not this command's to install (see [run]).
	logging.SetVerbosity(flagLogLevel(*logLevel, *debug), logging.ParseFormat(*logFormat))
	log := logging.Get("cli")
	warnUnrecognisedLogNames(log, "flag", "-log-level", *logLevel, "-log-format", *logFormat)

	// A background context: the run's own, cancelled by a signal, is made
	// below once the log file this load names is open. The load reads only
	// this node's files, bounded by the store's busy timeout.
	boot, seed, loadErr := cfg.loadForRun(context.Background(), fs, *importCompany)
	if boot == nil {
		// Tier A itself failed: no document names a log file; stderr is the record.
		return loadErr
	}
	// The logging flags fold into the document, as -roles and the API flags
	// do ([overrideNode]), so everything reading Tier A's `logging:` block
	// reads what this run applies (including the advisory about a verbose
	// broker, which `-debug` would otherwise trip).
	if err = overrideLogging(boot, fs, *logLevel, *logFormat, *debug, *logFile); err != nil {
		return err
	}
	// Now the file's settings, which make `logging.level` take effect; lines
	// before this point came out under the flags alone.
	logging.SetVerbosity(boot.LogSettings())
	// The log file opens as soon as the document naming it is read: what can
	// still fail below (keyring, store, company activation) is what an
	// operator reads there afterwards.
	//
	// Its detach is deferred first, so under LIFO it runs last: the trace
	// flush and the drain log too, and would lose the shutdown the file
	// exists to record.
	logFileSet, _ := boot.Logging.File.LogFileSettings()
	detachLogFile, err := attachLogFile(logFileSet, boot.Logging.StderrEnabled(), os.Stderr)
	if err != nil {
		return err
	}
	defer detachLogFile()
	// The failure ending this function is recorded before the file closes
	// (registered after the detach, so LIFO runs it first): main prints the
	// error after return, when the file is shut, so a boot failure nobody
	// watched would never reach the durable record.
	defer func() {
		if err != nil {
			log.Error("run_failed", "error", err)
		}
	}()

	// The seed's own failure, now that it can be recorded.
	if loadErr != nil {
		return loadErr
	}
	// Flags override the file and apply after it loads, so a validation
	// failure names the file's value. Each applies only when given: an unset
	// -api-port is not "port 0", which serves no HTTP and deafens every
	// integration.
	if err = overrideNode(boot, fs, *roles, *apiHost, *apiPort); err != nil {
		return err
	}
	// Every other Tier A rule, over the folded document ([validateFolded]
	// says why the engine's own check is too late here).
	if err = validateFolded(boot, fs); err != nil {
		return err
	}
	// After the overrides, because the loopback refusal reads `api.host`:
	// built before, it would judge the file's bind on a run that moved it,
	// permitting the flag where -api-host opened the node to the network.
	// And before anything starts, so a run that cannot have what it asked
	// for fails here.
	dev, err := auth.NewDevPrincipal(*devPrincipal, boot)
	if err != nil {
		return err
	}
	if dev != nil {
		// Loud, every boot: the whole surface is open to this machine.
		log.Warn("api_dev_principal_enabled",
			"login", *devPrincipal, "bind", boot.API.Host,
			"hint", "every request without a token acts as this login. "+
				"This only works on a development build bound to loopback")
	}
	// The mode is a flag, never a config field: it is a property of this run
	// (every node restarts into and out of it over one capacity change), and
	// a file carrying it would leave a node refusing to publish after an
	// unrelated restart.
	nodeMode := statelog.MaintenanceMode(strings.TrimSpace(*mode))
	if !nodeMode.Valid() {
		return fmt.Errorf("-mode %q is not one of %s: the two maintenance modes "+
			"start no publisher, so a typo here would be a node that boots, "+
			"reports healthy and runs nothing", *mode, words.Or(statelog.MaintenanceModes))
	}
	// Tier A's advisories ([logBootstrapAdvisories]), said on every start only
	// here: after every refusal Tier A can draw ([validateFolded]), where a
	// warning is noise; after the log file attaches; and after every flag
	// folds in, so they describe the node this run starts.
	logBootstrapAdvisories(log, boot)
	// What the last process left for issue reports, before anything here can
	// crash: its crash output moves aside for the first revision to judge,
	// and this process's is armed if the node last applied the setting on.
	nextStart, nextErr := issuereport.OpenNextStart(filepath.Dir(boot.Store.Path), issuereport.OriginOf(version.String()))
	if nextErr != nil {
		log.Warn("issue_reports_next_start_unread", "error", nextErr.Error(),
			"hint", "the node could not read or arm its crash output; a crash of this process may go unreported")
	}

	// The engine owns the process signals exclusively: a graceful drain is
	// the difference between resuming cleanly and redelivering half-finished
	// turns. Nothing else may install a handler.
	ctx, signals := interruptContext()
	defer signals.end()

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return err
	}
	// Listeners bind before the first boot step that can take time (from
	// [decideSeed] on), so a taken port fails at once, not after a long
	// recovery. Until handed the node's surface they answer [api.Boot]:
	// /health 503 until recovery completes (so a startup probe bounds it),
	// /ready 503 throughout, and 503 `starting` with Retry-After elsewhere.
	listeners, err := bindListeners(ctx, boot, nodeID, bind, log)
	if err != nil {
		return err
	}
	// Closed on every failure before the handover, which disarms this: from
	// [serveAPI]'s return the surface owns the sockets and [shutdown] closes
	// them. Deferred, so probes answer through the teardown.
	handedOver := false
	defer func() {
		if !handedOver {
			listeners.close(context.WithoutCancel(ctx), log)
		}
	}()
	// One recorder for the process, handed to both halves: the engine writes
	// into it and tracing's MeterProvider reads it, so a collector and
	// `crewlet retention status` cannot disagree about one event.
	recorder, err := metrics.New()
	if err != nil {
		return err
	}
	// Tracing before the engine, whose subscriptions and publish listener
	// open spans; a later provider would leave the boot untraced. The flush
	// is deferred, running after every return and the drain, so the drain's
	// own spans are in the buffer when it flushes.
	flushTraces, err := tracing.Configure(ctx, tracing.Options{
		NodeID:   nodeID,
		Version:  version.String(),
		Recorder: recorder,
	})
	if err != nil {
		return err
	}
	defer func() {
		if flushErr := flushTraces(ctx); flushErr != nil {
			// Lost telemetry does not fail a shutdown that has succeeded.
			log.WarnContext(ctx, "trace_flush_failed", "error", flushErr)
		}
	}()

	// What the Tier B file is for and what the engine starts on: one decision
	// ([decideSeed]) handed to both the boot and the import, so they cannot
	// disagree. After the log file and Tier A's last refusals, so an ignored
	// seed's warning reaches the durable record; after the listeners, since
	// it opens the store. It holds the store until the engine has opened it,
	// so no offline command can replace what the engine is about to boot on;
	// the claim goes back on every way out ([bootEngine]).
	e, plan, err := bootEngine(ctx, boot, seed, storeAt(*cfg.bootstrap),
		func(ctx context.Context, opts engine.Options) (*engine.Engine, error) {
			opts.Metrics, opts.Mode, opts.NextStart = recorder, nodeMode, nextStart
			// When /health turns 200: [engine.Options.Recovered].
			opts.Recovered = listeners.recovered
			// Where a direct box dials the bridge and telemetry receiver:
			// [engine.Options.BoxHostURL].
			opts.BoxHostURL = listeners.boxHostURL(boot, nodeID)
			return engine.New(ctx, opts)
		}, log)
	if err != nil {
		// A migration that stopped the start is kept for the next successful one to report.
		if keepErr := nextStart.MigrationFailed(err); keepErr != nil {
			log.Warn("issue_reports_migration_unkept", "error", keepErr.Error())
		}
		return err
	}

	// The company's chat presence hub, on every node: a seat raises its
	// working indicator on whichever node its turn runs, so the hub is the
	// process's, not the API's; built after the engine, started before it is
	// attached through its setter. serveAPI mounts the socket over it. See
	// [api.EngineChatLive].
	chatLive := api.NewEngineChatLive(ctx, e)
	// abandon takes down what a boot that failed before the engine started
	// had brought up: the hub before the engine whose appliers call it.
	abandon := func() {
		chatLive.Stop(context.WithoutCancel(ctx))
		e.Stop(context.WithoutCancel(ctx))
	}

	// One keyring for the process, the engine's: the reconciler opens
	// revisions under it, so the config surface and the seed seal with it.
	cipher := e.Cipher()

	// The store is authoritative at runtime; the file is a seed (without the
	// seed a first run has nothing to activate, without the store a PUT
	// /config on one node is invisible to the rest). Converged before Start,
	// so seats are claimed under the epoch this node will serve; on a node
	// booted unconfigured to import its file, the first reconcile builds it.
	reconciler, err := startReconciler(ctx, e, boot, plan, cipher, log)
	if err != nil {
		abandon()
		return err
	}

	// The posture reaches peers from here (the reconciler owns it and exists
	// only after the engine); set before Start so the first heartbeat carries it.
	e.SetPosture(func(ctx context.Context) (string, error) {
		// ReadPosture, not Posture: the fail-open serve is the probe's
		// answer, never one to broadcast to the fleet.
		posture, readErr := reconciler.ReadPosture(ctx)
		return string(posture), readErr
	})

	// And the gate that acts on it: reporting a posture is not applying it,
	// and without this a `shed` reached the dashboard and probe but the
	// inbound edge, scheduler and seat inboxes still admitted work. Set
	// before Start, so it is live from the first delivery.
	e.SetAdmits(reconciler.Admits)

	// The config surface is built here, not in serveAPI, so a worker-only
	// node with `api.port: 0` still gets a config writer: the reconcile loop
	// is a fleet singleton that can land on it. It needs no listener, so
	// every node that holds the estate builds it and serveAPI shares the one
	// instance with the REST routes and socket queries. A node without
	// `data` builds none (reconcile is a `workers` duty, the API an
	// `ingress` surface, both needing `data`).
	configSurface, err := companySurface(e, boot, cipher)
	if err != nil {
		abandon()
		return err
	}
	// Until the DISCONNECT surface is set, the loop refuses a disconnect
	// rather than tearing down at the third-party app and leaving the block.
	if configSurface != nil {
		e.UseConfigWriter(engineConfigWriter{surface: configSurface})
		// The company moves onto per-seat access before seats start, on
		// every estate node (the first writer wins the revision's
		// compare-and-set). Safe because of the revision's gate, not this
		// order: no seat is acted on until the document marks the
		// integration migrated, so an early pass or an unanswering vendor
		// changes nothing.
		migrateSeatAccessOnce(ctx, e, configSurface,
			fleetsecrets.New(e.Backends().Fleet, cipher), log)
	}

	// One process is engine and API, sharing one broker and store; every
	// inbound webhook arrives through the API. Served before the engine
	// starts, because seat claiming waits for per-role MCP children (21 on
	// the Nimbus example) and the inbound edge would go unserved meanwhile.
	// Nothing here needs a started engine: /health reports no seats yet and a
	// webhook in the window is retained (mailboxes exist before claiming; see
	// Node.Start).
	surface, err := serveAPI(ctx, boot, e, reconciler, cipher, configSurface,
		chatLive.Hub(), dev, listeners, log)
	if err != nil {
		abandon()
		return err
	}
	handedOver = true

	if err = e.Start(ctx); err != nil {
		// Everything the engine opened comes down, or held seat leases stay
		// until the TTL and no peer can take those seats. In [shutdown]'s one
		// order: the listener is already accepting, and the drain, not a
		// closed port, refuses new work on it.
		shutdown(context.WithoutCancel(ctx), e, surface, chatLive, log)
		return err
	}

	// Started only once serving: a mid-boot reconcile would apply an epoch to
	// a node that has claimed nothing.
	go reconciler.Run(ctx)

	// Keeps looking: an integration left gated because its vendor did not
	// answer, or connected since, is migrated by a later attempt.
	if configSurface != nil {
		go keepMigratingSeatAccess(ctx, e, configSurface,
			fleetsecrets.New(e.Backends().Fleet, cipher), log)
		// Lapsed agent grants leave the document once no credential holds
		// them, with the requests nobody answered.
		go keepSweepingGrants(ctx, e, configSurface, log)
	}

	// A node stops for a signal or because the engine lost something it
	// cannot run without, a broker connection NATS closed for good
	// ([engine.Engine.Fatal]). Both take the one shutdown below, and a fatal
	// RETURNS its cause so main exits non-zero and the supervisor restarts
	// the node, rather than it running on healthy to a liveness probe with
	// nothing to publish over.
	var fatal error
	select {
	case <-ctx.Done():
		// Signals were handed back before cancel ([interrupts]), so the
		// second interrupt the drain line promises works.
	case <-e.Fatal():
		fatal = e.FatalCause()
		// No signal arrived, so the hold is still on its first tier: handed
		// back here, the operator's first interrupt ends the drain rather
		// than cancelling a context it is already detached from.
		signals.handBack()
		log.ErrorContext(ctx, "engine_fatal", "error", fatal,
			"detail", "this node cannot go on running: it stops the way a signal "+
				"stops it and exits non-zero, for whatever supervises it to start "+
				"it again")
	}

	// Said before the drain, which can take minutes: what the node waits for
	// and how to stop waiting. The probe text is conditional on a listener
	// (api.port 0 is a real posture, [serveAPI]'s api_disabled line; a promised
	// /health would be a refused connection), and the work-route text on any
	// existing: a node without ingress serves only probes.
	probes := "this node binds no HTTP listener, so its drain is visible " +
		"only in this log"
	finish := "the turns already running finish"
	switch {
	case surface != nil && surface.app != nil:
		probes = "/health stays 200, /ready answers 503 and every route " +
			"that would start new work answers 503"
		finish += ", and then the git requests it is serving,"
	case surface != nil:
		probes = "/health stays 200 and /ready answers 503"
	}
	// What it waits for depends on which connection a fatal lost: with the
	// queue's own gone no turn is waited for (they are cancelled), and git
	// requests are waited for either way ([shutdown]). Which press ends it
	// depends on the cause: after a signal the second interrupt, after a
	// fatal the first.
	press := "a second interrupt"
	if fatal != nil {
		press = "an interrupt"
	}
	acksLost := false
	select {
	case <-e.AcksLost():
		acksLost = true
	default:
	}
	draining := drainLine(e.Backends().Queue.InFlightCount(), drainCase{acksLost: acksLost,
		ownDown: !acksLost && fatal != nil && e.OwnLinkDown(), serving: surface != nil && surface.app != nil},
		finish, probes, press)
	log.InfoContext(ctx, "engine_draining", draining...)

	// Detached from the signal context, already cancelled: the drain is bounded
	// only by its context, and that one would abandon every running turn. The
	// bound is the supervisor's (kill grace, or the next interrupt:
	// [interrupts]).
	//
	// On a fatal it still ends. With the queue's connection gone it cancels
	// running turns at once ([node.Node.Drain]): their acks cannot land, a peer
	// reruns them, and finishing would double side effects. With only the
	// coordination store gone acks land, so it waits and turns end as their
	// lease and ledger writes fail.
	shutdown(context.WithoutCancel(ctx), e, surface, chatLive, log)
	if fatal != nil {
		return fmt.Errorf("the node stopped itself: %w", fatal)
	}
	return nil
}

// drainCase is what the engine_draining line depends on: whether a fatal lost
// the queue's own connection ([engine.Engine.AcksLost]), whether that one is
// down while another's loss is handled ([engine.Engine.OwnLinkDown]), and
// whether the node serves git requests a drain still waits for.
type drainCase struct {
	acksLost, ownDown, serving bool
}

// drainLine is the engine_draining line's fields: what the drain waits for and
// how to stop waiting. With the queue's own connection gone no turn is waited
// for. ANOTHER CONNECTION GONE FIRST while the queue's own is down too, as both
// to one server go when it refuses the node's credentials, leaves the queue's
// own to decide, which it has not yet.
func drainLine(inFlight int, c drainCase, finish, probes, press string) []any {
	switch {
	case c.acksLost:
		detail := "this node's own broker connection is closed for good, so " +
			"nothing the turns still running conclude can be acknowledged and " +
			"whichever node takes each seat runs it again: they are cancelled " +
			"rather than waited for, and the drain ends at once"
		if c.serving {
			detail += "; the git requests it is serving still finish before " +
				"this node stops"
		}
		return []any{"in_flight", inFlight, "detail", detail}
	case c.ownDown:
		return []any{"in_flight", inFlight,
			"detail", "this node's own broker connection is down too: if it is closed " +
				"for good, the turns still running are cancelled rather than waited for " +
				"and the drain ends at once, and if it comes back " + finish +
				" before this node stops"}
	}
	return []any{"in_flight", inFlight,
		"detail", finish + " before this node stops; until they have, " + probes,
		"hint", press + " exits at once, leaving those turns to be " +
			"redelivered once their ack window elapses"}
}

// shutdown is the one order a node comes down in, whatever stopped it:
//  1. DRAIN WITH THE LISTENER UP: /health and /ready keep answering while turns
//     finish, and the drain gate refuses routes that would start new work.
//  2. FINISH THE GIT REQUESTS ([api.App.DrainGit]), listener up: a clone or push
//     a close cut restarts from nothing on a peer. Peers' upload RPCs are
//     still served; the supervisor's grace is the wait's only bound.
//  3. CLOSE THE LISTENER, not before: that fails liveness during a healthy drain.
//  4. STOP THE CHAT HUB before the engine, whose appliers would announce into
//     a hub that no longer drains; detached first, with the indicator driver.
//  5. TEAR THE ENGINE DOWN: probes and git requests read the stores it closes.
//
// A nil surface or hub is skipped; [TestShutdownTakesANodeDownInOneOrder] pins it.
func shutdown(ctx context.Context, e drainingEngine, surface listeners,
	chatLive stoppingHub, log *slog.Logger) {

	e.Drain(ctx)
	surface.finishGit(ctx)
	surface.stop(ctx, log)
	chatLive.Stop(ctx)
	e.Stop(ctx)
}

// drainingEngine is the engine as [shutdown] takes it down.
type drainingEngine interface {
	Drain(ctx context.Context)
	Stop(ctx context.Context)
}

// listeners is the HTTP surface as [shutdown] closes it: the git requests it
// is serving finished first, then its listeners closed.
type listeners interface {
	finishGit(ctx context.Context)
	stop(ctx context.Context, log *slog.Logger)
}

// stoppingHub is the chat hub as [shutdown] stops it.
type stoppingHub interface{ Stop(ctx context.Context) }

// httpSurface is the HTTP listeners a node binds: the whole API on an ingress
// node (on api.port, with the routes outside parties call on their own public
// listener when api.public is set), else its probes on api.port alone, where
// the app, projector and public server are nil. The sign-in surface is nil
// there too and where none is served ([api.NewHumanSurfaces]), and is never
// mounted on the public listener.
type httpSurface struct {
	app       *api.App
	server    *apiListener
	public    *apiListener
	projector *observe.Projector

	// artifacts is the artifact origin's listener (api.artifacts), nil where
	// no port is set; stopArtifacts ends its handler's subscription.
	artifacts     *apiListener
	stopArtifacts func()

	// auth is the sign-in surface, whose post-answer work (a stale
	// password's rewrite) must stop before the engine its writes go to.
	auth *authapi.Service
	runs *observe.SandboxReconciler
	// seeding is the boot seeds still being read; they write into the
	// projection, so stop before the app.
	seeding *observe.Seeding
	// drainGit waits for the git requests being served ([api.App.DrainGit]);
	// nil without the ingress role.
	drainGit func(context.Context) error

	// assistant is the Dashboard Assistant's service, nil without ingress.
	// Closed before any listener so open streams end rather than holding a
	// listener's grace.
	assistant *assistantNode

	// bridge is the engine's tool bridge, nil where none is built. Shut down
	// before any listener: an executor's notes poll is held open for seconds
	// (internal/api/mcpbridge) and would spend a listener's grace.
	bridge *mcpbridge.Bridge
}

// finishGit waits for the git requests this surface is serving, once the
// engine has drained ([shutdown], [api.App.DrainGit]). Nil-safe like
// [httpSurface.stop].
func (s *httpSurface) finishGit(ctx context.Context) {
	if s == nil || s.drainGit == nil {
		return
	}
	_ = s.drainGit(ctx)
}

// apiListener is one socket a node binds, the server serving it, and the slot
// holding what that server answers with.
//
// The socket is kept beside the server because the server does not hold it
// until the background Serve ([listenAPI]) registers it, and Shutdown closes
// only registered listeners: an earlier stop (every failed public bind) left
// api.port accepting. [shutdownListener] closes the socket itself.
//
// One server for the process, with a changing handler ([handlerSlot]): bound
// before the boot, served the boot surface, then handed the node's own
// ([apiListener.serve]). A started Server's Handler must not change, a second
// server races Accept, and a Shutdown would close the needed socket.
type apiListener struct {
	server  *http.Server
	socket  net.Listener
	handler *handlerSlot
	// served is closed once the goroutine serving the socket has returned.
	served <-chan struct{}
	// unused is every connection that has sent no request, closed as
	// shutdown begins.
	unused *unusedConns
}

// serve hands this listener the handler it answers with from now on. A request
// already being answered finishes under the handler it began under.
func (l *apiListener) serve(h http.Handler) { l.handler.store(h) }

// stop closes the HTTP surface, once the engine has drained ([shutdown]).
//
// The public listener goes before api.port: the probes are on api.port, and
// closing it while the public one spends its grace (an open bridge session
// spends it whole, [apiShutdownGrace]) would fail liveness on a healthy
// node. Each gets its own grace.
func (s *httpSurface) stop(ctx context.Context, log *slog.Logger) {
	if s == nil {
		return
	}
	// The assistant before every listener: each turn it runs ends
	// interrupted, which ends its streams, requests that would spend a
	// listener's grace whole. The engine is not torn down yet.
	closeAssistant(ctx, s.assistant, log)
	// Every held poll for a run's notes likewise, served here or relayed to
	// the session's holder: released at once as a 503 the harness pauses on
	// before polling the seat's next node. A relayed tool call or read
	// finishes within the grace.
	s.bridge.Shutdown()
	// The artifact origin first: it serves no probe, and an artifact frame
	// is the request most able to wait for a departing node.
	if s.artifacts != nil {
		shutdownListener(ctx, s.artifacts, "artifacts", log)
	}
	if s.stopArtifacts != nil {
		s.stopArtifacts()
	}
	if s.public != nil {
		shutdownListener(ctx, s.public, "public", log)
	}
	if s.server != nil {
		shutdownListener(ctx, s.server, "api", log)
	}
	grace, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	// After the listeners, so no socket reads the projection while its feed
	// is torn down, and after the drain, so a dashboard saw the turns finish.
	// Nil without ingress.
	if s.projector != nil {
		s.projector.Stop(grace)
	}
	// Nil-safe: nothing to keep or seed without ingress.
	s.seeding.Stop()
	s.runs.Stop()
	if s.app != nil {
		s.app.Stop(grace)
	}
	// After the listener, so no request starts another rewrite, and before
	// [shutdown] tears down the engine it writes through, within the rest of
	// the same grace.
	if s.auth != nil {
		s.auth.Stop(grace)
	}
	log.InfoContext(ctx, "api_stopped")
}

// shutdownListener closes one listener, waiting at most [apiShutdownGrace] for
// its requests, and returns once the socket is closed and the goroutine
// serving it has ended. listener is the name the log uses.
//
// The server's own Shutdown is not enough: it closes only listeners Serve
// has registered, and [listenAPI] starts Serve on a goroutine. A stop that
// won that race (a failed public bind) left the socket accepting on a node
// that had reported failure (42-61% of immediate stops on 4 cores, Go
// 1.27.2). So it closes the socket after the shutdown, then waits for the
// goroutine, which returns [http.ErrServerClosed] once the server is
// stopping; the wait has no bound and cannot come first.
func shutdownListener(ctx context.Context, l *apiListener, listener string, log *slog.Logger) {
	grace, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	if err := l.server.Shutdown(grace); err != nil {
		// Grace over with a request still running: it is cut, cancelling its
		// context, rather than left to read a store and broker about to close.
		log.WarnContext(ctx, "api_shutdown_failed", "listener", listener, "error", err,
			"detail", "requests still running after the shutdown grace were cut")
		if closeErr := l.server.Close(); closeErr != nil {
			log.WarnContext(ctx, "api_close_failed", "listener", listener, "error", closeErr)
		}
	}
	// The socket, registered by Serve or not ([apiListener]); already closed
	// is ordinary, and a later Serve finds the server shutting down.
	if err := l.socket.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.WarnContext(ctx, "api_close_failed", "listener", listener, "error", err)
	}
	// The serving goroutine belongs to this stop: nothing of this listener
	// runs into the teardown or logs after it.
	<-l.served
}

// apiShutdownGrace bounds how long a listener waits for in-flight requests
// (not turns; the drain waits for those), then cuts what is left. Five
// seconds covers a REST read or a webhook's signature check plus publish, so
// only a client that stopped reading spends it.
//
// [mcpbridge.PathPrefix] sessions have no idle timeout
// ([mcpbridge.Bridge.Handler]) because a harness goes quiet for a build, so
// stopping with one open spends the grace and logs api_shutdown_failed,
// honestly: this process never resumes that run. The /operator/mcp stream
// ends as shutdown begins ([api.App.EndListens]); idle connections close
// ([unusedConns]).
const apiShutdownGrace = 5 * time.Second

// seedPauses puts every paused seat on the live projection, by the agent id the
// projection keys seats by, and pushes the seats it moved to every open socket.
// A pause whose seat the company no longer has is skipped: the activation that
// removed the seat clears it.
func seedPauses(ctx context.Context, e *engine.Engine, live *stream.Service) error {
	pauses, err := e.Backends().Fleet.ListSeatPauses(ctx)
	if err != nil {
		return err
	}
	company := e.Company()
	if company == nil || company.Org == nil {
		// Read all the same: the projection learns the record was read (an
		// item's flow waits on it; livestate's posture.go).
		live.SeedPauses(nil)
		return nil
	}
	seeds := make([]livestate.SeedPause, 0, len(pauses))
	for _, p := range pauses {
		// Keyed by the seat's id, derived from its handle (ADR-0013).
		if company.Org.AgentSeatByID(p.Seat) == nil {
			continue
		}
		seeds = append(seeds, livestate.SeedPause{AgentID: p.Seat.String(), Paused: livestate.Paused{
			By: p.By, ByKind: p.ByKind, OperatorID: p.OperatorID,
			At: p.At.UTC().Format(time.RFC3339Nano), Reason: p.Reason, StopRunning: p.StopRunning,
		}})
	}
	live.SeedPauses(seeds)
	return nil
}

// companyConfig is the company this node serves: the stored document and the
// org built from it, or nil for both. One epoch read gives both halves,
// since two reads can straddle an activation and describe a company that
// never existed.
func companyConfig(e *engine.Engine) (*config.Company, *org.Organization) {
	if company := e.Company(); company != nil {
		return company.Config, company.Org
	}
	return nil, nil
}

// seatDisplayName is the name the running org gives the seat a handle
// addresses, or the handle itself for a seat it does not hold (a retired one,
// or a node serving no company yet), read at the moment of asking so a rename
// is exported as it now reads.
func seatDisplayName(e *engine.Engine, handle string) string {
	if _, o := companyConfig(e); o != nil {
		if seat := o.SeatByHandle(handle); seat != nil && seat.Name != "" {
			return seat.Name
		}
	}
	return handle
}

// artifactDataSurfaces builds the datastore's HTTP surface, its read
// questions' source and the socket's artifact rooms over the engine's estate
// router, or none on an engine with no router. All decide through the
// artifacts' own gate ([artifactsGate]), so one admission says who may touch
// one artifact.
func artifactDataSurfaces(e *engine.Engine, boot *config.Bootstrap) (
	*artifactdataapi.Service, *queries.ArtifactDataSource, *api.Rooms, error) {

	data, routed := engine.ArtifactDataEstate(e)
	heads, headsRouted := engine.ArtifactsEstate(e)
	if !routed || !headsRouted {
		return nil, nil, nil, nil
	}
	gate := artifactsGate(e, boot, heads)
	surface, err := artifactdataapi.New(artifactdataapi.Options{Gate: gate, Data: data, Versions: heads,
		Names: func(handle string) string { return seatDisplayName(e, handle) }})
	if err != nil {
		return nil, nil, nil, err
	}
	var rooms *api.Rooms
	// Rooms need the presence hub (every node with a broker runs one); none, no rooms.
	if hub := e.Presence(); hub != nil {
		rooms = &api.Rooms{Gate: gate, Stores: data, Presence: hub, Feed: e}
		if m := e.Recorder(); m != nil {
			rooms.Metrics = m
		}
	}
	return surface, &queries.ArtifactDataSource{Gate: gate, Reads: data}, rooms, nil
}

// companySecrets reads verification material from the engine's current epoch
// on every request: a receiver holding a captured one would keep rejecting
// deliveries signed with a rotated secret, looking like an attack until restart.
func companySecrets(e *engine.Engine) webhooks.Secrets { return e.WebhookSecrets() }

// companySurface is the config surface a node writes the company document
// through, or nil on a node that holds no estate (see [runEngine]): the
// surface refuses a write taking away a held human seat, and without `data`
// there is no identity directory to ask, so [configapi.New] would refuse it.
func companySurface(e *engine.Engine, boot *config.Bootstrap,
	cipher secrets.Cipher) (*configapi.Service, error) {

	if !engine.HoldsEstate(boot) {
		return nil, nil
	}
	return configapi.New(configapi.Options{
		Store: e.Backends().Store, Cipher: cipher,
		// Each MCP server's OAuth grant, which removing the server revokes.
		Grants: e,
		// The activation pointer; without it every /config write reached a nil plane.
		Plane: e.Backends().Fleet,
		// The nudge, so a change lands on every node in milliseconds, not
		// at the next reconcile poll.
		Queue: e.Backends().Queue,
		// The directory, so a write taking away a held human seat is refused
		// naming the holder. Every estate node runs the identity domain.
		Holders: configapi.DirectoryHolders(e.IAM()),
		// Who alone may change a managed document (ADR-0030): Tier A's writer
		// list, the one field of Tier A this surface reads.
		CompanyWriters: boot.API.Auth.CompanyWriters,
		// A company's cross-tier consequences, in a write's answer
		// ([configapi.Options.TierWarnings]). The explicit budgets only
		// ([config.TierWarnings]), as `crewlet validate` compares; what the
		// node resolved is the apply's log line ([config.NodeTierWarnings]).
		TierWarnings: func(c *config.Company) []config.Warning {
			return append(config.TierWarnings(boot, c), engine.BoxCredentialWarnings(c)...)
		},
		// The operator's sign-in limits, refused at this node's door (ADR-0152).
		SignInLimits: boot.API.Auth.Limits,
		// The engine's name for this node, never boot.Node.ID, which is empty
		// when named through CREWLET_NODE_ID; /health and the fleet answer
		// name it the same way.
		Node: e.Node().ID(),
		// Which agent seats a write raises to an administration role at a
		// vendor (D10), so every authored write asks for a fresh step-up.
		Raises: e.SeatAccessRaises,
		// The credential store /secrets writes: removing an MCP server offers
		// the values only it read and deletes those the caller names.
		Secrets: fleetsecrets.New(e.Backends().Fleet, cipher),
	})
}

// serveAPI hands the listeners [bindListeners] bound the node's HTTP surface,
// or reports that this node serves none. It binds nothing: it builds the
// surface and stores it into each listener's handler slot, the whole
// handover. A failure leaves both listeners with the caller, which closes
// them.
//
// chatLive is the process's chat hub, mounted under the socket only where
// this node serves the API, which tells the hub its node shows anybody what
// it hears.
func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, cipher secrets.Cipher,
	configSurface *configapi.Service, chatLive *stream.ChatHub,
	dev *auth.DevPrincipal, listeners *nodeListeners, log *slog.Logger,
) (*httpSurface, error) {
	if boot.API.Port == 0 {
		// A real posture; saying so lets an operator expecting an integration or
		// probe learn it here, not from a silent webhook or restart loop. What it
		// gave up depends on its roles: with ingress, every integration is
		// deaf; without, a seat-running node's direct boxes lose their
		// loopback routes and reach tools only at the fleet's address through
		// an ingress node. Roles are read off Tier A, the presence lease's
		// answer (the profile's id is the one field not needed).
		hint := "api.port is 0, so this node binds no listener: no " +
			"orchestrator can probe it (/health, /ready)"
		switch profile := boot.Profile(""); {
		case profile.RunsIngress():
			hint = "api.port is 0, so this node serves no dashboard, no REST " +
				"API, no webhook endpoint, no probe and no executor tool " +
				"bridge; every integration is deaf here"
		case profile.RunsSeats():
			hint += ", and its direct boxes have no listener of this node's to " +
				"dial, so they reach their tools at " + mcpbridge.BaseURLVar +
				" alone"
		}
		// Warn only where nothing else did: an ingress node whose DECLARED
		// roles name ingress got the `api.port` advisory already
		// ([logBootstrapAdvisories]), and a fact warned twice teaches
		// skimming. That advisory omits undeclared node.roles on purpose
		// (every role, also the single-process shape before a port is
		// chosen), so that node is warned here. Others get info.
		level := slog.LevelInfo
		if boot.Node.Roles == nil {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, "api_disabled", "hint", hint)
		return nil, nil
	}
	if listeners == nil {
		return nil, fmt.Errorf("api: api.port is %d and no listener was bound for it; "+
			"bindListeners binds it before the boot", boot.API.Port)
	}

	// The name the engine runs under, not a second resolution: its presence
	// lease carries this value, so /health, the fleet answer and a backup's
	// trim name this node as its peers do.
	nodeID := e.Node().ID()
	// The ingress role decides, not only the port: a seats-only node must not
	// answer webhooks, the dashboard and REST. Except its probes, on api.port
	// so an orchestrator can restart a wedged node, and the two routes its
	// direct boxes dial (tool bridge, telemetry receiver) on this host's
	// loopback ([nodeListeners.boxHostURL]). Every other box dials the
	// fleet's bridge address and the ingress node relays the call over the
	// broker (internal/api/mcpbridge). See [api.Probes].
	if !boot.Profile(nodeID).RunsIngress() {
		return serveProbes(ctx, boot, e, reconciler, listeners, log)
	}
	// The config surface is the caller's, built early so a node with no HTTP
	// listener still has a config writer (runEngine). One instance serves
	// REST, socket queries and disconnects, sealing with the same keyring the
	// reconciler applies through: a second cipher would write revisions no
	// node can read.
	//
	// The identity estate (rotation's second half; who holds a seat for
	// /config and /health) is opened on every estate node, which a node
	// serving this API always is. An engine holding none did not come from
	// engine.New: refused by name, not handed on as nil.
	directory, keyring := e.IAM(), e.IdentityKeyring()
	if directory == nil || keyring == nil {
		return nil, errors.New("api: the engine opened no identity estate, and " +
			"engine.New opens it on every node holding the estate, company or not")
	}
	// The assistant estate opens beside the identity estate (engine core),
	// so a node holding one holds the other.
	assistantValues := e.AssistantReader()
	if assistantValues == nil {
		return nil, errors.New("api: the engine opened no assistant estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	executorValues := e.ExecutorReader()
	if executorValues == nil {
		return nil, errors.New("api: the engine opened no executor estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	accessValues := e.AccessKeyring()
	if accessValues == nil {
		return nil, errors.New("api: the engine opened no access estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	secretSurface, err := secretsapi.New(secretsapi.Options{
		Fleet: e.Backends().Fleet, Cipher: cipher,
		ActiveKeyID: boot.Secrets.ActiveKeyID,
		// The identity estate's half of a rotation and the access log's
		// commands, and the assistant's and executor sessions', which a
		// rotation waits out.
		Identity: keyring, Assistant: assistantValues, Executor: executorValues,
		Access: accessValues,
		// The config surface, so writing a value the company reads
		// re-activates its revision (ADR-0070) through `POST /config/reload`'s Reload.
		Config: configSurface,
	})
	if err != nil {
		return nil, err
	}
	// The way in, the directory and the human write surface, with the two
	// credential arms (cookie, machine token). Built by internal/api because
	// the end-to-end suite uses the same constructor ([api.HumanSurfaces]);
	// built first so a node serving none says so in its log, not by a quietly
	// absent route.
	humans, err := api.NewHumanSurfaces(boot, e)
	if err != nil {
		return nil, err
	}
	// What a company with nobody in it does next: the log says once at boot
	// which declared human seats nothing holds yet, or the step that must
	// come first. The directory surface above refuses a node with no
	// identity reader, so one is here.
	announceUnclaimed(ctx, e.AnyPerson, engine.SeatViewOf(e).HumanSeats,
		e.IAM().SeatClaims)

	// Slack install links, signed under the fleet keyring and spent on the
	// fleet's claim registry, so any node finishes a link any node minted.
	// Built before the setup surface that mints them and the webhook landing
	// that finishes them.
	slackInstalls, err := e.SlackInstalls(boot.Secrets.TokenMaterial())
	if err != nil {
		return nil, err
	}

	// Connecting an integration from the dashboard, and each agent's access
	// at each vendor: built by the one constructor the fleet suite calls too.
	setupSurface, err := api.NewSetupSurface(boot, e, configSurface, cipher, slackInstalls)
	if err != nil {
		return nil, err
	}

	// What only the engine can answer (in flight, seats, the posture and
	// the applied epoch, the tool catalogue), read live on every probe
	// through the reconciler that owns this node's config posture.
	runtime, err := api.NewEngineRuntime(e, reconciler)
	if err != nil {
		return nil, err
	}
	// The engine's own wiring, every registered log included, so a backup
	// names each and the trim and backup alarm can count it.
	backups, err := backup.New(e.BackupOptions())
	if err != nil {
		return nil, err
	}
	// The artifacts' routes and read questions, deciding through one shared
	// admission over the estate router, so a node whose copy is out of
	// service answers through a peer's.
	artifactsSurface, artifactsSource, err := artifactsSurfaces(e, boot)
	if err != nil {
		return nil, err
	}
	// An artifact's datastore, likewise: write routes and read questions
	// over the router's datastore and artifacts.
	artifactDataSurface, artifactDataSource, artifactRooms, err := artifactDataSurfaces(e, boot)
	if err != nil {
		return nil, err
	}
	// The contextcheck exemption is for the two push ticks this constructor
	// registers (roster re-send, health frame): each makes a bounded context
	// of its own, correct per [api.tickReadBudget] and
	// [api.App.streamHealth], since a tick is a timer, not a request. Nothing
	// else reached from here creates a context.
	opts := api.Options{
		Bootstrap: boot,
		// DICTATION'S RECOGNIZER, which loads nothing until the first
		// dictation (internal/speech); nil where dictation is switched off.
		Dictation: dictationRecognizer(boot),
		// The company's half of a Tier A principal: the directory's binding
		// for a token's login, through the same seat view a signed-in
		// person's seat uses, so a token bound to a removed seat is refused
		// as a cookie is. A node with no company yet answers 503, never
		// seatless.
		SeatBindings: auth.SeatBindings{
			Directory: e, Chart: engine.SeatViewOf(e),
		},
		// Nil on every ordinary run: `-dev-principal` builds one, refused at
		// boot unless a development build is bound to loopback.
		DevPrincipal: dev,
		Runtime:      runtime,
		// Whose inbox moved, from the engine's Worklet applier.
		Inbox: e,
		// The node's objective meter, which every committed change the
		// socket pushes is timed into.
		Objectives: e.Objectives(),
		// Whose credentials moved, from the identity applier: closes the open
		// tab of a session that ended.
		Credentials: e,
		// The process's chat hub, attached to the engine and mounted here.
		ChatLive: chatLive,
		// The engine's own receiver, not a second one: the API verifies tokens
		// this process's engine minted.
		OtelReceiver: e.OtelReceiver(),
		// The engine's own bridge: a run's session lives in the object that
		// opened it, so another would 401 a healthy run's box.
		Bridge: e.Bridge(),
		// The engine's trail, which the guard counts a refused bearer through.
		AuthEvents:   e.AuthEvents(),
		QueueBackend: e.Backends().Queue.Backend(),
		// A question with no source comes back unknown, not empty: "no event
		// log on this node" differs from "the company has done nothing".
		Sources: queries.Sources{
			// The fleet's turn-level history: every live data node's store,
			// asked at query time, non-answerers named (ADR-0021). The
			// engine's, whose lease view is the roster and whose registered
			// answerer is the protocol's other half.
			Events: e.History(),
			// Every data node's company days for each spend window and series:
			// the replicated usage domain (ADR-0020) through the estate router,
			// so an out-of-service copy answers from a peer's (ADR-0079).
			Usage: e.UsageReads(),
			// Read through the engine's epoch, not a captured company: an
			// apply replaces it.
			Company: func() (*config.Company, *org.Organization) { return companyConfig(e) },
			// The web pages Ask Crewlet reads, through the address check
			// every outbound call takes (ADR-0151).
			Research: research.New(httpx.Guarded(research.Timeout), time.Now),
			Searcher: e,
			// Which of the company's services this node serves only once
			// restarted, so the org projection does not show a workspace whose
			// questions this node refuses.
			AwaitingRestart: e.AwaitingRestart,
			// The lead relations the personal questions are scoped by (inbox,
			// day, person record), through the same seam every surface asks.
			// Left out, those decisions stayed unknown and a lead's report
			// inbox got 503 on every retry.
			Chart: engine.ChartAuthorityOf(e),
			// Whose record somebody else's login names, through the directory
			// and seat view a request resolves through, so a lead reading by
			// login reads the record the report's own screen does.
			Holders: e,
			// Contact identities this node's registry withholds (suspended or
			// departed people), so rosters and lookups leave out whom an
			// inbound message would not be attributed to.
			WithheldContacts: e.WithheldContacts,
			// What a `${VAR}` in a seat's contact ids resolves to: this node's
			// chain, which its routing resolves the same fields through. Who a
			// caller is is the identity directory's answer, never the chart's.
			Env:   e.LookupSecret,
			Coord: e.Backends().Coord,
			// Each node's MCP, model and runner rows, on its status record
			// beside its presence, counted against Coord's.
			NodeStatuses: e.Backends().Fleet,
			Plane:        e.Backends().Fleet,
			// Each person's Home board, the fleet's record, so it follows
			// them to whichever node serves their browser.
			HomeBoards: e.Backends().Fleet,
			// And their own Ask Crewlet instructions, beside it.
			Instructions: e.Backends().Fleet,
			// The object collector's last report, from the store every node
			// reads, for the fleet card: a report on the object store this node
			// agreed with the fleet, through the reader the collector's duty
			// resumes from, so one left by a move of the files is none here.
			Objects: e.Backends(),
			// A seat's memory, answered by the node holding the seat: the
			// engine's reader, since the registered answerer on every node is
			// the protocol's other half and a node without an API may hold it.
			Memory: memoryReads(e),
			// The fleet's agent-to-agent authorization record, not this node's:
			// a channel is opened by the node owning the requester's seat, so
			// a per-node read would differ by answerer.
			Channels: e.Backends().Fleet,
			// The fleet's record of the operating insights it shares (ADR-0119).
			Insights: e.Backends().Fleet,
			// What the Advisor's model pass runs on (ADR-0164).
			AdvisorModel: engine.AdvisorModels(e),
			// The company's one knowledge backend, behind the seam a seat's
			// own turn searches through. Nil when none is configured, leaving
			// the question unregistered rather than answering an empty search
			// as though it ran. Resolved per call: an apply rebuilds the
			// searcher, so a value read once would use a rotated credential's
			// predecessor, a retired wiki, or "no backend" forever.
			Knowledge: e.Knowledge,
			Config:    configSurface,
			Budget:    e.Backends().Fleet,
			Holds:     e.Backends().Fleet,
			// Where a third-party app reaches this deployment: the public
			// listener's `api.public.external_url` where configured, else
			// `api.external_url`, which a surface's recorded endpoint is
			// compared with. The same accessor the reconcile loop and setup
			// surface stamp from, never `api.external_url` alone: behind a
			// public listener every registration would read as moved. A
			// function because it is built once and reads Tier A, which cannot
			// change under a running node.
			WebhookBase:   func() string { return boot.API.WebhookBase() },
			DashboardBase: func() string { return boot.API.ExternalBase() },
			// What the reconcile loop last found per surface: the fleet's
			// record, since the loop is a worker duty and on a split-role
			// deployment the answering node never wrote it.
			Reconciles: func(ctx context.Context) []integration.State {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				states, err := e.IntegrationStates(ctx)
				if err != nil {
					// Nil renders "cannot say", not "nothing reconciled": an
					// unreachable coordination store is no evidence about
					// anybody's integrations, and the two send an operator
					// opposite ways.
					log.Warn("integration_status_unreadable", "error", err)
					return nil
				}
				return states
			},
			// Each surface's status alone, for the Advisor duty's signature,
			// which names the findings by the digest a status carries.
			ReconcileStatuses: func(ctx context.Context) []integration.State {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				states, err := e.IntegrationStatuses(ctx)
				if err != nil {
					log.Warn("integration_status_unreadable", "error", err)
					return nil
				}
				return states
			},
			// Which surfaces a pass converges, so the roll-up can tell one
			// awaiting its first report from one that never gets any.
			Converges: e.Converges,
			// The apps each App Store Connect team was last read to reach,
			// held on this node by the pass and the access routes' reads.
			AppStoreConnectReach: e.AppStoreConnectReach,
			// The durable record of every detached executor, whole: the board
			// needs the row's own facts (branch, pause TTL, bridge call log)
			// the live panel lacks. The fleet's record, since a run is
			// recovered by whichever node owns its seat and a per-node read
			// drew a dashboard disagreeing with itself.
			Sandbox: sandbox.NewCoordStore(e.Backends().Fleet),
			// Each asking agent's pending access review (ADR-0111).
			Access: e,
			// Who gave a run's recorded answer, which only the answer's seal
			// says: opened under this node's fleet keyring.
			RunAnswers: e.AnswerSeal(),
			// What a held call would send, sealed on its run's row and opened
			// under the fleet keyring for who may approve it.
			HeldCalls: sandbox.NewCallSeal(e.Cipher()),
			// A running run's live output, answered by the node owning the
			// run: the engine's reader, since a node serving no API may own it.
			ExecutorTail: executorTails(e),
			// A seat's executor sessions: rows through the estate router (a
			// node without `data` answers as one with it), sealed bytes
			// through the fleet's object store under this node's keyring.
			ExecutorSessions: executorSessionReads(e),
			SessionObjects:   sessionObjects(e, log),
			// Whether a launch continues each one (a relaunch of its turn or
			// its conversation's next), decided as the launch decides: off the
			// fleet's run records and the member the node holding the seat
			// would pick, asked of that node.
			SessionRuns:     sandbox.NewCoordStore(e.Backends().Fleet),
			RelaunchTargets: e,
			// What this node can say about the log's own history: how far each
			// domain may be trimmed, what stops it, what replacing this node
			// costs. Assembled per call, since half is coordination that
			// changes under the answer.
			Retention: api.RetentionSource(ctx, e),
			// What the fleet has backed up: each node's newest announced point
			// and the operator's newest acknowledgement per log, marked by the
			// trim's policy (Tier A, fixed for this process).
			Backups:     e.Backends().Fleet,
			BackupFloor: boot.Stream.TrackerRetention.Floor(),
			NodeID:      nodeID,
			// Artifacts, decided per artifact and shaped by the reader's role;
			// nil on an engine with no router.
			Artifacts: artifactsSource,
			// An artifact's data, decided per artifact and shaped by each
			// collection's read rule; nil on an engine with no router.
			ArtifactData: artifactDataSource,
		},
		// The fleet's record of what the log may delete, for the one
		// retention gesture the engine cannot make alone: an operator's
		// assertion that a copy has left the host.
		Retention: e.Backends().Fleet,
		// The eviction gate, a record on every identity-claiming log rather
		// than a coordination write: judged once, written to each log,
		// answered per log with the usual three-valued outcome.
		Nodes: nodeGate(e),
		// Gitlet and the code reads, asked per request: Gitlet is built with
		// the native runtime, which a company applied after boot brings up
		// later. Git routes mount where this node's roles name gitlet; the
		// reads on every node, through the estate router.
		Code: api.EngineCode(e),
		// The capacity window. The engine refuses the verb when this node is
		// publishing, so the route exists in every mode and answers "wrong
		// mode" rather than 404, which would send an operator hunting a
		// version mismatch.
		Capacity: e,
		// The fleet broker's membership: what nodes advertise against what
		// the metadata group counts, through a member, and removing one gone
		// for good through a member's system account, reachable only from
		// inside a member.
		FleetBroker: e.FleetBroker(),
		// The wake feeds' dead letters, listed and discarded through the
		// engine's one ledger over its queue, so a discard takes the claim
		// every node's expiry count reads, kept in the broker's dead-letter
		// stream (no socket on the default topology).
		DeadLetters: feedDeadLetters(e),
		// Both estates a node holds, reachable only from inside it: the store
		// is locked to this process and the broker binds no socket
		// (internal/backup).
		Backup: backups,
		// The move of the company's files to S3, which only the engine can
		// make: it reads the objects and the estate naming them, and ends
		// with the fleet's coordination record.
		Objects: e,
		// The runtime audit goes onto this node's own queue, whose publish
		// listener writes the event store here, as the operator surface's does.
		Audit:   e.Backends().Queue,
		Config:  configSurface,
		Secrets: secretSurface,
		Setup:   setupSurface,
		// Whether this node can answer from the estate at all, for /ready:
		// the engine's own verdict on keeping its seats, so the two agree.
		// [engine.Engine.SeatsServiceable] rather than
		// [engine.Engine.StateLogHydrated]: hydration is a copy behind, which
		// catches up, and refusing traffic for a legitimate boot pulls a node
		// out of rotation; a wrong copy goes out of service and requests
		// route to a sound data node. A method value, not a snapshot, taking
		// the request's context so a probe on a slow broker ends at its
		// deadline.
		Estate: e.SeatsServiceable,
		// Whether the company has its first person: the one thing an
		// unclaimed company's dashboard can read, since /health is the probe
		// every visitor reaches. Read off this node's identity rows (applied
		// from boot on every estate node), and a "nobody" is proved against
		// the log's end, so a catching-up node answers `unknown`.
		Identity: e.AnyPerson,
		// The inbound edge republishes onto this node's queue and dedupes
		// through the fleet's coordination store, so a delivery landing on
		// any node wakes the seat's owner exactly once; a retrying app reaches
		// whichever node the load balancer picks, so a node-local claim
		// would suppress nothing.
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return companySecrets(e) },
			Publisher: e.Backends().Queue,
			Claims:    e.Backends().Fleet,
			AppFlow:   setupSurface.AppFlow(),
			Recheck:   e,
			CloudIDs:  e.AtlassianCloudIDs(),
			// Each agent's Slack install, finished here when Slack returns the
			// browser; the object the setup surface mints links with.
			SlackInstalls: slackInstalls,
		},
	}
	// How a person becomes a principal: the cookie's other end, the tokens
	// /iam/credentials mints, the identity directory, the human write
	// surface and the one operator surface (both transports, and the
	// viewer's `acts`), each absent on a node serving none. One method keeps
	// an absent surface absent instead of mounting routes over a nil
	// service. See [api.HumanSurfaces.Mount].
	humans.Mount(&opts)
	// The Dashboard Assistant, after the hand-over above: the writes a person
	// may propose are the operator surface's verdict. Its service is closed
	// by every return path without a surface, and by the surface's stop.
	assistantSvc, err := mountAssistant(ctx, boot, e, &opts)
	if err != nil {
		return nil, err
	}
	// The artifacts' routes, each stating its authority where mounted. Set
	// only where built, so an absent surface is a nil interface, not a nil
	// service mounted.
	if artifactsSurface != nil {
		opts.Artifacts = artifactsSurface
	}
	// The datastore's writes, undo, schema, per-viewer state and CSV export,
	// each deciding on the artifact's own shares; set only where built.
	if artifactDataSurface != nil {
		opts.ArtifactData = artifactDataSurface
	}
	// A dashboard's datasets, read from the artifacts' boards and the
	// datastore, each answered as its viewer (adr/0043). Set only where both
	// were built.
	if artifactsSurface != nil && artifactDataSource != nil {
		opts.Datasets = &api.DatasetsSource{Boards: artifactsSurface, Data: artifactDataSource.Reads}
	}
	// The socket's artifact rooms, by that same admission, fed by this
	// engine's appliers and presence hub.
	opts.Rooms = artifactRooms
	// Worklet, Doclet and Chatlet through the estate router as a seat's
	// tools reach them, resolved per question: the native halves come up with
	// the node's first company (at an apply, on a node booted with none), so
	// a value read once here would be nothing for that process's life. A
	// node that has not met its company answers `503 no_active_revision`; a
	// half kept elsewhere is a question this node does not have. See
	// [api.NativeSources], which sets every one or none.
	api.NativeSources(e).Into(&opts.Sources)
	// Settings > Models: the model routes, writing through the config
	// surface and the fleet's store under one keyring, audited on this node's
	// queue, and the questions reading every model's state. A failure returns
	// without a surface, so it closes the assistant, as every path after the
	// assistant's mount does.
	modelSurface, err := api.NewModelSurface(e, configSurface, cipher, e.Backends().Queue)
	if err != nil {
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}
	opts.Models = modelSurface
	api.ModelSources(&opts.Sources, e)
	app, err := api.New(opts) //nolint:contextcheck // see the paragraph above the options
	if err == nil {
		err = assistantSvc.bindRoutes(app)
	}
	if err != nil {
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}
	// Nothing pushes "configured": [api.App.Configured] reads the engine's
	// live epoch through Sources.Company, so an apply bringing the first
	// revision flips it. It is still not the posture: a failed apply leaves
	// the previous epoch serving, a configured node, and /ready reads the
	// posture too; collapsing them would pull a correctly-serving node out of
	// rotation for being behind.
	app.Start(ctx)
	// Every tracker read here takes its items' flow from the projection this
	// app folds (ADR-0162); a node serving no API folds none, answering
	// `flow_live: false`.
	e.UsePostures(app.Postures())

	// The other half of observability: the engine persists what this node
	// publishes (engine.Backends); this puts the whole company's events on
	// this dashboard, by broadcast subscription since a browser here must see
	// turns that ran anywhere (observe.Projector). Started before the
	// handover so no socket opens onto an unfed projection.
	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err = projector.Start(ctx); err != nil {
		app.Stop(context.WithoutCancel(ctx))
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}

	// And the history behind it, or every screen it feeds starts at this
	// process's boot. After the subscription, so an event between read and
	// subscribe cannot fall in the gap (the projection counts a double once);
	// before the handover, so the first socket sees the seeded snapshot
	// ([stream.Service.Seed]). Fleet history comes from every live data node's
	// store (one node shows a restarted one only its share); paused seats and
	// executors in their boxes from the durable record, since no event this
	// process hears mentions them; then every interval. Side by side, each
	// read until it lands ([observe.Seeding]): in sequence a slow history seed
	// starved the others.
	runs := observe.NewSandboxReconciler(sandbox.NewCoordStore(e.Backends().Fleet), app.Stream())
	seeding := observe.StartSeeding(ctx,
		observe.HistorySeed(e.History(), func() *org.Organization {
			_, composed := companyConfig(e)
			return composed
		}, app.Stream()),
		observe.BootSeed{
			Name:      "the paused seats",
			NotSeeded: "seat_pauses_not_seeded", Seeded: "seat_pauses_seeded",
			Hint: "a seat paused before this process started shows as paused once " +
				"the seed lands, or from its next pause or resume; the pause itself is in force",
			Attempt: func(ctx context.Context) error { return seedPauses(ctx, e, app.Stream()) },
		},
		observe.BootSeed{
			Name:      "the executors in flight",
			NotSeeded: "sandbox_panel_not_seeded", Seeded: "sandbox_panel_seeded",
			Hint: "the running-runs panel starts from what this process hears until " +
				"the durable run record is read",
			Attempt: runs.Reconcile,
		})
	seeding.Wait(ctx, projectionSeedBudget)
	runs.Start(ctx)

	surface := &httpSurface{
		app: app, projector: projector, runs: runs, seeding: seeding, auth: humans.SignIn,
		drainGit: app.DrainGit, assistant: assistantSvc, bridge: e.Bridge(),
	}
	// The public listener was bound by the rule the app serves one by
	// ([api.PublicListener]); a disagreement would serve webhooks on no socket
	// or leave one answering `starting` for ever, so it is refused.
	if (app.Public() != nil) != (listeners.public != nil) {
		// Through the one stop, with no listener on it: the projector and the
		// sign-in surface's work are already running and nothing else holds
		// them. The listeners are the caller's to close.
		surface.stop(context.WithoutCancel(ctx), log)
		return nil, fmt.Errorf("api: this node serves a public listener: %t, and bound one: %t",
			app.Public() != nil, listeners.public != nil)
	}
	// The artifact origin is built before the handover, by the rule its
	// listener was bound by ([api.ArtifactsListener]): one this node cannot
	// build fails the boot with every socket still the caller's, rather than
	// serving a dashboard whose frames meet a listener answering `starting`.
	origin, err := artifactOrigin(boot, e, app, nodeID, listeners)
	if err != nil {
		surface.stop(context.WithoutCancel(ctx), log)
		return nil, err
	}
	// The handover, api.port first, then the public listener: the probes are
	// on api.port, so an orchestrator is answered before a vendor.
	listeners.api.serve(app)
	surface.server = listeners.api
	// An assistant's listening stream is one request held open on
	// /operator/mcp; unended, shutdown waited out apiShutdownGrace and cut it,
	// logging api_shutdown_failed on every such stop and burying the real
	// stuck-request warning. Only api.port: operator routes are never public.
	listeners.api.server.RegisterOnShutdown(app.EndListens)
	addr, publicAddr := listeners.api.socket.Addr().String(), ""
	if listeners.public != nil {
		listeners.public.serve(app.Public())
		surface.public = listeners.public
		publicAddr = listeners.public.socket.Addr().String()
	}
	// The artifact origin, the third listener, last: it serves no probe and
	// no route an outside party calls.
	if origin != nil {
		listeners.artifacts.serve(origin.handler)
		surface.artifacts, surface.stopArtifacts = listeners.artifacts, origin.stop
		log.InfoContext(ctx, "api_artifacts_listening",
			"addr", listeners.artifacts.socket.Addr().String(),
			"external_url", boot.API.Artifacts.ExternalBase(),
			// What isolates one artifact from another: a real origin of its
			// own, or an opaque one (same-site templates; `crewlet validate`
			// says why).
			"mode", string(boot.ArtifactOriginMode()))
	}

	// Kept at the handover though the sockets were bound earlier (api_bound):
	// it is the moment this node serves, which an operator timing a boot reads.
	log.InfoContext(ctx, "api_listening", "addr", addr,
		"external_url", boot.API.ExternalBase(),
		"optional_second_factor_allowed", boot.API.Auth.Limits.OptionalSecondFactor(),
		// Empty is api.port itself, every deployment without api.public.
		"public_addr", publicAddr,
		"tokens", app.Guard().Tokens(),
		// Zero is same-origin only, all the served dashboard needs.
		"cross_origin_sites", app.CORS().Origins())
	// The company-derived surfaces (roster, org tree, tool catalogue) are
	// re-sent whenever a published company changes them: no event corrects
	// them and an overlay merge cannot express a deletion. Whole payloads,
	// since the hub silently drops the oldest queued envelope and a dropped
	// delta would be unrecoverable. A setter, as the app exists only now
	// (Engine.SetOnCompanyPublished). Listening assistants on /operator/mcp
	// are told their tools changed at the publish, not at the next request
	// ([api.App.OperatorToolsMoved]).
	e.SetOnCompanyPublished(func(ctx context.Context) {
		app.Stream().CompanyPublished()
		app.OperatorToolsMoved(ctx)
	})
	// The tool catalogue alone when it moves between publishes (a shared MCP
	// server back with other tools, or up on a retry): Engine.SetOnCatalogueChanged.
	// The operator's catalogue carries the same tools, so its listeners are
	// told too.
	e.SetOnCatalogueChanged(func(ctx context.Context) {
		app.Stream().ToolsChanged()
		app.OperatorToolsMoved(ctx)
	})

	return surface, nil
}

// builtOrigin is the artifact origin [artifactOrigin] built: the handler the
// api.artifacts listener serves and the function that stops it hearing this
// node's artifacts movements.
type builtOrigin struct {
	handler http.Handler
	stop    func()
}

// artifactOrigin builds the artifact origin for the api.artifacts listener
// [bindListeners] bound, or nil on a node that binds none: the handler
// internal/artifactserve builds over this engine, its ticket principals
// re-composed through the API's own guard so a frame's credential is decided
// by the arms a request is. The listener was bound by the same rule
// ([api.ArtifactsListener]); a disagreement is refused, as the public
// listener's is.
func artifactOrigin(boot *config.Bootstrap, e *engine.Engine, app *api.App, nodeID string,
	listeners *nodeListeners,
) (*builtOrigin, error) {
	serves := api.ArtifactsListener(boot, nodeID)
	if serves != (listeners.artifacts != nil) {
		return nil, fmt.Errorf("api: this node serves an artifact origin: %t, and bound "+
			"a listener for one: %t", serves, listeners.artifacts != nil)
	}
	if !serves {
		return nil, nil
	}
	creds, err := auth.NewLineages(app.Guard(), e)
	if err != nil {
		return nil, fmt.Errorf("api: the artifact origin: %w", err)
	}
	handler, stop, err := engine.ArtifactOrigin(e, boot, creds, static.FS())
	if err != nil {
		return nil, fmt.Errorf("api: the artifact origin: %w", err)
	}
	return &builtOrigin{handler: handler, stop: stop}, nil
}

// serveProbes is serveAPI for a node whose roles leave out ingress: api.port
// gets the probes and, where it runs seats, the routes its own direct boxes
// dial, on this host's loopback alone ([api.Probes]). Nothing more reaches
// the network: an off-host box dials the fleet's bridge address and the
// ingress node relays its call here over the broker (internal/api/mcpbridge).
func serveProbes(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, listeners *nodeListeners, log *slog.Logger,
) (*httpSurface, error) {
	opts, err := api.EngineProbeOptions(boot, e, reconciler)
	if err != nil {
		return nil, err
	}
	handler, err := api.Probes(opts)
	if err != nil {
		return nil, err
	}
	if boot.API.Artifacts.Enabled() {
		// The origin is ingress's, like the dashboard it frames
		// ([api.ArtifactsListener]); said at info so a port in a file shared
		// across roles is not read as a failed listener.
		log.InfoContext(ctx, "api_artifacts_not_bound", "addr", boot.API.Artifacts.Addr(),
			"hint", "node.roles does not include ingress, so this node serves no "+
				"artifacts; api.artifacts is bound by the ingress nodes")
	}
	listeners.api.serve(handler)
	// The address this node's direct boxes dial, empty where it hands them
	// none (no seats, or a bind loopback does not reach).
	own := listeners.boxHostURL(boot, opts.NodeID)
	hint := "node.roles does not include ingress, so this node serves only " +
		"/health and /ready; webhooks, the dashboard, the REST API and the " +
		"executors' tool bridge (" + mcpbridge.PathPrefix + "{token}, which relays a " +
		"box's call to this node when it holds the run's seat) are a peer's " +
		"with the ingress role"
	if own != "" {
		hint = "node.roles does not include ingress, so this node serves the " +
			"network /health and /ready alone, and this host's loopback the routes " +
			"its own direct boxes dial (" + mcpbridge.PathPrefix + "{token} and, with " +
			"a telemetry receiver, " + auth.OTLPPrefix + "); webhooks, the dashboard, " +
			"the REST API and the tool bridge every other box dials are a peer's " +
			"with the ingress role"
	}
	log.InfoContext(ctx, "api_probes_listening", "addr", listeners.api.socket.Addr().String(),
		"node", opts.NodeID, "roles", opts.Roles, "box_host_url", own, "hint", hint)
	// The bridge, so [httpSurface.stop] releases a direct box's held notes
	// poll first, as on an ingress node.
	return &httpSurface{server: listeners.api, bridge: opts.Bridge}, nil
}

// bindFunc opens the socket one of the node's listeners serves on. The
// binary's is [bindTCP]; it is a parameter because a test serving a node must
// write the port into its configuration before the node binds, and a port
// found free and released is one the suite's thousands of sockets can take
// meanwhile. Such a test holds the socket from picking the port and hands it
// over here, where every listener opens ([bindListeners]).
type bindFunc func(ctx context.Context, addr string) (net.Listener, error)

// bindTCP binds addr, how a running node opens a listener. Through a
// ListenConfig so a shutdown signal aborts a bind blocked on a DNS lookup
// rather than leaving a listener nobody serves.
func bindTCP(ctx context.Context, addr string) (net.Listener, error) {
	var listenCfg net.ListenConfig
	return listenCfg.Listen(ctx, "tcp", addr)
}

// listenAPI opens addr through bind and serves handler on it in the
// background until [apiListener.serve] hands it another, returning the
// listener [shutdownListener] ends. One path for every listener, so they
// cannot drift on the timeouts bounding an unauthenticated client, bind
// failure reports, or how a stop ends the goroutine and socket.
func listenAPI(ctx context.Context, bind bindFunc, addr string, handler http.Handler,
	log *slog.Logger,
) (*apiListener, string, error) {
	listener, err := bind(ctx, addr)
	if err != nil {
		return nil, "", fmt.Errorf("api: bind %s: %w", addr, err)
	}
	slot := newHandlerSlot(handler)
	unused := newUnusedConns()
	server := &http.Server{
		Handler: slot,
		// An unauthenticated client can reach this surface, and a read that
		// never completes would hold a connection forever.
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ConnState:         unused.track,
	}
	server.RegisterOnShutdown(unused.close)
	// [shutdownListener] waits for this goroutine.
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.ErrorContext(ctx, "api_serve_failed", "error", err)
		}
	}()
	return &apiListener{server: server, socket: listener, handler: slot, served: served, unused: unused},
		listener.Addr().String(), nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers: generous on any real network, short enough that a
// silent connection, the cheapest denial against a listener, costs a slot
// for ten seconds, not for ever.
const apiReadHeaderTimeout = 10 * time.Second

// projectionSeedBudget is how long the handover waits for the boot seeds (the
// fleet reads seeding the projection, the pause record, the first durable
// run read: [observe.Seeding]). They run side by side, so the wait is the
// slowest; healthy, milliseconds. A peer that does not answer sizes it: each
// scatter waits [eventfan.FleetReadBudget] and a seat's turn read is two,
// so five seconds is those plus one. A ceiling on the handover, never on a
// seed: every webhook gets `503 starting` until then, and a late seed lands
// on every open screen.
const projectionSeedBudget = 5 * time.Second

// apiIdleTimeout bounds how long a kept-alive connection may sit between
// requests; without it net/http falls back to the also-unset ReadTimeout and
// a quiet client holds its slot for ever (ReadHeaderTimeout bounds only the
// first header). Sixty seconds is well above a real client's think time (the
// dashboard polls faster; its websocket leaves the pool). A blanket
// ReadTimeout is not the instrument: it caps bodies, limiting `crewlet
// backup` and large config imports, which are bounded per route.
const apiIdleTimeout = 60 * time.Second

// issueReportCatalogue names the engine's issue report catalogue to `crewlet
// schema`, and is the name of its checked-in file.
const issueReportCatalogue = "issue-report-catalogue"

// insightsCatalogue names the operating insights catalogue the same way.
const insightsCatalogue = "insights-catalogue"

// catalogues are the exports `crewlet schema` emits beside the two tiers.
var catalogues = map[string]func() ([]byte, error){
	issueReportCatalogue: issuereport.CatalogueJSON,
	insightsCatalogue:    insights.CatalogueJSON,
}

// emitSchema writes a tier's JSON Schema. The engine does not read it (the
// Go types in `internal/config` are the validator; the schema is a subset),
// it serves people and tools authoring a config without running one (the
// `# yaml-language-server: $schema=` modeline, a CI linter, an assistant). It
// is generated because a hand-written one diverges, and a schema that
// red-underlines a runnable config teaches authors to ignore it; CI compares
// the copies under `schema/`.
func emitSchema(args []string, stdout, stderr io.Writer) error {
	// The tier comes off the front before flags parse: flag parsing stops at
	// the first non-flag, so `crewlet schema company -o path` would leave
	// `-o` in the tail and silently write to stdout.
	name, args := splitSubject(args)
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Exactly one tier, wherever it appeared: the tail holds the trailing
	// form (`schema -o path company`) and any leftover, which must be an error.
	name, given := onePositional(fs, name)
	if given > 1 {
		fmt.Fprintf(stderr, "usage: crewlet schema [%s|%s|%s|%s] [-o path]\n",
			config.TierCompany, config.TierBootstrap, issueReportCatalogue, insightsCatalogue)
		return errors.New("name at most one tier")
	}
	if name == "" {
		// Company is the default: it is what an author edits (Tier A is a few
		// URLs written once), so it is what editor completion is for.
		name = string(config.TierCompany)
	}

	var body []byte
	if export, ok := catalogues[name]; ok {
		// Not a tier: a catalogue a relay vendors, emitted and gated beside
		// the schemas. It ends in its own newline.
		catalogue, err := export()
		if err != nil {
			return err
		}
		body = catalogue
	} else {
		schema, err := config.Schema(config.Tier(name))
		if err != nil {
			return err
		}
		// A trailing newline, since the file is checked in and compared.
		body = schema
		body = append(body, '\n')
	}

	if *out == "" {
		_, err := stdout.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%s, %d bytes)\n", *out, name, len(body))
	return nil
}

// splitSubject peels a leading positional off a subcommand's args, "" when
// the first is a flag, so `schema company -o x` and `schema -o x company` both work.
func splitSubject(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// flagLogLevel is the level the command line alone asks for; the shorthand
// wins, as writing both is asking for debug as loudly as possible.
func flagLogLevel(logLevel string, debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return logging.ParseLevel(logLevel)
}

// warnUnrecognisedLogNames says so when a level or format name did not
// resolve to what it spells. Both fail soft on purpose (a misspelled level
// must never stop a boot or cost a CI step its migration) but not silently.
// It logs rather than errors because the file is where a bad value is refused
// (config.Logging.validate); a flag or variable may not fail its invocation.
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

// levelNames and formatNames render the closed sets from the sets
// themselves; a hand-written list would drift.
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

// overrideLogging folds the run flags that override Tier A's `logging:` block
// into the document, so verbosity, file and advisories read the block this
// run applies (judged on the document alone, `-debug` with `stream.debug:
// true` was told the broker's debug output had nowhere to go); written once,
// as [overrideNode] writes `-roles`. The result must pass [validateFolded].
// The command line wins only where it spoke ([isFlagSet]: a flag carries its
// default, else `-log-level` kills `logging.level: warn`). An explicit
// `-log-file ""` drops the file and its shape, level and caps, which the
// validator refuses without a path. `-debug` only raises; `-log-file` moves
// the path, trimmed as Tier A trims strings. `logging.stderr: false` with no
// file is refused.
func overrideLogging(boot *config.Bootstrap, fs *flag.FlagSet,
	logLevel, logFormat string, debug bool, logFile string,
) error {
	if isFlagSet(fs, "log-level") {
		boot.Logging.Level = flagLevel(logLevel)
	}
	if isFlagSet(fs, "log-format") {
		boot.Logging.Format = logging.ParseFormat(logFormat)
	}
	if isFlagSet(fs, "debug") && debug {
		boot.Logging.Level = logging.LevelDebug
	}
	if !isFlagSet(fs, "log-file") {
		return nil
	}
	if logFile = strings.TrimSpace(logFile); logFile != "" {
		boot.Logging.File.Path = logFile
		return nil
	}
	if !boot.Logging.StderrEnabled() {
		return errors.New(
			"-log-file \"\" removes the only destination this node has: " +
				"logging.stderr is false in the Tier A document, so there " +
				"would be nowhere left to write. Name a file, or set " +
				"logging.stderr back to true")
	}
	boot.Logging.File = config.LogFile{}
	return nil
}

// flagLevel is a `-log-level` value as Tier A spells a level. The flag
// resolves leniently ([logging.ParseLevel]: a typo is info, `warning` is
// warn) because a flag may not fail a run; the document gets the level it
// resolved to, one of [logging.Levels] its validator accepts. slog's four
// names lowered are exactly that set.
func flagLevel(name string) logging.Level {
	return logging.Level(strings.ToLower(logging.ParseLevel(name).String()))
}

// attachLogFile installs the log file beside stderr and returns the teardown
// this process runs on its way out. It fails the command rather than falling
// back to stderr alone: an operator who configured a durable record and
// silently did not get one has nothing pointing at why.
//
// Detached before closed, or a late line would meet a closed descriptor and
// report shutdown as a write failure. The console is silenced only once the
// file is open, or a failed open would leave no destination for the failure.
// report carries notices that cannot go through the logger (os.Stderr; a
// parameter as with [logging.OpenFile]), never the process-wide sink
// ([logging.Configure]).
func attachLogFile(settings config.LogFileSettings, stderr bool, report io.Writer) (func(), error) {
	if settings.Open.Path == "" {
		// Nothing to attach or silence: the validator and [overrideLogging]
		// already refused the documents that would leave nowhere to write.
		return func() {}, nil
	}
	f, err := logging.OpenFile(settings.Open, report)
	if err != nil {
		return nil, err
	}
	sink := settings.Sink
	sink.Writer = f
	logging.SetFile(sink)
	// The first line in the file says which file it is, so a relative path
	// is not guessed at. Ordinary telemetry: through the logger, obeying the level.
	logging.Get("cli").Info("log_file_opened", "path", f.Path(), "stderr", stderr)
	if !stderr {
		// The handover goes straight to stderr, as the sink's own failure
		// report does: a logger cannot announce that it is about to stop
		// writing here, and the line above is filtered on a
		// `logging.level: warn` node, leaving a terminal that printed nothing
		// and no way to discover the log is in a file.
		fmt.Fprintf(report,
			"crewlet: logging to %s from here; stderr carries no further log "+
				"lines (logging.stderr is false)\n", f.Path())
		logging.SetConsole(false)
	}
	return func() {
		// The console comes back first, or the teardown and anything logging
		// during it would have no destination.
		logging.SetConsole(true)
		logging.SetFile(logging.FileSink{})
		if closeErr := f.Close(); closeErr != nil {
			fmt.Fprintf(report, "crewlet: closing log file %s: %v\n",
				f.Path(), closeErr)
		}
	}, nil
}

// overrideNode applies the run flags that override Tier A. Only these three
// fields, whose right value depends on where the process runs (the node's
// job in a fleet, where its HTTP surface binds); everything else belongs in
// the reviewed file, and a flag for each would be a second configuration
// surface with no history or validation.
func overrideNode(boot *config.Bootstrap, fs *flag.FlagSet,
	roles, apiHost string, apiPort int,
) error {
	if isFlagSet(fs, "roles") {
		names := splitRoles(roles)
		if len(names) == 0 {
			return errors.New("-roles was given with no role names")
		}
		// The file's parser, which fails closed: a typo like `-roles seat`
		// must be an error, not a node that runs nothing and reports healthy.
		// The flag applies after the file loads, so this alone validates it.
		if _, err := placement.ParseRoles(names); err != nil {
			return fmt.Errorf("-roles: %w", err)
		}
		boot.Node.Roles = names
		// And what the roles require of the rest of the file: holding data
		// decides whether the store is deleted at boot and the broker joins
		// as a leaf, so removing `data` from a node with a durable store is
		// refused here, naming the file's setting.
		if err := boot.ValidateRoles(); err != nil {
			return fmt.Errorf("-roles %s: %w", strings.Join(names, ","), err)
		}
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
	// And what the listeners require, for -roles' reason: the file was
	// validated with its own api.port and roles, and a flag moving api.port
	// onto the public port, or to 0 beneath one on an ingress node, would bind
	// what validation never saw. After every flag, since the rule reads both:
	// `-api-port 0` is a bind-nothing node under `-roles data,seats,workers`
	// and a probe-less one under `-roles ingress`.
	if given := givenFlags(fs, "roles", "api-host", "api-port"); len(given) > 0 {
		if err := boot.ValidateListeners(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(given, " "), err)
		}
	}
	return nil
}

// validateFolded holds the document the run flags were folded into to every
// Tier A rule, refusing it naming the flags given. A flag reaches every rule
// reading what it moved: `-api-port 8080` on a document with no `api:` block
// turns the API on, which needs `api.external_url`. engine.New refuses that
// too, but after the advisories were logged as though the document were the
// one running, and without naming the flag. Here the refusal names the flags
// and precedes [bindListeners] and [decideSeed]. It normalizes in place as
// [config.Bootstrap.Validate] does, and only when a flag writing into the
// document was given; otherwise it is the validated file.
func validateFolded(boot *config.Bootstrap, fs *flag.FlagSet) error {
	given := givenFlags(fs, "roles", "api-host", "api-port",
		"log-level", "log-format", "debug", "log-file")
	if len(given) == 0 {
		return nil
	}
	if err := boot.Validate(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(given, " "), err)
	}
	return nil
}

// logBootstrapAdvisories logs every advisory Tier A raises
// ([config.Bootstrap.Warnings]), one `config_advisory` WARN line each in the
// document's order. Most deployments (image, Helm chart, operator) never run
// validate, and the node's log is the one place all have; Tier A changes only
// on restart, so once per start is complete. Fields are those a person acts
// on: `config` names the tier (as `config_reference_unresolved` does), `path`
// is where the remedy is written, `detail` the whole message. No `kind`.
func logBootstrapAdvisories(log *slog.Logger, boot *config.Bootstrap) {
	for _, w := range engine.BootstrapWarnings(boot) {
		log.Warn("config_advisory", "config", "bootstrap", "path", w.Path, "detail", w.Message)
	}
}

// givenFlags names, as `-name`, the flags among names the command line set.
func givenFlags(fs *flag.FlagSet, names ...string) []string {
	var given []string
	for _, name := range names {
		if isFlagSet(fs, name) {
			given = append(given, "-"+name)
		}
	}
	return given
}

// splitRoles reads a comma-separated role list, ignoring blank entries so a
// trailing comma is not a role named "".
func splitRoles(value string) []string {
	var out []string
	for name := range strings.SplitSeq(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// operatorLogLevel is the level every non-`run` command logs at: warn unless
// $CREWLET_LOG_LEVEL names another. A typo resolves to warn, never failing
// or drifting to info (`run` applies the same never-fail rule with info as
// fallback). Checked against the closed set explicitly because
// logging.ParseLevel cannot report "not recognised": it answers info for
// unknown, right for `run` and wrong here.
func operatorLogLevel() slog.Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CREWLET_LOG_LEVEL")))
	switch raw {
	case "debug", "info", "warn", "warning", "error":
		return logging.ParseLevel(raw)
	default:
		return slog.LevelWarn
	}
}

// operatorLogFormat is the shape every non-`run` command logs in: console
// unless $CREWLET_LOG_FORMAT names another, the sibling of $CREWLET_LOG_LEVEL
// (these commands take no logging flags, and a CI step shipping `crewlet
// migrate` output to a collector needs `json`). An unknown name resolves to
// console, logging.ParseFormat's never-fail rule.
func operatorLogFormat() logging.Format {
	return logging.ParseFormat(os.Getenv("CREWLET_LOG_FORMAT"))
}

// engineConfigWriter is the engine's own way back onto the company: the same
// PATCH /config surface every other write uses — one merge, one validation,
// one compare-and-set onto the document.
type engineConfigWriter struct{ surface *configapi.Service }

func (w engineConfigWriter) Apply(ctx context.Context, patch []byte, summary string,
	by iam.Actor) error {

	_, err := w.surface.Apply(ctx, configapi.ApplyRequest{
		Patch: patch, Summary: summary, By: by, Engine: true,
	})
	return err
}

// Disconnect is the one write of a disconnect: the block gone and the agents
// on through it turned off, as one revision ([configapi.Service.Disconnect]).
func (w engineConfigWriter) Disconnect(ctx context.Context, kind integration.Kind, summary string,
	by iam.Actor) ([]string, error) {

	done, err := w.surface.Disconnect(ctx, configapi.DisconnectRequest{
		Kind: string(kind), Summary: summary, By: by, Engine: true,
	})
	return done.SeatsOff, err
}

// Reload re-activates the current document, so a pass that sealed a
// credential gets everything built at apply time rebuilt against it. On
// whatever the fleet runs, expecting no revision: every node must rebuild
// against the store as the pass left it, including for a revision written
// since the pass read the company, unlike a /setup rotation whose answer says
// the sealed value reached the seats.
func (w engineConfigWriter) Reload(ctx context.Context, summary string, by iam.Actor) error {
	_, err := w.surface.Reload(ctx, summary, by, "")
	return err
}

// Seat and SetSeat are the per-seat write, through the entity route: a seat
// is addressed by its handle, because a merge patch cannot reach one element
// of a list without replacing the list.
func (w engineConfigWriter) Seat(ctx context.Context, handle string) ([]byte, error) {
	entity, err := w.surface.Entity(ctx, "roles", handle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(entity)
}

func (w engineConfigWriter) SetSeat(
	ctx context.Context, handle string, body []byte, summary string, by iam.Actor,
) error {
	_, err := w.surface.ApplyEntity(ctx, configapi.ApplyEntityRequest{
		Kind: "roles", ID: handle, Body: body, Summary: summary, By: by, Engine: true,
	})
	return err
}

// nodeGate is the eviction gate over every identity-claiming log (the
// engine's core, run from boot by every estate node), or an untyped nil where
// the engine holds none, which [api.New] refuses by name; a typed nil would
// pass its check and panic on the first press. Not the tracker's writer: that
// lifted one log's pin, leaving the others counting the node for ever, and an
// external-tracker company could evict nobody.
func nodeGate(e *engine.Engine) api.NodeGate {
	if g := e.NodeGate(); g != nil {
		return g
	}
	return nil
}

// feedDeadLetters is the wake feeds' dead letters through the engine's one
// ledger over the node's queue, or an untyped nil where it keeps none, which
// [api.New] refuses by name; a typed nil would panic on the first read.
func feedDeadLetters(e *engine.Engine) api.FeedDeadLetters {
	if d := e.FeedDeadLetters(); d != nil {
		return d
	}
	return nil
}

// memoryReads is the engine's memory reader as the read surface's seam, or
// nil when none: a typed nil would register `agent_memory` over a reader
// that cannot answer.
func memoryReads(e *engine.Engine) queries.SeatMemory {
	if r := e.MemoryReads(); r != nil {
		return r
	}
	return nil
}

// executorTails is this node's run-output reader, converted for
// [memoryReads]'s reason: a nil *TailReader would register a question whose
// every answer panics.
func executorTails(e *engine.Engine) queries.ExecutorTails {
	if r := e.ExecutorTails(); r != nil {
		return r
	}
	return nil
}

// executorSessionReads is the executor log's reads through this node's estate
// router, or nil on an engine with no router, leaving the sessions questions
// unregistered rather than registered over a reader that cannot answer.
func executorSessionReads(e *engine.Engine) queries.ExecutorSessions {
	if reads, ok := engine.ExecutorSessionReads(e); ok {
		return reads
	}
	return nil
}

// sessionObjects is the store a session's sealed object is opened through, or
// nil (for [memoryReads]'s reason) on a node with no object store or whose
// keyring cannot open a sealed stream, which is logged: sessions still list
// there, with no transcript.
func sessionObjects(e *engine.Engine, log *slog.Logger) queries.SessionObjects {
	store, err := e.ExecutorSessions()
	if err != nil {
		log.Warn("executor_session_transcripts_off", "error", err.Error(),
			"detail", "this node's keyring cannot open a session's sealed stream, so it "+
				"answers no session's transcript; the keyring's own error names what to fix")
		return nil
	}
	if store == nil {
		return nil
	}
	return store
}

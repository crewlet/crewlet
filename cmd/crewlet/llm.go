package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/secrets"
)

// `crewlet llm` — the operator's window onto the subscription CLI backends.
//
// # Why these commands exist at all
//
// Every other provider type is configured and done: an API key is a string in
// the secret store. A `cli-agent` provider is a LOGIN — browser OAuth with
// PKCE, often behind SSO and MFA — held by a vendor's own command-line tool
// in a directory on disk. There is no password grant to script, and driving a
// headless browser through a vendor's login page breaks on their next
// redesign. So Crewlet does not re-implement any of it. What it controls is
// WHERE the credential lands: inside the provider's own isolated directory,
// separate from the operator's personal login on the same machine.
//
// # Why doctor is the command that matters
//
// A cli-agent provider can be configured perfectly and still not work: the
// binary can be missing from the engine host, the profile's flags can have
// drifted from the installed version, the login can be present but expired,
// and — the one nothing else catches — the model can answer prose instead of
// the tool-call envelope, which costs every phase corrective rounds and, when
// the model never manages the envelope, ends it without its submission.
// Only a real completion with a real tool proves the last one, so `doctor`
// runs one.
//
// Two of the profile's claims are measured the same way, because a flag that
// stopped working is silent in both directions. The shell probe asks the CLI
// to run a command on the engine host and reports whether it did — a profile
// that denies local tools and a CLI that ran one is a hole where the isolation
// was assumed. The web probe asks it to fetch a URL, because web is the one
// local tool every profile keeps ON and a vendor's sandbox flag can cut it
// without saying so.
//
// # Why doctor also reads anthropic entries
//
// An `anthropic` entry needs no login, but it can be configured perfectly and
// still be refused on every call: its request is SHAPED from a capability
// table compiled into this build (claudemodel), and a model the vendor has
// since changed — or a gateway alias the table reads as the wrong generation —
// answers that shape with a 400, which the fallback chain does not retry. So
// doctor reads the model's record from the vendor's Models API and reports
// where the table disagrees with it, and sends one round in the shape a phase
// sends to certify a tool call comes back. The other subcommands stay
// cli-agent only: there is no login to broker, list or export.

const llmUsage = `crewlet llm — subscription CLI backends: logins, health and tokens;
and the health of the anthropic entries beside them

Usage:
  crewlet llm list                        Providers, agent, model and how each is signed in
  crewlet llm doctor [KEY]                Verify cli-agent and anthropic entries end to end
                                          (-no-smoke skips the real calls)
  crewlet llm login KEY                   Broker the vendor's own interactive login
  crewlet llm login KEY -from-host        Adopt a login already on this machine
  crewlet llm login KEY -capture-token    Mint a headless token into the secret store
  crewlet llm login KEY -capture-token -print-token
                                          ... or to stdout, storing nothing
  crewlet llm login KEY -token-stdin      Store a token you already have
  crewlet llm login KEY -username U -password-stdin
                                          Where the CLI genuinely has a credential login
  crewlet llm status KEY                  Ask the CLI who it is logged in as
  crewlet llm logout KEY                  Revoke locally and delete the credentials
  crewlet llm export KEY [-secret-store]  Pack the login into a blob, or into the secret store
  crewlet llm import KEY                  Restore a bundle from stdin onto this host

Flags:
  -company PATH  Tier B, the company document naming the providers (default %q)
  -config PATH   Tier A, carrying the store and the secret keyring (default %q)
  -api URL       Store a token or bundle through this running node, which puts it
                 on every node at once, rather than into the rows of the store
                 -config names (login and export -secret-store)
  -home PATH     Read a host login from somewhere other than this user's home
  -no-smoke      Skip doctor's real completions (the tool call and both probes; an
                 anthropic entry's Models API read still runs, it bills nothing)
  -print-token   Write a captured token to stdout instead of the store (login only)
`

// runLLM dispatches `crewlet llm`.
//
// STDIN IS AN ARGUMENT for every subcommand that reads it — a token or a
// password `login` is handed, the prompts of a vendor's own login, and the
// bundle `import` restores — as it is for `crewlet iam`, so a suite can pipe
// one in and drive the write each of them ends in.
func runLLM(environ config.Source, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, llmUsage, defaultCompanyPath, defaultBootstrapPath)
		return flag.ErrHelp
	}
	// The provider key comes off before the flags, for the same reason the
	// subcommand did: `llm doctor default -no-smoke` is the natural
	// spelling, and Go's flag package stops at the first non-flag argument.
	key, rest := splitSubject(rest)

	fs := flag.NewFlagSet("llm "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	companyPath := fs.String("company", defaultCompanyPath,
		"Tier B config: the company document naming the providers")
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store and its secret keyring")
	fromHost := fs.Bool("from-host", false,
		"adopt the login this machine already has (login only)")
	captureToken := fs.Bool("capture-token", false,
		"run the vendor's token-minting command (login only)")
	tokenStdin := fs.Bool("token-stdin", false,
		"read a headless token from stdin (login only)")
	username := fs.String("username", "",
		"username for a CLI with a real credential login (login only)")
	passwordStdin := fs.Bool("password-stdin", false,
		"read the password from stdin (login only)")
	home := fs.String("home", "",
		"read a host login from this home directory instead of the engine user's")
	secretStore := fs.Bool("secret-store", false,
		"write the bundle into the encrypted secret store (export only)")
	noSmoke := fs.Bool("no-smoke", false,
		"skip the real completions — the tool call and both isolation probes (doctor only)")
	printToken := fs.Bool("print-token", false,
		"write a captured token to stdout instead of storing it (login only)")
	// THE FLAG `crewlet secrets` AND THE PROVISIONERS TAKE, for their
	// reason: the store is the fleet's, and naming a running node is how a
	// value written where no engine is running reaches the fleet now rather
	// than at this node's next start.
	apiURL := nodeAPIFlag(fs,
		"store a token or bundle through this running node rather than into "+
			"the store -config names (login and export only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	key, given := onePositional(fs, key)
	if given > 1 {
		return fmt.Errorf("llm %s takes one provider key, got %d", sub, given)
	}

	ctx := context.Background()
	company, resolver, closeResolver, err := loadLLMConfig(ctx, *companyPath, *bootstrapPath, stderr)
	if err != nil {
		return err
	}
	defer closeResolver()

	if sub == "doctor" {
		entries, buildErr := doctorEntries(company, resolver, *companyPath)
		if buildErr != nil {
			return buildErr
		}
		return doctorLLM(ctx, entries, key, !*noSmoke, stdout)
	}
	providers, err := cliAgents(company, resolver, *companyPath)
	if err != nil {
		return err
	}

	switch sub {
	case "list":
		return listLLMProviders(ctx, providers, stdout, stderr)
	case "login":
		return loginLLM(ctx, environ, loginRequest{
			providers: providers, key: key, home: *home,
			fromHost: *fromHost, captureToken: *captureToken,
			tokenStdin: *tokenStdin, username: *username, passwordStdin: *passwordStdin,
			bootstrapPath: *bootstrapPath, apiURL: *apiURL, printToken: *printToken,
			stdin: stdin,
		}, stdout, stderr)
	case "status":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		restoreConfiguredBundle(ctx, p, stderr)
		return p.Status(ctx, stdout, stderr)
	case "logout":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		logoutErr := p.Logout(ctx, stdout, stderr)
		if logoutErr == nil {
			fmt.Fprintf(stdout, "Logged %s out and removed its credentials.\n", p.Key())
		}
		// WHETHER OR NOT THE LOGOUT AS A WHOLE SUCCEEDED: a vendor logout that
		// failed after the login was removed fails this command with the
		// logout recorded all the same, and that record is what keeps the
		// configured bundle out — which the operator revoking a compromised
		// login most needs to hear. See [cliagent.Provider.LogoutNotice].
		switch notice, err := p.LogoutNotice(); {
		case err != nil:
			fmt.Fprintf(stderr, "crewlet: %s: %v\n", p.Key(), err)
		case notice != "":
			fmt.Fprintln(stdout, notice)
		}
		return logoutErr
	case "export":
		return exportLLM(ctx, environ, providers, key, *secretStore,
			secretDestination{bootstrapPath: *bootstrapPath, apiURL: *apiURL}, stdout)
	case "import":
		return importLLM(ctx, providers, key, stdin, stdout)
	default:
		fmt.Fprintf(stderr, llmUsage, defaultCompanyPath, defaultBootstrapPath)
		return fmt.Errorf("unknown llm command %q", sub)
	}
}

// cliAgentProvider pairs a built provider with the key it was configured
// under, in the order the company document wrote them.
type cliAgentProvider struct {
	key      string
	provider *cliagent.Provider
}

// loadLLMConfig reads the company document and the resolver its references
// go through.
//
// The SAME resolver the provisioning CLIs use — store first, then the
// environment — because a token rotated into the secret store must win over a
// stale `.env` exported into this shell months ago. A command that resolved
// from the environment alone would report a provider as having no token while
// the running engine used one.
func loadLLMConfig(ctx context.Context, companyPath, bootstrapPath string, notes io.Writer) (*config.Company, *config.Resolver, func(), error) {
	company, err := config.LoadCompanyToRun(companyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, _, closeResolver, err := companyResolver(ctx, bootstrapPath, notes)
	if err != nil {
		return nil, nil, nil, err
	}
	return company, resolver, closeResolver, nil
}

// cliAgents builds every cli-agent provider the company declares, in the
// order the document wrote them.
func cliAgents(company *config.Company, resolver *config.Resolver, companyPath string) ([]cliAgentProvider, error) {
	var out []cliAgentProvider
	for _, key := range company.Providers.ProviderOrder() {
		spec := company.Providers.LLM[key]
		if spec.Type != config.LLMCLIAgent {
			continue
		}
		built, err := engine.BuildCLIAgent(key, spec, resolver)
		if err != nil {
			return nil, err
		}
		out = append(out, cliAgentProvider{key: key, provider: built})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"%s declares no cli-agent providers — see "+
				"docs/concepts/subscription-llm-backends.md for the config block",
			companyPath)
	}
	return out, nil
}

// oneProvider picks the provider a key names, or explains the choice.
func oneProvider(providers []cliAgentProvider, key string) (*cliagent.Provider, error) {
	if key == "" {
		if len(providers) == 1 {
			return providers[0].provider, nil
		}
		return nil, fmt.Errorf("name a provider: %s", strings.Join(providerKeys(providers), ", "))
	}
	for _, p := range providers {
		if p.key == key {
			return p.provider, nil
		}
	}
	return nil, fmt.Errorf("no cli-agent provider %q (have %s)",
		key, strings.Join(providerKeys(providers), ", "))
}

// restoreConfiguredBundle makes the restore the engine makes before it runs an
// entry ([cliagent.Provider.RestoreConfiguredBundle]), for a command that
// answers about the login the engine would run with, and says on stderr why
// it restored nothing where it did not: the engine warns and runs the entry
// signed out, and the command then reports a login that is not there, which
// this line explains — a bundle that failed, or a logout that keeps it out
// until a login is written again.
func restoreConfiguredBundle(ctx context.Context, p *cliagent.Provider, stderr io.Writer) {
	err := p.RestoreConfiguredBundle(ctx)
	switch {
	case err == nil:
	case errors.Is(err, cliagent.ErrLoggedOut):
		fmt.Fprintf(stderr, "crewlet: %s: %v\n", p.Key(), err)
	default:
		fmt.Fprintf(stderr, "crewlet: %s: the entry's configured credential bundle did not restore: %v\n",
			p.Key(), err)
	}
}

func providerKeys(providers []cliAgentProvider) []string {
	keys := make([]string, 0, len(providers))
	for _, p := range providers {
		keys = append(keys, p.key)
	}
	slices.Sort(keys)
	return keys
}

// listLLMProviders is `crewlet llm list`: each entry's CLI, model, state
// directory and how it is signed in.
//
// Each entry's configured bundle is RESTORED FIRST, as `doctor` restores it
// before it diagnoses: the SIGN-IN column is the first route doctor names, and
// read before the restore it said `none` for a fresh container that doctor —
// and the engine — would sign in from its bundle. A restore that fails, or a
// logout that keeps the bundle out, is said on stderr, as `status` says it.
func listLLMProviders(ctx context.Context, providers []cliAgentProvider, stdout, stderr io.Writer) error {
	for _, p := range providers {
		restoreConfiguredBundle(ctx, p.provider, stderr)
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tAGENT\tMODEL\tSIGN-IN\tSTATE DIR")
	for _, p := range providers {
		model := p.provider.Model()
		if model == "" {
			model = "(the CLI's default)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			p.key, p.provider.Agent(), model,
			p.provider.SignInState(), p.provider.Workspace().Root())
	}
	return w.Flush()
}

// diagnosis is what doctor prints about one entry, whichever backend it is.
// Declared here, by the one caller that needs both backends' reports to look
// alike.
type diagnosis interface {
	Render(w io.Writer)
	Healthy() bool
}

// doctorEntry is one entry doctor can examine.
type doctorEntry struct {
	key  string
	kind config.LLMProviderType
	// diagnose measures the entry; smoke sends the billed calls.
	diagnose func(ctx context.Context, smoke bool) diagnosis
}

// doctorEntries is every entry doctor examines, in the order the document
// wrote them: the cli-agent entries and the anthropic ones.
//
// AN ENTRY THAT DOES NOT BUILD IS A REPORT, not an abort. The engine would
// refuse to apply this document over it, which is precisely what an operator
// running doctor needs to read — beside every other entry's report, rather
// than instead of it.
//
// The openai entries are not examined (there is no capability table to drift
// from and no login to check), and naming one is refused by name rather than
// answered with a report of nothing.
func doctorEntries(company *config.Company, resolver *config.Resolver, companyPath string) ([]doctorEntry, error) {
	var out []doctorEntry
	for _, key := range company.Providers.ProviderOrder() {
		spec := company.Providers.LLM[key]
		entry := doctorEntry{key: key, kind: spec.Type}
		switch spec.Type {
		case config.LLMCLIAgent:
			built, err := engine.BuildCLIAgent(key, spec, resolver)
			if err != nil {
				entry.diagnose = unbuilt(key, err)
				break
			}
			entry.diagnose = func(ctx context.Context, smoke bool) diagnosis {
				// A logout that keeps the bundle out is no failure of the
				// bundle: the diagnosis names it beside the missing sign-in,
				// as it would for the entry the engine runs.
				if err := built.RestoreConfiguredBundle(ctx); err != nil &&
					!errors.Is(err, cliagent.ErrLoggedOut) {
					return bundleFailure{key: key, err: err}
				}
				return built.Diagnose(ctx, cliagent.DiagnoseOptions{
					Smoke: smoke,
					// THE ENGINE'S OWN ANSWERS, not the provider's guess at
					// them: which runners this build registers and what a
					// sandbox can dial are facts about the process, and both
					// decide whether an agent-mode entry works at all.
					AgentRunners: codingagent.Names(),
					BridgeURL:    os.Getenv(mcpbridge.BaseURLVar),
				})
			}
		case config.LLMAnthropic:
			built, err := engine.BuildAnthropic(key, spec, resolver)
			if err != nil {
				entry.diagnose = unbuilt(key, err)
				break
			}
			entry.diagnose = func(ctx context.Context, smoke bool) diagnosis {
				return built.Diagnose(ctx, anthropic.DiagnoseOptions{Key: key, Smoke: smoke})
			}
		}
		out = append(out, entry)
	}
	if !slices.ContainsFunc(out, doctorEntry.examined) {
		return nil, fmt.Errorf(
			"%s declares no cli-agent or anthropic providers, which are the entries "+
				"doctor examines — see docs/concepts/subscription-llm-backends.md",
			companyPath)
	}
	return out, nil
}

// examined reports whether doctor has a report for this entry's type.
func (e doctorEntry) examined() bool { return e.diagnose != nil }

// buildFailure is the report for an entry that did not build.
type buildFailure struct {
	key string
	err error
}

// bundleFailure is a cli-agent entry whose configured credential bundle did not
// restore: the engine warns and runs it signed out, so there is no login to
// diagnose, and the bundle is what to fix.
type bundleFailure struct {
	key string
	err error
}

func (b bundleFailure) Healthy() bool { return false }

func (b bundleFailure) Render(w io.Writer) {
	fmt.Fprintf(w, "%-14s: %s\n", "provider", b.key)
	fmt.Fprintln(w, "problems:")
	fmt.Fprintf(w, "  - its configured credential bundle did not restore, so the engine would run it "+
		"signed out — export the bundle again (`crewlet llm export`): %v\n", b.err)
}

func unbuilt(key string, err error) func(context.Context, bool) diagnosis {
	return func(context.Context, bool) diagnosis { return buildFailure{key: key, err: err} }
}

func (b buildFailure) Healthy() bool { return false }

func (b buildFailure) Render(w io.Writer) {
	fmt.Fprintf(w, "%-14s: %s\n", "provider", b.key)
	fmt.Fprintln(w, "problems:")
	fmt.Fprintf(w, "  - it does not build, so the engine would refuse this document: %v\n", b.err)
}

func doctorLLM(ctx context.Context, entries []doctorEntry, key string, smoke bool, stdout io.Writer) error {
	selected := slices.DeleteFunc(slices.Clone(entries), func(e doctorEntry) bool { return !e.examined() })
	if key != "" {
		i := slices.IndexFunc(entries, func(e doctorEntry) bool { return e.key == key })
		switch {
		case i < 0:
			keys := make([]string, 0, len(selected))
			for _, e := range selected {
				keys = append(keys, e.key)
			}
			slices.Sort(keys)
			return fmt.Errorf("no provider %q (doctor examines %s)", key, strings.Join(keys, ", "))
		case !entries[i].examined():
			return fmt.Errorf(
				"provider %q is type %q; doctor examines cli-agent and anthropic entries",
				key, entries[i].kind)
		}
		selected = []doctorEntry{entries[i]}
	}
	unhealthy := 0
	for i, e := range selected {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		d := e.diagnose(ctx, smoke)
		d.Render(stdout)
		if !d.Healthy() {
			unhealthy++
		}
	}
	if unhealthy > 0 {
		// A non-zero exit, because `doctor` is what a deploy script runs
		// before it lets a node take seats — a green-looking command that
		// exited 0 with problems printed is one nobody gates on.
		return fmt.Errorf("%d of %d providers have problems", unhealthy, len(selected))
	}
	return nil
}

// loginRequest is what `crewlet llm login` was asked to do.
type loginRequest struct {
	providers     []cliAgentProvider
	key           string
	home          string
	fromHost      bool
	captureToken  bool
	tokenStdin    bool
	username      string
	passwordStdin bool
	bootstrapPath string
	apiURL        string
	printToken    bool
	stdin         io.Reader
}

func loginLLM(ctx context.Context, environ config.Source, req loginRequest, stdout, stderr io.Writer) error {
	p, err := oneProvider(req.providers, req.key)
	if err != nil {
		return err
	}
	key := req.key
	if key == "" {
		key = req.providers[0].key
	}

	// The modes are mutually exclusive: each writes the credential to a
	// different place, and running two would leave an operator unsure
	// which one the engine reads.
	chosen := 0
	for _, on := range []bool{req.fromHost, req.captureToken, req.tokenStdin, req.username != ""} {
		if on {
			chosen++
		}
	}
	if chosen > 1 {
		return errors.New("choose one of -from-host, -capture-token, -token-stdin or -username")
	}

	switch {
	case req.fromHost:
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		taken, err := p.AdoptHostLogin(ctx, req.home)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Adopted %s into %s.\n",
			strings.Join(taken, ", "), p.Workspace().CredentialsDir())
		// Said every time, because it is the one cost of this route and
		// it only bites later: both copies now descend from one refresh
		// token, and a vendor that rotates them logs out whichever side
		// refreshes second.
		fmt.Fprintln(stdout,
			"\nThis is a COPY: your own login is untouched, but both copies now share\n"+
				"one refresh token, and a vendor that rotates them can log out whichever\n"+
				"side refreshes second.")
		if tokenVar, err := cliagent.TokenVarName(p.Profile()); err == nil {
			fmt.Fprintf(stdout,
				"Prefer `crewlet llm login %s -capture-token`, which mints a headless\n"+
					"%s and avoids the shared refresh token entirely.\n", key, tokenVar)
		}
		return nil

	case req.captureToken, req.tokenStdin:
		var token string
		if req.tokenStdin {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			raw, err := io.ReadAll(req.stdin)
			if err != nil {
				return fmt.Errorf("reading the token from stdin: %w", err)
			}
			token = strings.TrimSpace(string(raw))
			if token == "" {
				return errors.New("stdin held no token")
			}
		} else {
			token, err = p.CaptureToken(ctx, req.stdin, stderr)
			if err != nil {
				return err
			}
		}
		tokenVar, err := cliagent.TokenVarName(p.Profile())
		if err != nil {
			return fmt.Errorf("cannot store a token for %q: %w", p.Agent(), err)
		}
		if req.printToken {
			// TO STDOUT, STORING NOTHING. An operator whose secrets live
			// in somebody else's manager should not have to write the
			// token into Crewlet's store on the way past — which is
			// exactly what the two-step alternative (-capture-token then
			// `secrets get -reveal`) does, leaving a copy behind in a
			// revision history that keeps what a later write deletes.
			//
			// REFUSED ON A TERMINAL: this is a credential, and printing
			// one into a scrollback that a screen-share or a shell
			// history will outlive is the accident the flag exists to
			// enable, not to cause. Redirect it or pipe it.
			if isTerminal(stdout) {
				return errors.New(
					"-print-token writes a credential to stdout and refuses to " +
						"do it on a terminal: pipe it into your secret manager, " +
						"or redirect it to a file you then remove")
			}
			fmt.Fprintln(stdout, token)
			fmt.Fprintf(stderr,
				"Wrote the headless token to stdout and stored NOTHING. "+
					"Reference it as ${%s}.\n", tokenVar)
			return nil
		}
		fmt.Fprintf(stdout, "Storing the headless token as %s.\n", tokenVar)
		if err := storeLLMSecret(ctx, environ, secretDestination{
			bootstrapPath: req.bootstrapPath, apiURL: req.apiURL,
		}, tokenVar, token, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout,
			"\nReference it from the company document as:\n\n"+
				"  providers:\n    llm:\n      %s:\n        cli:\n          auth:\n"+
				"            token: \"${%s}\"\n", key, tokenVar)
		return nil

	case req.username != "":
		if !req.passwordStdin {
			return errors.New("-username requires -password-stdin: a password on argv is " +
				"visible in ps and lands in shell history")
		}
		raw, err := io.ReadAll(req.stdin)
		if err != nil {
			return fmt.Errorf("reading the password from stdin: %w", err)
		}
		password := strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			return errors.New("stdin held no password")
		}
		if err := p.CredentialLogin(ctx, req.username, password, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Logged %s in as %s.\n", key, req.username)
		return nil

	default:
		if err := p.BrokerLogin(ctx, req.stdin, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nLogin written to %s.\n", p.Workspace().CredentialsDir())
		fmt.Fprintf(stdout, "Verify it with `crewlet llm doctor %s`.\n", key)
		return nil
	}
}

func exportLLM(ctx context.Context, environ config.Source, providers []cliAgentProvider,
	key string, toStore bool, to secretDestination, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
	}
	if err = p.RestoreConfiguredBundle(ctx); errors.Is(err, cliagent.ErrLoggedOut) {
		return fmt.Errorf("%s has no login to export: %w", key, err)
	} else if err != nil {
		return fmt.Errorf("the entry's configured credential bundle did not restore, so there is "+
			"no login the engine would run with to export: %w", err)
	}
	bundle, err := p.ExportBundle()
	if err != nil {
		return err
	}
	if !toStore {
		// To stdout, so an operator can pipe it into their own secret
		// manager. It is a CREDENTIAL — the caller asked for it in the
		// clear, and the reminder is the honest thing to print beside it.
		fmt.Fprintln(stdout, bundle)
		return nil
	}
	name := cliagent.BundleVarName(key)
	fmt.Fprintf(stdout, "Storing the credential bundle as %s.\n", name)
	if err := storeLLMSecret(ctx, environ, to, name, bundle, stdout); err != nil {
		return err
	}
	// EVERY ENGINE OF THE COMPANY, not "this node's". The secret store is
	// the fleet's — a write through a running node lands there at once, and
	// one into this node's own rows is moved there at its next start — so
	// every node whose credentials directory for this provider is empty,
	// and on which nobody logged the entry out since its last login,
	// restores the same login, and running this once per node, as this
	// used to say, rewrote one shared value with each node's copy in turn.
	// What that sharing costs is the one thing worth saying beside it.
	fmt.Fprintf(stdout,
		"\nAn engine restores it into %s's credentials directory at boot and at\n"+
			"every apply it publishes, when that directory is empty and nobody logged\n"+
			"the entry out there since its last login — on a fleet, every such node.\n"+
			"Each copy then descends from one refresh token, and a vendor that rotates\n"+
			"them can log out whichever side refreshes second; a headless token, where\n"+
			"the CLI mints one (`crewlet llm login %s -capture-token`), avoids that.\n"+
			"Reference it as:\n\n"+
			"  providers:\n    llm:\n      %s:\n        cli:\n          auth:\n"+
			"            credential_bundle: \"${%s}\"\n", key, key, key, name)
	return nil
}

// secretDestination is where `crewlet llm` stores a credential: the store the
// Tier A file at bootstrapPath describes, or the running node apiURL names.
type secretDestination struct {
	bootstrapPath string
	apiURL        string
}

// storeLLMSecret writes one value into the encrypted secret store — as
// [secrets.SourceLLMLogin] where the store keeps the word, a stopped node's
// own table, and as the running node's route records it otherwise — and says
// which store it was, the fleet's through a running node or this node's own
// rows, with what each does next, exactly as `crewlet secrets set` does,
// because which of the two a credential landed in decides whether a fleet
// has it.
func storeLLMSecret(ctx context.Context, environ config.Source, to secretDestination,
	name, value string, stdout io.Writer) error {

	sv, closeStore, err := openSecretStore(ctx, environ, to.bootstrapPath, to.apiURL)
	if err != nil {
		return fmt.Errorf("cannot reach the secret store to save %s: %w", name, err)
	}
	defer closeStore()
	if _, err := sv.Set(ctx, name, value, hostAuthor(), secrets.SourceLLMLogin,
		time.Now().UTC()); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	sv.reportWrite(stdout)
	return nil
}

// importLLM restores an exported credential bundle onto this host.
//
// # The other half of export
//
// `crewlet llm export KEY` writes a blob to stdout and, without this, nothing
// read it back: the documented way to "move a login onto another host" ended
// at a string in a terminal. The `-secret-store` half reaches a second host
// wherever the two are nodes of one company — the store is the fleet's — and
// not where they are two unrelated laptops.
//
// FROM STDIN, never from a path argument: a credential bundle on argv is
// visible in `ps` and lands in shell history, and the natural spelling —
// `crewlet llm export k | ssh host crewlet llm import k` — is a pipe anyway.
//
// It REFUSES TO OVERWRITE an existing login, which is [Provider.RestoreBundle]'s
// own rule: a host that has been running holds the fresher refresh token, and
// restoring a blob over it is how a fleet logs itself out. That is reported
// here rather than swallowed, because "nothing happened" and "restored" look
// identical from the outside and only one of them means the host is ready —
// asked before stdin is read, so a refusal costs nothing, and answered again by
// [cliagent.Provider.ImportBundle] under the lock its write takes, because a
// login landing between the two was otherwise reported as a bundle restored.
func importLLM(ctx context.Context, providers []cliAgentProvider, key string, stdin io.Reader, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
	}
	present := func() error {
		return fmt.Errorf(
			"%s already has a login in %s, and restoring over it would replace "+
				"a fresher refresh token with an older one — log it out first "+
				"(`crewlet llm logout %s`) if you mean to replace it",
			key, p.Workspace().CredentialsDir(), key)
	}
	if p.Workspace().HasLogin() {
		return present()
	}
	// +1 SO THE OVERRUN IS VISIBLE. io.LimitReader stops at the cap and
	// reports a clean EOF, so reading exactly maxBundleBytes cannot be told
	// from a bundle that was clipped there — and a clipped bundle is a
	// truncated tar that RestoreBundle would unpack as far as it goes. The
	// constant's own doc says this "refuses to buffer" an oversized pipe;
	// reading one extra byte is what makes that true.
	raw, err := io.ReadAll(io.LimitReader(stdin, maxBundleBytes+1))
	if err != nil {
		return fmt.Errorf("reading the bundle from stdin: %w", err)
	}
	if len(raw) > maxBundleBytes {
		return fmt.Errorf(
			"the bundle on stdin exceeds %d bytes, which is far larger than any "+
				"real credential directory — check what is on the other side of "+
				"the pipe (`crewlet llm export %s` produces one)",
			maxBundleBytes, key)
	}
	blob := strings.TrimSpace(string(raw))
	if blob == "" {
		return errors.New(
			"stdin held no bundle: pipe one in, e.g. " +
				"`crewlet llm export " + key + " | ssh host crewlet llm import " + key + "`")
	}
	if err := p.ImportBundle(ctx, blob); errors.Is(err, cliagent.ErrLoginPresent) {
		return present()
	} else if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %s into %s.\nVerify it with `crewlet llm doctor %s`.\n",
		key, p.Workspace().CredentialsDir(), key)
	return nil
}

// maxBundleBytes bounds what import will read.
//
// A credential directory is a handful of small JSON files; 8 MiB is orders of
// magnitude above any real one, and a pipe carrying more than that is REFUSED
// rather than read to the cap — a clipped bundle is a truncated tar, and
// unpacking one as far as it goes writes a partial credential directory that
// looks like a finished login.
const maxBundleBytes = 8 << 20

// isTerminal reports whether a writer is a character device.
//
// Used by exactly one caller, to refuse printing a credential into a
// scrollback. A conservative answer: anything this cannot inspect is treated
// as NOT a terminal, because the alternative refuses a legitimate pipe.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

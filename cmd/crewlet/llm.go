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
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
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
// the tool-call envelope, which costs every seat a corrective round for ever.
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

const llmUsage = `crewlet llm — subscription CLI backends: logins, health and tokens

Usage:
  crewlet llm list                        Providers, agent, model and login state
  crewlet llm doctor [KEY]                Verify end to end (-no-smoke skips the real calls)
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
  -home PATH     Read a host login from somewhere other than this user's home
  -no-smoke      Skip doctor's real completions (the tool call and both probes)
  -print-token   Write a captured token to stdout instead of the store (login only)
`

func runLLM(args []string, stdout, stderr io.Writer) error {
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
	if err := fs.Parse(rest); err != nil {
		return err
	}
	key, given := onePositional(fs, key)
	if given > 1 {
		return fmt.Errorf("llm %s takes one provider key, got %d", sub, given)
	}

	ctx := context.Background()
	providers, closeResolver, err := loadCLIAgents(ctx, *companyPath, *bootstrapPath, stderr)
	if err != nil {
		return err
	}
	defer closeResolver()

	switch sub {
	case "list":
		return listLLMProviders(providers, stdout)
	case "doctor":
		return doctorLLM(ctx, providers, key, !*noSmoke, stdout)
	case "login":
		return loginLLM(ctx, loginRequest{
			providers: providers, key: key, home: *home,
			fromHost: *fromHost, captureToken: *captureToken,
			tokenStdin: *tokenStdin, username: *username, passwordStdin: *passwordStdin,
			bootstrapPath: *bootstrapPath, printToken: *printToken,
		}, stdout, stderr)
	case "status":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		return p.Status(ctx, stdout, stderr)
	case "logout":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		if err := p.Logout(ctx, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Logged %s out and removed its credentials.\n", key)
		return nil
	case "export":
		return exportLLM(ctx, providers, key, *secretStore, *bootstrapPath, stdout)
	case "import":
		return importLLM(providers, key, os.Stdin, stdout)
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

// loadCLIAgents builds every cli-agent provider the company declares.
//
// Through the SAME resolver the provisioning CLIs use — store first, then the
// environment — because a token rotated into the secret store must win over a
// stale `.env` exported into this shell months ago. A command that resolved
// from the environment alone would report a provider as having no token while
// the running engine used one.
func loadCLIAgents(ctx context.Context, companyPath, bootstrapPath string, notes io.Writer) ([]cliAgentProvider, func(), error) {
	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return nil, nil, err
	}
	resolver, closeResolver, err := companyResolver(ctx, bootstrapPath, notes)
	if err != nil {
		return nil, nil, err
	}

	var out []cliAgentProvider
	for _, key := range company.Providers.ProviderOrder() {
		spec := company.Providers.LLM[key]
		if spec.Type != config.LLMCLIAgent {
			continue
		}
		built, err := engine.BuildCLIAgent(key, spec, resolver)
		if err != nil {
			closeResolver()
			return nil, nil, err
		}
		out = append(out, cliAgentProvider{key: key, provider: built})
	}
	if len(out) == 0 {
		closeResolver()
		return nil, nil, fmt.Errorf(
			"%s declares no cli-agent providers — see "+
				"docs/concepts/subscription-llm-backends.md for the config block",
			companyPath)
	}
	return out, closeResolver, nil
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

func providerKeys(providers []cliAgentProvider) []string {
	keys := make([]string, 0, len(providers))
	for _, p := range providers {
		keys = append(keys, p.key)
	}
	slices.Sort(keys)
	return keys
}

func listLLMProviders(providers []cliAgentProvider, stdout io.Writer) error {
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tAGENT\tMODEL\tLOGIN\tSTATE DIR")
	for _, p := range providers {
		model := p.provider.Model()
		if model == "" {
			model = "(the CLI's default)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			p.key, p.provider.Agent(), model,
			p.provider.LoginState(), p.provider.Workspace().Root())
	}
	return w.Flush()
}

func doctorLLM(ctx context.Context, providers []cliAgentProvider, key string, smoke bool, stdout io.Writer) error {
	selected := providers
	if key != "" {
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		selected = []cliAgentProvider{{key: key, provider: p}}
	}
	unhealthy := 0
	for i, p := range selected {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		d := p.provider.Diagnose(ctx, cliagent.DiagnoseOptions{
			Smoke: smoke,
			// THE ENGINE'S OWN ANSWERS, not the provider's guess at them:
			// which runners this build registers and what a sandbox can
			// dial are facts about the process, and both decide whether
			// an agent-mode entry works at all.
			AgentRunners: codingagent.Names(),
			BridgeURL:    os.Getenv(mcpbridge.BaseURLVar),
		})
		d.Render(stdout)
		if !d.Healthy() {
			unhealthy++
		}
	}
	if unhealthy > 0 {
		// A non-zero exit, because `doctor` is what a deploy script runs
		// before it lets a node take seats — a green-looking command that
		// exited 0 with problems printed is one nobody gates on.
		return fmt.Errorf("%d of %d cli-agent providers have problems", unhealthy, len(selected))
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
	printToken    bool
}

func loginLLM(ctx context.Context, req loginRequest, stdout, stderr io.Writer) error {
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
		taken, err := p.AdoptHostLogin(req.home)
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
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("reading the token from stdin: %w", err)
			}
			token = strings.TrimSpace(string(raw))
			if token == "" {
				return errors.New("stdin held no token")
			}
		} else {
			token, err = p.CaptureToken(ctx, os.Stdin, stderr)
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
		if err := storeLLMSecret(ctx, req.bootstrapPath, tokenVar, token); err != nil {
			return err
		}
		fmt.Fprintf(stdout,
			"Stored the headless token as %s in the encrypted secret store.\n"+
				"Reference it from the company document as:\n\n"+
				"  providers:\n    llm:\n      %s:\n        cli:\n          auth:\n"+
				"            token: \"${%s}\"\n", tokenVar, key, tokenVar)
		return nil

	case req.username != "":
		if !req.passwordStdin {
			return errors.New("-username requires -password-stdin: a password on argv is " +
				"visible in ps and lands in shell history")
		}
		raw, err := io.ReadAll(os.Stdin)
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
		if err := p.BrokerLogin(ctx, os.Stdin, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nLogin written to %s.\n", p.Workspace().CredentialsDir())
		fmt.Fprintf(stdout, "Verify it with `crewlet llm doctor %s`.\n", key)
		return nil
	}
}

func exportLLM(ctx context.Context, providers []cliAgentProvider, key string, toStore bool, bootstrapPath string, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
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
	if err := storeLLMSecret(ctx, bootstrapPath, name, bundle); err != nil {
		return err
	}
	// NOT "any engine sharing that database". The store is one file, one
	// process — a second engine pointed at this path is corruption, not a
	// warm standby — so what actually restores a bundle on another node is
	// running this command there too.
	fmt.Fprintf(stdout,
		"Stored the credential bundle as %s in THIS NODE's encrypted secret\n"+
			"store. This engine restores it at boot when its own credentials\n"+
			"directory is empty; on a fleet, run this once per node.\n"+
			"Reference it as:\n\n"+
			"  providers:\n    llm:\n      %s:\n        cli:\n          auth:\n"+
			"            credential_bundle: \"${%s}\"\n", name, key, name)
	return nil
}

// storeLLMSecret writes one value into the encrypted secret store.
func storeLLMSecret(ctx context.Context, bootstrapPath, name, value string) error {
	sv, closeStore, err := openSecretStore(ctx, bootstrapPath, "")
	if err != nil {
		return fmt.Errorf("cannot reach the secret store to save %s: %w", name, err)
	}
	defer closeStore()
	if err := sv.Set(ctx, name, value, currentOperator(), "llm-login", time.Now().UTC()); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
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
// identical from the outside and only one of them means the host is ready.
func importLLM(providers []cliAgentProvider, key string, stdin io.Reader, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
	}
	if p.Workspace().HasLogin() {
		return fmt.Errorf(
			"%s already has a login in %s, and restoring over it would replace "+
				"a fresher refresh token with an older one — log it out first "+
				"(`crewlet llm logout %s`) if you mean to replace it",
			key, p.Workspace().CredentialsDir(), key)
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
	if err := p.RestoreBundle(blob); err != nil {
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

package main

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // a git object id IS a SHA-1: the case names the blob git stored, it trusts no hash of its own to it
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// isCredentialHelperCall reports an argv as git starts the helper `crewlet
// code` configures — `code credential <action>` — which TestMain answers as
// main() would.
func isCredentialHelperCall(args []string) bool {
	return len(args) >= 2 && args[0] == "code" && args[1] == "credential"
}

// helperRaceEnv is the GORACE a git that runs this test binary as its helper
// hands it: without it a race-built helper sleeps a second at every clean
// exit, and git runs one at every request it authenticates.
func helperRaceEnv() string {
	return procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions()
}

// codeHost is a node codeNode booted: the Tier A file the CLI reads its
// address and token from, the API's base and the token, and the node itself.
type codeHost struct {
	cfg, base, token string
	node             *unconfiguredNode
}

// codeNode boots a real engine — code.native, the repositories reposYAML
// declares (each `- name: …` entry of code.repos, indented four spaces), a
// human seat `founder` bound to the token the CLI reads — and serves its API
// as `crewlet run` does, so every `crewlet code` test talks to the production
// routes: the /code/ reads and the git routes, through the real guard. Its
// hook is THIS test binary through the engine's default GitHook (TestMain
// dispatches it), the production argv. It returns once every declared
// repository is listed at the log's head.
func codeNode(t *testing.T, reposYAML string) codeHost {
	t.Helper()
	return codeNodeWith(t, codeDeclaration{repos: reposYAML}, nil)
}

// codeDeclaration is the company's code block as a case writes it: repos, the
// entries of code.repos (each `- name: …`, indented four spaces), and settings,
// the block's other lines (indented two spaces — code.limits).
type codeDeclaration struct {
	repos, settings string
}

// company is the company document declaring d: companyYAML, the founder's
// human seat — which [unconfiguredNode.bindToken] binds the token `ops` to —
// and code.native with d.
func (d codeDeclaration) company() string {
	doc := companyYAML + `  - name: Founder
    handle: founder
    kind: human
code:
  native: true
` + d.settings
	if strings.TrimSpace(d.repos) != "" {
		doc += "  repos:\n" + d.repos
	}
	return doc
}

// codeNodeWith is [codeNode] declaring d, with the Tier A tokens tokens names
// (id → value) beside the founder's — bound to no seat, since the directory
// binds `ops` alone.
func codeNodeWith(t *testing.T, d codeDeclaration, tokens map[string]string) codeHost {
	t.Helper()
	return codeNodeAmended(t, d, tokens, nil)
}

// codeNodeAmended is [codeNodeWith] with amend applied to its Tier A last —
// a node of other roles.
func codeNodeAmended(t *testing.T, d codeDeclaration, tokens map[string]string,
	amend func(*config.Bootstrap)) codeHost {

	t.Helper()
	gittest.RequireNode(t)
	// THE CREDENTIAL [cli] PRESENTS, so a command a case runs through it
	// reaches this node as its operator: a node client reads its token from
	// the invocation's environment and nothing else.
	const token = cliFixtureToken
	n := bootUnconfiguredWith(t, token, func(b *config.Bootstrap) {
		// THE CASE'S OWN, never the machine's: an explicit cache budget
		// and a pinned transfer lane, so the room this machine's disk has
		// and the cores it runs on decide nothing about what it serves.
		b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
		b.Git.MaxTransfers = 4
		for _, id := range slices.Sorted(maps.Keys(tokens)) {
			// A TOKEN STATES WHAT IT MAY DO — Tier A refuses one that
			// lists nothing — and no app is a code permission: what a
			// credential may do to a repository is the standing of the
			// person it is bound to (ADR-0058), so the grant decides
			// nothing a code case reads.
			b.API.Auth.Tokens = append(b.API.Auth.Tokens, config.APIToken{
				ID: id, Token: tokens[id], Grants: []iam.Grant{iam.GrantTracker}})
		}
		if amend != nil {
			amend(b)
		}
	})
	n.reconcile()
	h := codeHost{base: n.base, token: token, node: n}
	h.declare(t, d)
	n.bindToken("ops", "founder")
	// A WHOLE TIER A NAMING THIS NODE'S ADDRESS, for a command that finds its
	// node from a config file: the file is validated before its address is
	// read, and it names the address alone — the credential is the
	// invocation's environment, never the file.
	h.cfg = bootstrapForURL(t, n.base)
	return h
}

// declare applies the company declaring d as a new revision and returns once
// the node serves it and lists every repository d declares at the log's head.
func (h codeHost) declare(t *testing.T, d codeDeclaration) {
	t.Helper()
	var declared []struct {
		Name string `yaml:"name"`
	}
	if strings.TrimSpace(d.repos) != "" {
		if err := yaml.Unmarshal([]byte(d.repos), &declared); err != nil {
			t.Fatalf("the repositories a case declares: %v", err)
		}
	}
	if err := h.node.settle(h.node.put(d.company())); err != nil {
		t.Fatalf("the company: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status, body := h.node.get(t, "/code/repos?read_level=linearizable")
		var listing struct {
			Repos []struct {
				Name string `json:"name"`
			} `json:"repos"`
		}
		if status == http.StatusOK && json.Unmarshal([]byte(body), &listing) == nil &&
			len(listing.Repos) == len(declared) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the declared repositories were never listed: GET /code/repos = %d %s", status, body)
		}
	}
}

// push commits files to a fresh history and pushes it to the repository's
// refspecs as the founder, failing the case unless every ref landed.
func (h codeHost) push(t *testing.T, repo string, files map[string]string, refspecs ...string) string {
	t.Helper()
	person := codetest.PersonWithToken(t, h.token)
	dir := person.Clone(h.base + "/git/" + repo + ".git")
	commit := person.Commit(dir, files, "a commit of "+strings.Join(slices.Sorted(mapKeys(files)), ","))
	pushed := person.Push(t.Context(), dir, h.base+"/git/"+repo+".git", refspecs...)
	for _, spec := range refspecs {
		_, dst, _ := strings.Cut(spec, ":")
		if !pushed.OK(dst) {
			t.Fatalf("the push of %s did not land:\n%s", spec, pushed)
		}
	}
	return commit
}

// mapKeys is m's keys, for a commit message.
func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// hermeticGit is the real git behind a stand-in that strips the developer's
// own configuration — a HOME and a configuration home of its own, no system or
// global file, no system attributes, the C locale and no proxy
// ([hermeticGitIn]) — and hands the helper it runs the race options, for the
// case whose subject is the CLI running the operator's git: what that git
// does must not depend on the machine the suite runs on. Everything else of
// the environment is the CLI's to pass on, and is.
func hermeticGit(t *testing.T) string {
	t.Helper()
	return hermeticGitWith(t, "")
}

// hermeticGitWith is [hermeticGit] running more — POSIX sh, "$@" git's
// arguments — before the real git: what an operator's own environment would
// have set, or a step of git's work a case holds.
func hermeticGitWith(t *testing.T, more string) string {
	t.Helper()
	git, _ := hermeticGitIn(t, "/dev/null", more)
	return git
}

// hermeticGitIn is [hermeticGitWith] reading global as its global
// configuration file, for the case that hands the operator's git a setting of
// their own, and the HOME it gives git.
//
// What it replaces is every input of git's the suite's scrub cannot remove
// ([scrubGitEnv] takes git's own variables out of the whole process): the
// FILES git reads beside them — no system configuration or attributes, the
// global configuration named, and a HOME and a configuration home of its own,
// since git takes its global attributes and ignore files from
// $XDG_CONFIG_HOME/git before HOME, whatever GIT_CONFIG_GLOBAL says — the
// LOCALE, which translates the sentences of git's a case reads; and curl's
// PROXY variables, the scheme's own and ALL_PROXY with NO_PROXY, under which
// even a request to the node on this machine goes to the person's proxy.
// Pinned on git alone rather than taken from the suite: the process keeps its
// HOME and XDG_CACHE_HOME for the store's per-user cache, and its locale and
// proxies are the person's to keep for everything that is not git
// ([TestTheOperatorsGitInTheSuiteReadsNothingOfTheMachine]).
func hermeticGitIn(t *testing.T, global, more string) (git, home string) {
	t.Helper()
	home = t.TempDir()
	return gittest.Wrap(t, "export HOME="+shellQuote(home)+
		" XDG_CONFIG_HOME="+shellQuote(filepath.Join(home, ".config"))+" LC_ALL=C"+
		" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="+shellQuote(global)+" GIT_ATTR_NOSYSTEM=1 "+
		procgrouptest.GORACE+"="+shellQuote(procgrouptest.StandInRaceOptions())+"\n"+
		"unset http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY\n"+
		more), home
}

// THE OPERATOR'S GIT A CASE STARTS READS NOTHING OF THE MACHINE THROUGH WHAT
// THE SCRUB CANNOT REACH ([hermeticGit], [scrubGitEnv]). The CLI hands its git
// the whole environment, as it must an operator's, and three of git's inputs
// there are no GIT_ variable: the configuration home, where git finds its
// global attributes and ignore files whatever GIT_CONFIG_GLOBAL says (a
// developer's `* -delta` there turned deltas off and moved the
// repository-limit case's refusal a step later); the locale, which translates
// every sentence of git's a case reads; and curl's proxy variables, through
// which even a request to this machine's own node goes to the person's proxy.
// Planted here as a developer's shell holds them, and read back through git
// itself, run the way the CLI runs it, over this process's environment.
//
// Mutation: drop hermeticGitIn's XDG_CONFIG_HOME → the planted attributes and
// ignore files answer (red). Drop its LC_ALL → the locale row (red). Drop its
// unset of the proxies → the node hears nothing (red).
func TestTheOperatorsGitInTheSuiteReadsNothingOfTheMachine(t *testing.T) {
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"attributes": "* -delta\n", "ignore": "*\n"} {
		if err := os.WriteFile(filepath.Join(xdg, "git", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var heard atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heard.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(node.Close)
	unreachable := "http://127.0.0.1:1"
	for name, value := range map[string]string{
		"XDG_CONFIG_HOME": xdg,
		"LC_ALL":          "de_DE.UTF-8", "LANG": "de_DE.UTF-8", "LANGUAGE": "de",
		"http_proxy": unreachable, "https_proxy": unreachable, "HTTPS_PROXY": unreachable,
		"all_proxy": unreachable, "ALL_PROXY": unreachable, "no_proxy": "", "NO_PROXY": "",
	} {
		t.Setenv(name, value)
	}

	// What git is started under, as the stand-in leaves it.
	dump := filepath.Join(t.TempDir(), "env")
	probe, home := hermeticGitIn(t, "/dev/null", "env > "+shellQuote(dump)+"\nexit 0\n")
	if out, err := exec.CommandContext(t.Context(), probe, "status").CombinedOutput(); err != nil {
		t.Fatalf("the probe: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if name, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
			env[name] = value
		}
	}
	for name, want := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"LC_ALL":          "C",
	} {
		if env[name] != want {
			t.Errorf("the operator's git is started with %s=%q, want %q", name, env[name], want)
		}
	}
	for _, name := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy",
		"ALL_PROXY", "no_proxy", "NO_PROXY"} {
		if value, ok := env[name]; ok {
			t.Errorf("the operator's git is started with %s=%q, want none", name, value)
		}
	}

	// And git agrees, read through the CLI's own git.
	git, home := hermeticGitIn(t, "/dev/null", "")
	g, err := newOperatorGit(git, &nodeClient{base: node.URL, token: "a-token"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	in := func(env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), g.path, args...)
		cmd.Env, cmd.Dir = env, repo
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := in(g.environ, "init", "-q"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// `git var GIT_ATTR_GLOBAL` names the global attributes file git reads
	// (2.42 and newer; an older git knows no such variable, and check-attr
	// below holds the line there).
	if out, err := in(g.environ, "var", "GIT_ATTR_GLOBAL"); err == nil {
		if own := filepath.Join(home, ".config", "git", "attributes"); strings.TrimSpace(out) != own {
			t.Errorf("the operator's git reads the global attributes file %s, want its own HOME's %s",
				strings.TrimSpace(out), own)
		}
	}
	if out, err := in(g.environ, "check-attr", "delta", "--", "f.bin"); err != nil ||
		strings.TrimSpace(out) != "f.bin: delta: unspecified" {
		t.Errorf("git check-attr delta = %q (%v), want it unspecified: the planted `* -delta` was read",
			strings.TrimSpace(out), err)
	}
	if out, err := in(g.environ, "check-ignore", "-v", "f.bin"); err == nil {
		t.Errorf("git check-ignore ignores f.bin (%s): the planted ignore file was read", strings.TrimSpace(out))
	}
	if out, err := in(g.targetEnv(), "ls-remote", g.repoURL("web")); heard.Load() == 0 {
		t.Errorf("the node heard nothing of the operator's git (%v): it went to the planted proxy\n%s", err, out)
	}
}

// ---- dispatch ---------------------------------------------------------------

// `crewlet code` NAMES EVERY SUBCOMMAND, REFUSES ONE IT DOES NOT HAVE, AND THE
// HOOK ASKED BY HAND IS THE HOST'S OWN SENTENCE. main() dispatches the real
// hook before run is reached, so run reaches it only from a test — and then
// it is what a hand-run is.
func TestTheCodeCommandDispatchesItsSubcommands(t *testing.T) {
	t.Parallel()
	out, errOut, err := cli(t, "code")
	if !errors.Is(err, flag.ErrHelp) || out != "" {
		t.Fatalf("`crewlet code` = %v, stdout %q; want the usage and flag.ErrHelp", err, out)
	}
	for _, sub := range []string{"status", "verify", "export", "import", "credential"} {
		if !strings.Contains(errOut, "  "+sub+" ") {
			t.Errorf("the usage does not name %s:\n%s", sub, errOut)
		}
	}
	if _, _, err := cli(t, "code", "teleport"); err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Errorf("an unknown code command = %v, want a refusal naming it", err)
	}
	out, errOut, err = cli(t, "code", "hook", "proc-receive")
	if !errors.Is(err, errSilent) || out != "" ||
		errOut != "crewlet code hook: run by the engine's git, never by hand\n" {
		t.Errorf("a hand-run hook through run = %v, stdout %q, stderr %q; want the host's one sentence",
			err, out, errOut)
	}
}

// ---- credential -------------------------------------------------------------

// THE HELPER ANSWERS ONLY OVER HTTPS, OR PLAIN HTTP TO THIS MACHINE. A Basic
// credential over plain http to another host is the token in the clear on the
// network, so the helper says so and answers nothing; a loopback host
// crosses no network. The user name is fixed — the API reads only the
// password — and the token is the variable's, -token-env naming another.
//
// Mutation: drop the http check, and the token is answered to
// code.example.com over plain http.
func TestTheCredentialHelperAnswersOnlyOverHTTPS(t *testing.T) {
	t.Parallel()
	const answer = "username=crewlet\npassword=s3cret\n"
	env := map[string]string{apiTokenEnv: "s3cret", "OTHER": "other-token"}
	getenv := func(k string) string { return env[k] }
	for _, tc := range []struct {
		name, input string
		args        []string
		getenv      func(string) string
		out, warn   string
	}{
		{name: "https", input: "protocol=https\nhost=code.example.com\n", out: answer},
		{name: "https with a path and git's capabilities", input: "capability[]=authtype\nprotocol=https\n" +
			"host=code.example.com:8443\npath=git/web.git\nwwwauth[]=Basic realm=\"crewlet\"\n\n", out: answer},
		{name: "plain http to another host", input: "protocol=http\nhost=code.example.com\n",
			warn: "not sending a token over plain http to code.example.com; serve the API over HTTPS"},
		{name: "plain http to loopback", input: "protocol=http\nhost=127.0.0.1:8080\n", out: answer},
		{name: "plain http to loopback's range", input: "protocol=http\nhost=127.0.0.2\n", out: answer},
		{name: "plain http to IPv6 loopback", input: "protocol=http\nhost=[::1]:8080\n", out: answer},
		{name: "plain http to localhost", input: "protocol=http\nhost=localhost:8080\n", out: answer},
		{name: "not a node", input: "protocol=ssh\nhost=code.example.com\n"},
		{name: "after the blank line nothing counts", input: "protocol=http\nhost=code.example.com\n\n" +
			"protocol=https\n", warn: "not sending a token"},
		{name: "no token", input: "protocol=https\nhost=code.example.com\n",
			getenv: func(string) string { return "" }},
		{name: "another variable", input: "protocol=https\nhost=code.example.com\n",
			args: []string{"-token-env", "OTHER"}, out: "username=crewlet\npassword=other-token\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lookup := getenv
			if tc.getenv != nil {
				lookup = tc.getenv
			}
			var out, errOut bytes.Buffer
			err := codeCredential(append(slices.Clone(tc.args), "get"), strings.NewReader(tc.input), &out,
				&errOut, lookup)
			if err != nil || out.String() != tc.out {
				t.Errorf("get = %v, stdout %q; want %q", err, out.String(), tc.out)
			}
			if tc.warn == "" && errOut.Len() != 0 || !strings.Contains(errOut.String(), tc.warn) {
				t.Errorf("stderr %q, want %q", errOut.String(), tc.warn)
			}
		})
	}
}

// THE HELPER KEEPS NOTHING: the token's home is the variable, so `store` and
// `erase` read git's attributes to their end and write nothing — no answer,
// no file. And it refuses what is not git's protocol: no action, two, one git
// never sends, more input than a credential request, and a token whose line
// break would end the password and begin an attribute of the token's own.
func TestTheCredentialHelperKeepsNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	getenv := func(k string) string {
		return map[string]string{"HOME": home, apiTokenEnv: "s3cret"}[k]
	}
	attrs := "protocol=https\nhost=code.example.com\nusername=crewlet\npassword=s3cret\n\n"
	for _, action := range []string{"store", "erase"} {
		var out, errOut bytes.Buffer
		if err := codeCredential([]string{action}, strings.NewReader(attrs), &out, &errOut,
			getenv); err != nil || out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("%s = %v, stdout %q, stderr %q; want nothing at all", action, err, out.String(),
				errOut.String())
		}
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("the helper left %v in HOME (%v)", entries, err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		env   string
	}{
		{"no action", nil, attrs, "s3cret"},
		{"two actions", []string{"get", "store"}, attrs, "s3cret"},
		{"an action git never sends", []string{"list"}, attrs, "s3cret"},
		{"more than a request", []string{"get"}, strings.Repeat("x", maxCredentialInput+1), "s3cret"},
		{"a token of two lines", []string{"get"}, attrs, "s3cret\nprotocol=evil"},
	} {
		var out bytes.Buffer
		err := codeCredential(tc.args, strings.NewReader(tc.input), &out, &bytes.Buffer{},
			func(string) string { return tc.env })
		if err == nil || out.Len() != 0 {
			t.Errorf("%s = %v, stdout %q; want a refusal and no answer", tc.name, err, out.String())
		}
	}
}

// GIT USES THE HELPER, AGAINST A REAL NODE: the one case that holds the
// helper's wire format to git itself. A client configured as a person is told
// to — credential.helper = !'<crewlet>' code credential, the binary being this
// test binary, which TestMain answers as main() does — lists a repository
// after the node's challenge with the token from CREWLET_API_TOKEN, and with
// the variable unset is refused, the node's sentence naming the helper.
func TestGitUsesTheHelperAgainstARealNode(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	commit := h.push(t, "web", map[string]string{"README.md": "# web\n"}, "HEAD:refs/heads/main")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := gittest.NewClient(t).Env("GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0="+credentialHelper(self), helperRaceEnv())
	repo := h.base + "/git/web.git"

	out, stderr, err := client.Env(apiTokenEnv+"="+h.token).Try(t, "", nil, "ls-remote", repo)
	if err != nil || !strings.Contains(out, commit+"\trefs/heads/main") {
		t.Fatalf("ls-remote through the helper = %v, %q\n%s; want main at %s", err, out, stderr, commit)
	}
	// NO TOKEN: the helper answers nothing, and git — asked for a name it
	// may not prompt for — gives up with nothing listed.
	out, _, err = client.Try(t, "", nil, "ls-remote", repo)
	if err == nil || out != "" {
		t.Fatalf("ls-remote with no token = %v, %q; want git refused", err, out)
	}
	// A WRONG ONE: the helper answers it, the node refuses it, and git
	// prints the node's sentence — which names this helper.
	out, stderr, err = client.Env(apiTokenEnv+"=not-a-token").Try(t, "", nil, "ls-remote", repo)
	if err == nil || out != "" || !strings.Contains(stderr, "(crewlet code credential)") {
		t.Errorf("ls-remote with a wrong token = %v, %q\n%s; want it refused, the node's sentence naming "+
			"the helper", err, out, stderr)
	}
}

// ---- the operator's git -----------------------------------------------------

// THE NODE'S GIT IS GIVEN THE TOKEN HELPER AND NOTHING ELSE, AND KEEPS THE
// OPERATOR'S OWN SETTINGS. A git that talks to the node clears every helper
// the operator's configuration lists, so a stored and stale password for the
// same host is never sent instead, and runs this binary's; and it numbers its
// two settings after the GIT_CONFIG_COUNT the operator's environment already
// carries, never over it. Held against real git: `git credential fill` asks
// the helpers git would, and `git config` reads the operator's setting.
//
// Mutation: drop the empty credential.helper, and the stale password the
// operator's global helper answers is what git takes; number the settings
// from zero, and the operator's own user.name is lost; leave the helper
// unscoped, and another host is handed the node's token.
func TestTheNodesGitIsGivenOnlyTheTokenHelper(t *testing.T) {
	t.Parallel()
	real := gittest.Require(t)
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	stale := "[credential]\n\thelper = \"!f() { echo username=stale; echo password=stale; }; f\"\n"
	if err := os.WriteFile(global, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=The Operator",
		apiTokenEnv + "=an-older-token", helperRaceEnv(),
	}
	g, err := newOperatorGit(real, &nodeClient{base: "https://code.example.com", token: "the-node-token"},
		environ)
	if err != nil {
		t.Fatal(err)
	}
	filled, err := g.runInput(t, "protocol=https\nhost=code.example.com\n\n", "credential", "fill")
	if err != nil {
		t.Fatalf("git credential fill: %v", err)
	}
	if !strings.Contains(filled, "username=crewlet\npassword=the-node-token\n") {
		t.Errorf("git filled %q, want the node's token from this binary's helper alone", filled)
	}
	// ANOTHER HOST — one a redirect from the node's address would send git
	// to — is offered no token: the helper is the node's URL's alone, and
	// with every other helper cleared and no prompt, git has nothing to fill.
	if other, err := g.runInput(t, "protocol=https\nhost=elsewhere.example.com\n\n", "credential",
		"fill"); err == nil || strings.Contains(other, "the-node-token") {
		t.Errorf("git filled %q (%v) for another host, want no credential at all", other, err)
	}
	name, err := g.run(t.Context(), g.targetEnv(), home, "config", "user.name")
	if err != nil || strings.TrimSpace(string(name)) != "The Operator" {
		t.Errorf("the operator's own user.name read %q (%v), want it kept", name, err)
	}
	if _, err := newOperatorGit(real, &nodeClient{}, []string{"GIT_CONFIG_COUNT=lots"}); err == nil ||
		!strings.Contains(err.Error(), "GIT_CONFIG_COUNT") {
		t.Errorf("a count git cannot read = %v, want it refused by name", err)
	}
}

// runInput is [operatorGit.run] with stdin, for the one command a case feeds:
// git's credential protocol.
func (g operatorGit) runInput(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), g.path, args...)
	cmd.Env, cmd.Dir = g.targetEnv(), t.TempDir()
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, errOut.String())
	}
	return out.String(), nil
}

// ---- status -----------------------------------------------------------------

// A CODE READ'S ANSWER IS READ BY THE READER'S OWN BOUND. The reader ends a page
// on the row that crosses code.MaxPageBytes and keeps it, so an answer carries
// up to that budget, one row more and its envelope; the CLI reads code answers
// up to codeAnswerBytes, which must hold the largest. Built here from the
// bounds that make a row large AT THEIR WORST ENCODING — every byte of every
// ref, every protection pattern and the default branch a `<`, legal in all
// three and written by JSON as six bytes — so a bound that grows, or an
// encoding that does, fails this rather than an operator's command. And the
// pages the reader then serves are past the cap every other answer is read
// by, which is why the code reads have their own.
//
// Mutation: read the code answers by maxNodeResponseBytes, or size
// codeRowBytes by a row whose refs nothing escapes, and this goes red.
func TestTheCodePagesFitOneAnswer(t *testing.T) {
	t.Parallel()
	ref := "refs/heads/" + strings.Repeat("<", gitref.MaxRefNameBytes-len("refs/heads/"))
	branch := strings.Repeat("<", gitref.MaxRefNameBytes-len("refs/heads/"))
	if !gitref.ValidRef(ref) || !gitref.ValidPattern(ref) || !gitref.ValidBranch(branch) {
		t.Fatalf("a ref, a pattern and a branch of `<` are no longer legal (%v %v %v): build the worst row "+
			"from the byte JSON escapes widest that they still admit", gitref.ValidRef(ref),
			gitref.ValidPattern(ref), gitref.ValidBranch(branch))
	}
	oid := strings.Repeat("a", 40)
	pos := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1 << 20, Seq: 1 << 40}
	stamp := time.Date(2026, 10, 14, 23, 59, 59, 999999999, time.UTC)

	// A SEAT'S RUN AT ITS BOUND beside the longest way: every field of it
	// code.MaxProvenanceBytes of `<`, which a stamp carries as it is.
	runField := strings.Repeat("<", code.MaxProvenanceBytes)
	if code.FitProvenance(runField) != runField {
		t.Fatalf("a run field of %d `<` is no longer carried as it is: build the widest run from what "+
			"code.FitProvenance admits", len(runField))
	}
	var via code.PushVia
	for _, v := range code.PushVias() {
		if len(v) > len(via) {
			via = v
		}
	}
	push := code.PushRow{PushID: strings.Repeat("p", 36), RepoID: strings.Repeat("r", 36), Position: pos,
		At: stamp, Actor: strings.Repeat("a", 128), ActorKind: "operator", ActorSeat: strings.Repeat("s", 64),
		Via:  via,
		Run:  code.RunProvenance{TurnID: runField, WorkBackend: runField, WorkKey: runField, Model: runField},
		Node: strings.Repeat("n", 128), Atomic: true, PackID: strings.Repeat("k", 36), PackBytes: 1 << 40,
		IntroducedCount: 1 << 30, ReceivedAt: stamp}
	var longest code.Refusal
	for _, r := range code.Refusals() {
		if len(r) > len(longest) {
			longest = r
		}
	}
	for range code.MaxCommandsPerPush {
		push.Commands = append(push.Commands, code.Command{Ref: ref, Old: oid, New: oid, Forced: true,
			Result: code.ResultNG, Reason: longest})
	}
	for range code.MaxIntroduced {
		push.Introduced = append(push.Introduced, oid)
	}
	repo := code.RepoSummary{RepoRef: code.RepoRef{ID: strings.Repeat("r", 36),
		Name: strings.Repeat("n", gitref.MaxRepoNameBytes), Status: code.RepoStatusArchived,
		ManagedBy: code.ManagedConfig, Owner: code.Owner{Kind: code.OwnerUnit, Ref: strings.Repeat("u", 128)},
		DefaultBranch: branch, Description: strings.Repeat("<", config.CodeMaxDescriptionBytes), DeletedAt: stamp,
		Refs: 1 << 40, LivePacks: 1 << 40, LiveBytes: 1 << 50, LiveObjects: 1 << 40, Position: pos},
		Head: oid, Pushes: 1 << 40, LastPushAt: stamp}
	for range config.CodeMaxProtections {
		repo.Protections = append(repo.Protections, code.Protection{Pattern: ref, AllowForce: true,
			AllowDelete: true, DirectPush: code.DirectPersons})
	}

	// encoded is the length of a page of rows rows that add appends, with the
	// longest cursor a page carries.
	encoded := func(rows int, add func(*code.ReadAnswer)) int {
		page := code.ReadAnswer{Next: strings.Repeat("c", 256), Position: pos, Level: statelog.ReadLinearizable}
		for range rows {
			add(&page)
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	for _, kind := range []struct {
		name    string
		row     any
		perPage int
		add     func(*code.ReadAnswer)
		tiny    func(*code.ReadAnswer)
	}{
		{"pushes", push, code.MaxPushesPerPage, func(a *code.ReadAnswer) { a.Pushes = append(a.Pushes, push) },
			func(a *code.ReadAnswer) { a.Pushes = append(a.Pushes, code.PushRow{}) }},
		{"repositories", repo, code.MaxReposPerPage, func(a *code.ReadAnswer) { a.Repos = append(a.Repos, repo) },
			func(a *code.ReadAnswer) { a.Repos = append(a.Repos, code.RepoSummary{}) }},
	} {
		raw, err := json.Marshal(kind.row)
		if err != nil {
			t.Fatal(err)
		}
		row := len(raw)
		if row > codeRowBytes {
			t.Errorf("the largest row of %s encodes to %d bytes, past codeRowBytes (%d)", kind.name, row,
				codeRowBytes)
		}
		// THE WORST ANSWER: rows one byte short of the page's budget, the row
		// that crosses it at its largest, and the envelope of the most rows
		// a page carries — its commas, cursor, position and level.
		tinyRow, err := json.Marshal(code.PushRow{})
		if kind.name == "repositories" {
			tinyRow, err = json.Marshal(code.RepoSummary{})
		}
		if err != nil {
			t.Fatal(err)
		}
		env := encoded(kind.perPage, kind.tiny) - kind.perPage*len(tinyRow)
		if worst := code.MaxPageBytes - 1 + row + env; worst > codeAnswerBytes {
			t.Errorf("the largest answer of %s can carry %d bytes, past the %d the code reads are read to",
				kind.name, worst, codeAnswerBytes)
		}
		// AND A PAGE THE READER SERVES: maximal rows until one crosses the
		// budget, as page.add closes it.
		page := code.ReadAnswer{Next: strings.Repeat("c", 256), Position: pos, Level: statelog.ReadLinearizable}
		for filled := 0; filled < code.MaxPageBytes; filled += row {
			kind.add(&page)
		}
		served, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(served) > codeAnswerBytes {
			t.Errorf("a page of the largest %s is %d bytes, past the %d the code reads are read to", kind.name,
				len(served), codeAnswerBytes)
		}
		if len(served) <= maxNodeResponseBytes {
			t.Errorf("a page of the largest %s is %d bytes, inside the %d every other answer is read to: "+
				"the case no longer builds the page the code reads' own bound is for", kind.name, len(served),
				maxNodeResponseBytes)
		}
		t.Logf("%s: the largest row %d bytes, its envelope %d, a page the reader serves %d, the worst answer "+
			"%d of the %d read", kind.name, row, env, len(served), code.MaxPageBytes-1+row+env, codeAnswerBytes)
	}
}

// EVERY CODE READ IS READ BY THE CODE READS' OWN CAP. The arithmetic above says
// codeAnswerBytes holds the reader's largest page; this says each command
// reads by it — the status listing, a repository's detail and its history,
// the verify sweep's listing and the export's first read — by answering each
// with a page past the cap every other answer is read to and short of the
// code reads' own.
//
// Mutation: read any one of them by maxNodeResponseBytes, and that command
// fails with the answer refused unread.
func TestEveryCodeReadTakesAPagePastTheGeneralCap(t *testing.T) {
	t.Parallel()
	padding := strings.Repeat("x", maxNodeResponseBytes+64<<10)
	if len(padding)+1<<10 > codeAnswerBytes {
		t.Fatalf("a padding of %d bytes is past codeAnswerBytes (%d): the case no longer builds an answer the "+
			"code reads take and the rest refuse", len(padding), codeAnswerBytes)
	}
	const position = `"position":{"stream":"CREWLET_CODE_LOG","generation":1,"seq":7}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/code/repos/web/verify":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","reason":"not_held","detail":"web is not held here"}`))
			return
		case r.URL.Path == "/code/repos":
			body = `{"repos":[{"name":"web","status":"active"}],` + position
		case r.URL.Path == "/code/repos/web":
			body = `{"repo":{"name":"web","status":"active"},` + position
		case r.URL.Path == "/code/repos/web/pushes":
			body = `{"pushes":[],` + position
		case r.URL.Path == "/code/repos/gone":
			body = `{"repo":{"name":"gone","status":"deleted"},` + position
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no_route","detail":"no such route"}`))
			return
		}
		_, _ = w.Write([]byte(body + `,"padding":"` + padding + `"}`))
	}))
	t.Cleanup(server.Close)
	node := []string{"-url", server.URL}
	for _, c := range []struct {
		args []string
		// ok is whether the command succeeds; the export of a deleted
		// repository fails, AFTER it has read the answer, in its own words.
		ok   bool
		says string
	}{
		{[]string{"code", "status"}, true, "web"},
		{[]string{"code", "status", "-repo", "web"}, true, "web"},
		{[]string{"code", "verify"}, true, "not held on this node"},
		{[]string{"code", "export", "gone", filepath.Join(t.TempDir(), "gone.bundle")}, false, "gone is deleted"},
	} {
		out, _, err := cli(t, append(c.args, node...)...)
		said := out
		if err != nil {
			said += err.Error()
		}
		if (err == nil) != c.ok || !strings.Contains(said, c.says) || strings.Contains(said, "exceeded") {
			t.Errorf("%v of an answer past the general cap = %v, stdout %q; want it read, saying %q", c.args,
				err, textcut.Bytes(out, 300), c.says)
		}
	}
}

// `crewlet code status` READS THE NODE: every repository with its owner,
// default branch, head, refs, packs, size and last push, and the position it
// was read at; one repository in detail — its rules, its refs, its packs and
// the serving node's limits — with its newest pushes, who made each and what
// became of every ref; the node's answers as they came with -json; and a
// repository nobody declared refused in the node's words, naming code.repos.
func TestCodeStatusReadsTheNode(t *testing.T) {
	t.Parallel()
	h := codeNode(t, `    - name: web
      owner: {seat: founder}
      description: The customer-facing web app.
      protect:
        - branch: main
    - name: docs
      owner: {seat: founder}
`)
	first := h.push(t, "web", map[string]string{"README.md": "# web\n"}, "HEAD:refs/heads/main")
	h.push(t, "web", map[string]string{"x.txt": "x\n"}, "HEAD:refs/heads/feature")

	out, errOut, err := cli(t, "code", "status", h.cfg)
	if err != nil {
		t.Fatalf("code status: %v\n%s", err, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("the listing is %q, want a header and two repositories", out)
	}
	if f := strings.Fields(lines[1]); len(f) < 8 || f[0] != "docs" || f[1] != "active" || f[2] != "seat:founder" ||
		f[3] != "main" || f[4] != "-" || f[len(f)-1] != "-" {
		t.Errorf("docs reads %q, want active, the founder's, main, no head, never pushed", lines[1])
	}
	if f := strings.Fields(lines[2]); len(f) < 8 || f[0] != "web" || f[3] != "main" || f[4] != first[:7] ||
		f[5] != "2" || f[6] != "2" {
		t.Errorf("web reads %q, want main at %s, two refs and two packs", lines[2], first[:7])
	}
	if !strings.HasPrefix(errOut, "read at CREWLET_CODE_LOG@") {
		t.Errorf("the listing said %q on stderr, want the position it was read at", errOut)
	}

	out, _, err = cli(t, "code", "status", h.cfg, "-repo", "web")
	if err != nil {
		t.Fatalf("code status -repo web: %v", err)
	}
	for _, want := range []string{
		"web — active, owned by seat:founder, default branch main\n",
		"  description:  The customer-facing web app.\n",
		"  visibility:   company — every seat reads it\n",
		"  protections:  refs/heads/main (no force, no delete)\n",
		"  refs:         2 branches, 0 tags; main at " + first + "\n",
		"  pushes:       2 ever",
		"PUSHED AT",
		"ops (founder)",
		"ok refs/heads/main 0000000.." + first[:7],
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the detail does not say %q:\n%s", want, out)
		}
	}
	out, _, err = cli(t, "code", "status", h.cfg, "-repo", "web", "-pushes", "1")
	if err != nil || strings.Count(out, "ops (founder)") != 1 || !strings.Contains(out, "refs/heads/feature") {
		t.Errorf("-pushes 1 printed %q (%v), want only the newest push", out, err)
	}

	out, _, err = cli(t, "code", "status", h.cfg, "-json")
	if err != nil {
		t.Fatalf("code status -json: %v", err)
	}
	var page struct {
		Repos []json.RawMessage `json:"repos"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil || len(page.Repos) != 2 {
		t.Errorf("-json printed %q (%v), want the node's one page of two repositories", out, err)
	}

	_, _, err = cli(t, "code", "status", h.cfg, "-repo", "nope")
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "code.repos") {
		t.Errorf("status of an undeclared repository = %v, want a refusal naming it and code.repos", err)
	}
}

// A REPOSITORY'S DETAIL SAYS WHO CHANGES IT AND HOW IT COMES BACK: one
// code.repos declares is restored by declaring it again, one a tool made by
// a restore through the tools — a configuration never adopts it, so the
// declared repository's sentence would send a person to a refusal — a
// scratch repository says whose it is and the clock its idle expiry reads,
// and a repository asked for by an old name says so and names the current
// one.
//
// Mutation: print every deleted repository's restore as a declaration's —
// the tool-made case, red.
func TestARepositorysDetailSaysWhoChangesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, view string
		want       []string
	}{
		{"a declared one, deleted",
			`{"name":"web","status":"deleted","managed_by":"config","kind":"project",` +
				`"deleted_at":"2026-10-20T09:00:00Z","owner":{"kind":"unit","ref":"eng"}}`,
			[]string{"web — deleted on 2026-10-20 09:00 UTC; declaring it again in code.repos restores it",
				"  managed by:   code.repos\n"}},
		{"a tool-made one, deleted",
			`{"name":"api","status":"deleted","managed_by":"tools","kind":"project",` +
				`"deleted_at":"2026-10-20T09:00:00Z","owner":{"kind":"unit","ref":"eng"}}`,
			[]string{"api — deleted on 2026-10-20 09:00 UTC; a restore through the code tools brings it back",
				"  managed by:   the code tools — a project\n"}},
		{"a scratch repository",
			`{"name":"alice.spike","status":"active","managed_by":"tools","kind":"scratch",` +
				`"last_activity_at":"2026-10-27T10:30:00Z","owner":{"kind":"seat","ref":"alice"},` +
				`"default_branch":"main"}`,
			[]string{"alice.spike — active, owned by seat:alice, default branch main\n",
				"  managed by:   the code tools — alice's scratch repository, last active 2026-10-27 10:30 UTC, " +
					"deleted once idle for code.scratch.idle_days unless a live turn holds it\n"}},
		{"one asked for by an old name",
			`{"name":"web","status":"active","managed_by":"tools","kind":"project","redirect":"web",` +
				`"redirect_until":"2026-11-27T10:00:00Z","owner":{"kind":"unit","ref":"eng"},` +
				`"default_branch":"main"}`,
			[]string{"  renamed:      the name asked for is an old one, redirecting here until 2026-11-27 10:00 UTC; " +
				"set a remote to /git/web.git\n"}},
	} {
		var r codeRepoView
		if err := json.Unmarshal([]byte(tc.view), &r); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var out strings.Builder
		if err := renderCodeRepo(&out, r, nil); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s does not say %q:\n%s", tc.name, want, out.String())
			}
		}
	}
}

// A PUSH LINE NAMES WHO MADE THE PUSH AS ITS ROW DOES: a person's credential
// with the seat they hold beside it; a seat by its handle — the actor and the
// seat at once, so never twice — with the turn that made the push and the work
// item it was charged to, where the row names them; and nothing for what a row
// does not say.
//
// Mutation: print a seat's push as a person's (`<actor> (<seat>)`) — the
// handle twice, and no turn.
func TestAPushLineNamesWhoMadeIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, row, want string
	}{
		{"a person's", `{"actor":"pat:019a2f4e","actor_kind":"operator","actor_seat":"founder","via":"git"}`,
			"pat:019a2f4e (founder)"},
		{"an unbound token's", `{"actor":"token:ci","actor_kind":"operator","via":"git"}`, "token:ci"},
		{"a seat's on a work item", `{"actor":"docs-writer","actor_kind":"agent","actor_seat":"docs-writer",` +
			`"via":"engine","run":{"turn_id":"0192f00d-0000-7000-8000-000000000001","work_backend":"native",` +
			`"work_key":"ENG-12","model":"claude-sonnet-4-5"}}`,
			"docs-writer (turn 0192f00d-0000-7000-8000-000000000001, native ENG-12)"},
		{"a seat's on no work item", `{"actor":"docs-writer","actor_kind":"agent","actor_seat":"docs-writer",` +
			`"via":"engine","run":{"turn_id":"t-1"}}`, "docs-writer (turn t-1)"},
	} {
		var p codePushView
		if err := json.Unmarshal([]byte(tc.row), &p); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := pushLine(p).by; got != tc.want {
			t.Errorf("%s push is printed by %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A COMPANY THAT DECLARES NO REPOSITORY IS SAID SO, never an empty table — by
// status and by verify alike; and with -json verify says it beside the
// node's answers, keeping stdout to them, as every other sentence of it is.
//
// Mutation: say it on stdout under -json, and a script reading the JSON lines
// meets prose.
func TestCodeCommandsSayWhenNothingIsDeclared(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "")
	for _, verb := range []string{"status", "verify"} {
		out, _, err := cli(t, "code", verb, h.cfg)
		if err != nil || out != "no repositories: code.repos declares none\n" {
			t.Errorf("code %s = %v, %q; want it said that none is declared", verb, err, out)
		}
	}
	out, errOut, err := cli(t, "code", "verify", h.cfg, "-json")
	if err != nil || out != "" || errOut != "no repositories: code.repos declares none\n" {
		t.Errorf("code verify -json = %v, stdout %q, stderr %q; want the sentence on stderr alone", err, out,
			errOut)
	}
}

// ---- verify -----------------------------------------------------------------

// `crewlet code verify` CHECKS THE NODE'S CACHE, ALL OF IT, AND SAYS WHAT IT
// FOUND: a copy that is its rows is ok; one the node does not hold is passed
// over, since nothing there can be damaged; a damaged copy is named with
// every problem and the command exits non-zero — the hook a cron runs on each
// git node — and -repair rebuilds it whole. A name nobody declared is refused
// in the node's words, naming code.repos, and counted.
//
// The damage is to the README's blob, whose bytes no git process on the node
// reads on its own — not the maintainer's graph or bitmap, nor the check's
// connectivity-only fsck — so the node's scrub is never asked and only the
// verify finds it: damage a git process does read, reported, is repaired by
// the node behind the serving before an operator could ask, and a case
// damaging the middle of the pack met that repair first in five runs of five.
func TestCodeVerifyChecksTheNodesCache(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n    - name: docs\n      owner: {seat: founder}\n")
	h.push(t, "web", map[string]string{"README.md": "# web\n"}, "HEAD:refs/heads/main")

	out, errOut, err := cli(t, "code", "verify", h.cfg)
	if err != nil || !strings.Contains(out, "web: ok (1 packs, 1 refs)\n") ||
		!strings.Contains(out, "docs: not held on this node") {
		t.Fatalf("code verify = %v\nstdout:\n%s\nstderr:\n%s\nwant web ok and docs passed over", err, out, errOut)
	}

	// DAMAGE: the compressed bytes of web's README blob, as a disk would.
	status, body := h.node.get(t, "/code/repos/web")
	var detail struct {
		Repo struct {
			ID string `json:"id"`
		} `json:"repo"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil || status != http.StatusOK {
		t.Fatalf("GET /code/repos/web = %d %s", status, body)
	}
	cacheDir := h.node.boot.Git.CacheDirFor(h.node.boot.Store.Path)
	copies, err := filepath.Glob(filepath.Join(cacheDir, "repos", detail.Repo.ID+".*.git"))
	if err != nil || len(copies) != 1 {
		t.Fatalf("web's cached copies are %v (%v), want one", copies, err)
	}
	readme := []byte("# web\n")
	blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(readme))), readme...))
	gittest.Damage(t, copies[0], gittest.EntryBytes, hex.EncodeToString(blob[:]))

	out, _, err = cli(t, "code", "verify", h.cfg, "-repo", "web")
	if err == nil || !strings.Contains(err.Error(), "1 of 1 repositories did not verify clean") ||
		!strings.Contains(out, "problem(s) (1 packs, 1 refs):\n") || !strings.Contains(out, "hashes to") ||
		!strings.Contains(out, "-repair") {
		t.Fatalf("a verify of a damaged copy = %v\n%s\nwant the problem named and a non-zero exit", err, out)
	}
	out, errOut, err = cli(t, "code", "verify", h.cfg, "-repo", "web", "-repair")
	if err != nil || !strings.Contains(out, "web: damaged, and rebuilt") ||
		!strings.Contains(out, "web: the rebuilt copy is whole\n") {
		t.Fatalf("a repair = %v\nstdout:\n%s\nstderr:\n%s\nwant it rebuilt whole", err, out, errOut)
	}
	// AS JSON, stdout is the node's verifications alone, one a line, and
	// what is said of them — docs passed over among it — goes beside them.
	out, errOut, err = cli(t, "code", "verify", h.cfg, "-json")
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); err != nil || len(lines) != 1 ||
		!json.Valid([]byte(lines[0])) || !strings.Contains(lines[0], `"state":"done"`) ||
		!strings.Contains(errOut, "docs: not held on this node") {
		t.Errorf("a verify after the repair, as JSON = %v\nstdout %q\nstderr %q\nwant web's done "+
			"verification alone on stdout", err, out, errOut)
	}

	_, errOut, err = cli(t, "code", "verify", h.cfg, "-repo", "nope", "-repo", "web")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 repositories") ||
		!strings.Contains(errOut, `"nope"`) || !strings.Contains(errOut, "code.repos") {
		t.Errorf("a verify naming an undeclared repository = %v, stderr %q; want it refused and counted, "+
			"the rest verified", err, errOut)
	}
}

// WHAT THE SYNC BEFORE A CHECK REPAIRED IS TOLD FIRST, AND LEAVES THE COPY
// CLEAN. A node whose first sync of a copy found a file damaged and put it
// right in place reports it apart from the problems (repaired_by_sync): the
// copy it then checked holds no such damage, so the command says what was
// repaired — a disk that damaged one file may hold others damaged — and
// answers the copy clean, which is no failure for a cron and no reason to run
// it again with -repair.
//
// Mutation: read repaired_by_sync as problems (the copy answered unclean, and
// told to run again with -repair).
func TestWhatTheSyncRepairedIsToldAndLeavesTheCopyClean(t *testing.T) {
	t.Parallel()
	var v codeVerificationView
	if err := json.Unmarshal([]byte(`{"id":"v1","repo":"web","state":"done","report":{"packs":3,"refs":2,`+
		`"repaired_by_sync":["the sync before the check found objects/pack/multi-pack-index-1.bitmap failing `+
		`its checksum and dropped it to be written again"]}}`), &v); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	clean := renderVerification(&out, &errOut, "web", v, false, false)
	if !clean {
		t.Errorf("a copy whose sync repaired a file answered unclean:\n%s", out.String())
	}
	want := "web: the sync before the check found damage on disk and repaired it in place:\n" +
		"  the sync before the check found objects/pack/multi-pack-index-1.bitmap failing its checksum and " +
		"dropped it to be written again\n" +
		"web: ok (3 packs, 2 refs)\n"
	if out.String() != want {
		t.Errorf("rendered\n%s\nwant\n%s", out.String(), want)
	}
	if strings.Contains(out.String(), "-repair") {
		t.Error("a copy left clean was told to run again with -repair")
	}
}

// A REPAIR IS TOLD AS THE NODE MADE IT. A check asked to repair puts the copy's
// own directory right IN PLACE, keeping every pack and index, and rebuilds
// only what that cannot end (cache.Verification.Run): an operator reading
// "rebuilt" for a repair in place would expect the bitmapless clones a new
// copy has until the node indexes it again, and one reading neither would
// not know the copy had been touched at all. Each way is told with what the
// check before it found, and a copy left whole is clean.
//
// Mutation: drop the in-place cases from the heading's switch, and the
// repaired copy prints only "the repaired copy is whole" with nothing found
// before it.
func TestARepairIsToldAsTheNodeMadeIt(t *testing.T) {
	t.Parallel()
	const found = `"found":["an entry the cache does not write: description"]`
	for _, tc := range []struct {
		name, report, want string
		clean              bool
	}{{
		name:   "in place",
		report: `{"packs":3,"refs":2,"repaired_in_place":true,` + found + `}`,
		want: "web: out of place, and repaired in place (3 packs, 2 refs); the check before the repair found:\n" +
			"  an entry the cache does not write: description\n" +
			"web: the repaired copy is whole\n",
		clean: true,
	}, {
		name:   "in place, then rebuilt",
		report: `{"packs":3,"refs":2,"repaired_in_place":true,"rebuilt":true,` + found + `}`,
		want: "web: out of place, repaired in place and then rebuilt for what that left (3 packs, 2 refs); the " +
			"check before the repair found:\n" +
			"  an entry the cache does not write: description\n" +
			"web: the rebuilt copy is whole\n",
		clean: true,
	}, {
		name:   "rebuilt",
		report: `{"packs":3,"refs":2,"rebuilt":true,"found":["pack p hashes to a; its row says b"]}`,
		want: "web: damaged, and rebuilt (3 packs, 2 refs); the check before the rebuild found:\n" +
			"  pack p hashes to a; its row says b\n" +
			"web: the rebuilt copy is whole\n",
		clean: true,
	}, {
		name: "in place, left a problem",
		report: `{"packs":3,"refs":2,"repaired_in_place":true,` + found +
			`,"problems":["an entry the cache does not write: hooks/post-update"]}`,
		want: "web: out of place, and repaired in place (3 packs, 2 refs); the check before the repair found:\n" +
			"  an entry the cache does not write: description\n" +
			"web: the copy repaired in place still has 1 problem(s):\n" +
			"  an entry the cache does not write: hooks/post-update\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v codeVerificationView
			if err := json.Unmarshal([]byte(`{"id":"v1","repo":"web","state":"done","report":`+tc.report+`}`),
				&v); err != nil {
				t.Fatal(err)
			}
			var out, errOut strings.Builder
			if clean := renderVerification(&out, &errOut, "web", v, true, false); clean != tc.clean {
				t.Errorf("clean = %v, want %v", clean, tc.clean)
			}
			if out.String() != tc.want {
				t.Errorf("rendered\n%s\nwant\n%s", out.String(), tc.want)
			}
		})
	}
}

// A VERIFY WAITS FOR THE NODE'S CHECK, AS THE NODE SAYS TO. Against the routes'
// contract, scripted: a check that ended while its request waited is answered
// at once, with no read after; a refusal the node says clears by waiting is
// asked again after its Retry-After, and the check is read until it ends; one
// still refused past the time the command gives refusals is reported; a node
// that forgot the check, one that goes silent past the contact the command
// keeps, a check that failed, and one still running past the deadline the
// node stated for it are each a verify that did not end clean, said as what
// it is; a copy the node does not hold is passed over; and the verify asks at
// the floor it is given.
//
// Mutation: ignore the Retry-After, and the busy repository fails; ignore
// the node's deadline, and the late one is waited for until the case gives up.
func TestAVerifyWaitsForTheNodesCheck(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		asked = map[string]int{}
		posts = map[string]string{}
		// late's check states one deadline a moment off on every answer, and
		// never ends by it.
		lateBy = time.Now().Add(20 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.Method+" "+r.URL.Path]++
		n := asked[r.Method+" "+r.URL.Path]
		if r.Method == http.MethodPost {
			posts[r.URL.Path] = r.URL.RawQuery
		}
		mu.Unlock()
		answer := func(status int, body string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
		running := func(repo string) string {
			return `{"id":"v-` + repo + `","repo":"` + repo + `","state":"running"}`
		}
		repo := strings.Split(r.URL.Path, "/")[3]
		switch r.Method + " " + r.URL.Path {
		case "POST /code/repos/busy/verify":
			if n == 1 {
				w.Header().Set("Retry-After", "1")
				answer(http.StatusServiceUnavailable, `{"error":"unavailable","detail":"the lane is busy"}`)
				return
			}
			answer(http.StatusAccepted, running("busy"))
		case "GET /code/repos/busy/verify/v-busy":
			if n < 3 {
				answer(http.StatusOK, running("busy"))
				return
			}
			answer(http.StatusOK, `{"id":"v-busy","repo":"busy","state":"done","report":{"packs":2,"refs":5}}`)
		case "POST /code/repos/quick/verify":
			answer(http.StatusOK, `{"id":"v-quick","repo":"quick","state":"done","report":{"packs":1,"refs":1}}`)
		case "POST /code/repos/jammed/verify":
			w.Header().Set("Retry-After", "1")
			answer(http.StatusServiceUnavailable, `{"error":"unavailable","detail":"the lane is busy"}`)
		case "POST /code/repos/late/verify", "GET /code/repos/late/verify/v-late":
			answer(http.StatusAccepted, `{"id":"v-late","repo":"late","state":"running","deadline":"`+lateBy+`"}`)
		case "POST /code/repos/forgot/verify", "POST /code/repos/silent/verify",
			"POST /code/repos/broken/verify":
			answer(http.StatusAccepted, running(repo))
		case "GET /code/repos/forgot/verify/v-forgot":
			answer(http.StatusNotFound, `{"error":"not_found","detail":"this node keeps no verify"}`)
		case "GET /code/repos/silent/verify/v-silent":
			panic(http.ErrAbortHandler)
		case "GET /code/repos/broken/verify/v-broken":
			answer(http.StatusOK, `{"id":"v-broken","repo":"broken","state":"failed","error":"query_failed",`+
				`"detail":"pack p1 is missing","report":{"packs":1,"problems":["pack p1 hashes to 01"]}}`)
		case "POST /code/repos/elsewhere/verify":
			answer(http.StatusNotFound, `{"error":"not_found","reason":"not_held","detail":"holds no copy"}`)
		case "POST /code/repos/nowhere/verify":
			answer(http.StatusNotFound, `{"error":"no_route","detail":"no such route"}`)
		default:
			answer(http.StatusNotFound, `{"error":"not_found","detail":"no such route in this script"}`)
		}
	}))
	t.Cleanup(server.Close)
	client := &nodeClient{base: server.URL, token: "t", http: httpxtest.Pool(t)}
	waits := codeVerifyWaits{pollFirst: time.Millisecond, pollMax: 5 * time.Millisecond,
		lostContact: 50 * time.Millisecond, grace: 10 * time.Millisecond, refusedFor: 1500 * time.Millisecond}
	floor := codePosition{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 42}

	for _, tc := range []struct {
		repo    string
		clean   bool
		out     string
		errText string
	}{
		{"busy", true, "busy: ok (2 packs, 5 refs)\n", "asking again in 1s"},
		{"quick", true, "quick: ok (1 packs, 1 refs)\n", ""},
		{"jammed", false, "", "the lane is busy"},
		{"late", false, "", "said it would end its check (verification v-late) by"},
		{"forgot", false, "", "no longer keeps verification v-forgot"},
		{"silent", false, "", "lost the node while it checked"},
		{"broken", false, "", "found before it failed: pack p1 hashes to 01"},
		{"elsewhere", true, "elsewhere: not held on this node", ""},
	} {
		var out, errOut bytes.Buffer
		clean, err := verifyOne(t.Context(), client, tc.repo, floor, false, false, waits, &out, &errOut)
		if err != nil || clean != tc.clean || !strings.Contains(out.String(), tc.out) ||
			!strings.Contains(errOut.String(), tc.errText) {
			t.Errorf("a verify of %s = %v, %v\nstdout %q\nstderr %q\nwant %v, %q and %q", tc.repo, clean, err,
				out.String(), errOut.String(), tc.clean, tc.out, tc.errText)
		}
	}
	mu.Lock()
	quick, jammed, busy := asked["GET /code/repos/quick/verify/v-quick"], asked["POST /code/repos/jammed/verify"],
		posts["/code/repos/busy/verify"]
	mu.Unlock()
	if n := quick; n != 0 {
		t.Errorf("a check answered done was read %d more times, want none", n)
	}
	if n := jammed; n != 2 {
		t.Errorf("a verify refused for now was asked %d times, want twice inside the time refusals are given", n)
	}
	if q := busy; q != "min_position=CREWLET_CODE_LOG%401%3A42" {
		t.Errorf("a verify asked %q, want it at its floor", q)
	}

	// A NODE SERVING NO VERIFY is the -url's, not the repository's: an error
	// that stops a sweep, naming the flag.
	if _, err := verifyOne(t.Context(), client, "nowhere", floor, false, false, waits, io.Discard,
		io.Discard); err == nil || !strings.Contains(err.Error(), "point -url at one") {
		t.Errorf("a verify of a node serving no verify = %v, want an error naming -url", err)
	}
}

// A SWEEP OF A NODE THAT SERVES NO VERIFY STOPS AT ITS FIRST ANSWER: whether the
// route exists is the node's, never the repository's, so one refusal says the
// -url is wrong — once, not once a repository.
//
// Mutation: count a missing route against the repository and go on, and the
// sweep asks the node once for each.
func TestAVerifySweepOfANodeWithNoVerifyStopsAtOnce(t *testing.T) {
	t.Parallel()
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no_route","detail":"no such route"}`))
			return
		}
		_, _ = w.Write([]byte(`{"repos":[{"name":"a"},{"name":"b"},{"name":"c"}],` +
			`"position":{"stream":"CREWLET_CODE_LOG","generation":0,"seq":7}}`))
	}))
	t.Cleanup(server.Close)
	_, _, err := cli(t, "code", "verify", "-url", server.URL)
	if err == nil || !strings.Contains(err.Error(), "point -url at one") || posts.Load() != 1 {
		t.Errorf("a sweep of a node serving no verify = %v after %d verifies, want one, and an error naming "+
			"-url", err, posts.Load())
	}
}

// A NODE SERVING NO CODE READS IS NAMED BY THE FLAG THE COMMAND TAKES: every
// `crewlet code` command names its node with -url, so the sentence for a
// node without the routes says -url, never the -api the config and secrets
// clients take.
func TestACodeReadOfANodeWithoutTheRoutesNamesURL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no_route","detail":"no such route"}`))
	}))
	t.Cleanup(server.Close)
	for _, args := range [][]string{
		{"code", "status", "-url", server.URL},
		{"code", "status", "-repo", "web", "-url", server.URL},
		{"code", "verify", "-url", server.URL},
	} {
		_, _, err := cli(t, args...)
		if err == nil || !strings.Contains(err.Error(), "point -url at a node") || strings.Contains(err.Error(), "-api") {
			t.Errorf("%v of a node serving no code reads = %v, want it to name -url and not -api", args, err)
		}
	}
}

// A CODE READ PAYS ONE BARRIER, AND EVERY PAGE AFTER IT READS AT ITS FLOOR.
// Against a real node, paged a row at a time as a sweep of a large company
// is: the listing's first page is read at the head, and every later page —
// the listing's, a history's and every verify of the sweep — at stale with
// the first page's position as its floor, which the node serves with no
// barrier, so a listing and a sweep cost the code log one each however many
// pages and repositories they span; and the pages read at or past that floor
// are one monotone history. A history asks for no more pushes than are still
// wanted.
//
// Mutation: read every page at the first page's level, and the later pages
// ask for the head.
func TestACodeReadPaysOneBarrier(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n    - name: docs\n      owner: {seat: founder}\n"+
		"    - name: api\n      owner: {seat: founder}\n")
	for i := range 3 {
		h.push(t, "web", map[string]string{fmt.Sprintf("f%d.txt", i): "x\n"}, fmt.Sprintf("HEAD:refs/heads/b%d", i))
	}
	rec := &recordedRequests{inner: httpxtest.Pool(t).Transport}
	client := (&nodeClient{base: h.base, token: h.token, http: &http.Client{Transport: rec}}).forCodeReads()

	var names []string
	first, last, err := codePages(t.Context(), client, "/code/repos", url.Values{"read_level": {"linearizable"}},
		0, 1, func(_ json.RawMessage, page codeReadView) bool {
			for _, r := range page.Repos {
				names = append(names, r.Name)
			}
			return true
		})
	if err != nil || len(names) != 3 {
		t.Fatalf("a listing a row a page = %v, %v; want the three repositories", names, err)
	}
	if before, err := (statelog.Position{Stream: last.Stream, Generation: last.Generation, Seq: last.Seq}).Before(
		statelog.Position{Stream: first.Stream, Generation: first.Generation, Seq: first.Seq}); err != nil || before {
		t.Errorf("the last page was read at %s, before the first's %s", last, first)
	}
	var pushes int
	if _, _, err := codePages(t.Context(), client, "/code/repos/web/pushes", floorAt(first), 2,
		code.MaxPushesPerPage, func(_ json.RawMessage, page codeReadView) bool {
			pushes += len(page.Pushes)
			return true
		}); err != nil || pushes != 2 {
		t.Fatalf("two pushes of web's history = %d, %v; want two", pushes, err)
	}
	targets, floor, err := codeVerifyTargets(t.Context(), client, nil)
	if err != nil || len(targets) != 3 {
		t.Fatalf("the sweep's targets = %v, %v", targets, err)
	}
	for _, name := range targets {
		if _, err := verifyOne(t.Context(), client, name, floor, false, false, productionVerifyWaits(), io.Discard,
			io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	reads := rec.requests()
	linearizable := 0
	for i, r := range reads {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pushes"):
			if q.Get("limit") != "2" {
				t.Errorf("a history wanting two pushes asked for %q", q.Get("limit"))
			}
			fallthrough
		case r.Method == http.MethodGet && q.Get("read_level") == "stale":
			if q.Get("min_position") == "" {
				t.Errorf("request %d (%s) read stale with no floor", i, r.URL)
			}
		case r.Method == http.MethodGet && q.Get("read_level") == "linearizable":
			linearizable++
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/verify/"):
			// A check that outlasted the request that began it, read again.
		case r.Method == http.MethodPost:
			if q.Get("min_position") != floor.String() {
				t.Errorf("a verify (%s) asked at %q, want the sweep's floor %s", r.URL, q.Get("min_position"), floor)
			}
		default:
			t.Errorf("request %d (%s %s) is no read this case makes", i, r.Method, r.URL)
		}
	}
	// One for the listing's first page, one for the sweep's.
	if linearizable != 2 {
		t.Errorf("the reads asked for the log's head %d times, want twice — once a listing:\n%v", linearizable,
			reads)
	}
}

// recordedRequests is a transport that keeps every request it carries.
type recordedRequests struct {
	inner http.RoundTripper
	mu    sync.Mutex
	seen  []*http.Request
}

func (r *recordedRequests) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	return r.inner.RoundTrip(req)
}

// requests is every request so far, in the order sent.
func (r *recordedRequests) requests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// ---- export -----------------------------------------------------------------

// AN EXPORT RESTORES AS A CLONE (R9): every ref the node serves and every
// object behind them, in one bundle a person's git clones from — branches, a
// lightweight and an annotated tag — verified before it is put in place, and
// nothing of the export's own left beside it.
func TestAnExportRestoresAsAClone(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	person := codetest.PersonWithToken(t, h.token)
	repo := h.base + "/git/web.git"
	dir := person.Clone(repo)
	person.Commit(dir, map[string]string{"a.txt": "a\n"}, "one")
	person.Commit(dir, map[string]string{"b.txt": "b\n"}, "two")
	person.Git().Run(t, dir, "branch", "dev")
	person.Commit(dir, map[string]string{"c.txt": "c\n"}, "three")
	person.Git().Run(t, dir, "tag", "v1")
	person.Git().Run(t, dir, "tag", "-a", "-m", "the first release", "v1.0.0")
	pushed := person.PushArgs(t.Context(), dir, []string{"--follow-tags"}, repo, "HEAD:refs/heads/main",
		"dev:refs/heads/dev", "refs/tags/v1:refs/tags/v1", "refs/tags/v1.0.0:refs/tags/v1.0.0")
	if !pushed.OK("refs/heads/main") || !pushed.OK("refs/tags/v1.0.0") {
		t.Fatalf("the history did not land:\n%s", pushed)
	}

	outDir := t.TempDir()
	bundle := filepath.Join(outDir, "web.bundle")
	out, errOut, err := cli(t, "code", "export", "web", bundle, h.cfg, "-git", hermeticGit(t))
	if err != nil || !strings.HasPrefix(out, "exported web: 4 refs, ") {
		t.Fatalf("code export = %v\nstdout %q\nstderr %q", err, out, errOut)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 1 {
		t.Errorf("the export left %v beside the bundle, want the bundle alone", entries)
	}

	served := person.LsRemote(repo, 2)
	clone := gittest.NewClient(t)
	restored := filepath.Join(t.TempDir(), "restored")
	clone.Run(t, "", "clone", "--quiet", "--mirror", bundle, restored)
	clone.Run(t, restored, "fsck", "--full", "--strict")
	refs := map[string]string{}
	for line := range strings.Lines(clone.Run(t, restored, "for-each-ref", "--format=%(objectname) %(refname)")) {
		oid, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[name] = oid
	}
	for ref, oid := range served {
		if ref == "HEAD" || strings.HasSuffix(ref, "^{}") {
			continue
		}
		if refs[ref] != oid {
			t.Errorf("the restored %s is %q, the node serves %s", ref, refs[ref], oid)
		}
	}
	if len(refs) != 4 {
		t.Errorf("the restored repository holds %v, want the four refs the node serves", refs)
	}
}

// AN EXPORT OF A REPOSITORY NOBODY DECLARED SAYS SO, in the node's words naming
// code.repos, before any git runs — and leaves nothing at the path.
func TestAnExportOfARepositoryNotDeclaredSaysSo(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	outDir := t.TempDir()
	_, _, err := cli(t, "code", "export", "nope", filepath.Join(outDir, "nope.bundle"), h.cfg,
		"-git", hermeticGit(t))
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "code.repos") {
		t.Errorf("an export of an undeclared repository = %v, want it refused naming it and code.repos", err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("a refused export left %v", entries)
	}
	if _, _, err := cliIn(t, tokenEnviron(h.token), "code", "export", "web", "-url", h.base); err == nil ||
		!strings.Contains(err.Error(), "bundle file") {
		t.Errorf("an export naming no bundle file = %v, want it refused naming what is missing", err)
	}
}

// THE CLI HOLDS AN OPERATOR'S GIT TO THE CLIENT FLOOR — gitproc.ClientMinVersion,
// 2.40 — and never to the node's: the operator's git talks to a git node and
// runs none of its own. A git below the floor is refused as gitproc's own
// *ErrTooOld at the client floor, naming the git, its version, the floor and
// -git, before it is handed any work, and leaves nothing at the path; one AT
// the floor gets past the check, to the clone this stand-in cannot make.
//
// Mutation: compare against gitproc.Version{Major: 2, Minor: 30} in
// atClientFloor, and 2.39.5 is handed the clone — red.
func TestTheCLIRefusesAGitBelowTheClientFloor(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	const body = `echo "the stand-in was asked: $*" >&2; exit 1`

	outDir := t.TempDir()
	old := gittest.Fake(t, "git version 2.39.5", body)
	_, _, err := cli(t, "code", "export", "web", filepath.Join(outDir, "web.bundle"), h.cfg, "-git", old)
	var tooOld *gitproc.ErrTooOld
	if !errors.As(err, &tooOld) || tooOld.Floor != gitproc.FloorClient || tooOld.Path != old {
		t.Fatalf("an export on git 2.39.5 = %v, want a *gitproc.ErrTooOld at the client floor for %s", err, old)
	}
	for _, want := range []string{old, "2.39.5", gitproc.ClientMinVersion.String(), "gitproc.ClientMinVersion",
		"-git"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("a refused export left %v", entries)
	}

	atFloor := gittest.Fake(t, "git version "+gitproc.ClientMinVersion.String(), body)
	_, _, err = cli(t, "code", "export", "web", filepath.Join(outDir, "web.bundle"), h.cfg, "-git", atFloor)
	switch {
	case errors.As(err, &tooOld):
		t.Fatalf("a git at the client floor was refused: %v", err)
	case err == nil || !strings.Contains(err.Error(), "the stand-in was asked: clone"):
		t.Fatalf("an export on git %s = %v, want it past the check, at the clone the stand-in "+
			"cannot make", gitproc.ClientMinVersion, err)
	}
}

// AN EXPORT RUNS UNDER THE OPERATOR'S OWN HARDENING, FROM ANY -git: with
// safe.bareRepository=explicit in their configuration — git refuses every
// bare repository it would discover from a working directory — the mirror is
// named to every git command run on it rather than found, and a -git given
// relative to where the command runs names that git for every command, the
// ones run on the mirror too.
//
// Mutation: run the mirror's commands from its directory, and the hardened
// git refuses them ("cannot use bare repository").
func TestAnExportRunsUnderTheOperatorsHardening(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	h.push(t, "web", map[string]string{"README.md": "# web\n"}, "HEAD:refs/heads/main")
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[safe]\n\tbareRepository = explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hardened, _ := hermeticGitIn(t, global, "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, hardened)
	if err != nil || filepath.IsAbs(relative) {
		t.Fatalf("no relative path to the case's git (%q, %v)", relative, err)
	}
	bundle := filepath.Join(t.TempDir(), "web.bundle")
	out, errOut, err := cli(t, "code", "export", "web", bundle, h.cfg, "-git", relative)
	if err != nil || !strings.HasPrefix(out, "exported web: 1 refs, ") {
		t.Fatalf("an export through a hardened git named relatively = %v\nstdout %q\nstderr %q", err, out, errOut)
	}
}

// AN EXPORT PUTS NOTHING IN PLACE THAT A CLONE WOULD REFUSE. The bundle git
// writes is held to what a clone holds it to before it is renamed into place:
// its pack is read once from start to end and its bytes hashed against its
// trailer, so a pack cut short or with a byte changed — which `git bundle
// verify`, reading the header alone, calls okay — is refused, and nothing is
// left at the path or beside it. And the check itself refuses each way a file
// can fail to be a bundle git restores.
//
// Mutation: put the bundle in place on `git bundle verify`'s word, and the
// export of the damaged bundle succeeds.
func TestAnExportOfADamagedBundleLeavesNothing(t *testing.T) {
	t.Parallel()
	h := codeNode(t, "    - name: web\n      owner: {seat: founder}\n")
	h.push(t, "web", map[string]string{"README.md": "# web\n", "big.bin": strings.Repeat("payload ", 4096)},
		"HEAD:refs/heads/main")
	// A git that damages every bundle it writes, eight bytes into its pack's
	// last object, as a disk could.
	damaging := hermeticGitWith(t, `case " $* " in
*" bundle create "*)
	"$real_git" "$@" || exit $?
	prev=
	for a in "$@"; do
		if [ "$prev" = create ]; then
			size=$(wc -c < "$a")
			printf '\377\377\377\377\377\377\377\377' | dd of="$a" bs=1 seek=$((size - 28)) conv=notrunc 2>/dev/null
		fi
		prev=$a
	done
	exit 0;;
esac`)
	outDir := t.TempDir()
	_, _, err := cli(t, "code", "export", "web", filepath.Join(outDir, "web.bundle"), h.cfg, "-git", damaging)
	if err == nil || !strings.Contains(err.Error(), "not whole") || !strings.Contains(err.Error(), "trailer") {
		t.Errorf("an export of a damaged bundle = %v, want it refused, the pack's hash named", err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("a refused export left %v", entries)
	}

	// THE CHECK, over a real bundle and the ways a file fails to be one.
	src := gittest.NewClient(t)
	repo := src.Init(t)
	src.Run(t, repo, "commit", "--allow-empty", "-q", "-m", "one")
	whole := filepath.Join(t.TempDir(), "whole.bundle")
	src.Run(t, repo, "bundle", "create", whole, "--all")
	if err := checkBundle(t.Context(), whole); err != nil {
		t.Fatalf("a bundle git wrote fails the check: %v", err)
	}
	raw, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	flipped := slices.Clone(raw)
	flipped[len(flipped)-30] ^= 0xff
	header := bytes.Index(raw, []byte("\n\n")) + 2
	for name, damaged := range map[string][]byte{
		"a byte of its pack changed":   flipped,
		"cut short":                    raw[:len(raw)-100],
		"its trailer gone":             raw[:len(raw)-20],
		"no pack after its header":     raw[:header],
		"no header at all":             raw[header:],
		"a prerequisite":               append([]byte("# v2 git bundle\n-"+strings.Repeat("a", 40)+"\n"), raw[16:]...),
		"a header with no ref in it":   append([]byte("# v2 git bundle\n\n"), raw[header:]...),
		"something else in its header": append([]byte("# v2 git bundle\nnot a ref\n\n"), raw[header:]...),
	} {
		path := filepath.Join(t.TempDir(), "damaged.bundle")
		if err := os.WriteFile(path, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkBundle(t.Context(), path); err == nil {
			t.Errorf("a bundle %s passes the check", name)
		}
	}
}

// A BUNDLE'S CHECK STOPS WITH ITS EXPORT. The export catches an interrupt to
// cancel its context rather than die of it, and the bundle it checks is the
// repository's whole size, so the check reads by that context: a bundle git
// wrote passes under a live one, and under one that ended is
// context.Canceled, its pack read no further.
//
// Mutation: hash the pack under a context that never ends — red.
func TestABundlesCheckStopsWithItsExport(t *testing.T) {
	t.Parallel()
	src := gittest.NewClient(t)
	repo := src.Init(t)
	src.Run(t, repo, "commit", "--allow-empty", "-q", "-m", "one")
	whole := filepath.Join(t.TempDir(), "whole.bundle")
	src.Run(t, repo, "bundle", "create", whole, "--all")
	if err := checkBundle(t.Context(), whole); err != nil {
		t.Fatalf("a bundle git wrote fails the check: %v", err)
	}
	interrupted, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkBundle(interrupted, whole); !errors.Is(err, context.Canceled) {
		t.Errorf("the check of an interrupted export: %v, want context.Canceled", err)
	}
}

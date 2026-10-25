package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
)

// builderCompany is a company as the dashboard's builder creates one: its
// charter, the operator's own human seat and the agent seat it manages, and
// no providers.llm, which no dashboard screen writes.
const builderCompany = `
name: Acme
mission: Ship things
roles:
  - name: Founder
    handle: founder
    kind: human
    manages: [ceo]
  - name: Chief Executive
    handle: ceo
    goal: Run the company
`

// THE STEPS THE DASHBOARD GIVES FOR A FIRST MODEL RUN ON THE COMPANY IT MADE.
//
// The builder creates a company with PUT /config, so it is in the store and in
// no file, and the card it shows next hands the operator the terminal steps
// that add a model. Those steps are this binary's commands, and they started
// by editing a company.yaml a dashboard-created company never had: `crewlet
// llm` read the entry from a file that was not there, and `crewlet config
// import` was left to store whatever file was over the company just built.
//
// So the steps are read out of the dashboard (PROVIDER_STEPS) and walked here,
// in order, from the state a create leaves — crewlet.yaml in the working
// directory, the store holding the company, no company file beside it, and a
// node that HAS RUN, since the company was created through its API — stopped,
// as the steps say to leave it:
//
//   - a `crewlet config` command RUNS, and a `> FILE` after it is written;
//   - a `crewlet llm` command loads what it loads before the vendor's CLI
//     starts — the company file, the entry it names, and the company's stored
//     values through the store its run reaches — because the sign-in and the
//     smoke test are the vendor's, and no test calls a vendor. A
//     `-capture-token` login stores what the command stores once the vendor
//     has minted, a fixture token in the vendor's place;
//   - `crewlet run` starts the node on the revision the import marked active,
//     which the walk reads off the store as it starts, and from then on the
//     store is held, as the engine holds it, and the node's API serves what
//     the stopped node's own rows held, as the engine's start carries them
//     onto the fleet's store ([fleetsecrets.Migrate]);
//   - the lines under a step that are neither a command nor a comment are the
//     YAML it adds, and they go into the file an earlier step wrote.
//
// Anything else fails: another command, a block with no file to go in, or a
// step the binary refuses where the steps run it, is a step nothing here has
// run. On a stopped node that has run, its own rows are not the company's
// whole secret store, so every command that reads the company's values is
// refused there; the check the steps end on is made through the running node,
// and has to read the token the login stored. And the guide that explains the
// steps shows them as they are.
//
// NOT PARALLEL: the steps name their files relative to where they are typed,
// so the walk moves into that directory, and a working directory is the
// process's.
func TestTheFirstModelStepsRunOnACompanyOnlyTheStoreHolds(t *testing.T) {
	steps, err := clientsource.Scalar(clientsource.Tree(t), "PROVIDER_STEPS")
	if err != nil {
		t.Fatal(err)
	}
	// THE GUIDE SHOWS THE STEPS THE PANEL DOES, so what this walk runs is
	// what a reader of either is told: a copy edited in one place only is
	// a page sending operators down steps nothing has run.
	guide, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "org-builder.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), "```bash\n"+steps+"\n```\n") {
		t.Errorf("docs/guides/org-builder.md does not show PROVIDER_STEPS as they are, " +
			"in one bash block: give its \"Giving the company a model\" the steps verbatim")
	}

	// THE NODE THE STEPS START AGAIN: its API, at the address its Tier A
	// gives, holding nothing until `crewlet run` and then what the stopped
	// node's own rows held.
	node := newRevealNode(t, map[string]string{})
	dir := t.TempDir()
	tierA := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "node.db")) + fixtureStream(dir) +
		servingAPI(node.server.Listener.Addr().(*net.TCPAddr).Port) + fixtureKeyring
	if err := os.WriteFile(filepath.Join(dir, defaultBootstrapPath), []byte(tierA), 0o600); err != nil {
		t.Fatal(err)
	}
	// THE BUILDER'S NODE HAS RUN: the company was made through its API, so
	// its broker has started on its store directory, and the fleet's secret
	// store is that broker's, which only a running engine serves.
	markBrokerRun(t, filepath.Join(dir, defaultBootstrapPath))
	// THE COMPANY GOES IN FROM ELSEWHERE, so the store holds it and the
	// directory the steps run in holds no company file — what a create
	// through the API leaves behind.
	created := filepath.Join(t.TempDir(), "created.yaml")
	if err := os.WriteFile(created, []byte(builderCompany), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, err := configCmd(t, filepath.Join(dir, defaultBootstrapPath),
		"import", created); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Chdir(dir)
	if _, err := os.Stat(defaultCompanyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the walk must start with no %s beside %s: %v",
			defaultCompanyPath, defaultBootstrapPath, err)
	}

	w := stepWalk{t: t, node: node, captured: map[string]capturedToken{}}
	for line := range strings.SplitSeq(steps, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			w.addBlock()
		case strings.HasPrefix(trimmed, "crewlet "):
			w.addBlock()
			w.command(trimmed)
		case line == trimmed && !strings.HasSuffix(strings.Fields(trimmed)[0], ":"):
			// AT THE MARGIN AND NOT A KEY, so a command: the YAML a step adds
			// is a top-level key and what is indented under it. Read as YAML,
			// a `curl` line would fail the walk for the wrong reason.
			t.Fatalf("the steps run %q, which is not this binary's: nothing here "+
				"can say whether it works on a company only the store holds", trimmed)
		default:
			w.block = append(w.block, line)
		}
	}
	w.addBlock()

	// WHAT THE NODE STARTS ON: the company the builder made, every seat of
	// it, with each model a step signed in. A step that imported some other
	// file would keep the model and lose the company.
	if len(w.signedIn) == 0 {
		t.Fatal("no step signs a model in, so the steps add nothing an agent can run on")
	}
	if w.started == nil {
		t.Fatal("no step starts the node again, so nothing runs on the model the steps add")
	}
	active := w.started
	if active.Name != "Acme" {
		t.Errorf("the active company is %q, not the one the builder made", active.Name)
	}
	o, err := active.Organization()
	if err != nil {
		t.Fatal(err)
	}
	var seats []string
	for role := range o.AllRoles() {
		seats = append(seats, role.Handle())
	}
	slices.Sort(seats)
	if want := []string{"ceo", "founder"}; !slices.Equal(seats, want) {
		t.Errorf("the active company's seats are %v, want the builder's %v", seats, want)
	}
	for _, key := range w.signedIn {
		if _, ok := active.Providers.LLM[key]; !ok {
			t.Errorf("a step signs %q in, and the company the node starts on has no such "+
				"providers.llm entry: the steps never imported it", key)
		}
		// THE CHECK IS OF WHAT THE ENGINE RUNS ON: the entry as the running
		// node resolves it, the stored sign-in included. A check made
		// anywhere else, or never, leaves the operator to find a broken
		// sign-in on the first turn.
		if !slices.Contains(w.checked, key) {
			t.Errorf("the steps sign %q in and never check it with `crewlet llm doctor` "+
				"through the running node", key)
		}
	}
}

// stepWalk is the state of one walk through the steps: the file the last
// redirected step wrote, the YAML waiting to go into it, every model a step
// signed in, the token each `-capture-token` stored, every model a step
// checked through the running node — and the node: whether a step has
// started it, and the company it started on.
type stepWalk struct {
	t        *testing.T
	written  string
	block    []string
	signedIn []string
	captured map[string]capturedToken
	checked  []string

	node    *revealNode
	running bool
	started *config.Company
}

// capturedToken is the headless token a `-capture-token` login stored for one
// entry, and the variable it stored it under.
type capturedToken struct {
	name, value string
}

// addBlock adds the YAML lines read since the last step to the file a step
// wrote, as an operator pasting them at its end would.
func (w *stepWalk) addBlock() {
	w.t.Helper()
	if len(w.block) == 0 {
		return
	}
	if w.written == "" {
		w.t.Fatalf("the steps add\n%s\nto a file before any step has written one",
			strings.Join(w.block, "\n"))
	}
	f, err := os.OpenFile(w.written, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = io.WriteString(f, strings.Join(w.block, "\n")+"\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		w.t.Fatal(err)
	}
	w.block = nil
}

// command walks one `crewlet ...` line.
func (w *stepWalk) command(line string) {
	w.t.Helper()
	args := strings.Fields(line)[1:]
	target := ""
	if i := slices.Index(args, ">"); i >= 0 {
		if i != len(args)-2 {
			w.t.Fatalf("%q: a step's output goes to one file, named last", line)
		}
		target, args = args[i+1], args[:i]
	}
	if len(args) == 0 {
		w.t.Fatalf("%q names no command", line)
	}
	var stdout bytes.Buffer
	switch args[0] {
	case "config":
		var stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil {
			w.t.Fatalf("`%s` fails on a company only the store holds: %v\n%s",
				line, err, stderr.String())
		}
	case "llm":
		w.llm(line, args[1:])
	case "run":
		w.start(line)
	default:
		w.t.Fatalf("the steps run `crewlet %s`, which this walk does not model: "+
			"say here what it reads and writes, or it is a step nothing has run", args[0])
	}
	if target != "" {
		if err := os.WriteFile(target, stdout.Bytes(), 0o600); err != nil {
			w.t.Fatal(err)
		}
		w.written = target
	}
}

// start walks `crewlet run`: the node starts on the revision the import marked
// active, read off the store before the node takes it; holds its store for the
// rest of the walk, as the engine does while it runs; and carries this node's
// own secret rows onto the fleet's store, which its API then serves — the
// engine's start migrates them ([fleetsecrets.Migrate]), and on a node whose
// fleet holds nothing yet every row it carries is the later write.
func (w *stepWalk) start(line string) {
	w.t.Helper()
	if w.running {
		w.t.Fatalf("%q starts a node the steps already started", line)
	}
	cs, closeStore, err := openConfigStore(w.t.Context(), defaultBootstrapPath, "")
	if err != nil {
		w.t.Fatalf("`%s` starts on the store at %s, and it cannot be read: %v",
			line, defaultBootstrapPath, err)
	}
	_, active, err := storedCompany(w.t.Context(), cs, "")
	closeStore()
	if err != nil {
		w.t.Fatalf("`%s` starts on no active company: %v", line, err)
	}
	w.started = active
	rows := rowsOf(w.t, defaultBootstrapPath)
	holdTheStore(w.t, storePathOf(w.t, defaultBootstrapPath), store.EstateNode)
	w.node.hold(rows)
	w.running = true
}

// llm holds a `crewlet llm SUB KEY ...` step to what the command loads before
// it starts the vendor's CLI ([runLLM]): the company file it names or
// defaults to, held to the runnable rules, carrying the entry it names, the
// company's stored values the entry reads, through the store the run reaches —
// this node's own rows while the node is stopped, the running node's API once
// it runs — and that entry built, the one entry the command resolves and
// builds. Before `crewlet run` the step has no credential for a node, as the
// stopped node serves none; after it, the operator's Tier A token.
func (w *stepWalk) llm(line string, args []string) {
	w.t.Helper()
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		w.t.Fatalf("%q names no providers.llm entry, and the steps are for one", line)
	}
	sub, key := args[0], args[1]
	companyPath := defaultCompanyPath
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "-company="); ok {
			companyPath = value
		} else if arg == "-company" && i+1 < len(args) {
			companyPath = args[i+1]
		}
	}
	environ := noEnviron
	if w.running {
		environ = tokenEnviron(cliFixtureToken)
	}
	listed := w.node.listed()
	bootstrapPath, apiURL := defaultBootstrapPath, ""
	use := readsTheCompany
	if slices.Contains(llmActsOnItsOwn, sub) {
		use = actsOnItsOwn
	}
	companySecrets, err := openCompanyStore(environ,
		storeFlags{bootstrap: &bootstrapPath, api: &apiURL}, "crewlet llm "+sub, use)
	if err != nil {
		w.t.Fatalf("`%s` reads Tier A from %s, and cannot: %v", line, bootstrapPath, err)
	}
	defer companySecrets.Close()
	company, keys, resolver, err := loadLLMConfig(w.t.Context(), companyPath, companySecrets,
		sub, key, io.Discard)
	switch {
	case errors.Is(err, errStoppedNode):
		w.t.Fatalf("`%s` is refused on a stopped node that has run: move it after "+
			"`crewlet run`, where it reads the company's values through the running "+
			"node: %v", line, err)
	case err != nil:
		w.t.Fatalf("`%s` reads the company from %s, and cannot: %v",
			line, companyPath, err)
	}
	providers, err := cliAgents(company, keys, resolver)
	if err != nil {
		w.t.Fatalf("`%s`: %v", line, err)
	}
	switch {
	case !w.running && w.node.requests() > 0:
		w.t.Fatalf("`%s` runs while the node is stopped and reached its API, which "+
			"serves nothing until `crewlet run`", line)
	case w.running && w.node.listed() == listed:
		w.t.Fatalf("`%s` runs beside the running node and read nothing through it, "+
			"so it did not read the store the engine runs on", line)
	}
	switch sub {
	case "login":
		if slices.Contains(args, "-capture-token") {
			w.capture(line, key, providers, companySecrets)
		}
		if !slices.Contains(w.signedIn, key) {
			w.signedIn = append(w.signedIn, key)
		}
	case "doctor":
		if !w.running {
			break
		}
		// THE TOKEN THE LOGIN STORED, AS THE RUNNING NODE SERVES IT: the
		// stopped node wrote it into its own rows, and the doctor checks
		// the sign-in the engine runs on only where the node's start has
		// carried it onto the store the engine reads.
		if captured, ok := w.captured[key]; ok {
			if got, _ := resolver.LookupOK(captured.name); got != captured.value {
				w.t.Fatalf("`%s` reads ${%s} as %q through the running node, not the "+
					"token the login stored before the node started", line, captured.name, got)
			}
		}
		w.checked = append(w.checked, key)
	}
}

// capture stores what `crewlet llm login KEY -capture-token` stores once the
// vendor's CLI has minted a token ([loginLLM]): after the same refusal of a
// token nothing would read, a token under the CLI's own token variable,
// through the run's store — a fixture value in place of the vendor's mint.
func (w *stepWalk) capture(line, key string, providers []cliAgentProvider, companySecrets *companyStore) {
	w.t.Helper()
	p, err := oneProvider(providers, key)
	if err != nil {
		w.t.Fatalf("`%s`: %v", line, err)
	}
	if err := refuseUnreadToken(p, key, loginRequest{captureToken: true}); err != nil {
		w.t.Fatalf("`%s` would mint a token nothing reads: %v", line, err)
	}
	token := capturedToken{name: p.Profile().TokenEnv, value: "sk-ant-oat01-walked-" + key}
	if _, err := storeLLMSecret(w.t.Context(), companySecrets, token.name, token.value); err != nil {
		w.t.Fatalf("`%s` cannot store the token it captures: %v", line, err)
	}
	w.captured[key] = token
}

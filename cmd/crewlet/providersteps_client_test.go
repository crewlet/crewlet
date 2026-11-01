package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/sourcetree"
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
// directory, the store holding the company, no company file beside it — with
// no node running, as the steps say to leave it:
//
//   - a `crewlet config` command RUNS, and a `> FILE` after it is written;
//   - a `crewlet llm` command loads what it loads before the vendor's CLI
//     starts — the company file, and the entry it names — because the sign-in
//     and the smoke test are the vendor's, and no test calls a vendor;
//   - `crewlet run` publishes the revision the import marked active, which the
//     end of the walk reads off the store instead;
//   - the lines under a step that are neither a command nor a comment are the
//     YAML it adds, and they go into the file an earlier step wrote.
//
// Anything else fails: another command, or a block with no file to go in, is
// a step nothing here has run. And the guide that explains the steps shows
// them as they are.
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

	dir := t.TempDir()
	tierA := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "node.db")) + fixtureStream(dir) + fixtureKeyring
	if err := os.WriteFile(filepath.Join(dir, defaultBootstrapPath), []byte(tierA), 0o600); err != nil {
		t.Fatal(err)
	}
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

	w := stepWalk{t: t}
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
	cs, closeStore, err := openConfigStore(t.Context(), defaultBootstrapPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	_, active, err := storedCompany(t.Context(), cs, "")
	if err != nil {
		t.Fatal(err)
	}
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
	}
}

// stepWalk is the state of one walk through the steps: the file the last
// redirected step wrote, the YAML waiting to go into it, and every model a
// step signed in.
type stepWalk struct {
	t        *testing.T
	written  string
	block    []string
	signedIn []string
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
		// The node starting again publishes what the import marked active,
		// which the walk reads off the store once every step is done.
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

// llm holds a `crewlet llm SUB KEY ...` step to what the command loads before
// it starts the vendor's CLI ([runLLM]): the company file it names or
// defaults to, held to the runnable rules, carrying the entry it names, and
// that entry built — the one entry the command resolves and builds.
func (w *stepWalk) llm(line string, args []string) {
	w.t.Helper()
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		w.t.Fatalf("%q names no providers.llm entry, and the steps are for one", line)
	}
	key := args[1]
	companyPath := defaultCompanyPath
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "-company="); ok {
			companyPath = value
		} else if arg == "-company" && i+1 < len(args) {
			companyPath = args[i+1]
		}
	}
	bootstrapPath, apiURL := defaultBootstrapPath, ""
	companySecrets, err := openCompanyStore(noEnviron,
		storeFlags{bootstrap: &bootstrapPath, api: &apiURL}, "crewlet llm "+args[0])
	if err != nil {
		w.t.Fatalf("`%s` reads Tier A from %s, and cannot: %v", line, bootstrapPath, err)
	}
	defer companySecrets.Close()
	company, keys, resolver, err := loadLLMConfig(w.t.Context(), companyPath, companySecrets,
		args[0], key, io.Discard)
	if err != nil {
		w.t.Fatalf("`%s` reads the company from %s, and cannot: %v",
			line, companyPath, err)
	}
	if _, err := cliAgents(company, keys, resolver); err != nil {
		w.t.Fatalf("`%s`: %v", line, err)
	}
	if !slices.Contains(w.signedIn, key) {
		w.signedIn = append(w.signedIn, key)
	}
}

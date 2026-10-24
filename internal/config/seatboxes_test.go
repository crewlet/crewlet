package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// setupFileDoc is a company whose catalogue is catalogue, whose engine-wide
// setup is shared and whose seats are roles — every block already indented
// for its place, so a case reads as the document it is.
func setupFileDoc(catalogue, shared, llm, roles string) string {
	doc := "name: Acme\nproviders:\n  sandbox:\n" + catalogue
	if shared != "" {
		doc += "    setup:\n" + shared
	}
	if llm != "" {
		doc += "  llm:\n" + llm
	}
	return doc + "roles:\n" + roles
}

// One step writing one file, indented for a list under `setup:` at depth.
func fileStep(indent, name, path string) string {
	return indent + "- name: " + name + "\n" +
		indent + "  files:\n" +
		indent + "    " + path + ": \"x\"\n"
}

const (
	directOnly    = "    local: {}\n    default_run_in: direct\n"
	directAndImg  = "    local: {image: img}\n    default_run_in: direct\n"
	e2bOnly       = "    e2b: {api_key: k}\n"
	directAndE2B  = "    local: {}\n    e2b: {api_key: k}\n    default_run_in: direct\n"
	agentModeLLM  = "    sub:\n      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent}\n"
	agentModeE2B  = "    sub:\n      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent, run_in: e2b}\n"
	plainSeat     = "  - name: SWE\n    sandbox: {enabled: true}\n"
	agentSeat     = "  - name: SWE\n    llm: sub\n"
	seatStepsHead = "  - name: SWE\n    sandbox:\n      enabled: true\n"
)

// seatWith is a seat whose gate is gate (indented under `sandbox:`) and whose
// own setup is steps.
func seatWith(head, steps string) string {
	return head + "      setup:\n" + steps
}

// A SETUP FILE A LOCAL BOX WOULD REFUSE IS REFUSED ON THE WRITE, at the file
// in the step that writes it, naming whose box it fails and the remedy.
//
// It used to validate cleanly and fail every box the seat opened, at setup:
// the reference example's engine-wide git-auth step wrote /usr/local/bin while
// every engineering seat ran `direct`. Each case is one cell the step reaches,
// and the controls are the paths the same cell takes, so a rule that refused
// too much fails here as surely as one that refused too little.
func TestASetupFileALocalBoxRefusesIsRefusedOnTheWrite(t *testing.T) {
	t.Parallel()
	const shared = "providers.sandbox.setup[0].files."
	const own = "roles[0].sandbox.setup[0].files."
	cases := []struct {
		name string
		doc  string
		// path is where the refusal is placed; empty means the document
		// is valid.
		path string
		says []string
	}{
		{
			name: "an engine-wide system path reaching a direct seat",
			doc:  setupFileDoc(directOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "", plainSeat),
			path: shared + "/usr/local/bin/helper",
			says: []string{`setup step "git-auth"`, "/usr/local/bin/helper", "direct box", `Seat "swe"`,
				"Name the file relative to the box's home", `"$HOME"`},
		},
		{
			name: "a seat's own system path in a direct box",
			doc:  setupFileDoc(directOnly, "", "", seatWith(seatStepsHead, fileStep("        ", "own", "/usr/local/bin/helper"))),
			path: own + "/usr/local/bin/helper",
			says: []string{`setup step "own"`, "mints per box"},
		},
		{
			name: "the container's own home is not a direct box's",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", "/home/user/.npmrc"), "", plainSeat),
			path: shared + "/home/user/.npmrc",
		},
		{
			name: "a relative path in a direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", ".npmrc"), "", plainSeat),
		},
		{
			// A tilde is the home in every cell, as an E2B box's own agent
			// reads one.
			name: "a tilde path in a direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", `"~/.npmrc"`), "", plainSeat),
		},
		{
			name: "another account's home in a direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", `"~bob/.npmrc"`), "", plainSeat),
			path: shared + "~bob/.npmrc",
			says: []string{"another account's home"},
		},
		{
			name: "the tilde alone, which is the home itself",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", `"~"`), "", plainSeat),
			path: shared + "~",
			says: []string{"names the home itself"},
		},
		{
			name: "a relative path climbing out of a direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", "../escape"), "", plainSeat),
			path: shared + "../escape",
			says: []string{"climbs out"},
		},
		{
			name: "a path under the home mount of a container box",
			doc: setupFileDoc(directAndImg, fileStep("      ", "rc", "/home/user/.npmrc"), "",
				"  - name: SWE\n    sandbox: {enabled: true, run_in: container}\n"),
		},
		{
			name: "a system path in a container box",
			doc: setupFileDoc(directAndImg, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "",
				"  - name: SWE\n    sandbox: {enabled: true, run_in: container}\n"),
			path: shared + "/usr/local/bin/helper",
			says: []string{"container box", "/home/user"},
		},
		{
			name: "a climb off the home mount of a container box",
			doc: setupFileDoc(directAndImg, fileStep("      ", "rc", "/home/user/../../etc/cron.d/x"), "",
				"  - name: SWE\n    sandbox: {enabled: true, run_in: container}\n"),
			path: shared + "/home/user/../../etc/cron.d/x",
		},
		{
			// The box's own agent writes a file outside its home, so a
			// system path is refused there by nothing.
			name: "a system path in an e2b box",
			doc:  setupFileDoc(e2bOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "", plainSeat),
		},
		{
			name: "an engine-wide system path no local box reaches",
			doc: setupFileDoc(directAndE2B, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "",
				"  - name: SWE\n    sandbox: {enabled: true, run_in: e2b}\n"),
		},
		{
			// An agent-mode seat's gate offers it no box: the executor's,
			// in e2b here, is the one its steps reach, and not the direct
			// one the catalogue's default would give a run_sandbox box.
			name: "an agent-mode seat's gate falls to a direct default",
			doc: setupFileDoc(directAndE2B, "", agentModeE2B,
				"  - name: SWE\n    llm: sub\n    sandbox:\n      enabled: true\n      setup:\n"+
					fileStep("        ", "own", "/usr/local/bin/helper")),
		},
		{
			// No gate at all: the executor's own box is placed by its
			// entry, and given the engine-wide steps.
			name: "an agent-mode executor's direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), agentModeLLM, agentSeat),
			path: shared + "/usr/local/bin/helper",
			says: []string{"runs its executor in one", "providers.llm.sub"},
		},
		{
			// `self` is no box of its own, so the seat's step reaches only
			// the executor's — in the cell its entry names.
			name: "a self seat's step in its executor's direct box",
			doc: setupFileDoc(directOnly, "", agentModeLLM,
				"  - name: SWE\n    llm: sub\n    sandbox:\n      enabled: true\n      run_in: self\n      setup:\n"+
					fileStep("        ", "own", "/usr/local/bin/helper")),
			path: own + "/usr/local/bin/helper",
			says: []string{"runs its executor in one"},
		},
		{
			name: "an agent-mode executor placed in e2b",
			doc:  setupFileDoc(directAndE2B, fileStep("      ", "git-auth", "/usr/local/bin/helper"), agentModeE2B, agentSeat),
		},
		{
			name: "the double writes nothing anywhere",
			doc:  setupFileDoc("    fake: true\n", fileStep("      ", "git-auth", "/usr/local/bin/helper"), "", plainSeat),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.path == "" {
				mustCompany(t, tc.doc)
				return
			}
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want ErrConflict, got %v", err)
			}
			for _, said := range tc.says {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("the refusal does not say %q:\n%v", said, err)
				}
			}
		})
	}
}

// AN E2B BOX TAKES ALMOST ANY PATH, AND THE FEW IT REFUSES ARE REFUSED ON THE
// WRITE TOO.
//
// Its own agent expands a leading "~", joins a relative path onto the home and
// creates the file wherever that lands, an absolute path outside the home and
// a climb out of it included. It will not expand another account's "~name",
// and it answers a path naming a directory "path is a directory" — of which
// the home and every directory above it are certain in every template. Each
// of those used to validate and then fail every box the seat opened, at setup.
// The controls are paths that lie beside the refused ones, so a rule that
// compared strings rather than path segments fails here too.
func TestASetupFileAnE2BBoxRefusesIsRefusedOnTheWrite(t *testing.T) {
	t.Parallel()
	const shared = "providers.sandbox.setup[0].files."
	refused := []struct {
		file string
		says []string
	}{
		{"~bob/.npmrc", []string{"which an e2b box refuses", "another account's home", "will not expand"}},
		{"~", []string{"e2b box", "/home/user", "a directory"}},
		{"~/", nil},
		{".", nil},
		{"..", nil},
		{"/home/user", nil},
		{"/home/user/", nil},
		{"/home/user/.local/..", nil},
		{"/home", nil},
		{"/", nil},
		// The home through `~/..`, which only expanding the tilde BEFORE
		// cleaning refuses: cleaned first, it is `user`, a file under it.
		{"~/../user", nil},
	}
	for _, tc := range refused {
		t.Run("refused "+tc.file, func(t *testing.T) {
			t.Parallel()
			doc := setupFileDoc(e2bOnly, fileStep("      ", "rc", `"`+tc.file+`"`), "", plainSeat)
			err := rejects(t, doc, shared+tc.file)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want ErrConflict, got %v", err)
			}
			for _, said := range append(tc.says, `setup step "rc"`, `Seat "swe"`,
				"Name the file relative to the box's home") {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("the refusal does not say %q:\n%v", said, err)
				}
			}
		})
	}
	// A trailing slash or a "." segment names the same file in every cell:
	// the box writes a file's cleaned path, as both local cells do. And a
	// `~/..` climbs from the home the tilde names, as the box's own agent
	// expands it, so `~/../user/.npmrc` is `/home/user/.npmrc` and taken —
	// a key both orders admit, which is why the refused `~/../user` above
	// is the case that holds the order.
	for _, file := range []string{
		".npmrc", "~/.npmrc", "/home/user/.npmrc", "/usr/local/bin/helper",
		"../shared/x", "/home/username/x", "/home/use",
		"/home/user/.npmrc/", "/home/user/./.npmrc", "~/../user/.npmrc",
	} {
		t.Run("taken "+file, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, setupFileDoc(e2bOnly, fileStep("      ", "rc", `"`+file+`"`), "", plainSeat))
		})
	}
}

// AN ENGINE-WIDE STEP IS ONE LINE TO CHANGE, so it is one problem, however
// many seats it fails: a problem per seat buried that line under every seat
// that inherits it. The first seat is named and the rest are counted.
func TestARefusedEngineWideFileIsOneProblemNamingItsSeats(t *testing.T) {
	t.Parallel()
	doc := setupFileDoc(directOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "",
		"  - name: SWE\n    sandbox: {enabled: true}\n"+
			"  - name: QA\n    sandbox: {enabled: true}\n"+
			"  - name: Ops\n    sandbox: {enabled: true}\n")
	_, err := ParseCompany([]byte(doc))
	problems := Problems(err)
	if len(problems) != 1 {
		t.Fatalf("want one problem for one file, got %d:\n%v", len(problems), err)
	}
	if msg := problems[0].Message; !strings.Contains(msg, `Seat "swe"`) || !strings.Contains(msg, "2 more seats") {
		t.Errorf("the problem does not name the first seat and count the rest:\n%s", msg)
	}
}

// THE RULE IS AN ADMISSION RULE: the company runs, and only the boxes the step
// reaches fail, so a stored revision keeping such a step is applied and
// warned about rather than refused — an apply that refused it would take a
// node off the fleet's epoch over a revision another build admitted.
func TestARefusedSetupFileIsAnAdmissionRule(t *testing.T) {
	t.Parallel()
	assertAdmissionOnly(t,
		setupFileDoc(directOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "", plainSeat),
		"providers.sandbox.setup[0].files./usr/local/bin/helper")
}

// A SEAT'S BOX IS PLACED AS THE ENGINE PLACES IT: the run_sandbox box its gate
// offers — in the gate's cell, else the catalogue's default, which is how the
// manager resolves an empty one at launch — or, for a seat whose executor runs
// in agent mode, the box that executor runs in, placed by its entry, and no
// other: such a seat is never offered run_sandbox. Each is given the
// engine-wide steps and then the seat's own, as the engine's runEnvironment
// builds them.
func TestSeatBoxesArePlacedAsTheEngineLaunchesThem(t *testing.T) {
	t.Parallel()
	type box struct {
		seat, executor string
		cell           Placement
		steps          []string
	}
	cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n"+
		"    local: {image: img}\n    e2b: {api_key: k}\n    default_run_in: direct\n"+
		"    setup:\n      - {name: shared, brief: b}\n"+
		"  llm:\n    main:\n      type: anthropic\n      model: claude-golden\n"+
		"    sub:\n      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent, run_in: e2b}\n"+
		"    deflt:\n      type: cli-agent\n      model: sonnet\n      cli: {agent: claude-code, mode: agent}\n"+
		"roles:\n"+
		// The gate's own cell.
		"  - name: Named\n    llm: main\n    sandbox:\n      enabled: true\n      run_in: container\n"+
		"      setup:\n        - {name: mine, brief: b}\n"+
		// The catalogue's default.
		"  - name: Silent\n    llm: main\n    sandbox: {enabled: true}\n"+
		// No box at all: a gate that is off, on a native executor.
		"  - name: Off\n    llm: main\n    sandbox: {enabled: false}\n"+
		// `self` is no box of its own; the executor's entry places it.
		"  - name: Self\n    llm: sub\n    sandbox:\n      enabled: true\n      run_in: self\n"+
		"      setup:\n        - {name: own, brief: b}\n"+
		// An agent-mode seat's enabled gate offers it nothing: its one box
		// is the executor's, in e2b, not a run_sandbox box in the
		// catalogue's default — though the box is given the gate's steps.
		"  - name: Agent\n    llm: sub\n    sandbox:\n      enabled: true\n"+
		"      setup:\n        - {name: kept, brief: b}\n"+
		// An agent-mode seat with no gate at all still runs in its
		// executor's box, and an entry that names no cell takes the
		// catalogue's default.
		"  - name: Bare\n    llm: deflt\n")

	var got []box
	for b := range cfg.SeatBoxes() {
		var steps []string
		for _, s := range b.Steps {
			steps = append(steps, s.Step.Name+"@"+s.At.String())
		}
		got = append(got, box{b.Seat.Name, b.Executor, b.Cell, steps})
	}
	shared := "shared@providers.sandbox.setup[0]"
	want := []box{
		{"Named", "", PlacementContainer, []string{shared, "mine@roles[0].sandbox.setup[0]"}},
		{"Silent", "", PlacementDirect, []string{shared}},
		{"Self", "sub", PlacementE2B, []string{shared, "own@roles[3].sandbox.setup[0]"}},
		{"Agent", "sub", PlacementE2B, []string{shared, "kept@roles[4].sandbox.setup[0]"}},
		{"Bare", "deflt", PlacementDirect, []string{shared}},
	}
	if !slices.EqualFunc(got, want, func(a, b box) bool {
		return a.seat == b.seat && a.executor == b.executor && a.cell == b.cell && slices.Equal(a.steps, b.steps)
	}) {
		t.Errorf("SeatBoxes placed\n  %+v\nwant\n  %+v", got, want)
	}
}

// NO CATALOGUE, NO BOX, and a human seat opens none either: it runs no turn.
//
// Read without validation, because a person's sandbox block is refused on
// its own and the answer must not depend on that refusal having run first.
func TestSeatBoxesAreNoneWithoutACatalogueOrForAPerson(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"no catalogue": "name: Acme\nroles:\n  - name: SWE\n    sandbox: {enabled: true}\n",
		"a person": "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n" +
			"roles:\n  - name: Founder\n    kind: human\n    sandbox: {enabled: true}\n",
	} {
		cfg, err := ParseCompanyDocument([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for b := range cfg.SeatBoxes() {
			t.Errorf("%s: %q got a box in %s", name, b.Seat.Name, b.Cell)
		}
	}
}

package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// setupFileDoc is a company whose catalogue is catalogue, whose engine-wide
// setup is shared, whose providers.llm entries are llm (one claude-code entry
// in the catalogue's default cell when empty) and whose seats are roles —
// every block already indented for its place, so a case reads as the
// document it is.
func setupFileDoc(catalogue, shared, llm, roles string) string {
	doc := "name: Acme\nproviders:\n  sandbox:\n" + catalogue
	if shared != "" {
		doc += "    setup:\n" + shared
	}
	if llm == "" {
		llm = defaultLLM
	}
	return doc + "  llm:\n" + llm + "roles:\n" + roles
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
	defaultLLM    = "    main:\n      model: sonnet\n"
	containerLLM  = "    main:\n      model: sonnet\n      run_in: container\n"
	plainSeat     = "  - name: SWE\n"
	seatStepsHead = "  - name: SWE\n    sandbox:\n"
)

// seatWith is a seat whose gate is gate (indented under `sandbox:`) and whose
// own setup is steps.
func seatWith(head, steps string) string {
	return head + "      setup:\n" + steps
}

// A SETUP FILE A BOX WOULD REFUSE IS REFUSED ON THE WRITE, at the file in the
// step that writes it, naming whose box it fails and the remedy.
//
// It used to validate cleanly and fail every box the seat opened, at setup:
// the reference example's engine-wide git-auth step wrote /usr/local/bin while
// every engineering seat ran `direct`. Each case is one cell the step reaches,
// and the controls are the paths the same cell takes, so a rule that refused
// too much fails here as surely as one that refused too little.
func TestASetupFileABoxRefusesIsRefusedOnTheWrite(t *testing.T) {
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
				"runs its executor in one (providers.llm.main)",
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
			// A tilde is the home in every cell.
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
			// No shell reads ~\ as the home: to a POSIX host the backslash
			// is part of a file name, so the file would be named "\.npmrc".
			name: "a backslash after the tilde in a direct box",
			doc:  setupFileDoc(directOnly, fileStep("      ", "rc", `'~\.npmrc'`), "", plainSeat),
			path: shared + `~\.npmrc`,
			says: []string{"only before a /", "backslash is a character of a file name", "~/.npmrc"},
		},
		{
			name: "a backslash after the tilde in a container box",
			doc:  setupFileDoc(directAndImg, fileStep("      ", "rc", `'~\.npmrc'`), containerLLM, plainSeat),
			path: shared + `~\.npmrc`,
			says: []string{"container box", "backslash is a character of a file name"},
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
			// The entry places the box, not the catalogue's default: the
			// default's direct box would refuse this path, and a seat whose
			// executor runs in a container opens none for it to fail in.
			name: "a path under the home mount of a container box",
			doc:  setupFileDoc(directAndImg, fileStep("      ", "rc", "/home/user/.npmrc"), containerLLM, plainSeat),
		},
		{
			name: "a system path in a container box",
			doc:  setupFileDoc(directAndImg, fileStep("      ", "git-auth", "/usr/local/bin/helper"), containerLLM, plainSeat),
			path: shared + "/usr/local/bin/helper",
			says: []string{"container box", "/home/user"},
		},
		{
			name: "a climb off the home mount of a container box",
			doc:  setupFileDoc(directAndImg, fileStep("      ", "rc", "/home/user/../../etc/cron.d/x"), containerLLM, plainSeat),
			path: shared + "/home/user/../../etc/cron.d/x",
		},
		{
			// A launch falls back to the next member when the first sits
			// out, so the seat's step reaches the fallback member's box too,
			// in the cell that member names.
			name: "a fallback member's container box",
			doc: setupFileDoc(directAndImg, "", defaultLLM+"    boxed:\n      model: sonnet\n      run_in: container\n",
				"  - name: SWE\n    llm: [main, boxed]\n    sandbox:\n      setup:\n"+
					fileStep("        ", "own", "/home/user/../../etc/cron.d/x")),
			path: own + "/home/user/../../etc/cron.d/x",
			says: []string{"container box", "runs its executor in one (providers.llm.boxed)"},
		},
		{
			// A member no launch picks opens no box, so the default cell
			// it would otherwise resolve to judges nothing about it.
			name: "a member a launch skips",
			doc: setupFileDoc(directOnly, "", defaultLLM+customEntry,
				"  - name: SWE\n    llm: [main, house]\n    sandbox:\n      setup:\n"+
					fileStep("        ", "own", ".npmrc")),
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

// AN ENGINE-WIDE STEP IS ONE LINE TO CHANGE, so it is one problem, however
// many seats it fails: a problem per seat buried that line under every seat
// that inherits it. The first seat is named and the rest are counted.
func TestARefusedEngineWideFileIsOneProblemNamingItsSeats(t *testing.T) {
	t.Parallel()
	doc := setupFileDoc(directOnly, fileStep("      ", "git-auth", "/usr/local/bin/helper"), "",
		"  - name: SWE\n  - name: QA\n  - name: Ops\n")
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

// A SEAT'S BOXES ARE PLACED AS THE ENGINE PLACES THEM: one in each cell a
// member of its executor chain that can run it lands in — the member's own
// run_in, else the catalogue's default, which is how the manager resolves an
// empty one at launch — under the first member in that cell, and none for a
// member a launch skips. Each is given the engine-wide steps and then the
// seat's own, as the engine's runEnvironment builds them. A person opens none.
func TestSeatBoxesArePlacedAsTheEngineLaunchesThem(t *testing.T) {
	t.Parallel()
	type box struct {
		seat, executor string
		cell           Placement
		steps          []string
	}
	cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n"+
		"    local: {image: img}\n    default_run_in: direct\n"+
		"    setup:\n      - {name: shared, brief: b}\n"+
		"  llm:\n    main:\n      model: claude-golden\n"+
		"    twin:\n      model: sonnet\n"+
		"    boxedcc:\n      model: sonnet\n      run_in: container\n"+
		"    boxed:\n      model: anthropic/claude-sonnet-5\n      harness: opencode\n      run_in: container\n"+
		customEntry+
		"roles:\n"+
		"  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"+
		// Every cell the chain can land in, each with the seat's steps.
		"  - name: Fallback\n    llm: [main, boxedcc]\n    sandbox:\n"+
		"      setup:\n        - {name: mine, brief: b}\n"+
		// Two members in one cell open one box, under the first.
		"  - name: Twins\n    llm: [main, twin]\n"+
		// A member a launch skips opens nothing, and a seat naming no
		// key runs on the first declared entry.
		"  - name: Mixed\n    llm: [house, boxed]\n"+
		"  - name: Silent\n")

	var got []box
	for b := range cfg.SeatBoxes() {
		var steps []string
		for _, s := range b.Steps {
			steps = append(steps, s.Step.Name+"@"+s.At.String())
		}
		got = append(got, box{b.Seat.Name, b.Executor, b.Cell, steps})
	}
	shared := "shared@providers.sandbox.setup[0]"
	mine := "mine@roles[1].sandbox.setup[0]"
	want := []box{
		{"Fallback", "main", PlacementDirect, []string{shared, mine}},
		{"Fallback", "boxedcc", PlacementContainer, []string{shared, mine}},
		{"Twins", "main", PlacementDirect, []string{shared}},
		{"Mixed", "boxed", PlacementContainer, []string{shared}},
		{"Silent", "main", PlacementDirect, []string{shared}},
	}
	if !slices.EqualFunc(got, want, func(a, b box) bool {
		return a.seat == b.seat && a.executor == b.executor && a.cell == b.cell && slices.Equal(a.steps, b.steps)
	}) {
		t.Errorf("SeatBoxes placed\n  %+v\nwant\n  %+v", got, want)
	}
}

// NO CATALOGUE, NO MODEL, NO BOX — and a human seat opens none either: it
// runs no turn.
//
// Read without validation, because a company with an agent seat and no
// catalogue, and a person's sandbox block, are refused on their own and the
// answer must not depend on that refusal having run first.
func TestSeatBoxesAreNoneWithoutACatalogueOrForAPerson(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"no catalogue": "name: Acme\nproviders:\n  llm:\n    main: {model: sonnet}\n" +
			"roles:\n  - name: SWE\n    sandbox: {max_turns: 5}\n",
		"no model": "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n" +
			"roles:\n  - name: SWE\n    sandbox: {max_turns: 5}\n",
		"a person": "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n" +
			"  llm:\n    main: {model: sonnet}\n" +
			"roles:\n  - name: Founder\n    kind: human\n    sandbox: {max_turns: 5}\n",
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

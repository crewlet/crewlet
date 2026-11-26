package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// THE CHECKOUT CORE hands a run the instructions and skills of each repository
// checked out directly under its working directory, which a harness started
// there does not load as a session started in the checkout would. It names no
// harness: a [checkoutSpec] says what one reads from a checkout's root and
// where the run's copies go, checkout-launch runs it once before the harness
// starts, and a harness's nested adapter (codex-nested, pi-nested) runs it
// after each of the main loop's tool calls and writes what it claimed in its
// harness's shape.
// The package doc's "Checkouts below the working directory" has the contract.

// checkoutSpec is what a harness reads from a checkout's root and where a run
// puts its copies: Instructions, the slots ([instructionSlot]), every slot's
// text in order, within Limit, the harness's own bound on them in bytes (nil
// where it has none); SkillDirs, the skill directories ([skillDirSlot]), an
// earlier one's skill winning a name; Skills, the run's skills directory (""
// copies none), and Copied, the file listing each skill copied there;
// Reserved, the tags no repository takes; ImportInstructions, each instruction
// file named as an import in place of its text ([rootImports]), Limit not
// applying; SkillSep, what joins a copy's tag and skill ([skillName]);
// FiringLimit, a nested firing's bound in bytes on all its checkouts'
// instructions, frames counted, nil for none ([fitFiring]); Gated, the
// harness's own code answering for each checkout ([checkoutGate]) on stdin.
type checkoutSpec struct {
	Instructions       []instructionSlot `json:"instructions"`
	Limit              *int              `json:"limit,omitempty"`
	FiringLimit        *int              `json:"firingLimit,omitempty"`
	SkillDirs          []skillDirSlot    `json:"skillDirs,omitempty"`
	Skills             string            `json:"skills,omitempty"`
	Copied             string            `json:"copied,omitempty"`
	Reserved           []string          `json:"reserved,omitempty"`
	ImportInstructions bool              `json:"importInstructions,omitempty"`
	SkillSep           skillSep          `json:"skillSep,omitempty"`
	Gated              bool              `json:"gated,omitempty"`
	// ListedAs and HomeSkills are the harness whose rule names a listed skill
	// ([listingRules]) and the home's skill directories whose names a copy may
	// not take ([homeHolds]).
	ListedAs   string         `json:"listedAs,omitempty"`
	HomeSkills []skillDirSlot `json:"homeSkills,omitempty"`
	// held is the names the home takes, read once a firing ([homeNames]);
	// gate the firing's [checkoutGate] where Gated, and room what its answer
	// has left ([claimRoom]). None is on the wire.
	held *homeNames
	gate *checkoutGate
	room claimRoom
}

// skillSep is what joins a copy's tag and skill: `:` (the zero value), or `-`
// for a harness that lists `a:b` as `a-b` (Gemini CLI 0.70.0, measured), so a
// home skill's name is compared as the harness lists it.
type skillSep string

// Valid reports whether s is a separator the helper names copies with.
func (s skillSep) Valid() bool { return s == "" || s == ":" || s == "-" }

// String is s as it joins a name.
func (s skillSep) String() string {
	if s == "" {
		return ":"
	}
	return string(s)
}

// launchConfig is what -config names for checkout-launch: the run's checkout
// state directory (the checkouts handed, the lock), the instruction file it
// replaces with the launch's sections, the checkouts handed first, in order
// (the repositories the run pushes to), the bound the sections share with
// the text the harness reads beside them (nil for none), the spec,
// NestedSkillsOnly for a nested adapter that hands skills alone, whose spec
// then names no instruction file to read, and Roots, the file it lists each
// checkout handed's root in, a line each, for a harness that reads their
// instructions itself where the runner names them (Gemini CLI's
// --include-directories). The working directory is the adapter's own.
type launchConfig struct {
	State            string        `json:"state"`
	Agents           string        `json:"agents"`
	First            []string      `json:"first"`
	Budget           *launchBudget `json:"budget,omitempty"`
	NestedSkillsOnly bool          `json:"nestedSkillsOnly,omitempty"`
	Roots            string        `json:"roots,omitempty"`
	checkoutSpec
}

// nestedConfig is what -config names for a nested adapter, written by
// checkout-launch: the state directory, the working directory its checkouts
// sit directly under, and the launch's spec; firing, not on the wire, is the
// firing's own bound where its adapter takes -room, in place of FiringLimit.
type nestedConfig struct {
	State   string `json:"state"`
	Workdir string `json:"workdir"`
	checkoutSpec
	firing *firingBound
}

// maxSkillDescription is the most characters of a skill's description a
// listing carries: the Agent Skills specification's bound on the field.
const maxSkillDescription = 1024

// handedCheckout is a checkout claimed: its name under the working directory,
// its root, its instructions ("" for none), the files they were read from
// (parts, as the core read them; files, as a gate names them), and the skills
// copied for it.
type handedCheckout struct {
	name, root, text string
	parts            []instructionPart
	files            []string
	skills           []handedSkill
}

// instructionPart is one file of a checkout's instructions: its path and the
// text handed of it, for an adapter whose harness wraps each file by its path;
// unread for one a claim left out by its size alone ([fitClaim]).
type instructionPart struct {
	path, text string
	unread     bool
}

// handedSkill is a checkout's skill as the run lists it: its name, its
// description and the path of its copied SKILL.md.
type handedSkill struct {
	name, description, path string
}

// runCheckoutLaunch hands the checkouts under its working directory their
// instructions and skills as the run starts, and writes the state a nested
// adapter reads. Exit 0 once written, 1 where it could not write them.
func runCheckoutLaunch(_ context.Context, f flags, stdin io.Reader, _, stderr io.Writer, _ func(string) string) int {
	cwd, err := os.Getwd()
	if err == nil {
		err = checkoutLaunch(f.config, cwd, stdin, stderr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing the run's repositories their instructions: %v\n", err)
		return exitError
	}
	return exitDone
}

// checkoutLaunch is checkout-launch run in the working directory cwd, a
// gated spec's gate read from stdin.
func checkoutLaunch(config, cwd string, stdin io.Reader, stderr io.Writer) error {
	raw, err := readBounded(config, maxConfig)
	if err != nil {
		return err
	}
	var c launchConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("the launch config: %w", err)
	}
	if c.State == "" || c.Agents == "" || (c.Limit != nil && *c.Limit < 0) ||
		(c.FiringLimit != nil && *c.FiringLimit < 0) {
		return errors.New("the launch config names no state, no instruction file, or a negative limit or firingLimit")
	}
	if !c.SkillSep.Valid() {
		return fmt.Errorf("the launch config's skillSep %q is neither %q nor %q", c.SkillSep, ":", "-")
	}
	if c.Budget != nil && (c.Budget.Form != budgetHermes || c.Budget.Bytes <= 0) {
		return fmt.Errorf("the launch config's budget, form %q of %d bytes, is none the helper knows", c.Budget.Form,
			c.Budget.Bytes)
	}
	if _, ok := listingRules[c.ListedAs]; c.ListedAs != "" && !ok {
		return fmt.Errorf("the launch config's listedAs %q is none of %q, %q and %q", c.ListedAs, listedGrok,
			listedKimi, listedOpenCode)
	}
	if c.Gated {
		if c.gate, err = readGate(stdin); err != nil {
			return err
		}
	}
	work, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	var room *hermesRoom
	if c.Budget != nil {
		if room, err = hermesLaunchRoom(*c.Budget); err != nil {
			return err
		}
		c.room = room
	}
	if err = os.MkdirAll(c.State, 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(c.State, "handed"), nil, 0o600); err != nil {
		return err
	}
	handed, err := handCheckouts(c.State, work, c.First, c.checkoutSpec,
		unboundNames(&firingBound{left: math.MaxInt, count: byteCount, unitBytes: 1}), &skips{stderr: stderr})
	if err != nil {
		return err
	}
	var sections, roots []string
	if room != nil {
		if answer := hermesAnswer(handed, nil, room); answer != "" {
			sections = append(sections, answer)
		}
	}
	kept := make([]launched, 0, len(handed))
	for _, h := range handed {
		kept = append(kept, launched{Name: h.name, Root: h.root, Text: h.text})
		if room == nil && h.text != "" {
			sections = append(sections, checkoutHeader(h.name, h.root)+"\n\n"+h.text)
		}
		roots = append(roots, h.root+"\n")
	}
	if c.Roots != "" {
		if err = os.WriteFile(c.Roots, []byte(strings.Join(roots, "")), 0o600); err != nil {
			return err
		}
	}
	if err = writeLaunched(c.State, kept); err != nil {
		return err
	}
	if err = os.Remove(c.Agents); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(sections) > 0 {
		if err = os.WriteFile(c.Agents, []byte(strings.Join(sections, "\n\n")), 0o600); err != nil {
			return err
		}
	}
	spec := c.checkoutSpec
	if c.NestedSkillsOnly {
		spec.Instructions, spec.Limit = nil, nil
	}
	nested, err := json.Marshal(nestedConfig{State: c.State, Workdir: work, checkoutSpec: spec})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.State, "nested.json"), append(nested, '\n'), 0o600)
}

// checkoutHeader is the line a checkout's section opens with, naming it.
func checkoutHeader(name, root string) string { return "for `" + name + "` with root " + root }

// launchedFile is the file under a run's checkout state where checkout-launch
// keeps every checkout it handed, in its order, with its instructions, for a
// runner that composes the launch again without some of them (copilot-launch).
const launchedFile = "launched.json"

// launched is one checkout checkout-launch handed: its name, root and
// instructions ("" for none).
type launched struct {
	Name string `json:"name"`
	Root string `json:"root"`
	Text string `json:"text"`
}

// writeLaunched writes kept to state's [launchedFile].
func writeLaunched(state string, kept []launched) error {
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(state, launchedFile), append(raw, '\n'), 0o600)
}

// readLaunched is state's [launchedFile].
func readLaunched(state string) ([]launched, error) {
	raw, err := readBounded(filepath.Join(state, launchedFile), maxLaunched)
	if err != nil {
		return nil, err
	}
	var kept []launched
	if err = json.Unmarshal(raw, &kept); err != nil {
		return nil, fmt.Errorf("%s: %w", launchedFile, err)
	}
	return kept, nil
}

// maxLaunched bounds [launchedFile] as read back: the instructions of every
// checkout under a harness with no limit, which Copilot refuses to start with
// far below this (its static budget, some 110,000 tokens at most).
const maxLaunched = 64 << 20

// skippedLog is the file under a run's checkout state a nested firing keeps
// what it left out in ([skips]), for the runner's script to print once the
// harness has stopped (internal/sandbox/executor's checkoutHome.saySkipped):
// no harness the runners drive puts a hook's stderr in the run's output
// (measured: Codex 0.171.0, and 0.172.0, keep it nowhere; Claude Code 2.1.330
// only in its session record; Grok Build, Muse Code, Copilot CLI and Pi drop
// it).
const skippedLog = "skipped"

// maxSkippedLog bounds [skippedLog]: printed at the error stream's end, of
// which the engine shows a failure up to sandbox.MaxFailureBytes (2 MiB less
// a reserve), it never pushes the harness's own last words out of what is
// shown. 64 KiB holds some 250 lines; the first that would pass it is
// replaced by [skippedCut], written once, and the rest dropped.
const maxSkippedLog = 64 << 10

// skippedCut is the line that ends a [skippedLog] cut at [maxSkippedLog].
const skippedCut = "notehelper: more was left out than the run keeps; the rest is not listed\n"

// skips is what a firing left out, a line each naming the checkout, the
// skill or tree, and why: held for the firing's answer ([skippedListing]),
// and, where keep names a file, appended to it under state's lock
// ([handCheckouts]) for the run's end, else said on stderr as it is found.
// One place each: a runner that prints the log and passes a firing's stderr
// on (Pi's) says each line once, and the helper's own trouble stays on
// stderr. A checkout that waits for a later firing's room is said on stderr
// alone: it is handed then, never left out.
type skips struct {
	stderr io.Writer
	keep   string
	lines  []string
	files  int // how many of lines, from the first, [skips.sayFiles] said
}

// say says one line of what the firing left out.
func (s *skips) say(format string, args ...any) {
	s.put(len(s.lines), fmt.Sprintf(format, args...))
}

// sayFiles says lines about a checkout's instruction files, ahead of every
// line about a skill: they name what a claimed checkout was not handed, and a
// cut listing keeps its first lines.
func (s *skips) sayFiles(lines []string) {
	for _, line := range lines {
		s.put(s.files, line)
		s.files++
	}
}

func (s *skips) put(at int, line string) {
	s.lines = slices.Insert(s.lines, at, line)
	if s.keep == "" {
		fmt.Fprintf(s.stderr, "notehelper: %s\n", line)
	}
}

// sayAll says each of lines ([skips.say]).
func (s *skips) sayAll(lines []string) {
	for _, line := range lines {
		s.say("%s", line)
	}
}

// kept appends s's lines to s.keep, within [maxSkippedLog], its trouble on
// stderr: a firing's answer is the model's, never held up by its log.
func (s *skips) kept() {
	if s.keep == "" || len(s.lines) == 0 {
		return
	}
	err := appendSkipped(s.keep, s.lines)
	if err != nil {
		fmt.Fprintf(s.stderr, "notehelper: keeping what the firing left out for the run's end: %v\n", err)
	}
}

// appendSkipped appends lines to the log at path, made where there is none,
// the first that would pass [maxSkippedLog] replaced by [skippedCut] and the
// rest dropped; a log that ends in the cut takes nothing more.
func appendSkipped(path string, lines []string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // the run's own state
	if err != nil {
		return err
	}
	var out strings.Builder
	info, err := file.Stat()
	if err == nil && !endsCut(file, info.Size()) {
		for _, line := range lines {
			line = "notehelper: " + line + "\n"
			if info.Size()+int64(out.Len()+len(line)+len(skippedCut)) > maxSkippedLog {
				out.WriteString(skippedCut)
				break
			}
			out.WriteString(line)
		}
		_, err = file.WriteString(out.String())
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return err
}

// endsCut reports whether the log file, of size bytes, ends in [skippedCut].
func endsCut(file *os.File, size int64) bool {
	if size < int64(len(skippedCut)) {
		return false
	}
	tail := make([]byte, len(skippedCut))
	_, err := file.ReadAt(tail, size-int64(len(tail)))
	return err == nil && string(tail) == skippedCut
}

// endedMarker is the file under a run's checkout state its runner's restore
// writes once the harness has stopped (internal/sandbox/executor's
// checkoutHome.restore), under state's lock, which a firing holds for all
// its work ([handCheckouts]): one that began finishes first, and one after,
// orphaned by the harness's end, claims nothing, so nothing is copied after
// the restore removed the run's copies. A box without flock(1) writes it
// unlocked, and there a firing already past its check may still copy.
const endedMarker = "ended"

// handNested is a nested adapter's firing under c ([handCheckouts]), within
// its room, else its spec's FiringLimit in bytes: the checkouts handed and
// what it left out, kept in [skippedLog] too.
func handNested(c nestedConfig, stderr io.Writer) ([]handedCheckout, []string, error) {
	said := &skips{stderr: stderr, keep: filepath.Join(c.State, skippedLog)}
	firing := c.firing
	if firing == nil {
		firing = &firingBound{left: math.MaxInt, count: byteCount, unitBytes: 1, names: newListingBudget().namesWithin(),
			namesCap: true}
		if c.FiringLimit != nil {
			firing.left = *c.FiringLimit
		}
	}
	handed, err := handCheckouts(c.State, c.Workdir, nil, c.checkoutSpec, firing, said)
	return handed, said.lines, err
}

// firingBound is what is left of a firing's bound on its instructions, and
// how it counts text: in bytes ([byteCount]), or as the harness reading the
// answer counts its characters ([utf16Count]).
type firingBound struct {
	left  int
	count func(string) int
	// unitBytes is the most bytes one unit of count takes, so a file's size
	// over it is the least it counts (1 a byte, 3 a UTF-16 unit, 4 a code
	// point); readCap, where above 0, the most units a file is read to whatever
	// left allows, a file past it kept unread for its adapter to name, not
	// left out.
	unitBytes, readCap int
	// names is what is left, in nameCount (bytes where nil), for the lines
	// naming the files left out, each with its "\n- ", and for nameCost's
	// block naming a claimed checkout's files ([fitClaim]); namesCap whether
	// no later firing names more, so the first checkout is claimed even past
	// it; namesUsed whether one took any; namesSaid whether a wait was said.
	names                          int
	nameCount                      func(string) int
	nameCost                       func(name, root string, parts []instructionPart) int
	namesCap, namesUsed, namesSaid bool
}

// unboundNames is a firing bound with no bound on its names: a launch's.
func unboundNames(b *firingBound) *firingBound {
	b.names, b.namesCap = math.MaxInt, true
	return b
}

// namesWithin is the bytes a firing's left-out listing, taken first from
// budget b ([listingBudget.listings]), holds for its lines.
func (b *listingBudget) namesWithin() int {
	return max(b.left-len(skippedListing([]string{""})), 0)
}

// byteCount is s's length in bytes.
func byteCount(s string) int { return len(s) }

// utf16Count is s's length as a JavaScript string's, in UTF-16 code units: one
// a character, two past U+FFFF, and one an invalid byte, which decoding
// replaces with U+FFFD once a sequence at most. A unit is never more than
// three bytes, so a byte bound three times a unit bound holds the same text.
func utf16Count(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// fitFiring is parts within what is left of a firing's bound on its
// instructions, each counted with its frame ([framedCost]): each part that
// fits whole, in order, and the rest left out whole, each said by its path
// for the model to read there, never cut, as a cut file reads as the whole.
func fitFiring(parts []instructionPart, firing *firingBound, said *skips) []instructionPart {
	var kept []instructionPart
	for _, p := range parts {
		if cost := framedCost(p, firing.count); p.unread && firing.readCap == 0 || cost > firing.left {
			said.say("the instruction file %s is left out: it passes what is left of the firing's bound on "+
				"instructions; read it at that path", p.path)
		} else {
			firing.left -= cost
			kept = append(kept, p)
		}
	}
	return kept
}

// framedCost is what part p takes of a firing's bound, in count: its text,
// its path three times and 64 more, more than any adapter wraps a file in
// (qwen-nested: the path twice and 47 bytes; a checkout's section header, its
// root and 22 bytes, once per checkout; pi-nested: the path and 54 bytes, and
// once per checkout a label of its name, its root and 21 bytes, a file's path
// its root's and 10 bytes more), so the bound holds the answer whole.
func framedCost(p instructionPart, count func(string) int) int {
	return count(p.text) + 3*count(p.path) + 64
}

// skippedListing is what a firing left out, as a nested adapter hands it to
// the model, "" for nothing: a line each, so a model told of a repository's
// skill knows when one it looks for was not copied, and why.
func skippedListing(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "# Left out of the repositories below the working directory\n\n- " + strings.Join(lines, "\n- ")
}

// maxListing bounds what one firing lists for the model, the skills it copied
// and what it left out: beside one checkout's instructions within Codex's
// default project_doc_max_bytes (32 KiB), 64 KiB more keeps an answer near
// 24,000 tokens at some four bytes a token, inside the 32,768-token
// additionalContextLimit a Codex run sets, so Codex hands it whole rather
// than spill it to a file. Past it the rest is cut, [listingCut] saying so.
const maxListing = 64 << 10

// listingCut ends a firing's listings cut at [maxListing].
const listingCut = "(The rest of this listing passes its 64 KiB bound and is not shown.)"

// listingBudget is what is left of one firing's bound on its listings, the
// line a cut ends with, naming that bound, and whether they were cut.
type listingBudget struct {
	left    int
	cutLine string
	cut     bool
}

func newListingBudget() *listingBudget { return newListingBudgetWithin(maxListing) }

// newListingBudgetWithin is a budget whose listings, with the blank line
// before each, never pass bound bytes: the cut line and the separator before
// the block it ends are reserved, since a cut keeps at most what is left (its
// last newline standing in for the cut line's own). A bound below
// [maxListing] is the room the answer had, which its cut line names; one too
// small to say a cut lists nothing.
func newListingBudgetWithin(bound int) *listingBudget {
	cutLine := listingCut
	if bound < maxListing {
		cutLine = fmt.Sprintf("(The rest of this listing passes the %d bytes this answer had room for and is not shown.)",
			bound)
	}
	b := &listingBudget{left: bound - len(cutLine) - len("\n\n"), cutLine: cutLine}
	b.cut = b.left < 0
	return b
}

// take is block within what is left of b, block apart from the one before by
// a blank line: whole where it fits, else cut after its last line that does,
// with b's cut line, once; "" for an empty block and for every block after a
// cut.
func (b *listingBudget) take(block string) string {
	if block == "" || b.cut {
		return ""
	}
	if len(block)+2 <= b.left {
		b.left -= len(block) + 2
		return block
	}
	b.cut = true
	kept := block[:max(b.left, 0)]
	if i := strings.LastIndexByte(kept, '\n'); i >= 0 {
		return kept[:i] + "\n" + b.cutLine
	}
	return b.cutLine
}

// appendNonEmpty is blocks with each of more that is not "".
func appendNonEmpty(blocks []string, more ...string) []string {
	for _, b := range more {
		if b != "" {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// listings is each handed checkout's skills' listing ([skillsListing]) and
// what the firing left out ([skippedListing]) within b, in the order they
// are shown (each checkout's, then the left out), the left-out lines taken
// first: a checkout is claimed for good, so a file or skill it was not
// handed is the model's to find only by the line naming it.
func (b *listingBudget) listings(handed []handedCheckout, skipped []string) (skills []string, left string) {
	left = b.take(skippedListing(skipped))
	skills = make([]string, len(handed))
	for i, h := range handed {
		skills[i] = b.take(skillsListing(h))
	}
	return skills, left
}

// readNested is a nested adapter's config, refused where it names no state or
// working directory.
func readNested(path string) (nestedConfig, error) {
	var c nestedConfig
	raw, err := readBounded(path, maxConfig)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.State == "" || c.Workdir == "" {
		return c, errors.New("it names no state and working directory")
	}
	return c, nil
}

// handCheckouts claims, under state's lock, each checkout under work no launch
// or firing has handed yet ([checkoutNames]) whose clone has finished
// ([cloneFinished]) and whose skills fit what is left of this firing's bound
// ([copyRoom]), planned before it is claimed ([planCheckoutSkills]); one that
// does not fit waits for a later firing's fresh room, except that a fresh room
// takes a checkout whose skills alone pass it with those that fit, and one with
// no skills is claimed whatever is left, in its order. The claims
// are recorded in state before anything is handed: each one's instructions
// within the harness's limit, as a session started in it reads them, all of
// theirs within firing ([fitFiring]), and its skills copied as
// `<tag>:<skill>` ([copyPlanned]). What it leaves out is
// said to said, and kept where said asks, under the lock.
func handCheckouts(state, work string, first []string, spec checkoutSpec, firing *firingBound, said *skips) ([]handedCheckout, error) {
	spec.held = &homeNames{}
	lock, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(filepath.Join(state, endedMarker)); err == nil {
		return nil, nil
	}
	defer said.kept()
	seen, err := readHanded(filepath.Join(state, "handed"))
	if err != nil {
		return nil, err
	}
	limit := math.MaxInt
	if spec.Limit != nil {
		limit = *spec.Limit
	}
	names, err := checkoutNames(work, first)
	if err != nil {
		return nil, err
	}
	tags := newCheckoutTags(spec.Reserved)
	for _, name := range seen {
		tags.take(name)
	}
	was := map[string]bool{}
	for _, name := range seen {
		was[name] = true
	}
	room, spent := newCopyRoom(), false
	var claimed []string
	plans := map[string][]plannedSkill{}
	parts := map[string][]instructionPart{}
	// THE ANSWER'S ROOM DECIDES LAST: a checkout whose skills wait spends none
	// of it ([claimRoom]); a gated spec's files are the gate's, so only the
	// room charges them, and an ungated one's only the firing ([fitClaim]).
	answers := func(name string) bool {
		if spec.room == nil {
			return fitClaim(work, name, limit, spec, firing, parts, said)
		}
		h := handedCheckout{name: name, root: filepath.Join(work, name)}
		if spec.gate != nil {
			g := spec.gate.Checkouts[h.root]
			h.text, h.files = g.Text, g.Files
		}
		return spec.room.take(h, said)
	}
	for _, name := range names {
		if was[name] || !cloneFinished(filepath.Join(work, name)) {
			continue
		}
		if spec.gate != nil && !spec.gate.holds(filepath.Join(work, name)) {
			continue // finished after the gate was read: the next firing's gate holds it
		}
		if spec.Skills == "" {
			if answers(name) {
				claimed = append(claimed, name)
			}
			continue
		}
		if room.spent() {
			if !spent {
				fmt.Fprintln(said.stderr, "notehelper: this firing's copy bound is spent; the checkouts with skills left wait for the next")
				spent = true
			}
			if dirs, over := checkoutSkillDirs(filepath.Join(work, name), spec); len(dirs) > 0 || len(over) > 0 ||
				!answers(name) {
				continue
			}
			tags.take(name)
			claimed = append(claimed, name)
			continue
		}
		// ITS PLAN'S LINES ARE SAID ONCE IT IS CLAIMED: a checkout that waits is
		// planned again at every firing until one has room, and said nothing
		// before it is handed.
		planSaid := &skips{stderr: io.Discard}
		planned, past := planCheckoutSkills(filepath.Join(work, name), tags.peek(name), spec, planSaid)
		if (past || !room.fits(planned)) && room.used || !answers(name) {
			continue
		}
		said.sayAll(planSaid.lines)
		if past {
			said.say("the skills of %s past one firing's bound are left out", filepath.Join(work, name))
		}
		var taken []plannedSkill
		for _, p := range planned {
			if !room.take(p) {
				said.say("the skill %s is left out: its %d entries of %d bytes pass what is left of the firing's bound",
					p.src, len(p.items), p.bytes)
				continue
			}
			taken = append(taken, p)
		}
		tags.take(name)
		claimed, plans[name] = append(claimed, name), taken
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	// CLAIMED BEFORE ANYTHING IS HANDED: a firing that dies past this point has
	// handed some of a checkout, never one the next firing claims again and
	// finds its skills' names taken by its own copies.
	if err = appendLines(filepath.Join(state, "handed"), claimed); err != nil {
		return nil, err
	}
	handed := make([]handedCheckout, 0, len(claimed))
	staging := filepath.Join(state, stagingDir)
	if spec.Skills != "" {
		staging = stagingFor(state, spec.Skills)
	}
	for _, name := range claimed {
		root := filepath.Join(work, name)
		h := handedCheckout{name: name, root: root}
		switch {
		case spec.gate != nil:
			g := spec.gate.Checkouts[root]
			h.text, h.files = g.Text, g.Files
			said.sayAll(g.Said)
		case spec.ImportInstructions:
			h.text = rootImports(root, spec.Instructions, said)
		default:
			h.text, h.parts = joinParts(parts[name]), parts[name]
		}
		for _, p := range plans[name] {
			skill, err := copyPlanned(p, spec, staging, said)
			if err != nil {
				said.say("the skill %s is left out: %v", p.src, err)
				continue
			}
			h.skills = append(h.skills, skill)
		}
		handed = append(handed, h)
	}
	return handed, nil
}

// fitClaim is whether the checkout name under work may be claimed now for
// its instructions: its files read within limit and what is left of firing,
// one whose size alone passes that never read ([instructionPartsWithin]),
// fitted ([fitFiring]) into parts, the lines naming those left out said
// first ([skips.sayFiles]); those lines and firing's nameCost within its
// names. One past what is left waits for a later firing, said once, unless
// it would be the first to name any where no firing names more: a checkout
// is claimed for good, and a file it was not handed is the model's to find
// only by its name. An importing spec reads its files at hand, and a gated
// one takes them from its gate ([checkoutGate]), naming none here.
func fitClaim(work, name string, limit int, spec checkoutSpec, firing *firingBound,
	parts map[string][]instructionPart, said *skips) bool {
	if spec.ImportInstructions || spec.gate != nil {
		return true
	}
	root := filepath.Join(work, name)
	trial := &skips{stderr: io.Discard}
	read, _ := instructionPartsWithin(root, limit, spec.Instructions, firing, trial)
	try := *firing
	kept := fitFiring(read, &try, trial)
	count := firing.nameCount
	if count == nil {
		count = byteCount
	}
	cost := 0
	for _, line := range trial.lines {
		cost += count(line) + len("\n- ")
	}
	if firing.nameCost != nil && len(kept) > 0 {
		cost += firing.nameCost(name, root, kept)
	}
	if cost > 0 && cost > firing.names && (firing.namesUsed || !firing.namesCap) {
		if !firing.namesSaid {
			fmt.Fprintln(said.stderr, "notehelper: this firing's room to name what it leaves out is spent; the "+
				"checkouts left wait for the next")
			firing.namesSaid = true
		}
		return false
	}
	firing.left, firing.names = try.left, firing.names-cost
	firing.namesUsed = firing.namesUsed || cost > 0
	parts[name] = kept
	said.sayFiles(trial.lines)
	return true
}

// checkoutNames is every checkout directly under work: the names in first
// that are one, in their order, then the rest by name. A name starting with
// a dot is never one, whether first names it or not: the run's own state
// sits under such names, and the launch and the hook hold to one rule.
func checkoutNames(work string, first []string) ([]string, error) {
	var names []string
	taken := map[string]bool{}
	for _, name := range first {
		if !taken[name] && checkoutName(work, name) {
			taken[name] = true
			names = append(names, name)
		}
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if name := e.Name(); !taken[name] && checkoutName(work, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// checkoutName reports whether name is a checkout directly under work: one
// path element, no leading dot and no control character (the handed file holds
// a name a line, and reading it back drops a line's trailing CR), a directory
// or a link to one, holding .git.
func checkoutName(work, name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsRune(name, filepath.Separator) ||
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(work, name))
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(work, name, ".git"))
	return err == nil
}

// instructionParts is root's instructions, file by file, each slot's in order
// (a slot of names its first regular file, links followed, within the
// checkout alone where Inside; a tree's files, [treeTexts]; where Once, none
// an earlier slot handed, [slotOptions]): each file's path and its text, cut to what is left of limit
// on a rune boundary, invalid UTF-8 replaced, and how many bytes they took
// ([joinParts] hands them as one text). A file the limit cut spends all of
// it, as a harness charging what it read hands nothing after it, never the
// bytes a split rune left. A file of whitespace alone, as a harness reads
// one, is its slot's file and hands nothing, charged nothing, unless the slot
// passes it over (SkipBlank).
func instructionParts(root string, limit int, slots []instructionSlot, said *skips) ([]instructionPart, int) {
	return instructionPartsWithin(root, limit, slots, nil, said)
}

// instructionPartsWithin is [instructionParts] for a claim within fit (nil for
// none): a slot's file whose size over fit's unitBytes, with its frame, passes
// what is left of fit is left out unread, never opened, and one read is read
// to at most unitBytes a unit left, past which it is left out too, so a file
// of any size costs a firing a stat.
func instructionPartsWithin(root string, limit int, slots []instructionSlot, fit *firingBound,
	said *skips) ([]instructionPart, int) {
	var texts []instructionPart
	spent := 0
	handed := handedFiles{}
	for _, slot := range slots {
		if slot.Tree != nil {
			for _, file := range treeTexts(root, *slot.Tree, said) {
				if !handed.first(file.real, slot.Tree.Once) {
					continue
				}
				text, n, cut := fitInstructions(file.raw, limit-spent)
				if n > 0 {
					texts, spent = append(texts, instructionPart{path: file.path, text: text}), spent+n
				}
				if cut {
					spent = limit
				}
			}
			continue
		}
		for _, name := range slot.Names {
			path := filepath.Join(root, name)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if real, ok := slotFile(root, path, slot.slotOptions); !ok || !handed.first(real, slot.Once) {
				continue
			}
			room, most := limit-spent, math.MaxInt
			if fit != nil {
				most = fit.left
				if fit.readCap > 0 && fit.readCap < most {
					most = fit.readCap
				}
			}
			if fit != nil && most < math.MaxInt/max(fit.unitBytes, 1) {
				least := int((info.Size() + int64(fit.unitBytes) - 1) / int64(max(fit.unitBytes, 1)))
				if least+3*fit.count(path)+64 > most {
					texts = append(texts, instructionPart{path: path, unread: true})
					break
				}
				if bytes := fit.unitBytes * most; bytes < room {
					if info.Size() > int64(bytes) {
						texts = append(texts, instructionPart{path: path, unread: true})
						break
					}
					room = bytes
				}
			}
			text, n, cut := readInstructions(path, info.Size(), room)
			if cut && room < limit-spent {
				texts = append(texts, instructionPart{path: path, unread: true}) // it grew past the cap since its stat
				break
			}
			if n == 0 && !cut && slot.SkipBlank {
				continue
			}
			if n > 0 {
				texts, spent = append(texts, instructionPart{path: path, text: text}), spent+n
			}
			if cut {
				spent = limit
			}
			break
		}
	}
	return texts, spent
}

// handedFiles is the real paths of the files a checkout's slots handed.
type handedFiles map[string]bool

// first records real as handed and reports whether it may be handed: always,
// unless once and an earlier slot handed it.
func (h handedFiles) first(real string, once bool) bool {
	if once && h[real] {
		return false
	}
	h[real] = true
	return true
}

// slotFile is the real path of a slot's file at path, of the checkout at
// root, and whether the slot hands it: not where it is Inside and the file
// leads out of the checkout.
func slotFile(root, path string, o slotOptions) (string, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, !o.Inside
	}
	if !o.Inside {
		return real, true
	}
	realRoot, err := filepath.EvalSymlinks(root)
	return real, err == nil && within(realRoot, real)
}

// joinParts is parts' texts as one, apart by a blank line.
func joinParts(parts []instructionPart) string {
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return strings.Join(texts, "\n\n")
}

// readInstructions is the file at path, of size bytes, cut to room bytes on a
// rune boundary and made valid UTF-8, the bytes it took, and whether room cut
// it; "" and 0 for no room, an unreadable file and one of whitespace alone.
func readInstructions(path string, size int64, room int) (string, int, bool) {
	if room <= 0 {
		return "", 0, size > 0
	}
	raw, err := readBounded(path, int64(room))
	if err != nil {
		return "", 0, false
	}
	cut := size > int64(len(raw))
	text, n := instructionText(raw, cut)
	return text, n, cut
}

// fitInstructions is raw, a whole file, as [readInstructions] hands a file
// of its size within room.
func fitInstructions(raw []byte, room int) (string, int, bool) {
	if room <= 0 {
		return "", 0, len(raw) > 0
	}
	cut := len(raw) > room
	if cut {
		raw = raw[:room]
	}
	text, n := instructionText(raw, cut)
	return text, n, cut
}

// instructionText is raw as handed and the bytes it took: less the rune a cut
// split, valid UTF-8, "" and 0 for whitespace alone.
func instructionText(raw []byte, cut bool) (string, int) {
	if cut {
		raw = cutAtRune(raw)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", 0
	}
	return strings.ToValidUTF8(string(raw), "�"), len(raw)
}

// cutAtRune is b, cut where a byte limit fell, less the rune the limit split.
func cutAtRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}

// checkoutTags gives each checkout the tag its skills are copied under: its
// name lower-cased, every byte but [a-z0-9] a hyphen, and where a reserved
// name or an earlier checkout took it, the first free `<tag>-<n>` from 2.
// Replaying the handed checkouts in their order gives each its tag again.
type checkoutTags map[string]bool

func newCheckoutTags(reserved []string) checkoutTags {
	t := checkoutTags{}
	for _, r := range reserved {
		t[r] = true
	}
	return t
}

// peek is the tag name would take now, nothing taken.
func (t checkoutTags) peek(name string) string {
	base := slug(name, "")
	tag := base
	for n := 2; t[tag]; n++ {
		tag = base + "-" + strconv.Itoa(n)
	}
	return tag
}

// take takes the tag name would take ([checkoutTags.peek]).
func (t checkoutTags) take(name string) { t[t.peek(name)] = true }

// slug is s lower-cased with every byte outside [a-z0-9] and keep a hyphen,
// byte by byte, so a multibyte character takes one hyphen a byte.
func slug(s, keep string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', strings.IndexByte(keep, c) >= 0:
		default:
			b[i] = '-'
		}
	}
	return string(b)
}

// renameSkill is a SKILL.md with its frontmatter's name set to name, every
// other line as it was, and the description it holds ([skillDescription]);
// false for a file that does not open with `---`, which no harness lists. A
// harness lists a skill by its frontmatter's name, and a seat's skill loads
// are reported by names a repository's never take.
func renameSkill(raw []byte, name string) ([]byte, string, bool) {
	text := strings.TrimSuffix(string(raw), "\n")
	if len(raw) == 0 {
		return nil, "", false
	}
	lines := strings.Split(text, "\n")
	if strings.TrimSuffix(lines[0], "\r") != "---" {
		return nil, "", false
	}
	var out strings.Builder
	out.WriteString(lines[0] + "\n" + "name: \"" + name + "\"\n")
	var front []string
	inFront, drop := true, false
	for _, line := range lines[1:] {
		l := strings.TrimSuffix(line, "\r")
		switch {
		case inFront && l == "---":
			inFront = false
		case inFront:
			if key, ok := frontKey(l); ok {
				drop = key == "name"
			}
			front = append(front, l)
			if drop {
				continue
			}
		}
		out.WriteString(line + "\n")
	}
	return []byte(out.String()), skillDescription(front), true
}

// frontKey is the key a frontmatter line opens, true where it opens one: a
// line starting with none of space, tab, `#` or `-`, up to its first colon.
func frontKey(l string) (string, bool) {
	if l == "" || strings.ContainsRune(" \t#-", rune(l[0])) {
		return "", false
	}
	key, _, ok := strings.Cut(l, ":")
	return strings.TrimRight(key, " \t"), ok
}

// skillDescription is the frontmatter's description on one line: a plain or
// quoted value, or a block's (`|`, `>`) indented lines joined by spaces, cut
// to [maxSkillDescription] characters.
func skillDescription(front []string) string {
	for i, l := range front {
		if key, ok := frontKey(l); !ok || key != "description" {
			continue
		}
		_, value, _ := strings.Cut(l, ":")
		value = strings.TrimSpace(value)
		if value != "" && strings.ContainsRune("|>", rune(value[0])) {
			var parts []string
			for _, next := range front[i+1:] {
				if next != "" && next[0] != ' ' && next[0] != '\t' {
					break
				}
				if part := strings.TrimSpace(next); part != "" {
					parts = append(parts, part)
				}
			}
			value = strings.Join(parts, " ")
		} else if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if r := []rune(value); len(r) > maxSkillDescription {
			value = string(r[:maxSkillDescription])
		}
		return value
	}
	return ""
}

// skillsListing is the skills copied for h as a nested adapter lists them,
// "" for none: each by its name, its description and its copy's SKILL.md,
// `- <name>: <description> (file: <path>)` as Codex lists a skill, for a
// model to read where its harness lists no skill copied during a session.
func skillsListing(h handedCheckout) string {
	if len(h.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills of " + h.root + "\n\nEach is a set of local instructions to follow, stored in its " +
		"`SKILL.md` file; to use one, read that file.")
	for _, s := range h.skills {
		if s.description == "" {
			fmt.Fprintf(&b, "\n- %s (file: %s)", s.name, s.path)
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s (file: %s)", s.name, s.description, s.path)
	}
	return b.String()
}

// readBounded is the first n bytes of path, or the whole file where shorter.
func readBounded(path string, n int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the run's own state, or a checkout's file the harness itself would read
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, n))
}

// readHanded is the checkouts the run has handed, one name a line, in the
// order they were claimed.
func readHanded(path string) ([]string, error) {
	file, err := os.Open(path) //nolint:gosec // the run's own state
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var handed []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		handed = append(handed, scanner.Text())
	}
	return handed, scanner.Err()
}

// appendLines adds lines to the file at path, made where there is none, and
// syncs it, so a line a later step relies on is on disk before that step.
func appendLines(path string, lines []string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the run's own state
	if err != nil {
		return err
	}
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	if err == nil {
		err = file.Sync()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return err
}

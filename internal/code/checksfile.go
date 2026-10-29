package code

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE CHECKS A REPOSITORY DEFINES: `.crewlet/checks.yaml` at the root of its
// tree, read as the one grammar every reader of it shares — the home git node
// that queues a change's checks from it, the runner that runs one, and
// `crewlet validate -tier checks` on a laptop before anything is pushed.
//
// # Why a file in the repository, read at the TARGET
//
// A check is a measurement of a commit, so what it measures is defined beside
// the code it measures and moves with it — a new test target lands in the same
// change as the tests. But a change must never define the measurement it is
// judged by: the file that governs a change's checks is the one its TARGET
// branch holds, so a change that edits checks.yaml is judged by the file it
// edits, never by its own edit. Reading the file there is the dispatch's.
//
// # Why strict
//
// Every key is one this grammar knows, and an unknown one is refused rather
// than ignored, as every config document's is: `timout_seconds:` ignored would
// run the check under no timeout its author believes it has, and a check whose
// typo'd `paths:` is ignored runs on every change instead of the few it meant.
// So the parser walks the YAML tree itself and refuses, located at its line,
// every key it does not know, every key written twice, every value of the
// wrong shape — a number or a boolean where text belongs, which YAML reads
// without a word (`name: 1.10` is the number 1.1) — every alias and merge key,
// and every value outside its bound. EVERY failure, not the first: an author
// fixing one field at a time pays a push per mistake, and they are usually
// made together.
//
// The faults are config's own ([config.Fault]: the authored path, the kind and
// the line), so `crewlet validate -json` reports a checks file exactly as it
// reports a company document, and an authoring loop reads one shape.
//
// # What the file does not decide
//
// Which checks a merge REQUIRES is the target's protection rule's, never the
// file's: a file that could mark its own checks optional is a change that
// could waive its own gate. How long a company lets a check run
// (code.checks.max_timeout_seconds), which cell runs one the file leaves to
// the company (code.checks.run_in) and which secrets exist to be named are the
// company's configuration's: the file names, the company decides.

// ChecksFilePath is where a repository defines its checks: the path, from the
// root of its tree, of the file [ParseChecks] reads.
const ChecksFilePath = ".crewlet/checks.yaml"

// The file's bounds. Each one is a refusal at its field, so a file that passes
// is one every reader holds whole: a check's definition rides every queued
// record of it ([CheckDef]), and a home node reads the file on every change it
// dispatches.
const (
	// MaxChecksFileBytes is the largest checks file read: 64 KiB. A file of
	// MaxChecksPerFile checks at a few lines each is a few KiB; one past this
	// is generated, and generated configuration belongs to a build tool the
	// check runs, never to the file the engine parses on every dispatch.
	MaxChecksFileBytes = 64 << 10

	// MaxChecksPerFile is how many checks one file defines: 64, past any CI
	// pipeline's job list a person maintains (a large monorepo's runs tens),
	// and the bound that keeps a commit's dispatch — a record a check — a
	// burst the code log takes in stride.
	MaxChecksPerFile = 64

	// MaxCheckRunBytes is the longest command a check runs: 4 KiB. A check's
	// command is handed to `sh -c` whole — a script a block scalar spells over
	// several lines included — and one past a few hundred bytes is a script
	// that belongs in the repository, where it is reviewed as code.
	MaxCheckRunBytes = 4 << 10

	// CheckTimeoutCeiling is the longest a check may run: six hours, GitHub
	// Actions' own per-job ceiling — the bound the CI a company already runs
	// holds its longest job to, so no pipeline moved here has to be cut.
	// What a company lets its checks run is its own, at or under this
	// (code.checks.max_timeout_seconds); the file is held to the ceiling.
	CheckTimeoutCeiling = 6 * time.Hour

	// MaxCheckPaths is how many path globs one check names: 32, past the
	// directories any one check is about (a service, its shared libraries,
	// its build files), and with gitref's 128-byte glob a check's paths stay
	// a few KiB on every record that carries its definition.
	MaxCheckPaths = 32

	// MaxCheckEnv is how many of the company's check secrets one check is
	// given: 32, past what a build reads (a registry token, a cloud key, a
	// database URL). Each one is a credential a change's own code reads, so a
	// check names those it needs and nothing is handed to it by default.
	MaxCheckEnv = 32

	// MaxCheckEnvNameBytes is the longest variable name a check names: 128
	// bytes, four times the longest a common tool reads
	// (GOOGLE_APPLICATION_CREDENTIALS is 30), so with MaxCheckEnv a check's
	// list is at most 4 KiB.
	MaxCheckEnvNameBytes = 128

	// MaxCheckNameBytes is config.CodeMaxCheckNameBytes: the longest check
	// name ([config.ValidCodeCheckName]), 64 bytes, a CI job name's length. A
	// name is a key in every check row and a component of the token a check's
	// box fetches with.
	MaxCheckNameBytes = config.CodeMaxCheckNameBytes
)

// CheckCell is the kind of box a check runs in.
//
// TWO OF THE SANDBOX'S FOUR PLACEMENTS (config.Placements): a container on a
// git node, or a remote E2B box. A check runs a change's own code — its build,
// its tests, whatever its author put there — so it never runs where that code
// reaches the engine: `direct` runs it as the engine's own user on the engine
// host, reading what the engine reads, and `self` is a seat's own executor
// box, which a check, run by no seat, does not have. Both are refused by name
// (U1 a).
type CheckCell string

const (
	// CellContainer is a container on a git node, which holds the
	// repository's cache beside it.
	CellContainer CheckCell = "container"
	// CellE2B is a remote E2B box, which clones the one commit it checks.
	CellE2B CheckCell = "e2b"
)

// CheckCells is the closed set, in this order.
func CheckCells() []CheckCell { return []CheckCell{CellContainer, CellE2B} }

// Valid reports a cell this build runs a check in.
func (c CheckCell) Valid() bool { return slices.Contains(CheckCells(), c) }

// refusedCells are the sandbox placements a check never runs in, each with the
// sentence a file naming it is refused with.
var refusedCells = map[CheckCell]string{
	"direct": "direct runs a box as the engine's own user on the engine host, and a check runs a " +
		"change's own code, which would read what the engine reads; run it in a container or on e2b",
	"self": "self is a seat's own executor box, and a check is run by no seat; run it in a container " +
		"or on e2b",
}

// CheckReport names the files a check writes its results to, each a path
// inside its checkout that the runner reads once the command ends: a JUnit
// XML file, and a SARIF log. Either, both or neither — a check with neither is
// judged by its exit code alone.
type CheckReport struct {
	JUnit string `json:"junit,omitempty"`
	SARIF string `json:"sarif,omitempty"`
}

// CheckCheckout is what of the repository a check's box checks out: the
// whole tree (the zero value), a CONE — the directories named, git's sparse
// checkout — or nothing at all (None, the file's scalar `none`), for a check
// that reads what it needs by object id through the API instead of a clone.
type CheckCheckout struct {
	None bool     `json:"none,omitempty"`
	Cone []string `json:"cone,omitempty"`
}

// CheckDef is one check as the file defines it — and as every queued record
// of a check carries it, so the definition a run was queued under is the one
// it runs under, whatever the file says by the time its box starts.
type CheckDef struct {
	// Name is the check's name in its file ([config.ValidCodeCheckName]),
	// unique there.
	Name string `json:"name"`
	// Run is the command, handed to `sh -c` whole.
	Run string `json:"run"`
	// TimeoutSeconds is how long it may run, 1 to CheckTimeoutCeiling.
	TimeoutSeconds int `json:"timeout_seconds"`
	// Paths are the path globs (gitref.ParsePathGlob) a change must touch for
	// the check to run on it; none runs it on every change.
	Paths []string `json:"paths,omitempty"`
	// Report names the result files the runner reads.
	Report CheckReport `json:"report,omitzero"`
	// RunIn is the cell it runs in; empty is the company's
	// code.checks.run_in.
	RunIn CheckCell `json:"run_in,omitempty"`
	// Checkout is what of the tree its box checks out.
	Checkout CheckCheckout `json:"checkout,omitzero"`
	// Env names the entries of the company's code.checks.env the check is
	// given; none is given none.
	Env []string `json:"env,omitempty"`
}

// Timeout is TimeoutSeconds as a duration — the one conversion, at the edge.
func (d CheckDef) Timeout() time.Duration { return time.Duration(d.TimeoutSeconds) * time.Second }

// Validate refuses a definition no file passes ([ErrInvalid], naming the
// field): the rules [ParseChecks] holds each of a file's checks to, for a
// definition that arrives some other way — on a queued check's record, whose
// writer read it from a file.
func (d CheckDef) Validate() error {
	if faults := defFaults(d); len(faults) > 0 {
		f := faults[0]
		return fmt.Errorf("%w: %s: %s: %s", ErrInvalid, f.path.String(), f.kind.Error(), f.detail)
	}
	return nil
}

// ChecksFile is a parsed `.crewlet/checks.yaml`: its checks in the order the
// file lists them, each name once.
type ChecksFile struct {
	Checks []CheckDef `json:"checks"`
}

// validEnvName reports a variable name a shell takes: a letter or an
// underscore, then letters, digits and underscores, at most
// MaxCheckEnvNameBytes.
func validEnvName(name string) bool {
	if name == "" || len(name) > MaxCheckEnvNameBytes {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// ChecksFileError is every fault [ParseChecks] found in a file, each a
// [config.Fault] at the authored path and line it is about.
//
// IT IS A JOIN BY CONTRACT — it renders one fault a line and unwraps to them —
// so config.Problems reads it as it reads a company document's refusal, one
// problem a fault; and it IS [ErrInvalid], this domain's refusal of a
// declaration by field.
type ChecksFileError struct {
	Faults []*config.Fault
}

// Error renders one fault a line.
func (e *ChecksFileError) Error() string {
	lines := make([]string, len(e.Faults))
	for i, f := range e.Faults {
		lines[i] = f.Error()
	}
	return strings.Join(lines, "\n")
}

// Unwrap is the faults, so a walk reads each one.
func (e *ChecksFileError) Unwrap() []error {
	out := make([]error, len(e.Faults))
	for i, f := range e.Faults {
		out[i] = f
	}
	return out
}

// Is reports that the file was refused by field.
func (e *ChecksFileError) Is(target error) bool { return target == ErrInvalid }

// ParseChecks reads a `.crewlet/checks.yaml` STRICTLY: one YAML document, a
// mapping holding `checks:` — a list of at most [MaxChecksPerFile] checks,
// `checks: []` for none — each a mapping of these keys and no other:
//
//	name             required; a check name (config.ValidCodeCheckName), once
//	run              required; the command, at most MaxCheckRunBytes
//	timeout_seconds  required; a whole number, 1 to CheckTimeoutCeiling
//	paths            a list of at most MaxCheckPaths path globs (gitref)
//	report           a mapping of junit and sarif, each a path in the checkout
//	run_in           container or e2b (never direct or self)
//	checkout         a list of a cone's directories, or the scalar none
//	env              a list of at most MaxCheckEnv variable names
//
// A file past [MaxChecksFileBytes] is refused unread. Every refusal is a
// [ChecksFileError] holding every fault found, each located at the field and
// the line it is about.
func ParseChecks(raw []byte) (ChecksFile, error) {
	p := &checksParser{}
	if len(raw) > MaxChecksFileBytes {
		p.fault(nil, nil, config.ErrOutOfRange, "the file is %d bytes, past the %d a checks file may hold "+
			"(code.MaxChecksFileBytes); a generated pipeline belongs to a build tool a check runs", len(raw),
			MaxChecksFileBytes)
		return ChecksFile{}, p.err()
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	switch err := dec.Decode(&doc); {
	case errors.Is(err, io.EOF):
		p.fault(nil, nil, config.ErrMissing, "the file holds no document; a checks file is a mapping "+
			"holding `checks:`, and `checks: []` defines none")
		return ChecksFile{}, p.err()
	case err != nil:
		p.fault(nil, nil, config.ErrShape, "the file is not YAML: %v", err)
		return ChecksFile{}, p.err()
	}
	var next yaml.Node
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		// A SECOND DOCUMENT, or text past the first that is not YAML: either
		// way the file is not the one document a reader takes it for — a
		// parser reading only the first would run checks a person reading
		// the whole file never sees run, or the reverse.
		p.fault(nil, nil, config.ErrShape, "the file holds more than one YAML document, or text past the "+
			"first that is not YAML; a checks file is one document")
		return ChecksFile{}, p.err()
	}
	file := p.file(&doc)
	if err := p.err(); err != nil {
		return ChecksFile{}, err
	}
	return file, nil
}

// checksParser walks one file's tree, gathering every fault it finds.
type checksParser struct {
	faults []*config.Fault
}

// fault records one failure at path, on n's line where there is a node.
func (p *checksParser) fault(path config.Path, n *yaml.Node, kind error, detail string, args ...any) {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	f := &config.Fault{Path: path, Kind: kind, Detail: detail}
	if n != nil {
		f.Line = n.Line
	}
	p.faults = append(p.faults, f)
}

// err is every fault found, as one [ChecksFileError], or nil for none.
func (p *checksParser) err() error {
	if len(p.faults) == 0 {
		return nil
	}
	return &ChecksFileError{Faults: p.faults}
}

// file reads the document's root: a mapping holding `checks:` and nothing
// else.
func (p *checksParser) file(doc *yaml.Node) ChecksFile {
	root := doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		p.fault(nil, root, config.ErrShape, "the file is %s, and a checks file is a mapping holding "+
			"`checks:`", shapeOf(root))
		return ChecksFile{}
	}
	var list *yaml.Node
	present := p.mapping(nil, root, func(key string, keyNode, value *yaml.Node) {
		if key != "checks" {
			p.fault(config.Path{key}, keyNode, config.ErrUnknownField, "a checks file holds `checks:` alone")
			return
		}
		list = value
	})
	path := config.Path{"checks"}
	switch {
	case !present["checks"]:
		p.fault(path, root, config.ErrMissing, "a checks file holds `checks:`, a list of the "+
			"repository's checks — `checks: []` for none")
		return ChecksFile{}
	case list.Kind == yaml.AliasNode:
		p.alias(path, list)
		return ChecksFile{}
	case list.Kind != yaml.SequenceNode:
		p.fault(path, list, config.ErrShape, "%s, where a list of checks belongs — `checks: []` for none",
			shapeOf(list))
		return ChecksFile{}
	case len(list.Content) > MaxChecksPerFile:
		p.fault(path, list, config.ErrOutOfRange, "%d checks, past the %d one file defines "+
			"(code.MaxChecksPerFile)", len(list.Content), MaxChecksPerFile)
		return ChecksFile{}
	}
	file := ChecksFile{Checks: make([]CheckDef, 0, len(list.Content))}
	named := map[string]int{}
	for i, item := range list.Content {
		def, ok := p.check(under(path, i), item)
		if !ok {
			continue
		}
		if first, taken := named[def.Name]; taken {
			p.fault(under(path, i, "name"), valueOf(item, "name"), config.ErrConflict, "%q is checks[%d]'s "+
				"name too; a check's name is its key in every row and on every token, so each names one "+
				"check", def.Name, first)
			continue
		}
		named[def.Name] = i
		file.Checks = append(file.Checks, def)
	}
	return file
}

// The keys a check is written with, the three it must have, and the reports
// it may name.
var (
	checkKeys     = []string{"name", "run", "timeout_seconds", "paths", "report", "run_in", "checkout", "env"}
	requiredCheck = []string{"name", "run", "timeout_seconds"}
	reportKeys    = []string{"junit", "sarif"}
)

// check reads one entry of `checks:`, answering false where any fault is
// about it.
//
// A FIELD THAT DID NOT READ is not judged again by the rules over its value
// ([defFaults]), which would only restate the fault it already has — a missing
// timeout is not also out of range — while every field that did read is, so a
// check with a typo'd key and a name outside the grammar reports both.
func (p *checksParser) check(path config.Path, n *yaml.Node) (CheckDef, bool) {
	switch {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
		return CheckDef{}, false
	case n.Kind != yaml.MappingNode:
		p.fault(path, n, config.ErrShape, "%s, where a check belongs: a mapping of name, run, "+
			"timeout_seconds and the keys it sets beside them", shapeOf(n))
		return CheckDef{}, false
	}
	before := len(p.faults)
	var def CheckDef
	failed := map[string]bool{}
	present := p.mapping(path, n, func(key string, keyNode, value *yaml.Node) {
		field := under(path, key)
		var ok bool
		switch key {
		case "name":
			def.Name, ok = p.text(field, value)
		case "run":
			def.Run, ok = p.text(field, value)
		case "timeout_seconds":
			def.TimeoutSeconds, ok = p.seconds(field, value)
		case "paths":
			def.Paths, ok = p.texts(field, value)
		case "report":
			def.Report, ok = p.report(field, value)
		case "run_in":
			var cell string
			cell, ok = p.text(field, value)
			def.RunIn = CheckCell(cell)
		case "checkout":
			def.Checkout, ok = p.checkout(field, value)
		case "env":
			def.Env, ok = p.texts(field, value)
		default:
			p.fault(field, keyNode, config.ErrUnknownField, "a check takes %s", strings.Join(checkKeys, ", "))
			return
		}
		failed[key] = !ok
	})
	for _, key := range requiredCheck {
		if !present[key] {
			p.fault(under(path, key), n, config.ErrMissing, "a check names its %s", key)
			failed[key] = true
		}
	}
	for _, f := range defFaults(def) {
		if key, _ := f.path[0].(string); failed[key] {
			continue
		}
		p.fault(under(path, f.path...), nodeAt(n, f.path), f.kind, "%s", f.detail)
	}
	return def, len(p.faults) == before
}

// under is path extended by segments, in a new backing array: sibling fields
// are built from one parent, and a shared array would make each overwrite the
// other's last segment.
func under(path config.Path, segments ...any) config.Path {
	out := make(config.Path, 0, len(path)+len(segments))
	out = append(out, path...)
	return append(out, segments...)
}

// valueOf is the value a mapping holds at key, or the node itself where it is
// no mapping holding key — the node a fault about that key is located at.
func valueOf(n *yaml.Node, key string) *yaml.Node {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
	}
	return n
}

// nodeAt is the node a fault at a definition's field is about, for its line:
// the field's own value, an item of a list where the fault names one — or the
// nearest node above it the file wrote.
func nodeAt(check *yaml.Node, path config.Path) *yaml.Node {
	n := check
	for _, segment := range path {
		switch s := segment.(type) {
		case string:
			n = valueOf(n, s)
		case int:
			if n.Kind != yaml.SequenceNode || s >= len(n.Content) {
				return n
			}
			n = n.Content[s]
		}
	}
	return n
}

// mapping walks a mapping's pairs, handing fn each key once, and answers the
// keys it handed. A key that is not text, an alias, a merge key (`<<`) and a
// key written twice are faults of their own and handed to nobody: a merge key
// spells a check's fields somewhere else, and a key written twice is one YAML
// reads as its last and a person reading the file as its first.
func (p *checksParser) mapping(path config.Path, n *yaml.Node,
	fn func(key string, keyNode, value *yaml.Node)) map[string]bool {

	seen := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode, value := n.Content[i], n.Content[i+1]
		switch {
		case keyNode.Kind == yaml.AliasNode:
			p.alias(path, keyNode)
			continue
		case keyNode.ShortTag() == "!!merge":
			p.fault(path, keyNode, config.ErrShape, "a merge key (<<) spells fields somewhere else, and "+
				"every check is written out where it stands")
			continue
		case keyNode.Kind != yaml.ScalarNode || keyNode.ShortTag() != "!!str":
			p.fault(path, keyNode, config.ErrShape, "a key is %s, and every key of a checks file is text",
				shapeOfValue(keyNode))
			continue
		}
		key := keyNode.Value
		if first, dup := seen[key]; dup {
			p.fault(under(path, key), keyNode, config.ErrConflict, "written twice, first on line %d — YAML "+
				"keeps the last and a person reading the file the first, so each key is written once",
				first.Line)
			continue
		}
		seen[key] = keyNode
		fn(key, keyNode, value)
	}
	present := make(map[string]bool, len(seen))
	for key := range seen {
		present[key] = true
	}
	return present
}

// alias records an alias where a value or a key belongs.
func (p *checksParser) alias(path config.Path, n *yaml.Node) {
	p.fault(path, n, config.ErrShape, "an alias (*%s) spells a value somewhere else, and every check is "+
		"written out where it stands", n.Value)
}

// text reads a scalar that is TEXT — never a number, a boolean or a null YAML
// resolved it to, which a check's text never means: `name: 1.10` is the
// number 1.1, and `run: true` a boolean.
func (p *checksParser) text(path config.Path, n *yaml.Node) (string, bool) {
	switch tag := n.ShortTag(); {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
	case n.Kind != yaml.ScalarNode:
		p.fault(path, n, config.ErrShape, "%s, where text belongs", shapeOf(n))
	case tag == "!!null":
		p.fault(path, n, config.ErrMissing, "it has no value")
	case tag != "!!str":
		p.fault(path, n, config.ErrShape, "%s where text belongs — text that reads as one is quoted (%q)",
			shapeOfValue(n), n.Value)
	default:
		return n.Value, true
	}
	return "", false
}

// texts reads a list of text.
func (p *checksParser) texts(path config.Path, n *yaml.Node) ([]string, bool) {
	switch {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
		return nil, false
	case n.Kind != yaml.SequenceNode:
		p.fault(path, n, config.ErrShape, "%s, where a list belongs — one item is a list of one, [%s]",
			shapeOf(n), n.Value)
		return nil, false
	}
	out := make([]string, 0, len(n.Content))
	ok := true
	for i, item := range n.Content {
		s, read := p.text(under(path, i), item)
		ok = ok && read
		out = append(out, s)
	}
	return out, ok
}

// seconds reads a whole number of seconds, which a timeout is: never a
// fraction, which a reading into a whole number would cut without a word.
func (p *checksParser) seconds(path config.Path, n *yaml.Node) (int, bool) {
	switch {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
		return 0, false
	case n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int":
		p.fault(path, n, config.ErrShape, "%s where a whole number of seconds belongs", shapeOfValue(n))
		return 0, false
	}
	var v int64
	if err := n.Decode(&v); err != nil {
		p.fault(path, n, config.ErrOutOfRange, "%s seconds, past any number a check may run for", n.Value)
		return 0, false
	}
	if ceiling := int64(CheckTimeoutCeiling / time.Second); v < 1 || v > ceiling {
		p.fault(path, n, config.ErrOutOfRange, "%d seconds: a check runs for 1 to %d seconds "+
			"(code.CheckTimeoutCeiling, six hours)", v, ceiling)
		return 0, false
	}
	return int(v), true
}

// report reads `report:`, a mapping of junit and sarif.
func (p *checksParser) report(path config.Path, n *yaml.Node) (CheckReport, bool) {
	switch {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
		return CheckReport{}, false
	case n.Kind != yaml.MappingNode:
		p.fault(path, n, config.ErrShape, "%s, where a mapping of junit and sarif belongs — each the path "+
			"of a file the check writes", shapeOf(n))
		return CheckReport{}, false
	}
	var r CheckReport
	ok := true
	p.mapping(path, n, func(key string, keyNode, value *yaml.Node) {
		field := under(path, key)
		var read bool
		switch key {
		case "junit":
			r.JUnit, read = p.text(field, value)
		case "sarif":
			r.SARIF, read = p.text(field, value)
		default:
			p.fault(field, keyNode, config.ErrUnknownField, "a check's report is %s",
				strings.Join(reportKeys, " or "))
		}
		ok = ok && read
	})
	return r, ok
}

// checkout reads `checkout:`: the scalar none, or a list of a cone's
// directories.
func (p *checksParser) checkout(path config.Path, n *yaml.Node) (CheckCheckout, bool) {
	switch {
	case n.Kind == yaml.AliasNode:
		p.alias(path, n)
		return CheckCheckout{}, false
	case n.Kind == yaml.SequenceNode && len(n.Content) == 0:
		p.fault(path, n, config.ErrShape, "an empty list is a cone of no directory: leave checkout out for "+
			"the whole tree, or write none for no checkout at all")
		return CheckCheckout{}, false
	case n.Kind == yaml.SequenceNode:
		cone, ok := p.texts(path, n)
		return CheckCheckout{Cone: cone}, ok
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" && n.Value == "none":
		return CheckCheckout{None: true}, true
	}
	p.fault(path, n, config.ErrShape, "%s where a list of the directories to check out belongs — or "+
		"none, for no checkout", shapeOfValue(n))
	return CheckCheckout{}, false
}

// shapeOf words what a node is, for a fault about its shape.
func shapeOf(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.AliasNode:
		return "an alias"
	case yaml.ScalarNode:
		if n.ShortTag() == "!!null" {
			return "empty"
		}
		return strconv.Quote(n.Value)
	}
	return "empty"
}

// shapeOfValue is [shapeOf], naming the type YAML read a scalar as too.
func shapeOfValue(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode && n.ShortTag() != "!!null" {
		return fmt.Sprintf("%s, read as %s,", strconv.Quote(n.Value), strings.TrimPrefix(n.ShortTag(), "!!"))
	}
	return shapeOf(n)
}

// defFault is one rule a definition breaks, at its field below the check.
type defFault struct {
	path   config.Path
	kind   error
	detail string
}

// defFaults is every rule d breaks — the one statement of them, which
// [ParseChecks] locates in the file and [CheckDef.Validate] reports by field.
func defFaults(d CheckDef) []defFault {
	var out []defFault
	add := func(path config.Path, kind error, detail string, args ...any) {
		out = append(out, defFault{path: path, kind: kind, detail: fmt.Sprintf(detail, args...)})
	}
	if !config.ValidCodeCheckName(d.Name) {
		add(config.Path{"name"}, config.ErrUnknownValue, "%q is not a check name: a lower-case letter or "+
			"digit, then up to %d lower-case letters, digits, dots, underscores and hyphens", d.Name,
			MaxCheckNameBytes-1)
	}
	switch run := d.Run; {
	case strings.TrimSpace(run) == "":
		add(config.Path{"run"}, config.ErrMissing, "a check runs a command, and this one is blank")
	case len(run) > MaxCheckRunBytes:
		add(config.Path{"run"}, config.ErrOutOfRange, "%d bytes, past the %d a check's command may hold "+
			"(code.MaxCheckRunBytes); a longer script belongs in the repository, where it is reviewed",
			len(run), MaxCheckRunBytes)
	case !utf8.ValidString(run) || strings.IndexByte(run, 0) >= 0:
		add(config.Path{"run"}, config.ErrShape, "a check's command is text a shell is handed whole, "+
			"which holds no NUL and no byte that is not UTF-8")
	}
	if ceiling := int(CheckTimeoutCeiling / time.Second); d.TimeoutSeconds < 1 || d.TimeoutSeconds > ceiling {
		add(config.Path{"timeout_seconds"}, config.ErrOutOfRange, "%d seconds: a check runs for 1 to %d "+
			"seconds (code.CheckTimeoutCeiling, six hours)", d.TimeoutSeconds, ceiling)
	}
	if len(d.Paths) > MaxCheckPaths {
		add(config.Path{"paths"}, config.ErrOutOfRange, "%d globs, past the %d a check names "+
			"(code.MaxCheckPaths)", len(d.Paths), MaxCheckPaths)
	}
	for i, glob := range d.Paths {
		if _, err := gitref.ParsePathGlob(glob); err != nil {
			add(config.Path{"paths", i}, config.ErrUnknownValue, "%q is not a path glob: %v", glob, err)
		} else if j := slices.Index(d.Paths[:i], glob); j >= 0 {
			add(config.Path{"paths", i}, config.ErrConflict, "%q is paths[%d] too", glob, j)
		}
	}
	for _, r := range []struct{ key, path string }{{"junit", d.Report.JUnit}, {"sarif", d.Report.SARIF}} {
		if r.path == "" {
			continue
		}
		if err := ValidTreePath(r.path); err != nil {
			add(config.Path{"report", r.key}, config.ErrUnknownValue, "%q is not a path inside the "+
				"checkout: %v", r.path, err)
		}
	}
	if d.Report.JUnit != "" && d.Report.JUnit == d.Report.SARIF {
		add(config.Path{"report", "sarif"}, config.ErrConflict, "%q is the junit report too; one file is "+
			"read as one report", d.Report.SARIF)
	}
	if d.RunIn != "" && !d.RunIn.Valid() {
		if why, refused := refusedCells[d.RunIn]; refused {
			add(config.Path{"run_in"}, config.ErrUnknownValue, "a check never runs in %s: %s", d.RunIn, why)
		} else {
			add(config.Path{"run_in"}, config.ErrUnknownValue, "%q is not one of %v", d.RunIn, CheckCells())
		}
	}
	switch co := d.Checkout; {
	case co.None && len(co.Cone) > 0:
		add(config.Path{"checkout"}, config.ErrConflict, "a checkout is none or a cone, never both")
	case len(co.Cone) > sandbox.MaxConePaths:
		add(config.Path{"checkout"}, config.ErrOutOfRange, "%d directories, past the %d a box's cone "+
			"names (sandbox.MaxConePaths)", len(co.Cone), sandbox.MaxConePaths)
	}
	for i, dir := range d.Checkout.Cone {
		if why := sandbox.ConePathProblem(dir); why != "" {
			add(config.Path{"checkout", i}, config.ErrUnknownValue, "%q %s", dir, why)
		} else if j := slices.Index(d.Checkout.Cone[:i], dir); j >= 0 {
			add(config.Path{"checkout", i}, config.ErrConflict, "%q is checkout[%d] too", dir, j)
		}
	}
	if len(d.Env) > MaxCheckEnv {
		add(config.Path{"env"}, config.ErrOutOfRange, "%d names, past the %d a check is given "+
			"(code.MaxCheckEnv)", len(d.Env), MaxCheckEnv)
	}
	for i, name := range d.Env {
		if !validEnvName(name) {
			add(config.Path{"env", i}, config.ErrUnknownValue, "%q is not a variable name: a letter or an "+
				"underscore, then letters, digits and underscores, at most %d bytes — the key of an entry "+
				"in the company's code.checks.env", name, MaxCheckEnvNameBytes)
		} else if j := slices.Index(d.Env[:i], name); j >= 0 {
			add(config.Path{"env", i}, config.ErrConflict, "%q is env[%d] too", name, j)
		}
	}
	return out
}

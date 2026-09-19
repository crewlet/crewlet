// Package adr is the gate behind the architecture decision records in adr/.
//
// # Why a record needs a gate at all
//
// This repository has kept a register of architectural decisions before. It
// was docs/reference/design-decisions.md, it carried fifteen titled decisions
// with their reasoning, it was linked from the documentation index as "why
// certain architectural choices were made" — and nothing in the tree referred
// to it, no checklist mentioned it, and by the time anybody compared it with
// the code it asserted that full-text search was unavailable (the engine ships
// its own BM25 index), that Confluence was the knowledge implementation (the
// engine ships its own pages) and that task state lived in an external tool
// under a heading that said so (the native tracker is the default).
//
// None of that is a formatting problem, and no template field would have
// caught it. What the register lacked was an ANCHOR: a reason for anybody
// changing the code to encounter it, and a check that noticed when the two
// stopped agreeing.
//
// So every record names an AUTHORITY — the one package or page whose own doc
// holds the detail — and that authority's doc names the record back. Both
// directions are checked:
//
//   - a record whose authority does not carry its id fails, because a record
//     nothing points at is a record nobody reads;
//   - a doc carrying an id no record declares fails, because a dangling
//     pointer reads exactly like a live one. Three package docs in this tree
//     cited internal/projection for months after the package was deleted.
//
// The anchor is what puts the decision in front of the person about to violate
// it: `go doc ./internal/statelog` prints ADR-0002 on its first screen.
//
// # What the gate cannot do, and why Enforced-by is the load-bearing field
//
// It can prove the anchor exists. It cannot prove the sentence is true —
// [internal/statelog]'s own vocabulary gate says the same of itself, "a grep
// cannot see a PARAPHRASE… the list is a floor". So the field that carries the
// weight is Enforced-by, which names the test that holds the decision, or the
// literal "nothing".
//
// "nothing" is a legitimate answer and is why [Unenforced] exists. An honest
// register of which decisions are held by prose alone is worth more than a
// register that pretends they all have gates — and the entries are checked in
// BOTH directions, so a decision that acquires a gate and forgets to leave the
// list fails, the same way internal/skipgate's Always entries do.
package adr

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Dir is where the records live, relative to the module root.
//
// ROOT-LEVEL, beside CONTRIBUTING.md and SECURITY.md, rather than under docs/.
// Two reasons, and the second is the one that decides it. docs/ is written for
// people RUNNING Crewlet and is published to docs.crewlet.ai; reasoning aimed
// at people CHANGING it is maintainer material by the same test CONTRIBUTING.md
// applies to every other page. And the documentation site is rendered by a
// separate repository that pulls this tree, so a page's navigation requirement
// is enforced somewhere no gate here can see — a constraint worth not importing
// for a directory whose readers all have a checkout.
const Dir = "adr"

// Unenforced is every decision whose Enforced-by is "nothing", with why.
//
// ADDING AN ENTRY IS THE DECISION, so it belongs in a diff somebody reviews
// rather than in a count somebody raises — the same reasoning, and the same
// two-sided shape, as internal/skipgate's allowance table. A count would go
// green the moment one decision acquired a gate and another lost one.
//
// Before adding an entry, try to answer the question it is standing in for:
// what would a test have to look at to catch a violation? The four gates that
// exist over this tree's architectural rules were all written after somebody
// asked that about a rule that had just been broken.
var Unenforced = []Exemption{
	{
		ID: "ADR-0009",
		Why: "A gate would have to observe two nodes applying one pointer and " +
			"converging on the same epoch, which is a property of a running " +
			"fleet rather than of the source. The nearest static formulation " +
			"— no configuration is applied from a consumed message — would " +
			"forbid a shape nothing currently writes, so it would certify " +
			"nothing. internal/configplane's suite covers the posture " +
			"arithmetic and the reconcile cadence; what is uncovered is the " +
			"delivery mechanism the decision rejected.",
	},
	{
		ID: "ADR-0010",
		Why: "The engine's exporter and the sandbox forwarder agree because " +
			"they read the same standard environment variables directly, " +
			"which is the point — so there is no shared helper for a walk to " +
			"hold them against. A test that both resolve one endpoint from " +
			"one environment is the right gate and is worth building the day " +
			"a third reader appears; with two, it would assert what the " +
			"variable names already do.",
	},
}

// Exemption is one record that no gate holds, and the case for that.
type Exemption struct {
	// ID is the record's number, as in "ADR-0009".
	ID string

	// Why must say what a gate would have to look at, and why that is not
	// reachable — not merely that the decision is hard to check. "It is a
	// judgement call" is not a reason; "the violation is a sentence in a
	// prompt, and no walk over source reaches what a model was asked" is.
	Why string
}

// Record is one decision as the gate reads it.
type Record struct {
	// ID is "ADR-0002", derived from the file name.
	ID string

	// Number is the same thing as an integer, for the ordering checks.
	Number int

	// File is the record's name within [Dir].
	File string

	// Title is the text after the em dash on the heading line.
	Title string

	// Fields are the header's key/value lines, keyed as written.
	Fields map[string]string
}

// Authority is the package or page whose doc must name this record back.
func (r Record) Authority() string { return r.Fields["Authority"] }

// EnforcedBy is the test, compile error or the literal "nothing".
func (r Record) EnforcedBy() string { return r.Fields["Enforced-by"] }

// Status is "accepted" or "superseded".
func (r Record) Status() string { return r.Fields["Status"] }

// Required is the field set every record carries.
//
// Measured and Cost-when-tried are deliberately NOT here: a decision that had
// nothing to measure should say nothing rather than write "n/a", which is a
// field somebody filled in to get past a check. The four below are the ones
// whose absence makes a record unusable — the last of them because a decision
// whose enforcement nobody stated is the exact shape this package exists for.
var Required = []string{"Status", "Authority", "Enforced-by", "Tag-status"}

// fileName matches a record's file: four digits, a dash, a slug.
var fileName = regexp.MustCompile(`^(\d{4})-[a-z0-9]+(?:-[a-z0-9]+)*\.md$`)

// headerLine matches one field of the header block.
var headerLine = regexp.MustCompile(`^- \*\*([A-Za-z][A-Za-z-]*):\*\* (.+)$`)

// heading matches the record's own title line.
var heading = regexp.MustCompile(`^# (ADR-(\d{4})) — (.+)$`)

// Reference matches a record's id wherever it is cited.
var Reference = regexp.MustCompile(`\bADR-(\d{4})\b`)

// Load reads every record under root/[Dir].
//
// The template is skipped by NUMBER rather than by name: it is 0000, which is
// the one number a real record cannot take, so a template renamed or copied
// badly shows up as a malformed record instead of being silently ignored.
func Load(root string) ([]Record, error) {
	dir := filepath.Join(root, Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("adr: read %s: %w", Dir, err)
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("adr: %s is not named NNNN-a-short-title.md, "+
				"so its number cannot be read and the index cannot order it", e.Name())
		}
		n, _ := strconv.Atoi(m[1])
		if n == 0 {
			continue // the template
		}
		rec, err := parse(filepath.Join(dir, e.Name()), e.Name(), n)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b Record) int { return a.Number - b.Number })
	return out, nil
}

// parse reads one record's heading and header block.
//
// THE HEADER STOPS AT THE FIRST NON-FIELD LINE, so a "- **Note:**" bullet in
// the body cannot become a field. A record whose header is interrupted is a
// record whose later fields are invisible, which is why the heading and the
// block are read together rather than by grepping the whole file.
func parse(path, name string, number int) (Record, error) {
	f, err := os.Open(path) //nolint:gosec // a path this package built from its own directory listing
	if err != nil {
		return Record{}, fmt.Errorf("adr: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	rec := Record{Number: number, File: name, Fields: map[string]string{}}
	scan := bufio.NewScanner(f)
	inHeader := false
	for scan.Scan() {
		line := strings.TrimRight(scan.Text(), " \t")
		if rec.ID == "" {
			m := heading.FindStringSubmatch(line)
			if m == nil {
				if strings.HasPrefix(line, "# ") {
					return Record{}, fmt.Errorf("adr: %s opens with %q; the "+
						"heading is `# ADR-NNNN — Title`, with an em dash",
						name, line)
				}
				continue
			}
			if m[2] != fmt.Sprintf("%04d", number) {
				return Record{}, fmt.Errorf("adr: %s is numbered %s in its "+
					"heading and %04d in its file name — one of the two is "+
					"what a reader will cite", name, m[2], number)
			}
			rec.ID, rec.Title = m[1], m[3]
			continue
		}
		if m := headerLine.FindStringSubmatch(line); m != nil {
			inHeader = true
			key := m[1]
			if _, dup := rec.Fields[key]; dup {
				return Record{}, fmt.Errorf("adr: %s sets %s twice, so one of "+
					"the two values is unread", name, key)
			}
			rec.Fields[key] = strings.TrimSpace(strings.Trim(m[2], "`"))
			continue
		}
		if inHeader && strings.TrimSpace(line) != "" {
			break
		}
	}
	if err := scan.Err(); err != nil {
		return Record{}, fmt.Errorf("adr: read %s: %w", name, err)
	}
	if rec.ID == "" {
		return Record{}, fmt.Errorf("adr: %s has no `# ADR-NNNN — Title` "+
			"heading, so nothing can cite it", name)
	}
	return rec, nil
}

// AuthorityPath turns an authority into the file or directory whose doc must
// carry the record's id, and says whether it is a Go package.
//
// Two shapes, because a decision's detail lives in one of two places here: a
// package doc for anything the engine does, and a published page for anything
// an operator reads. Nothing else is accepted, so an authority that is neither
// fails loudly rather than being checked against a file that does not exist.
func AuthorityPath(authority string) (path string, isPackage bool, err error) {
	switch {
	case strings.HasPrefix(authority, "internal/"), strings.HasPrefix(authority, "cmd/"):
		return filepath.FromSlash(authority), true, nil
	case strings.HasSuffix(authority, ".md"):
		return filepath.FromSlash(authority), false, nil
	}
	return "", false, fmt.Errorf("adr: %q is not an authority this gate can "+
		"check: name a package under internal/ or cmd/, whose doc comment "+
		"carries the record's id, or a .md page that does", authority)
}

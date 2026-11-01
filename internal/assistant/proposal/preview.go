package proposal

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/words"
)

// THE PREVIEW: what a write tool's call would change, read by the server from
// the object as it is now, never from what the model said about it.

// Read sends one read through the turn's grant, as the person.
type Read func(ctx context.Context, r exec.Request) (exec.Response, error)

// Previewer is a write tool's preview: it reads the object's current values
// through read and answers what the call would change and the exact request
// it would send, as of now (a relative date such as "+7d" is resolved on it).
// A catalogue descriptor provides one per write tool and page action.
type Previewer func(ctx context.Context, read Read, now time.Time, args map[string]any) (Preview, error)

// Revisions re-reads, through read, the revision a [Revision] names now: what
// an approval compares the preview's revision against. Answering the same
// string means the object has not moved.
type Revisions func(ctx context.Context, read Read, sourceRevision string) (string, error)

// Preview is one write as the person is shown it, and the request it sends.
type Preview struct {
	// Title is the action in sentence case ("Create task"), Target what it
	// changes ("ENG-142"), Summary what it does where the title does not say
	// it (what a destructive change takes away), and Note its consequence in
	// one sentence (whether it can be reverted).
	Title   string
	Target  string
	Summary string
	Note    string

	// Risk is how much care the approval asks, computed from what the
	// change does ([wire.RiskDestructive]); empty for an ordinary change.
	Risk wire.ProposalRisk

	// Technical is the exact request in the API's own terms, for an
	// approver who administers the company; nil for everybody else.
	Technical *wire.TechnicalRequest
	// ApproveLabel names the effect on the approve button ("Approve and
	// create"), never "OK".
	ApproveLabel string

	// Changes are the rows, built with [Fact], [Changed], [Added],
	// [Removed] and [Text]; [Rows] orders them.
	Changes []wire.Change

	// Audience is who will see the result where the write changes who can
	// see something: a share, an unshare, a comment others are notified of
	// (decision D09).
	Audience []string

	// StepUp is a write whose approval asks the person to confirm it is
	// them: a verb whose row asks a recent proof, or a share to the whole
	// company (decision D09).
	StepUp bool

	// SourceRevision is the object's revision the preview read
	// ([Revision.String]): a generic create's is the absence it read
	// ([RevisionCreate]), and it is empty for a write bound to nothing the
	// preview read (a POST, which creates a member of a collection).
	SourceRevision string

	// Request is the exact request the approval sends, with no operation
	// key ([New] mints it) and no precondition ([RequestOf] derives it from
	// SourceRevision).
	Request exec.Request

	// Act is the verb the request is decided by, for a tool that executes
	// on whichever route its call names (a generic API change): what the
	// approval asks the step-up of and the audit names. Empty for every
	// other tool, whose own act tool or verb it is.
	Act string

	// Removes are the fields a replacing write removes from the object as it
	// is now, by label: what Summary leads the card with, so a removal is
	// never only a struck-through row among the others. Computed from the
	// body and the representation the source revision names, both of which
	// the digest binds.
	Removes []string
}

// Check refuses a preview no card is drawn from: a write with no title, no
// approve label, no change or no request, one that changes visibility and
// names no audience, and a request carrying its own operation key.
func (p Preview) Check(changesVisibility bool) error {
	switch {
	case strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.ApproveLabel) == "":
		return fmt.Errorf("%w: a preview names its title and its approve label", ErrInvalid)
	case len(p.Changes) == 0:
		return fmt.Errorf("%w: a preview shows at least one change", ErrInvalid)
	case p.Request.Method == "" || p.Request.Path == "":
		return fmt.Errorf("%w: a preview names the request it sends", ErrInvalid)
	case p.Request.IdempotencyKey != "":
		return fmt.Errorf("%w: the operation key is minted with the proposal, never "+
			"by its preview", ErrInvalid)
	case p.Request.IfMatch != "" || p.Request.IfNoneMatch != "":
		return fmt.Errorf("%w: a write's precondition is derived from its source "+
			"revision, never set by its preview", ErrInvalid)
	case changesVisibility && len(p.Audience) == 0:
		return fmt.Errorf("%w: a write that changes who sees something names its "+
			"audience (D09)", ErrInvalid)
	}
	for _, c := range p.Changes {
		if !c.Kind.Valid() || c.Field == "" || c.Label == "" {
			return fmt.Errorf("%w: change %q is a kind of %s with a field and a label",
				ErrInvalid, c.Field, words.Or(wire.ChangeKinds))
		}
	}
	return nil
}

// Fact is a plain row: the scope, the target, a consequence.
func Fact(field, label, value string) wire.Change {
	return wire.Change{Kind: wire.ChangeFact, Field: field, Label: label, To: value}
}

// Changed is a value that moves from one value to another.
func Changed(field, label, from, to string) wire.Change {
	return wire.Change{Kind: wire.ChangeChanged, Field: field, Label: label, From: from, To: to}
}

// Added is a value set where there was none. isNew marks a field the record
// did not carry before.
func Added(field, label, to string, isNew bool) wire.Change {
	return wire.Change{Kind: wire.ChangeAdded, Field: field, Label: label, To: to, New: isNew}
}

// Removed is a value that is cleared.
func Removed(field, label, from string) wire.Change {
	return wire.Change{Kind: wire.ChangeRemoved, Field: field, Label: label, From: from}
}

// Text is a long text shown as a word diff, with the whole change behind a
// disclosure: From and To are the whole values, Words the runs between them.
func Text(field, label, from, to string) wire.Change {
	return wire.Change{Kind: wire.ChangeText, Field: field, Label: label, From: from, To: to,
		Words: WordDiff(from, to)}
}

// LongTextRunes is where a value stops being a value and becomes a text: past
// it, or across a line, a struck old value and a new one side by side are two
// paragraphs the person compares by eye, and a word diff shows what moved.
const LongTextRunes = 80

// Value is the row for one field moving from from to to, by what moved: an
// added, a removed, a changed or (for a long or multi-line value) a text row.
// Equal values are no change, and answer false.
func Value(field, label, from, to string) (wire.Change, bool) {
	switch {
	case from == to:
		return wire.Change{}, false
	case from == "":
		return Added(field, label, to, false), true
	case to == "":
		return Removed(field, label, from), true
	case long(from) || long(to):
		return Text(field, label, from, to), true
	}
	return Changed(field, label, from, to), true
}

// List is the row for a list of plain values moving from from to to: the
// items ride beside the joined line ([wire.Change.Items]), so a card draws a
// list as a list rather than one comma-joined line, and never as a word diff,
// which would read a list's order as prose. Equal lists are no change.
func List(field, label string, from, to []string) (wire.Change, bool) {
	if slices.Equal(from, to) {
		return wire.Change{}, false
	}
	was, is := strings.Join(from, ", "), strings.Join(to, ", ")
	switch {
	case len(from) == 0:
		row := Added(field, label, is, false)
		row.Items = to
		return row, true
	case len(to) == 0:
		row := Removed(field, label, was)
		row.FromItems = from
		return row, true
	}
	row := Changed(field, label, was, is)
	row.FromItems, row.Items = from, to
	return row, true
}

// long reports a value shown as a text.
func long(s string) bool {
	return utf8.RuneCountInString(s) > LongTextRunes || strings.ContainsRune(s, '\n')
}

// kindOrder is D10's order: what the change is about first, then what moves.
var kindOrder = []wire.ChangeKind{
	wire.ChangeFact, wire.ChangeChanged, wire.ChangeAdded, wire.ChangeRemoved, wire.ChangeText,
}

// Rows orders changes by D10's grammar, facts first and texts last, keeping
// each kind's own order.
func Rows(changes []wire.Change) []wire.Change {
	out := slices.Clone(changes)
	slices.SortStableFunc(out, func(a, b wire.Change) int {
		return slices.Index(kindOrder, a.Kind) - slices.Index(kindOrder, b.Kind)
	})
	return out
}

// MaxDiffTokens bounds the word diff: past it on either side the comparison
// would be a quadratic table of millions of cells for one card, so the change
// is shown as the old text removed and the new one added, which is still
// exactly what will be written.
const MaxDiffTokens = 2000

// WordDiff is the runs of words from from to to: what both keep, what is
// removed and what is added, each run's text verbatim with the whitespace
// after its words, so the kept and removed runs join to from and the kept and
// added runs join to to.
func WordDiff(from, to string) []wire.Word {
	a, b := tokens(from), tokens(to)
	if len(a) > MaxDiffTokens || len(b) > MaxDiffTokens {
		var out []wire.Word
		if from != "" {
			out = append(out, wire.Word{Op: wire.WordRemoved, Text: from})
		}
		if to != "" {
			out = append(out, wire.Word{Op: wire.WordAdded, Text: to})
		}
		return out
	}
	// lcs[i][j] is the longest common run of a[i:] and b[j:].
	lcs := make([][]uint16, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]uint16, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []wire.Word
	add := func(op wire.WordOp, text string) {
		if n := len(out); n > 0 && out[n-1].Op == op {
			out[n-1].Text += text
			return
		}
		out = append(out, wire.Word{Op: op, Text: text})
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			add(wire.WordSame, a[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			add(wire.WordRemoved, a[i])
			i++
		default:
			add(wire.WordAdded, b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		add(wire.WordRemoved, a[i])
	}
	for ; j < len(b); j++ {
		add(wire.WordAdded, b[j])
	}
	return out
}

// tokens splits s into words, each carrying the whitespace after it, and the
// whitespace before the first word as a token of its own.
func tokens(s string) []string {
	var out []string
	start, inSpace := 0, true
	for i, r := range s {
		space := unicode.IsSpace(r)
		if !space && inSpace && i > start {
			out = append(out, s[start:i])
			start = i
		}
		inSpace = space
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

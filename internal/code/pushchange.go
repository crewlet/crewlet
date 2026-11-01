package code

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// A CHANGE OPENED BY A PUSH: `git push -o change` opens a change on the one
// branch the push sets, IN THE PUSH'S OWN RECORD. The push entry carries the
// change its decide opens ([PushEntry.Change]) and the record's apply writes
// the change's rows beside the refs the push moves (revisions.go), so the
// change and the commit it proposes land together or not at all, on every
// data node, and no second record is ever half of it.
//
// # Where the options come from
//
// git hands its proc-receive hook the options the client sent (`-o`, or
// push.pushOption), each one line, after the ref updates: what receive-pack
// read and acted on, never the client's own bytes beside it. So the relay
// reads them there (internal/code/host's session) and hands them to the
// push's landing, which reads a change from them here ([ParseChangeOptions]).
// The CHANGE NAMESPACE is the code host's: `change`, which asks for one, and
// the `change.<key>` options, which shape it and do nothing without it, so a
// target a person keeps in their push.pushOption configuration opens nothing
// until a push asks. Every other option is a forge's or a CI's (GitLab's
// ci.skip) and read by nobody here.
//
// # A change's options never fail its push
//
// A push lands or not by its commands alone: options that are no change's, a
// push that does not single out one branch, and a change its decide refuses —
// no live target, the source at its target's commit — leave the push to land
// as one that asked for nothing, and its pusher is told in one line what
// became of the change ([ChangeAnswer]). A push refused for a typo in an
// option would send its pusher to push the same commits again, and a coding
// run's push refused for its change would lose the work it delivers.
//
// # Open it, or name the change the push moves
//
// A push's change is opened from its ONE branch command ([ChangeSource]) —
// one push, one proposal — at the head that command set, into the target the
// options name or the repository's default branch, by its pusher, who is its
// owner and first author, and the push itself is its first revision. A
// branch heads one live change at a time, so a push to a branch heading one
// already NAMES that change — its own move revised it, as every push to its
// source does — and opens none; options shape only a change a push opens. Who
// may open one is whoever may push its source, which the push's own command,
// decided ok under the same grant, has already shown.

// The keys of the change namespace, as a person spells them after -o.
const (
	pushOptionChange      = "change"
	pushOptionTarget      = "change.target"
	pushOptionTitle       = "change.title"
	pushOptionDescription = "change.description"
	pushOptionDraft       = "change.draft"
)

// ChangeOptionKeys is every option of the change namespace, as a push spells
// it — `change`, which asks for a change, and the four that shape it — in the
// order the guide lists them.
func ChangeOptionKeys() []string {
	return []string{pushOptionChange, pushOptionTarget + "=<branch>", pushOptionTitle + "=<title>",
		pushOptionDescription + "=<text>", pushOptionDraft}
}

// ChangeOption is the change a push's options ask for: what its receiver
// proposes beside the push ([Proposal.Change]) and what an entry whose decide
// opens it carries into the record ([PushEntry.Change]).
//
// BASE FORMAT, AS A STATED CONDITION, as [PushEntry.Change] states it: every
// field below rides that one field and takes no row of its own.
type ChangeOption struct {
	// Target is the branch the change proposes its source into, as a ref
	// (refs/heads/main). A proposal leaves it empty to ask for the
	// repository's default branch, which its decide stamps from the snapshot
	// it decides in, so an entry always names it.
	Target string `json:"target,omitempty"`
	// Title is one line, at most [MaxChangeTitleBytes]: the option's, or the
	// subject of the commit the push set its source to, which the receiver
	// fills in where the options name none.
	Title string `json:"title" untrusted:"name"`
	// Body is the change's description, at most [MaxChangeBodyBytes] — one
	// line, since git takes no line break in a push option.
	Body string `json:"body,omitempty" untrusted:"fence"`
	// Draft opens it as a draft, not yet asking to be merged.
	Draft bool `json:"draft,omitempty"`
}

// ParseChangeOptions reads the change a push's options ask for, the options
// as git handed them to the push's hook, in their order. It answers nil and
// no error for a push that asks for none — no `change` option, whatever else
// of the namespace it carries — and an error, whose words are its pusher's
// to read, for options that are no change's: a key of the namespace this
// engine does not read, a key given twice, `change` or `change.draft` given a
// value, or a value its field refuses — a target that is no branch a change
// may name, a title that is empty or past [MaxChangeTitleBytes], a
// description past [MaxChangeBodyBytes]. The change it answers has no Title
// where the options named none, which its receiver fills in, and no Target
// where they named none, which its decide does.
//
// NO BOUND OF ITS OWN ON THE OPTIONS: every key at most once and every value
// held to its field's bound make a change's options at most five, of 33 343
// bytes at their longest, so a count or a size checked here would refuse
// nothing these rules do not; and what a push's options can make a node hold
// at all is bounded where receive-pack reads them, by the command section's
// bound (internal/code/host's MaxCommandSectionBytes), before any process
// holds one.
func ParseChangeOptions(options []string) (*ChangeOption, error) {
	type option struct {
		key, value string
		valued     bool
	}
	var ours []option
	asked := false
	for _, o := range options {
		key, value, valued := strings.Cut(o, "=")
		if key != pushOptionChange && !strings.HasPrefix(key, pushOptionChange+".") {
			continue
		}
		asked = asked || key == pushOptionChange
		ours = append(ours, option{key, value, valued})
	}
	if !asked {
		return nil, nil
	}
	opt := &ChangeOption{}
	seen := make(map[string]bool, len(ours))
	for _, o := range ours {
		key := optionName(o.key)
		if seen[o.key] {
			return nil, fmt.Errorf("%s is given twice: a change takes each of its options once", key)
		}
		seen[o.key] = true
		switch o.key {
		case pushOptionChange, pushOptionDraft:
			if o.valued {
				return nil, fmt.Errorf("%s takes no value: give it as -o %s", key, key)
			}
			opt.Draft = opt.Draft || o.key == pushOptionDraft
		case pushOptionTarget:
			if o.value == "" {
				return nil, fmt.Errorf("%s names the branch the change proposes into, as -o %s=main", key, key)
			}
			ref := o.value
			if !strings.HasPrefix(ref, branchPrefix) {
				ref = branchPrefix + ref
			}
			if len(ref) > MaxRefNameBytes {
				return nil, fmt.Errorf("%s: %d bytes, past the %d a ref holds", key, len(ref), MaxRefNameBytes)
			}
			if why := branchProblem(ref); why != "" {
				return nil, fmt.Errorf("%s: %s", key, why)
			}
			opt.Target = ref
		case pushOptionTitle:
			if !o.valued {
				return nil, fmt.Errorf("%s names the change in one line, as -o %s=<title>", key, key)
			}
			if why := TitleProblem(o.value); why != "" {
				return nil, fmt.Errorf("%s: %s", key, why)
			}
			opt.Title = o.value
		case pushOptionDescription:
			if !o.valued {
				return nil, fmt.Errorf("%s describes the change, as -o %s=<text>", key, key)
			}
			if why := textProblem(o.value, MaxChangeBodyBytes, "change's description"); why != "" {
				return nil, fmt.Errorf("%s: %s", key, why)
			}
			opt.Body = o.value
		default:
			return nil, fmt.Errorf("%s is no option of a change, which takes %s", key,
				strings.Join(ChangeOptionKeys(), ", "))
		}
	}
	return opt, nil
}

// optionName is an option's key as an answer names it: whole up to 64 bytes —
// every key the namespace reads is under 20 — and past that cut with a
// marker, so a key a client spelled a mebibyte long is not echoed back whole;
// and QUOTED, escapes and all, where it holds anything but printable text,
// since the answer is a line on the pusher's terminal, and a key carrying
// a terminal's escape sequence would otherwise be written to it raw.
func optionName(key string) string {
	key = textcut.Within(key, 64)
	if !utf8.ValidString(key) || strings.ContainsFunc(key, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return strconv.Quote(key)
	}
	return key
}

// validFields holds a proposal's change to what its receiver may propose
// ([Proposal.Validate]): a target that is a branch a change may name, or
// none; a title, which the receiver fills in where the options name none;
// and a description within its bound.
func (o *ChangeOption) validFields() error {
	if o.Target != "" {
		if why := branchProblem(o.Target); why != "" {
			return badField("change.target", "%s", why)
		}
	}
	if why := TitleProblem(o.Title); why != "" {
		return badField("change.title", "%s — the receiver fills in the source's subject where the options "+
			"name none", why)
	}
	if why := textProblem(o.Body, MaxChangeBodyBytes, "change's description"); why != "" {
		return badField("change.body", "%s", why)
	}
	return nil
}

// measured is o as an entry carrying it is measured before its decide has
// stamped it ([entryBound]): its target at its longest spelling where the
// decide is to fill in the repository's default branch — a branch's ref of
// [MaxRefNameBytes], every byte one JSON spells in six — and nil for none.
func (o *ChangeOption) measured() *ChangeOption {
	if o == nil {
		return nil
	}
	m := *o
	if m.Target == "" {
		m.Target = branchPrefix + strings.Repeat("<", MaxRefNameBytes-len(branchPrefix))
	}
	return &m
}

// ChangeSource is the one command of cmds a push's change is opened from —
// the push's one command that SETS A BRANCH, a create or an update under
// refs/heads/, whatever became of it — or, where there is no such one, the
// refusal and its sentence: no_source for a push that sets no branch,
// ambiguous_source for one that sets several. Every command the push sent
// counts, the ones its receiver or its decide refused among them: which
// branch a change proposes is its asker's to say by what they push, never
// what survived the push's checks.
func ChangeSource(cmds []Command) (int, ChangeRefusal, string) {
	source := -1
	var branches []string
	for i, c := range cmds {
		if c.New == ZeroOID || !strings.HasPrefix(c.Ref, branchPrefix) {
			continue
		}
		source = i
		branches = append(branches, c.Ref)
	}
	switch len(branches) {
	case 0:
		return -1, ChangeRefusalNoSource, "this push sets no branch, and a change is opened from the one " +
			"branch its push creates or moves (git push -o change origin <branch>)"
	case 1:
		return source, "", ""
	}
	named := words.AndMore(branches, 2)
	return -1, ChangeRefusalAmbiguousSource, fmt.Sprintf("this push sets %d branches (%s), and a change "+
		"proposes one: push the change's branch alone with -o change", len(branches), named)
}

// pushOpen decides what p's change comes to, its commands as this round
// decided them (cmds), over the working state the batch's entries before it
// left (w) and the changes its own entry's moves leave (book): the change its
// entry carries — its target stamped — where it opens one, and its answer.
// Pure over what it is handed. In this order:
//
//  1. its source — the push's one branch command ([ChangeSource]), which its
//     receiver judged — moved its branch: else no_source, since a change
//     proposes a branch its push set;
//  2. the source heads no live change once the entry's moves are made: else
//     the push NAMES the one it heads, which its own move revised, and opens
//     none — a branch heads one live change at a time;
//  3. its target — the branch its options name, else the repository's
//     default branch — is another branch than its source (same_ref), live
//     once the entry has applied (no_target), and at another commit than the
//     source's new head (no_difference): the rules an open of a change's
//     lifecycle is held to ([decideOpen]), in its order, on the state this
//     push leaves.
//
// WHO MAY OPEN IT is whoever may push its source — what an open is held to,
// a fast-forward of the branch ([Grant.RefRule]) — which the source's own
// command, found ok under the same grant this round (the decide's rule 2b),
// has already shown: a fast-forward of a branch needs no more than a create
// or an update of it does.
func pushOpen(p Proposal, cmds []Command, w *working, book *changeBook, repo pushRepo) (*ChangeOption,
	*ChangeAnswer) {

	refuse := func(code ChangeRefusal, detail string) (*ChangeOption, *ChangeAnswer) {
		return nil, &ChangeAnswer{Refusal: code, Detail: detail}
	}
	i, r, why := ChangeSource(cmds)
	if r != "" {
		// A PROPOSAL VALIDATE REFUSES — its receiver answers a push of no
		// branch or of several itself — answered as that receiver answers
		// it, never a decide's panic on an index.
		return refuse(r, why)
	}
	source := cmds[i]
	switch source.Result {
	case ResultNoop:
		return refuse(ChangeRefusalNoSource, fmt.Sprintf("%s is already at %s, so this push moved no branch to "+
			"open a change from", source.Ref, short(source.New)))
	case ResultNG:
		return refuse(ChangeRefusalNoSource, fmt.Sprintf("this push's update of %s was refused (%s), so it moved "+
			"no branch to open a change from", source.Ref, source.Reason))
	}
	if live, ok := book.liveOn(source.Ref); ok {
		return nil, &ChangeAnswer{ChangeID: live.id, Number: live.number}
	}
	target := changeTarget(p.Change, repo)
	named := target
	if p.Change.Target == "" {
		named += ", its default branch,"
	}
	head := w.after(cmds, target)
	switch {
	case target == source.Ref:
		return refuse(ChangeRefusalSameRef, fmt.Sprintf("%s is both the change's source and its target: a change "+
			"proposes one branch into another — name its target with -o %s=<branch>", target, pushOptionTarget))
	case head == ZeroOID:
		return refuse(ChangeRefusalNoTarget, fmt.Sprintf("%s holds no live %s to merge into", repo.name, named))
	case head == source.New:
		return refuse(ChangeRefusalNoDifference, fmt.Sprintf("%s and %s are both at %s: the source holds nothing "+
			"its target does not", source.Ref, target, short(head)))
	}
	opened := *p.Change
	opened.Target = target
	return &opened, &ChangeAnswer{ChangeID: PushChangeID(p.PushID), Opened: true}
}

// PushChangeID is the id of the change push pushID opens from its options:
// DERIVED from the push's id (statelog.DeriveOpID, at the instant that id was
// minted), as [OpenChangeID] derives an open's from its operation id — so the
// apply of the push's record names the same change on every data node, and an
// answer that finds a change of that id on the repository knows this push
// opened it, whichever round of its batch decided the record. Its identity
// carries "push" beside the push's id, so no open of a change's lifecycle,
// whose operation id is its asker's, derives the same one.
func PushChangeID(pushID string) string {
	at, _ := statelog.OpMintedAt(pushID)
	return statelog.DeriveOpID(at, "", "code.change", "push", pushID)
}

// ChangeAnswer is what a push's change came to, beside the push's commands
// ([PushAnswer.Change]): the change it opened, the live change its source
// already headed — which the push's own move revised — or why it opened none.
// Never a refusal of the push.
type ChangeAnswer struct {
	// ChangeID is the change the push opened or named, "" where it did
	// neither.
	ChangeID string `json:"change_id,omitempty"`
	// Number is that change's number on its repository: read from its row
	// once the push's record applied, and for a pending or unknown push the
	// number of a change it named that existed before it — 0 where no row
	// has said it yet.
	Number int64 `json:"number,omitempty"`
	// Opened is a change the push opened; false beside a ChangeID, the live
	// change its source branch already headed.
	Opened bool `json:"opened,omitempty"`
	// Refusal is why the push opened none, one of [PushChangeRefusals], and
	// Detail its sentence.
	Refusal ChangeRefusal `json:"refusal,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

// PushChangeRefusals is every code a push's change can come to
// ([ChangeAnswer.Refusal]), in the order a push meets them: its receiver's —
// options that are no change's, a push of no branch or of several — and then
// its decide's, over the rows: a source that did not move, a target that is
// the source, no live target, a source at its target's commit.
func PushChangeRefusals() []ChangeRefusal {
	return []ChangeRefusal{ChangeRefusalBadOption, ChangeRefusalNoSource, ChangeRefusalAmbiguousSource,
		ChangeRefusalSameRef, ChangeRefusalNoTarget, ChangeRefusalNoDifference}
}

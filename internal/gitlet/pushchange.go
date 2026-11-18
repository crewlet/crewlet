package gitlet

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

// A change opened by a push, in the push's own record: the package doc's "A
// change is rows on its repository's subject" has the model.
//
// The options are read where git hands them to the proc-receive hook, after
// the ref updates (internal/gitlet/host's session): what receive-pack acted
// on, never the client's own bytes. `change` asks for a change and the
// `change.<key>` options shape it and do nothing without it, so a target kept
// in push.pushOption opens nothing until a push asks; every other option
// (GitLab's ci.skip) is read by nobody. A change's options NEVER FAIL ITS
// PUSH ([ChangeAnswer] says what became of it): a push refused for a typo
// would make its pusher push the same commits again, and an executor's push
// refused for its change would lose the work it delivers.

// The keys of the change namespace, as a person spells them after -o.
const (
	pushOptionChange      = "change"
	pushOptionTarget      = "change.target"
	pushOptionTitle       = "change.title"
	pushOptionDescription = "change.description"
	pushOptionDraft       = "change.draft"
)

// ChangeOptionKeys is every option of the change namespace as a push spells
// it, in the order the guide lists them (TestTheGuideNamesEveryChangeOption).
func ChangeOptionKeys() []string {
	return []string{pushOptionChange, pushOptionTarget + "=<branch>", pushOptionTitle + "=<title>",
		pushOptionDescription + "=<text>", pushOptionDraft}
}

// ChangeOption is the change a push's options ask for: proposed beside the
// push ([Proposal.Change]) and carried by the entry that opens it
// ([PushEntry.Change]). BASE FORMAT, as that field is: every field rides it
// and takes no row of its own.
type ChangeOption struct {
	// Target is the branch the change proposes into, as a ref. A proposal
	// leaves it empty for the repository's default branch, which its decide
	// stamps, so an entry always names it.
	Target string `json:"target,omitempty"`
	// Title is one line, at most [MaxChangeTitleBytes]: the option's, else
	// the source commit's subject, which the receiver fills in.
	Title string `json:"title" untrusted:"name"`
	// Body is the description, at most [MaxChangeBodyBytes]; one line, since
	// git takes no line break in a push option.
	Body string `json:"body,omitempty" untrusted:"fence"`
	// Draft opens it as a draft, not yet asking to be merged.
	Draft bool `json:"draft,omitempty"`
	// Reverts is the number of the change this one reverts, set by a
	// revert_change's build alone (an engine write's push), never by a push's
	// options; kept on code_changes.reverts. VERSION 9 ([RevertsVersion]).
	Reverts int64 `json:"reverts,omitempty"`
}

// ParseChangeOptions reads the change a push's options ask for, in the order
// git handed them to the push's hook: nil and no error where no `change`
// option asks for one, and an error worded for the pusher for options that
// are no change's — a key of the namespace it does not read, a key given
// twice, a value on `change` or `change.draft`, or a value its field refuses.
// Where the options name none, Title is left to the receiver and Target to
// the decide.
//
// It sets no count or size bound of its own: one of each key, each held to
// its field's bound, caps the options at five and 33 343 bytes, and what they
// can make a node hold is bounded earlier, where receive-pack reads them
// (internal/gitlet/host's MaxCommandSectionBytes).
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

// optionName is an option's key as an answer names it: cut past 64 bytes
// (every key the namespace reads is under 20), so a mebibyte-long key is not
// echoed whole, and QUOTED where it holds anything but printable text, since
// the answer is a line on the pusher's terminal and an escape sequence would
// otherwise reach it raw.
func optionName(key string) string {
	key = textcut.Within(key, 64)
	if !utf8.ValidString(key) || strings.ContainsFunc(key, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return strconv.Quote(key)
	}
	return key
}

// validFields holds a proposal's change to what its receiver may propose
// ([Proposal.Validate]): a nameable branch or no target, a title (the
// receiver's to fill in), and a description within its bound.
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

// measured is o as [entryBound] measures it before its decide stamps it, nil
// for none: a target left to the decide at a branch ref's longest
// ([MaxRefNameBytes]), and a title left to the receiver at
// [MaxChangeTitleBytes], every byte counted as the six JSON may spell it in.
//
// AN UNTITLED CHANGE IS MEASURED, never refused: the receiver asks
// [Proposal.ValidateCommands] for room before it reads the source commit's
// subject it titles the change with (read beside the upload, so no push waits
// on a git process before its bytes start for the object store), and that
// title is at most what this measured. Every other check holds the title to
// [ChangeOption.validFields] first, which refuses an empty one.
func (o *ChangeOption) measured() *ChangeOption {
	if o == nil {
		return nil
	}
	m := *o
	if m.Target == "" {
		m.Target = branchPrefix + strings.Repeat("<", MaxRefNameBytes-len(branchPrefix))
	}
	if m.Title == "" {
		m.Title = strings.Repeat("<", MaxChangeTitleBytes)
	}
	return &m
}

// ChangeSource is the index of the one command of cmds a push's change is
// opened from, the one that SETS A BRANCH (a create or update under
// refs/heads/), or -1 with the refusal and its sentence: no_source for none,
// ambiguous_source for several. Refused commands count too: which branch a
// change proposes is said by what its asker pushes, never by what survived
// the push's checks.
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

// pushOpen decides what p's change comes to, from cmds as this round decided
// them, the working state w earlier entries left and the book of changes this
// entry's moves leave: the change its entry carries, target stamped, and its
// answer. Pure. In order: (1) its source ([ChangeSource]) moved its branch,
// else no_source; (2) the source heads no live change, else the push NAMES
// that one, which its move revised, and opens none, since a branch heads one
// live change at a time; (3) its target, the options' or the default branch,
// is not the source (same_ref), is live (no_target) and is at another commit
// (no_difference): [decideOpen]'s rules in its order, on the state this push
// leaves. WHO MAY OPEN IT, whoever may fast-forward the source ([Grant.RefRule]),
// the source's own command already showed, ok under the same grant this round
// (the decide's rule 2b): a fast-forward needs no more than a create or update.
func pushOpen(p Proposal, cmds []Command, w *working, book *changeBook, repo pushRepo) (*ChangeOption,
	*ChangeAnswer) {

	refuse := func(code ChangeRefusal, detail string) (*ChangeOption, *ChangeAnswer) {
		return nil, &ChangeAnswer{Refusal: code, Detail: detail}
	}
	i, r, why := ChangeSource(cmds)
	if r != "" {
		// Validate refuses such a proposal and its receiver answers it;
		// answered the same here, never a panic on the index.
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

// PushChangeID is the id of the change push pushID opens, DERIVED from the
// push's id at its minting instant (statelog.DeriveOpID), as [OpenChangeID]
// derives an open's: every data node's apply names the same change, and an
// answer finding a change of that id knows this push opened it, whichever
// round decided the record. "push" in its identity keeps it from colliding
// with an open's, whose operation id is its asker's.
func PushChangeID(pushID string) string {
	at, _ := statelog.OpMintedAt(pushID)
	return statelog.DeriveOpID(at, "", "code.change", "push", pushID)
}

// ChangeAnswer is what a push's change came to ([PushAnswer.Change]): the
// change it opened, the live change its source already headed (which its
// move revised), or why it opened none. Never a refusal of the push.
type ChangeAnswer struct {
	// ChangeID is the change the push opened or named, "" for neither.
	ChangeID string `json:"change_id,omitempty"`
	// Number is that change's number, read from its row: 0 where no row has
	// said it yet, as for a change a pending or unknown push opened.
	Number int64 `json:"number,omitempty"`
	// Opened is a change the push opened; false beside a ChangeID, the live
	// change its source already headed.
	Opened bool `json:"opened,omitempty"`
	// Refusal is why the push opened none, one of [PushChangeRefusals], and
	// Detail its sentence.
	Refusal ChangeRefusal `json:"refusal,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

// PushChangeRefusals is every code a push's change can come to
// ([ChangeAnswer.Refusal]), in the order a push meets them: its receiver's,
// then its decide's over the rows.
func PushChangeRefusals() []ChangeRefusal {
	return []ChangeRefusal{ChangeRefusalBadOption, ChangeRefusalNoSource, ChangeRefusalAmbiguousSource,
		ChangeRefusalSameRef, ChangeRefusalNoTarget, ChangeRefusalNoDifference}
}

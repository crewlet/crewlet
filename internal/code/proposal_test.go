package code_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A REF IS RECORDED WHOLE, OR BOUNDED BY A DIGEST OF THE WHOLE: every ref a
// push may move — and every one refused for its shape alone — rides its record
// as itself; one past MaxRefNameBytes or not UTF-8 rides it within
// MaxRefNameBytes of UTF-8 that names its length and a digest of all of it, so
// two refs sharing an opening still differ, the form survives JSON as itself,
// and recording a recorded ref changes nothing.
func TestARefIsRecordedWholeOrBoundedByADigestOfTheWhole(t *testing.T) {
	t.Parallel()
	longest := "refs/heads/" + strings.Repeat("x", code.MaxRefNameBytes-len("refs/heads/"))
	for _, ref := range []string{"refs/heads/main", longest, "refs/heads/a..b", "refs/heads/&<>" + string(rune(0x2028))} {
		if got := code.RecordedRef(ref); got != ref {
			t.Errorf("a ref of %d bytes of UTF-8 is recorded as %q, want itself", len(ref), got)
		}
	}

	shared := "refs/heads/" + strings.Repeat("&", 400)
	bounded := []string{
		longest + "y",
		shared + "a",
		shared + "b",
		"refs/heads/" + strings.Repeat("&", 60_000),
		// A rune straddling every cut the form could make.
		"refs/heads/" + strings.Repeat("é", 200),
		"refs/heads/\xff",
		"refs/heads/\xfe",
	}
	seen := map[string]string{}
	for _, ref := range bounded {
		got := code.RecordedRef(ref)
		switch {
		case len(got) > code.MaxRefNameBytes || !utf8.ValidString(got):
			t.Errorf("a ref of %d bytes is recorded as %d bytes (UTF-8: %t), want at most %d of UTF-8",
				len(ref), len(got), utf8.ValidString(got), code.MaxRefNameBytes)
		case !strings.Contains(got, fmt.Sprintf(" [%d bytes, sha256 ", len(ref))):
			t.Errorf("a ref of %d bytes is recorded as %q, which names neither its length nor its digest",
				len(ref), got)
		case code.RecordedRef(got) != got:
			t.Errorf("recording the recorded form %q changed it", got)
		}
		if other, ok := seen[got]; ok {
			t.Errorf("refs of %d and %d bytes are both recorded as %q", len(other), len(ref), got)
		}
		seen[got] = ref
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var back string
		if err := json.Unmarshal(encoded, &back); err != nil || back != got {
			t.Errorf("the recorded form %q reads back from JSON as %q (%v)", got, back, err)
		}
	}
}

// A PROPOSAL CARRIES EVERY REF IN ITS RECORDED FORM: a command naming a ref
// past MaxRefNameBytes, or one that is not UTF-8, is refused by name — so
// nothing that reaches a record or a row is longer, or reads back as another
// ref — and the same command carrying the recorded form is taken.
func TestAProposalCarriesEveryRefInItsRecordedForm(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"refs/heads/" + strings.Repeat("&", 60_000), "refs/heads/\xff"} {
		bad := code.Command{Ref: ref, Old: code.ZeroOID, New: oidOf("B"), Result: code.ResultNG,
			Reason: code.RefusalBadRef}
		p := proposal("0190f3c4-0000-7000-8000-00000000c0de", bad, update("refs/heads/main", code.ZeroOID,
			oidOf("B")))
		if err := p.Validate(); !errors.Is(err, code.ErrInvalidProposal) ||
			!strings.Contains(err.Error(), "commands[0].ref") {
			t.Errorf("a proposal carrying a ref of %d bytes whole: %v, want ErrInvalidProposal naming it",
				len(ref), err)
		}
		p.Commands[0].Ref = code.RecordedRef(ref)
		if err := p.Validate(); err != nil {
			t.Errorf("a proposal carrying a ref of %d bytes recorded: %v", len(ref), err)
		}
	}
}

// THE COMMANDS' BOUND HOLDS WHATEVER A PUSH'S WORK ADDS: a push whose commands
// ValidateCommands admits stays within EntryRoom once its work has made it as
// large as it can — every command forced and refused under the longest code,
// a pack, the most introduced ids and the largest count, the longest
// principal and node — and one command more of the same is refused as too
// wide, typed with what the push takes and what one push may. Measured with
// refs JSON spells six times over, the shape that reaches the bound: plain
// refs at the longest take under half the room for every command a push may
// carry.
func TestTheCommandsBoundHoldsWhateverAPushsWorkAdds(t *testing.T) {
	t.Parallel()
	principal := code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
		Seat: strings.Repeat("s", 64)}
	node := strings.Repeat("n", 64)
	skeleton := func(n int, fill string) code.Proposal {
		cmds := make([]code.Command, n)
		for i := range cmds {
			ref := fmt.Sprintf("refs/heads/%04d", i)
			cmds[i] = code.Command{Ref: ref + strings.Repeat(fill, code.MaxRefNameBytes-len(ref)),
				Old: oidOf(fmt.Sprint("old", i)), New: oidOf(fmt.Sprint("new", i))}
		}
		return code.Proposal{Principal: principal, Node: node, Commands: cmds}
	}

	if err := skeleton(code.MaxCommandsPerPush, "x").ValidateCommands(); err != nil {
		t.Fatalf("MaxCommandsPerPush commands at the longest plain ref: %v", err)
	}

	// THE MOST COMMANDS OF THE COSTLIEST REF ONE PUSH IS ADMITTED WITH.
	admitted := 0
	for n := 1; n <= code.MaxCommandsPerPush; n++ {
		if skeleton(n, "&").ValidateCommands() != nil {
			break
		}
		admitted = n
	}
	if admitted == code.MaxCommandsPerPush {
		t.Fatalf("MaxCommandsPerPush commands at the costliest ref are admitted: the case reaches no bound")
	}
	var wide *code.TooWideError
	if err := skeleton(admitted+1, "&").ValidateCommands(); !errors.As(err, &wide) ||
		!errors.Is(err, code.ErrInvalidProposal) || wide.Bytes <= wide.Room || wide.Room != code.EntryRoom {
		t.Fatalf("%d commands at the costliest ref: %v, want a TooWideError past EntryRoom", admitted+1, err)
	}

	// Completed as far as its work can take it, it is still a proposal.
	p := skeleton(admitted, "&")
	p.PushID, p.RepoID, p.ReceivedAt = "0190f3c4-0000-7000-8000-000000000002", "0190f3c4-0000-7000-8000-00000000b0b0",
		harnessAt
	longest := code.Refusal("")
	for _, r := range code.Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	for i := range p.Commands {
		p.Commands[i].Forced, p.Commands[i].Result, p.Commands[i].Reason = true, code.ResultNG, longest
	}
	p.Commands[0] = update(p.Commands[0].Ref, p.Commands[0].Old, p.Commands[0].New)
	p.Commands[0].Forced = true
	p.Atomic = true
	for i := range code.MaxIntroduced {
		p.Introduced = append(p.Introduced, oidOf(fmt.Sprint("introduced", i)))
	}
	p.IntroducedCount = math.MaxInt
	p.Pack = suitePack("completed")
	p.Pack.PackID = p.PushID
	if err := p.Validate(); err != nil {
		t.Errorf("a push of %d commands ValidateCommands admitted, completed by the most its work adds: %v",
			admitted, err)
	}
}

// THE RECEIVER CHECKS AN UNSTORED PACK AS ITS COMMITTER WILL CHECK IT STORED:
// ValidateUnstored answers what Validate answers once the put has filled in
// the object's key and digest — a proposal both take, one past its record's
// room both refuse, a pack whose sizes lay out no bundle both refuse — and
// refuses a pack already stored, which Validate is the check of.
func TestAnUnstoredPackIsCheckedAsItWillBeStored(t *testing.T) {
	t.Parallel()
	stored := proposal("0190f3c4-0000-7000-8000-00000000c0de", update("refs/heads/main", code.ZeroOID,
		oidOf("B")))
	stored.Pack = suitePack("unstored")
	stored.Pack.PackID = stored.PushID
	unstored := func(p code.Proposal) code.Proposal {
		pack := *p.Pack
		pack.Object, pack.Hash = objstore.Key{}, ""
		p.Pack = &pack
		return p
	}
	if err := unstored(stored).ValidateUnstored(); err != nil {
		t.Errorf("an unstored pack its stored form validates: %v", err)
	}
	if err := stored.ValidateUnstored(); !errors.Is(err, code.ErrInvalidProposal) {
		t.Errorf("ValidateUnstored of a stored pack: %v, want ErrInvalidProposal", err)
	}

	badSizes := stored
	pack := *stored.Pack
	pack.IndexBytes++
	badSizes.Pack = &pack
	tooWide := stored
	for i := range 700 {
		ref := fmt.Sprintf("refs/heads/%04d", i)
		tooWide.Commands = append(tooWide.Commands, code.Command{
			Ref: ref + strings.Repeat("&", code.MaxRefNameBytes-len(ref)), Old: code.ZeroOID, New: oidOf("B")})
	}
	for name, p := range map[string]code.Proposal{"a pack laying out no bundle": badSizes, "too wide": tooWide} {
		stored, unstored := p.Validate(), unstored(p).ValidateUnstored()
		if stored == nil || unstored == nil || stored.Error() != unstored.Error() {
			t.Errorf("%s: stored %v, unstored %v — want one refusal", name, stored, unstored)
		}
	}
}

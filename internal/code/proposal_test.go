package code_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode"
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
// carry. And for a SEAT'S push, whose principal carries the run behind it,
// every field of the run at MaxProvenanceBytes of what JSON spells six times
// over — a coding box's run id at its one spelling, a UUID's, beside its
// coding agent at that bound — the way — the longest, and a coding box's —
// and the company day its committer stamps on the entry at its longest: the
// principal and the way are measured as their proposer stamped them, so the
// room they take is room the commands are not admitted to, and so is the
// day's, which no proposer sends.
//
// Mutations: measure the completed entry with the principal's run dropped
// (completed() clearing w.Principal.Run) — the seat's push is admitted
// commands its run then has no room beside, and its entry runs past EntryRoom;
// measure a seat's entry without its day — the entry its committer stamps
// outgrows the bound, red.
func TestTheCommandsBoundHoldsWhateverAPushsWorkAdds(t *testing.T) {
	t.Parallel()
	node := strings.Repeat("n", 64)
	costliest := strings.Repeat("&", code.MaxProvenanceBytes)
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: strings.Repeat("s", 64),
		Run: code.RunProvenance{TurnID: costliest, WorkBackend: costliest, WorkKey: costliest, Model: costliest}}
	box := seat
	box.Run.RunID, box.Run.CodingAgent = "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", costliest
	for _, arm := range []struct {
		name      string
		principal code.Principal
		via       code.PushVia
		// proposal is whether the completed push is one Validate takes
		// whole: one the engine built moves one ref, so one of hundreds of
		// commands is held to the record's budget alone.
		proposal bool
	}{
		{"a person's", code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
			Seat: strings.Repeat("s", 64)}, code.ViaGit, true},
		{"a seat's the engine built", seat, code.ViaEngine, false},
		{"a coding box's git received", box, code.ViaGit, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			if code.FitProvenance(costliest) != costliest {
				t.Fatalf("a run field of %d '&' is not one FitProvenance admits as it is", len(costliest))
			}
			skeleton := func(n int, fill string) code.Proposal {
				cmds := make([]code.Command, n)
				for i := range cmds {
					ref := fmt.Sprintf("refs/heads/%04d", i)
					cmds[i] = code.Command{Ref: ref + strings.Repeat(fill, code.MaxRefNameBytes-len(ref)),
						Old: oidOf(fmt.Sprint("old", i)), New: oidOf(fmt.Sprint("new", i))}
				}
				return code.Proposal{Principal: arm.principal, Via: arm.via, Node: node, Commands: cmds}
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

			// Completed as far as its work can take it, it is still a
			// proposal — or, for a seat, still an entry its record holds.
			p := skeleton(admitted, "&")
			p.PushID, p.RepoID, p.ReceivedAt = "0190f3c4-0000-7000-8000-000000000002",
				"0190f3c4-0000-7000-8000-00000000b0b0", harnessAt
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
			bound, err := code.EntryBound(p)
			switch {
			case err != nil:
				t.Fatalf("measure the completed push: %v", err)
			case bound > code.EntryRoom:
				t.Errorf("a push of %d commands ValidateCommands admitted, completed by the most its work adds, "+
					"takes %d bytes of its record, past EntryRoom (%d)", admitted, bound, code.EntryRoom)
			}
			// THE ENTRY ITS COMMITTER WRITES — a seat's stamped with the day
			// it is counted on, at its longest — within that bound.
			stamped, err := json.Marshal(code.EntryOf(p, code.LongestDay))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(stamped) + len(p.Commands)*code.CommandResultBound + 1; got > bound {
				t.Errorf("the entry its committer writes takes %d bytes of its record, past the %d it was "+
					"measured at", got, bound)
			}
			if !arm.proposal {
				return
			}
			if err := p.Validate(); err != nil {
				t.Errorf("a push of %d commands ValidateCommands admitted, completed by the most its work adds: %v",
					admitted, err)
			}
		})
	}
}

// engineBuilt is p built by the engine, carrying a one-object pack of
// packBytes bytes.
func engineBuilt(p code.Proposal, packBytes int64) code.Proposal {
	p = packed(p, harnessAt)
	p.Via = code.ViaEngine
	p.Pack.Size = packBytes + p.Pack.IndexBytes + p.Pack.ReverseBytes
	return p
}

// A PROPOSAL NAMES HOW ITS PACK WAS BUILT, and an engine-built one is one
// commit on one ref: a proposal naming no way, or one this build does not know,
// is the proposer's bug and refused naming via — by ValidateCommands too, whose
// measure would otherwise be short of the entry; a push the engine built moving
// two refs, or --atomic, or storing a pack past what a git node's build stores
// (code.MaxWritePackBytes, which only an engine-built push is held to), is
// refused naming what it broke; and a person's push the engine built of one
// command is a proposal. So is a seat's, whichever way it was built, since the
// decide judges a seat's grant — one naming the run that made it, which the
// engine stamps on every seat's push: a seat's push naming none is its
// proposer's bug, refused naming principal.run. A seat's push git received is
// its coding box's, which names its coding run and agent — one naming none is
// refused naming principal.run.run_id — and a push the engine built is a tool
// call's, whose run is its turn, so one naming a coding run is refused naming
// the same.
//
// Mutations: refuse every principal but an operator's again (validFields'
// refusal the decide's grant lifts) — the seat's pushes are refused, red; take
// a seat's push git received naming no coding run — its row would be one no
// read of the run finds, red.
func TestAProposalNamesHowItsPackWasBuilt(t *testing.T) {
	t.Parallel()
	one := proposal("0190f3c4-0000-7000-8000-00000000c0de", update("refs/heads/main", code.ZeroOID, oidOf("B")))
	two := proposal("0190f3c4-0000-7000-8000-00000000c0de", update("refs/heads/main", code.ZeroOID, oidOf("B")),
		update("refs/heads/other", code.ZeroOID, oidOf("C")))
	turn := code.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001"}
	box := code.RunProvenance{TurnID: turn.TurnID, RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b",
		CodingAgent: "claude-code"}
	for _, tc := range []struct {
		name  string
		p     code.Proposal
		names string // the field a refusal names; "" for a proposal
	}{
		{"a push git received", one, ""},
		{"a person's push the engine built", func() code.Proposal { p := one; p.Via = code.ViaEngine; return p }(), ""},
		{"a push naming no way", func() code.Proposal { p := one; p.Via = ""; return p }(), "via"},
		{"a push naming a way this build does not know", func() code.Proposal {
			p := one
			p.Via = "telepathy"
			return p
		}(), "via"},
		{"two refs the engine built", func() code.Proposal { p := two; p.Via = code.ViaEngine; return p }(),
			"commands"},
		{"one ref the engine built --atomic", func() code.Proposal {
			p := one
			p.Via, p.Atomic = code.ViaEngine, true
			return p
		}(), "atomic"},
		{"an engine build's pack at its bound", engineBuilt(one, code.MaxWritePackBytes), ""},
		{"an engine build's pack past its bound", engineBuilt(one, code.MaxWritePackBytes+1), "pack"},
		{"a pushed pack past an engine build's bound", func() code.Proposal {
			p := engineBuilt(one, code.MaxWritePackBytes+1)
			p.Via = code.ViaGit
			return p
		}(), ""},
		{"a seat's push the engine built", func() code.Proposal {
			p := one
			p.Via = code.ViaEngine
			p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: turn}
			return p
		}(), ""},
		{"a coding box's push git received", func() code.Proposal {
			p := two
			p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: box}
			return p
		}(), ""},
		{"a seat's push git received naming no coding run", func() code.Proposal {
			p := two
			p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: turn}
			return p
		}(), "principal.run.run_id"},
		{"a seat's push the engine built naming a coding run", func() code.Proposal {
			p := one
			p.Via = code.ViaEngine
			p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: box}
			return p
		}(), "principal.run.run_id"},
		{"a seat's push naming no run", func() code.Proposal {
			p := one
			p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer"}
			return p
		}(), "principal.run"},
		{"a token bound to nobody's push", func() code.Proposal {
			p := one
			p.Principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"}
			return p
		}(), "principal.seat"},
	} {
		err := tc.p.Validate()
		switch {
		case tc.names == "" && err != nil:
			t.Errorf("%s: %v, want a proposal", tc.name, err)
		case tc.names != "" && (!errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.names+":")):
			t.Errorf("%s: %v, want ErrInvalidProposal naming %s", tc.name, err, tc.names)
		}
	}
	for _, via := range []code.PushVia{"", "telepathy"} {
		p := one
		p.Via = via
		if err := p.ValidateCommands(); !errors.Is(err, code.ErrInvalidProposal) ||
			!strings.Contains(err.Error(), "via:") {
			t.Errorf("ValidateCommands of a push naming the way %q: %v, want ErrInvalidProposal naming via", via, err)
		}
	}
	if got := code.EntryOf(func() code.Proposal { p := one; p.Via = code.ViaEngine; return p }(), "").Via; got != code.ViaEngine {
		t.Errorf("a proposal's entry names the way %q, want the proposal's own", got)
	}
}

// A PROVENANCE FIELD IS FITTED, NEVER REFUSED: a work key copied from another
// system as it arrived — a GitLab key of a nested namespace's path past the
// bound, a key holding a newline, one that is not UTF-8 — comes back at most
// MaxProvenanceBytes of valid UTF-8 with no control character, a run carrying
// it is one Validate takes, two keys sharing their first 256 bytes come back
// different, a key already within the rules comes back as it is, and fitting a
// fitted key changes nothing.
//
// Mutation: FitProvenance answering s as it is — the long key fails Validate.
func TestAProvenanceFieldIsFittedNeverRefused(t *testing.T) {
	t.Parallel()
	// A merge request's key on a project eighteen namespaces deep: 300 bytes.
	nested := "acme/" + strings.Repeat("platform-group/", 17)
	gitlab := nested + strings.Repeat("s", 300-len(nested)-len("!12")) + "!12"
	shared := strings.Repeat("k", code.MaxProvenanceBytes)
	seen := map[string]string{}
	for _, key := range []string{gitlab, "ENG-12\nInjected: line", "ENG-\xff12", "ENG-12\x00",
		shared + strings.Repeat("a", 44), shared + strings.Repeat("b", 44), "ENG-12\u0085"} {
		got := code.FitProvenance(key)
		run := code.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001", WorkKey: got}
		switch {
		case len(got) > code.MaxProvenanceBytes || !utf8.ValidString(got) || strings.ContainsFunc(got, unicode.IsControl):
			t.Errorf("a key of %d bytes is fitted as %d bytes (UTF-8: %t): %q", len(key), len(got),
				utf8.ValidString(got), got)
		case run.Validate() != nil:
			t.Errorf("a run carrying the fitted %q is refused: %v", got, run.Validate())
		case !strings.Contains(got, fmt.Sprintf(" [%d bytes, sha256 ", len(key))):
			t.Errorf("a key of %d bytes is fitted as %q, which names neither its length nor its digest", len(key), got)
		case code.FitProvenance(got) != got:
			t.Errorf("fitting the fitted %q changed it", got)
		}
		if other, ok := seen[got]; ok {
			t.Errorf("keys %q and %q are both fitted as %q", other, key, got)
		}
		seen[got] = key
	}
	for _, key := range []string{"ENG-12", "acme/api#88", "日本-12", shared, "anthropic.claude-3-5-sonnet-20241022-v2:0"} {
		if got := code.FitProvenance(key); got != key {
			t.Errorf("a key within the rules, %q, is fitted as %q", key, got)
		}
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

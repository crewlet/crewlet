package gitlet

import (
	"testing"
)

// A PUSH A RULE REFUSES WHOLE IS AN ATTEMPT: a push git received every command
// of which is refused, one by a rule on who may move which ref, is an entry of
// the record that moves nothing, whose apply writes its row and its counters
// and no ref; one refused only as stale, and one the engine built, write
// nothing. A seat's attempt counts against its day, and one past the day is
// told its own refusal, never push_quota, and recorded nowhere. A decide-made
// attempt, which its receiver stored a pack for, names no pack and no commit,
// at the pack bound too.
//
// Mutations: record every push refused whole (the stale push is an entry,
// red); judge the quota before the attempt (push_quota, red); keep the pack on
// a decide-made attempt (a pack on the entry, red).
func TestAPushRefusedWholeByARuleIsAnAttempt(t *testing.T) {
	t.Parallel()
	locked := func(name string) Command { return cmd("refs/heads/locked", ZeroOID, oid(name)) }

	t.Run("a person's", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		refused := locked("L")
		refused.Result, refused.Reason, refused.Detail = ResultNG, RefusalProtectedDirect, "the receiver's sentence"
		p := proposalOf(refused)
		pushes := e.num(`SELECT pushes FROM code_repos WHERE id = ?`, decideRepo)
		entries, answers := decideIn(t, e, DefaultLimits(), p)
		if len(entries) != 1 || !answers[0].inRecord || answers[0].refused != "" {
			t.Fatalf("%d entries, in the record %v, refused %q: want the attempt alone in the record",
				len(entries), answers[0].inRecord, answers[0].refused)
		}
		got := entries[0]
		if got.PushID != p.PushID || got.Pack != nil || len(got.Commands) != 1 ||
			got.Commands[0].Reason != RefusalProtectedDirect || got.Commands[0].Detail != "" {
			t.Fatalf("the attempt's entry is %+v, want its one refused command, no sentence and no pack", got)
		}
		e.apply(pushOf(t, decideRepo, entries...))
		if n := e.num(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND pack_id = ''`, p.PushID); n != 1 {
			t.Fatalf("%d rows of the attempt with no pack, want one", n)
		}
		if n := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND ref = 'refs/heads/locked'`,
			decideRepo); n != 0 {
			t.Fatal("the attempt moved the ref it was refused")
		}
		if after := e.num(`SELECT pushes FROM code_repos WHERE id = ?`, decideRepo); after != pushes+1 {
			t.Fatalf("the repository counts %d pushes after the attempt, want %d", after, pushes+1)
		}
		e.countersMatchRows(decideRepo)
	})

	t.Run("no attempt", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		engine := proposalOf(locked("E"))
		engine.Via = ViaEngine
		for name, p := range map[string]Proposal{
			"stale":               proposalOf(cmd("refs/heads/gone", oid("Z"), oid("B"))),
			"built by the engine": engine,
			"a bad ref":           proposalOf(cmd("refs/heads/a..b", ZeroOID, oid("B"))),
			"an over-large blob": proposalOf(Command{Ref: "refs/heads/feature", Old: oid("F"), New: oid("G"),
				Result: ResultNG, Reason: RefusalBlobTooLarge, Detail: "a blob of 2 GiB"}),
		} {
			entries, answers := decideIn(t, e, DefaultLimits(), p)
			if len(entries) != 0 || answers[0].inRecord {
				t.Errorf("%s: %d entries, want none: only a rule's refusal of a push git received is an attempt",
					name, len(entries))
			}
		}
	})

	t.Run("a seat's", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		limits := DefaultLimits()
		limits.PushesPerSeatDay = 2
		entries, _ := decideSeats(t, e, limits, pushBy(seatPusher("dev"), locked("S1")))
		if len(entries) != 1 || entries[0].Day != "2031-04-02" {
			t.Fatalf("the seat's attempt: %d entries, want one counted on its day", len(entries))
		}
		e.apply(pushOf(t, decideRepo, entries...))
		if n := e.num(`SELECT pushes FROM code_seat_days WHERE seat = 'dev' AND day = '2031-04-02'`); n != 1 {
			t.Fatalf("the seat's day counts %d pushes after its attempt, want 1", n)
		}
		e.exec(`UPDATE code_seat_days SET pushes = 2 WHERE seat = 'dev'`)
		entries, answers := decideSeats(t, e, limits, pushBy(seatPusher("dev"), locked("S2")))
		got := answers[0]
		if len(entries) != 0 || got.inRecord || got.refused != "" || !Attempted(got.commands) {
			t.Fatalf("an attempt past the seat's day: %d entries, refused %q, its command %s; want its own "+
				"refusal and no entry", len(entries), got.refused, got.commands[0].Reason)
		}
	})

	t.Run("decide-made", func(t *testing.T) {
		t.Parallel()
		e := decideEstate(t)
		p := withPack(proposalOf(locked("D")), 64, decideAt)
		p.Introduced, p.IntroducedCount = []string{oid("D")}, 3
		for _, packs := range []int{0, MaxPacksPerRepo} {
			e.exec(`UPDATE code_repos SET live_packs = ? WHERE id = ?`, packs, decideRepo)
			entries, answers := decideIn(t, e, DefaultLimits(), p)
			if len(entries) != 1 || answers[0].refused != "" {
				t.Fatalf("at %d live packs: %d entries, refused %q; want the attempt", packs, len(entries),
					answers[0].refused)
			}
			if got := entries[0]; got.Pack != nil || len(got.Introduced) != 0 || got.IntroducedCount != 0 {
				t.Fatalf("at %d live packs the attempt names pack %v and %d commits, want none", packs, got.Pack,
					got.IntroducedCount)
			}
		}
	})
}

// ONLY A RULE'S REFUSAL OF EVERY COMMAND IS AN ATTEMPT: one ok or undecided
// command, or every refusal one of the client's view, the content's size or the
// fleet's, is none, and --atomic's refusal rides beside a rule's.
//
// Mutation: drop a code from the rules' set (that code's push is none, red).
func TestOnlyARulesRefusalOfEveryCommandIsAnAttempt(t *testing.T) {
	t.Parallel()
	ng := func(r Refusal) Command { return Command{Ref: "refs/heads/" + string(r), Result: ResultNG, Reason: r} }
	for _, r := range []Refusal{RefusalProtectedDirect, RefusalProtectedForce, RefusalProtectedDelete,
		RefusalTagMoved, RefusalReservedRef, RefusalNotPermitted, RefusalDefaultBranch} {
		if !Attempted([]Command{ng(r)}) || !Attempted([]Command{ng(RefusalAtomic), ng(r)}) {
			t.Errorf("a push refused %s whole is no attempt", r)
		}
		if Attempted([]Command{ng(r), {Ref: "refs/heads/ok", Result: ResultOK}}) ||
			Attempted([]Command{ng(r), {Ref: "refs/heads/noop", Result: ResultNoop}}) ||
			Attempted([]Command{ng(r), {Ref: "refs/heads/undecided"}}) {
			t.Errorf("a push refused %s beside a command that is not refused is an attempt", r)
		}
	}
	for _, r := range Refusals() {
		switch r {
		case RefusalProtectedDirect, RefusalProtectedForce, RefusalProtectedDelete, RefusalTagMoved,
			RefusalReservedRef, RefusalNotPermitted, RefusalDefaultBranch:
			continue
		}
		if Attempted([]Command{ng(r)}) {
			t.Errorf("a push refused %s whole is an attempt", r)
		}
	}
	if Attempted(nil) {
		t.Error("a push of no command is an attempt")
	}
}

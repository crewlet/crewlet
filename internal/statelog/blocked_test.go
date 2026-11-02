package statelog_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY BLOCK A WINDOW CAN CARRY HAS A SENTENCE OF ITS OWN, so no block
// reaches a person as its code.
//
// The dashboard's banner and both CLI pages printed `abandoned_by_operator`
// and `capacity_operation_unsealed` as the reason a window needed a person. The
// code is for scripts, and every surface now shows [statelog.BlockedDetail]
// instead. That holds only while every block has a sentence written for it, so
// this walks the blocks coord DECLARES, not a list beside them: a block added
// there and left out of [coord.MaintenanceBlocks] or of the sentences fails
// here rather than on a screen.
//
// Mutation: declare a block and give it no case, or leave it out of the list,
// and this fails; print a code inside a sentence, or an em dash, and so does
// this.
func TestEveryMaintenanceBlockHasASentenceOfItsOwn(t *testing.T) {
	t.Parallel()
	declared := declaredConstants(t, "../coord/maintenance.go", "MaintenanceBlock")
	if len(declared) == 0 {
		t.Fatal("found no MaintenanceBlock constant in coord — the walk reads nothing")
	}
	unknown := blockedOn("a_block_no_build_declares", coord.PhaseSealing)
	if unknown == "" {
		t.Fatal("a block this build doesn't know reads as no block at all")
	}
	if strings.Contains(unknown, "a_block_no_build_declares") {
		t.Errorf("a block this build doesn't know prints its code: %q", unknown)
	}
	for _, code := range declared {
		block := coord.MaintenanceBlock(code)
		if !slices.Contains(coord.MaintenanceBlocks, block) || !block.Valid() {
			t.Errorf("block %q is declared and missing from coord.MaintenanceBlocks", code)
		}
		for _, phase := range []coord.MaintenancePhase{coord.PhaseSealing, coord.PhaseSealed} {
			said := blockedOn(block, phase)
			switch {
			case said == "":
				t.Errorf("block %q at %s has no sentence", code, phase)
			case said == unknown:
				t.Errorf("block %q at %s is said as a block this build doesn't know",
					code, phase)
			}
			if strings.Contains(said, code) {
				t.Errorf("block %q's sentence prints its code: %q", code, said)
			}
			if strings.Contains(said, "—") {
				t.Errorf("block %q's sentence carries an em dash: %q", code, said)
			}
			if first := []rune(said); len(first) == 0 || !unicode.IsUpper(first[0]) ||
				!strings.HasSuffix(said, ".") {
				t.Errorf("block %q's sentence isn't one: %q", code, said)
			}
		}
	}
	if len(coord.MaintenanceBlocks) != len(declared) {
		t.Errorf("coord.MaintenanceBlocks has %d blocks and coord declares %d",
			len(coord.MaintenanceBlocks), len(declared))
	}
	if blockedOn("", coord.PhaseSealing) != "" {
		t.Error("a window that isn't blocked is given a reason")
	}
}

// AN ABANDONED WINDOW SAYS WHAT CLOSES IT, DECIDED BY ITS SEAL AND NEVER BY ITS
// PHASE, with every command as the CLI takes it.
//
// Abandoning puts a window back at `sealing` from wherever it was, so a window
// abandoned out of attempts, whose fleet was already in seal mode with its
// acknowledgements recorded, was told to restart every node with `-mode seal`,
// a fleet-wide restart that changed nothing: one set-capacity closes it. The
// seal's own rule says which nodes it still waits on. And the commands were
// named as no CLI takes them: `set-capacity` with no log or bytes, `abandon`
// with no `-stream` or `-confirm`, `-mode seal` with no `crewlet run`.
//
// Mutation: decide the clause by the phase, drop a command's arguments, or
// name the abandoning party (every surface prints them beside this), and this
// fails.
func TestAnAbandonedWindowSaysWhatClosesItBySeal(t *testing.T) {
	t.Parallel()
	setCapacity := "`crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 " +
		"-confirm 8589934592`"
	abandoned := func(phase coord.MaintenancePhase) coord.MaintenanceOperation {
		op := operation(phase)
		op.Blocked = coord.BlockAbandoned
		op.AbandonedBy = coord.MaintenanceParty{By: "ana.admin", OperatorID: "pat:x"}
		return op
	}
	for name, tc := range map[string]struct {
		phase   coord.MaintenancePhase
		acks    []coord.MaintenanceAck
		acksErr error
		says    []string
		never   []string
	}{
		"before any node restarted for the seal": {coord.PhaseSealing, nil, nil,
			[]string{"no longer trying to reach 8.0 GiB",
				`once "node-1" and "node-2" have restarted with ` +
					"`crewlet run -mode seal` and " + setCapacity + " runs again."},
			nil},
		// THE REVIEWED CASE: abandoned from `sealed` (out of attempts), back
		// at `sealing`, with the seal acknowledgements the fleet already wrote.
		"abandoned at sealing over a seal that holds": {coord.PhaseSealing, sealed(1), nil,
			[]string{"Its seal is waiting on no node, so " + setCapacity +
				" closes it at the ceiling the log has."},
			[]string{"-mode seal"}},
		"acknowledgements unread": {coord.PhaseSealing, nil, errors.New("store unreachable"),
			[]string{"once its seal holds and " + setCapacity + " runs again",
				"`crewlet retention maintenance status -stream CREWLET_TRACKER_LOG`"},
			[]string{"store unreachable"}},
		"sealed": {coord.PhaseSealed, nil, nil,
			[]string{"Run " + setCapacity + " to close it at the ceiling the log has."},
			[]string{"-mode seal"}},
	} {
		t.Run(name, func(t *testing.T) {
			said := statelog.BlockedDetail(abandoned(tc.phase), tc.acks, tc.acksErr)
			for _, want := range tc.says {
				if !strings.Contains(said, want) {
					t.Errorf("the sentence is %q, want it to say %q", said, want)
				}
			}
			for _, not := range append(tc.never, "ana.admin", "pat:x") {
				if strings.Contains(said, not) {
					t.Errorf("the sentence is %q, carrying %q", said, not)
				}
			}
		})
	}
}

// A WINDOW OUT OF ATTEMPTS NAMES THE WAYS OUT THAT WORK. Nothing sends the
// target again once the last attempt's verify failed, so "run set-capacity
// again" got the same block back. Abandoning it and running set-capacity from
// a node in seal mode closes it at the ceiling the log has. The verb's own
// refusal says the same sentence, so the two never disagree.
//
// Mutation: advise set-capacity alone, drop a command's arguments, or give the
// refusal words of its own, and this fails.
func TestAWindowOutOfAttemptsNamesTheWaysOutThatWork(t *testing.T) {
	t.Parallel()
	unsealed := operation(coord.PhaseSealed)
	unsealed.Attempt = statelog.ControlRaiseAttempts
	unsealed.Blocked, unsealed.ObservedMaxBytes = coord.BlockUnsealed, 4<<30
	said := statelog.BlockedDetail(unsealed, nil, nil)
	for _, want := range []string{"stopped after 3 attempts at 8.0 GiB",
		"the broker still reports 4.0 GiB, and it won't send the change again",
		"To close the window at 4.0 GiB, abandon it with `crewlet retention " +
			"maintenance abandon -stream CREWLET_TRACKER_LOG -confirm op-1`, then run " +
			"`crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm " +
			"8589934592` from a node started with `crewlet run -mode seal`.",
		"open a new window for it"} {
		if !strings.Contains(said, want) {
			t.Errorf("a window out of attempts says %q, want it to say %q", said, want)
		}
	}
	_, err := statelog.NextAttempt(unsealed, 4<<30, maintenanceAt)
	if told, ok := statelog.Tell(err); !ok || told != said {
		t.Errorf("the verb's refusal tells %q, want the sentence every surface "+
			"shows, %q", told, said)
	}
}

// blockedOn is the sentence for a block on an operation in a phase.
func blockedOn(block coord.MaintenanceBlock, phase coord.MaintenancePhase) string {
	op := operation(phase)
	op.Blocked = block
	return statelog.BlockedDetail(op, nil, nil)
}

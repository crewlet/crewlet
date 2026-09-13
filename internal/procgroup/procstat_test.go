package procgroup

import "testing"

// A real stat line from a process whose command name holds the two things that
// break a naive split: a space and a closing parenthesis. Counting fields from
// the first ")" instead of the last reads the wrong column as the start time.
const trickyStat = "4242 (my) worker (x)) S 1 4242 4242 0 -1 4194560 118 0 0 0 0 0 0 0 20 0 1 0 " +
	"987654 4235264 164 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"

func TestAStatLineIsReadFromItsLastParenthesis(t *testing.T) {
	proc, err := parseProcStat(trickyStat, "boot")
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if proc.Start != "boot:987654" {
		t.Fatalf("Start = %q, want %q: field 22 was not the column read", proc.Start, "boot:987654")
	}
	if proc.Zombie {
		t.Fatal("a sleeping process read as a zombie")
	}
}

func TestAZombieOrReleasedStateReadsAsZombie(t *testing.T) {
	for _, state := range []string{"Z", "X"} {
		line := "7 (sh) " + state + " 1 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 55 0 0 0\n"
		proc, err := parseProcStat(line, "boot")
		if err != nil {
			t.Fatalf("parseProcStat(%s): %v", state, err)
		}
		if !proc.Zombie {
			t.Errorf("state %s did not read as a zombie", state)
		}
	}
}

// The same process read in two boots must not compare equal, or a service that
// comes up at the same pid and tick every boot would match a job recorded
// before a reboot.
func TestTheBootIsPartOfTheStartTime(t *testing.T) {
	first, _ := parseProcStat(trickyStat, "boot-one")
	second, _ := parseProcStat(trickyStat, "boot-two")
	if first.Start == second.Start {
		t.Fatalf("one tick count in two boots produced one start time: %q", first.Start)
	}
}

func TestAMalformedStatLineIsRefused(t *testing.T) {
	for _, line := range []string{
		"",
		"4242 no-parenthesis S 1",
		"4242 (short) S 1 2 3",
		"4242 (bad) S 1 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 not-a-number 0\n",
	} {
		if proc, err := parseProcStat(line, "boot"); err == nil {
			t.Errorf("parseProcStat(%q) = %+v, want a refusal", line, proc)
		}
	}
}

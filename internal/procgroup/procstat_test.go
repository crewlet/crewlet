package procgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"testing"
)

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
	if proc.Group != 4242 {
		t.Fatalf("Group = %d, want 4242: field 5 was not the column read", proc.Group)
	}
	if proc.Zombie {
		t.Fatal("a sleeping process read as a zombie")
	}
}

func TestAZombieOrReleasedStateReadsAsZombie(t *testing.T) {
	for _, state := range []string{"Z", "X", "x"} {
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
		"4242 (bad) S 1 not-a-group 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 55 0\n",
	} {
		if proc, err := parseProcStat(line, "boot"); err == nil {
			t.Errorf("parseProcStat(%q) = %+v, want a refusal", line, proc)
		}
	}
}

// statLine builds a /proc/<pid>/stat line in the kernel's own layout, with the
// fields this package reads placed by their documented numbers (proc(5)) and
// every other field a plausible value, so a parser counting from the wrong
// place reads a wrong value rather than an accidentally right one.
func statLine(pid int, comm, state string, pgrp, threads int, start uint64) string {
	return fmt.Sprintf("%d (%s) %s 1 %d 31 34816 %d 4194560 118 0 0 0 7 3 0 0 20 0 %d 0 %d "+
		"4235264 164 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n",
		pid, comm, state, pgrp, pgrp+5, threads, start)
}

// The Linux parse, over literal lines, on every platform: nobody developing on
// darwin can run the Linux build, and the release runs it on every box.
func TestTheStatParseReadsStateGroupAndStartFromTheRightFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string
		group  int
		start  StartTime
		exited bool
	}{
		{"plain", statLine(100, "sleep", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"running", statLine(100, "node", "R", 90, 12, 5000), 90, "boot:5000", false},
		{"stopped", statLine(100, "node", "T", 90, 1, 5000), 90, "boot:5000", false},
		{"disk sleep", statLine(100, "git", "D", 90, 1, 5000), 90, "boot:5000", false},
		{"a space in the command", statLine(100, "Web Content", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"a closing parenthesis in the command", statLine(100, "a)b", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"an empty command", statLine(100, "", "S", 90, 1, 5000), 90, "boot:5000", false},
		// A process names itself, and this one names itself as the fields
		// that follow, claiming to be a zombie in group 7.
		{"a command forging a zombie", statLine(100, ") Z 1 7 7 0", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"a command forging a live process", statLine(100, ") S 1 90 90", "Z", 7, 1, 5000), 7, "boot:5000", true},
		{"a command of parentheses", statLine(100, "((()))", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"a newline in the command", statLine(100, "a\n) R 1 9", "S", 90, 1, 5000), 90, "boot:5000", false},
		{"zombie", statLine(100, "sh", "Z", 90, 1, 5000), 90, "boot:5000", true},
		{"released", statLine(100, "sh", "X", 90, 1, 5000), 90, "boot:5000", true},
		{"released, old spelling", statLine(100, "sh", "x", 90, 1, 5000), 90, "boot:5000", true},
		// The main thread called pthread_exit and two threads still run: the
		// process is not over.
		{"an exited main thread with threads running", statLine(100, "node", "Z", 90, 3, 5000), 90, "boot:5000", false},
		{"a group other than the pid", statLine(4242, "sleep", "S", 4000, 1, 1), 4000, "boot:1", false},
		{"a start past 32 bits", statLine(100, "sleep", "S", 90, 1, 1<<40), 90, "boot:1099511627776", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc, err := parseProcStat(tc.line, "boot")
			if err != nil {
				t.Fatalf("parseProcStat(%q): %v", tc.line, err)
			}
			if proc.Group != tc.group || proc.Start != tc.start || proc.Zombie != tc.exited {
				t.Fatalf("parseProcStat(%q) = group %d, start %q, exited %v; want group %d, start %q, exited %v",
					tc.line, proc.Group, proc.Start, proc.Zombie, tc.group, tc.start, tc.exited)
			}
		})
	}
}

// fakeProc is one entry of a [procTable] a test builds: a stat line, or the
// error reading it fails with.
type fakeProc struct {
	line string
	err  error
}

func fakeTable(entries map[string]fakeProc, listErr error) procTable {
	return procTable{
		list: func() ([]string, error) {
			if listErr != nil {
				return nil, listErr
			}
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			return names, nil
		},
		stat: func(pid int) ([]byte, error) {
			entry, ok := entries[strconv.Itoa(pid)]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(entry.line), entry.err
		},
		boot: func() (string, error) { return "boot", nil },
	}
}

// What the member scan makes of every kind of entry /proc can hold, including
// the ones a live table produces only by racing: a process reaped between the
// listing and its read is no member, while a table that cannot be listed, a
// record that does not parse, or an unreadable record with no living member
// found is "could not ask", never "no member".
func TestTheGroupScanCountsOnlyMembersThatHaveNotExited(t *testing.T) {
	const pgid = 500
	vanished := fakeProc{err: &fs.PathError{Op: "open", Path: "/proc/7/stat", Err: fs.ErrNotExist}}
	reapedMidRead := fakeProc{err: &fs.PathError{Op: "read", Path: "/proc/8/stat", Err: errNoProcess}}
	hidden := fakeProc{err: &fs.PathError{Op: "open", Path: "/proc/9/stat", Err: fs.ErrPermission}}
	for _, tc := range []struct {
		name    string
		entries map[string]fakeProc
		listErr error
		living  bool
		wantErr bool
	}{
		{name: "an empty table", entries: map[string]fakeProc{}},
		{name: "a living leader", entries: map[string]fakeProc{
			"500": {line: statLine(500, "sh", "S", pgid, 1, 1)},
		}, living: true},
		{name: "a living member under a reaped leader", entries: map[string]fakeProc{
			"501": {line: statLine(501, "sleep", "S", pgid, 1, 1)},
		}, living: true},
		{name: "only zombies", entries: map[string]fakeProc{
			"500": {line: statLine(500, "sh", "Z", pgid, 1, 1)},
			"501": {line: statLine(501, "sleep", "Z", pgid, 1, 1)},
			"502": {line: statLine(502, "sleep", "X", pgid, 1, 1)},
		}},
		{name: "a zombie member beside a living stranger", entries: map[string]fakeProc{
			"501": {line: statLine(501, "sleep", "Z", pgid, 1, 1)},
			"600": {line: statLine(600, "sleep", "S", 600, 1, 1)},
		}},
		{name: "a stranger whose command forges the group", entries: map[string]fakeProc{
			"600": {line: statLine(600, ") S 1 500 500", "S", 600, 1, 1)},
		}},
		{name: "a member whose main thread alone has exited", entries: map[string]fakeProc{
			"501": {line: statLine(501, "node", "Z", pgid, 4, 1)},
		}, living: true},
		{name: "entries that are not processes", entries: map[string]fakeProc{
			"self": {line: "garbage"}, "sys": {line: "garbage"}, "0": {line: "garbage"},
			"-500": {line: "garbage"}, "500x": {line: "garbage"},
		}},
		{name: "a process reaped between the listing and its read", entries: map[string]fakeProc{
			"7": vanished, "8": reapedMidRead,
			"501": {line: statLine(501, "sleep", "Z", pgid, 1, 1)},
		}},
		{name: "a table that cannot be listed", listErr: fs.ErrPermission, wantErr: true},
		{name: "a record that does not parse", entries: map[string]fakeProc{
			"501": {line: "501 (sleep) S 1"},
		}, wantErr: true},
		{name: "a record that fails for another reason", entries: map[string]fakeProc{
			"501": {err: &fs.PathError{Op: "read", Path: "/proc/501/stat", Err: errors.New("input/output error")}},
		}, wantErr: true},
		{name: "a hidden record and no living member", entries: map[string]fakeProc{
			"9":   hidden,
			"501": {line: statLine(501, "sleep", "Z", pgid, 1, 1)},
		}, wantErr: true},
		{name: "a hidden record beside a living member", entries: map[string]fakeProc{
			"9":   hidden,
			"501": {line: statLine(501, "sleep", "S", pgid, 1, 1)},
		}, living: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			living, err := fakeTable(tc.entries, tc.listErr).groupLiving(pgid)
			if (err != nil) != tc.wantErr || living != tc.living {
				t.Fatalf("groupLiving = %v, %v; want %v, error %v", living, err, tc.living, tc.wantErr)
			}
		})
	}
}

// Inspect's three answers from one record: found, no such process (the entry
// is gone, or the process was reaped mid-read), and could not ask.
func TestAStatRecordIsFoundAbsentOrUnknown(t *testing.T) {
	table := fakeTable(map[string]fakeProc{
		"10": {line: statLine(10, "sleep", "S", 10, 1, 42)},
		"11": {err: &fs.PathError{Op: "read", Path: "/proc/11/stat", Err: errNoProcess}},
		"12": {err: &fs.PathError{Op: "open", Path: "/proc/12/stat", Err: fs.ErrPermission}},
	}, nil)
	if proc, found, err := table.inspect(10); err != nil || !found || proc.Start != "boot:42" {
		t.Errorf("inspect of a live record = %+v, %v, %v; want its record", proc, found, err)
	}
	for _, pid := range []int{11, 13} {
		if proc, found, err := table.inspect(pid); err != nil || found {
			t.Errorf("inspect(%d) = %+v, %v, %v; want no such process", pid, proc, found, err)
		}
	}
	if _, found, err := table.inspect(12); err == nil || found {
		t.Errorf("inspect of an unreadable record = found %v, %v; want an error", found, err)
	}
	broken := table
	broken.boot = func() (string, error) { return "", errors.New("no boot id") }
	if _, found, err := broken.inspect(10); err == nil || found {
		t.Errorf("inspect with no boot id = found %v, %v; want an error", found, err)
	}
}

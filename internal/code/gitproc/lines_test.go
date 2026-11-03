package gitproc_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// collect is a Lines keeping a copy of every line.
func collect() (*gitproc.Lines, *[]string) {
	var got []string
	return gitproc.NewLines(func(line []byte) error {
		got = append(got, string(line))
		return nil
	}), &got
}

// A LINE IS A LINE however git's writes cut it: one line over three writes,
// three lines in one, and a carriage return is content, not an end.
func TestLinesSplitsAcrossWrites(t *testing.T) {
	t.Parallel()
	l, got := collect()
	for _, w := range []string{"ab", "c", "d\n", "e\nf\r\ng\n"} {
		if n, err := l.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("Write(%q) = %d, %v", w, n, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"abcd", "e", "f\r", "g"}; !slices.Equal(*got, want) {
		t.Fatalf("lines %q, want %q", *got, want)
	}
}

// OUTPUT THAT ENDS INSIDE A LINE IS REPORTED, never handed over as a line.
func TestLinesReportsAPartialLastLine(t *testing.T) {
	t.Parallel()
	l, got := collect()
	if _, err := l.Write([]byte("whole\npart")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); !errors.Is(err, gitproc.ErrPartialLine) {
		t.Fatalf("Close after a partial line = %v, want ErrPartialLine", err)
	}
	if want := []string{"whole"}; !slices.Equal(*got, want) {
		t.Fatalf("lines %q, want %q", *got, want)
	}
}

// THE FIRST REFUSAL STOPS THE RUN: fn's error ends the Write, git is cut off
// and the run ends — `yes` never stops on its own, so only the stop ends it.
func TestLinesStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()
	errStop := errors.New("first offender")
	seen := 0
	l := gitproc.NewLines(func([]byte) error {
		if seen++; seen == 3 {
			return errStop
		}
		return nil
	})
	if _, err := l.Write([]byte("a\nb\nc\nd\n")); !errors.Is(err, errStop) {
		t.Fatalf("Write past the refusal = %v, want fn's error", err)
	}
	if seen != 3 {
		t.Fatalf("fn saw %d lines after refusing the third", seen)
	}
	if err := l.Close(); !errors.Is(err, errStop) {
		t.Fatalf("Close = %v, want fn's error", err)
	}

	p := fakePool(t, "yes", gitproc.Options{})
	lines := gitproc.NewLines(func([]byte) error { return errStop })
	o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdout: lines, Class: gitproc.ClassRead, Deadline: time.Minute}), "a run whose lines were refused")
	if !errors.Is(o.err, gitproc.ErrOutput) || !errors.Is(o.err, errStop) {
		t.Fatalf("a run whose first line was refused: %v, want ErrOutput wrapping the refusal", o.err)
	}
}

// A LINE PAST MaxLine IS REFUSED, whether it arrives whole or in pieces.
func TestLinesRefusesAnOverlongLine(t *testing.T) {
	t.Parallel()
	for name, writes := range map[string][]string{
		"in one write":  {strings.Repeat("x", gitproc.MaxLine+1)},
		"in two writes": {strings.Repeat("x", gitproc.MaxLine), "x"},
		"ended":         {strings.Repeat("x", gitproc.MaxLine+1) + "\n"},
		"ended later":   {strings.Repeat("x", gitproc.MaxLine), "x\n"},
	} {
		l, _ := collect()
		var err error
		for _, w := range writes {
			if _, err = l.Write([]byte(w)); err != nil {
				break
			}
		}
		if !errors.Is(err, gitproc.ErrLineTooLong) {
			t.Errorf("%s: %v, want ErrLineTooLong", name, err)
		}
	}
	l, got := collect()
	if _, err := l.Write([]byte(strings.Repeat("x", gitproc.MaxLine) + "\n")); err != nil || len(*got) != 1 {
		t.Fatalf("a line of exactly MaxLine: %v", err)
	}
}

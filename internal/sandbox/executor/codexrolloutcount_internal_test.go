package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexMetered is codexMeter's reading of rollout packed as a Codex archive,
// written by release.
func codexMetered(t *testing.T, rollout []byte, release string) meterReading {
	t.Helper()
	read, err := Codex{}.meter(context.Background(), bytes.NewReader(codexArchive(t, rollout)), codexThread,
		release, new(passBudget))
	if err != nil {
		t.Fatalf("meter the archive: %v", err)
	}
	return read
}

// CODEX COUNTS A SESSION AT ITS NEWEST LAST TOTAL ALONE, as it does before a
// turn, which a carried session's next request opens: the newest
// token_count's last_token_usage.total_tokens (after a compaction, Codex's
// own estimate of the replaced history), never the cumulative total and
// never the items since (CODEX-MEASURE §16.4). The window is the model's own,
// the reported 95% scaled back; the meter's figure is the newest request's
// input, and the next request is estimated at the count.
//
// Mutation: anchor on total_token_usage or the first token_count, take the
// compaction request's 60000 for the history after it, add the note written
// after the anchor, or report the 95% window, and a figure goes wrong.
func TestCodexCountsASessionAtItsNewestLastTotalAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		fixture       string
		count, figure int64
		restart       string
	}{
		{"rollout.jsonl", 1290, 1234, ""},
		{"compaction-note-before.jsonl", 6070, 60000, "ordinal-25"},
		{"compaction-note-after.jsonl", 6053, 60000, "ordinal-18"},
	} {
		raw := codexRolloutFixture(t)
		if c.fixture != "rollout.jsonl" {
			raw = codexFixture(t, c.fixture)
		}
		read := codexMetered(t, raw, "0.171.0")
		n, ok := read.count.Bound()
		restart, known := read.count.Restarted()
		if !ok || n != c.count || read.tokens != c.figure || read.next.Tokens != c.count ||
			read.count.Window != 272000 || !known || restart != c.restart {
			t.Errorf("%s: count %d (%v) at %q (%v), figure %d, next %d, window %d; want %d at %q, figure %d",
				c.fixture, n, ok, restart, known, read.tokens, read.next.Tokens, read.count.Window,
				c.count, c.restart, c.figure)
		}
	}
}

// A CODEX COUNT IS MARKED WHERE THE ROLLOUT CANNOT STATE IT: a compaction
// after the newest figure (its history's figure not yet written), a session
// with no figure at all (the system prompt and tools are in no rollout), a
// release whose rule was not read, and an archive holding no one rollout;
// a marked count estimates no next request.
//
// Mutation: count across a compaction, count an unanchored session from its
// bytes alone, or read another release by this one's rule, and a row below
// reads a bound.
func TestACodexCountIsMarkedWhereTheRolloutCannotStateIt(t *testing.T) {
	t.Parallel()
	lines := strings.SplitAfter(string(codexFixture(t, "compaction-note-before.jsonl")), "\n")
	upToCompacted := ""
	for _, l := range lines {
		upToCompacted += l
		if strings.Contains(l, `"type":"compacted"`) {
			break
		}
	}
	for _, c := range []struct {
		name, rollout, release string
		mark                   sandbox.CountMark
	}{
		{"compacted after the figure", upToCompacted, "0.171.0", sandbox.CountCompacted},
		{"no figure", strings.Join(lines[:8], ""), "0.171.0", sandbox.CountUnanchored},
		{"another release", string(codexRolloutFixture(t)), "0.172.0", sandbox.CountRelease},
	} {
		read := codexMetered(t, []byte(c.rollout), c.release)
		if read.count == nil || read.count.Mark != c.mark || read.next.Tokens != 0 {
			t.Errorf("%s: the count reads %+v, next %d; want marked %s", c.name, read.count, read.next.Tokens, c.mark)
		}
	}
	read, err := codexMeter(context.Background(), strings.NewReader("not gzip"), "0.171.0", nil)
	if err == nil || read.count == nil || read.count.Mark != sandbox.CountUnread || read.count.Restart != nil {
		t.Errorf("an archive that is no gzip reads %+v (%v), want unread", read.count, err)
	}
	var none bytes.Buffer
	zw := gzip.NewWriter(&none)
	if err := tar.NewWriter(zw).Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := codexMeter(context.Background(), &none, "0.171.0", nil); !errors.Is(err, errCodexArchive) {
		t.Errorf("an archive with no rollout reads with %v, want errCodexArchive", err)
	}
}

package cache

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// Missing reports which of oids the repository's installed packs do not hold,
// in the order they were asked about, each once — what a fetch's wants hold
// that the cache lacks, which sends the fetch to one linearizable sync.
//
// One `git cat-file --batch-check --buffer` in s's directory over the ids, a
// line each: present objects print their type and size, absent ones
// `missing`.
// Every id must be an object id — 40 lowercase hex digits, not the zero id —
// and the first that is not is refused by name ([ErrInvalid]) before git
// runs, since what reaches cat-file's input is a revision expression to git.
// The input is the host's request, bounded by its request bound, and the
// deadline follows its size (gitproc.PackWorkDeadline, the pool's rule for a
// read proportional to its input). An empty list runs nothing.
func (c *Cache) Missing(ctx context.Context, s Synced, oids []string) ([]string, error) {
	if err := c.handedOut(s, "a check of missing objects"); err != nil {
		return nil, err
	}
	if c.isStopped() {
		return nil, ErrStopped
	}
	var asked []string
	seen := make(map[string]bool, len(oids))
	for _, oid := range oids {
		if !gitref.ValidOID(oid) || oid == gitref.ZeroOID {
			return nil, fmt.Errorf("%w: %q is not an object id: 40 lowercase hex digits, and not the zero id",
				ErrInvalid, oid)
		}
		if !seen[oid] {
			seen[oid] = true
			asked = append(asked, oid)
		}
	}
	if len(asked) == 0 {
		return nil, nil
	}
	stdin := strings.Join(asked, "\n") + "\n"
	var missing []string
	n := 0
	lines := gitproc.NewLines(func(line []byte) error {
		if n == len(asked) {
			return fmt.Errorf("cat-file answered more lines than the %d ids it was asked about", len(asked))
		}
		oid, rest, _ := bytes.Cut(line, []byte{' '})
		if string(oid) != asked[n] {
			return fmt.Errorf("cat-file answered %q for %s", line, asked[n])
		}
		if string(rest) == "missing" {
			missing = append(missing, asked[n])
		}
		n++
		return nil
	})
	// --buffer: no answer is needed before the next id is sent, and without
	// it git writes and flushes every line on its own — 14 ms against 22 ms
	// a thousand ids (gitproc's measured section), seconds at the hundred
	// thousand wants a ref-heavy clone may ask about, all of it silence on
	// the client's connection before upload-pack starts.
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Args: []string{"cat-file", "--batch-check", "--buffer"}, Config: c.ServeConfig(),
		Stdin: strings.NewReader(stdin), Stdout: lines, Class: gitproc.ClassRead,
		Deadline: gitproc.PackWorkDeadline(int64(len(stdin))),
	})
	if err == nil {
		err = lines.Close()
	}
	if err == nil && n != len(asked) {
		err = fmt.Errorf("cat-file answered %d lines for %d ids", n, len(asked))
	}
	if err != nil {
		return nil, fmt.Errorf("cache: which objects of %s are missing: %w", s.entry.id, err)
	}
	return missing, nil
}

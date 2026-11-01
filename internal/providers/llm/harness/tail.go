package harness

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// recordPoll is how often a record file is looked for and read while its run
// goes.
//
// A tenth of a second: far under a model request's own latency, so a request is
// reported long before the calls it asked for need it — the host waits two
// seconds for the record of a call's request before placing the call without
// it — and a stat and a read a tenth of a second cost a run nothing.
const recordPoll = 100 * time.Millisecond

// recordTail follows the file a CLI keeps its record in, for a profile whose
// record is not its stdout ([harnessprofile.SourceFile]).
//
// The file does not exist when the run starts — the CLI creates it — so it is
// looked for by its glob until it appears, and a file the glob already matched
// before the run is never taken for this run's. Where the glob cannot name the
// run's own file, owns says which is: it is handed a new file's lines from its
// first until one decides, and a file it refuses is another run's, passed over
// for good ([harnessprofile.RecordOwner]).
type recordTail struct {
	pattern string
	before  []string
	owns    func(line string) (decided, owned bool)
	foreign []string
	tee     *lineTee
	done    chan struct{}
}

// tailRecord starts following the first file pattern matches that did not
// exist when it was called — and which owns accepts, where owns is not nil —
// handing each complete line to line, until stop is closed, and then reads the
// file to its end, its last line included whether or not it ended in a newline.
// wait returns when that is done.
func tailRecord(pattern string, owns func(string) (bool, bool), line func(string), stop <-chan struct{}) (wait func()) {
	before, _ := filepath.Glob(pattern)
	t := &recordTail{
		pattern: pattern, before: before, owns: owns, done: make(chan struct{}),
		// The same bound a stdout record's lines have: one line past the
		// cap is skipped rather than held.
		tee: &lineTee{sink: io.Discard, on: line, limit: maxOutput, every: true},
	}
	go t.follow(stop)
	return func() { <-t.done }
}

func (t *recordTail) follow(stop <-chan struct{}) {
	defer close(t.done)
	ticker := time.NewTicker(recordPoll)
	defer ticker.Stop()
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	chunk := make([]byte, 64<<10)
	read := func() {
		if f == nil {
			f = t.open()
		}
		if f == nil {
			return
		}
		for {
			n, err := f.Read(chunk)
			if n > 0 {
				_, _ = t.tee.Write(chunk[:n])
			}
			if err != nil || n == 0 {
				return
			}
		}
	}
	for {
		select {
		case <-stop:
			read()
			// The last line, if the CLI ended it without a newline.
			if f != nil {
				_, _ = t.tee.Write([]byte("\n"))
			}
			return
		case <-ticker.C:
			read()
		}
	}
}

// open is the run's record file, once it exists.
func (t *recordTail) open() *os.File {
	matches, err := filepath.Glob(t.pattern)
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	for _, m := range matches {
		if slices.Contains(t.before, m) || slices.Contains(t.foreign, m) {
			continue
		}
		f, err := os.Open(m) //nolint:gosec // a profile glob rooted in the call's own home or work dir
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil
		}
		if t.owns == nil {
			return f
		}
		switch decided, owned := headOwned(f, t.owns); {
		case !decided:
			// The line that names its run is still to be written: asked
			// again at the next poll.
			_ = f.Close()
		case owned:
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				_ = f.Close()
				return nil
			}
			return f
		default:
			_ = f.Close()
			t.foreign = append(t.foreign, m)
		}
	}
	return nil
}

// maxOwnerHead bounds what is read of a file to find the line that names its
// run: a record's header, a few hundred bytes to a few kilobytes (Codex's
// session_meta carries its base instructions), or the few events before the
// one that names it (Kimi Code's wire log lists the tools the run was offered,
// schemas and all, before its profile.bind). A file whose head runs past it
// without deciding is read as undecided, and so is never taken.
const maxOwnerHead = 4 << 20

// headOwned hands f's complete lines to owns, from the first, until one
// decides, and reports what it decided — undecided where the lines written so
// far, or the bound, run out first.
func headOwned(f *os.File, owns func(string) (bool, bool)) (decided, owned bool) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	start := 0
	for len(buf) < maxOwnerHead {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.IndexByte(buf[start:], '\n')
			if i < 0 {
				break
			}
			if decided, owned := owns(string(buf[start : start+i])); decided {
				return true, owned
			}
			start += i + 1
		}
		if err != nil || n == 0 {
			return false, false
		}
	}
	return false, false
}

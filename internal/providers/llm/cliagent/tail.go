package cliagent

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
// record is not its stdout ([cliprofile.SourceFile]).
//
// The file does not exist when the run starts — the CLI creates it — so it is
// looked for by its glob until it appears, and a file the glob already matched
// before the run is never taken for this run's. Where the glob cannot name the
// run's own file, owns says which is: it is handed a new file's first line,
// and a file whose first line it refuses is another run's, passed over for
// good ([cliprofile.RecordOwner]).
type recordTail struct {
	pattern string
	before  []string
	owns    func(first string) bool
	foreign []string
	tee     *lineTee
	done    chan struct{}
}

// tailRecord starts following the first file pattern matches that did not
// exist when it was called — and whose first line owns accepts, where owns is
// not nil — handing each complete line to line, until stop is closed, and then
// reads the file to its end, its last line included whether or not it ended in
// a newline. wait returns when that is done.
func tailRecord(pattern string, owns func(string) bool, line func(string), stop <-chan struct{}) (wait func()) {
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
		switch first, whole := headLine(f); {
		case !whole:
			// Its first line is still being written: asked again at the
			// next poll.
			_ = f.Close()
		case t.owns(first):
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

// maxFirstLine bounds what is read of a file to find its first line: a record's
// first event is its header, a few hundred bytes to a few kilobytes (Codex's
// session_meta carries its base instructions). A file whose first line runs
// past it is read as not having one yet, and so is never taken.
const maxFirstLine = 1 << 20

// headLine is f's first line and whether it is whole — ended by a newline.
func headLine(f *os.File) (string, bool) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	for len(buf) < maxFirstLine {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			return string(buf[:i]), true
		}
		if err != nil || n == 0 {
			return "", false
		}
	}
	return "", false
}

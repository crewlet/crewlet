package cliagent

import (
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
// before the run is never taken for this run's.
type recordTail struct {
	pattern string
	before  []string
	tee     *lineTee
	done    chan struct{}
}

// tailRecord starts following the first file pattern matches that did not
// exist when it was called, handing each complete line to line, until stop is
// closed — and then reads the file to its end, its last line included whether
// or not it ended in a newline. wait returns when that is done.
func tailRecord(pattern string, line func(string), stop <-chan struct{}) (wait func()) {
	before, _ := filepath.Glob(pattern)
	t := &recordTail{
		pattern: pattern, before: before, done: make(chan struct{}),
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
		if slices.Contains(t.before, m) {
			continue
		}
		f, err := os.Open(m) //nolint:gosec // a profile glob rooted in the call's own home or work dir
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil
		}
		return f
	}
	return nil
}

package codingagent

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The live reading of a running job — what [Runner.Follow] begins, and what a
// person watching the run is shown.
//
// # The same two accounts Collect reads, settled as they grow
//
// The transcript the CLI's decoder builds out of its event stream, and the
// error stream for a job whose stream has said nothing yet (a CLI that failed
// to start). Each read takes only what was written since the last one — a
// ranged read of each stream's end, sized to what has grown — feeds the new
// complete lines through the same decoder for as long as the reading lasts,
// and SETTLES what it can show ([redact.Settled]): whole lines, redacted over
// everything written before them, never a line a later one could still
// change. So what a reading shows is an append-only text, which is what lets
// the node that owns the run send a viewer only the part it lacks
// (internal/sandbox, livefeed.go) — and it is exactly the whole text redacted,
// however the reads fell.
//
// # Where a reading begins, and why that is its name
//
// At the start of each stream's last [liveWindow] — all of a stream shorter
// than that — on a line. The reading's ORIGIN is that place, with the account
// it reads, so two owners reading the same stream from the same place name the
// same origin and derive the same text; a reading that has to begin again
// further on — the stream outran [liveBacklog] between two reads, or shrank —
// names another, and so does the switch from the error stream to the
// transcript the moment the transcript has something to say.

// liveWindow is how much of the END of a stream a reading begins from.
//
// One transcript line stands for one event whose raw size is dominated by what
// the event echoes — a tool's whole output on a stream that carries it (Claude
// Code's carries it twice: the result block, and the CLI's own copy beside it),
// tens of KiB for a file read or a long command. A mebibyte of stream is the
// last dozen or more such events whole, which is a screen of activity at once,
// in one ranged read — and a reading that began further back would show a
// person what the run did a while ago until it caught up.
const liveWindow = 1 << 20

// liveReadFloor is the least one read asks for beyond what the reading has not
// taken yet: the growth of a stream between two reads a couple of seconds
// apart, at the rate a coding agent writes when it is not echoing a file.
const liveReadFloor = 64 << 10

// liveBacklog is the most one read catches up. A stream that grew by more than
// this between two reads — a tool's echoed output of a large file — is read
// again from its end rather than in full, because the whole of it would cost a
// read of that size for one screen's worth of what it says.
const liveBacklog = 8 << 20

// Follow implements [sandbox.Runner].
func (r *Runner) Follow(sandbox.RunHandle) sandbox.LiveReading {
	return &follower{cli: r.cli, lineBound: r.lineBound}
}

// follower is one live reading of one job.
type follower struct {
	cli CLI
	// lineBound is its runner's ([Runner.lineBound]).
	lineBound int

	// events is the event stream, decoded by dec into transcript entries
	// that wait in transcript until they settle.
	events     rawStream
	dec        Decoder
	transcript pendingText
	// spoke is whether the transcript has ever had an entry: from then on
	// it is the account shown.
	spoke bool
	// last is the stream's last complete line, which says whether a CLI
	// that finishes without exiting has finished.
	last []byte

	// errs is the error stream, read while the transcript has said nothing.
	errs    rawStream
	errText pendingText
}

// Read implements [sandbox.LiveReading].
func (f *follower) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.LiveRead, error) {
	paths := PathsFor(box)
	out := f.cli.Output(paths)
	marker, err := box.ReadFile(ctx, paths.Done())
	if err != nil {
		return sandbox.LiveRead{}, err
	}
	// DONE MEANS WHOLE. The wrapper writes its marker after the CLI has
	// exited, so nothing more is written to either stream: what is held
	// back is settled, and an unfinished last line is a line.
	done := len(marker) > 0
	read := sandbox.LiveRead{AsOf: time.Now().UTC(), Finished: done}
	if out.Events {
		if err = f.readEvents(ctx, box, out.Stdout, done); err != nil {
			return sandbox.LiveRead{}, err
		}
		if !done && out.Terminal && len(f.last) > 0 {
			read.Finished = f.cli.Finished(string(f.last))
		}
	}
	if f.spoke {
		read.Origin = f.events.origin("transcript")
		read.Source = sandbox.SourceTranscript
		read.Front = f.events.start > 0
		read.Text = f.transcript.settle(done)
		read.Held = f.transcript.held()
		return read, nil
	}
	lines, restarted, err := f.errs.next(ctx, box, paths.Err(), done)
	if err != nil {
		return sandbox.LiveRead{}, err
	}
	if restarted {
		f.errText = pendingText{}
	}
	f.errText.add(string(lines))
	read.Origin = f.errs.origin("stderr")
	read.Front = f.errs.start > 0
	read.Text = f.errText.settle(done)
	read.Held = f.errText.held() + int(f.errs.size-f.errs.pos)
	read.Source = sandbox.SourceNone
	if f.errs.size > f.errs.start {
		read.Source = sandbox.SourceStderr
	}
	return read, nil
}

// readEvents feeds the event stream's new complete lines through the
// reading's decoder, and its new entries to the transcript.
func (f *follower) readEvents(ctx context.Context, box sandbox.Sandbox, path string, done bool) error {
	lines, restarted, err := f.events.next(ctx, box, path, done)
	if err != nil {
		return err
	}
	if restarted || f.dec == nil {
		f.dec, f.transcript = f.cli.Events(), pendingText{}
	}
	if len(lines) == 0 {
		return nil
	}
	// A bytes.Reader cannot fail, so neither can this.
	_ = eachLine(bytes.NewReader(lines), f.dec, f.lineBound)
	for _, entry := range f.dec.Entries() {
		f.transcript.add(entry + "\n")
		f.spoke = true
	}
	// CLONED: the line is a slice of the read's buffer, which would
	// otherwise be held for as long as the reading lasts.
	trimmed := bytes.TrimRight(lines, "\r\n")
	f.last = bytes.Clone(trimmed[bytes.LastIndexByte(trimmed, '\n')+1:])
	return nil
}

// pendingText is display text written but not shown yet, until its redaction
// is settled.
type pendingText struct{ text string }

func (p *pendingText) add(s string) { p.text += s }

// settle takes what is settled — all of it, once the job is done — redacted.
func (p *pendingText) settle(done bool) string {
	n := len(p.text)
	if !done {
		n = redact.Settled(p.text)
	}
	out := redact.Secrets(p.text[:n])
	p.text = p.text[n:]
	return out
}

func (p *pendingText) held() int { return len(p.text) }

// rawStream is where a reading is in one of a box's streams.
type rawStream struct {
	started bool
	// start is where the reading began, pos how far it has taken whole
	// lines, size the stream's size at the last read and growth how much
	// it grew then.
	start, pos, size, growth int64
	// restarts is how many times the reading began again, which is part
	// of its origin: a stream truncated and written anew from the same
	// place is not the text read from there before.
	restarts int
	// partialOpen is set when the reading began inside a line longer than
	// the whole window, so there was no line break to begin after. Until a
	// break arrives the reading is still mid-line, and what has come so far
	// is the END of a line whose start is off the window — unshowable, the
	// way [FileTail.Lines] drops a partial FIRST line. Showing it would put
	// the tail of a credential that straddled the window's edge on screen,
	// redacted in isolation with its rule's anchor cut off before it.
	partialOpen bool
}

// origin names the reading of this stream as one account.
func (s *rawStream) origin(account string) string {
	if s.restarts == 0 {
		return fmt.Sprintf("%s@%d", account, s.start)
	}
	return fmt.Sprintf("%s@%d#%d", account, s.start, s.restarts)
}

// next is the complete lines written since the last read — everything left,
// once the stream is done — and whether the reading had to begin again.
//
// SIZED TO WHAT HAS GROWN. A read asks for what it has not taken yet plus
// twice the last growth (at least [liveReadFloor]), so a reading costs about
// what the job writes rather than a window per read. A stream that outgrew the
// guess is asked again for exactly its backlog, and one whose backlog is past
// [liveBacklog], or that shrank, is read again from its end.
func (s *rawStream) next(ctx context.Context, box sandbox.Sandbox, path string, done bool) ([]byte, bool, error) {
	if !s.started {
		return s.begin(ctx, box, path, done, false)
	}
	want := (s.size - s.pos) + max(2*s.growth, liveReadFloor)
	tail, err := box.ReadTail(ctx, path, int(min(want, liveBacklog+liveReadFloor)))
	if err != nil {
		return nil, false, err
	}
	if tail.Size < s.size {
		return s.begin(ctx, box, path, done, true)
	}
	if from := tail.Size - int64(len(tail.Data)); from > s.pos {
		if tail.Size-s.pos > liveBacklog {
			return s.begin(ctx, box, path, done, true)
		}
		if tail, err = box.ReadTail(ctx, path, int(tail.Size-s.pos+liveReadFloor)); err != nil {
			return nil, false, err
		}
		if tail.Size < s.size || tail.Size-int64(len(tail.Data)) > s.pos {
			return s.begin(ctx, box, path, done, true)
		}
	}
	s.growth = tail.Size - s.size
	s.size = tail.Size
	from := tail.Size - int64(len(tail.Data))
	return s.take(tail.Data[s.pos-from:], done), false, nil
}

// begin starts the reading at the start of the stream's last [liveWindow], on
// a line — or, when the window holds no line break at all, mid-line with
// [rawStream.partialOpen] set, so the rest of that unreadable line is dropped
// rather than shown as if it were whole ([rawStream.take]).
func (s *rawStream) begin(ctx context.Context, box sandbox.Sandbox, path string, done, again bool) ([]byte, bool, error) {
	tail, err := box.ReadTail(ctx, path, liveWindow)
	if err != nil {
		return nil, false, err
	}
	data, partial := tail.Lines()
	s.start = tail.Before() + int64(partial)
	s.pos, s.size, s.growth, s.started = s.start, tail.Size, 0, true
	// A mid-file window with no break is one long line whose start is off
	// the window. Lines returns nothing, and partial is the whole window;
	// begin would otherwise start at the file's end and the next read would
	// hand the rest of that line to the display as a whole line.
	s.partialOpen = !tail.Whole() && len(data) == 0 && bytes.IndexByte(tail.Data, '\n') < 0
	if again {
		s.restarts++
	}
	return s.take(data, done), again, nil
}

// take is data up to its last line break — all of it once the stream is done
// — and moves the reading past it.
//
// While [rawStream.partialOpen], the reading is still inside a line too long
// to have begun after: its bytes so far are dropped up to the first break —
// the rest of that line — and only then does normal line-taking resume. A
// stream that ends (done) still inside such a line has no whole line to show,
// so nothing settles.
func (s *rawStream) take(data []byte, done bool) []byte {
	if s.partialOpen {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			s.pos += int64(len(data))
			return nil
		}
		s.partialOpen = false
		s.pos += int64(i + 1)
		data = data[i+1:]
	}
	n := len(data)
	if !done {
		n = bytes.LastIndexByte(data, '\n') + 1
	}
	s.pos += int64(n)
	return data[:n]
}

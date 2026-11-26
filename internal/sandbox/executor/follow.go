package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The live reading of a running job ([Runner.Follow]), of Collect's accounts:
// the transcript, or the error stream until the transcript has an entry. Each
// read takes only what grew, through one decoder kept for the whole reading,
// and shows only what is settled ([redact.Settled]), so the text is
// append-only (what lets the run's owner send a viewer only what it lacks,
// internal/sandbox's livefeed.go) and is the whole text redacted however the
// reads fell. A reading begins on a line at the start of a stream's last
// [liveWindow]; its ORIGIN names that place and the account, so two owners
// derive the same text, and beginning again (the stream outran [liveBacklog]
// or shrank, or the transcript spoke) names another. The error stream reads
// redaction context before that place ([rawStream.leads]).

// liveWindow is how much of a stream's END a reading begins from. An event's
// size is dominated by what it echoes (a tool's output, twice on Claude Code's
// stream), tens of KiB for a file read; a mebibyte is the last dozen or more
// events whole, a screen of activity in one ranged read. Beginning further
// back would show a person old activity until the reading caught up.
const liveWindow = 1 << 20

// liveReadFloor is the least one read asks for beyond what the reading has not
// taken yet: the growth of a stream between two reads a couple of seconds
// apart, at the rate a harness writes when it is not echoing a file.
const liveReadFloor = 64 << 10

// liveBacklog is the most one read catches up: a stream that grew more between
// two reads (a tool echoing a large file) is read again from its end, since
// reading it all costs that much for one screen of it.
const liveBacklog = 8 << 20

// Follow implements [sandbox.Runner].
func (r *Runner) Follow(sandbox.RunHandle) sandbox.LiveReading {
	return &follower{harness: r.harness, lineBound: r.lineBound, errs: rawStream{leads: true}}
}

// follower is one live reading of one job.
type follower struct {
	harness Harness
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

	// errs is the error stream, read while the transcript has said nothing.
	errs    rawStream
	errText pendingText
}

// Read implements [sandbox.LiveReading].
func (f *follower) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.LiveRead, error) {
	paths := PathsFor(box)
	out := f.harness.Output(paths)
	marker, err := box.ReadFile(ctx, paths.Done())
	if err != nil {
		return sandbox.LiveRead{}, err
	}
	// DONE MEANS WHOLE: the wrapper writes its marker after the harness exits,
	// so what is held back is settled and an unfinished last line is a line.
	done := len(marker) > 0
	read := sandbox.LiveRead{AsOf: time.Now().UTC(), Finished: done}
	if out.Events {
		if err = f.readEvents(ctx, box, out.Stdout, done); err != nil {
			return sandbox.LiveRead{}, err
		}
	}
	if spender, ok := f.dec.(interface{ Spent() int }); ok {
		read.Spent = spender.Spent()
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
	f.errText.lead(string(f.errs.takeLead()))
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
		f.dec, f.transcript = f.harness.Events(), pendingText{}
		// Read through Entries from its first line ([Decoder.Entries]).
		_ = f.dec.Entries(false)
	}
	if len(lines) > 0 {
		// A bytes.Reader cannot fail, so neither can this.
		_ = eachLine(bytes.NewReader(lines), f.dec, f.lineBound)
	}
	// DONE, every line is fed ([rawStream.take]), so the end is said even by
	// a read that brought no line: the skipped lines it ends on were fed by
	// an earlier one.
	entries := f.dec.Entries(done)
	if len(entries) == 0 {
		return nil
	}
	// ADDED ONCE, each entry a line: an add appends to the pending text, so an
	// add per entry would copy that text once per entry, quadratic in a read of
	// many short events. Adding the read's text in one piece settles the same.
	size := len(entries)
	for _, entry := range entries {
		size += len(entry)
	}
	var text strings.Builder
	text.Grow(size)
	for _, entry := range entries {
		text.WriteString(entry)
		text.WriteByte('\n')
	}
	f.transcript.add(text.String())
	f.spoke = true
	return nil
}

// pendingText is display text written but not shown yet, until its redaction
// is settled — and, before a reading's first text is shown, the CONTEXT the
// stream holds before that text ([pendingText.lead]).
type pendingText struct{ ctx, text string }

// add adds what the stream holds after what was added before.
//
// ONLY A LINE THE TEXT OPENS IS SHOWN: while the context ends inside a line,
// s up to its first break ends that line and joins the context, untrimmed.
// What a rule replaces runs no further than its line's break and is not
// bounded short of it ([redact.MaxReach]), so a line's end redacts as its
// line does, from the line's start. A line the text opens is redacted with
// all the context the lead kept and the rest of the line it ended in, as the
// first line a stream's end shows is with all read before it ([redact.SettledEnd]).
func (p *pendingText) add(s string) {
	if p.text == "" && p.ctx != "" && p.ctx[len(p.ctx)-1] != '\n' {
		i := strings.IndexByte(s, '\n') + 1
		if i == 0 {
			i = len(s)
		}
		p.ctx += s[:i]
		s = s[i:]
	}
	p.text += s
}

// lead adds context: what the stream holds before the text, which comes
// before any text is added. Only its last [redact.LookBack] bytes are kept, the
// redaction's look-back.
func (p *pendingText) lead(s string) {
	if s == "" {
		return
	}
	p.ctx += s
	if len(p.ctx) > redact.LookBack {
		p.ctx = p.ctx[len(p.ctx)-redact.LookBack:]
	}
}

// settle takes what is settled (all of it once the job is done), redacted.
//
// THE CONTEXT SETTLES THE TEXT'S START, a line's ([pendingText.add]), and is
// never shown: until what settles reaches past it, what is shown is what the
// context settles ([redact.SettledEnd]). Past that nothing before can change
// how what follows redacts, so the context goes.
func (p *pendingText) settle(done bool) string {
	if p.ctx == "" {
		n := len(p.text)
		if !done {
			n = redact.Settled(p.text)
		}
		out := redact.Secrets(p.text[:n])
		p.text = p.text[n:]
		return out
	}
	whole := p.ctx + p.text
	n := len(whole)
	if !done {
		n = redact.Settled(whole)
	}
	if n <= len(p.ctx) {
		return ""
	}
	shown, _ := redact.SettledEnd(whole[:n], len(p.ctx))
	rest := p.text[n-len(p.ctx):]
	p.ctx, p.text = "", rest
	return shown
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
	// the window: until a break arrives, what comes is the END of a line
	// begun off the window, dropped as [FileTail.Lines] drops a partial first
	// line. Shown, it could put a credential's tail on screen, redacted
	// without its rule's anchor.
	partialOpen bool
	// leads is set on a stream shown as written (the error stream): a reading
	// that begins inside it reads [redact.LookBack] more before that place and
	// hands it on as lead, the context the text's start is redacted with and
	// never shows ([pendingText.settle]), with the rest of an overlong line it
	// drops ([rawStream.take]).
	leads bool
	lead  []byte
}

// takeLead is the context read since it was last taken ([rawStream.leads]):
// the stream's bytes, in order, straight before the first text taken.
func (s *rawStream) takeLead() []byte {
	lead := s.lead
	s.lead = nil
	return lead
}

// keepLead adds b to the context to hand on, keeping only its last
// [redact.LookBack] bytes, as [pendingText.lead] does.
func (s *rawStream) keepLead(b []byte) {
	if !s.leads || len(b) == 0 {
		return
	}
	s.lead = append(s.lead, b...)
	if over := len(s.lead) - redact.LookBack; over > 0 {
		s.lead = append(s.lead[:0], s.lead[over:]...)
	}
}

// origin names the reading of this stream as one account.
func (s *rawStream) origin(account string) string {
	if s.restarts == 0 {
		return fmt.Sprintf("%s@%d", account, s.start)
	}
	return fmt.Sprintf("%s@%d#%d", account, s.start, s.restarts)
}

// next is the complete lines written since the last read (everything left once
// the stream is done) and whether the reading had to begin again.
//
// A read asks for what it has not taken plus twice the last growth (at least
// [liveReadFloor]), so a reading costs about what the job writes, not a window
// a read. A stream that outgrew that is asked again for its exact backlog; one
// past [liveBacklog], or that shrank, is read again from its end.
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
// a line, or mid-line with [rawStream.partialOpen] set when the window holds no
// break. A stream that leads reads [redact.LookBack] more before the window and
// keeps all it read before the reading's start as lead.
func (s *rawStream) begin(ctx context.Context, box sandbox.Sandbox, path string, done, again bool) ([]byte, bool, error) {
	want := liveWindow
	if s.leads {
		want += redact.LookBack
	}
	read, err := box.ReadTail(ctx, path, want)
	if err != nil {
		return nil, false, err
	}
	tail := read
	if before := len(read.Data) - liveWindow; before > 0 {
		tail = sandbox.FileTail{Data: read.Data[before:], Size: read.Size}
	}
	data, partial := tail.Lines()
	s.lead = nil
	s.keepLead(read.Data[:len(read.Data)-len(data)])
	s.start = tail.Before() + int64(partial)
	s.pos, s.size, s.growth, s.started = s.start, tail.Size, 0, true
	// A mid-file window with no break: Lines returns nothing and partial is the
	// whole window, so without this the next read would show the rest of that
	// line as a whole one.
	s.partialOpen = !tail.Whole() && len(data) == 0 && bytes.IndexByte(tail.Data, '\n') < 0
	if again {
		s.restarts++
	}
	return s.take(data, done), again, nil
}

// take is data up to its last line break (all of it once the stream is done)
// and moves the reading past it. While [rawStream.partialOpen], bytes up to the
// first break are the rest of a line begun off the window and are dropped; a
// stream done still inside it shows nothing.
func (s *rawStream) take(data []byte, done bool) []byte {
	if s.partialOpen {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			s.keepLead(data)
			s.pos += int64(len(data))
			return nil
		}
		s.keepLead(data[:i+1])
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

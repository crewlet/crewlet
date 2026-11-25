package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Qwen Code note is CONFIRMED BY ITS RECORD AND THE REPLY AFTER IT. Qwen Code
// records a drained message (`type: user`, subtype `mid_turn_user_message`)
// BEFORE the request carrying it goes out (0.28.4: ahead of its
// request_lifecycle start), so only the next assistant record shows the model
// read it. Not a request's successful end: the first after a note can be a
// compaction's own summary call (chat-compact.jsonl). THE RECORD REFRAMES the
// message behind Qwen Code's framing ("\n" + [qwenNoteFraming]), cut exactly
// before the text is held to a serve's digest ([ItemDigest]). The helper
// confirms on the notes route as the run goes (qwen_helper.mjs), a read of the
// job's session here ([recordConfirmer]). The model writes no user record, and a
// tool's output sits in a tool_result record, never read.

// notesHeld is the inbox ids of takes the session's chat file holds, matched
// in serve order ([recordConfirmer]). No chat file holds none.
func (QwenCode) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake, summary *summaryRead) ([]string, error) {

	if !sandbox.ValidSessionID(session.ID) {
		return nil, summary.failedBy(fmt.Errorf("%q is no session id Qwen Code is created under", session.ID))
	}
	file, err := qwenChatFile(ctx, box, session.ID)
	if err != nil {
		return nil, summary.failedBy(err)
	}
	if file == "" {
		_ = summary.failedBy(errChatFile)
		return nil, nil
	}
	// A RUN'S START THAT DOES NOT READ decides no take, and the walk reads the
	// summary alone: it needs no start.
	from, fromErr := qwenRunFrom(ctx, box, session.ID)
	if fromErr != nil && summary == nil {
		return nil, fromErr
	}
	if fromErr != nil {
		takes = nil
	}
	r, err := box.OpenFile(ctx, file)
	if err != nil {
		return nil, summary.failedBy(fmt.Errorf("opening %s: %w", file, err))
	}
	defer r.Close()
	held, err := qwenNotesHeld(ctx, r, takes, from, maxLineBytes, summary)
	switch {
	case err != nil:
		return nil, summary.failedBy(err)
	case fromErr != nil:
		return nil, fromErr
	}
	return held, nil
}

// recordSummary reports whether the receipts' walk of the chat file reads
// ask's summary: on the release its summary was read on, where no count is
// asked, whose walk of the same file reads it instead.
func (QwenCode) recordSummary(ask sandbox.MainThreadAsk) bool {
	return summaryReleaseWhy(ask.Release, qwenRelease) == "" && !ask.Count
}

// qwenRunFrom is the first line of session's chat file the box's last run
// wrote, off the box's record of where it began ([Paths.QwenRunStart]). A
// record that cannot be read, does not parse or names another session is an
// error, never 0: from 0, a continued session's earlier records would
// confirm a note this run lost, so the take stays undecided instead.
func qwenRunFrom(ctx context.Context, box sandbox.Sandbox, session string) (int, error) {
	path := PathsFor(box).QwenRunStart()
	raw, err := box.ReadFile(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("reading where the run began in its session (%s): %w", path, err)
	}
	id, count, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	n, atoiErr := strconv.Atoi(count)
	switch {
	case !ok || atoiErr != nil || n < 0:
		return 0, fmt.Errorf("%s says %q, not a session and a count of its lines", path, raw)
	case id != session:
		return 0, fmt.Errorf("%s names the session %q, and the run's is %s: the box ran another "+
			"session since", path, id, session)
	}
	return n, nil
}

// qwenChatFile is the chat file Qwen Code records session in, "" for none:
// under its home's projects directory, in the one keyed by the run's working
// directory, which the runner does not know, so it is found by the session's
// id, which names one file.
func qwenChatFile(ctx context.Context, box sandbox.Sandbox, session string) (string, error) {
	glob := shellword.Quote(PathsFor(box).QwenDir()+"/projects") + "/*/chats/" + shellword.Quote(session+".jsonl")
	res, err := box.Exec(ctx, `for f in `+glob+`; do [ -f "$f" ] && printf '%s\n' "$f"; done; true`,
		sandbox.ExecOptions{})
	if err != nil {
		return "", fmt.Errorf("finding the session's chat file: %w", err)
	}
	files := strings.Fields(res.Stdout)
	switch len(files) {
	case 0:
		return "", nil
	case 1:
		return files[0], nil
	}
	return "", fmt.Errorf("%d chat files name the session %s", len(files), session)
}

// qwenNotesHeld is the inbox ids of takes, in serve order, whose items the
// session read from r holds as a current instruction: each record, at or past
// the line from where the takes' run began, matches the first unmatched take
// after the last one matched with its handed text's digest, so two takes of
// identical text need two records. A match is settled only by the next
// assistant record, the reply to a request that carried the note plain; a
// compaction before it keeps the match only where the note survives plain in
// the history Qwen Code goes on with (ADR-0177). A match no reply follows is
// not held, so the note is served again. A line past bound, or one that does
// not decode, drops every pending match: it may have been a compaction that
// summarized them. The record is walked for notes ([lineWalk.noteLine]), and
// every line for the newest summary where summary is not nil ([qwenSummaries]).
func qwenNotesHeld(ctx context.Context, r io.Reader, takes []ServedTake, from, bound int,
	summary *summaryRead) ([]string, error) {
	type match struct{ id, digest string }
	var settled []string
	var pending []match
	next, at := 0, -1
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), bound)
	w.passImages()
	var l qwenNoteLine
	var sums *qwenSummaries
	skip := func() error { return w.skip(jsonscan.ObjectStart) }
	if summary != nil {
		sums = newQwenSummaries(summary)
		l.payload = &sums.payload
		skip = func() error { return l.readKind(&w) }
	}
	for {
		l.kept.want = map[string]int{}
		for _, m := range pending {
			l.kept.want[m.digest]++
		}
		read := func() error { return l.read(&w) }
		if at+1 < from {
			read = skip
		}
		line, err := w.noteLine(ctx, read)
		if err != nil {
			return nil, fmt.Errorf("executor: reading which notes the Qwen Code session holds: %w", err)
		}
		if line.none {
			break
		}
		if sums != nil {
			sums.line(ctx, line, &l)
		}
		at++
		switch {
		case line.past:
			pending = pending[:0]
		case at < from, line.blank, line.null:
		case !line.object || !l.whole:
			pending = pending[:0]
		case l.typ == "assistant":
			// A TURN CANCELLED MID-REPLY records the reply's streamed part as
			// an assistant record, and session/load hands the note back plain
			// before it (measured on 0.28.4: session/cancel after a first
			// chunk, then a resumed request): it settles as any reply does.
			for _, m := range pending {
				settled = append(settled, m.id)
			}
			pending = pending[:0]
		case l.typ == "system" && l.subtype == "chat_compression":
			tail := l.kept.tail()
			kept := pending[:0]
			for _, m := range pending {
				if tail[m.digest] > 0 {
					tail[m.digest]--
					kept = append(kept, m)
				}
			}
			pending = kept
		case l.typ == "user" && l.subtype == "mid_turn_user_message" && l.handedSet && l.handed.framed():
			digest := l.handed.digest()
			for i := next; i < len(takes); i++ {
				if takes[i].Digest == digest {
					pending, next = append(pending, match{takes[i].InboxID, digest}), i+1
					break
				}
			}
		}
		if line.ended {
			break
		}
	}
	if sums != nil && sums.lines.err != nil {
		return nil, sums.lines.err
	}
	return settled, nil
}

// qwenNoteLine is what the confirm reader reads of one chat-file record: its
// type and subtype, its message's first part's text, digested with Qwen
// Code's framing cut, and what a compaction keeps of the pending notes; whole
// where each member of those is of its whole-line decode's kind ([fieldKind]).
type qwenNoteLine struct {
	typ, subtype     string
	whole, handedSet bool
	handed           framedDigest
	kept             qwenPreserved
	// payload, where a read asks the summary, is the system payload as written.
	payload *qwenPayload
}

// read reads one record's members. A KEY MATCHES ITS NAME EXACTLY, AND ONE
// WRITTEN TWICE HOLDS ITS LAST VALUE ([fieldKind]), where the whole-line decode
// (encoding/json) matched a name in any case (`Subtype`) and merged an object
// written twice: Qwen Code writes neither.
func (l *qwenNoteLine) read(w *lineWalk) error {
	l.typ, l.subtype, l.whole, l.handedSet = "", "", true, false
	l.kept.reset()
	l.payload.reset()
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.typ, ok, err = w.wordField(k)
		case "subtype":
			l.subtype, ok, err = w.wordField(k)
		case "systemPayload":
			if err = l.payload.tee(w); err != nil {
				return err
			}
			ok, err = true, l.kept.read(w, k)
		case "message":
			l.handedSet = false
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "parts" {
					return w.skip(k)
				}
				l.handedSet = false
				first := true
				listed, listErr := w.listField(k, func(k jsonscan.Kind) error {
					took, partErr := w.objectField(k, func(name string, k jsonscan.Kind) error {
						if name != "text" {
							return w.skipMember(name, k)
						}
						switch k {
						case jsonscan.String:
							if !first {
								return w.skip(k)
							}
							l.handedSet = true
							return l.handed.scan(w)
						case jsonscan.Null:
							l.handedSet = l.handedSet && !first
							return nil
						}
						l.whole = false
						return w.skip(k)
					})
					first = false
					l.whole = l.whole && took
					return partErr
				})
				l.whole = l.whole && listed
				return listErr
			})
		default:
			return w.skip(k)
		}
		l.whole = l.whole && ok
		return err
	})
}

// readKind reads a line before the run's start for what its newest summary
// reads alone: its type, subtype and system payload.
func (l *qwenNoteLine) readKind(w *lineWalk) error {
	l.typ, l.subtype = "", ""
	l.payload.reset()
	return w.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "type":
			l.typ, _, err = w.wordField(k)
		case "subtype":
			l.subtype, _, err = w.wordField(k)
		case "systemPayload":
			if err = l.payload.tee(w); err == nil {
				err = w.skip(k)
			}
		default:
			err = w.skip(k)
		}
		return err
	})
}

// qwenPreserved counts, by digest, the handed texts of the notes pending
// (want, each digest by how many) that a chat_compression record keeps plain:
// each text part of a user entry of the history Qwen Code goes on with, past
// its prefix, its framing cut (a model entry's words are never a note). The
// prefix is the larger of summaryPrefixLength (the summary and the model's
// acknowledgement, measured on 0.28.4) and contextPrefixLength, which no
// capture tells apart from it: the larger keeps less, so a note the
// compaction summarized is never held. A record whose summary prefix does not
// read, or whose decode refuses it, keeps nothing. The prefix is written
// after the history, so each digest keeps the entries of its last
// occurrences, as many as notes pending wear it.
type qwenPreserved struct {
	want                   map[string]int
	object, refused        bool
	summary, context       int64
	summarySet, contextSet bool
	entries                int
	role                   string
	counts                 map[string]int
	last                   map[string][]int
	text                   framedDigest
}

// reset starts a payload that is none.
func (p *qwenPreserved) reset() {
	p.object, p.refused, p.summarySet, p.contextSet, p.entries = false, false, false, false, 0
	clear(p.last)
}

// read reads a record's system payload, each member checked by its
// whole-line decode's field.
func (p *qwenPreserved) read(w *lineWalk, k jsonscan.Kind) error {
	p.reset()
	if p.object = k == jsonscan.ObjectStart; !p.object {
		p.refused = k != jsonscan.Null
		return w.skip(k)
	}
	if p.last == nil {
		p.last, p.counts = map[string][]int{}, map[string]int{}
	}
	return w.members(func(name string, k jsonscan.Kind) error {
		var n int64
		var ok bool
		var err error
		switch name {
		case "summaryPrefixLength":
			n, ok, err = w.intField(k)
			p.summary, p.summarySet = n, ok && k == jsonscan.Number
		case "contextPrefixLength":
			n, ok, err = w.intField(k)
			p.context, p.contextSet = n, ok && k == jsonscan.Number
		case "compressedHistory":
			p.entries = 0
			clear(p.last)
			ok, err = w.listField(k, func(k jsonscan.Kind) error { return p.entry(w, k) })
		default:
			return w.skip(k)
		}
		p.refused = p.refused || !ok
		return err
	})
}

// entry reads one entry of the history, counting the texts of a user one.
func (p *qwenPreserved) entry(w *lineWalk, k jsonscan.Kind) error {
	p.role = ""
	clear(p.counts)
	took, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "role":
			p.role, ok, err = w.wordField(k)
		case "parts":
			clear(p.counts)
			ok, err = w.listField(k, func(k jsonscan.Kind) error {
				took, partErr := w.objectField(k, func(name string, k jsonscan.Kind) error {
					if name != "text" {
						return w.skipMember(name, k)
					}
					switch k {
					case jsonscan.String:
						if scanErr := p.text.scan(w); scanErr != nil {
							return scanErr
						}
						if d := p.text.digest(); p.want[d] > 0 {
							p.counts[d]++
						}
						return nil
					case jsonscan.Null:
						return nil
					}
					p.refused = true
					return w.skip(k)
				})
				p.refused = p.refused || !took
				return partErr
			})
		default:
			return w.skip(k)
		}
		p.refused = p.refused || !ok
		return err
	})
	p.refused = p.refused || !took
	e := p.entries
	p.entries++
	if p.role == "user" {
		for d, c := range p.counts {
			for range min(c, p.want[d]) {
				if p.last[d] = append(p.last[d], e); len(p.last[d]) > p.want[d] {
					p.last[d] = p.last[d][1:]
				}
			}
		}
	}
	return err
}

// tail counts, by digest, the pending notes' texts the history keeps past its
// prefix; none where the record keeps nothing.
func (p *qwenPreserved) tail() map[string]int {
	kept := map[string]int{}
	if !p.object || p.refused || !p.summarySet || p.summary < 0 {
		return kept
	}
	prefix := p.summary
	if p.contextSet {
		prefix = max(prefix, p.context)
	}
	for d, entries := range p.last {
		for _, e := range entries {
			if int64(e) >= prefix {
				kept[d]++
			}
		}
	}
	return kept
}

// framedDigest is a text's digest ([ItemDigest]) read a piece at a time, with
// Qwen Code's framing ("\n" + [qwenNoteFraming]) cut where the text opens
// with it, and whether it did.
type framedDigest struct {
	whole, rest hash.Hash
	n           int
	opens       bool
}

// scan reads w's current string into d, holding none of it.
func (d *framedDigest) scan(w *lineWalk) error {
	if d.whole == nil {
		d.whole, d.rest = sha256.New(), sha256.New()
	}
	d.whole.Reset()
	d.rest.Reset()
	d.n, d.opens = 0, true
	framing := "\n" + qwenNoteFraming
	for {
		n, err := w.scan.Read(w.piece)
		p := w.piece[:n]
		_, _ = d.whole.Write(p)
		if d.n < len(framing) {
			k := min(len(p), len(framing)-d.n)
			d.opens = d.opens && string(p[:k]) == framing[d.n:d.n+k]
			_, _ = d.rest.Write(p[k:])
		} else {
			_, _ = d.rest.Write(p)
		}
		d.n += n
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// framed reports whether the text opens with the framing.
func (d *framedDigest) framed() bool { return d.opens && d.n >= len("\n"+qwenNoteFraming) }

// digest is the text's digest, its framing cut where it opens with it.
func (d *framedDigest) digest() string {
	if d.framed() {
		return hex.EncodeToString(d.rest.Sum(nil))
	}
	return hex.EncodeToString(d.whole.Sum(nil))
}

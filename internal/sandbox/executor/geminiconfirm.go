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

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A Gemini CLI take is CONFIRMED BY ITS CONTENT (ruling N2 of #1818): no
// record out of the model's view reaches the session, so no take token can,
// and a take counts as read once the main session holds its exact rendered
// text as a user message, where Gemini CLI puts an AfterAgent block's reason.
// Measured on 0.70.0: written before the next request, kept through a SIGKILL
// of every gemini process, and what a resume sends. Only a user entry's text
// part counts, whole. A compression's
// summary confirms nothing, its restatement of the kept entries neither
// (geminirecord.go): a note was sent plain before any compression could fold
// it, so its plain entry confirms it, even once a later `$patch` drops it.

// geminiAskedTake is one take [geminiConfirmed] is asked of: its rendered
// text's digest ([ItemDigest]), and the lines of the session it can be
// confirmed on: from the count of the session's lines as its run started (the
// record's `from`, [geminiScript]) up to, not including, the next run's start
// in the same record (until; -1 for the last run's takes). A continued
// session holds an earlier job's deliveries, which a later take of the same
// text was never handed, and a later job's, which an earlier take never was.
type geminiAskedTake struct {
	digest      string
	from, until int
}

// geminiConfirmed is which of takes, in the order they were made, the main
// session read from r holds: each user text part confirms the first
// unconfirmed take after the last one confirmed whose digest its text has and
// whose run had started by its line, so two takes of identical text need two
// deliveries, and a take never delivered (its hook killed after it answered)
// leaves the later ones to be confirmed. A line that does not decode is passed
// over, as a torn last line is, and counted. The record is walked for notes
// ([lineWalk.noteLine]), a line's parts confirming only once its type is read.
func geminiConfirmed(ctx context.Context, r io.Reader, takes []geminiAskedTake, bound int) ([]bool, error) {
	confirmed := make([]bool, len(takes))
	next, restating, at := 0, false, -1
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), bound)
	l := geminiNoteLine{takes: takes}
	for {
		read := func() error { return w.scan.Skip() }
		if next < len(takes) {
			read = func() error { return l.read(&w, next, at+1) }
		}
		line, err := w.noteLine(ctx, read)
		if err != nil {
			return nil, fmt.Errorf("executor: reading which takes the Gemini CLI session holds: %w", err)
		}
		if line.none {
			break
		}
		at++
		if !line.past && line.object && l.whole && next < len(takes) {
			if l.patched {
				restating = false
			}
			switch {
			case restating || l.typ != "user":
			case l.snapshot:
				restating = true
			default:
				for _, i := range l.confirmed {
					confirmed[i] = true
				}
				next = l.next
			}
		}
		if line.ended {
			break
		}
	}
	return confirmed, nil
}

// geminiNoteLine is what the confirm reader reads of one session line: its
// type, whole where it is of its whole-line decode's kind, whether it closes
// a sync, and which takes its content's text parts would confirm, from next
// on, at its line, and whether its first text a thought is not opens a
// compression's summary.
type geminiNoteLine struct {
	takes                 []geminiAskedTake
	typ                   string
	whole, patched        bool
	from, next, at        int
	confirmed             []int
	firstSeen, snapshot   bool
	partText, partThought bool
	partDigest            string
	h                     hash.Hash
	opening               textTrim
}

// read reads one line's members, confirming from next at line at. A KEY
// MATCHES ITS NAME EXACTLY, AND ONE WRITTEN TWICE HOLDS ITS LAST VALUE
// ([fieldKind]), where the whole-line decode (encoding/json) matched a name in
// any case (`Content`, `$Patch`): Gemini CLI writes neither.
func (l *geminiNoteLine) read(w *lineWalk, next, at int) error {
	l.typ, l.whole, l.patched, l.from, l.at = "", true, false, next, at
	l.contentStart()
	return w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "type":
			var ok bool
			var err error
			l.typ, ok, err = w.wordField(k)
			l.whole = l.whole && ok
			return err
		case "$patch", "$rewindTo":
			l.patched = true
		case "content":
			l.contentStart()
			return l.content(w, k)
		}
		return w.scan.Skip()
	})
}

// contentStart starts a content: no part read, nothing confirmed.
func (l *geminiNoteLine) contentStart() {
	l.next, l.confirmed, l.firstSeen, l.snapshot = l.from, l.confirmed[:0], false, false
}

// content reads a content's text parts as [chatReading.parts] reads them.
func (l *geminiNoteLine) content(w *lineWalk, k jsonscan.Kind) error {
	switch k {
	case jsonscan.String:
		l.partText, l.partThought = true, false
		if err := l.text(w); err != nil {
			return err
		}
		if l.opening.n > 0 {
			l.partEnd()
		}
		return nil
	case jsonscan.ArrayStart:
		return w.elements(func(k jsonscan.Kind) (bool, error) {
			if k != jsonscan.ObjectStart {
				return true, w.scan.Skip()
			}
			l.partText, l.partThought = false, false
			err := w.members(func(name string, k jsonscan.Kind) error {
				switch name {
				case "text":
					if l.partText = k == jsonscan.String; l.partText {
						return l.text(w)
					}
				case "thought":
					l.partThought = k == jsonscan.True
				}
				return w.scan.Skip()
			})
			l.partEnd()
			return true, err
		})
	}
	return w.scan.Skip()
}

// text reads the current string: its digest, and its opening trimmed.
func (l *geminiNoteLine) text(w *lineWalk) error {
	if l.h == nil {
		l.h = sha256.New()
	}
	l.h.Reset()
	l.opening.begin(io.Discard, 0)
	for {
		n, err := w.scan.Read(w.piece)
		_, _ = l.h.Write(w.piece[:n])
		_, _ = l.opening.Write(w.piece[:n])
		switch {
		case errors.Is(err, io.EOF):
			l.partDigest = hex.EncodeToString(l.h.Sum(nil))
			return nil
		case err != nil:
			return err
		}
	}
}

// partEnd confirms the first take from next its text's digest is, delivered
// by the line, and reads the first text a thought is not for a summary.
func (l *geminiNoteLine) partEnd() {
	if !l.partText {
		return
	}
	if !l.firstSeen && !l.partThought {
		l.firstSeen, l.snapshot = true, l.opening.opens(geminiSnapshot)
	}
	for i := l.next; i < len(l.takes); i++ {
		t := l.takes[i]
		if t.digest == l.partDigest && t.from <= l.at && (t.until < 0 || l.at < t.until) {
			l.confirmed, l.next = append(l.confirmed, i), i+1
			return
		}
	}
}

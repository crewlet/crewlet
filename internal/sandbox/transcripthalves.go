package sandbox

import (
	"bytes"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// TranscriptHalves is what a run's record keeps of a transcript's redaction
// written to it a piece at a time: [boundTranscript]'s rule, applied to what
// its two buffers hold, in [MaxRunTextBytes] bytes whatever the length.
// The head buffer holds the first [transcriptHeadBytes], all a first line
// longer than its half needs, since its cut reads one byte past its bound;
// the ring the last [MaxRunTextBytes]-[transcriptHeadBytes],
// [transcriptNoteRoom] more than the end's half, so where each line the end
// keeps starts is known. Up to [MaxRunTextBytes] the two meet and hold every
// byte. The zero value is empty and ready.
type TranscriptHalves struct {
	head   []byte // the first halvesHeadBytes bytes written
	ring   []byte // the last halvesRingBytes bytes written
	next   int    // where the ring's next byte goes once it is full
	bytes  int    // every byte written
	breaks int    // every line break written
	last   byte   // the last byte written
}

const (
	halvesHeadBytes = transcriptHeadBytes
	halvesRingBytes = MaxRunTextBytes - transcriptHeadBytes
)

// Write is [TranscriptHalves.WriteString] of p. It never fails.
func (h *TranscriptHalves) Write(p []byte) (int, error) {
	return h.WriteString(string(p))
}

// WriteString writes s as the next piece of the transcript, keeping no more
// of it than the halves need. It never fails.
func (h *TranscriptHalves) WriteString(s string) (int, error) {
	n := len(s)
	if n == 0 {
		return 0, nil
	}
	h.bytes += n
	h.breaks += strings.Count(s, "\n")
	h.last = s[n-1]
	if room := halvesHeadBytes - len(h.head); room > 0 {
		if h.head == nil {
			h.head = make([]byte, 0, halvesHeadBytes)
		}
		k := min(room, len(s))
		h.head = append(h.head, s[:k]...)
		s = s[k:]
	}
	if len(s) == 0 {
		return n, nil
	}
	if h.ring == nil {
		h.ring = make([]byte, 0, halvesRingBytes)
	}
	if room := halvesRingBytes - len(h.ring); room > 0 {
		k := min(room, len(s))
		h.ring = append(h.ring, s[:k]...)
		s = s[k:]
	}
	if len(s) >= halvesRingBytes {
		s, h.next = s[len(s)-halvesRingBytes:], 0
	}
	for len(s) > 0 {
		k := copy(h.ring[h.next:], s)
		s, h.next = s[k:], (h.next+k)%halvesRingBytes
	}
	return n, nil
}

// Bounded is the transcript as its record keeps it, and the lines and bytes
// of its middle left out ([boundTranscript]).
func (h *TranscriptHalves) Bounded() (text string, elidedLines, elidedBytes int) {
	end := make([]byte, 0, len(h.ring))
	end = append(append(end, h.ring[h.next:]...), h.ring[:h.next]...)
	if h.bytes <= MaxRunTextBytes {
		return string(h.head) + string(end), 0, 0
	}

	// The start's whole lines within its half, else its first line's start.
	keptHead := bytes.LastIndexByte(h.head, '\n') + 1
	headLines := bytes.Count(h.head[:keptHead], []byte("\n"))
	// The end's whole lines within its half, else its last line's end: from
	// the first line start at least the half back from the end. The ring is
	// longer than the half, so the byte before each such start is held.
	from := len(end) - transcriptTailBytes
	keptFrom := len(end)
	if at := bytes.IndexByte(end[from-1:], '\n'); at >= 0 {
		keptFrom = from + at
	}
	tailLines := bytes.Count(end[keptFrom:], []byte("\n"))
	if keptFrom < len(end) && h.last != '\n' {
		tailLines++
	}

	const marker = "…"
	var startOf, endOf string
	if headLines == 0 {
		// The head's half holds no line break: all of it is the first line's.
		startOf = textcut.Within(string(h.head), transcriptHeadBytes-1) + "\n"
	}
	if keptFrom == len(end) {
		endOf = textcut.Tail(string(end[len(end)-transcriptTailBytes:]), transcriptTailBytes-len(marker))
	}

	lines := h.breaks
	if h.last != '\n' {
		lines++
	}
	middle := lines - headLines - tailLines
	dropped := h.bytes - keptHead - (len(end) - keptFrom)
	// As [boundTranscript] counts: bytes kept of a partial line are not
	// left out, its marker is not content, and a line kept in part is not
	// a line left out.
	partial := 0
	if startOf != "" {
		dropped -= len(startOf) - len(marker)
		partial++
	}
	if endOf != "" {
		dropped -= len(endOf) - len(marker)
		if startOf == "" || middle > 1 {
			partial++
		}
	}
	whole := middle - partial

	var b strings.Builder
	b.Grow(MaxRunTextBytes)
	b.Write(h.head[:keptHead])
	b.WriteString(startOf)
	b.WriteString(elidedNote(whole, partial, dropped))
	b.WriteString(endOf)
	b.Write(end[keptFrom:])
	return b.String(), whole, dropped
}

// Cut keeps the first n bytes written, cut back to the last line break among
// them, as the transcript of a stream still being written ends: at what its
// redaction made final. False, changing nothing, where a byte it would drop,
// or the line break it would end at, is no longer held, or what is left could
// not be bounded as [TranscriptHalves.Bounded] bounds it.
func (h *TranscriptHalves) Cut(n int) bool {
	n = max(0, min(n, h.bytes))
	end := make([]byte, 0, len(h.ring))
	end = append(append(end, h.ring[h.next:]...), h.ring[:h.next]...)
	// whole: the head and the ring meet, and every byte written is held.
	whole := h.bytes == len(h.head)+len(end)
	head, kept := h.head, end
	switch drop := h.bytes - n; {
	case drop <= len(end):
		kept = end[:len(end)-drop]
	case whole:
		head, kept = h.head[:n], end[:0]
	default:
		return false
	}
	switch i := bytes.LastIndexByte(kept, '\n'); {
	case i >= 0:
		kept = kept[:i+1]
	case whole:
		head, kept = head[:bytes.LastIndexByte(head, '\n')+1], kept[:0]
	default:
		return false
	}
	removed := bytes.Count(h.head[len(head):], []byte("\n")) + bytes.Count(end[len(kept):], []byte("\n"))
	left := h.bytes - (len(h.head) - len(head)) - (len(end) - len(kept))
	if !whole && (left <= MaxRunTextBytes || len(kept) <= transcriptTailBytes) {
		return false
	}
	h.head, h.ring, h.next = head, kept, 0
	h.bytes, h.breaks = left, h.breaks-removed
	switch {
	case len(kept) > 0:
		h.last = kept[len(kept)-1]
	case len(head) > 0:
		h.last = head[len(head)-1]
	default:
		h.last = 0
	}
	return true
}

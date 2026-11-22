package redact

// A StretchReader reads a redacted text's own words as an [OwnWordsWriter] hands them over: the stretches
// [OwnWords] yields, in order, each begun by Stretch at its offset before its first byte, its bytes handed
// over by Write. A run that opens with [Marker] is undecided until it closes: Mark comes before it, then its
// bytes, and then Commit, where it closed as the text's own words, or Rewind, where it was a marker, every
// byte of it, its closing bracket among them, written since Mark. Where the run would begin a stretch, its
// Stretch comes after Mark, so Rewind takes it back too. One run is undecided at a time.
type StretchReader interface {
	Stretch(at int)
	Write(p []byte) (int, error)
	Mark()
	Commit()
	Rewind()
}

// markerBytes is Marker, handed to a reader where the bytes an OwnWordsWriter held while matching it turn
// out to be words or open a run.
var markerBytes = []byte(Marker)

// An OwnWordsWriter is [OwnWords] of a redacted text written to it as it arrives ([NewOwnWordsWriter]): it
// hands its reader the text's stretches and its runs, as [StretchReader] says. IT HOLDS NONE OF THE TEXT:
// while the text's last bytes match the opening of [Marker] it holds how many do, which are Marker's own,
// and once Marker is whole the run is the reader's, a byte at a time as it arrives, whatever its length,
// since [writtenMarkerLen] reads a kind's name of any length.
type OwnWordsWriter struct {
	r StretchReader
	// off is where in the text the next byte written sits, and open says a stretch is open, which the
	// text's own words go on.
	off  int
	open bool
	// matched is how many bytes of Marker the text's last bytes, held, match, from matchAt on.
	matched, matchAt int
	// inRun says a run is undecided, and named that it holds a byte of a kind's name.
	inRun, named bool
	// err is what the reader answered, which ends the splitting.
	err error
}

// NewOwnWordsWriter is an [OwnWordsWriter] that hands the text written to it to r.
func NewOwnWordsWriter(r StretchReader) *OwnWordsWriter {
	return &OwnWordsWriter{r: r}
}

// Write reads p as the text's next bytes. It answers len(p), or what the reader answered: an error of the
// reader's ends the splitting, which hands it nothing more and answers that error from then on.
func (w *OwnWordsWriter) Write(p []byte) (int, error) {
	// p[from:i] is not yet handed over: the run's, while one is undecided, else the text's own words.
	from := 0
	for i := 0; i < len(p) && w.err == nil; i++ {
		c := p[i]
		if w.inRun {
			switch {
			case 'a' <= c && c <= 'z' || c == '-':
				w.named = true
				continue
			case c == ']' && w.named:
				w.write(p[from : i+1])
				w.decide(true)
				from = i + 1
				continue
			}
			// The run is the text's own words; c, which ended it, is read on as the text's, and may open the
			// next run.
			w.write(p[from:i])
			w.decide(false)
			from = i
		}
		if w.matched > 0 {
			if c == Marker[w.matched] {
				w.matched++
				from = i + 1
				if w.matched == len(Marker) {
					w.beginRun()
				}
				continue
			}
			w.words(markerBytes[:w.matched], w.matchAt)
			w.matched = 0
		}
		if c == Marker[0] {
			w.words(p[from:i], w.off+from)
			w.matched, w.matchAt, from = 1, w.off+i, i+1
		}
	}
	switch {
	case w.inRun:
		w.write(p[from:])
	case w.matched == 0:
		w.words(p[from:], w.off+from)
	}
	w.off += len(p)
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

// Close ends the text: what it held matching Marker, and a run still undecided, are its own words.
func (w *OwnWordsWriter) Close() error {
	if w.matched > 0 {
		w.words(markerBytes[:w.matched], w.matchAt)
		w.matched = 0
	}
	if w.inRun {
		w.decide(false)
	}
	return w.err
}

// beginRun tells the reader a run begins where the Marker it matched does, and hands over Marker.
func (w *OwnWordsWriter) beginRun() {
	w.matched, w.inRun, w.named = 0, true, false
	if w.err != nil {
		return
	}
	w.r.Mark()
	if !w.open {
		w.r.Stretch(w.matchAt)
		w.open = true
	}
	w.write(markerBytes)
}

// decide ends the undecided run: a marker, which ends the stretch it went on, or the text's own words.
func (w *OwnWordsWriter) decide(marker bool) {
	w.inRun = false
	if w.err != nil {
		return
	}
	if marker {
		w.r.Rewind()
		w.open = false
		return
	}
	w.r.Commit()
}

// words hands over b, the text's own words beginning at the byte offset at, beginning a stretch there
// where none is open.
func (w *OwnWordsWriter) words(b []byte, at int) {
	if len(b) == 0 || w.err != nil {
		return
	}
	if !w.open {
		w.r.Stretch(at)
		w.open = true
	}
	w.write(b)
}

// write hands b to the reader, unless it has failed.
func (w *OwnWordsWriter) write(b []byte) {
	if len(b) == 0 || w.err != nil {
		return
	}
	_, w.err = w.r.Write(b)
}

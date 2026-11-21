package executor

import (
	"errors"
	"io"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// THE ESTIMATE READ IN THE LINE'S ONE PASS, for a reader that asked
// ([threadReader.Estimating]): each length the estimate's rules charge
// (claudelineestimate.go) is the scanner's count as the value passes
// ([jsonscan.Scanner.Units] as written, [jsonscan.Scanner.TextUnits]
// decoded), at any length, nothing held but a PDF read's pages. Every key a
// rule reads is read by its decoded name and last occurrence, whatever the
// type written before it, since an element's type is known only at its end.
// The walk recurses an element list at a time, to [estimateNestMax]. The
// package doc ("What Claude Code estimates a line's content at") has the rest.

// The types the estimate's other walks read a value's `type` as: a task
// delivery's image and a rendered message's text block.
var (
	estimateImageTypes = [...]string{estimateImage}
	estimateTextTypes  = [...]string{blockText}
)

// pdfPagesBytes is the most of a read's `pages` the pass captures: a count is
// at most [wireCountMax], 16 digits, so a longer value is no count.
const pdfPagesBytes = 20

// estTextOf is the value of kind k the scanner has just passed whole as a
// text a rule reads ([estimateText]): its kind, and a string's decoded length.
func estTextOf(scan *jsonscan.Scanner, k jsonscan.Kind) estimateText {
	t := estimateText{kind: scannedKind(k)}
	if k == jsonscan.String {
		t.units = scan.TextUnits()
	}
	return t
}

// takenOf is how many bytes of a value Next took as it answered the value's
// first token, of kind k: a container's bracket or a string's quote, a whole
// literal, nothing of a number. A value's length as written begins that many
// units before the scanner's count.
func takenOf(k jsonscan.Kind) int64 {
	switch k {
	case jsonscan.ObjectStart, jsonscan.ArrayStart, jsonscan.String:
		return 1
	case jsonscan.True, jsonscan.Null:
		return int64(len("true"))
	case jsonscan.False:
		return int64(len("false"))
	}
	return 0
}

// charge is the block just read charged by the estimate's rule of an element,
// written its length as written, its type read off its held `type`.
func (b *blockScan) charge(written int64) estimateCharge {
	b.est.typ = estimateTypeOf(b.typ, estimateElementTypes[:])
	return b.est.charge(written)
}

// estKey reads the member name of an element, its value's first token k,
// where the estimate's rule reads it and no rule of a block does: `thinking`
// and `data` as texts, `title` ([lineDecoder.title]), and `source`, whose
// list is one deeper than level ([lineDecoder.estSource]); every other key is
// passed over.
func (d *lineDecoder) estKey(scan *jsonscan.Scanner, k jsonscan.Kind, name []byte, o *estimateObject,
	level int) error {
	var err error
	switch string(name) {
	case "thinking":
		err = scan.Skip()
		o.thinking = estTextOf(scan, k)
	case "data":
		err = scan.Skip()
		o.data = estTextOf(scan, k)
	case "title":
		o.titleKind, o.titleJSON, err = d.title(scan, k)
	case "source":
		o.sourceKind, o.source, err = d.estSource(scan, k, level)
	default:
		err = scan.Skip()
	}
	return err
}

// estElement reads one element of a list at level, its first token k, and
// charges it by the estimate's rule of an element: a string at its length,
// an object by its type written its length as written, anything else
// nothing; an object past [estimateNestMax] passed over as the nesting fault.
// digest reads a result's first element as the text's digest besides
// ([lineDecoder.resultElement]); no other level touches the block's state.
func (d *lineDecoder) estElement(scan *jsonscan.Scanner, k jsonscan.Kind, level int,
	digest bool) (estimateCharge, error) {
	switch {
	case k == jsonscan.String:
		err := scan.Skip()
		return estimateCharge{est: estimateUnits(scan.TextUnits())}, err
	case k != jsonscan.ObjectStart:
		return estimateCharge{}, scan.Skip()
	case level > estimateNestMax:
		return estimateCharge{fault: estimateFault{what: estimateFaultNested}}, scan.Skip()
	}
	start := scan.Units() - 1
	var o estimateObject
	each := func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "type":
			o.typ, err = d.estType(scan, k, estimateElementTypes[:])
		case "text":
			err = scan.Skip()
			o.text = estTextOf(scan, k)
		case "name":
			err = scan.Skip()
			o.name = estTextOf(scan, k)
		case "input":
			from := scan.Units() - takenOf(k)
			err = scan.Skip()
			o.input, o.inputSet = scan.Units()-from, k != jsonscan.Null
		case "content":
			o.content, err = d.estList(scan, k, level+1)
		default:
			err = d.estKey(scan, k, name, &o, level)
		}
		return err
	}
	var err error
	if digest {
		err = d.resultElement(scan, &o, each)
	} else {
		err = d.members(scan, each)
	}
	if err != nil {
		return estimateCharge{}, err
	}
	return o.charge(scan.Units() - start), nil
}

// estList reads the value whose first token, k, the scanner stands at as a
// list of elements at level ([estimateList]): a string one element, a list
// each of its elements ([lineDecoder.estElement]), anything else nothing.
func (d *lineDecoder) estList(scan *jsonscan.Scanner, k jsonscan.Kind, level int) (estimateList, error) {
	l := estimateList{kind: scannedKind(k)}
	switch k {
	case jsonscan.String:
		err := scan.Skip()
		l.all.est = estimateUnits(scan.TextUnits())
		return l, err
	case jsonscan.ArrayStart:
	default:
		return l, scan.Skip()
	}
	for {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ArrayEnd {
			return l, err
		}
		c, err := d.estElement(scan, k, level, false)
		if err != nil {
			return l, err
		}
		l.add(k == jsonscan.ObjectStart, c)
	}
}

// estSource reads a document's `source` of an element at level, its first
// token k: its kind, and of an object its type, its data as a text and its
// content as a list one level deeper, each by its last occurrence.
func (d *lineDecoder) estSource(scan *jsonscan.Scanner, k jsonscan.Kind, level int) (valueKind, estimateSource,
	error) {
	var s estimateSource
	if k != jsonscan.ObjectStart {
		return scannedKind(k), s, scan.Skip()
	}
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "type":
			s.typ, err = d.estType(scan, k, estimateSourceTypes[:])
		case "data":
			err = scan.Skip()
			s.data = estTextOf(scan, k)
		case "content":
			s.content, err = d.estList(scan, k, level+1)
		default:
			err = scan.Skip()
		}
		return err
	})
	return '{', s, err
}

// estType is the value the scanner stands at, of kind k, as the one of types
// it names, decoded, as [estimateTypeOf] reads one: "" where it is no string
// or names none. Read no further than one byte past the longest of types
// ([lineDecoder.opening]), the rest left to the scanner's next call.
func (d *lineDecoder) estType(scan *jsonscan.Scanner, k jsonscan.Kind, types []string) (string, error) {
	if k != jsonscan.String {
		return "", scan.Skip()
	}
	longest := 0
	for _, t := range types {
		longest = max(longest, len(t))
	}
	opening, err := d.opening(scan, longest)
	if err != nil {
		return "", err
	}
	for _, t := range types {
		if string(opening) == t {
			return t, nil
		}
	}
	return "", nil
}

// title reads a document's `title`, its first token k: its kind, and of a
// string whether it names a JSON file ([titleScan]), its decoded text
// streamed whole through the automaton.
func (d *lineDecoder) title(scan *jsonscan.Scanner, k jsonscan.Kind) (valueKind, bool, error) {
	if k != jsonscan.String {
		return scannedKind(k), false, scan.Skip()
	}
	var t titleScan
	for {
		n, err := scan.Read(d.text[:])
		t.write(d.text[:n])
		if errors.Is(err, io.EOF) {
			return '"', t.json(), nil
		}
		if err != nil {
			return '"', false, err
		}
	}
}

// titleScan reads a title's decoded text a piece at a time as the harness
// splits it (`e.title?.split(".").pop()?.toLowerCase()`): whether its last
// part after a dot, lower-cased, is `json`, `jsonl` or `jsonc`, the harness
// then counting the document's text at two characters a token. It holds at
// most the five characters of the last part a match needs: a sixth, or any
// character past ASCII (none lower-cases to one of those letters), makes the
// part long, and a dot begins the next.
type titleScan struct {
	part [len("jsonl")]byte
	n    int
	long bool
}

// write reads p, the title's next decoded bytes.
func (t *titleScan) write(p []byte) {
	for _, c := range p {
		switch {
		case c == '.':
			t.n, t.long = 0, false
		case t.long:
		case t.n == len(t.part) || c >= utf8.RuneSelf:
			t.long = true
		default:
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			t.part[t.n] = c
			t.n++
		}
	}
}

// json reports whether the title read names a JSON file.
func (t *titleScan) json() bool {
	if t.long {
		return false
	}
	switch string(t.part[:t.n]) {
	case "json", "jsonl", "jsonc":
		return true
	}
	return false
}

// taskImages is how many images a task delivery, its first token k, carries
// (S @292454728): of an object, the elements of its result's content that are
// objects whose last `type` is image, where the result is its last MCP task
// snapshot's last `result` where that snapshot is an object, else its own
// last `result`; none where that is no object or its content no list. Read in
// whichever order the two come, decided as the delivery ends.
func (d *lineDecoder) taskImages(scan *jsonscan.Scanner, k jsonscan.Kind) (int64, error) {
	if k != jsonscan.ObjectStart {
		return 0, scan.Skip()
	}
	var (
		own, snapshot int64
		snapshotted   bool
	)
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "mcpTaskSnapshot":
			snapshotted, snapshot = k == jsonscan.ObjectStart, 0
			if !snapshotted {
				return scan.Skip()
			}
			return d.members(scan, func(name []byte, k jsonscan.Kind) error {
				if string(name) != "result" {
					return scan.Skip()
				}
				var err error
				snapshot, err = d.resultImages(scan, k)
				return err
			})
		case "result":
			var err error
			own, err = d.resultImages(scan, k)
			return err
		}
		return scan.Skip()
	})
	if snapshotted {
		return snapshot, err
	}
	return own, err
}

// resultImages is how many images a task's result, its first token k,
// carries: of an object, the elements of its last `content`, where that is a
// list, that are objects whose last `type` is image.
func (d *lineDecoder) resultImages(scan *jsonscan.Scanner, k jsonscan.Kind) (int64, error) {
	if k != jsonscan.ObjectStart {
		return 0, scan.Skip()
	}
	var images int64
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		if string(name) != "content" {
			return scan.Skip()
		}
		images = 0
		_, err := d.elements(scan, k, func(k jsonscan.Kind) error {
			if k != jsonscan.ObjectStart {
				return scan.Skip()
			}
			image := false
			err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
				if string(name) != "type" {
					return scan.Skip()
				}
				typ, err := d.estType(scan, k, estimateImageTypes[:])
				image = typ == estimateImage
				return err
			})
			if image {
				images++
			}
			return err
		})
		return err
	})
	return images, err
}

// pdfReads is a PDF read's `reads`, its first token k, as its pages
// ([pdfPages]): each read's last `pages` captured within [pdfPagesBytes]
// (longer is no count) and held to a count ([wireCount]), never marking the
// line past its bound.
func (d *lineDecoder) pdfReads(scan *jsonscan.Scanner, k jsonscan.Kind) (pdfPages, error) {
	p := pdfPages{ok: true}
	list, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		if !p.ok {
			return scan.Skip()
		}
		if k != jsonscan.ObjectStart {
			p.ok = false
			return scan.Skip()
		}
		var raw [pdfPagesBytes]byte
		// size is the pages' length as written; -1 absent, -2 past the bound.
		size := -1
		err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
			if string(name) != "pages" {
				return scan.Skip()
			}
			got, whole, err := scan.Capture(pdfPagesBytes)
			if size = copy(raw[:], got); !whole {
				size = -2
			}
			return err
		})
		if err != nil {
			return err
		}
		if size < 0 {
			p.ok = false
			return nil
		}
		if n, countErr := wireCount("pages", raw[:size]); countErr != nil || n < 1 {
			p.ok = false
		} else {
			p.pages = min(p.pages+n, threadEstimateMax)
		}
		return nil
	})
	p.ok = p.ok && list
	return p, err
}

// renderedOf is a record's stored rendering, its `rendered`, its first token
// k, as the harness charges it ([renderedCharge]), counted as it passes.
func (d *lineDecoder) renderedOf(scan *jsonscan.Scanner, k jsonscan.Kind) (renderedCharge, error) {
	scan.CountUnits(true)
	defer scan.CountUnits(false)
	r, messages := renderedCharge{ok: true}, 0
	list, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		messages++
		if !r.ok {
			return scan.Skip()
		}
		if k != jsonscan.ObjectStart {
			r.ok = false
			return scan.Skip()
		}
		var content renderedCharge
		err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
			if string(name) != "content" {
				return scan.Skip()
			}
			var err error
			content, err = d.renderedContent(scan, k)
			return err
		})
		r.est, r.ok = r.est.plus(content.est), content.ok
		return err
	})
	r.ok = r.ok && list && messages > 0
	return r, err
}

// renderedContent is one rendered message's `content`, its first token k,
// charged ([renderedCharge]): a non-empty string at its length, or a
// non-empty list of objects whose last `type` is text and last `text` a
// non-empty string, each at its own length; not ok for anything else.
func (d *lineDecoder) renderedContent(scan *jsonscan.Scanner, k jsonscan.Kind) (renderedCharge, error) {
	switch k {
	case jsonscan.String:
		err := scan.Skip()
		units := scan.TextUnits()
		return renderedCharge{est: estimateUnits(units), ok: units > 0}, err
	case jsonscan.ArrayStart:
	default:
		return renderedCharge{}, scan.Skip()
	}
	c, blocks := renderedCharge{ok: true}, 0
	_, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		blocks++
		if !c.ok {
			return scan.Skip()
		}
		if k != jsonscan.ObjectStart {
			c.ok = false
			return scan.Skip()
		}
		var (
			typ  string
			text estimateText
		)
		err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
			switch string(name) {
			case "type":
				var err error
				typ, err = d.estType(scan, k, estimateTextTypes[:])
				return err
			case "text":
				err := scan.Skip()
				text = estTextOf(scan, k)
				return err
			}
			return scan.Skip()
		})
		if typ != blockText || text.kind != '"' || text.units == 0 {
			c.ok = false
		}
		c.est = c.est.plus(estimateUnits(text.units))
		return err
	})
	c.ok = c.ok && blocks > 0
	return c, err
}

// finishEstimate settles f's estimate once the line has ended, from what the
// pass read, after [settle] has emptied every frame the record's type does
// not read: a user or assistant record's message, an object whose content its
// rule reads, from its content's charge and the images the record's task
// delivery carries ([messageCharge]); an attachment record's attachment, an
// object, from its type, a PDF read's pages, its own delivery's images and
// the record's stored rendering ([attachmentCharge]).
func (d *lineDecoder) finishEstimate(f *lineFacts) {
	f.estimating = true
	if m := &f.message; m.kind == '{' && m.contentErr == nil {
		m.estimate, m.estimateErr = messageCharge(m.charge, m.emptyText, d.images)
	}
	if a := &f.attachment; a.kind == '{' {
		a.estimate, a.estimated = attachmentCharge(a.typ, a.pdf, a.images, d.rendered)
	}
}

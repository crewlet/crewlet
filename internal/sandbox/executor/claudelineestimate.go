package executor

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
)

// WHAT CLAUDE CODE ESTIMATES A LINE'S CONTENT AT: the facts a reader of its
// count reads of a line ([threadLine.Estimate], [threadLine.AttachmentEstimate]),
// filled as the line is read, for a reader that asked
// ([threadReader.Estimating]), by the harness's own rules, which this file
// holds: a user or assistant record at its message's content, an attachment
// at its stored rendering, each element rounded as the harness rounds it, in
// JavaScript's lengths, at both of its text rates. The line's one pass reads
// each length (claudelineestimatescan.go). The package doc ("What Claude Code
// estimates a line's content at") has each rule and the shipped code it was
// read in.

// threadEstimate is Claude Code's estimate of a value's tokens at each of the
// two rates it counts text at: At3 at three characters a token, the rate of
// every current model, and At4 at four, the rate of the fourteen older models
// it names. Each is a sum of per-element charges each rounded as the harness
// rounds it, so the two are two sums, never one scaled to the other.
type threadEstimate struct{ At3, At4 int64 }

// threadEstimateMax is the most either rate of an estimate holds: 2^53 - 1
// ([wireCountMax]), past every model's window by nine orders of magnitude, and
// where a sum of estimates stops ([threadEstimate.plus]), so a PDF read whose
// stated pages charge past an int64 reads as at least every window rather than
// as a wrapped, negative, figure.
const threadEstimateMax = wireCountMax

// plus is e and o summed, each rate at most [threadEstimateMax].
func (e threadEstimate) plus(o threadEstimate) threadEstimate {
	return threadEstimate{At3: min(e.At3+o.At3, threadEstimateMax), At4: min(e.At4+o.At4, threadEstimateMax)}
}

// estimateMediaTokens is what the harness charges an image or a document it
// does not read as text, flat (`m=2000`, 2.1.322 @292454717), and an image an
// MCP task delivers; estimatePDFPageTokens what it charges a page of a PDF
// read (`kbs=3000` @292454941).
const (
	estimateMediaTokens   = 2_000
	estimatePDFPageTokens = 3_000
)

// estimateFlat is a charge of tokens whatever the rate.
func estimateFlat(tokens int64) threadEstimate {
	tokens = min(tokens, threadEstimateMax)
	return threadEstimate{At3: tokens, At4: tokens}
}

// estimateUnits is a text of units UTF-16 code units at both rates.
func estimateUnits(units int64) threadEstimate {
	return threadEstimate{At3: roundedPer(units, 3), At4: roundedPer(units, 4)}
}

// estimateHalf is a text of units UTF-16 code units at two characters a token,
// a JSON-titled text document's rate at either model's (Math.min(n, 2)).
func estimateHalf(units int64) threadEstimate {
	return estimateFlat(roundedPer(units, 2))
}

// roundedPer is Math.round(units / n), which rounds half up, in integers: a
// length is never negative, and no length of a line comes near overflowing
// 2*units.
func roundedPer(units, n int64) int64 { return (2*units + n) / (2 * n) }

// The types of a content element the harness's estimate reads by name, beside
// the layer's own blocks ([blockText], [blockThinking]...).
const (
	estimateImage    = "image"
	estimateDocument = "document"
	// A document's source types.
	estimateSourceText    = "text"
	estimateSourceContent = "content"
)

// estimateElementTypes is every type of a content element the estimate charges
// by a rule of its own; any other is charged at its JSON ([estimateObject]).
// estimateSourceTypes is every type of a document's source it reads as text.
var (
	estimateElementTypes = [...]string{blockText, estimateDocument, estimateImage, blockToolResult, blockToolUse,
		blockThinking, blockRedactedThinking}
	estimateSourceTypes = [...]string{estimateSourceText, estimateSourceContent}
)

// estimateNestMax is the deepest element list whose objects the estimate
// reads: a list one deeper through an element's `content` or its source's
// `content`, the only two the harness's rules recurse through. Past it an
// object a rule charges is no figure ([estimateFaultNested]), never a guess.
// 8: the API's deepest charged chain is 3 (a result's document's source
// element) and the probe threads' 2, so a release may nest five more; the
// walk recurses a Go frame a level, about 0.6 KB, so 8 holds a read's stack to
// kilobytes where [jsonscan.MaxDepth] would hold megabytes, which a base read
// has no room for (the package doc's "A collection's read of a thread").
const estimateNestMax = 8

// quotedIs reports whether raw, a JSON value as written, is the string want,
// where want is ASCII: compared as written where it holds no escape, else
// decoded, where it is short enough to be want at all (six bytes a character
// written as an escape such as `\u0074`, and the quotes), so a long string
// costs no copy.
func quotedIs(raw []byte, want string) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner) == want
	}
	if len(raw) > 2+6*len(want) {
		return false
	}
	var buf [2 + 6*len(blockRedactedThinking)]byte
	got, err := jsontext.AppendUnquote(buf[:0], raw)
	return err == nil && string(got) == want
}

// estimateTypeOf is raw, a type as written, as the one of types it is, "" where
// it is none of them, a string or not.
func estimateTypeOf(raw []byte, types []string) string {
	for _, typ := range types {
		if quotedIs(raw, typ) {
			return typ
		}
	}
	return ""
}

// estimateCharge is a value's estimate, and fault where the harness's own
// estimate of it throws rather than answering: no figure, which a reader
// takes as the estimate not reading. Held beside the estimate rather than
// returned at once, since a value whose fault is in an element its parent's
// rule does not read (a malformed call inside an unknown block, charged at its
// JSON) is charged with no fault.
type estimateCharge struct {
	est   threadEstimate
	fault estimateFault
}

// plus is c and o summed, the first fault kept.
func (c estimateCharge) plus(o estimateCharge) estimateCharge {
	if c.fault.what == "" {
		c.fault = o.fault
	}
	c.est = c.est.plus(o.est)
	return c
}

// estimateFault is why the estimate of a value does not read, as a value a
// charge carries until the line ends, then an error once ([estimateFault.err]):
// what names the value, "" where none faults, and kind its kind.
type estimateFault struct {
	what string
	kind valueKind
}

// The values an estimate faults on: a call's name and a text document's title
// that are there and no string, on which the harness's own estimate throws;
// and an object nested past [estimateNestMax], which the layer does not read.
const (
	estimateFaultName   = "a tool_use's name"
	estimateFaultTitle  = "a document's title"
	estimateFaultNested = "a content element"
)

// err is f as the estimate's error, nil where nothing faulted.
func (f estimateFault) err() error {
	switch f.what {
	case "":
		return nil
	case estimateFaultNested:
		return fmt.Errorf("%s is nested past %d element lists, which the estimate does not read", f.what,
			estimateNestMax)
	}
	return fmt.Errorf("%s is %s, not a string", f.what, f.kind)
}

// estimateList is a value read as a list of elements: its kind, every
// element's charge summed (all), and the charges of its elements that are
// objects alone (objects), the two ways the harness reads a list. A string
// is one element, all its own; any other value charges nothing.
type estimateList struct {
	kind         valueKind
	all, objects estimateCharge
}

// add charges c, an element's charge, to l: to its objects too where the
// element is an object.
func (l *estimateList) add(object bool, c estimateCharge) {
	l.all = l.all.plus(c)
	if object {
		l.objects = l.objects.plus(c)
	}
}

// estimateText is a key's value where the rule reads it as a text: its kind,
// and its length where it is a string.
type estimateText struct {
	kind  valueKind
	units int64
}

// estimateObject is what an element's rules read of an object, each key by
// its exact name and a key written twice by its last value, as JSON.parse
// reads it: its type, where it is one a rule names; each text a rule reads;
// its input's length as written; its source, title and content.
type estimateObject struct {
	typ                        string
	text, thinking, data, name estimateText
	// input is the length of `input` as written; inputSet whether it is
	// there and not null, the harness's `e.input ?? {}`.
	input    int64
	inputSet bool
	// sourceKind is `source`'s kind, and source what it holds where that is
	// an object.
	sourceKind valueKind
	source     estimateSource
	// titleJSON is whether `title` names a JSON file, and titleKind its kind.
	titleJSON bool
	titleKind valueKind
	content   estimateList
}

// estimateSource is what a document's source holds that its rule reads.
type estimateSource struct {
	typ     string
	data    estimateText
	content estimateList
}

// charge is the object's charge by its type, written the length of its JSON
// as written (n7e, in its order).
func (o *estimateObject) charge(written int64) estimateCharge {
	switch o.typ {
	case blockText:
		return estimateCharge{est: estimateUnits(o.text.units)}
	case estimateDocument:
		if c, ok := o.sourceCharge(); ok {
			return c
		}
		return estimateCharge{est: estimateFlat(estimateMediaTokens)}
	case estimateImage:
		return estimateCharge{est: estimateFlat(estimateMediaTokens)}
	case blockToolResult:
		// Its content by the rules again (EX): a list, a string, or nothing.
		return o.content.all
	case blockToolUse:
		// `e.name + JSON.stringify(e.input ?? {})`: a name that is no string
		// the harness's own concatenation reads as JavaScript prints that
		// value, which no rule here does.
		if o.name.kind != '"' {
			return estimateCharge{fault: estimateFault{what: estimateFaultName, kind: o.name.kind}}
		}
		input := o.input
		if !o.inputSet {
			input = int64(len(`{}`))
		}
		return estimateCharge{est: estimateUnits(o.name.units + input)}
	case blockThinking:
		return estimateCharge{est: estimateUnits(o.thinking.units)}
	case blockRedactedThinking:
		return estimateCharge{est: estimateUnits(o.data.units)}
	}
	return estimateCharge{est: estimateUnits(written)}
}

// sourceCharge is a document's charge by its source, false where the source
// is none the rule reads as text, which charges the document flat: an object
// whose type is text (its data, at two characters a token where its title
// names a JSON file), or content (a string, or the objects of a list).
func (o *estimateObject) sourceCharge() (estimateCharge, bool) {
	if o.sourceKind != '{' {
		return estimateCharge{}, false
	}
	switch o.source.typ {
	case estimateSourceText:
		// `e.title?.split(".")`: a title that is there, not null and no
		// string has no split, and the harness's estimate throws.
		if o.titleKind != 0 && o.titleKind != 'n' && o.titleKind != '"' {
			return estimateCharge{fault: estimateFault{what: estimateFaultTitle, kind: o.titleKind}}, true
		}
		if o.titleJSON {
			return estimateCharge{est: estimateHalf(o.source.data.units)}, true
		}
		return estimateCharge{est: estimateUnits(o.source.data.units)}, true
	case estimateSourceContent:
		switch o.source.content.kind {
		case '"':
			return o.source.content.all, true
		case '[':
			return o.source.content.objects, true
		}
	}
	return estimateCharge{}, false
}

// messageCharge is a user or assistant record's estimate from what its line's
// pass read: c its message's content's charge, emptyText whether that content
// is the empty string, and images how many images its task delivery carries,
// charged only beside a content that is no empty string (the harness's
// `e.message?.content` charges a record only where it is truthy). A fault
// answers its error and leaves the images off, as the harness throws first.
func messageCharge(c estimateCharge, emptyText bool, images int64) (threadEstimate, error) {
	if err := c.fault.err(); err != nil || emptyText {
		return c.est, err
	}
	return c.est.plus(estimateFlat(estimateMediaTokens * images)), nil
}

// The attachments the harness's estimate reads by type, not by what they
// store: a PDF read ([pdfPages]), and the two it renders again
// whatever they store (L_r @298641216); and a queued command, which also
// charges the images its task delivery carries.
const (
	attachmentPDFReads      = "message_pdf_reads"
	attachmentPollEvents    = "poll_events"
	attachmentProjectDocs   = "project_doc_reads"
	attachmentQueuedCommand = "queued_command"
)

// attachmentsOfNoMessage is the five attachment types the harness renders as
// no message (Nh @283080639), none of which stores a rendering: each reads as
// nothing where it stores none.
var attachmentsOfNoMessage = [...]string{"prompt_snapshot", "prompt_render_point", "deferred_tools_record",
	"thinking_drop", "credential_org"}

// pdfPages is a PDF read's pages (vbs @292454941): the sum of its reads'
// `pages`, each a count of at least 1, saturating at [threadEstimateMax]; ok
// false where `reads` is no list, or a read no object stating its pages. The
// harness charges nothing where a read fails the rest of its schema (a path,
// a result), which this charges by its pages: the floor's safe side.
type pdfPages struct {
	pages int64
	ok    bool
}

// renderedCharge is a record's stored rendering, its `rendered`, as the
// harness charges it, and whether it holds to the shape the harness reads one
// by (Cje's schema @298645370, whose objects keep any other key): a list of at
// least one message, each an object whose `content` is a non-empty string or
// a non-empty list of objects of type `text` whose text is a non-empty string,
// every content at its length, each text block on its own.
type renderedCharge struct {
	est threadEstimate
	ok  bool
}

// attachmentCharge is an attachment's estimate from what its line's pass read:
// its `type` as written, a PDF read's pages, the images its task delivery
// carries and the record's stored rendering; false where the estimate does not
// read ([lineFacts]'s rules, in the package doc). A type that is no string
// reads as not reading: its accessor answers the type's error first.
func attachmentCharge(typ json.RawMessage, pdf pdfPages, images int64, rendered renderedCharge) (threadEstimate,
	bool) {
	kind, err := wireString("attachment.type", typ)
	if err != nil {
		return threadEstimate{}, false
	}
	switch kind {
	case attachmentPDFReads:
		if !pdf.ok {
			return threadEstimate{}, false
		}
		return estimateFlat(min(pdf.pages, threadEstimateMax/estimatePDFPageTokens) * estimatePDFPageTokens), true
	case attachmentPollEvents, attachmentProjectDocs:
		return threadEstimate{}, false
	}
	if rendered.ok {
		e := rendered.est
		if kind == attachmentQueuedCommand {
			e = e.plus(estimateFlat(estimateMediaTokens * images))
		}
		return e, true
	}
	if typ == nil || slices.Contains(attachmentsOfNoMessage[:], kind) {
		return threadEstimate{}, true
	}
	return threadEstimate{}, false
}

// errNotEstimated is a line whose reader did not ask for its estimate
// ([threadReader.Estimating]): no figure, never an estimate of nothing.
var errNotEstimated = errors.New("executor: the line's reader did not ask for its estimate")

// Estimate is Claude Code's estimate of a user or assistant record after the
// last reply its count reads ([lineFacts]'s rules, in the package doc): its
// message's content, and on a record whose content is no empty string the
// images its task delivery carries. Nothing, and no error, on every record of
// another type.
//
// Its reader sums it over every record after that reply ([nextReading]). ITS
// ERRORS ARE, FIRST, [errNotEstimated] where the line's reader did not ask for
// it; then the message's ([threadLine.Message]); and where the harness's own
// estimate throws on the content (a document's title that is there and no
// string, a call's name that is no string) or the content nests an object
// past [estimateNestMax], that. Read once, as the line is read.
func (l threadLine) Estimate() (threadEstimate, error) {
	if !l.facts().estimating {
		return threadEstimate{}, errNotEstimated
	}
	if _, err := l.Message(); err != nil {
		return threadEstimate{}, err
	}
	m := &l.facts().message
	switch {
	case l.Head.Type != recordUser && l.Head.Type != recordAssistant:
		return threadEstimate{}, nil
	case m.estimateErr != nil:
		return threadEstimate{}, fmt.Errorf("its message's content: %w", m.estimateErr)
	}
	return m.estimate, nil
}

// AttachmentEstimate is Claude Code's estimate of an attachment record after
// the last reply its count reads ([lineFacts]'s rules, in the package
// doc): what its stored rendering holds, or a PDF read's
// pages; and whether that reads, false where the harness renders the
// attachment again by its type, a template this layer does not hold, so its
// reader bounds it otherwise. Nothing, read, on an attachment record with no
// attachment and on every record of another type, which the harness charges
// nothing as an attachment.
//
// ITS ERRORS ARE, FIRST, [errNotEstimated] where the line's reader did not
// ask for it; then the type's ([threadLine.AttachmentType]). Read once, as the
// line is read.
func (l threadLine) AttachmentEstimate() (threadEstimate, bool, error) {
	if !l.facts().estimating {
		return threadEstimate{}, false, errNotEstimated
	}
	if _, err := l.AttachmentType(); err != nil {
		return threadEstimate{}, false, err
	}
	a := &l.facts().attachment
	if l.Head.Type != recordAttachment || a.kind == 0 {
		return threadEstimate{}, true, nil
	}
	return a.estimate, a.estimated, nil
}

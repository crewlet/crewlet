package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Where the PARSER'S failures are in the document an operator wrote.
//
// # Why this is harder than reading the line yaml reports
//
// Because the line yaml reports is not a line in that document. The strict
// decoder cannot decode a yaml.Node with unknown keys refused, so
// [decodeKnown] re-encodes the node and decodes the text it produced, and
// every custom decoder does the same with its own sub-node. A failure is
// therefore reported at a line of a buffer nobody wrote: comments and blank
// lines are gone from it, a flow mapping has been reflowed, and a typo inside
// a seat's `llm:` mapping is counted from the top of that mapping alone. Taken
// at its word, a typo on line 9 of a company file was reported as `line 2`,
// and one in a compact JSON body was always on line 1.
//
// So a line is never kept as it was reported. Each decodeKnown re-parses the
// buffer it decoded, pairs every node in it with the node it was encoded
// from, and moves each failure onto that node: its line and column in the
// text the node was parsed from, and whether it is a mapping key. A failure
// arriving from a nested decoder is already on a node of THIS buffer, so the
// same pairing moves it one level further out, and by the time it leaves the
// outermost decoder it sits on a node of the document itself. There the node
// names its authored path (roles[0].llm.pln) and its real line.

// position is where a node sits in the text it was parsed from, and whether
// it is a mapping key: a mapping starts where its first key does, so the two
// share a line and a column and only this tells them apart.
type position struct {
	line, column int
	key          bool
}

func (p position) valid() bool { return p.line > 0 }

func positionOf(n *yaml.Node, key bool) position {
	return position{line: n.Line, column: n.Column, key: key}
}

// placed is one node of a parsed buffer and whether it is a mapping key.
type placed struct {
	node *yaml.Node
	key  bool
}

// bufferIndex pairs the nodes of a buffer a decoder produced with the nodes
// it was encoded from.
type bufferIndex struct {
	// input is each buffer node's position mapped to its source node's.
	input map[position]position
	// lines is every buffer node on a line, in document order, for the
	// failures yaml reports by line alone.
	lines map[int][]placed
}

// indexBuffer re-parses buf, which was encoded from input, and pairs the two
// trees node for node. They have the same shape by construction: encoding
// adds no node and drops none, it only moves them to other lines.
func indexBuffer(buf []byte, input *yaml.Node) *bufferIndex {
	idx := &bufferIndex{input: map[position]position{}, lines: map[int][]placed{}}
	var parsed yaml.Node
	if err := yaml.Unmarshal(buf, &parsed); err != nil {
		return idx
	}
	root := &parsed
	// A sub-node encodes as a document of its own, so the parse has one
	// document node the input does not.
	if input.Kind != yaml.DocumentNode && root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	idx.pair(root, input, false)
	return idx
}

func (idx *bufferIndex) pair(buffer, input *yaml.Node, key bool) {
	if buffer == nil || input == nil {
		return
	}
	// A document node is never what a failure is about, and it starts where
	// its root does: recording it would shadow the root.
	if buffer.Kind != yaml.DocumentNode {
		idx.input[positionOf(buffer, key)] = positionOf(input, key)
		idx.lines[buffer.Line] = append(idx.lines[buffer.Line], placed{node: buffer, key: key})
	}
	// An alias is a leaf here: following it would walk the anchor twice, and
	// forever on a recursive one.
	if buffer.Kind == yaml.AliasNode || buffer.Kind != input.Kind || len(buffer.Content) != len(input.Content) {
		return
	}
	for i := range buffer.Content {
		idx.pair(buffer.Content[i], input.Content[i], buffer.Kind == yaml.MappingNode && i%2 == 0)
	}
}

// find returns the first node on a buffer line that match accepts.
//
// NO NODE IS CLAIMED by the failure that found it, and that is deliberate. A
// buffer is written in block style (see [blockStyle]), which puts one key and
// at most one scalar value on a line, so two different failures never compete
// for one node. The same failure reported twice does, and it is reported twice
// whenever the block holding it is used through an alias: the decoder decodes
// the anchor again at every use, at the same node. A claim would hand the
// second report nothing to land on, and it would leave the parser as a problem
// with no path and no line; unclaimed, both land on the one node the mistake
// was written at, and [placeInDocument] reports it once.
func (idx *bufferIndex) find(line int, match func(placed) bool) (position, bool) {
	for _, candidate := range idx.lines[line] {
		if match(candidate) {
			return idx.input[positionOf(candidate.node, candidate.key)], true
		}
	}
	return position{}, false
}

// yaml.v3's own phrasings: a key the struct does not define, a value of the
// wrong type, and the line prefix both carry.
var (
	unknownFieldRE    = regexp.MustCompile(`^line (\d+): field (\S+) not found in type (\S+)$`)
	positionedLineRE  = regexp.MustCompile(`^line (\d+): (.*)$`)
	cannotUnmarshalRE = regexp.MustCompile(`^cannot unmarshal (!!\w+)`)
	syntaxLineRE      = regexp.MustCompile(`^yaml: line (\d+):`)
)

// typeFault translates one line of a yaml.TypeError into a fault on the node
// it is about.
func (idx *bufferIndex) typeFault(line string, retired map[string]string) *Fault {
	if f, ok := parseCarried(line); ok {
		f.pos = idx.input[f.pos]
		return f
	}
	if m := unknownFieldRE.FindStringSubmatch(line); m != nil {
		lineNo, _ := strconv.Atoi(m[1])
		name := strings.Trim(m[2], `"`)
		pos, _ := idx.find(lineNo, func(c placed) bool { return c.key && c.node.Value == name })
		// A key that was REMOVED needs its own message. "debug is not a
		// setting" is true and useless to someone reading a file the
		// quickstart told them to write: they need the line that replaced
		// it, not a spelling check.
		if replacement, gone := retired[retiredKey(m[3], m[2])]; gone {
			return &Fault{Kind: ErrUnknownField, Detail: replacement, pos: pos}
		}
		return &Fault{Kind: ErrUnknownField, pos: pos, Detail: fmt.Sprintf(
			"%q is not a setting: check the spelling, or the block it belongs under", name)}
	}
	if m := positionedLineRE.FindStringSubmatch(line); m != nil {
		lineNo, _ := strconv.Atoi(m[1])
		return &Fault{Kind: ErrShape, Detail: m[2], pos: idx.valueOn(lineNo, m[2])}
	}
	return &Fault{Kind: ErrShape, Detail: strings.TrimSpace(line)}
}

// valueOn finds the value node a failure reported by line alone is about. A
// "cannot unmarshal !!str" names the tag of the node it refused, which picks it
// out of a line that holds several: in `- token_budget: abc` the list item's
// mapping starts on the same line as the value, and the mapping comes first.
func (idx *bufferIndex) valueOn(line int, detail string) position {
	if m := cannotUnmarshalRE.FindStringSubmatch(detail); m != nil {
		if pos, ok := idx.find(line, func(c placed) bool {
			return !c.key && c.node.ShortTag() == m[1]
		}); ok {
			return pos
		}
	}
	pos, _ := idx.find(line, func(c placed) bool { return !c.key })
	return pos
}

// relocate moves the faults in err from this buffer onto the input it was
// encoded from. An error a custom decoder returned without being a fault is
// made one: it is positioned when it follows yaml's own `line N:` form, and a
// wrong shape either way, which is what every such decoder refuses.
func (idx *bufferIndex) relocate(err error) error {
	var out problems
	for _, leaf := range leafErrors(err) {
		f, isFault := leafFault(leaf)
		switch {
		case isFault:
			moved := *f
			if f.pos.valid() {
				moved.pos = idx.input[f.pos]
			}
			out = append(out, &moved)
		default:
			if m := positionedLineRE.FindStringSubmatch(leaf.Error()); m != nil {
				lineNo, _ := strconv.Atoi(m[1])
				out = append(out, &Fault{Kind: ErrShape, Detail: m[2], pos: idx.valueOn(lineNo, "")})
				continue
			}
			out = append(out, &Fault{Kind: ErrShape, Detail: leaf.Error()})
		}
	}
	return out.err()
}

// placeInDocument gives each parser fault its authored path and its line in
// the document the operator wrote, which is the text doc was parsed from.
// A fault that is not the parser's passes through unchanged.
func placeInDocument(doc *yaml.Node, err error) error {
	paths := map[position]Path{}
	var walk func(n *yaml.Node, path Path)
	walk = func(n *yaml.Node, path Path) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, child := range n.Content {
				walk(child, path)
			}
		case yaml.MappingNode:
			paths[positionOf(n, false)] = path
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				here := entry(path, k.Value)
				paths[positionOf(k, true)] = here
				walk(v, here)
			}
		case yaml.SequenceNode:
			paths[positionOf(n, false)] = path
			for i, child := range n.Content {
				walk(child, idx(path, i))
			}
		default:
			paths[positionOf(n, false)] = path
		}
	}
	walk(doc, nil)

	// ONE FAULT PER MISTAKE AS WRITTEN. An alias decodes its anchor again
	// wherever it is used, so a typo inside an anchored block is found once
	// per use, at the same node; it is one line of the file to fix.
	type mistake struct {
		pos    position
		kind   error
		detail string
	}
	seen := map[mistake]bool{}
	var out problems
	for _, leaf := range leafErrors(err) {
		f, isFault := leafFault(leaf)
		if !isFault || !f.pos.valid() {
			out = append(out, leaf)
			continue
		}
		placed := Fault{Path: paths[f.pos], Kind: f.Kind, Detail: f.Detail, Line: f.pos.line}
		key := mistake{pos: f.pos, kind: f.Kind, detail: f.Detail}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, &placed)
	}
	return out.err()
}

// syntaxFault is a document yaml could not parse at all: a wrong shape with
// no path, carrying the line yaml names when it names one.
func syntaxFault(err error) *Fault {
	f := &Fault{Kind: ErrShape, Detail: err.Error()}
	if m := syntaxLineRE.FindStringSubmatch(err.Error()); m != nil {
		f.Line, _ = strconv.Atoi(m[1])
	}
	return f
}

// nodeFault is a wrong shape a custom decoder refuses, placed on the node it
// refused and handed back in the form the calling decoder keeps collecting
// after (see [carryFaults]).
func nodeFault(n *yaml.Node, detail string) error {
	return carryFaults(&Fault{Kind: ErrShape, Detail: detail, pos: positionOf(n, false)})
}

// carryFaults hands the faults a custom unmarshaler found to the decoder that
// called it as a *yaml.TypeError.
//
// # Why a TypeError, and why its lines are not yaml's
//
// Because it is the only error a decoder collects and keeps going after. Any
// other error an UnmarshalYAML returns ABORTS the whole decode, so one typo in
// a seat's `llm:` mapping used to hide every other failure in the document,
// the key misspelled two seats earlier included, and an author fixing them
// paid a round trip for each.
//
// A TypeError holds strings, though, and the decoder copies them into its
// own. So each fault travels as one line in a form only this package writes
// and reads ([parseCarried]): its place in the text the calling decoder
// parsed (line, column, key or not), its kind and its detail. yaml's own
// "line N: ..." form would lose the column and the kind, and the column is
// what tells two failures on one line of a JSON body apart.
func carryFaults(err error) error {
	if err == nil {
		return nil
	}
	var lines []string
	for _, leaf := range leafErrors(err) {
		f, ok := leafFault(leaf)
		if !ok {
			f = &Fault{Kind: ErrShape, Detail: leaf.Error()}
		}
		lines = append(lines, fmt.Sprintf("%s line=%d column=%d key=%t kind=%s: %s",
			carriedMarker, f.pos.line, f.pos.column, f.pos.key, kindName(f.Kind), f.Detail))
	}
	return &yaml.TypeError{Errors: lines}
}

// carriedMarker opens a line [carryFaults] wrote. No yaml message starts with
// it: every one of those starts with "line ".
const carriedMarker = "crewlet-fault:"

var carriedRE = regexp.MustCompile(`(?s)^crewlet-fault: line=(\d+) column=(\d+) key=(true|false) kind=(\w+): (.*)$`)

// parseCarried reads a fault back from a line [carryFaults] wrote, still in
// the coordinates of the decoder that collected it.
func parseCarried(line string) (*Fault, bool) {
	m := carriedRE.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	lineNo, _ := strconv.Atoi(m[1])
	column, _ := strconv.Atoi(m[2])
	f := &Fault{Kind: ErrShape, Detail: m[5], pos: position{line: lineNo, column: column, key: m[3] == "true"}}
	// The first sentinel of that name is this package's own, which is the
	// one a parser fault carries.
	for _, k := range problemKinds {
		if k.name == m[4] {
			f.Kind = k.sentinel
			break
		}
	}
	return f, true
}

// leafErrors flattens the joins in err into the errors they hold. Only a
// join is flattened: see [joinedParts].
func leafErrors(err error) []error {
	if err == nil {
		return nil
	}
	parts, joined := joinedParts(err)
	if !joined {
		return []error{err}
	}
	var out []error
	for _, part := range parts {
		out = append(out, leafErrors(part)...)
	}
	return out
}

// joinedParts returns the errors a JOIN holds, and false for anything else.
//
// A join is told apart by how it renders, which is its contract: one part per
// line. fmt.Errorf with several %w verbs has the same Unwrap() []error method
// and renders as ONE line ("wrong shape: yaml: line 1: ..."), so a walk that
// took every Unwrap() []error for a join split that one failure in two, one
// of them carrying nothing but the sentinel's text.
func joinedParts(err error) ([]error, bool) {
	multi, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil, false
	}
	parts := multi.Unwrap()
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != nil {
			texts = append(texts, part.Error())
		}
	}
	if strings.Join(texts, "\n") != err.Error() {
		return nil, false
	}
	return parts, true
}

// blockStyle returns a copy of n with every flow mapping and flow sequence
// turned into block style, so the buffer it encodes to holds one key or one
// list item per line.
//
// A JSON body is one flow mapping on one line, and yaml.v3 keeps the style
// when it encodes: the whole company would come back as a handful of wrapped
// lines, and a failure yaml reports by line alone could not be told apart
// from its neighbours. Block style changes where the nodes are written and
// nothing about what they mean. The input is never modified, because it is
// the caller's document, and an alias in the copy points at the copy of its
// anchor.
func blockStyle(n *yaml.Node) *yaml.Node {
	copies := map[*yaml.Node]*yaml.Node{}
	var clone func(*yaml.Node) *yaml.Node
	clone = func(src *yaml.Node) *yaml.Node {
		if src == nil {
			return nil
		}
		if done, ok := copies[src]; ok {
			return done
		}
		dst := *src
		copies[src] = &dst
		if dst.Kind == yaml.MappingNode || dst.Kind == yaml.SequenceNode {
			dst.Style &^= yaml.FlowStyle
		}
		if src.Content != nil {
			dst.Content = make([]*yaml.Node, len(src.Content))
			for i, child := range src.Content {
				dst.Content[i] = clone(child)
			}
		}
		dst.Alias = clone(src.Alias)
		return &dst
	}
	return clone(n)
}

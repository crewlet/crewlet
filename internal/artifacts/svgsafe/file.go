package svgsafe

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- a standalone file ---------------------------------------------------- //

// The namespaces a standalone drawing may name: SVG's, which its root must
// declare, and XLink's, for `xlink:href` alone.
const (
	NamespaceSVG   = "http://www.w3.org/2000/svg"
	NamespaceXLink = "http://www.w3.org/1999/xlink"

	// namespaceXML is the namespace the reserved `xml:` prefix is bound to,
	// as the decoder resolves it.
	namespaceXML = "http://www.w3.org/XML/1998/namespace"
)

// The bounds a standalone drawing is held to beside its byte size, which the
// caller names (a logo's is the brand format's).
const (
	// MaxElements bounds the elements a file holds: a logo is a few hundred,
	// a detailed illustration a few thousand.
	MaxElements = 10_000

	// MaxDepth bounds how deep elements nest, so reading and writing the
	// tree never recurses without end.
	MaxDepth = 64

	// MaxExpandedElements bounds the elements a drawing draws once every
	// `<use>` is expanded: five times [MaxElements], room for a drawing that
	// reuses its symbols generously, and a hard stop for one built to
	// multiply them.
	MaxExpandedElements = 5 * MaxElements

	// MinAspect and MaxAspect bound the viewBox's width over its height: a
	// logo is a wordmark or a mark, and a drawing a hundred times wider
	// than tall is a rule, not a logo.
	MinAspect = 1.0 / 8
	MaxAspect = 8.0
)

// Finding is one way a standalone drawing is outside the allowlist: the line
// where there is one, and what to change.
type Finding struct {
	Line    int
	Message string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("line %d: %s", f.Line, f.Message)
	}
	return f.Message
}

// Refused is a standalone drawing outside the allowlist: every finding, in
// file order.
type Refused struct {
	Findings []Finding
}

func (r *Refused) Error() string {
	lines := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		lines[i] = f.String()
	}
	return "svgsafe: the drawing is outside the allowlist:\n" + strings.Join(lines, "\n")
}

// Canonical is a standalone drawing as the engine stores it: its
// re-serialisation, and its viewBox's width and height.
type Canonical struct {
	Bytes  []byte
	Width  float64
	Height float64
}

// node is one element of a standalone drawing's tree.
type node struct {
	name     string
	attrs    map[string]string // canonical name to value
	children []child
	line     int
}

// child is an element or a run of text, in document order.
type child struct {
	elem *node
	text string
}

// ParseFile reads a standalone `.svg` file of at most maxBytes, holds it to
// the allowlist, and answers its canonical form, or a *[Refused] naming every
// finding.
func ParseFile(src []byte, maxBytes int) (Canonical, error) {
	f := &fileReader{}
	if len(src) > maxBytes {
		f.bad(0, "the file is %d bytes and a drawing may be at most %d: simplify its paths or "+
			"export it at a lower precision", len(src), maxBytes)
		return Canonical{}, f.refused()
	}
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(src) {
		f.bad(0, "the file is not UTF-8 text: export it as UTF-8")
		return Canonical{}, f.refused()
	}
	root := f.read(src)
	if root == nil || len(f.found) > 0 {
		return Canonical{}, f.refused()
	}
	width, height := f.viewBox(root)
	f.drawing.Resolve(func(line int, format string, args ...any) { f.bad(line, format, args...) })
	if len(f.found) > 0 {
		return Canonical{}, f.refused()
	}
	var out bytes.Buffer
	write(&out, root, true)
	out.WriteByte('\n')
	return Canonical{Bytes: out.Bytes(), Width: width, Height: height}, nil
}

// fileReader is one standalone drawing as it is read.
type fileReader struct {
	found   []Finding
	drawing *Drawing
	count   int
}

func (f *fileReader) bad(line int, format string, args ...any) {
	f.found = append(f.found, Finding{Line: line, Message: fmt.Sprintf(format, args...)})
}

func (f *fileReader) refused() error { return &Refused{Findings: f.found} }

// read builds the tree, or answers nil once the file cannot be read on.
func (f *fileReader) read(src []byte) *node {
	f.drawing = NewDrawing()
	d := xml.NewDecoder(bytes.NewReader(src))
	// STRICT, AND NO ENTITY BUT XML's FIVE: an entity this decoder does not
	// know is an error, so nothing a DOCTYPE could declare ever expands.
	d.Strict = true
	d.Entity = nil
	var (
		root  *node
		stack []*node
		first = true
	)
	for {
		tok, err := d.Token()
		line, _ := d.InputPos()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.bad(line, "the file is not well-formed XML: %v", err)
			return nil
		}
		at := first
		first = false
		switch t := tok.(type) {
		case xml.ProcInst:
			if t.Target != "xml" || !at {
				f.bad(line, "<?%s?>: a drawing holds no processing instruction (an xml-stylesheet "+
					"loads a stylesheet); export the plain SVG", t.Target)
				return nil
			}
		case xml.Directive:
			f.bad(line, "<!%s>: a drawing declares no DOCTYPE or ENTITY; export the plain SVG",
				clip(string(t), 20))
			return nil
		case xml.Comment:
			// NOT CONTENT: a comment draws nothing and is not stored.
		case xml.CharData:
			text := string(t)
			top := topOf(stack)
			switch {
			case top != nil && IsTextElement(top.name):
				top.children = append(top.children, child{text: text})
			case strings.TrimSpace(text) == "":
			case top == nil:
				f.bad(line, "text outside the <svg>: a drawing file is its <svg> alone")
				return nil
			default:
				f.bad(line, "text directly in <%s>: text in a drawing sits in <text>", top.name)
			}
		case xml.StartElement:
			n, ok := f.open(t, stack, root, line)
			if !ok {
				return nil
			}
			if root == nil {
				root = n
			} else {
				top := topOf(stack)
				top.children = append(top.children, child{elem: n})
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			f.drawing.Close()
		}
	}
	if root == nil {
		f.bad(0, "the file holds no <svg>")
	}
	return root
}

// open checks one start tag and answers its element, or false once the file
// cannot be read on.
func (f *fileReader) open(t xml.StartElement, stack []*node, root *node, line int) (*node, bool) {
	name := t.Name.Local
	switch {
	case root != nil && len(stack) == 0:
		f.bad(line, "<%s> after the <svg>: a drawing file is one <svg>", name)
		return nil, false
	case len(stack) >= MaxDepth:
		f.bad(line, "<%s> nests more than %d elements deep: flatten the groups", name, MaxDepth)
		return nil, false
	}
	f.count++
	if f.count > MaxElements {
		// READ NO FURTHER: past the bound the drawing is refused whatever
		// else it holds.
		f.bad(line, "the drawing holds more than %d elements: simplify it", MaxElements)
		return nil, false
	}
	switch {
	case t.Name.Space == "" && root == nil:
		f.bad(line, "<svg> declares no namespace: write xmlns=%q on it", NamespaceSVG)
		return nil, false
	case t.Name.Space != NamespaceSVG:
		f.bad(line, "<%s> is in namespace %q, and a drawing is SVG alone: export the plain SVG "+
			"(Inkscape: Plain SVG; Illustrator: presentation attributes)", name, t.Name.Space)
		return nil, false
	case root == nil && name != "svg":
		f.bad(line, "the file's root is <%s>, and a drawing file is one <svg>", name)
		return nil, false
	case !IsElement(name):
		if canonical, ok := Element(strings.ToLower(name)); ok {
			f.bad(line, "<%s>: write <%s>, as SVG spells it", name, canonical)
		} else {
			f.bad(line, "<%s>: %s", name, ElementHint(name))
		}
		return nil, false
	}
	if top := topOf(stack); top != nil {
		if hint, refused := NestHint(top.name, name); refused {
			f.bad(line, "%s", hint)
			return nil, false
		}
	}
	n := &node{name: name, line: line}
	f.drawing.Open(name, line)
	bad := func(format string, args ...any) { f.bad(line, format, args...) }
	for _, a := range t.Attr {
		key, ok := f.attrKey(name, a, bad)
		if !ok {
			continue
		}
		attr, value, ok := f.drawing.Attr(name, key, a.Value, line, bad)
		if !ok {
			continue
		}
		if n.attrs == nil {
			n.attrs = map[string]string{}
		}
		if _, twice := n.attrs[attr]; twice {
			bad("<%s> names %s twice (once as xlink:href)", name, attr)
			continue
		}
		n.attrs[attr] = value
	}
	return n, true
}

// attrKey is the key an attribute is checked under (lowercased, `xlink:href`
// for the XLink reference), or false for a namespace declaration it accepted
// or an attribute it refused.
func (f *fileReader) attrKey(elem string, a xml.Attr, bad func(string, ...any)) (string, bool) {
	space, local := a.Name.Space, a.Name.Local
	switch {
	case space == "" && local == "xmlns":
		if a.Value != NamespaceSVG {
			bad("<%s xmlns=%q>: a drawing is in the SVG namespace, %q", elem, a.Value, NamespaceSVG)
		}
		return "", false
	case space == "xmlns":
		if local != "xlink" || a.Value != NamespaceXLink {
			bad("<%s xmlns:%s=%q>: a drawing names no namespace but SVG's and XLink's; export "+
				"the plain SVG", elem, local, a.Value)
		}
		return "", false
	case space == NamespaceXLink && local == "href":
		return "xlink:href", true
	case (space == "xml" || space == namespaceXML) && local == "base":
		bad("<%s xml:base>: a drawing resolves nothing against another address", elem)
		return "", false
	case space == "xml" || space == namespaceXML:
		bad("<%s xml:%s> is not an attribute a drawing may carry: export without it", elem, local)
		return "", false
	case space != "":
		bad("<%s %s:%s> is in namespace %q, and a drawing carries SVG attributes alone: export the "+
			"plain SVG", elem, prefixOf(space), local, space)
		return "", false
	}
	key := strings.ToLower(local)
	if canonical, ok := attributes[key]; ok && canonical != local {
		bad("<%s %s>: write %s, as SVG spells it", elem, local, canonical)
		return "", false
	}
	return key, true
}

// viewBox checks the root's viewBox and answers its width and height.
func (f *fileReader) viewBox(root *node) (float64, float64) {
	raw, ok := root.attrs["viewBox"]
	if !ok {
		f.bad(root.line, "<svg> has no viewBox: export with a viewBox (min-x min-y width height), "+
			"so the drawing scales to whatever box shows it")
		return 0, 0
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' ||
			r == '\n' || r == '\r'
	})
	nums := make([]float64, 0, 4)
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			break
		}
		nums = append(nums, v)
	}
	if len(parts) != 4 || len(nums) != 4 {
		f.bad(root.line, "viewBox=%q: a viewBox is four numbers, min-x min-y width height", raw)
		return 0, 0
	}
	w, h := nums[2], nums[3]
	if w <= 0 || h <= 0 {
		f.bad(root.line, "viewBox=%q: its width and height must be above zero", raw)
		return 0, 0
	}
	if aspect := w / h; aspect < MinAspect || aspect > MaxAspect {
		f.bad(root.line, "viewBox=%q is %.2f times as wide as it is tall, and a drawing is between "+
			"1:8 and 8:1: crop it to the logo", raw, aspect)
		return 0, 0
	}
	return w, h
}

// ---- the canonical form --------------------------------------------------- //

// write re-serialises one element: attributes sorted by name, values and text
// escaped, an element with no children self-closed. The root carries the one
// namespace declaration.
func write(out *bytes.Buffer, n *node, root bool) {
	out.WriteByte('<')
	out.WriteString(n.name)
	if root {
		out.WriteString(` xmlns="` + NamespaceSVG + `"`)
	}
	keys := make([]string, 0, len(n.attrs))
	for k := range n.attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out.WriteByte(' ')
		out.WriteString(k)
		out.WriteString(`="`)
		escape(out, n.attrs[k], true)
		out.WriteByte('"')
	}
	if len(n.children) == 0 {
		out.WriteString("/>")
		return
	}
	out.WriteByte('>')
	for _, c := range n.children {
		if c.elem != nil {
			write(out, c.elem, false)
			continue
		}
		escape(out, c.text, false)
	}
	out.WriteString("</")
	out.WriteString(n.name)
	out.WriteByte('>')
}

// escape writes s as XML text, or as an attribute value: every character
// markup could read as syntax is a reference, and in a value so is every
// white space a reader would otherwise normalise to a space.
func escape(out *bytes.Buffer, s string, attr bool) {
	for _, r := range s {
		switch {
		case r == '&':
			out.WriteString("&amp;")
		case r == '<':
			out.WriteString("&lt;")
		case r == '>':
			out.WriteString("&gt;")
		case r == '"' && attr:
			out.WriteString("&quot;")
		case (r == '\n' || r == '\r' || r == '\t') && attr:
			fmt.Fprintf(out, "&#%d;", r)
		case r == '\r':
			out.WriteString("&#13;")
		default:
			out.WriteRune(r)
		}
	}
}

func topOf(stack []*node) *node {
	if len(stack) == 0 {
		return nil
	}
	return stack[len(stack)-1]
}

// prefixOf is how a refusal names a namespace: its last path segment.
func prefixOf(space string) string {
	if i := strings.LastIndexAny(space, "/:#"); i >= 0 && i < len(space)-1 {
		return space[i+1:]
	}
	return space
}

// clip cuts s to at most n bytes at a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

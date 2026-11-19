package wiregate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// wire is what the surfaces send: every field of every struct a client
// receives, and the opaque ones among them.
type wire struct {
	root   string
	fset   *token.FileSet
	roots  []root
	fields []wireField
	opaque []wireField
	// declared is every opaque field's row that resolved, by key.
	declared map[string]bool
	// documents is every struct a document reaches, with the document.
	documents map[string]string
	// objects is every object a client receives, by struct, by key.
	objects map[string]map[string]wireField
}

// loaded is the module's wire, read once for every test of this binary.
var loaded = sync.OnceValues(func() (*wire, error) {
	rootDir, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	d := newDiscovery(fset)
	err = sourcetree.TypeCheck{Fset: fset, Patterns: append(slices.Clone(surfaces), declaring...)}.Run(rootDir, func(u *sourcetree.Unit) error {
		d.add(u.Types, u.Files, u.Info, rootDir)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d.wire(rootDir, opaqueFields, documents)
})

// wire walks every root, and every type an opaque field's row says it
// carries, until a walk finds no opaque field it has not walked the row of;
// then it marks the structs each document reaches.
func (d *discovery) wire(rootDir string, rows map[string]carried, docs map[string]string) (*wire, error) {
	w := &wire{root: rootDir, fset: d.fset, roots: d.sortedRoots(), declared: map[string]bool{},
		documents: map[string]string{}}
	r := newReach()
	for _, rt := range w.roots {
		r.walk(rt.typ, "", rt.at)
	}
	for walked := 0; walked < len(r.opaque); walked++ {
		f := r.opaque[walked]
		key := f.key()
		row, ok := rows[key]
		if !ok || w.declared[key] {
			continue
		}
		w.declared[key] = true
		for _, name := range row.types {
			resolved, err := d.lookup(name)
			if err != nil {
				return nil, fmt.Errorf("the row for %s: %w", key, err)
			}
			r.walk(resolved, "", key)
		}
	}
	w.fields, w.opaque, w.objects = w.distinct(r.fields), w.distinct(r.opaque), r.objects
	for doc, why := range docs {
		resolved, err := d.lookup(doc)
		if err != nil {
			return nil, fmt.Errorf("the document %s: %w", doc, err)
		}
		in := newReach()
		in.walk(resolved, "", doc)
		for _, f := range in.fields {
			w.documents[f.owner] = doc + ", " + why
		}
	}
	return w, nil
}

// distinct is fields with each declaration once: a package checked from
// source is also read from export data by the packages importing it, so its
// structs can be walked twice.
func (w *wire) distinct(fields []wireField) []wireField {
	seen := map[string]bool{}
	out := fields[:0:0]
	for _, f := range fields {
		at := w.fset.Position(f.field.Pos())
		key := fmt.Sprintf("%s:%d:%s", at.Filename, at.Line, f.name)
		if !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
	}
	return out
}

// key names a field as the table of opaque fields does: its struct, and the
// key it is sent under.
func (f wireField) key() string { return f.owner + "." + f.name }

// at is where f is declared, from the module root.
func (w *wire) at(f wireField) string {
	return relative(w.fset.Position(f.field.Pos()), w.root)
}

// markers reads the wiregate markers each declaration carries, from source.
type markers struct {
	mu    sync.Mutex
	files map[string]*ast.File
	fset  *token.FileSet
}

func newMarkers() *markers {
	return &markers{files: map[string]*ast.File{}, fset: token.NewFileSet()}
}

// marker is one `//wiregate:<kind> <reason>` line.
type marker struct {
	kind, reason string
	at           token.Position
}

// markerPrefix starts every marker.
const markerPrefix = "//wiregate:"

// of returns the markers on the field f, declared at pos: in its doc comment
// or after it on its line.
func (m *markers) of(pos token.Position, fieldName string) ([]marker, error) {
	file, err := m.parse(pos.Filename)
	if err != nil {
		return nil, err
	}
	var out []marker
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		field, ok := n.(*ast.Field)
		if !ok || !namedAt(m.fset, field, fieldName, pos.Line) && !embeddedAt(m.fset, field, pos.Line) {
			return true
		}
		found = true
		out = append(out, read(m.fset, field.Doc)...)
		out = append(out, read(m.fset, field.Comment)...)
		return false
	})
	if !found {
		return nil, fmt.Errorf("%s:%d: no field %s is declared there", pos.Filename, pos.Line, fieldName)
	}
	return out, nil
}

func namedAt(fset *token.FileSet, field *ast.Field, fieldName string, line int) bool {
	for _, n := range field.Names {
		if n.Name == fieldName && fset.Position(n.Pos()).Line == line {
			return true
		}
	}
	return false
}

func embeddedAt(fset *token.FileSet, field *ast.Field, line int) bool {
	return len(field.Names) == 0 && fset.Position(field.Type.Pos()).Line == line
}

// read is the markers in a comment group.
func read(fset *token.FileSet, group *ast.CommentGroup) []marker {
	if group == nil {
		return nil
	}
	var out []marker
	for _, c := range group.List {
		text, ok := strings.CutPrefix(c.Text, markerPrefix)
		if !ok {
			continue
		}
		kind, reason, _ := strings.Cut(text, " ")
		out = append(out, marker{kind: kind, reason: strings.TrimSpace(reason), at: fset.Position(c.Pos())})
	}
	return out
}

func (m *markers) parse(filename string) (*ast.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.files[filename]; ok {
		return f, nil
	}
	f, err := parser.ParseFile(m.fset, filename, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	m.files[filename] = f
	return f, nil
}

// everyMarker is every marker in the module's Go source, with the field each
// is on, keyed by the field's file and line; one on no field is keyed by its
// own place, with no field name.
func everyMarker(rootDir string) (map[token.Position][]marker, map[token.Position]string, error) {
	fset := token.NewFileSet()
	found := map[token.Position][]marker{}
	names := map[token.Position]string{}
	err := sourcetree.Walk(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // a file of this tree
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), markerPrefix) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		claimed := map[*ast.Comment]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok {
				return true
			}
			for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
				if group == nil {
					continue
				}
				for _, c := range group.List {
					if strings.HasPrefix(c.Text, markerPrefix) {
						claimed[c] = true
					}
				}
				on := fset.Position(field.Type.Pos())
				fieldName := ""
				if len(field.Names) > 0 {
					on = fset.Position(field.Names[0].Pos())
					fieldName = field.Names[0].Name
				}
				on.Column, on.Offset = 0, 0
				if ms := read(fset, group); len(ms) > 0 {
					found[on] = append(found[on], ms...)
					names[on] = fieldName
				}
			}
			return true
		})
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, markerPrefix) && !claimed[c] {
					at := fset.Position(c.Pos())
					at.Column, at.Offset = 0, 0
					found[at] = append(found[at], read(fset, &ast.CommentGroup{List: []*ast.Comment{c}})...)
				}
			}
		}
		return nil
	})
	return found, names, err
}

// sortedPositions is ps in file and line order.
func sortedPositions[V any](ps map[token.Position]V) []token.Position {
	out := make([]token.Position, 0, len(ps))
	for p := range ps {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b token.Position) int {
		if c := strings.Compare(a.Filename, b.Filename); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return out
}

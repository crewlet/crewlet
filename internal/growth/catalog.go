package growth

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Operation is one method of a vendor's API as its product's catalog lists it:
// one row of the product's coverage matrix (package doc, "Every capability").
type Operation struct {
	// ID names the operation in its catalog, unique there: the vendor's own
	// operation ID where it publishes one.
	ID string
	// Method and Path are the vendor's HTTP method and path template.
	Method, Path string
	// Resource is the vendor's name for what the operation acts on, which the
	// matrix groups rows by.
	Resource string
	// Class is the operation's class, or, for a write with [Operation.Fields],
	// the class of changing a field no rule names.
	Class Class
	// Reads marks a call that is not a GET and changes nothing: a report, a
	// query, a search sent as a POST. Only such a call may be read.
	Reads bool
	// Fields classes a write by what it changes ([Classify]): pausing and
	// lowering are operate, enabling and raising spend.
	Fields []FieldRule
	// Roles are the catalog's roles ([Role.ID]) that may call it; empty is
	// every role.
	Roles []string
	// Permissions are the catalog's permissions ([Permission.ID]) it needs.
	Permissions []string
	// States are the classes a write takes while its target is in a state the
	// product reads before acting: a draft edit is operate where the same edit
	// to what customers see is public. The first that holds replaces Class; an
	// unread or unknown state keeps Class, the stricter.
	States []StateRule
	// Danger marks a change that cannot be undone, which a card asks a person
	// to confirm.
	Danger bool
	// Reason says why a never operation is not offered: the vendor's terms
	// forbid it, or it cannot be done. Required for never, a note otherwise.
	Reason string
}

// StateRule is the class of a write while its target is in a named state.
type StateRule struct {
	// When names the state, in the product's own vocabulary ("draft").
	When  string
	Class Class
}

// FieldRule is the class of changing one field of a write.
type FieldRule struct {
	// Field is the field's path in the request body, dotted.
	Field string
	// Class is the class of setting the field, or of raising it where Lower
	// is set.
	Class Class
	// Lower is the class of lowering a number, "" where lowering is Class.
	Lower Class
	// Pausing are the values that pause, which are operate; every other value
	// is Class.
	Pausing []string
}

// Change is one field a write changes, with its value before and after, as
// JSON decodes them; Before is nil for a field the target does not hold yet.
type Change struct {
	Field         string
	Before, After any
}

// Classify is the class of a write of op making changes: the highest class
// over its changes, and op's own class for a write that names no field.
func Classify(op Operation, changes []Change) Class {
	if len(changes) == 0 || len(op.Fields) == 0 {
		return op.Class
	}
	out := ClassRead
	for _, ch := range changes {
		c := op.Class
		if i := slices.IndexFunc(op.Fields, func(r FieldRule) bool { return r.Field == ch.Field }); i >= 0 {
			c = op.Fields[i].classOf(ch)
		}
		if c.Rank() > out.Rank() {
			out = c
		}
	}
	return out
}

func (r FieldRule) classOf(ch Change) Class {
	if len(r.Pausing) > 0 {
		if s, ok := ch.After.(string); ok && slices.Contains(r.Pausing, s) {
			return ClassOperate
		}
		return r.Class
	}
	if r.Lower == "" || ch.Before == nil {
		return r.Class
	}
	before, okB := number(ch.Before)
	after, okA := number(ch.After)
	if okB && okA && after.Cmp(before) < 0 {
		return r.Lower
	}
	// AN UNREADABLE NUMBER IS A RAISE: the higher class asks a person.
	return r.Class
}

// number reads a JSON number, or a decimal string, exactly.
func number(v any) (*big.Rat, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case fmt.Stringer:
		s = t.String()
	case float64, int, int64:
		s = fmt.Sprint(t)
	default:
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	return r, ok
}

// classes is every class a call of op can come to.
func (op Operation) classes() []Class {
	out := []Class{op.Class}
	for _, r := range op.States {
		out = append(out, r.Class)
	}
	for _, r := range op.Fields {
		out = append(out, r.Class)
		if r.Lower != "" {
			out = append(out, r.Lower)
		}
		if len(r.Pausing) > 0 {
			out = append(out, ClassOperate)
		}
	}
	return out
}

// Tool is one engine tool of a product, in exactly one class, and the
// operations it may call: what the engine registers as the tool.
type Tool struct {
	Name       string
	Class      Class
	Operations []string
	// Description and Parameters are the tool's definition as an agent reads
	// it: a sentence of what it does, and its arguments' JSON Schema.
	Description string
	Parameters  map[string]any
}

// Role is one role or permission level the vendor gives an identity, as the
// setup card explains it.
type Role struct {
	// ID is the vendor's own name for it, as its API reports it.
	ID string
	// Name is how people read it; Note what it allows, in one sentence.
	Name, Note string
}

// Permission is one OAuth scope or token permission an operation needs.
type Permission struct {
	ID, Note string
}

// Catalog is one product's whole API as the engine offers it: every
// operation with its class, every role and permission, and its tools.
type Catalog struct {
	// Kind is the product's integration kind.
	Kind string
	// API and Version name the vendor API the build calls; Retires is the
	// date the vendor retires that version, zero where none is announced.
	API, Version string
	Retires      time.Time
	Roles        []Role
	Permissions  []Permission
	Operations   []Operation
	Tools        []Tool
}

// methods is every method an operation may name.
var methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// Validate holds a catalog to its rules: unique names, known classes, roles
// and permissions, a reason on every never row, and tools whose class their
// operations can come to.
func (c Catalog) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(c.Kind) == "" {
		bad("growth: a catalog names its integration kind")
	}
	roles, perms := map[string]bool{}, map[string]bool{}
	for _, r := range c.Roles {
		if r.ID == "" || r.Name == "" || r.Note == "" || roles[r.ID] {
			bad("growth: %s role %q needs a unique ID, a name and a note", c.Kind, r.ID)
		}
		roles[r.ID] = true
	}
	for _, p := range c.Permissions {
		if p.ID == "" || p.Note == "" || perms[p.ID] {
			bad("growth: %s permission %q needs a unique ID and a note", c.Kind, p.ID)
		}
		perms[p.ID] = true
	}
	ops := map[string]Operation{}
	for _, op := range c.Operations {
		if op.ID == "" || ops[op.ID].ID != "" {
			bad("growth: %s operation %q needs a unique ID", c.Kind, op.ID)
		}
		ops[op.ID] = op
		switch {
		case !slices.Contains(methods, op.Method) || op.Path == "":
			bad("growth: %s operation %s needs a method and a path", c.Kind, op.ID)
		case !op.Class.Valid():
			bad("growth: %s operation %s has class %q", c.Kind, op.ID, op.Class)
		case op.Class == ClassNever && strings.TrimSpace(op.Reason) == "":
			bad("growth: %s operation %s is never offered and says no reason", c.Kind, op.ID)
		case (op.Method == "GET" || op.Reads) && op.Class != ClassRead && op.Class != ClassNever:
			bad("growth: %s operation %s reads, so it is read or never, not %s", c.Kind, op.ID, op.Class)
		case op.Method != "GET" && !op.Reads && op.Class == ClassRead:
			bad("growth: %s operation %s writes, so it is not read; a POST that changes nothing says Reads",
				c.Kind, op.ID)
		}
		for _, r := range op.Roles {
			if !roles[r] {
				bad("growth: %s operation %s names role %q, which the catalog does not list", c.Kind, op.ID, r)
			}
		}
		for _, p := range op.Permissions {
			if !perms[p] {
				bad("growth: %s operation %s names permission %q, which the catalog does not list", c.Kind, op.ID, p)
			}
		}
		for _, st := range op.States {
			if strings.TrimSpace(st.When) == "" || !st.Class.Callable() || st.Class == op.Class {
				bad("growth: %s operation %s state rule %q needs a state and a callable class other than "+
					"the operation's", c.Kind, op.ID, st.When)
			}
		}
		for _, f := range op.Fields {
			if f.Field == "" || !f.Class.Callable() || (f.Lower != "" && f.Lower.Rank() > f.Class.Rank()) {
				bad("growth: %s operation %s field rule %q needs a field, a callable class, "+
					"and a lowering class no higher than it", c.Kind, op.ID, f.Field)
			}
		}
	}
	tools := map[string]bool{}
	for _, t := range c.Tools {
		// A READ TOOL MAY NAME NO OPERATION: it answers from the catalog alone.
		// Its name starts with its kind's, so each product keeps to its own
		// names; Register and the authority refuse any that still collide.
		if !strings.HasPrefix(t.Name, c.Kind+"_") || tools[t.Name] || !t.Class.Callable() ||
			(len(t.Operations) == 0 && t.Class != ClassRead) ||
			strings.TrimSpace(t.Description) == "" || t.Parameters == nil {
			bad("growth: %s tool %q needs a unique name starting %s_, a callable class, its operations, a "+
				"description and its parameters", c.Kind, t.Name, c.Kind)
		}
		tools[t.Name] = true
		for _, id := range t.Operations {
			op, ok := ops[id]
			switch {
			case !ok:
				bad("growth: %s tool %s names operation %q, which the catalog does not list", c.Kind, t.Name, id)
			case !slices.Contains(op.classes(), t.Class):
				bad("growth: %s tool %s is %s, and its operation %s never comes to that class",
					c.Kind, t.Name, t.Class, id)
			}
		}
	}
	return errors.Join(errs...)
}

// Operation is the catalog's operation id, false for none.
func (c Catalog) Operation(id string) (Operation, bool) {
	i := slices.IndexFunc(c.Operations, func(op Operation) bool { return op.ID == id })
	if i < 0 {
		return Operation{}, false
	}
	return c.Operations[i], true
}

// Tool is the catalog's tool name, false for none.
func (c Catalog) Tool(name string) (Tool, bool) {
	i := slices.IndexFunc(c.Tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return c.Tools[i], true
}

// Permits reports whether an identity holding roles may call op: op names no
// role, or roles holds one it names.
func Permits(op Operation, roles []string) bool {
	if len(op.Roles) == 0 {
		return true
	}
	for _, r := range roles {
		if slices.Contains(op.Roles, r) {
			return true
		}
	}
	return false
}

var (
	catalogsMu sync.Mutex
	catalogs   = map[string]Catalog{}
)

// Register adds a product's catalog, from its package's init: an invalid
// catalog, a second one for a kind, or a tool another catalog lists is a
// build fault, and panics.
func Register(c Catalog) {
	if err := c.Validate(); err != nil {
		panic(err)
	}
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	if _, dup := catalogs[c.Kind]; dup {
		panic("growth: a second catalog for " + c.Kind)
	}
	for _, other := range catalogs {
		for _, t := range c.Tools {
			if _, listed := other.Tool(t.Name); listed {
				panic("growth: " + c.Kind + " lists tool " + t.Name + ", which " + other.Kind + " lists")
			}
		}
	}
	catalogs[c.Kind] = c
}

// Catalogs is every registered catalog, by kind.
func Catalogs() []Catalog {
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	out := make([]Catalog, 0, len(catalogs))
	for _, c := range catalogs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// KindOfTool is the kind whose catalog lists tool name, false for none.
func KindOfTool(name string) (string, bool) {
	for _, c := range Catalogs() {
		if _, ok := c.Tool(name); ok {
			return c.Kind, true
		}
	}
	return "", false
}

// CatalogOf is kind's catalog, false for a kind that registered none.
func CatalogOf(kind string) (Catalog, bool) {
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	c, ok := catalogs[kind]
	return c, ok
}

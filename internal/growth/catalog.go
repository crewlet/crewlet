package growth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/tools/toolschema"
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
	// the class of one naming no field ([Classify]).
	Class Class
	// Reads marks a call that is not a GET and changes nothing: a report, a
	// query, a search sent as a POST. Only such a call may be read.
	Reads bool
	// Fields classes a write by what it changes ([Classify]): pausing and
	// lowering are operate, enabling and raising spend.
	Fields []FieldRule
	// Classes are the further classes a product's own per-call rule can bring
	// a write to, for a write [FieldRule] cannot class (a mutate of several
	// operations, each compared with what it changes); Class is the highest.
	Classes []Class
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
	// Lower is the class of lowering, "" where lowering is Class: a number
	// made smaller or, for Exclusions, a list that keeps every entry it held.
	Lower Class
	// Exclusions marks a list of what is left out (events kept from an
	// export): adding an entry lowers, and dropping one raises at once.
	Exclusions bool
	// Pausing are the JSON values that pause, a string state or false for a
	// switch, which are operate; every other value is Class.
	Pausing []any
	// Values class setting the field to one of named values, before every
	// other rule: archiving is admin where every other status change is spend.
	// A cleared field is the value "".
	Values []ValueRule
	// While lowers Class while the target is in a state the product reads
	// before acting: a creative swapped on a paused ad shows nobody.
	While []StateRule
}

// ValueRule is the class of setting a field to one of Values.
type ValueRule struct {
	Values []string
	Class  Class
}

// Change is one field a write changes, with its value before and after, as
// JSON decodes them; Before is nil for a field the target does not hold yet.
type Change struct {
	Field         string
	Before, After any
}

// Classify is the class of a write of op making changes: the highest class
// over its changes, and op's own class for a write that names no field. A
// field no rule names takes the highest class op's rules give, so a field
// spelled otherwise or added by the vendor never lowers a write.
func Classify(op Operation, changes []Change) Class { return ClassifyIn(op, changes, nil) }

// ClassifyIn is [Classify] with the target's states as holds reads them: the
// first of op's States that holds replaces its class, the class of every field
// no rule names too, and a field's While lowers it. A nil holds holds no
// state, the stricter reading.
func ClassifyIn(op Operation, changes []Change, holds func(state string) bool) Class {
	in := func(state string) bool { return holds != nil && holds(state) }
	base := op.Class
	state := slices.IndexFunc(op.States, func(s StateRule) bool { return in(s.When) })
	if state >= 0 {
		base = op.States[state].Class
	}
	if len(changes) == 0 || len(op.Fields) == 0 {
		return base
	}
	unruled := base
	for _, r := range op.Fields {
		if state < 0 && r.Class.Rank() > unruled.Rank() {
			unruled = r.Class
		}
	}
	out := ClassRead
	for _, ch := range changes {
		c := unruled
		if i := slices.IndexFunc(op.Fields, func(r FieldRule) bool { return r.Field == ch.Field }); i >= 0 {
			c = op.Fields[i].classIn(ch, in)
		}
		if !c.Valid() {
			// A CLASS THIS BUILD DOES NOT KNOW IS NEVER: ranked below read, it
			// would lower a write nobody classed.
			return ClassNever
		}
		if c.Rank() > out.Rank() {
			out = c
		}
	}
	return out
}

// classIn is the class of ch under r, its While applied where its state holds.
func (r FieldRule) classIn(ch Change, in func(string) bool) Class {
	c := r.classOf(ch)
	if c != r.Class {
		return c
	}
	for _, w := range r.While {
		if in(w.When) {
			return w.Class
		}
	}
	return c
}

func (r FieldRule) classOf(ch Change) Class {
	for _, v := range r.Values {
		if slices.Contains(v.Values, valueOf(ch.After)) {
			return v.Class
		}
	}
	if len(r.Pausing) > 0 {
		if slices.ContainsFunc(r.Pausing, func(v any) bool { return sameScalar(v, ch.After) }) {
			return ClassOperate
		}
		return r.Class
	}
	if r.Exclusions {
		// AN ABSENT LIST IS EMPTY, as the vendor leaves an empty one out.
		if r.Lower != "" && keepsAll(ch.Before, ch.After) {
			return r.Lower
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

// sameScalar reports whether JSON value v is the string or boolean want, of
// the same type: the string "false" is no pause of a switch.
func sameScalar(want, v any) bool {
	switch w := want.(type) {
	case string:
		s, ok := v.(string)
		return ok && s == w
	case bool:
		b, ok := v.(bool)
		return ok && b == w
	}
	return false
}

// keepsAll reports whether list after holds every entry of list before, nil
// being empty; a value that is no list of strings keeps nothing.
func keepsAll(before, after any) bool {
	listOf := func(v any) ([]any, bool) {
		if v == nil {
			return nil, true
		}
		l, ok := v.([]any)
		return l, ok
	}
	was, okB := listOf(before)
	now, okA := listOf(after)
	if !okB || !okA {
		return false
	}
	for _, e := range was {
		if !slices.ContainsFunc(now, func(n any) bool { return sameScalar(e, n) }) {
			return false
		}
	}
	return true
}

// valueOf is a value as a [ValueRule] names it: a string as written, a flag
// or a number in JSON, and "" for one cleared (null, empty or absent).
func valueOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		if len(t) == 0 {
			return ""
		}
	case map[string]any:
		if len(t) == 0 {
			return ""
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
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
	out := append([]Class{op.Class}, op.Classes...)
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
		for _, v := range r.Values {
			out = append(out, v.Class)
		}
		for _, w := range r.While {
			out = append(out, w.Class)
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
	// Covers is what a class tool's methods change, in a few words, which a
	// tool below it names ([Catalog.Above]); every one above read says it.
	Covers string
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
var methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

// reads reports whether op changes nothing by its method or its note.
func (op Operation) reads() bool { return op.Method == "GET" || op.Method == "HEAD" || op.Reads }

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
		case op.reads() && op.Class != ClassRead && op.Class != ClassNever:
			bad("growth: %s operation %s reads, so it is read or never, not %s", c.Kind, op.ID, op.Class)
		case !op.reads() && op.Class == ClassRead:
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
		for _, k := range op.Classes {
			if !k.Callable() || k.Rank() >= op.Class.Rank() {
				bad("growth: %s operation %s may come to %q, which needs to be a callable class below its own",
					c.Kind, op.ID, k)
			}
		}
		for _, f := range op.Fields {
			if f.Field == "" || !f.Class.Callable() ||
				(f.Lower != "" && (!f.Lower.Callable() || f.Lower.Rank() > f.Class.Rank())) ||
				(f.Exclusions && (f.Lower == "" || len(f.Pausing) > 0)) ||
				slices.ContainsFunc(f.Pausing, func(v any) bool { return !sameScalar(v, v) }) {
				bad("growth: %s operation %s field rule %q needs a field, a callable class, a lowering "+
					"class no higher than it (one for exclusions, which name no pause), and pauses that "+
					"are strings or booleans", c.Kind, op.ID, f.Field)
			}
			for _, v := range f.Values {
				if len(v.Values) == 0 || !v.Class.Callable() {
					bad("growth: %s operation %s field rule %q classes values with none named or a class "+
						"no call is made in", c.Kind, op.ID, f.Field)
				}
			}
			for _, w := range f.While {
				if strings.TrimSpace(w.When) == "" || !w.Class.Callable() || w.Class.Rank() >= f.Class.Rank() {
					bad("growth: %s operation %s field rule %q lowers while in a state with no name or "+
						"to a class no lower than its own", c.Kind, op.ID, f.Field)
				}
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
		if t.TakesOperation() && t.Class != ClassRead && strings.TrimSpace(t.Covers) == "" {
			bad("growth: %s tool %q names methods by ID above read and says nothing of what they change (Covers)",
				c.Kind, t.Name)
		}
		if faults := toolschema.Faults(t.Parameters); len(faults) > 0 {
			bad("growth: %s tool %q has an invalid input schema: %s", c.Kind, t.Name, strings.Join(faults, "; "))
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
			case t.Class == ClassRead && slices.ContainsFunc(op.classes(), func(k Class) bool { return k != ClassRead }):
				// A READ TOOL IS RUN WITHOUT BEING CLASSED ([Hold]), so it names
				// reads alone.
				bad("growth: %s tool %s is read, and its operation %s can come to a write", c.Kind, t.Name, id)
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
	catalogs   = map[string]func() Catalog{}
)

// Register adds a product's catalog, from its package's init: an invalid
// catalog, a second one for a kind, or a tool another catalog lists is a
// build fault, and panics.
func Register(c Catalog) { RegisterFunc(c.Kind, func() Catalog { return c }) }

// RegisterFunc adds kind's catalog, built and validated at first use, so a
// catalog of thousands of rows costs nothing at init (every push's hook runs
// the binary's init).
func RegisterFunc(kind string, build func() Catalog) {
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	if _, dup := catalogs[kind]; dup {
		panic("growth: a second catalog for " + kind)
	}
	built := sync.OnceValue(func() Catalog {
		c := build()
		if c.Kind != kind {
			panic("growth: the catalog registered as " + kind + " says it is " + c.Kind)
		}
		if err := c.Validate(); err != nil {
			panic(err)
		}
		return c
	})
	// A TOOL NAME IS ONE PRODUCT'S. Each starts with its kind's (Validate), so
	// two catalogs can share one only where one kind begins the other's: those
	// pairs alone are built here to compare, and every other stays lazy.
	for other, theirs := range catalogs {
		if !strings.HasPrefix(kind+"_", other+"_") && !strings.HasPrefix(other+"_", kind+"_") {
			continue
		}
		for _, t := range built().Tools {
			if _, listed := theirs().Tool(t.Name); listed {
				panic("growth: " + kind + " lists tool " + t.Name + ", which " + other + " lists")
			}
		}
	}
	catalogs[kind] = built
}

// Catalogs is every registered catalog, by kind.
func Catalogs() []Catalog {
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	out := make([]Catalog, 0, len(catalogs))
	for _, c := range catalogs {
		out = append(out, c())
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
	if !ok {
		return Catalog{}, false
	}
	return c(), true
}

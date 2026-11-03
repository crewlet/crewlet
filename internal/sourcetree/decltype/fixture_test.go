package decltype_test

// fixture is the tree TestTheWalkTypesAKnownTree types, as module
// example.com/m. Each probe carries its verdict: want(EXPR, "verdict") for the
// type of EXPR — a declared type by its full import path and name, a literal
// by its shape ("[]T", "map[K]V", "chan T", "func", "struct", "interface"),
// "unread <package>", "param" (a type parameter nothing binds),
// "unresolved" or "unknown" — and sel(EXPR, "Name",
// "verdict") for what the selector resolves to. Pointers are transparent, as
// a selector makes them, so *T reads as T.
var fixture = map[string]string{
	"a/a.go": `package a

import (
	"context"
	"sync"
	"time"

	"example.com/m/b"
	"ext.example/lib/v2"
	"github.com/mattn/go-sqlite3"
	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

func want(v any, verdict string)       {}
func sel(v any, name, verdict string) {}

type Inner struct {
	N    int
	When time.Time
}

func (Inner) Method() string { return "" }

type Outer struct {
	*Inner
	Name string
	m    map[string]*Inner
	ch   chan b.Item
	fn   func() *Inner
	list []Inner
}

type Shadow struct {
	Inner
	N string
}

type Iface interface {
	Get() *Inner
	b.Getter
}

type Box[T any] struct{ v T }

func (x Box[T]) Get() T { return x.v }

func First[T any](vs []T) T { return vs[0] }

type Op[A, R any] struct{ serve func(A) R }

func Define[A, R any](serve func(A) R) Op[A, R] { return Op[A, R]{serve} }

func Call[A, R any](o Op[A, R], a A) R { return o.serve(a) }

type Alias = Inner

type Defined Inner

type Deep struct{ *Inner }

// Mixed embeds a type the walk does not read beside one it does. A member
// found DEEPER than sync.Mutex may be shadowed by one of Mutex's own, so it is
// undetermined; one found as deep is not, since the two would collide and the
// compiler refuse the selector.
type Mixed struct {
	sync.Mutex
	Deep
}

// Amb holds X twice at one depth, which the compiler refuses.
type L struct{ X int }

type R struct{ X int }

type Amb struct {
	L
	R
}

var pkgVar = &Outer{}

var opInner = Define(func(s string) *Inner { return nil })

var once = sync.OnceValue(func() *Inner { return nil })

var seq func(yield func(*Inner) bool)

func locals(ctx context.Context, o Outer, p *Outer, i Iface, x any, err error, vs ...*Inner) {
	want(o, "example.com/m/a.Outer")
	want(p, "example.com/m/a.Outer")
	want(ctx, "context.Context")
	want(x, "interface")
	want(err, "error")
	want(vs, "[]example.com/m/a.Inner")
	want(pkgVar, "example.com/m/a.Outer")
	local := o.Inner
	want(local, "example.com/m/a.Inner")
	var declared b.Item
	want(declared, "example.com/m/b.Item")
	var typed, untyped = b.Make(), 3
	want(typed, "example.com/m/b.Item")
	want(untyped, "int")
	two, fail := b.Pair()
	want(two, "example.com/m/b.Item")
	want(fail, "error")
	want(otherFile(), "example.com/m/a.Inner")
	want(b.Make(), "example.com/m/b.Item")
	want(b.Var, "example.com/m/b.Item")
	want(b.Make, "func")
	time := Inner{}
	want(time.N, "int")
}

func members(o Outer, i Iface, s Shadow, d Defined, al Alias) {
	want(o.N, "int")
	want(o.Inner.N, "int")
	want(o.When, "time.Time")
	want(o.Method(), "string")
	want(o.Method, "func")
	want(o.fn(), "example.com/m/a.Inner")
	want(o.fn, "func")
	want(i.Get(), "example.com/m/a.Inner")
	want(i.Fetch(), "example.com/m/b.Item")
	want(s.N, "string")
	want(d.N, "int")
	want(al.Method(), "string")

	sel(o, "N", "field example.com/m/a.Inner: int")
	sel(o, "Name", "field example.com/m/a.Outer: string")
	sel(o, "Inner", "field example.com/m/a.Outer: example.com/m/a.Inner")
	sel(o, "Method", "method example.com/m/a.Inner")
	sel(o, "Missing", "absent")
	sel(s, "N", "field example.com/m/a.Shadow: string")
	sel(i, "Get", "method example.com/m/a.Iface (interface)")
	sel(i, "Fetch", "method example.com/m/b.Getter (interface)")
	sel(i, "Name", "method example.com/m/b.Named (interface)")
	sel(d, "Method", "absent")
	sel(d, "N", "field example.com/m/a.Inner: int")
	sel(al, "Method", "method example.com/m/a.Inner")
	sel(Amb{}, "X", "undetermined")
	sel(Mixed{}, "N", "undetermined")
	sel(Mixed{}, "Inner", "field example.com/m/a.Deep: example.com/m/a.Inner")
	sel(Mixed{}, "Deep", "field example.com/m/a.Mixed: example.com/m/a.Deep")
	sel(Mixed{}, "Lock", "beyond sync.Mutex")
	sel(o.When, "Unix", "beyond time.Time")
	sel(once, "X", "fromunread")
	sel(errors(), "Error", "method error (interface)")
	sel(struct{ Q int }{}, "Q", "field -: int")
}

func errors() error { return nil }

func values(o Outer) {
	want((*Inner)(nil), "example.com/m/a.Inner")
	want(Defined{}, "example.com/m/a.Defined")
	want(Alias{}, "example.com/m/a.Inner")
	want(b.Item(b.Make()), "example.com/m/b.Item")
	want(int64(3), "int64")
	want(&Inner{}, "example.com/m/a.Inner")
	want([]string{"a"}, "[]string")
	want(map[string]int{}, "map[string]int")
	want(func() {}, "func")
	want(new(Inner), "example.com/m/a.Inner")
	want(make(chan b.Item), "chan example.com/m/b.Item")
	want(append(o.list, Inner{}), "[]example.com/m/a.Inner")
	want(len(o.list), "int")
	want(1, "int")
	want(1.5, "float64")
	want('r', "rune")
	want("s", "string")
	want(1 == 2, "bool")
	want(!true, "bool")
	want(o.N+1, "int")
	want(1+o.N, "int")
}

func elements(o Outer, arr [3]*Inner) {
	want(o.list[0], "example.com/m/a.Inner")
	want(o.list[1:], "[]example.com/m/a.Inner")
	want(arr[0], "example.com/m/a.Inner")
	want(o.m["k"], "example.com/m/a.Inner")
	got, ok := o.m["k"]
	want(got, "example.com/m/a.Inner")
	want(ok, "bool")
	want(<-o.ch, "example.com/m/b.Item")
	item, open := <-o.ch
	want(item, "example.com/m/b.Item")
	want(open, "bool")
	s := "text"
	want(s[0], "byte")
	for k, v := range o.m {
		want(k, "string")
		want(v, "example.com/m/a.Inner")
	}
	for i, v := range o.list {
		want(i, "int")
		want(v, "example.com/m/a.Inner")
	}
	for v := range o.ch {
		want(v, "example.com/m/b.Item")
	}
	for i, r := range s {
		want(i, "int")
		want(r, "rune")
	}
	for n := range 3 {
		want(n, "int")
	}
	for v := range seq {
		want(v, "example.com/m/a.Inner")
	}
}

func generics(vs []*Inner, bx Box[*Inner]) {
	want(First(vs), "example.com/m/a.Inner")
	want(First[*Inner](nil), "example.com/m/a.Inner")
	want(Call(opInner, "k"), "example.com/m/a.Inner")
	want(opInner, "example.com/m/a.Op")
	want(bx.Get(), "example.com/m/a.Inner")
	want(bx.v, "example.com/m/a.Inner")
	sel(bx, "v", "field example.com/m/a.Box: example.com/m/a.Inner")
	sel(bx, "Get", "method example.com/m/a.Box")
}

func unbound[T any](v T) {
	want(v, "param")
	sel(v, "N", "undetermined")
}

func (x Box[T]) unboundReceiver(v T) {
	want(v, "param")
	want(x.v, "param")
	var local T
	want(local, "param")
	for _, e := range []T{v} {
		want(e, "param")
	}
}

func (x Box[Inner]) shadowingReceiver(v Inner) {
	want(v, "param")
	want(x.v, "param")
}

func outside() {
	want(lib.New(), "ext.example/lib/v2.Client")
	want(lib.New().Conn(), "ext.example/lib/v2.Conn")
	want(lib.New().Conn().Addr, "string")
	sel(lib.New(), "Conn", "method ext.example/lib/v2.Client")
}

func unread(o Outer) {
	want(once, "unread sync")
	want(once(), "unread sync")
	want(once().N, "unread sync")
	want(time.Now(), "unread time")
	want(time.Now, "unread time")
	want(o.When.Location(), "unread time")
	want(nats.Connect, "unread github.com/nats-io/nats.go")
	want(yaml.Marshal, "unread gopkg.in/yaml.v3")
	want(sqlite3.Version, "unread github.com/mattn/go-sqlite3")
	want(time.Duration(5), "unread time")
}

func unknown(x any) {
	switch t := x.(type) {
	case *Inner:
		want(t, "unknown")
		sel(t, "N", "undetermined")
	}
	want(nil, "unknown")
	want(x.(*Inner), "example.com/m/a.Inner")
}

// A METHOD EXPRESSION is typed as the method value its type declares, the
// type named in this file, another package of the module, or a package
// outside it.
func methodExpressions() {
	want(Inner.Method, "func")
	want((*Inner).Method, "func")
	want(Inner.Method(Inner{}), "string")
	want(b.Item.Key, "func")
	want((*lib.Client).Conn, "func")
	want(time.Time.Unix, "unread time")
}
`,

	// Another file of the package, and a renamed import.
	"a/other.go": `package a

import bee "example.com/m/b"

func otherFile() *Inner { return nil }

func renamed() {
	want(bee.Make(), "example.com/m/b.Item")
	want(pkgVar.Name, "string")
}

// A METHOD EXPRESSION on a type another file of the package declares, which
// the parser's file-scoped resolution does not see, and on one of a renamed
// import.
func otherFilesMethodExpressions() {
	want(Inner.Method, "func")
	want((*Inner).Method, "func")
	want(Inner.Method(Inner{}), "string")
	want(bee.Item.Key, "func")
}
`,

	// A file built only on plan9: read, against the scope of the files this
	// platform builds, and adding nothing to it.
	"a/only_plan9.go": `//go:build plan9

package a

type OnlyOnPlan9 struct{}

func plan9() {
	want(otherFile(), "example.com/m/a.Inner")
	want(OnlyOnPlan9{}, "example.com/m/a.OnlyOnPlan9")
}
`,

	// Another package of the module.
	"b/b.go": `package b

type Item struct{ ID string }

func (Item) Key() string { return "" }

type Named interface{ Name() string }

type Getter interface {
	Fetch() Item
	Named
}

func Make() Item { return Item{} }

func Pair() (Item, error) { return Item{}, nil }

var Var Item
`,
}

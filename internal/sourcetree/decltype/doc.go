// Package decltype types a Go expression from the DECLARATIONS it reaches,
// read from source, with no type checker — the one walk a static gate that
// judges a value by its type is built on.
//
// # Why it is a package
//
// A gate asserting an absence by type — no non-test file reads a stream's
// creation instant as the broker reports it (internal/statelog), no deadline
// is set on a client's connection outside internal/api/paced — has to know
// what a value IS, because a name cannot say: the hook relay's deadline on a
// socket it accepted and a controller's on a client's connection are one
// method name. Each of those gates wrote its own walker, and by the second
// gate's review the two copies had drifted exactly as ADR-0008 says copies
// do: one named github.com/nats-io/nats.go `nats.go` and the other could not
// name gopkg.in/yaml.v3; one followed generics through instantiation and
// inference and the other reported every generic receiver untyped; and a bug
// in turning a path into a package was fixed in one copy only. So the walk is
// here, once, and a gate keeps only its own judgement — what it flags, its
// doors, its exemptions — and what each of the walk's answers means for its
// subject.
//
// # Why not go/types
//
// A type checker types every package the judged ones import, which for
// cmd/crewlet is the whole module: measured at 41 s cold with `go list
// -export`, and 10.7 s to type-check one package from source. This reads only
// the declarations a judged expression reaches, on demand, and a package
// once: the stream-identity gate reads 18 packages, NATS's three among them,
// to judge every non-test file of internal/ and cmd/ in 0.35–0.39 s, and the
// pacing gate 16, net/http and net among them, in 0.21–0.22 s — each with the
// `go list` that locates what it reads outside the module, on four vCPUs.
//
// # What it reads
//
// This module's packages, from the tree under the root it is given, and any
// other package a caller names a directory for — [Locate] asks `go list`,
// which builds nothing: the NATS packages from the module cache, net/http
// from the toolchain's tree. Each package is the files this platform builds
// ([World.FileAt] still reads a file built only elsewhere, against that
// scope). Every other package is UNREAD: a type of one is known by its
// identity alone — its package and its name ([World.Named]) — and never by
// its structure, and a value only its declarations could type is
// [Type.Unread].
//
// # How a value is typed
//
// A variable by the declaration the parser's file-scoped resolution names —
// a parameter, a var, a :=, a range clause (a slice's, an array's, a map's, a
// channel's, a string's, an integer's and an iterator function's); a
// package-level one by its package's declarations, in any file; a field by
// the struct it is selected on, a method by the type that declares it or the
// interface that lists it, each promoted through embedding at the shallowest
// depth as the language resolves a selector ([World.Select]) — and a method
// expression (T.M, (*T).M, pkg.T.M) as the method value T declares, whether
// T is declared in the expression's own file, another of its package's or an
// imported package; a call by the results its function, method, interface
// method or function value declares, a conversion and a composite literal by
// their type, a builtin by its own rule; an element by its collection; a
// comma-ok's second value as a bool; a basic literal by its default type.
// Pointers are transparent, as a selector makes them. GENERICS ARE FOLLOWED,
// because every seam of the estate router is one
// (`call[A, R any](…, o op[A, R], …) (R, error)`): a generic type's
// parameters are bound by its instantiation, a generic function's by its
// explicit type arguments or by unifying each parameter's type with its
// argument's, as the compiler infers them, and a generic method's by the
// receiver it is selected on.
//
// # Three answers, never two
//
// A [Type] is KNOWN, UNREAD or neither, and a [Selection] says which declared
// type holds a member, that nothing read holds it while a type the walk does
// not read may (Beyond), that the receiver is itself a value only a package
// the walk does not read can type (FromUnread), that a type read whole holds
// none (Absent: the compiler would refuse the selector, so something was
// misread), or that the walk cannot say (Undetermined) — which includes a
// member found DEEPER than a part it could not read, since that part may hold
// the same name and shadow it. What an answer MEANS is the gate's to say: an
// absence gate that took "cannot tell" for "not there" would certify nothing
// about exactly the values it skipped, and whether a value an unread package
// produced can be what a gate looks for is a fact about that gate's subject,
// not about typing — the stream-identity gate reports one, and the pacing
// gate, which reads net/http, passes one.
//
// # Coverage boundary
//
// Not seen: a dot import; a value reached through reflection, an encoding (a
// value marshalled and decoded as another type) or an interface whose
// dynamic type the walk cannot follow; a type switch's variable and a value
// of a type parameter nothing binds, which are unknown; an element of a
// composite literal whose type is elided; a constant repeating the
// specification before it (const ( a T = iota; b )), which is unknown; a
// method expression's receiver parameter, which its type as the method value
// leaves out; an untyped constant's type where it is not its default type;
// and a method
// declared with an alias of its receiver's type as the receiver, which a
// selector on that type finds Absent.
//
// # Import names
//
// [ImportName] is the one rule for the name an unnamed import is used by.
// It is a convention, not a fact: a package named otherwise is one a file's
// use of is not recognised, and a value reached through it is one the walk
// cannot type — reported by a gate, never passed.
package decltype

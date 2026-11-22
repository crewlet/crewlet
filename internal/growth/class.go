package growth

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/delegated"
)

// Class is what a growth tool or vendor operation does, which decides how the
// engine handles a call to it (package doc, "Six classes").
//
// The constants are also their wire spelling, so an unknown value off the
// wire is a Class that fails [Class.Valid], never a panic.
type Class string

// The six classes.
const (
	// ClassRead reads and changes nothing. Nobody is asked.
	ClassRead Class = "read"
	// ClassOperate is a reversible change that neither starts or raises
	// spend nor publishes: a pause, a lowered budget or bid, a draft edit.
	// Nobody is asked; the access log records it.
	ClassOperate Class = "operate"
	// ClassPublic publishes to customers or the world, or retracts what the
	// integration published: held for an admin at a fresh step-up.
	ClassPublic Class = "public"
	// ClassSpend starts or raises money going out: held for an admin at a
	// fresh step-up, after the spend guard.
	ClassSpend Class = "spend"
	// ClassAdmin manages a vendor account itself: its users, access, keys,
	// billing setup and policies, and every irreversible change that is
	// neither spend nor public. Held for an admin at a fresh step-up.
	ClassAdmin Class = "admin"
	// ClassNever is not offered, with a stated reason ([Operation.Reason]).
	ClassNever Class = "never"
)

// Classes is every class, in rank order ([Class.Rank]).
func Classes() []Class {
	return []Class{ClassRead, ClassOperate, ClassPublic, ClassSpend, ClassAdmin, ClassNever}
}

// Valid reports whether c is a class this build knows.
func (c Class) Valid() bool { return slices.Contains(Classes(), c) }

// Callable reports whether a call of class c may be made at all.
func (c Class) Callable() bool { return c.Valid() && c != ClassNever }

// Held reports whether every call of class c waits for a person's decision.
func (c Class) Held() bool { return c == ClassSpend || c == ClassPublic || c == ClassAdmin }

// Rank orders the classes by how much a call can do, for [Classify]: a write
// that both publishes and spends is spend, and one that also manages the
// account is admin. Never ranks above all, so it can never be outranked.
func (c Class) Rank() int { return slices.Index(Classes(), c) }

// Act is the decision a held class is asked as, "" for a class nobody is
// asked about.
func (c Class) Act() delegated.Kind {
	switch c {
	case ClassSpend:
		return delegated.KindAdSpend
	case ClassPublic:
		return delegated.KindPublicPost
	case ClassAdmin:
		return delegated.KindAccountAdmin
	}
	return ""
}

// Level is the least seat level a call of class c needs, "" for never.
func (c Class) Level() Level {
	switch c {
	case ClassRead:
		return LevelRead
	case ClassOperate, ClassPublic, ClassSpend:
		return LevelOperate
	case ClassAdmin:
		return LevelAdminister
	}
	return ""
}

// Level is a seat's level on a growth lane: what its agent may call or ask.
type Level string

// The three levels, least access first.
const (
	// LevelRead calls read tools: the level a seat block naming none holds.
	LevelRead Level = "read"
	// LevelOperate also calls operate tools and asks for spend and public acts.
	LevelOperate Level = "operate"
	// LevelAdminister also asks for admin acts.
	LevelAdminister Level = "administer"
)

// Levels is every level, least access first.
func Levels() []Level { return []Level{LevelRead, LevelOperate, LevelAdminister} }

// LevelNames is [Levels] as the strings a document writes.
func LevelNames() []string {
	out := make([]string, 0, len(Levels()))
	for _, l := range Levels() {
		out = append(out, string(l))
	}
	return out
}

// Valid reports whether l is a level this build knows.
func (l Level) Valid() bool { return slices.Contains(Levels(), l) }

// Allows reports whether a seat at l may call, or ask for, a call of class c.
func (l Level) Allows(c Class) bool {
	need := c.Level()
	return l.Valid() && need != "" && slices.Index(Levels(), l) >= slices.Index(Levels(), need)
}

// LevelOf is the level a seat block writes, [LevelRead] where it writes none.
// An unknown level is returned as written, and fails [Level.Valid].
func LevelOf(written string) Level {
	if w := strings.TrimSpace(written); w != "" {
		return Level(w)
	}
	return LevelRead
}

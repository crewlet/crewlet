package code

import "github.com/crewlet/crewlet/internal/gitref"

// The domain's names for gitref's rules, so a caller deciding a push asks the
// domain — while the grammar itself is gitref's, one copy below both this
// package and the configuration that validates a declaration with it.

// ZeroOID is git's null object id: the old value of a create and the new value
// of a delete.
const ZeroOID = gitref.ZeroOID

// MaxRefNameBytes is the longest ref a push may create or move.
const MaxRefNameBytes = gitref.MaxRefNameBytes

// ValidName reports whether name is a repository name.
func ValidName(name string) bool { return gitref.ValidRepoName(name) }

// ValidRef reports whether ref is a ref a push may name.
func ValidRef(ref string) bool { return gitref.ValidRef(ref) }

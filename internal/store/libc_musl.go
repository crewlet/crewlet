//go:build linux && musl

package store

// builtForMusl says which of the two linux archives this binary is.
//
// It mirrors the constraint upstream selects its embedded library with —
// turso-go-platform-libs carries libs/linux_{arch} behind `!musl` and
// libs/linux_{arch}_musl behind `musl`, and nothing else in the module reads
// the tag — so this constant and that embed always agree by construction.
//
// The tag is set by the `crewlet-musl` build in .goreleaser.yaml, and by
// `go build -tags musl` for anyone building their own. It is deliberately NOT
// inferred at run time: which library got compiled in is a property of the
// build, and a binary that guessed would be guessing about itself.
const builtForMusl = true

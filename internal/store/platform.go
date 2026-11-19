//go:build !((linux || darwin) && (amd64 || arm64))

package store

// The store builds for four GOOS/GOARCH pairs; this file tells anyone who asks
// for a fifth why they cannot have it.
//
// The limit is the database's: the Turso driver embeds a native library (see
// turso.go) for linux and darwin on amd64 and arm64, plus windows/amd64, and
// nothing else. Windows is out of the release matrix by decision: one working
// architecture beside a silently broken one is worse than neither.
//
// A build failure beats [prepareTursoLibrary]'s runtime error, met only after
// an operator starts a company. The constraint names the SUPPORTED SET, not
// windows: `GOOS=windows GOARCH=arm64 go build` exits 0 against an empty
// embed.FS. The declaration below deliberately does not compile.
type unsupportedPlatform struct{}

var _ unsupportedPlatform = "crewlet builds for linux/amd64, linux/arm64, darwin/amd64 " +
	"and darwin/arm64 only: the Turso database engine ships as a native library, and " +
	"upstream embeds that library for those four platforms and no others. There is no " +
	"second driver to fall back to. See internal/store's package doc."

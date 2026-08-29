//go:build !((linux || darwin) && (amd64 || arm64))

package store

// The store builds for four GOOS/GOARCH pairs, and this file is what tells
// anyone who asks for a fifth why they cannot have it.
//
// Everything else in this package is portable Go, so nothing here is a
// limitation of the ENGINE — it is a limitation of what the database can run
// on. The Turso driver's engine is a native library embedded in the driver
// module and extracted at run time (see turso.go), and upstream embeds it for
// exactly five platforms: linux/amd64, linux/arm64 (each in a glibc and a musl
// variant), darwin/amd64, darwin/arm64 and windows/amd64. There is nothing at
// all for windows/arm64, freebsd, or anything else.
//
// Windows is not in the shipped matrix, and that is a decision rather than an
// absence (decisions/003). windows/amd64 has a library and windows/arm64 does
// not, so the release used to publish two Windows archives of which one could
// never open a store: it failed at its first query unless the operator knew to
// set CREWLET_STORE_DRIVER=sqlite, which was the fallback that has now been
// removed. Shipping one architecture of an operating system and silently
// breaking the other is worse than shipping neither.
//
// # Why a build failure rather than a runtime error
//
// The loader returns "turso library is not embedded for the platform in the
// package", which [prepareTursoLibrary] would surface at the first Open — a
// legible error, but one an operator only meets after installing, configuring
// and starting a company. A build that stops here costs them nothing and says
// the same thing, and the store is not optional: there is no useful subset of
// this engine that runs without one.
//
// The constraint names the SUPPORTED SET rather than excluding windows,
// because the failure it is protecting against was never windows-shaped. It
// was a platform in the release matrix with no embedded library — and
// `GOOS=windows GOARCH=arm64 go build` exits 0 against an empty embed.FS,
// which is exactly how a broken arm64 archive got published. Naming the four
// pairs means the next platform anyone adds fails HERE, whichever one it is,
// instead of at an operator's first query.
//
// The declaration below is deliberate: it does not compile, and the compiler's
// message quotes the string, so `go build` on an unsupported platform prints
// the reason rather than a list of undefined symbols from lock_unix.go.
type unsupportedPlatform struct{}

var _ unsupportedPlatform = "crewlet builds for linux/amd64, linux/arm64, darwin/amd64 " +
	"and darwin/arm64 only: the Turso database engine ships as a native library, and " +
	"those are the platforms Crewlet ships for. See decisions/003-turso-is-the-only-driver.md."

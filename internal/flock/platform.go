//go:build !(linux || darwin)

package flock

// The lock is an flock(2), which the release matrix — linux and darwin, see
// internal/store's platform.go for why there are no others — has and Windows
// does not. The declaration below deliberately does not compile, and the
// compiler's message quotes the string, so a build for another platform says
// why rather than listing the two functions flock_unix.go did not provide.
type unsupportedPlatform struct{}

var _ unsupportedPlatform = "crewlet builds for linux and darwin only: its file locks are " +
	"flock(2), and its database engine ships for those platforms alone. See " +
	"internal/store's package doc."

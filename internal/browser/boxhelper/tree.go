package boxhelper

import (
	"crypto/sha256"
	_ "embed" // the lock's files and the installed digest, embedded by name
	"encoding/hex"
)

// The lock's two files and the digest of a whole install of them, each
// embedded by its name, never as tree/, whose pattern would embed a
// node_modules a bump's npm leaves there: a file missing fails the build.
// [Write] and configJSON are the only readers, and tests read the committed
// files from the source tree.
var (
	//go:embed tree/package.json
	manifest []byte
	//go:embed tree/package-lock.json
	lock []byte
	//go:embed tree/installed.sha256
	installed string
)

// pinDigest names the lock's two files in a box: the first 12 hex digits of
// SHA-256 over each file's hex SHA-256 and a newline, manifest then lock, the
// digest scripts/playwright-tree.sh names the suite's tree by. The tree's
// directory in a box carries it beside the release, so a tree of another lock
// is never read as this one's.
func pinDigest(manifest, lock []byte) string {
	m, l := sha256.Sum256(manifest), sha256.Sum256(lock)
	both := sha256.Sum256([]byte(hex.EncodeToString(m[:]) + "\n" + hex.EncodeToString(l[:]) + "\n"))
	return hex.EncodeToString(both[:])[:12]
}

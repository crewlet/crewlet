package jetstream

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nats-io/nats-server/v2/server"
)

// StoreHasRun reports whether an embedded member has ever started JetStream on
// storeDir — the directory a member's [Config.StoreDir] names, Tier A's
// `stream.store_dir`.
//
// # Why a directory answers it
//
// nats-server keeps a member's whole JetStream state under
// <storeDir>/[server.JetStreamStoreDir] — every stream, and the coordination
// KV beside them, which is where the fleet's secret store lives — and creates
// that directory the first time the member's JetStream comes up, before it
// holds anything. So until it exists, no broker has ever run on storeDir and
// nothing anywhere holds a state this member wrote; once it does, one has.
// The command line asks this of a STOPPED node: before the first start of a
// node that is the fleet's only member, its own secret rows are the company's
// whole store, and after it they are not.
//
// THE SERVER'S OWN CONSTANT, read off the package that writes the directory,
// so this cannot name a directory nats-server stopped writing; the package's
// suite starts a member on an empty directory and asks this of it, so a
// server that wrote its state elsewhere would turn it red.
//
// # Three answers, not two
//
// (false, nil) only where the directory is definitively absent
// ([fs.ErrNotExist]). Any other failure to look — the store directory a file,
// a component of its path a symlink loop — is an error: a caller that read
// it as "never ran" would take a node that may have run for one that has not,
// which is the one mistake the question exists to prevent. An empty storeDir
// is refused the same way, since it names no directory a member keeps.
func StoreHasRun(storeDir string) (bool, error) {
	if strings.TrimSpace(storeDir) == "" {
		return false, errors.New("jetstream: no store directory to look in: " +
			"an embedded member keeps its state under stream.store_dir")
	}
	state := filepath.Join(storeDir, server.JetStreamStoreDir)
	switch _, err := os.Stat(state); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("jetstream: look for a broker's state at %s: %w", state, err)
	}
}

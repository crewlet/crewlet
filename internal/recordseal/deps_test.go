package recordseal_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// THE RECORD SEAL LINKS NOTHING OF THE ENGINE. Every package that signs or
// verifies — the state log, the estate's envelopes, signedask, presence, the
// relay, the sandbox's answer seal — imports this one, so whatever this one
// links, all of them link. One import of a store, a domain or the
// configuration here would bring that graph back into everything that drives
// a box, the coupling the seal left the state log to remove, and nothing
// downstream would notice: the sandbox's own dependency test refuses only the
// state log, the configuration and the code host by name.
//
// So the rule is read from `go list -deps`, what the package's non-test build
// links, and no path of this module but the seal's own is allowed — not a
// list of what is refused, which would let the next engine package through.
//
// Mutation: a blank import of internal/words in recordseal.go — red.
func TestTheRecordSealLinksNothingOfTheEngine(t *testing.T) {
	t.Parallel()
	const (
		module = "github.com/crewlet/crewlet/"
		self   = module + "internal/recordseal"
	)

	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, stderr.String())
	}
	listed, sawSelf := 0, false
	for pkg := range strings.Lines(string(out)) {
		pkg = strings.TrimSpace(pkg)
		listed++
		switch {
		case pkg == self:
			sawSelf = true
		case pkg == strings.TrimSuffix(module, "/") || strings.HasPrefix(pkg, module):
			t.Errorf("internal/recordseal links %s: the seal is a leaf every signing package imports, "+
				"so it links nothing of the engine", pkg)
		}
	}
	// The standard library alone brings crypto/hmac, crypto/sha256 and their
	// own graph; a listing this short, or one without the seal, read
	// something else and would pass whatever this package linked.
	if !sawSelf || listed < 10 {
		t.Fatalf("go list -deps named %d packages and not internal/recordseal among them: it read something else",
			listed)
	}
}

package store_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NOTHING REACHES THE REPLICATED ESTATE'S POOL DIRECTLY.
//
// `DB.SQL()` answers a NIL `*sql.DB` on a handle that is not open, and a
// closed replicated peer is a LEGITIMATE, DOCUMENTED state rather than a
// fault: an adoption closes it between its rename and its reopen, and `Close`
// leaves it nil. A statement issued on that pool does not answer an error — it
// dereferences nil three frames inside database/sql and takes the process
// down.
//
// Six call sites had that shape, and the one that fired did so in the trim's
// own tick, which runs on a timer against an estate a shutdown is closing:
//
//	database/sql.(*DB).conn(0x0, …)
//	tracker.Evictions(…)
//	engine.(*retention).tombstones(…)
//
// Every one of them now goes through [DB.Read] or [DB.Tx], which answer
// [ErrNoEstate]. A caller in flight then reaches a closed estate honestly, and
// every one of these readers already had a branch for an unreadable one.
//
// A STRUCTURAL TEST, because the defect is structural: a behavioural one would
// have to close an estate underneath a running loop at the instant it issues a
// statement, which is a race a test can only lose reliably.
func TestNothingIssuesAStatementOnTheReplicatedPool(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"x.db.Replicated().SQL().QueryContext(ctx, q)",
		"db := g.db.Replicated().SQL()",
	} {
		if !reachesTheReplicatedPool(positive) {
			t.Errorf("control: %q reaches the pool and the matcher did not "+
				"flag it", positive)
		}
	}
	for _, negative := range []string{
		"x.db.Replicated().Read(ctx, fn)",
		"d.SQL().QueryContext(ctx, q)", // the NODE estate, which one Open owns
	} {
		if reachesTheReplicatedPool(negative) {
			t.Errorf("control: %q was flagged, so this guard fails on the "+
				"correct shape", negative)
		}
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "static":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			if reachesTheReplicatedPool(line) {
				rel, _ := filepath.Rel(root, path)
				found = append(found, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	for _, site := range found {
		t.Errorf("%s issues a statement on the replicated estate's pool, which "+
			"is NIL whenever that estate is not open — an adoption's rename or "+
			"a shutdown makes it so, and the statement panics inside "+
			"database/sql rather than answering ErrNoEstate. Go through Read "+
			"or Tx on the handle", site)
	}
}

// reachesTheReplicatedPool reports whether a line takes the replicated
// estate's connection pool rather than going through its handle.
func reachesTheReplicatedPool(line string) bool {
	return strings.Contains(line, "Replicated().SQL()")
}

// itoa avoids importing strconv for one call in a test whose subject is a walk.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

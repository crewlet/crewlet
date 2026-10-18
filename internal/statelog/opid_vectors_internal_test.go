package statelog

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// opIDVector is one fixed mint: the instant, the ten tail bytes and the name
// in, and the id out.
type opIDVector struct {
	UnixMS int64  `json:"unix_ms"`
	Tail   string `json:"tail"`
	Name   string `json:"name"`
	ID     string `json:"id"`
}

// A CLIENT THAT MINTS ITS OWN OPERATION ID MINTS THIS GRAMMAR'S, BYTE FOR BYTE.
//
// The command line and the dashboard both mint a gesture's id before they send
// it, because the node finishes a gesture whatever happens to the connection
// and an id minted on the node is lost with an answer that never arrives. The
// command line mints through [NewOpID]; the dashboard cannot import it and
// writes the layout itself, in TypeScript. The vectors in
// testdata/opid_vectors.json are the contract between the two: this test holds
// [layout] and [named] to them and holds every id to [CheckCallerOpID] and
// [OpMintedAt], and the dashboard's own suite holds its minter to the same
// file. A drift on either side is a failing test rather than a gesture the
// route refuses `op_id_invalid` — or worse, one it accepts at the wrong
// instant, which the ledger's vouching reads.
func TestAMintedOperationIDMatchesTheSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/opid_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []opIDVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors, so this test certifies nothing")
	}
	for _, v := range vectors {
		b, err := hex.DecodeString(v.Tail)
		if err != nil || len(b) != 10 {
			t.Fatalf("vector %s: tail %q is not ten bytes of hex", v.ID, v.Tail)
		}
		var tail [10]byte
		copy(tail[:], b)
		if got := named(layout(time.UnixMilli(v.UnixMS), tail), v.Name); got != v.ID {
			t.Errorf("layout(%d, %s) named %q = %q, want %q",
				v.UnixMS, v.Tail, v.Name, got, v.ID)
		}
		if err := CheckCallerOpID(v.ID); err != nil {
			t.Errorf("the vector %q is refused: %v", v.ID, err)
		}
		at, minted := OpMintedAt(v.ID)
		if !minted || at.UnixMilli() != v.UnixMS {
			t.Errorf("OpMintedAt(%q) = %v %v, want the instant %d", v.ID, at, minted,
				v.UnixMS)
		}
	}
}

// opNameVector is one value written into an operation id's name: the value in,
// the part out ([OpNamePart]).
type opNameVector struct {
	Value string `json:"value"`
	Part  string `json:"part"`
}

// A CLIENT THAT WRITES A VALUE INTO AN OPERATION'S NAME WRITES IT AS
// [OpNamePart] DOES, BYTE FOR BYTE.
//
// The dashboard names a gate gesture's id by its node, which may hold the dot
// that begins a step, and so escapes it as this package does — in TypeScript,
// over a digest of its own, since the browser's is not available to it. The
// vectors in testdata/opname_vectors.json are the contract between the two,
// as the layout's are: this test holds [OpNamePart] to them and the
// dashboard's own suite holds its escape to the same file, with every value
// written as itself and every escape on both sides of the bound among them.
func TestAValueIsWrittenIntoANameAsTheSharedVectorsSay(t *testing.T) {
	raw, err := os.ReadFile("testdata/opname_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []opNameVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	var kept, escaped int
	for _, v := range vectors {
		if got := OpNamePart(v.Value); got != v.Part {
			t.Errorf("OpNamePart(%q) = %q, want %q", v.Value, got, v.Part)
		}
		if v.Part == v.Value {
			kept++
		} else {
			escaped++
		}
	}
	if kept == 0 || escaped == 0 {
		t.Fatalf("the vectors keep %d value(s) and escape %d — a file without "+
			"both certifies only half the rule", kept, escaped)
	}
}

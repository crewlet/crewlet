package sandbox

import (
	"strings"
	"testing"
)

// A JSON-RPC ID IS NAMED AS THE STORE KEEPS ITS CALL. The bridge names a hook's
// call and the call a cancel gives up by NoteCallID, and a serve keeps a call
// by keptCallOf: an id the first names is one the second keeps whole, and one
// the first refuses is one no serve keeps, so a cancel can only ever name a
// call as a serve recorded it.
//
// Mutation: name a string id unquoted, or name one past the bound or outside
// the charset, and a case goes red.
func TestAJSONRPCIDIsNamedAsTheStoreKeepsItsCall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		id   any
		want string
		ok   bool
	}{
		{id: int64(7), want: "7", ok: true},
		{id: int64(-12), want: "-12", ok: true},
		{id: int64(0), want: "0", ok: true},
		{id: "a-1", want: `"a-1"`, ok: true},
		{id: strings.Repeat("q", MaxNoteCallBytes-2), want: `"` + strings.Repeat("q", MaxNoteCallBytes-2) + `"`,
			ok: true},
		{id: strings.Repeat("q", MaxNoteCallBytes-1)},
		{id: "not an id!"},
		{id: `a"b`},
		{id: float64(7)},
		{id: nil},
	} {
		got, ok := NoteCallID(c.id)
		if got != c.want || ok != c.ok {
			t.Errorf("NoteCallID(%#v) = %q, %v, want %q, %v", c.id, got, ok, c.want, c.ok)
		}
		if !ok {
			continue
		}
		call := &NoteCall{Session: "mcp-session-1", ID: got}
		if kept := keptCallOf(call); kept == nil || kept.ID != got {
			t.Errorf("a serve keeps the call %q as %+v, want it whole", got, kept)
		}
	}
}

package host

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A REFUSAL LINE FITS ONE PKT-LINE, newline included, whatever its ref: the
// ref whole — git matches a status to its command by the ref's name — and the
// sentence cut to the room left, marked as cut. The longest ref a command line
// carries (its pkt-line less both ids and their spaces) left a full sentence a
// line past git's largest, and git read the push's whole report as a protocol
// error.
func TestARefusalLineFitsOnePktLine(t *testing.T) {
	longest := "refs/heads/" + strings.Repeat("r", maxPktPayload-len(code.ZeroOID+" "+code.ZeroOID+" ")-
		len("refs/heads/"))
	// The longest sentence a line carries whole: maxSentence with its code.
	detail := strings.Repeat("d", maxSentence-len(code.RefusalStale+": "))
	for _, tc := range []struct {
		name string
		a    answered
		cut  bool
	}{
		{name: "a short ref keeps its whole sentence", a: answered{Ref: "refs/heads/main", Reason: code.RefusalStale,
			Detail: detail}},
		{name: "the longest ref, a code's own sentence", a: answered{Ref: longest, Reason: code.RefusalBadRef},
			cut: true},
		{name: "the longest ref, a receiver's sentence", a: answered{Ref: longest, Detail: detail}, cut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.a.line()
			if n := len(line + "\n"); n > maxPktPayload {
				t.Errorf("the line and its newline are %d bytes, past the %d one pkt-line carries", n,
					maxPktPayload)
			}
			if !strings.HasPrefix(line, "ng "+tc.a.Ref+" ") {
				t.Errorf("the line does not name its ref whole")
			}
			words := strings.TrimPrefix(line, "ng "+tc.a.Ref+" ")
			if cut := strings.HasSuffix(words, "…"); cut != tc.cut {
				t.Errorf("the sentence %q was cut %v, want %v", words, cut, tc.cut)
			}
		})
	}
}

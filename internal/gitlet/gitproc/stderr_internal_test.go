package gitproc

import (
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// refusingStream writes the text as it came, as a refusing [redact.Stream] may
// have, and refuses it at the end.
type refusingStream struct{ w io.Writer }

func (r refusingStream) Write(p []byte) (int, error) { return r.w.Write(p) }
func (r refusingStream) Close() error                { return redact.ErrDecidedEarly }

// A REMOTE RUN WHOSE REDACTION REFUSED SHOWS NONE OF ITS STDERR: what the
// stream wrote before refusing may hold what redaction takes out, so the tail
// is a line saying it was withheld, never those bytes.
//
// Mutation: show what the stream wrote anyway, and the case goes red.
func TestARefusedRemoteStderrIsWithheld(t *testing.T) {
	t.Parallel()
	s := newStderrSink(true, nil)
	s.redacting = refusingStream{&s.kept}
	if _, err := s.Write([]byte("remote: password: hunter2x9\nfatal: the remote hung up")); err != nil {
		t.Fatal(err)
	}
	s.finish()
	if got := s.tail(); got != withheldStderr || strings.Contains(got, "hunter2x9") {
		t.Errorf("a refused redaction's tail is %q, want %q", got, withheldStderr)
	}
}

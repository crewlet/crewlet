package ssh

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// timedOut is a dial that gave up waiting, as the net package reports one.
type timedOut struct{}

func (timedOut) Error() string   { return "i/o timeout" }
func (timedOut) Timeout() bool   { return true }
func (timedOut) Temporary() bool { return true }

// dialFailing is a dialer whose every dial fails with err.
func dialFailing(err error) dialer {
	return func(context.Context, string, string) (net.Conn, error) { return nil, err }
}

// The ways a dial fails, as the net package reports each.
var dialFailures = []struct {
	name string
	err  error
	// said must appear in the words; operator (the dial's text) must not.
	said     []string
	operator []string
}{
	{
		name: "a name that does not resolve",
		err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err: "no such host", Name: "web-1.example.com", Server: "127.0.0.53:53", IsNotFound: true}},
		said:     []string{"web-1.example.com does not resolve to an address", "Check the host name"},
		operator: []string{"127.0.0.53", "dial tcp", "lookup", "no such host"},
	},
	{
		name: "a resolver that did not answer",
		err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err: "server misbehaving", Name: "web-1.example.com", Server: "127.0.0.53:53", IsTemporary: true}},
		said:     []string{"web-1.example.com could not be looked up just now", "Try again"},
		operator: []string{"127.0.0.53", "misbehaving"},
	},
	{
		name: "a port nothing listens on",
		err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{
			Syscall: "connect", Err: syscall.ECONNREFUSED}},
		said:     []string{"web-1.example.com refused the connection on port 2222", "Check the port"},
		operator: []string{"connect:", "dial tcp"},
	},
	{
		name:     "a server that never answers",
		err:      &net.OpError{Op: "dial", Net: "tcp", Err: timedOut{}},
		said:     []string{"Nothing answered at web-1.example.com on port 2222 within 10s", "firewall"},
		operator: []string{"i/o timeout", "dial tcp"},
	},
	{
		name:     "a name that resolves to the engine host",
		err:      errors.Join(ErrUnreachableAddress, errors.New("127.0.0.1:2222")),
		said:     []string{"web-1.example.com resolves to the engine host or a link-local address"},
		operator: []string{"127.0.0.1", "ssh:"},
	},
}

// An unreachable server is said by how it failed, with that cause's remedy and
// none of the dial's text, alike by Scan and Test connection's reach step.
func TestAServerThatCannotBeReachedIsSaidByHowItFailed(t *testing.T) {
	t.Parallel()
	for _, tc := range dialFailures {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := scan(t.Context(), dialFailing(tc.err), "web-1.example.com", 2222)
			failed, ok := errors.AsType[*ScanError](err)
			if !ok {
				t.Fatalf("the scan failed with %v, want a ScanError", err)
			}
			reach := reachDetail("web-1.example.com:2222", tc.err)
			for _, words := range []struct{ where, text string }{{"scan", failed.Said}, {"reach step", reach}} {
				for _, want := range tc.said {
					if !strings.Contains(words.text, want) {
						t.Errorf("the %s says %q, want %q in it", words.where, words.text, want)
					}
				}
				for _, not := range tc.operator {
					if strings.Contains(words.text, not) {
						t.Errorf("the %s says %q, which carries the operator's %q", words.where, words.text, not)
					}
				}
			}
			// The chain is kept for the log.
			if !errors.Is(err, tc.err) {
				t.Errorf("the scan's error %v lost the dial's", err)
			}
		})
	}
}

// A server that answers with no key to pin is sent to paste its keys by hand.
func TestAServerThatAnswersWithNoKeyToPinIsSentToItsKeyFiles(t *testing.T) {
	t.Parallel()
	// A connection whose far end hangs up at once: reached, and no handshake.
	answered := func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	_, err := scan(t.Context(), answered, "web-1.example.com", 22)
	failed, ok := errors.AsType[*ScanError](err)
	if !ok || !errors.Is(err, ErrNoHostKey) {
		t.Fatalf("the scan failed with %v, want a ScanError over ErrNoHostKey", err)
	}
	for _, want := range []string{"web-1.example.com answered on port 22", "no host key Crewlet can pin", "/etc/ssh/ssh_host_*_key.pub"} {
		if !strings.Contains(failed.Said, want) {
			t.Errorf("the scan says %q, want %q in it", failed.Said, want)
		}
	}
}

// A port out of range is said with the range.
func TestAScanOfAPortOutOfRangeSaysTheRange(t *testing.T) {
	t.Parallel()
	_, err := scan(t.Context(), dialFailing(errors.New("not dialled")), "web-1.example.com", 70000)
	failed, ok := errors.AsType[*ScanError](err)
	if !ok || !strings.Contains(failed.Said, "1 to 65535") {
		t.Fatalf("the scan failed with %v, want the port's range said", err)
	}
}

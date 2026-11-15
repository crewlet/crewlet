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

// The ways a dial to a server fails, as the net package reports each: what a
// scan and the reach step of Test connection are handed.
var dialFailures = []struct {
	name string
	err  error
	// said is what the person must be told; operator lists what they must
	// not be: the dial's own words, the resolver's address.
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
		said:     []string{"Nothing answered at web-1.example.com on port 2222 within 10 seconds", "firewall"},
		operator: []string{"i/o timeout", "dial tcp"},
	},
	{
		name:     "a name that resolves to the engine host",
		err:      errors.Join(ErrUnreachableAddress, errors.New("127.0.0.1:2222")),
		said:     []string{"web-1.example.com resolves to the engine host or a link-local address"},
		operator: []string{"127.0.0.1", "ssh:"},
	},
}

// A SERVER THAT CANNOT BE REACHED IS SAID BY HOW IT FAILED, for the person who
// typed its address: a name that does not resolve is told to check the name,
// a refusing port to check the port, a silent server to check the firewall,
// and none of them reads the dial's own error. The scan before a server is
// added and Test connection's reach step dial the same way and say the same.
//
// Mutation: answer err.Error() in either place, or drop a case from
// unreachedWords so it falls to the last one, and this is red.
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
			// THE CHAIN IS KEPT for the operator's log, and its sentinels match.
			if !errors.Is(err, tc.err) {
				t.Errorf("the scan's error %v lost the dial's", err)
			}
		})
	}
}

// A SERVER THAT ANSWERS WITH NO KEY TO PIN is told apart from one that does not
// answer: it was reached, so the person is sent to paste its keys by hand.
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

// A PORT THAT CANNOT BE DIALLED is said with the range a port takes.
func TestAScanOfAPortOutOfRangeSaysTheRange(t *testing.T) {
	t.Parallel()
	_, err := scan(t.Context(), dialFailing(errors.New("not dialled")), "web-1.example.com", 70000)
	failed, ok := errors.AsType[*ScanError](err)
	if !ok || !strings.Contains(failed.Said, "1 to 65535") {
		t.Fatalf("the scan failed with %v, want the port's range said", err)
	}
}

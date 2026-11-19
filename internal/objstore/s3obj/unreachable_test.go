package s3obj_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// A DIAL NO ATTEMPT CAN COMPLETE IS NOT ASKED AGAIN, and every other failure to
// connect is. An unparsable address (port past 65535, IPv6 zone holding a `]`,
// too many colons, a bracket inside a name) and port 0 are final at the first
// attempt; a refused closed port, an unreachable network, a timeout, a
// resolver not yet answering and an answer with no address for the network are
// asked again, as the SDK asks every failure to send. Errors are the dialer's
// own, wrapped as the SDK wraps a failed send and judged by the client's own
// retryer, so a Go that rewords one fails here.
// Mutation: drop unreachableByConstruction from the retryables, and every
// final case is asked again; read every *net.AddrError as final, and "no
// suitable address found" is no longer asked again; drop the port-0 rule, and
// port 0 is asked again.
func TestADialNoAttemptCanCompleteIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	retryer := s3obj.ClientRetryer(openRetrying(t, s3fake.Start(t, "files"), "", s3obj.Clock{}))
	dial := func(network, address string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			conn.Close()
			t.Fatalf("dialed %s %s, which this case needs refused", network, address)
		}
		return err
	}
	// A PORT NOTHING LISTENS ON, so a dial to it is refused as a server not
	// up yet is.
	closed := httpxtest.Refusing(t)
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, timedOut := (&net.Dialer{}).DialContext(expired, "tcp", closed)

	for name, tc := range map[string]struct {
		err   error
		final bool
	}{
		"a port past 65535":         {dial("tcp", "127.0.0.1:65536"), true},
		"an IPv6 zone holding a ]":  {dial("tcp", "[fe80::1%eth0]]:443"), true},
		"too many colons":           {dial("tcp", "::1:9000"), true},
		"a [ inside a name":         {dial("tcp", "minio[1]:9000"), true},
		"a ] inside a name":         {dial("tcp", "minio]1:9000"), true},
		"a connection to port 0":    {dial("tcp", "127.0.0.1:0"), true},
		"a closed port":             {dial("tcp", closed), false},
		"no address of the network": {dial("tcp4", "[::1]:9000"), false},
		"a timeout":                 {timedOut, false},
		"an unreachable network": {&net.OpError{Op: "dial", Net: "tcp",
			Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 9000},
			Err:  os.NewSyscallError("connect", syscall.ENETUNREACH)}, false},
		"a resolver not answering yet": {&net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "server misbehaving", Name: "minio.example", IsTemporary: true}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tc.err == nil {
				t.Fatal("the premise: the dial failed")
			}
			sent := &smithyhttp.RequestSendError{Err: &url.Error{Op: "Head",
				URL: "http://127.0.0.1:9000/files", Err: tc.err}}
			if asked := retryer.IsErrorRetryable(sent); asked == tc.final {
				t.Errorf("%v asked again = %v, want %v", tc.err, asked, !tc.final)
			}
		})
	}
}

// AN ENDPOINT NO DIAL CAN REACH FAILS AT ONCE, the check of the bucket that
// opens the client included: under a deadline of ten seconds Open fails in a
// moment, naming the dialer's own error, where it asked the dial again until
// that deadline — and a node's boot, whose check is held to no deadline that
// short, until the minute a failing request is given up after and the
// check's clean-up after it: 77 to 86 s to refuse an address it could never
// reach. Config validation refuses each of these endpoints before a node
// starts; this is the client's own judgement, for what validation cannot see.
//
// Mutation: drop unreachableByConstruction from the retryables, and each case
// ends at its deadline.
func TestAnEndpointNoDialCanReachFailsAtOnce(t *testing.T) {
	t.Parallel()
	for name, endpoint := range map[string]string{
		"a port past 65535":        "http://127.0.0.1:65536",
		"port 0":                   "http://127.0.0.1:0",
		"an IPv6 zone holding a ]": "http://[fe80::1%25eth0]]:9000",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			began := time.Now()
			_, err := s3obj.Open(ctx, s3obj.Config{Endpoint: endpoint, Region: "us-east-1",
				Bucket: "files", PathStyle: true, AccessKeyID: "AKIAEXAMPLE",
				SecretAccessKey: "example-secret"})
			took := time.Since(began)
			if err == nil {
				t.Fatalf("opened a bucket at %s", endpoint)
			}
			if errors.Is(err, context.DeadlineExceeded) || took > 5*time.Second {
				t.Fatalf("Open at %s ended after %v with %v; want the dial given up at its first "+
					"attempt", endpoint, took.Round(time.Millisecond), err)
			}
			if _, ok := errors.AsType[*net.OpError](err); !ok {
				t.Errorf("Open at %s = %v; want the dialer's own error", endpoint, err)
			}
		})
	}
}

package httpxtest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
)

// Refusing is a loopback address that refuses every connection until the test
// ends: a socket holds its port bound and never listens, so a dial is reset
// and no listener, a parallel test's server included, is given the port. A
// port found free and closed again is free for exactly that.
func Refusing(tb testing.TB) string {
	tb.Helper()
	l := refusing(tb)
	tb.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// Gone is a started server nothing reaches, its URL a [Refusing] address, for
// a double whose subject is handed the server itself and must find it gone.
// Close it as any httptest.Server, which lets its port go.
func Gone(tb TB) *httptest.Server {
	tb.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	_ = srv.Listener.Close()
	srv.Listener = refusing(tb)
	srv.Start()
	return srv
}

// refusing binds a loopback port that never listens, as a listener nothing
// is accepted on.
func refusing(tb TB) *refusingListener {
	tb.Helper()
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		tb.Fatalf("httpxtest: open a socket: %v", err)
	}
	l := &refusingListener{fd: fd, closed: make(chan struct{})}
	if err = syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = l.Close()
		tb.Fatalf("httpxtest: bind a loopback port: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	bound, ok := sa.(*syscall.SockaddrInet4)
	if err != nil || !ok {
		_ = l.Close()
		tb.Fatalf("httpxtest: read the bound port: %v (%T)", err, sa)
	}
	l.addr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bound.Port}
	return l
}

// refusingListener holds a bound socket that never listens: Accept waits for
// Close, which lets the port go.
type refusingListener struct {
	fd     int
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func (l *refusingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *refusingListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		_ = syscall.Close(l.fd)
	})
	return nil
}

func (l *refusingListener) Addr() net.Addr { return l.addr }

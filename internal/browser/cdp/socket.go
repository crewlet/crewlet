package cdp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/coder/websocket"
)

// DialOptions is how [Dial] reaches a browser's DevTools socket.
type DialOptions struct {
	// HTTPClient carries the upgrade: the caller's own client, built by
	// internal/httpx (httpx.Guarded for an address a vendor named). Required:
	// nil is ErrNoHTTPClient, and nothing is dialled. Dial copies it and
	// refuses every redirect on the copy; the caller's client keeps its own
	// policy. The websocket library applies its Timeout to the handshake
	// alone.
	HTTPClient *http.Client
	// Header is sent on the upgrade: a pool's credentials, for one.
	Header http.Header
	// Host overrides the upgrade's Host header. A plain Chromium refuses an
	// upgrade whose Host is a DNS name rather than an address, so a caller
	// that reached it by a name says the address here.
	Host string
	// Logger takes the connection's DEBUG lines; nil is logging.Get("cdp").
	Logger *slog.Logger
}

// Dial connects to a browser's DevTools websocket at addr (a ws:// or wss://
// address Chromium printed or a pool handed out) over o.HTTPClient, and starts
// the connection; ctx bounds the dial alone. A message it reads past
// [ReadLimit] is read through and ends only what it is for, and it writes text
// frames. An error never names addr's query or credentials, which a vendor's
// address may carry.
func Dial(ctx context.Context, addr string, o DialOptions) (*Conn, error) {
	if o.HTTPClient == nil {
		return nil, ErrNoHTTPClient
	}
	client := *o.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirected }
	ws, resp, err := websocket.Dial(ctx, addr, &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: o.Header,
		Host:       o.Host,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, &dialError{url: redacted(addr), err: err}
	}
	// The library's own limit is lifted: it refuses a message past its limit
	// by closing the socket, which would end every session on it. The reader
	// holds a message to ReadLimit itself, and reads one past it through.
	ws.SetReadLimit(-1)
	return newConn(ctx, &socket{ws: ws, buf: make([]byte, framesBuffer)}, o.Logger), nil
}

// dialError is a failed Dial, naming the address without its query or
// credentials and the transport's own reason without the address it embeds.
type dialError struct {
	url string
	err error
}

func (e *dialError) Error() string {
	reason := e.err
	var u *url.Error
	if errors.As(e.err, &u) {
		// The HTTP client's error names the whole address, query and all.
		reason = u.Err
	}
	return fmt.Sprintf("cdp: dial %s: %v", e.url, reason)
}

func (e *dialError) Unwrap() error { return e.err }

// redacted is raw without its user information and query: scheme, host and
// path.
func redacted(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "an address that does not parse"
	}
	clean := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return clean.String()
}

// socket is the websocket transport.
type socket struct {
	ws   *websocket.Conn
	buf  []byte // the reader's own, framesBuffer long: read is its one caller
	once sync.Once
}

// read reads the next message a buffer at a time, to its end, as the library
// requires of every message: one past ReadLimit is read through and kept only
// as its start, its end and its size (gather).
func (s *socket) read(ctx context.Context) (message, error) {
	_, r, err := s.ws.Reader(ctx)
	if err != nil {
		return message{}, err
	}
	var g gather
	for {
		n, err := r.Read(s.buf)
		g.add(s.buf[:n])
		switch {
		case errors.Is(err, io.EOF):
			return g.message(), nil
		case err != nil:
			return message{}, err
		}
	}
}

// write sends one text frame under the connection's own context, which never
// ends: the connection's writer bounds the write, and closes the socket at that
// bound.
func (s *socket) write(ctx context.Context, msg []byte) error {
	return s.ws.Write(ctx, websocket.MessageText, msg)
}

// close ends the socket at once, without the close handshake, which would wait
// on a peer that may be what failed.
func (s *socket) close() error {
	var err error
	s.once.Do(func() { err = s.ws.CloseNow() })
	return err
}

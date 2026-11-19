package artifactrender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactrender/internal/cdp"
)

// An operator-run headless Chromium: a render opens a context and a tab of its
// own in it, and every request there is answered by the engine (serve.go).

// discoverTimeout bounds the read of a browser's /json/version, and
// discoverBytes its answer, a few hundred bytes of JSON.
const (
	discoverTimeout = 10 * time.Second
	discoverBytes   = 64 << 10
)

// closeTimeout bounds the context's disposal at a render's end, under a
// context of its own: the render's may be the one that ended.
const closeTimeout = 5 * time.Second

// dial connects to the configured browser's socket, or the one its
// /json/version names rebased onto the configured address. The Host is sent
// as an address: Chromium refuses a DevTools request named by host.
func (r *Renderer) dial(ctx context.Context) (*cdp.Conn, error) {
	u, err := url.Parse(r.browser)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBrowser, err)
	}
	host, err := hostHeader(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s: %w", ErrBrowser, u.Hostname(), err)
	}
	socket := r.browser
	if u.Scheme == "http" || u.Scheme == "https" {
		socket, err = r.discover(ctx, u, host)
		if err != nil {
			return nil, err
		}
	}
	conn, err := cdp.Dial(ctx, socket, cdp.DialOptions{HTTPClient: r.client, Host: host,
		Logger: log})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBrowser, err)
	}
	if _, err := cdp.CheckVersion(ctx, conn.Browser()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrBrowser, err)
	}
	return conn, nil
}

// hostHeader is u's host as Chromium admits it: an address, or localhost,
// with u's port.
func hostHeader(ctx context.Context, u *url.URL) (string, error) {
	name := u.Hostname()
	if name == "localhost" || net.ParseIP(name) != nil {
		return u.Host, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New("it resolves to no address")
	}
	ip := addrs[0].IP.String()
	if port := u.Port(); port != "" {
		return net.JoinHostPort(ip, port), nil
	}
	if strings.Contains(ip, ":") {
		return "[" + ip + "]", nil
	}
	return ip, nil
}

// discover reads the browser's socket address from its /json/version, and
// rebases it onto base: Chromium names its socket under the address it
// listens on, which is not always the one the engine reaches it at.
func (r *Renderer) discover(ctx context.Context, base *url.URL, host string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.Scheme+"://"+base.Host+
		"/json/version", nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBrowser, err)
	}
	req.Host = host
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: read %s/json/version: %w", ErrBrowser, base.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoverBytes))
	if err != nil {
		return "", fmt.Errorf("%w: read %s/json/version: %w", ErrBrowser, base.Host, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %s/json/version answered %s", ErrBrowser, base.Host, resp.Status)
	}
	var v struct {
		Socket string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &v) != nil || v.Socket == "" {
		return "", fmt.Errorf("%w: %s/json/version names no webSocketDebuggerUrl", ErrBrowser,
			base.Host)
	}
	named, err := url.Parse(v.Socket)
	if err != nil || !strings.HasPrefix(named.Path, "/devtools/browser/") {
		return "", fmt.Errorf("%w: %s/json/version names no browser socket", ErrBrowser, base.Host)
	}
	scheme := "ws"
	if base.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + base.Host + named.Path, nil
}

// tab is a render's own browser context and the one tab in it.
type tab struct {
	conn    *cdp.Conn
	context cdp.BrowserContextID
	target  cdp.TargetID
	page    *cdp.Session
}

// openTab creates a browser context and a tab on about:blank in it, and
// attaches to the tab.
func openTab(ctx context.Context, conn *cdp.Conn) (*tab, error) {
	b := conn.Browser()
	bc, err := cdp.Call(ctx, b, cdp.TargetCreateBrowserContext,
		cdp.TargetCreateBrowserContextParams{DisposeOnDetach: true})
	if err != nil {
		return nil, fmt.Errorf("%w: create a browser context: %w", ErrBrowser, err)
	}
	t := &tab{conn: conn, context: bc.BrowserContextID}
	created, err := cdp.Call(ctx, b, cdp.TargetCreateTarget,
		cdp.TargetCreateTargetParams{URL: "about:blank", BrowserContextID: bc.BrowserContextID})
	if err != nil {
		t.close(ctx)
		return nil, fmt.Errorf("%w: open a tab: %w", ErrBrowser, err)
	}
	t.target = created.TargetID
	attached, err := cdp.Call(ctx, b, cdp.TargetAttachToTarget,
		cdp.TargetAttachToTargetParams{TargetID: created.TargetID, Flatten: true})
	if err != nil {
		t.close(ctx)
		return nil, fmt.Errorf("%w: attach to its tab: %w", ErrBrowser, err)
	}
	t.page = conn.Session(attached.SessionID)
	return t, nil
}

// close disposes of the context, its tab with it, under ctx stripped of its
// end: the render's own may be what ended.
func (t *tab) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if _, err := cdp.Call(ctx, t.conn.Browser(), cdp.TargetDisposeBrowserContext,
		cdp.TargetDisposeBrowserContextParams{BrowserContextID: t.context}); err != nil {
		log.Debug("artifact_render_context_left", "error", err.Error())
	}
}

// Package httpx is the one HTTP transport every outbound client in this
// engine shares.
//
// It exists because leaving http.Client.Transport nil is not "the default
// settings" — it is http.DefaultTransport, whose MaxIdleConnsPerHost is 2,
// process-wide, across every client that shares it. Seven vendor clients did
// exactly that, so a company with more than two concurrent turns against one
// endpoint paid a fresh TCP and TLS handshake on every call past the second.
// Self-hosted Mattermost and GitLab are precisely the HTTP/1.1 endpoints where
// that costs a full round trip rather than a stream on an existing connection.
//
// ONE transport, not one per client. A per-client clone would be strictly
// worse than sharing the default: each would keep its own pool, so N clients
// against one host would hold N pools of idle connections and still reuse
// nothing between them. The whole value is that every caller to a host lands
// in the same pool.
package httpx

import (
	"net/http"
	"sync"
	"time"
)

// MaxIdleConnsPerHost is how many warm connections one host keeps.
//
// Anchored to node.DefaultMaxConcurrent (32), the per-node ceiling on
// simultaneous agent turns: that is the most callers that can plausibly be in
// flight against one endpoint at once, so it is the point past which another
// idle connection buys nothing. It costs at most 32 sockets per host, and the
// transport's own 90-second IdleConnTimeout still reaps them.
//
// It is NOT read from config. An operator has no way to know a better value
// than "as many as this node can have turns", and the two would then have to
// be kept in step by hand.
const MaxIdleConnsPerHost = 32

// Transport is the shared outbound transport.
//
// CLONED from http.DefaultTransport rather than built, so the proxy, TLS and
// HTTP/2 settings the process started with survive: a hand-built transport
// silently drops ProxyFromEnvironment, which is how a deployment behind a
// corporate proxy stops reaching anything with no error that says so.
//
// sync.OnceValue rather than a package var so the clone happens on first use
// rather than at init, and cannot be observed half-built.
var Transport = sync.OnceValue(func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Something replaced the default with a type this cannot clone —
		// a test harness, or an instrumented build. Using it as-is is the
		// honest answer: it is still the process's transport, and the
		// pool size is a tuning knob rather than a correctness property.
		return http.DefaultTransport
	}
	t := base.Clone()
	t.MaxIdleConnsPerHost = MaxIdleConnsPerHost
	return t
})

// Client builds a client on the shared transport with a whole-request
// timeout.
//
// The timeout is per CALLER because it is a property of what is being asked
// for, not of the network: a manifest push and a chat post do not deserve the
// same patience. Pass 0 for no client-level deadline, which is right only when
// the caller bounds each request some other way — a per-request context, or a
// stream whose own idle timeout is the real bound.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: Transport(), Timeout: timeout}
}

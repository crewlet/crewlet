package pulsar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// ErrAdmin means an admin call failed in a way the caller must NOT treat as
// done.
//
// Distinct from the tolerated already-exists / already-gone answers, which
// are successes. A caller that swallowed this would report the
// subscription-existence invariant established when it is not — and the
// symptom of that is a seat whose first publish vanishes, with no dead
// letter, no producer error and nothing to alert on.
var ErrAdmin = errors.New("pulsar: broker admin call failed")

// BrokerAdmin is subscription lifecycle, independent of any consumer.
//
// It is an interface here for the reason the contract package keeps
// EventQueue one: this is the seam a test doubles, and the queue must not be
// able to reach past it into an HTTP client. It is deliberately NOT the
// consumer-facing surface — nothing above internal/queue asks a broker to
// create a subscription; the queue does, on the engine's behalf.
type BrokerAdmin interface {
	// Subscriptions lists the subscription names on a subject. A topic
	// that does not exist has no subscriptions — that is an answer, not an
	// error.
	Subscriptions(ctx context.Context, subject string) ([]string, error)

	// EnsureSubscription creates group on subject AT THE EARLIEST MESSAGE,
	// reporting whether this call created it.
	EnsureSubscription(ctx context.Context, subject, group string) (bool, error)

	// DeleteSubscription deletes group from subject, reporting whether
	// this call deleted it. False means it was already gone, which is the
	// desired end state.
	DeleteSubscription(ctx context.Context, subject, group string) (bool, error)

	// PeekBacklog returns the raw payloads a subscription retains and has
	// not acked, in mailbox order, WITHOUT consuming any of them.
	PeekBacklog(ctx context.Context, subject, group string) ([][]byte, error)

	// Close releases the admin client's resources.
	Close()
}

// restAdmin implements BrokerAdmin over Pulsar's admin v2 REST API.
//
// WHY THE ADMIN API AT ALL — the engine never spoke to it in the earliest
// design, and this is the whole reason it does now:
//
// A seat's inbox subscription must exist whether or not any node owns the
// seat. That is what makes an unowned seat safe: a durable subscription
// retains its backlog with no consumer attached, so a publish landing during
// a lease gap, a claim ramp or a full fleet restart is held rather than
// discarded. The obvious way to establish it — subscribe, then immediately
// close — is unsafe and MEASURED to be so: a seat's subscription is Shared,
// so a second consumer joining one an owner is actively serving takes a share
// of that seat's live traffic into its own prefetch (12 of 20 messages,
// tests/test_queue/test_broker_behavior.py). A node doing that for every seat
// at boot manufactures exactly the double-consumer state seat ownership
// exists to prevent.
//
// Deletion has the mirror problem: Consumer.Unsubscribe() is the only
// client-side route and it needs a LOCAL consumer, which would make
// decommissioning a role depend on which node happened to be running it.
//
// Both calls are idempotent by TOLERANCE rather than by the broker: creating
// an existing subscription answers 409 and deleting an absent one answers
// 404, and both are the desired end state, so each returns a bool saying
// whether this call changed anything rather than raising.
type restAdmin struct {
	base   string
	nsPath string
	token  string
	client *http.Client
}

// newRESTAdmin builds the admin client for a config, deriving the endpoint
// when the operator did not name one.
func newRESTAdmin(cfg Config) (*restAdmin, error) {
	base := cfg.AdminURL
	if base == "" {
		derived, err := DeriveAdminURL(cfg.URL)
		if err != nil {
			return nil, err
		}
		base = derived
	}
	client := &http.Client{Timeout: adminTimeout}
	if cfg.TLSTrustCertsPath != "" {
		pool, err := certPool(cfg.TLSTrustCertsPath)
		if err != nil {
			return nil, err
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}}
	}
	return &restAdmin{
		base:   strings.TrimRight(base, "/"),
		nsPath: cfg.nsPath(),
		token:  cfg.Token,
		client: client,
	}, nil
}

func certPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading tls_trust_certs_path: %w", ErrConfig, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: %s contains no PEM certificates", ErrConfig, path)
	}
	return pool, nil
}

// topicPath is the admin URL of one subject's topic.
//
// The subject is path-escaped rather than interpolated: a subject is engine
// data, and a topic name that reached the URL unescaped could address a
// different resource entirely.
func (a *restAdmin) topicPath(subject string) string {
	return a.base + "/admin/v2/persistent/" + a.nsPath + "/" + url.PathEscape(subject)
}

func (a *restAdmin) subPath(subject, group string) string {
	return a.topicPath(subject) + "/subscription/" + url.PathEscape(group)
}

// do issues one admin request, reading the body so the connection is reusable
// and the caller can quote the broker's own explanation.
func (a *restAdmin) do(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: building %s %s: %w", ErrAdmin, method, endpoint, err)
	}
	if a.token != "" {
		// The same JWT the broker connection presents: the admin API is a
		// second door onto the same authorization, not an unguarded one.
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s %s: %w — the engine needs the broker's admin "+
			"endpoint to keep every seat's subscription alive; see admin_url",
			ErrAdmin, method, endpoint, err)
	}
	defer resp.Body.Close()
	// Bounded: an admin error page is for a human to read, and a broker
	// answering with something enormous must not be able to make an error
	// path allocate without limit.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, payload, nil
}

// explain quotes the broker's answer without letting an HTML error page take
// over a log line.
func explain(status int, body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return fmt.Sprintf("%d", status)
	}
	return fmt.Sprintf("%d: %s", status, text)
}

func (a *restAdmin) Subscriptions(ctx context.Context, subject string) ([]string, error) {
	if err := checkSubject(subject); err != nil {
		return nil, err
	}
	status, body, err := a.do(ctx, http.MethodGet, a.topicPath(subject)+"/subscriptions", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// No topic yet, therefore no subscriptions. A seat that has never
		// been published to is the normal state of a new company, not a
		// failure.
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: listing subscriptions for %q returned %s",
			ErrAdmin, subject, explain(status, body))
	}
	var names []string
	if err := json.Unmarshal(body, &names); err != nil {
		return nil, fmt.Errorf("%w: listing subscriptions for %q returned unreadable JSON: %w",
			ErrAdmin, subject, err)
	}
	return names, nil
}

func (a *restAdmin) EnsureSubscription(ctx context.Context, subject, group string) (bool, error) {
	if err := checkSubject(subject); err != nil {
		return false, err
	}
	if group == "" {
		return false, fmt.Errorf("%w: empty group", ErrSubject)
	}
	// "earliest", ALWAYS. A subscription created at the latest message
	// exists and still discards everything published before its first
	// consumer attached — which is the exact failure this call prevents,
	// and which was measured on the first real run of the Python harness
	// ("a NEW subscription starts at Latest, so a consumer attaching after
	// the publish sees nothing at all").
	status, body, err := a.do(ctx, http.MethodPut, a.subPath(subject, group), []byte(`"earliest"`))
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusConflict:
		return false, nil // already exists — the end state we wanted
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	default:
		return false, fmt.Errorf("%w: creating subscription %q on %q returned %s",
			ErrAdmin, group, subject, explain(status, body))
	}
}

func (a *restAdmin) DeleteSubscription(ctx context.Context, subject, group string) (bool, error) {
	if err := checkSubject(subject); err != nil {
		return false, err
	}
	if group == "" {
		return false, fmt.Errorf("%w: empty group", ErrSubject)
	}
	status, body, err := a.do(ctx, http.MethodDelete, a.subPath(subject, group), nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		return false, nil // already gone — the end state we wanted
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	default:
		// 412 is the one an operator will actually meet: the broker
		// refuses to delete a subscription that still has a connected
		// consumer. Deliberately NOT forced — disconnecting a live
		// consumer to delete the mailbox it is serving would strand a
		// running seat mid-turn. The caller releases the seat and retries.
		return false, fmt.Errorf("%w: deleting subscription %q on %q returned %s",
			ErrAdmin, group, subject, explain(status, body))
	}
}

// subscriptionStats is the sliver of Pulsar's topic stats this backend reads.
type subscriptionStats struct {
	Subscriptions map[string]struct {
		// MsgBacklog is everything from the mark-delete position on:
		// retained AND delivered-but-unacked.
		MsgBacklog int64 `json:"msgBacklog"`
		// UnackedMessages is the delivered-but-unacked half of it — the
		// messages a consumer is currently holding.
		UnackedMessages int64 `json:"unackedMessages"`
	} `json:"subscriptions"`
}

// backlogDepth reports how many messages a subscription is holding OUT to
// consumers and how many it retains behind them, and whether the subscription
// exists at all.
//
// The split matters. "Backlog" in this codebase means the mail an unowned
// seat is holding — retained and NOT delivered — because that is what a
// successor will receive. A message a consumer already has is not that: it is
// somebody's work in progress, and it becomes backlog only when that consumer
// hands it back (which on Pulsar means closing). Counting the two together
// makes "the mail is waiting" indistinguishable from "the mail is being
// worked", which reads as the mailbox filling up at exactly the moment it is
// being emptied.
func (a *restAdmin) backlogDepth(ctx context.Context, subject, group string) (delivered, retained int, exists bool, err error) {
	status, body, err := a.do(ctx, http.MethodGet, a.topicPath(subject)+"/stats", nil)
	if err != nil {
		return 0, 0, false, err
	}
	if status == http.StatusNotFound {
		return 0, 0, false, nil
	}
	if status != http.StatusOK {
		return 0, 0, false, fmt.Errorf("%w: stats for %q returned %s", ErrAdmin, subject, explain(status, body))
	}
	var stats subscriptionStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, 0, false, fmt.Errorf("%w: stats for %q returned unreadable JSON: %w", ErrAdmin, subject, err)
	}
	sub, ok := stats.Subscriptions[group]
	if !ok {
		return 0, 0, false, nil
	}
	delivered = min(max(int(sub.UnackedMessages), 0), int(sub.MsgBacklog))
	return delivered, int(sub.MsgBacklog) - delivered, true, nil
}

// PeekBacklog reads a subscription's retained, UNDELIVERED mail without
// consuming it.
//
// Pulsar's peek endpoint reads the Nth entry after the subscription's
// mark-delete position, so this walks the positions and stops at the first
// one the broker will not serve. Reading a mailbox must never change it, and
// peek is the only route that does not: a throwaway consumer would join the
// Shared subscription it is inspecting and take a share of the seat's live
// traffic, which is the same hazard EnsureSubscription exists to avoid.
//
// The first `delivered` positions are skipped because a Shared subscription
// dispatches in order, so the messages a consumer is holding are the oldest
// ones behind the mark-delete point. They are not backlog — see
// backlogDepth.
func (a *restAdmin) PeekBacklog(ctx context.Context, subject, group string) ([][]byte, error) {
	if err := checkSubject(subject); err != nil {
		return nil, err
	}
	delivered, retained, exists, err := a.backlogDepth(ctx, subject, group)
	if err != nil || !exists || retained <= 0 {
		return nil, err
	}
	out := make([][]byte, 0, retained)
	for position := delivered + 1; position <= delivered+retained; position++ {
		endpoint := fmt.Sprintf("%s/position/%d", a.subPath(subject, group), position)
		status, body, err := a.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return out, err
		}
		if status != http.StatusOK {
			// The depth from stats is the broker's own estimate; a
			// position it declines is the end of what it will show,
			// not a failure worth losing the rows already read over.
			break
		}
		out = append(out, body)
	}
	return out, nil
}

func (a *restAdmin) Close() { a.client.CloseIdleConnections() }

var _ BrokerAdmin = (*restAdmin)(nil)

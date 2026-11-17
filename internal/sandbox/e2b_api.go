package sandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The E2B control plane: minting, reclaiming and keeping a box alive.
//
// A thin typed client: E2B publishes no Go SDK, and the control plane is a
// handful of documented JSON endpoints, so the backend stays pure Go.
//
// It is the only half that carries the API key. The in-box plane (commands,
// files) is envd on a per-box hostname (e2b_envd.go), and nothing here talks
// to it: a box whose envd is unreachable is still killable, so a network blip
// cannot leak a running VM.

// The E2B addresses. The domain derives both the API and the per-box
// hostnames, so a self-hosted cluster is one field, not two that can disagree.
const (
	// DefaultE2BDomain is the vendor cloud.
	DefaultE2BDomain = "e2b.dev"

	// e2bEnvdPort is envd's port in every box, carried in the hostname
	// (E2B routes `<port>-<host>` to it), not the path.
	e2bEnvdPort = 49983
)

// E2BClientTimeout bounds one control-plane request. Generous: minting a VM
// from a cold template takes seconds, and a tighter bound would abandon boxes
// about to exist, the one failure with a running cost.
const E2BClientTimeout = 60 * time.Second

// e2bAPI is the control plane for one deployment.
type e2bAPI struct {
	base   string
	domain string
	key    string
	http   *http.Client
}

// newE2BAPI builds the control-plane client.
func newE2BAPI(apiKey, domain string, client *http.Client) *e2bAPI {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = DefaultE2BDomain
	}
	// A domain written as a URL is the obvious mistake, and it produces
	// "https://https://…" with no message that names the cause.
	domain = strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimPrefix(domain, "https://"), "http://"), "/")
	if client == nil {
		client = httpx.Client(E2BClientTimeout)
	}
	return &e2bAPI{
		base:   "https://api." + domain,
		domain: domain,
		key:    strings.TrimSpace(apiKey),
		http:   client,
	}
}

// E2BError is a refusal from the control plane, typed so a caller reads its
// status (a 404 to a kill is success) rather than matching vendor text.
type E2BError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *E2BError) Error() string {
	msg := fmt.Sprintf("e2b: %s %s: %d", e.Method, e.Path, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Gone reports a box the control plane no longer has.
func (e *E2BError) Gone() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

// why classes the refusal by status ([statusWhy]): the key refused, the
// account at a limit, or E2B failing. A status whose meaning depends on the
// call (a 404 to a connect or to a create) is that call's to class.
func (e *E2BError) why() (Why, bool) { return statusWhy(e.Status, true) }

func (a *e2bAPI) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("e2b: encode %s: %w", path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return fmt.Errorf("e2b: %s: %w", path, err)
	}
	// X-API-KEY, not a bearer: E2B takes a bearer only for a user-scoped
	// access token and refuses the dashboard's key under it.
	req.Header.Set("X-API-KEY", a.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		// No answer is E2B unavailable unless the domain failed
		// ([transportWhy]); a caller's cancellation is no class ([unanswered]).
		failed := fmt.Errorf("e2b: %s: %w", path, err)
		if cancelledBy(ctx) {
			return failed
		}
		return Because(transportWhy(err), failed)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail := httpx.ReadRefusal(resp)
		return &E2BError{
			Method: method, Path: path, Status: resp.StatusCode,
			Detail: strings.TrimSpace(detail),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("e2b: decode %s: %w", path, err)
	}
	return nil
}

// transportWhy is what a control-plane request that got no answer means. A
// domain that does not resolve, or a certificate not valid for it, is the
// providers.sandbox.e2b domain's fault, which no wait changes: read as an
// outage, a mistyped domain would be reported on every call as one that
// clears by itself. Anything else (a timeout, a reset, a refused connection,
// a silent resolver) is E2B, or the route to it, not answering.
func transportWhy(err error) Why {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return WhyConfiguration
	}
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verify) || errors.As(err, &authority) ||
		errors.As(err, &hostname) || errors.As(err, &invalid) {
		return WhyConfiguration
	}
	return WhyProviderUnavailable
}

// e2bBox is a sandbox as the control plane reports it. ClientID is
// load-bearing: the envd hostname is `<port>-<sandbox>-<client>`, so a
// reconnect that kept only the sandbox id could kill the box but never reach it.
type e2bBox struct {
	SandboxID  string `json:"sandboxID"`
	ClientID   string `json:"clientID"`
	TemplateID string `json:"templateID"`
	// EnvdVersion, which E2B bumps apart from the template, is read so a wire
	// mismatch shows in a log line, not as an unexplained 404 from the box.
	EnvdVersion string `json:"envdVersion"`

	// State is `running` or `paused`, answered only by [getBox]'s read; a
	// create or a connect answers a running box without it.
	State string `json:"state,omitempty"`

	// EnvdAccessToken is the credential a secured box's envd requires on
	// every request (`X-Access-Token`), and every box this backend creates is
	// secured ([e2bAPI.createBox]). E2B answers it on a create, a connect and
	// a read alike, so every handle carries it. Never logged or stored: every
	// call that reaches the box hands it back.
	//
	// Source: the Sandbox and SandboxDetail schemas of
	// https://github.com/e2b-dev/infra/blob/main/spec/openapi.yml.
	EnvdAccessToken string `json:"envdAccessToken,omitempty"`
}

// e2bPaused is the [e2bBox.State] of a snapshot.
const e2bPaused = "paused"

// host is the per-box envd hostname. A box that reports no client id has the
// short form; the long form would be `49983-abc-.e2b.dev`, which resolves to
// nothing.
func (b e2bBox) host(domain string) string {
	name := b.SandboxID
	if b.ClientID != "" {
		name += "-" + b.ClientID
	}
	return fmt.Sprintf("https://%d-%s.%s", e2bEnvdPort, name, domain)
}

// createBox mints one sandbox. The TTL is its kill timer, refreshed by
// [E2BSandbox.SetTimeout] every poll tick: it bounds how long a box outlives
// a stopped engine, never how long a job runs.
//
// Always `secure: true`, else envd requires no credential and E2B answers no
// token. Secured, envd requires the create's token ([e2bBox.EnvdAccessToken])
// on every request but its health check, as E2B's SDKs do since 2.0; E2B
// refuses a template whose envd is below 0.2.0, saying to rebuild it.
//
// Sources: https://e2b.dev/docs/sandbox/secured-access; in e2b-dev/infra,
// spec/openapi.yml (NewSandbox), packages/api/internal/handlers/sandbox_create.go
// and envd's WithAuthorization in packages/envd/internal/api/auth.go.
func (a *e2bAPI) createBox(ctx context.Context, template string, timeoutSec float64, env map[string]string) (e2bBox, error) {
	type request struct {
		TemplateID string            `json:"templateID"`
		Timeout    int               `json:"timeout,omitempty"`
		Secure     bool              `json:"secure"`
		EnvVars    map[string]string `json:"envVars,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	var out e2bBox
	err := a.do(ctx, http.MethodPost, "/sandboxes", request{
		TemplateID: template,
		Timeout:    int(timeoutSec),
		Secure:     true,
		EnvVars:    env,
		// Stamped so a leaked VM in the E2B dashboard is attributable: a
		// box outlives the process that made it by design.
		Metadata: map[string]string{"crewlet": "sandbox"},
	}, &out)
	if err != nil {
		var refused *E2BError
		if errors.As(err, &refused) && createRefusesTemplate(refused.Status) {
			// Everything a create carries is configuration, so a 400, 422
			// or 404 is a person's to change: likeliest the template
			// (providers.sandbox.e2b.template, or the harness's), missing or
			// with envd too old for secured access; the timeout and the
			// seat's run environment ride the same request.
			return e2bBox{}, Because(WhyConfiguration, err)
		}
		return e2bBox{}, err
	}
	if out.SandboxID == "" {
		// A 200 with no id leaves a VM running that nothing can address
		// or kill.
		return e2bBox{}, Because(WhyProviderUnavailable, fmt.Errorf(
			"e2b: the create call succeeded and named no sandbox, so a box "+
				"may be running that nothing can reach or reclaim — check the "+
				"E2B dashboard for an orphan"))
	}
	return out, nil
}

// createRefusesTemplate reports a status a create answers only about the
// template it names: a request E2B cannot read (400), a template it does not
// have (404), or one it will not boot (422).
func createRefusesTemplate(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusUnprocessableEntity
}

// connectBox reads one sandbox for a reconnect, resuming it if paused, in one
// call: /connect answers 200 for a running box and 201 for a woken one, with
// the sandbox either way. A GET then the deprecated /resume would take two
// round trips, race a reaper between them, and refuse "already running" (409).
//
// The fresh TTL is required: a snapshot runs no kill timer, so a box woken
// without one keeps what remained when it paused, for a run parked overnight
// nothing.
func (a *e2bAPI) connectBox(ctx context.Context, sandboxID string, seconds float64) (e2bBox, error) {
	type request struct {
		Timeout int `json:"timeout"`
	}
	var out e2bBox
	err := a.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/connect",
		request{Timeout: int(seconds)}, &out)
	if err != nil {
		return e2bBox{}, err
	}
	if out.SandboxID == "" {
		out.SandboxID = sandboxID
	}
	return out, nil
}

// getBox reads one sandbox as it is, without resuming it or touching its
// timer: `GET /sandboxes/{sandboxID}`, whose SandboxDetail carries `state`
// beside the client id. /connect is a resume's read: it wakes a paused box
// and extends its timer.
//
// Source: the SandboxDetail and SandboxState schemas of
// https://github.com/e2b-dev/infra/blob/main/spec/openapi.yml.
func (a *e2bAPI) getBox(ctx context.Context, sandboxID string) (e2bBox, error) {
	var out e2bBox
	if err := a.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID, nil, &out); err != nil {
		return e2bBox{}, err
	}
	if out.SandboxID == "" {
		out.SandboxID = sandboxID
	}
	return out, nil
}

// listBoxes lists at most limit sandboxes (`GET /v2/sandboxes`, the spec
// [e2bAPI.getBox] cites) and discards them: the runner probe's one
// authenticated read ([ProbeE2B]), proving the key and the route without
// making or waking a box.
func (a *e2bAPI) listBoxes(ctx context.Context, limit int) error {
	var out []e2bBox
	return a.do(ctx, http.MethodGet, fmt.Sprintf("/v2/sandboxes?limit=%d", limit), nil, &out)
}

// killBox terminates a sandbox by id without resuming it: the reaper exists
// because a paused snapshot bills, and resuming one to kill it would boot the
// VM for a shutdown.
func (a *e2bAPI) killBox(ctx context.Context, sandboxID string) error {
	err := a.do(ctx, http.MethodDelete, "/sandboxes/"+sandboxID, nil, nil)
	var apiErr *E2BError
	if err != nil && errors.As(err, &apiErr) && apiErr.Gone() {
		// Already gone is the outcome this call wanted.
		return nil
	}
	return err
}

// setBoxTimeout resets the kill timer to seconds from now.
func (a *e2bAPI) setBoxTimeout(ctx context.Context, sandboxID string, seconds float64) error {
	type request struct {
		Timeout int `json:"timeout"`
	}
	return a.do(ctx, http.MethodPost,
		"/sandboxes/"+sandboxID+"/timeout", request{Timeout: int(seconds)}, nil)
}

// pauseBox snapshots a sandbox.
func (a *e2bAPI) pauseBox(ctx context.Context, sandboxID string) error {
	return a.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/pause", nil, nil)
}

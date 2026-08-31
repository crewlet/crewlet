package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The E2B control plane: minting, reclaiming and keeping a box alive.
//
// # A thin typed client rather than a vendor SDK
//
// E2B publishes no Go SDK. The control plane is a handful of documented JSON
// endpoints, so this is a typed client over them — which is what this engine
// does wherever a vendor ships no Go SDK, and is why the whole backend stays
// pure Go and cross-compiles with everything else.
//
// # Two planes, and they are different hosts
//
// The CONTROL plane (here) mints, kills, pauses and re-times boxes. It is the
// only half that carries the API key.
//
// The IN-BOX plane — running commands, reading and writing files — is envd,
// an agent inside the box, reached on a per-sandbox hostname. See e2b_envd.go.
// Nothing in this file talks to it, and the split matters: a box whose envd
// is unreachable is still killable, which is what stops a network blip from
// leaking a running VM.

// The E2B addresses. `domain` is the whole cloud-to-self-hosted switch: the
// API and the per-box hostnames are both derived from it, so a cluster is one
// field rather than two that can disagree.
const (
	// DefaultE2BDomain is the vendor cloud.
	DefaultE2BDomain = "e2b.dev"

	// e2bEnvdPort is the port envd listens on inside every box. It is part
	// of the hostname rather than the path: E2B routes `<port>-<host>` to
	// that port in the box.
	e2bEnvdPort = 49983
)

// E2BClientTimeout bounds one control-plane request.
//
// GENEROUS compared to the notification path's, because minting a VM is a
// real allocation: a cold template can take several seconds, and a timeout
// short enough to be tidy would abandon boxes that are about to exist —
// which is the one failure mode with a running cost attached.
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

// E2BError is a refusal from the control plane.
//
// TYPED, so a caller deciding what a refusal MEANS — 404 is "this box is
// gone", which for a kill is success — does not substring-match a message the
// vendor changes freely.
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
	// X-API-KEY, not a bearer. E2B accepts a bearer only for a
	// user-scoped access token, and the key an operator gets from the
	// dashboard is refused under that scheme — the same credential
	// rejected purely on which header carried it.
	req.Header.Set("X-API-KEY", a.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail := readDetail(resp.Body)
		return &E2BError{
			Method: method, Path: path, Status: resp.StatusCode,
			Detail: strings.TrimSpace(detail),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("e2b: decode %s: %w", path, err)
	}
	return nil
}

// e2bBox is a sandbox as the control plane reports it.
//
// THE CLIENT ID IS LOAD-BEARING and is the field most easily mistaken for
// bookkeeping: a box's envd hostname is `<port>-<sandbox>-<client>`, so a
// reconnect that kept only the sandbox id can kill the box and can never
// talk to it again.
type e2bBox struct {
	SandboxID  string `json:"sandboxID"`
	ClientID   string `json:"clientID"`
	TemplateID string `json:"templateID"`
	// EnvdVersion is envd's own version, which E2B bumps independently of
	// the template. Read so a wire mismatch is visible in a log line
	// rather than as an unexplained 404 from inside a box.
	EnvdVersion string `json:"envdVersion"`
}

// host is the per-box envd hostname.
//
// A box created before E2B split the client id out reports an empty one, and
// its hostname is the short form. Building the long form unconditionally
// would produce `49983-abc-.e2b.dev`, which resolves to nothing.
func (b e2bBox) host(domain string) string {
	name := b.SandboxID
	if b.ClientID != "" {
		name += "-" + b.ClientID
	}
	return fmt.Sprintf("https://%d-%s.%s", e2bEnvdPort, name, domain)
}

// createBox mints one sandbox.
//
// The TTL is the box's own kill timer, refreshed by [E2BSandbox.SetTimeout]
// on every poll tick — so it bounds how long a box outlives an engine that
// stopped heart-beating, never how long a job may run.
func (a *e2bAPI) createBox(ctx context.Context, template string, timeoutSec float64, env map[string]string) (e2bBox, error) {
	type request struct {
		TemplateID string            `json:"templateID"`
		Timeout    int               `json:"timeout,omitempty"`
		EnvVars    map[string]string `json:"envVars,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	var out e2bBox
	err := a.do(ctx, http.MethodPost, "/sandboxes", request{
		TemplateID: template,
		Timeout:    int(timeoutSec),
		EnvVars:    env,
		// STAMPED SO A LEAK IS ATTRIBUTABLE. A box outlives the process
		// that made it by design, so an operator looking at a running VM
		// in the E2B dashboard has nothing else to go on.
		Metadata: map[string]string{"crewlet": "sandbox"},
	}, &out)
	if err != nil {
		return e2bBox{}, err
	}
	if out.SandboxID == "" {
		// A create that answered 200 with no id has left a VM running
		// that nothing can address — the worst outcome available, since
		// it cannot be killed either.
		return e2bBox{}, fmt.Errorf(
			"e2b: the create call succeeded and named no sandbox, so a box " +
				"may be running that nothing can reach or reclaim — check the " +
				"E2B dashboard for an orphan")
	}
	return out, nil
}

// connectBox reads one sandbox for a reconnect, resuming it if it was paused.
//
// ONE CALL FOR BOTH HALVES. E2B's /connect answers 200 when the box was
// already running and 201 when it had to be woken, and returns the sandbox
// either way — so the caller needs neither to know which happened nor to ask
// first. The pair this replaced (GET the box, then POST /resume) was two round
// trips on the completion path, raced a reaper between them, and had to map
// the resume's 409 "already running" onto success by hand.
//
// /resume is DEPRECATED in E2B's own spec, which is the other half of the
// reason: it is the endpoint that cannot express "already running" as anything
// but an error.
//
// A FRESH TTL IS NOT OPTIONAL HERE — the field is required by the API. A
// snapshot has no kill timer running, so a box woken without one would carry
// whatever remained of the timer it was paused with, which for a run parked
// overnight is nothing.
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

// killBox terminates a sandbox by id, without resuming it.
//
// A PAUSED BOX IS KILLED WHERE IT LIES. The reaper's whole reason to exist is
// that a paused snapshot bills; resuming it to kill it would boot the VM and
// bill runtime to pay for a shutdown.
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

// detailLimit bounds a vendor's own explanation of a refusal.
//
// The whole account of what went wrong — a quota message, a permission
// name, a validation list — and it reaches an operator and a model as the
// error's text. Two kilobytes holds any of those; past that it is a vendor
// serving an HTML page where an API response belongs.
const detailLimit = 2048

// readDetail reads a refusal's body, SAYING when it cut.
//
// An unmarked cut leaves "the explanation is off-screen" and "the vendor
// explained itself badly" as the same string, which is the distinction the
// reader most needs — and the read error is reported rather than dropped,
// because a body that died mid-read is a different fact from a short one.
func readDetail(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, detailLimit+1))
	text := strings.TrimSpace(string(raw))
	switch {
	case len(raw) > detailLimit:
		return strings.TrimSpace(string(raw[:detailLimit])) +
			"\n…(the rest of the response is past the 2048-byte cap this build reads)"
	case err != nil && text == "":
		return "(the response body could not be read: " + err.Error() + ")"
	default:
		return text
	}
}

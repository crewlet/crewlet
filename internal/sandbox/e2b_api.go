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
	"net/url"
	"strconv"
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

// The E2B addresses. The domain derives the API's and, unless E2B answers a
// domain of its own for a box, the per-box hostnames, so a self-hosted cluster
// is one field, not two that can disagree.
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

// e2bV2Floor is the oldest build a self-hosted cluster may run: the
// e2b-dev/runtime commit (formerly e2b-dev/infra) that adds the v2 create and
// connect this client calls ([e2bAPI.createBox], [e2bAPI.connectBox]), which
// E2B's own SDKs call too. It carries e2b-dev/infra#1474 and #1482, which
// mint the traffic token for a private box and exempt envd's port from it,
// so it is the only floor. Not a release: the newest, 2026.30, predates it.
// The cell probe asks for both routes ([e2bAPI.floorRouteMissing]).
const e2bV2Floor = "e2b-dev/runtime f5dc6426e"

// e2bLaunchKey is the metadata key a box carries its launch under
// ([Spec.LaunchID]), so the ending of what made it (a run, a check's claim)
// finds a box whose create's answer never reached it ([E2BProvider.KillLaunch]).
const e2bLaunchKey = "crewlet-launch"

// e2bListPage is the most boxes one list answers, the API's own maximum
// (paginationLimit in spec/openapi.yml). A launch makes one create, so at most
// one box carries its stamp: one page of the largest size cannot hide it.
const e2bListPage = 100

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

	// Domain is where E2B serves the box's ports: answered for a team on a
	// cluster of its own, empty otherwise. A cluster can move to a new domain
	// while its boxes run, so it is read from every answer that builds a handle.
	Domain string `json:"domain,omitempty"`

	// EnvdAccessToken is the credential a secured box's envd requires on
	// every request (`X-Access-Token`), and every box this backend creates is
	// secured ([e2bAPI.createBox]). E2B answers it on a create, a connect and
	// a read alike, so every handle carries it. Never logged or stored: every
	// call that reaches the box hands it back.
	//
	// Sources: E2B's "Secured access"
	// (https://docs.e2b.dev/sandbox/secured-access); the Sandbox and
	// SandboxDetail schemas of
	// https://github.com/e2b-dev/runtime/blob/main/spec/openapi.yml, and
	// envd's WithAuthorization in packages/envd/internal/api/auth.go.
	EnvdAccessToken string `json:"envdAccessToken,omitempty"`

	// TrafficToken is whether the answer carried the box's traffic access
	// token, which E2B mints only for a box made private ([e2bAPI.createBox]).
	// Never the token: nothing here sends it, so nothing holds it.
	TrafficToken tokenAnswered `json:"trafficAccessToken"`
}

// tokenAnswered is whether E2B answered a token, holding nothing of it.
type tokenAnswered bool

// UnmarshalJSON records whether a JSON string longer than its two quotes
// arrived; JSON null reaches it too. Any other value is no answer rather than
// an error, which would fail the decode and lose the id of a box just created.
func (t *tokenAnswered) UnmarshalJSON(raw []byte) error {
	*t = len(raw) > 2 && raw[0] == '"'
	return nil
}

// e2bNetwork is a create's network settings. AllowPublicTraffic is never
// omitempty: a false left out is the project's default, public on a project
// that predates E2B's private default. maskRequestHost is never set: it
// rewrites the Host header a box's service sees, and nothing the engine runs
// needs it. Sources: E2B's "Restricting public access"
// (https://docs.e2b.dev/network/restrict-public-access); in e2b-dev/runtime,
// spec/openapi.yml (SandboxNetworkConfig, Sandbox.trafficAccessToken),
// packages/api/internal/handlers/sandbox_create.go,
// packages/api/internal/orchestrator/create_instance.go, and the proxy's
// ResolveIngress in packages/shared/pkg/proxy/ingress.go.
type e2bNetwork struct {
	AllowPublicTraffic bool `json:"allowPublicTraffic"`
}

// e2bPaused is the [e2bBox.State] of a snapshot.
const e2bPaused = "paused"

// host is the per-box envd hostname, on the domain E2B answered for the box,
// else the deployment's. A box that reports no client id has the short form;
// the long form would be `49983-abc-.e2b.dev`, which resolves to nothing.
func (b e2bBox) host(domain string) string {
	if b.Domain != "" {
		domain = b.Domain
	}
	name := b.SandboxID
	if b.ClientID != "" {
		name += "-" + b.ClientID
	}
	return fmt.Sprintf("https://%d-%s.%s", e2bEnvdPort, name, domain)
}

// createBox mints one sandbox, private and secured, or none, through E2B's
// current create (`POST /v2/sandboxes`, NewSandboxV2). Public traffic is off,
// so E2B's proxy passes a request to envd's port alone without the box's traffic
// token; an answer carrying none came from an API that ignored the field, so its
// box is killed and refused ([e2bAPI.refusePublic]). E2B secures every box, its
// envd requiring the token every answer carries ([e2bBox.EnvdAccessToken]). The
// TTL is a kill timer every poll tick refreshes ([E2BSandbox.SetTimeout]), never
// a bound on a job. STAMPED WITH ITS LAUNCH, never sent an Idempotency-Key: a
// redelivered turn is a new run whose launch sends its own create, so none is
// sent twice for a key to answer once, and a box whose answer never reached
// what made it is killed by its stamp ([E2BProvider.KillLaunch]). Why: the
// package doc, "AND AN E2B BOX IS PRIVATE".
func (a *e2bAPI) createBox(ctx context.Context, template, launch string, timeoutSec float64, env map[string]string) (e2bBox, error) {
	type request struct {
		TemplateID string            `json:"templateID"`
		Timeout    int               `json:"timeout,omitempty"`
		Network    e2bNetwork        `json:"network"`
		EnvVars    map[string]string `json:"envVars,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	// Stamped so a leaked VM in the E2B dashboard is attributable: a box
	// outlives the process that made it by design.
	metadata := map[string]string{"crewlet": "sandbox"}
	if launch != "" {
		metadata[e2bLaunchKey] = launch
	}
	var out e2bBox
	err := a.do(ctx, http.MethodPost, "/v2/sandboxes", request{
		TemplateID: template,
		Timeout:    int(timeoutSec),
		Network:    e2bNetwork{AllowPublicTraffic: false},
		EnvVars:    env,
		Metadata:   metadata,
	}, &out)
	if err != nil {
		var refused *E2BError
		if errors.As(err, &refused) && createRefusesTemplate(refused.Status) {
			// Everything a create carries is configuration, so a 400, 422
			// or 404 is a person's to change: likeliest the template
			// (providers.sandbox.e2b.template, or the harness's), missing or
			// with envd too old for secured access; the timeout and the
			// seat's run environment ride the same request. And on a
			// cluster a 404 or 400 is as likely its API, older than the route.
			return e2bBox{}, Because(WhyConfiguration, a.olderThanTheRoute(err, refused.Status))
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
	if !out.TrafficToken {
		return e2bBox{}, a.refusePublic(ctx, out.SandboxID)
	}
	return out, nil
}

// refusePublic kills sandboxID, which E2B created without a traffic token and
// so public, and refuses its launch as configuration, on E2B's cloud as on a
// cluster: no wait the engine can see clears it (the package doc, "AND AN E2B
// BOX IS PRIVATE"). Only a cluster's remedy is a setting, so the words differ.
//
// The kill is the create's own rollback, so it runs under a
// [context.WithoutCancel] of its own, as a backend's rollback inside its call
// does ([detach]), bounded by the client's [E2BClientTimeout]: the box is
// killed though its caller gave up as the create answered. A failed kill is
// told inside the mark, so its own class (a 500's provider_unavailable) never
// replaces configuration ([WhyOf] reads the outermost).
func (a *e2bAPI) refusePublic(ctx context.Context, sandboxID string) error {
	public := fmt.Sprintf("e2b: the API at api.%s created sandbox %s without a traffic "+
		"access token, so every port of it but envd's is public", a.domain, sandboxID)
	remedy := "The launch is refused. Run " + e2bV2Floor + " or later on that cluster, " +
		"or leave providers.sandbox.e2b.domain empty for E2B's cloud"
	if a.domain == DefaultE2BDomain {
		remedy = "The launch is refused. On E2B's cloud no setting under providers.sandbox.e2b " +
			"mends this: report it to E2B, and until it is mended run in another cell (run_in)"
	}
	if err := a.killBox(context.WithoutCancel(ctx), sandboxID); err != nil {
		return Because(WhyConfiguration, fmt.Errorf("%s, and killing it failed (%w), so it "+
			"may still be running: find it in the E2B dashboard by its metadata "+
			"crewlet: sandbox. %s", public, err, remedy))
	}
	return Because(WhyConfiguration, fmt.Errorf("%s: it was killed. %s", public, remedy))
}

// olderThanTheRoute is err, a create's refusal with status, naming the
// cluster's floor where it is a 404 or 400 from a cluster: an API from before
// [e2bV2Floor] has no such route, and answers it as a missing template does
// (404) or, before 2026.04, as an unreadable request (400: the path is served
// for a list alone). The cell probe tells which ([e2bAPI.floorRouteMissing]).
func (a *e2bAPI) olderThanTheRoute(err error, status int) error {
	if a.domain == DefaultE2BDomain {
		return err
	}
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w (the template providers.sandbox.e2b.template or the harness's names is "+
			"missing, or the cluster at api.%s is older than %s, which serves the create this "+
			"engine calls)", err, a.domain, e2bV2Floor)
	case http.StatusBadRequest:
		return fmt.Errorf("%w (the request is one E2B cannot read, or the cluster at api.%s is older "+
			"than %s, which serves the create this engine calls)", err, a.domain, e2bV2Floor)
	}
	return err
}

// createRefusesTemplate reports a status a create answers only about the
// template it names: a request E2B cannot read (400), a template it does not
// have (404), or one it will not boot (422).
func createRefusesTemplate(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusUnprocessableEntity
}

// connectBox reads one sandbox for a reconnect, resuming it if paused, in one
// call (`POST /v2/sandboxes/{sandboxID}/connect`, ConnectSandboxV2): it
// answers 200 for a running box and 201 for a woken one, with the sandbox
// either way. A GET then the deprecated /resume would take two round trips,
// race a reaper between them, and refuse "already running" (409).
//
// The fresh TTL is required: a snapshot runs no kill timer, so a box woken
// without one keeps what remained when it paused, for a run parked overnight
// nothing.
func (a *e2bAPI) connectBox(ctx context.Context, sandboxID string, seconds float64) (e2bBox, error) {
	type request struct {
		Timeout int `json:"timeout"`
	}
	var out e2bBox
	err := a.do(ctx, http.MethodPost, "/v2/sandboxes/"+sandboxID+"/connect",
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
// https://github.com/e2b-dev/runtime/blob/main/spec/openapi.yml.
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
// authenticated read ([ProbeE2B]), proving the key without making or waking a
// box.
func (a *e2bAPI) listBoxes(ctx context.Context, limit int) error {
	var out []e2bBox
	return a.do(ctx, http.MethodGet, fmt.Sprintf("/v2/sandboxes?limit=%d", limit), nil, &out)
}

// launchBoxes is the ids of the boxes, running or paused, stamped with launch
// ([e2bLaunchKey]): `GET /v2/sandboxes` filtered by metadata, one `key=value`
// whose key and value are each URL-encoded (ParseMetadata in E2B's API).
func (a *e2bAPI) launchBoxes(ctx context.Context, launch string) ([]string, error) {
	query := url.Values{
		"metadata": {url.QueryEscape(e2bLaunchKey) + "=" + url.QueryEscape(launch)},
		"state":    {"running,paused"},
		"limit":    {strconv.Itoa(e2bListPage)},
	}
	var out []e2bBox
	if err := a.do(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, box := range out {
		if box.SandboxID != "" {
			ids = append(ids, box.SandboxID)
		}
	}
	return ids, nil
}

// e2bFloorRoutes are the routes [e2bV2Floor] adds, each with a body whose field
// has the wrong JSON type, which every build refuses before acting (its OpenAPI
// validator answers 400, as its handler's decode would), so a probe of them
// makes or wakes nothing ([e2bAPI.floorRouteMissing]).
var e2bFloorRoutes = []struct {
	path string
	body json.RawMessage
}{
	{"/v2/sandboxes", json.RawMessage(`{"templateID":0}`)},
	{"/v2/sandboxes/crewlet-probe/connect", json.RawMessage(`{"timeout":"0"}`)},
}

// floorRouteMissing is the first of [e2bFloorRoutes] the API does not serve,
// as "POST <path>", or "" where it serves both. A build without the route
// answers 404 to the connect's path, absent from its spec, and from 2026.04 on
// to the create's too; before 2026.04 it answers the create's 400, the path
// being served for a list, so the connect's is what tells such a build apart.
func (a *e2bAPI) floorRouteMissing(ctx context.Context) (string, error) {
	for _, route := range e2bFloorRoutes {
		err := a.do(ctx, http.MethodPost, route.path, route.body, nil)
		var refused *E2BError
		switch {
		case errors.As(err, &refused) && refused.Status == http.StatusNotFound:
			return http.MethodPost + " " + route.path, nil
		case err == nil, errors.As(err, &refused) && (refused.Status == http.StatusBadRequest ||
			refused.Status == http.StatusUnprocessableEntity):
			// Served: the body refused, as it is meant to be.
		default:
			return "", err
		}
	}
	return "", nil
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

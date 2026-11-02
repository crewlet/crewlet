package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/redact"
)

// TESTING A SERVER, AND CHECKING AN ADDRESS BEFORE ONE IS ADDED.
//
// Three routes, each [authz.ActionToolsCheck], and each audited as one
// `mcp_server_acted`:
//
//   - `POST /mcp-servers/{name}/check` tests the entry the company runs, on
//     every node, or for a per-agent server on the node holding the seat
//     the body names. The test is recorded as the server's latest, which its
//     detail answers to every administrator.
//   - `POST /mcp-checks` tests a DRAFT, an entry not written yet, in the
//     company it would join. It runs what a write would (a command on the
//     engine's host, an address dialled with the values the entry names
//     resolved from the secret store), so it is decided as that write too,
//     `config.write` and `secrets.write`, before anything is read past the
//     body ([mayRunDraft]); the draft is held to the configuration's own dry
//     run, and a refused one is answered as its write would be. Never
//     recorded.
//   - `POST /mcp-probe` checks an address before a server is added at it:
//     what the server is, and how it is signed in to. The address is the
//     caller's, dialled as the server would be once added, so it is decided
//     as the write that would make the engine dial it, `config.write` on
//     top; it resolves no secret and sends no credential.
//
// OUTSIDE /mcp-servers/{name} for the two that name no saved server, for the
// catalogue's reason: a literal segment there would win over a server it
// spells.
//
// A TEST NEVER CALLS A TOOL (internal/mcp's test.go), and its answer carries
// no header or variable value and no address with its credentials in it.

// Checker tests servers and addresses, adapted from the engine where the API
// is wired, with this package's own refusals ([ErrNoSuchServer] and its
// neighbours, and [ErrSeatRequired]).
type Checker interface {
	CheckServer(ctx context.Context, req CheckRequest) (Check, error)
	CheckAddress(ctx context.Context, address string) mcp.AddressCheck
}

// DraftChecker holds a draft entry to the configuration's own dry run: the
// company it would join, or the refusal a write of it would get
// ([configapi.Service.CheckEntity]).
type DraftChecker interface {
	CheckEntity(ctx context.Context, kind, id string, body []byte, create bool, by iam.Actor) (*config.Company, error)
}

// ErrSeatRequired is a per-agent server tested without naming the seat to
// test it for.
var ErrSeatRequired = errors.New("mcpapi: a per-agent MCP server is tested for one seat")

// CheckRequest is a test somebody asked for.
type CheckRequest struct {
	Server string
	// Draft is the entry as a write would leave it, nil for the saved one.
	Draft *config.MCPServer
	Seat  string
	// Value is a value tested before it is stored, nil for none.
	Value *ValueTry
	By    iam.Actor
}

// Check is what a test found, every sentence redacted and bounded.
type Check struct {
	Server string
	Seat   string
	Draft  bool
	At     time.Time
	By     string
	Node   string
	OK     bool
	Nodes  []CheckedNode
}

// CheckedNode is one node's test.
type CheckedNode struct {
	Node     string
	Answered bool
	Reason   string
	Report   mcp.TestReport
}

// checkRequest is the body `POST /mcp-servers/{name}/check` reads: empty for
// a shared server, or the seat a per-agent server is tested for.
type checkRequest struct {
	Seat string `json:"seat"`
}

// draftRequest is the body `POST /mcp-checks` reads: the draft entry, as
// `PUT /config/mcp-servers/{name}` would take it, and the seat a per-agent
// one is tested for.
type draftRequest struct {
	Entity json.RawMessage `json:"entity"`
	Seat   string          `json:"seat"`
}

// probeRequest is the body `POST /mcp-probe` reads.
type probeRequest struct {
	URL string `json:"url"`
}

// maxCheckBody bounds a test's body: one entry of the configuration, whose
// launch, headers and annotations are a few kilobytes.
const maxCheckBody = 64 << 10

// CheckAnswer is what `POST /mcp-servers/{name}/check` and `POST /mcp-checks`
// answer: what each node found, and from the first node that found the
// server serving, what it said of itself and the tools it would offer.
type CheckAnswer struct {
	Server string `json:"server"`
	// Seat is the handle the test was made for, "" for a shared server.
	Seat string `json:"seat"`
	// Draft is whether the entry tested is not written yet.
	Draft bool      `json:"draft"`
	At    time.Time `json:"at"`
	By    string    `json:"by"`
	// Node is the node the test was asked through.
	Node string `json:"node"`
	// OK is whether some node answered and every node that answered found
	// the server serving.
	OK bool `json:"ok"`
	// Nodes are the nodes asked, each with what it found. ALWAYS A LIST.
	Nodes []queries.MCPCheckNodeView `json:"nodes"`
	// ServerInfo is what the server said of itself, null where no node
	// found it serving.
	ServerInfo *coord.MCPServerInfo `json:"server_info"`
	// Tools are the tools it would offer, as the first node that found it
	// serving listed them. ALWAYS A LIST.
	Tools []CheckedTool `json:"tools"`
	// Warnings say what a reader of the tools should act on: each tool not
	// offered under its name, and why. ALWAYS A LIST.
	Warnings []string `json:"warnings"`
}

// CheckedTool is one tool a test listed, as the engine would offer it.
type CheckedTool struct {
	// Name is the name it would be offered under, Raw the server's.
	Name string `json:"name"`
	Raw  string `json:"raw"`
	// Title and Description are the server's words, bounded.
	Title       string `json:"title" untrusted:"name"`
	Description string `json:"description" untrusted:"fence"`
	// Annotations are its hints with the company's overrides applied.
	Annotations queries.ToolHints `json:"annotations"`
	// Risk, RiskDeclared and External are the engine's one reading of the
	// hints ([mcp.RiskOf]).
	Risk         mcp.Risk `json:"risk"`
	RiskDeclared bool     `json:"risk_declared"`
	External     bool     `json:"external"`
	// StartingLevel is the level the tool starts at when the server is
	// added ([mcp.StartingLevel]).
	StartingLevel mcp.StartLevel `json:"starting_level"`
	// Refused is why it would not be offered under its name, "" when it
	// would.
	Refused mcp.NameRefusal `json:"refused"`
}

// ProbeAnswer is what `POST /mcp-probe` answers: the address's walk with no
// credential sent, the sign-in the server asks for, and the curated entry the
// address is, if one.
type ProbeAnswer struct {
	// Address is the address checked, its credentials withheld.
	Address string `json:"address"`
	// Reachable is whether the server answered: its handshake, or a refusal
	// that asks for a credential.
	Reachable bool `json:"reachable"`
	// Check is the walk, as a node's test is answered.
	Check queries.MCPCheckNodeView `json:"check"`
	// Auth is the sign-in it asks for: `none`, `headers` or `oauth`, ""
	// where the check did not reach an answer to tell by.
	Auth catalogue.AuthMethod `json:"auth"`
	// OAuth is what its sign-in metadata says, null unless it challenged
	// for a Bearer token.
	OAuth *ProbeOAuth `json:"oauth"`
	// Tools counts the tools it lists to no credential, null where it asks
	// for a sign-in first or did not answer.
	Tools *int `json:"tools"`
	// Catalogue, Title and Vendor are the curated entry the address is, ""
	// where the curated catalogue does not know it.
	Catalogue string `json:"catalogue"`
	Title     string `json:"title"`
	Vendor    string `json:"vendor"`
}

// ProbeOAuth is what a server's sign-in metadata says: addresses its
// publisher wrote, each with its credentials withheld.
type ProbeOAuth struct {
	ResourceMetadata     string                `json:"resource_metadata"`
	Resource             string                `json:"resource" untrusted:"name"`
	AuthorizationServers []string              `json:"authorization_servers"`
	Issuer               string                `json:"issuer"`
	Scopes               []string              `json:"scopes"`
	Client               catalogue.OAuthClient `json:"client"`
	PKCE                 bool                  `json:"pkce"`
	// Failure is why the discovery stopped short, null where it read both
	// documents. A `blocked_address` is metadata naming an address the
	// engine does not follow.
	Failure *queries.MCPCheckFailure `json:"failure"`
}

// serveCheck tests the saved server its path names: 200 with what each node
// found, recorded as the server's latest.
func (s *Service) serveCheck(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	var req checkRequest
	if !decodeBody(w, r, &req, `{"seat": "<handle>"}, or empty for a shared server`) {
		return
	}
	rec.Seat = req.Seat
	s.check(w, r, CheckRequest{Server: r.PathValue("name"), Seat: req.Seat,
		By: auth.AttributionOf(r.Context())})
}

// serveDraftCheck tests a draft entry: decided as the write it stands for,
// before its body is read, then held to the configuration's dry run.
func (s *Service) serveDraftCheck(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	rec.Draft = true
	// A DRAFT IS DECIDED AS THE WRITE IT STANDS FOR, before anything of it
	// is read: a person who may not write the company and its secrets is
	// told so, rather than told what is wrong with an entry they could not
	// save.
	if !mayRunDraft(w, r, s.guard) {
		return
	}
	if s.drafts == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no configuration surface to hold a draft to; " +
				"ask through a node that serves the dashboard"})
		return
	}
	var req draftRequest
	if !decodeBody(w, r, &req, `{"entity": {...}, "seat": "<handle>"}`) {
		return
	}
	var named struct {
		Name string `json:"name"`
	}
	if len(req.Entity) == 0 || json.Unmarshal(req.Entity, &named) != nil || strings.TrimSpace(named.Name) == "" {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": `the body is {"entity": {...}}, an MCP server entry with its name`})
		return
	}
	rec.Server, rec.Seat = named.Name, req.Seat
	create := true
	if company := s.company(); company != nil {
		for i := range company.MCPServers {
			if company.MCPServers[i].Name == named.Name {
				create = false
			}
		}
	}
	by := auth.AttributionOf(r.Context())
	next, err := s.drafts.CheckEntity(r.Context(), configapi.EntityMCPServers, named.Name, req.Entity, create, by)
	if !configapi.RefuseDraft(w, err) {
		return
	}
	var draft *config.MCPServer
	for i := range next.MCPServers {
		if next.MCPServers[i].Name == named.Name {
			draft = &next.MCPServers[i]
		}
	}
	if draft == nil {
		log.ErrorContext(r.Context(), "mcp_draft_not_in_company", "server", named.Name)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	s.check(w, r, CheckRequest{Server: named.Name, Draft: draft, Seat: req.Seat, By: by})
}

// check runs one test and answers it, or the refusal it met.
func (s *Service) check(w http.ResponseWriter, r *http.Request, req CheckRequest) {
	done, err := s.checker.CheckServer(r.Context(), req)
	switch {
	case err == nil:
		httpjson.Write(w, http.StatusOK, CheckAnswerOf(done))
	case errors.Is(err, ErrSeatRequired):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "MCP server " + quoted(req.Server) + " runs one instance per agent, " +
				"each with that agent's credentials: name the seat to test it for, {\"seat\": \"<handle>\"}"})
	case errors.Is(err, ErrServerOff):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "MCP server " + quoted(req.Server) + " is turned off, so nothing runs " +
				"it to test: test its entry as a draft (POST /mcp-checks) to try it before turning it on"})
	default:
		s.refuseServerGesture(w, r, req.Server, req.Seat, err, "mcp_server_check_failed",
			"this node could not ask the fleet to test the server; ask again")
	}
}

// serveProbe checks an address before a server is added at it: decided as
// the write that would make the engine dial it.
func (s *Service) serveProbe(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
		return
	}
	var req probeRequest
	if !decodeBody(w, r, &req, `{"url": "https://..."}`) {
		return
	}
	address := strings.TrimSpace(req.URL)
	if address == "" {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": `the body is {"url": "https://..."}, the address to check`})
		return
	}
	rec.Address = redact.URL(address)
	httpjson.Write(w, http.StatusOK, ProbeAnswerOf(address, s.checker.CheckAddress(r.Context(), address)))
}

// mayRunDraft decides a request that runs a DRAFT entry as the writes it
// stands for, answering the refusal itself: a draft's launch and values are
// the caller's to choose, and its ${VAR}s resolve through the fleet's secret
// store as an apply's would, so running one is running whatever a config write
// could make the company run, with any stored credential handed to it.
func mayRunDraft(w http.ResponseWriter, r *http.Request, guard authz.Guard) bool {
	return authz.Admit(w, r, guard, authz.Policy{Action: authz.ActionConfigWrite}) &&
		authz.Admit(w, r, guard, authz.Policy{Action: authz.ActionSecretWrite})
}

// decodeBody reads a gesture's JSON body into out, refusing a field the route
// does not read; an empty body leaves out as it is.
func decodeBody(w http.ResponseWriter, r *http.Request, out any, shape string) bool {
	return decodeBodyWithin(w, r, out, shape, maxCheckBody)
}

// decodeBodyWithin is [decodeBody] for a body of at most limit bytes.
func decodeBodyWithin(w http.ResponseWriter, r *http.Request, out any, shape string, limit int64) bool {
	raw, err := httpjson.ReadBody(w, r, limit)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body is " + shape})
		return false
	}
	return true
}

// CheckAnswerOf is a test as the routes answer it.
func CheckAnswerOf(c Check) CheckAnswer {
	out := CheckAnswer{Server: c.Server, Seat: c.Seat, Draft: c.Draft, At: c.At, By: c.By, Node: c.Node, OK: c.OK,
		Nodes: make([]queries.MCPCheckNodeView, 0, len(c.Nodes)), Tools: []CheckedTool{}, Warnings: []string{}}
	var served *mcp.TestReport
	for i, n := range c.Nodes {
		out.Nodes = append(out.Nodes, queries.MCPCheckNodeOf(n.Node, n.Answered, n.Reason, n.Report))
		if served == nil && n.Answered && n.Report.Serving() {
			served = &c.Nodes[i].Report
		}
	}
	if served == nil {
		return out
	}
	id := served.Identity
	out.ServerInfo = &coord.MCPServerInfo{Name: id.Name, Title: id.Title, Version: id.Version, Protocol: id.Protocol}
	for _, t := range served.Tools {
		risk := mcp.RiskOf(t.Annotations)
		out.Tools = append(out.Tools, CheckedTool{Name: t.Name, Raw: t.Raw, Title: t.Title, Description: t.Description,
			Annotations: hintsOf(t.Annotations), Risk: risk.Risk, RiskDeclared: risk.Declared,
			External: risk.External, StartingLevel: mcp.StartingLevel(risk), Refused: t.Refused})
		if t.Refused != "" {
			out.Warnings = append(out.Warnings, t.Refused.Sentence(t.Name, strings.TrimSuffix(t.Name, t.Raw)))
		}
	}
	return out
}

// ProbeAnswerOf is an address check as `POST /mcp-probe` answers it.
func ProbeAnswerOf(address string, c mcp.AddressCheck) ProbeAnswer {
	out := ProbeAnswer{Address: redact.URL(address), Auth: c.Auth,
		Check: queries.MCPCheckNodeOf("", true, "", c.Test)}
	out.Reachable = c.Auth != ""
	if c.Test.Serving() {
		n := 0
		for _, t := range c.Test.Tools {
			if t.Refused == "" {
				n++
			}
		}
		out.Tools = &n
	}
	if o := c.OAuth; o != nil {
		out.OAuth = &ProbeOAuth{ResourceMetadata: o.ResourceMetadata, Resource: o.Resource,
			AuthorizationServers: o.AuthorizationServers, Issuer: o.Issuer, Scopes: o.Scopes, Client: o.Client,
			PKCE: o.PKCE}
		if o.Failure != nil {
			out.OAuth.Failure = &queries.MCPCheckFailure{Class: o.Failure.Class, Said: o.Failure.Said}
		}
	}
	if entry, ok := catalogue.Recognise(catalogue.Launch{Transport: catalogue.TransportHTTP, URL: address}); ok {
		out.Catalogue, out.Title, out.Vendor = entry.ID, entry.Title, entry.Vendor
	}
	return out
}

// hintsOf is a tool's hints as words, as every tool row carries them.
func hintsOf(a mcp.Annotations) queries.ToolHints {
	return queries.ToolHints{ReadOnly: a.ReadOnly.String(), Destructive: a.Destructive.String(),
		Idempotent: a.Idempotent.String(), OpenWorld: a.OpenWorld.String()}
}

// One MCP server's health and the facts the engine resolves about it: what
// its instances on every node say, and what its configuration means.

package queries

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
)

// MCPInstanceReader reads one MCP server's instances from the nodes holding
// them, at query time (internal/engine's mcpinstances.go, ADR-0021): this
// node's own directly and every other node's over the broker, each node asked
// named with whether it answered.
type MCPInstanceReader interface {
	MCPInstances(ctx context.Context, server string, nodes []string) (coord.MCPInstanceRead, error)
}

// MCPInstanceView is one instance of the server, as a node holding it said.
type MCPInstanceView struct {
	// Seat is the handle of the seat it runs for, "" for a shared server's
	// one instance; Node the node running it.
	Seat string `json:"seat"`
	Node string `json:"node"`
	// State is running, failed or waiting.
	State coord.MCPInstanceState `json:"state"`
	// Since is when that state began: when its session started, when it
	// stopped serving or first failed to start, or when it was first held
	// back. Null where the node did not say.
	Since *time.Time `json:"since"`
	// Restarts is how many times its node brought it back since it was
	// started from its spec.
	Restarts int `json:"restarts"`
	Tools    int `json:"tools"`
	// Failure is why a failed instance is not serving, null otherwise; Wait
	// what a waiting one waits for, "" otherwise.
	Failure *MCPFailureView `json:"failure"`
	Wait    string          `json:"wait"`
	// LastFailure is its latest failure, kept after it came back; null when
	// it has had none since it was started from its spec.
	LastFailure *MCPFailureView `json:"last_failure"`
	// LastAnswer is when it last answered: a start that listed its tools, or
	// a tool call that came back. Null before either.
	LastAnswer *time.Time `json:"last_answer"`
}

// MCPFailureView is one failure of an instance: why in a class the page words
// ([mcp.FailureClass], "" where none fits), the engine's sentence, when, and
// where.
type MCPFailureView struct {
	Class mcp.FailureClass `json:"class"`
	// Said carries what a server or its process said, which is somebody
	// else's text: redacted and bounded where it was read, and shown as
	// text, never acted on.
	Said string    `json:"said" untrusted:"fence"`
	At   time.Time `json:"at"`
	Node string    `json:"node"`
	Seat string    `json:"seat"`
}

// MCPInstanceNodeView is one node asked for its instances: whether it answered
// in time and why not, and how many instances it holds, so a short list of
// instances is never read as a server with fewer.
type MCPInstanceNodeView struct {
	Node     string `json:"node"`
	Answered bool   `json:"answered"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
}

// MCPAuth is how the engine proves who it is to a server, decided from its
// configuration.
type MCPAuth string

const (
	// MCPAuthNone is a server the engine sends no credential to.
	MCPAuthNone MCPAuth = "none"
	// MCPAuthHeaders is one it sends values the configuration sets for it:
	// HTTP headers to a remote server, environment variables to a local one.
	MCPAuthHeaders MCPAuth = "headers"
	// MCPAuthIntegration is a per-agent server that reads each agent's own
	// credential from one of the company's integrations
	// ([config.MCPServer.SeatBindings]).
	MCPAuthIntegration MCPAuth = "integration"
)

// MCPAuths is every value, for a reader's copy.
var MCPAuths = []MCPAuth{MCPAuthNone, MCPAuthHeaders, MCPAuthIntegration}

// MCPRunsOnEngineHost is where a local server runs: on the engine's host, each
// node that runs an instance of it starting its command. A remote server runs
// wherever its address is, which the engine does not say.
const MCPRunsOnEngineHost = "engine_host"

// MCPResolved is what the engine decides about a server from its
// configuration, so a screen never re-derives it.
type MCPResolved struct {
	// Auth is how the engine proves who it is to the server.
	Auth MCPAuth `json:"auth"`
	// RunsOn is [MCPRunsOnEngineHost] for a local server, "" for a remote
	// one.
	RunsOn string `json:"runs_on"`
	// Timeouts are the startup and per-call bounds in force, the engine's
	// defaults applied where the configuration sets none.
	Timeouts MCPTimeouts `json:"timeouts"`
	// Binding is the integration whose per-agent credential the server
	// reads, null for a server that reads none.
	Binding *MCPBinding `json:"binding"`
	// BoxSeats are the seats whose executor box is given the server
	// (`sandbox.mcp.servers`), each with the cell its box runs in. ALWAYS A
	// LIST.
	BoxSeats []MCPBoxSeat `json:"box_seats"`
	// Warnings are placements the engine expects to fail, each naming the
	// seat and why. ALWAYS A LIST.
	Warnings []MCPPlacementWarning `json:"warnings"`
}

// MCPTimeouts are the bounds a server is held to, in seconds, and whether each
// is the engine's default.
type MCPTimeouts struct {
	StartupSeconds float64 `json:"startup_seconds"`
	RequestSeconds float64 `json:"request_seconds"`
	StartupDefault bool    `json:"startup_default"`
	RequestDefault bool    `json:"request_default"`
}

// MCPBinding is the integration a per-agent server reads each agent's
// credential from.
type MCPBinding struct {
	// Vendor is the integration (`atlassian`, `github`, ...).
	Vendor string `json:"vendor"`
	// By says how the engine knows: `declared` by the server's
	// `seat_credentials`, or `recognised` from its address or command.
	By string `json:"by"`
	// Keys are the headers or variables the credential reaches the server
	// under, by name. ALWAYS A LIST.
	Keys []MCPBoundKey `json:"keys"`
}

// The two ways the engine knows a server's binding.
const (
	MCPBindingDeclared   = "declared"
	MCPBindingRecognised = "recognised"
)

// MCPBoundKey is one credential a per-agent server is started with: which of
// the integration's credentials, and the header or variable it goes in.
type MCPBoundKey struct {
	Field string `json:"field"`
	Key   string `json:"key"`
}

// MCPBoxSeat is one seat whose executor box is given the server, and the cell
// that box runs in (`direct`, `container` or `e2b`).
type MCPBoxSeat struct {
	Seat string `json:"seat"`
	Cell string `json:"cell"`
}

// MCPPlacementWarningKind is why the engine expects a placement to fail.
type MCPPlacementWarningKind string

const (
	// MCPWarnPrivateAddressInCloudBox is a remote server on a private
	// network given to a box in the `e2b` cell, which runs outside that
	// network and cannot reach it.
	MCPWarnPrivateAddressInCloudBox MCPPlacementWarningKind = "private_address_in_cloud_box"
	// MCPWarnCommandInBox is a local server given to a box that runs its
	// own image (`container` or `e2b`), which must provide the command.
	MCPWarnCommandInBox MCPPlacementWarningKind = "command_in_box"
)

// MCPPlacementWarningKinds is every kind, for a reader's copy.
var MCPPlacementWarningKinds = []MCPPlacementWarningKind{MCPWarnPrivateAddressInCloudBox, MCPWarnCommandInBox}

// MCPPlacementWarning is one placement the engine expects to fail.
type MCPPlacementWarning struct {
	Kind    MCPPlacementWarningKind `json:"kind"`
	Seat    string                  `json:"seat"`
	Cell    string                  `json:"cell"`
	Message string                  `json:"message"`
}

// resolvedOf is what the engine decides about server from the company
// document it is declared in.
func resolvedOf(company *config.Company, server *config.MCPServer) MCPResolved {
	out := MCPResolved{
		Auth:     authOf(server),
		Timeouts: timeoutsOf(server),
		Binding:  bindingOf(server),
		BoxSeats: []MCPBoxSeat{},
		Warnings: []MCPPlacementWarning{},
	}
	if server.Kind() == config.TransportStdio {
		out.RunsOn = MCPRunsOnEngineHost
	}
	if company == nil || !server.IsEnabled() {
		// A SERVER TURNED OFF IS GIVEN TO NO BOX ([config.MCPServer.IsEnabled]),
		// so it has no box to warn about either.
		return out
	}
	for box := range company.SeatBoxes() {
		if box.Seat.Sandbox == nil || !slices.Contains(box.Seat.Sandbox.MCP.Servers, server.Name) {
			continue
		}
		handle := box.Seat.Seat().Handle()
		out.BoxSeats = append(out.BoxSeats, MCPBoxSeat{Seat: handle, Cell: string(box.Cell)})
		if w, ok := placementWarning(server, handle, box.Cell); ok {
			out.Warnings = append(out.Warnings, w)
		}
	}
	return out
}

// placementWarning is why a box of a seat in cell, given server, is expected
// to fail, false where nothing is.
func placementWarning(server *config.MCPServer, seat string, cell config.Placement) (MCPPlacementWarning, bool) {
	switch {
	case server.Kind() == config.TransportHTTP && cell == config.PlacementE2B && privateAddress(server.URL):
		return MCPPlacementWarning{Kind: MCPWarnPrivateAddressInCloudBox, Seat: seat, Cell: string(cell),
			Message: "seat " + seat + "'s executor runs in an e2b box, outside the company's network, " +
				"and this server's address is on a private network the box cannot reach: run the " +
				"seat in a direct or container box, or give the server an address the box can reach"}, true
	case server.Kind() == config.TransportStdio && (cell == config.PlacementContainer || cell == config.PlacementE2B):
		return MCPPlacementWarning{Kind: MCPWarnCommandInBox, Seat: seat, Cell: string(cell),
			Message: "seat " + seat + "'s executor runs in a " + string(cell) + " box, which starts this " +
				"server's command from its own image: the image, or a setup step, must provide " +
				strings.TrimSpace(server.Command)}, true
	}
	return MCPPlacementWarning{}, false
}

// privateAddress reports whether a server's configured address names a host
// on a private network, by the engine's one rule ([config.MCPPrivateHost]).
// An address whose host is still a ${VAR} reference is judged by nobody until
// it resolves, so it is not one.
func privateAddress(raw string) bool {
	if strings.Contains(raw, "${") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return config.MCPPrivateHost(u.Hostname())
}

// authOf is how the engine proves who it is to server.
func authOf(server *config.MCPServer) MCPAuth {
	switch {
	case len(server.SeatBindings()) > 0:
		return MCPAuthIntegration
	case server.Kind() == config.TransportHTTP && len(server.Headers) > 0,
		server.Kind() == config.TransportStdio && len(server.Env) > 0:
		return MCPAuthHeaders
	}
	return MCPAuthNone
}

// timeoutsOf is the bounds server is held to, the engine's defaults applied
// where its configuration sets none (zero is "the default", as the spec reads
// it).
func timeoutsOf(server *config.MCPServer) MCPTimeouts {
	out := MCPTimeouts{
		StartupSeconds: server.StartupTimeoutSeconds, RequestSeconds: server.RequestTimeoutSeconds,
	}
	if out.StartupSeconds <= 0 {
		out.StartupSeconds, out.StartupDefault = mcp.DefaultStartupTimeout.Seconds(), true
	}
	if out.RequestSeconds <= 0 {
		out.RequestSeconds, out.RequestDefault = mcp.DefaultRequestTimeout.Seconds(), true
	}
	return out
}

// bindingOf is the integration server reads each agent's credential from, nil
// for one that reads none.
func bindingOf(server *config.MCPServer) *MCPBinding {
	bound := server.SeatBindings()
	if len(bound) == 0 {
		return nil
	}
	out := &MCPBinding{Vendor: bound[0].Kind, By: MCPBindingRecognised, Keys: make([]MCPBoundKey, 0, len(bound))}
	if len(server.SeatCredentials) > 0 {
		out.By = MCPBindingDeclared
	}
	for _, k := range bound {
		out.Keys = append(out.Keys, MCPBoundKey{Field: k.Field, Key: k.Key})
	}
	return out
}

// instanceNodes is the live nodes to ask for the server's instances: every
// node whose last beat reports one, and every node whose beat carried no
// report, which may hold one nobody has heard of.
func instanceNodes(row MCPServerRow) []string {
	var out []string
	for _, cell := range row.Nodes {
		if !cell.Reported || cell.Started+cell.Failed+cell.Waiting > 0 {
			out = append(out, cell.Node)
		}
	}
	return out
}

// instancesOf reads the server's instances from the nodes holding them and
// names them by seat handle on organization; with the latest answer and the
// latest failure across them.
func instancesOf(read coord.MCPInstanceRead, organization *org.Organization) (
	[]MCPInstanceView, []MCPInstanceNodeView, *time.Time, *MCPFailureView) {

	views := make([]MCPInstanceView, 0, len(read.Instances))
	var lastAnswer *time.Time
	var lastFailure *MCPFailureView
	for _, in := range read.Instances {
		seat := instanceSeat(organization, in.Seat)
		view := MCPInstanceView{
			Seat: seat, Node: in.Node, State: in.State, Since: instantOrNull(in.Since),
			Restarts: max(in.Restarts, 0), Tools: max(in.Tools, 0), Wait: in.Wait,
			Failure: failureView(in.Failure, in.Node, seat), LastFailure: failureView(in.LastFailure, in.Node, seat),
			LastAnswer: instantOrNull(in.LastAnswer),
		}
		views = append(views, view)
		if view.LastAnswer != nil && (lastAnswer == nil || view.LastAnswer.After(*lastAnswer)) {
			lastAnswer = view.LastAnswer
		}
		for _, f := range []*MCPFailureView{view.Failure, view.LastFailure} {
			if f != nil && (lastFailure == nil || f.At.After(lastFailure.At)) {
				lastFailure = f
			}
		}
	}
	nodes := make([]MCPInstanceNodeView, 0, len(read.Nodes))
	for _, n := range read.Nodes {
		nodes = append(nodes, MCPInstanceNodeView{Node: n.Node, Answered: n.Answered, Reason: n.Reason, Count: n.Count})
	}
	return views, nodes, lastAnswer, lastFailure
}

// failureView is a node's failure as the page reads it: a class this build
// does not name reads as none, never as a word the page cannot draw.
func failureView(f *coord.MCPInstanceFailure, node, seat string) *MCPFailureView {
	if f == nil {
		return nil
	}
	class := mcp.FailureClass(f.Class)
	if !class.Valid() {
		class = ""
	}
	return &MCPFailureView{Class: class, Said: f.Said, At: f.At, Node: node, Seat: seat}
}

// instantOrNull is t, or null for the zero instant.
func instantOrNull(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
)

// A value stored from an MCP server's page is sealed whole under one variable,
// and its key is pointed at that variable: the server's own map, one seat's
// `mcp_env` block for a per-agent server, or the server's OAuth client's
// secret (D5).

// MCPValueAs is where a value goes: to the server, as an HTTP header of a
// remote server or an environment variable of a local one, or to the server's
// authorization server, as its OAuth client's secret.
type MCPValueAs string

const (
	// MCPValueHeader is a header a remote (http) server is sent.
	MCPValueHeader MCPValueAs = "header"
	// MCPValueEnv is an environment variable a local (stdio) server is
	// started with.
	MCPValueEnv MCPValueAs = "env"
	// MCPValueOAuth is the secret of the company's own OAuth client a
	// server signs in with (`oauth.client_secret`): sent to its
	// authorization server, never to the server.
	MCPValueOAuth MCPValueAs = "oauth"
)

// MCPValueAses is every value, for a reader's copy.
var MCPValueAses = []MCPValueAs{MCPValueHeader, MCPValueEnv, MCPValueOAuth}

// MCPValueClientSecret is the key a server's OAuth client's secret is stored
// under from its page: the field's own path in the entry. No header beside an
// oauth block may be named it (validateOAuth), so the key names one value.
const MCPValueClientSecret = "oauth.client_secret"

// Valid reports whether a is a place this build sends a value.
func (a MCPValueAs) Valid() bool { return slices.Contains(MCPValueAses, a) }

// ValueAs is where the server is sent its values, by its transport.
func (m *MCPServer) ValueAs() MCPValueAs {
	if m.Kind() == TransportHTTP {
		return MCPValueHeader
	}
	return MCPValueEnv
}

// ownValues is the server's own map of the values it is sent, by transport.
func (m *MCPServer) ownValues() map[string]string {
	if m.Kind() == TransportHTTP {
		return m.Headers
	}
	return m.Env
}

// MCPValueVariable is MCP_<SERVER>_<KEY>[_<SEAT>], upper-snaked by
// [envref.SealedName].
func MCPValueVariable(server, key, seat string) string {
	return envref.SealedName("MCP_"+server, key, seat)
}

// The refusals of a value's slot, each naming what the person can change.
var (
	// ErrNoMCPServerEntry is a server the document does not declare.
	ErrNoMCPServerEntry = errors.New("config: no such MCP server")
	// ErrMCPValueSeatOnShared is a seat named for a shared server, which
	// runs one instance for the company with the values of its own.
	ErrMCPValueSeatOnShared = errors.New("config: a shared MCP server takes no seat's value")
	// ErrMCPValueNoSeat is a seat the organization does not have as an
	// agent seat.
	ErrMCPValueNoSeat = errors.New("config: no such agent seat")
	// ErrMCPValueNotGranted is a seat the server is not granted to.
	ErrMCPValueNotGranted = errors.New("config: the seat is not granted the MCP server")
	// ErrMCPValueBound is a key an integration hands each agent its own
	// value under, which no person stores.
	ErrMCPValueBound = errors.New("config: the key carries each agent's credential from an integration")
	// ErrMCPValueKey is a key that names nothing a server can be sent.
	ErrMCPValueKey = errors.New("config: not a header or variable name")
	// ErrMCPValueNoSecret is the client secret of a server whose sign-in
	// has no secret to store: no oauth block, or a registration (a client
	// metadata document, a dynamic registration) whose client is public.
	ErrMCPValueNoSecret = errors.New("config: the MCP server signs in with no client secret")
)

// MCPValueSharedError is a derived variable another field already reads.
type MCPValueSharedError struct {
	// Variable is the variable, and Readers the other fields reading it.
	Variable string
	Readers  []string
}

func (e *MCPValueSharedError) Error() string {
	return fmt.Sprintf("config: %s is also read by %s, so a value stored under it would reach "+
		"%s too", e.Variable, strings.Join(e.Readers, ", "), pluralIt(len(e.Readers)))
}

func pluralIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// MCPValueSlot is where one value of an MCP server is written and the variable
// it is sealed under.
type MCPValueSlot struct {
	// Server is the server's name, and Seat the handle of the seat whose
	// value it is, "" for the server's own.
	Server string
	Seat   string
	// Key is the map's own spelling of the key ([MCPServer.ValueKey]), else
	// the one asked for.
	Key string
	As  MCPValueAs
	// Path is the field the value's pointer is written at.
	Path Path
	// Current is what that field holds now, "" where the key is not
	// written in that map.
	Current string
	// Variable is what a value stored for the key is sealed under, and
	// Repoint whether the field must be written to read it.
	Variable string
	Repoint  bool
}

// Pointer is the field's value once it reads the slot's variable.
func (s MCPValueSlot) Pointer() string { return "${" + s.Variable + "}" }

// MCPValueSlot places server's key: the key's own variable where nothing else
// reads it, else [MCPValueVariable], refused when another field reads that.
// A seat's slot is its `mcp_env` block, for a seat already granted the server.
func (c *Company) MCPValueSlot(server, seat, key string) (MCPValueSlot, error) {
	i := slices.IndexFunc(c.MCPServers, func(s MCPServer) bool { return s.Name == server })
	if i < 0 {
		return MCPValueSlot{}, fmt.Errorf("%w: %q", ErrNoMCPServerEntry, server)
	}
	srv := &c.MCPServers[i]
	key = strings.TrimSpace(key)
	if key == MCPValueClientSecret {
		return c.clientSecretSlot(srv, i, seat)
	}
	if key == "" || strings.ContainsAny(key, " \t\r\n:=") || strings.ContainsRune(key, 0) {
		return MCPValueSlot{}, fmt.Errorf("%w: %q", ErrMCPValueKey, key)
	}
	for _, bound := range srv.SeatBindings() {
		if !bound.Sign && srv.ValueKey(bound.Key) == srv.ValueKey(key) {
			return MCPValueSlot{}, fmt.Errorf("%w: %s of %q is each agent's own %s credential",
				ErrMCPValueBound, key, server, bound.Kind)
		}
	}
	slot := MCPValueSlot{Server: server, Seat: seat, Key: key, As: srv.ValueAs()}
	var block map[string]string
	switch {
	case seat == "":
		block = srv.ownValues()
		own := "headers"
		if slot.As == MCPValueEnv {
			own = "env"
		}
		slot.Path = at(idx(field("mcp_servers"), i), own)
	case srv.IsShared():
		return MCPValueSlot{}, fmt.Errorf("%w: %q", ErrMCPValueSeatOnShared, server)
	default:
		role, path, err := c.grantedSeat(srv, seat)
		if err != nil {
			return MCPValueSlot{}, err
		}
		block = role.MCPEnv[server]
		slot.Path = entry(at(path, "mcp_env"), server)
	}
	for written, value := range block {
		if srv.ValueKey(written) == srv.ValueKey(key) {
			slot.Key, slot.Current = written, value
		}
	}
	slot.Path = entry(slot.Path, slot.Key)
	return c.placed(slot)
}

// placed is slot with its variable: the one its field reads already where it
// reads one whole and nothing else does, else [MCPValueVariable], refused when
// another field reads that.
func (c *Company) placed(slot MCPValueSlot) (MCPValueSlot, error) {
	readers := c.readersOf(slot.Path.String())
	if name, whole := envref.Whole(slot.Current); whole && len(readers[name]) == 0 {
		slot.Variable = name
		return slot, nil
	}
	slot.Variable, slot.Repoint = MCPValueVariable(slot.Server, slot.Key, slot.Seat), true
	if others := readers[slot.Variable]; len(others) > 0 {
		return MCPValueSlot{}, &MCPValueSharedError{Variable: slot.Variable, Readers: others}
	}
	return slot, nil
}

// clientSecretSlot places the secret of the OAuth client the server at index i
// signs in with: a service identity's, or the company's own client's. The
// server's own, never a seat's: a sign-in is the company's.
func (c *Company) clientSecretSlot(srv *MCPServer, i int, seat string) (MCPValueSlot, error) {
	switch o := srv.OAuth; {
	case o == nil, !o.ServiceIdentity() && o.Client != catalogue.ClientOwn:
		return MCPValueSlot{}, fmt.Errorf("%w: %q", ErrMCPValueNoSecret, srv.Name)
	case seat != "":
		return MCPValueSlot{}, fmt.Errorf("%w: %q", ErrMCPValueSeatOnShared, srv.Name)
	}
	return c.placed(MCPValueSlot{Server: srv.Name, Key: MCPValueClientSecret, As: MCPValueOAuth,
		Path: at(at(idx(field("mcp_servers"), i), "oauth"), "client_secret"), Current: srv.OAuth.ClientSecret})
}

// readersOf maps each variable the document reads to its fields but except.
func (c *Company) readersOf(except string) map[string][]string {
	out := map[string][]string{}
	for _, ref := range c.VariablesRead() {
		if ref.Path != except {
			out[ref.Name] = append(out[ref.Name], ref.Path)
		}
	}
	return out
}

// grantedSeat is the authored agent seat handle names, refused unless the
// server is granted to it in the built organization.
func (c *Company) grantedSeat(server *MCPServer, handle string) (*Role, Path, error) {
	built, _ := c.organization()
	seat := built.AgentSeatByHandle(handle)
	if seat == nil {
		return nil, nil, fmt.Errorf("%w: %q", ErrMCPValueNoSeat, handle)
	}
	if _, reached := server.Reach(seat); !reached {
		return nil, nil, fmt.Errorf("%w: %q is not granted %q", ErrMCPValueNotGranted, handle, server.Name)
	}
	for role, path := range c.EachRole() {
		if role.Handle == handle {
			return role, path, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: %q", ErrMCPValueNoSeat, handle)
}

// PointMCPValue points slot's key at its variable, replacing any other
// spelling of the key: the one edit a stored value makes to the document.
func (c *Company) PointMCPValue(slot MCPValueSlot) error {
	i := slices.IndexFunc(c.MCPServers, func(s MCPServer) bool { return s.Name == slot.Server })
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNoMCPServerEntry, slot.Server)
	}
	srv := &c.MCPServers[i]
	if slot.As == MCPValueOAuth {
		if srv.OAuth == nil {
			return fmt.Errorf("%w: %q", ErrMCPValueNoSecret, slot.Server)
		}
		srv.OAuth.ClientSecret = slot.Pointer()
		return nil
	}
	if slot.Seat == "" {
		values := srv.ownValues()
		if values == nil {
			values = map[string]string{}
		}
		for written := range values {
			if srv.ValueKey(written) == srv.ValueKey(slot.Key) {
				delete(values, written)
			}
		}
		values[slot.Key] = slot.Pointer()
		if srv.Kind() == TransportHTTP {
			srv.Headers = values
		} else {
			srv.Env = values
		}
		return nil
	}
	for role := range c.EachRole() {
		if role.Handle != slot.Seat {
			continue
		}
		if role.MCPEnv == nil {
			role.MCPEnv = org.MCPEnv{}
		}
		block := role.MCPEnv[slot.Server]
		if block == nil {
			block = map[string]string{}
		}
		for written := range block {
			if srv.ValueKey(written) == srv.ValueKey(slot.Key) {
				delete(block, written)
			}
		}
		block[slot.Key] = slot.Pointer()
		role.MCPEnv[slot.Server] = block
		return nil
	}
	return fmt.Errorf("%w: %q", ErrMCPValueNoSeat, slot.Seat)
}

// PointUnwrittenClientSecret points the client secret of the service identity
// called name at the variable `PUT /mcp-servers/{name}/values/oauth.client_secret`
// stores it under, where its entry names no secret yet; any other entry is
// left as it is.
//
// A SERVICE IDENTITY CANNOT BE WRITTEN WITHOUT A SECRET ([MCPOAuth]'s rules),
// and its secret is stored write-only, after the entry, by that route, which
// chooses the variable ([Company.MCPValueSlot]). An entry written through the
// API therefore names the variable the route will choose, rather than every
// writer deriving it: a header the route adds is pointed by the route, and the
// one value an entry cannot leave out is pointed by the write that adds it.
// Refused, as the route would refuse it, where another field reads that
// variable ([*MCPValueSharedError]).
func (c *Company) PointUnwrittenClientSecret(name string) error {
	i := slices.IndexFunc(c.MCPServers, func(s MCPServer) bool { return s.Name == name })
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNoMCPServerEntry, name)
	}
	srv := &c.MCPServers[i]
	if o := srv.OAuth; o == nil || !o.ServiceIdentity() || strings.TrimSpace(o.ClientSecret) != "" {
		return nil
	}
	slot, err := c.clientSecretSlot(srv, i, "")
	if err != nil {
		return err
	}
	return c.PointMCPValue(slot)
}

// MCPValueKey is one header or variable a server is sent, by name only.
type MCPValueKey struct {
	// Key is the document's spelling; As where it goes.
	Key string
	As  MCPValueAs
	// Variable is the one variable the value reads, "" for none or several;
	// Reads is every variable it reads.
	Variable string
	Reads    []string
	// Optional is [MCPServer.OptionalKey].
	Optional bool
	// BoundBy is the integration binding the key per agent, "" for none.
	BoundBy string
}

// MCPValueKeys lists server's keys by name, its integration-bound ones
// included; with seat, as that seat's instance reads them. False for an
// undeclared server or a seat that is not one of its agents.
func (c *Company) MCPValueKeys(server, seat string) ([]MCPValueKey, bool) {
	i := slices.IndexFunc(c.MCPServers, func(s MCPServer) bool { return s.Name == server })
	if i < 0 {
		return nil, false
	}
	srv := &c.MCPServers[i]
	written := map[string]string{}
	keys := map[string]string{}
	lay := func(block map[string]string) {
		for key, value := range block {
			id := srv.ValueKey(key)
			if old, ok := keys[id]; ok {
				delete(written, old)
			}
			keys[id], written[key] = key, value
		}
	}
	lay(srv.ownValues())
	if seat != "" {
		built, _ := c.organization()
		role := built.AgentSeatByHandle(seat)
		if role == nil {
			return nil, false
		}
		lay(role.MCPEnv[server])
	}
	bound := map[string]BoundSeatKey{}
	for _, b := range srv.SeatBindings() {
		if b.Sign {
			continue
		}
		bound[srv.ValueKey(b.Key)] = b
		if _, ok := keys[srv.ValueKey(b.Key)]; !ok {
			keys[srv.ValueKey(b.Key)] = b.Key
		}
	}
	out := make([]MCPValueKey, 0, len(keys)+1)
	// THE OAUTH CLIENT'S SECRET, where the server signs in with one: the
	// server's own value whichever seat asks, since a sign-in is the
	// company's.
	if o := srv.OAuth; o != nil && o.ClientSecret != "" {
		row := MCPValueKey{Key: MCPValueClientSecret, As: MCPValueOAuth, Reads: envref.Names(o.ClientSecret)}
		if len(row.Reads) == 1 {
			row.Variable = row.Reads[0]
		}
		out = append(out, row)
	}
	for _, id := range slices.Sorted(maps.Keys(keys)) {
		key := keys[id]
		row := MCPValueKey{Key: key, As: srv.ValueAs(), Optional: srv.OptionalKey(key)}
		if value, ok := written[key]; ok {
			row.Reads = envref.Names(value)
		}
		if b, ok := bound[id]; ok {
			row.BoundBy = b.Kind
			if seat != "" && len(row.Reads) == 0 {
				row.Reads = []string{envref.SealedName(b.Kind, b.Field, seat)}
			}
		}
		if len(row.Reads) == 1 {
			row.Variable = row.Reads[0]
		}
		out = append(out, row)
	}
	return out, true
}

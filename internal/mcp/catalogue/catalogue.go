// Package catalogue is the MCP servers this build knows: the curated table an
// operator adds a server from, the one recognition of a configured server by
// its address or its command, and the search of a community registry a
// company names.
//
// # One Go table, reviewed in pull requests
//
// The curated entries are source ([Curated]), not data fetched at run time.
// Every fact in one (the address, the header a static credential goes in and
// how its value is spelled, which sign-in the vendor offers, what an admin
// has to do at the vendor first) is a claim about one vendor's server read on
// one day, and each entry records that day as ReadAt. A claim a vendor moves
// is corrected in a pull request somebody reviews, and the table answers the
// same on an air-gapped node as on one with egress. An entry launched through
// a package runner names the exact release its facts were read against
// (Release), because a floating version runs a build nobody read. The docs
// that launch one of these packages are held to the same release by
// internal/config's toolservers_test.go, which reads its pins from here.
//
// # Recognition, one rule for three readers
//
// A configured server is recognised as an entry by its address or its command
// ([Recognise]), and three readers ask the same question: the status answer
// names the entry and the vendor mark it draws, a community listing is drawn
// with the vetted mark of the server it points at, and internal/config binds
// a seat's own credential to a per-seat server it recognises (D15, see
// toolbinding.go there). The binding is the security-bearing one: whatever a
// recognition matches is handed a seat's credential. So an address is
// matched by whole host labels, never a substring, and the matchers are the
// ones the binding table had before it moved here, held to it case by case by
// this package's suite.
//
// # A community registry is untrusted remote content
//
// A company may name a subregistry that speaks the MCP Registry's v0.1 API
// (Tier B `mcp_registry`). Off until set: the official registry is a frozen
// preview whose own guidance is that a host reads a curated mirror of it, and
// which mirror is the company's decision. A listing is somebody else's words
// about somebody else's server, so [Registry.Search] bounds what it reads and
// how many listings it keeps, refuses every field that is not the shape this
// build can offer, keeps only a launch it can spell without a shell and an
// address that is https, never a package from a registry other than the
// public one its runner fetches from, and drops everything else. It dials the
// registry and nothing a listing names: no icon, no repository, no remote. A
// listing is marked Community and is only ever a draft for the same add flow
// an address typed by hand goes through, so nothing it says starts a process
// or skips a test.
//
// # Who has to agree on it
//
// Nobody: the curated table is compiled in, and a community search is a read
// of the registry made for the person asking, kept nowhere.
package catalogue

import (
	"slices"
	"time"
)

// Transport is how the engine reaches a server, as internal/config spells it.
type Transport string

const (
	// TransportHTTP is a remote Streamable HTTP server.
	TransportHTTP Transport = "http"
	// TransportStdio is a local subprocess the engine launches.
	TransportStdio Transport = "stdio"
)

// Valid reports whether t is a transport this build speaks.
func (t Transport) Valid() bool { return t == TransportHTTP || t == TransportStdio }

// AuthMethod is how a server is signed in to: what the add flow offers first.
type AuthMethod string

const (
	// AuthNone needs no sign-in. A local server's variables are its
	// configuration, entered beside the launch, rather than a sign-in.
	AuthNone AuthMethod = "none"
	// AuthHeaders sends a static credential in a request header.
	AuthHeaders AuthMethod = "headers"
	// AuthOAuth signs the company in at the vendor with OAuth.
	AuthOAuth AuthMethod = "oauth"
	// AuthIntegration has the company's integration for the vendor hand
	// each agent its own credential (a per-agent server).
	AuthIntegration AuthMethod = "integration"
)

// AuthMethods is every method, in the order a person is offered them.
var AuthMethods = []AuthMethod{AuthIntegration, AuthOAuth, AuthHeaders, AuthNone}

// Valid reports whether a is a method this build names.
func (a AuthMethod) Valid() bool { return slices.Contains(AuthMethods, a) }

// OAuthClient is how the engine registers as an OAuth client at a vendor that
// offers OAuth: what the vendor's documentation says it supports.
type OAuthClient string

const (
	// ClientCIMD publishes the engine's client metadata at an https address
	// the vendor reads (a Client ID Metadata Document).
	ClientCIMD OAuthClient = "cimd"
	// ClientDCR registers a client at the vendor on first use (RFC 7591).
	ClientDCR OAuthClient = "dcr"
	// ClientOwn uses a client an admin registered at the vendor beforehand.
	ClientOwn OAuthClient = "own"
)

// OAuthClients is every registration, in order of preference.
var OAuthClients = []OAuthClient{ClientCIMD, ClientDCR, ClientOwn}

// Valid reports whether c is a registration this build names.
func (c OAuthClient) Valid() bool { return slices.Contains(OAuthClients, c) }

// Input is one header or variable a server reads, in the registry's
// KeyValueInput shape plus what the add flow needs to fill it.
type Input struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Required is whether the server works without it.
	Required bool `json:"required"`
	// Secret is whether the value is a credential, entered write-only.
	Secret bool `json:"secret"`
	// Value is a fixed value the entry sets, never a secret ("" for one the
	// operator gives): a switch the server needs to be useful, such as the
	// one that lets slack-mcp-server post.
	Value string `json:"value"`
	// Format is how a value is spelled, for a person typing one ("Bearer
	// <API key>"), or "".
	Format string `json:"format"`
	// FromIntegration is whether the entry's integration hands each agent
	// its own value under this key, so nobody types it.
	FromIntegration bool `json:"from_integration"`
}

// Hints is a tool's behavioural hints as an entry corrects them, for a server
// that under-declares its own; a nil hint is left as the server says it.
type Hints struct {
	ReadOnly    *bool `json:"read_only,omitempty"`
	Destructive *bool `json:"destructive,omitempty"`
	Idempotent  *bool `json:"idempotent,omitempty"`
	OpenWorld   *bool `json:"open_world,omitempty"`
}

// Entry is one server the catalogue offers: the registry's `server.json`
// facts, flattened into the one launch the add flow drafts, plus what the
// registry does not carry.
type Entry struct {
	// ID is the entry's key: the curated table's own (`github`), or the
	// name the registry lists a community server under
	// (`io.github.owner/server`), which holds a slash no curated key does.
	ID string `json:"id"`
	// Name is the server name the add flow suggests: the conventional one
	// for a curated entry (the name a seat's `mcp_env` block is keyed by),
	// derived from the listing's name for a community one.
	Name  string `json:"name"`
	Title string `json:"title"`
	// Vendor is the vetted mark the dashboard draws for it, or "" for the
	// generic server glyph. Never anything a listing supplied: a community
	// listing is drawn with a mark only when it points at a server the
	// curated table recognises.
	Vendor      string    `json:"vendor"`
	Description string    `json:"description"`
	Transport   Transport `json:"transport"`
	// URL is a remote server's address, "" for a local one.
	URL string `json:"url"`
	// Command and Args are a local server's launch, as an `mcp_servers`
	// entry spells them: passed as written, never through a shell.
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Headers are what a remote server reads, Env what a local one does.
	Headers []Input `json:"headers"`
	Env     []Input `json:"env"`
	// ToolPrefix is the prefix the entry's tools are filed under, or "".
	ToolPrefix string `json:"tool_prefix"`
	// ToolAnnotations corrects the hints of tools the server leaves
	// undeclared, keyed by the tool's own name.
	ToolAnnotations map[string]Hints `json:"tool_annotations"`
	// Auth are the sign-ins the vendor offers, the add flow's first first.
	Auth []AuthMethod `json:"auth"`
	// OAuthClient is how the engine registers at the vendor, "" where the
	// vendor offers no OAuth.
	OAuthClient OAuthClient `json:"oauth_client"`
	// Integration is the company integration that hands each agent its own
	// credential for this server, or "".
	Integration string `json:"integration"`
	// Prerequisite is what an admin must do at the vendor before the
	// server works, or "".
	Prerequisite string `json:"prerequisite"`
	// Docs is the vendor's page for the server, or "". Always https.
	Docs string `json:"docs"`
	// ReadAt is the day a curated entry's facts were last read from the
	// vendor (YYYY-MM-DD), "" for a community listing.
	ReadAt string `json:"read_at"`
	// Package and Release are the package a launch fetches through a
	// package runner and its exact release, the one a curated entry's facts
	// were read against, or "" for a launch that fetches nothing.
	Package string `json:"package"`
	Release string `json:"release"`

	// Community marks a listing from the company's registry: not reviewed
	// by anybody here.
	Community bool `json:"community"`
	// Publisher is the namespace the registry verified the listing's
	// publisher for (`io.github.owner`), "" for a curated entry.
	Publisher string `json:"publisher"`
	// Version is the listing's version, "" for a curated entry.
	Version string `json:"version"`
	// UpdatedAt is when the registry last saw the listing change, nil for
	// a curated entry or a listing that does not say.
	UpdatedAt *time.Time `json:"updated_at"`

	// recognise are how a configured server is recognised as this entry.
	recognise []recognition
}

// clone is a deep copy, so a caller holding an entry cannot change the table,
// with every list a list on the wire rather than null.
func (e Entry) clone() Entry {
	e.Args = append([]string{}, e.Args...)
	e.Headers = append([]Input{}, e.Headers...)
	e.Env = append([]Input{}, e.Env...)
	e.Auth = append([]AuthMethod{}, e.Auth...)
	hints := make(map[string]Hints, len(e.ToolAnnotations))
	for tool, h := range e.ToolAnnotations {
		hints[tool] = Hints{ReadOnly: copied(h.ReadOnly), Destructive: copied(h.Destructive),
			Idempotent: copied(h.Idempotent), OpenWorld: copied(h.OpenWorld)}
	}
	e.ToolAnnotations = hints
	e.recognise = slices.Clone(e.recognise)
	if e.UpdatedAt != nil {
		at := *e.UpdatedAt
		e.UpdatedAt = &at
	}
	return e
}

func copied(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

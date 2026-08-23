package config

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Bootstrap is Tier A — ops-owned, on disk, restart-only.
//
// It carries only what the engine needs to bring its own infrastructure up.
// Everything about the company lives in [Company], versioned in the store
// and delivered to a running engine.
//
// The Python engine's Tier A named a Pulsar broker, a PostgreSQL DSN and a
// pgvector knowledge store under one `providers:` block. None of those is
// what this engine runs on: the stream is NATS JetStream (embedded by
// default, so a company needs no external service at all), the store is one
// local file, and coordination is its own slot. Tier A therefore names the
// two SLOTS and the store directly rather than a provider bag — see
// [Stream], [Coordination] and [Store], and [Bootstrap.Validate] for the
// combinations that are refused.
type Bootstrap struct {
	// Node is this process's identity within the fleet.
	Node Node `yaml:"node,omitempty" json:"node"`

	// Store is the local database file this node materializes into.
	Store Store `yaml:"store,omitempty" json:"store"`

	// Stream is the durable event log — the source of truth every node
	// writes through.
	Stream Stream `yaml:"stream,omitempty" json:"stream"`

	// Coordination is where leases and shared counters live.
	Coordination Coordination `yaml:"coordination,omitempty" json:"coordination"`

	// API is the HTTP surface: the dashboard, the REST API, the webhooks.
	API API `yaml:"api,omitempty" json:"api"`

	// Secrets is the encryption keyring for company secrets at rest. It is
	// the root of trust and lives ONLY here — never in the store it opens.
	Secrets Secrets `yaml:"secrets,omitempty" json:"secrets"`

	// Debug turns on verbose logging.
	Debug bool `yaml:"debug,omitempty" json:"debug"`
}

// DefaultBootstrap is a Tier A config with every default applied: one node
// doing everything, an embedded in-process stream, local coordination, a
// store file beside the binary, and no API socket.
//
// Defaults live in a constructor rather than in per-field tags because the
// loader decodes INTO this value: a key absent from the file leaves the
// default in place, and a key present with no value under it (`api:`) reads
// as unset for the same reason. That is the shape a hand-written YAML uses
// for "empty", and it is what POST /config can produce.
func DefaultBootstrap() Bootstrap {
	return Bootstrap{
		Node:         Node{},
		Store:        Store{Path: DefaultStorePath},
		Stream:       Stream{Type: StreamEmbedded, Replicas: 1},
		Coordination: Coordination{Type: CoordinationLocal},
		API:          API{Host: DefaultAPIHost, Auth: APIAuth{AllowAnonymousRead: true}},
	}
}

// Validate reports every Tier A rule this config breaks, joined.
func (b *Bootstrap) Validate() error {
	var p problems
	p.wrap(b.Node.validate("node"))
	p.wrap(b.Store.validate("store"))
	p.wrap(b.Stream.validate("stream"))
	p.wrap(b.Coordination.validate("coordination"))
	p.wrap(b.API.validate("api"))
	p.wrap(b.Secrets.validate("secrets"))
	p.wrap(b.validateTopology())
	return p.err()
}

// validateTopology refuses slot combinations that cannot work.
//
// Both halves of a deployment are named in this one file, so the incoherent
// pairings are decidable here — and every one of them fails LATER as
// something that looks like a different problem entirely: a fleet on local
// coordination has each node claiming every seat, and a two-node embedded
// quorum wedges the moment either node restarts.
func (b *Bootstrap) validateTopology() error {
	var p problems

	peers := len(b.Stream.Cluster.Peers)
	clustered := peers > 0 || b.Stream.Cluster.Name != "" || b.Stream.Type != StreamEmbedded

	if b.Coordination.Type == CoordinationLocal && clustered {
		p.add("coordination.type", ErrConflict,
			"local coordination holds its leases in this process, so every "+
				"node in a fleet would claim every seat. A fleet needs "+
				"coordination.type %q", CoordinationEmbeddedKV)
	}

	// A fleet is one node or three; two is refused by name. Two embedded
	// KV members have no quorum without each other, so the fleet stops
	// serving the moment either restarts — and a rolling upgrade restarts
	// them one at a time, which makes the outage certain rather than
	// unlucky.
	if b.Coordination.Type == CoordinationEmbeddedKV {
		members := peers + 1
		if members == 2 {
			p.add("stream.cluster.peers", ErrConflict,
				"a two-node fleet has no coordination quorum: run one node "+
					"or three or more (this config names %d peer, so %d nodes)",
				peers, members)
		}
	}

	// Pulsar has no compare-and-set, so it can never fill the coordination
	// slot. A Pulsar estate still coordinates through the embedded KV
	// cluster; saying so here beats discovering it when two nodes run one
	// seat.
	if b.Stream.Type == StreamPulsar && b.Coordination.Type == CoordinationLocal {
		p.add("coordination.type", ErrConflict,
			"an external Pulsar stream cannot also carry coordination "+
				"(Pulsar has no compare-and-set): use %q", CoordinationEmbeddedKV)
	}

	if b.Stream.Replicas > 1 && peers == 0 {
		p.add("stream.replicas", ErrConflict,
			"replicas > 1 needs peers to replicate to; a solo node keeps 1")
	}
	return p.err()
}

// ---- node ------------------------------------------------------------ //

// DefaultNodeID is what a process with nothing configured calls itself: the
// single-process deployment, which is every company that has not scaled out.
const DefaultNodeID = "node-0"

// NodeIDEnvVar injects a node id without templating the config file, which
// is how a container orchestrator hands a pod its name.
const NodeIDEnvVar = "CREWLET_NODE_ID"

// nodeIDPattern is what a node id may look like. It ends up in log fields
// and in broker consumer names, so it is restricted to what both accept.
var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Node is Tier A identity of THIS process within the company.
//
// The id must be STABLE ACROSS RESTARTS, which is why it comes from the
// deployment rather than being generated: it is what per-node subscriptions,
// per-node config-apply status and the lease `preferred` hint are keyed on,
// and a fresh value per boot orphans everything the previous incarnation
// registered. In Kubernetes use the pod name (a StatefulSet ordinal is
// ideal); under systemd, the host name.
//
// A lease HOLDER is the opposite property and is not this — see
// [NewIncarnation].
type Node struct {
	// ID is this process's identity. Empty resolves via CREWLET_NODE_ID
	// and then DefaultNodeID, so nothing has to be set to run one engine.
	ID string `yaml:"id,omitempty" json:"id,omitempty" js:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" desc:"Stable identity of this process. Empty reads CREWLET_NODE_ID, then defaults to node-0."`

	// Roles is what this process is willing to do: ingress, seats,
	// workers. Omit the key to run every role — the single-process
	// default, and the shape every company starts as.
	//
	// Subtracting a role subtracts it from THIS node, never from the
	// company: a fleet with no workers node runs no scheduler and no
	// retention sweep, and one with no ingress node never hears a webhook.
	// Neither is visible in any single node's config, so the engine checks
	// it against live node presence at runtime.
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty" desc:"What this node does: ingress, seats, workers. Omit for all three."`

	// Labels are free-form facts about where this process runs (zone: eu,
	// gpu: "true"), matched exactly by a seat's role.placement selector.
	// Nothing here means anything to the engine on its own — the org
	// decides what to select on.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty" desc:"Free-form node facts a seat's placement selector matches on."`
}

// nodeRoleNames is the vocabulary, for the error message and the schema.
// It is derived from the placement package rather than restated, so the
// config layer and the seat host can never disagree about what a role is.
var nodeRoleNames = []string{
	string(placement.RoleIngress),
	string(placement.RoleSeats),
	string(placement.RoleWorkers),
}

func (n *Node) validate(path string) error {
	var p problems

	if n.ID != "" && !envref.Has(n.ID) && !nodeIDPattern.MatchString(n.ID) {
		// A reference is checked after resolution, in ResolveNodeID — the
		// YAML load path substitutes before this runs, so this branch only
		// catches a config built in code.
		p.add(at(path, "id"), ErrUnknownValue,
			"%q must start alphanumeric and contain only letters, digits, "+
				"'.', '_' or '-' (max 64 chars) — it lands in log fields and "+
				"broker consumer names", n.ID)
	}

	// An explicitly empty list is refused rather than read as "every
	// role". The placement package reads an empty set as every role
	// because that is the only safe reading of a PEER's presence row, but
	// an operator who wrote `roles: []` did not mean "do everything" —
	// they meant something they then failed to name.
	if n.Roles != nil && len(n.Roles) == 0 {
		p.add(at(path, "roles"), ErrMissing,
			"name at least one of %s — a node with no roles does nothing at "+
				"all. Omit the key to run every role, which is the "+
				"single-process default", strings.Join(nodeRoleNames, ", "))
	}
	if _, err := placement.ParseRoles(n.Roles); err != nil {
		p.add(at(path, "roles"), ErrUnknownValue, "%s (want %s)",
			err, strings.Join(nodeRoleNames, ", "))
	}

	for key := range n.Labels {
		if strings.TrimSpace(key) == "" {
			p.add(at(path, "labels"), ErrMissing, "label keys must not be empty")
		}
	}
	return p.err()
}

// RoleSet is what this node declared, in the form the seat host consumes.
// An unset list resolves to every role; see [placement.RoleSet].
func (n *Node) RoleSet() (placement.RoleSet, error) {
	return placement.ParseRoles(n.Roles)
}

// Profile is this node as its peers will see it on its presence lease.
// The id is passed in because it is RESOLVED (config, then environment,
// then the default) rather than read raw off the field.
func (n *Node) Profile(id string) placement.NodeProfile {
	roles, _ := placement.ParseRoles(n.Roles) // validated already
	labels := make(map[string]string, len(n.Labels))
	for k, v := range n.Labels {
		labels[k] = v
	}
	if len(labels) == 0 {
		labels = nil
	}
	return placement.NodeProfile{ID: id, Roles: roles, Labels: labels}
}

// ResolveNodeID answers what this process calls itself.
//
// Precedence: node.id in the file, then CREWLET_NODE_ID, then
// DefaultNodeID. The configured value is resolved through r like every
// other Tier A reference, so `node.id: "${HOSTNAME}"` works — and a
// resolved value that fails the pattern is rejected HERE rather than
// surfacing later as a malformed consumer name.
func ResolveNodeID(b *Bootstrap, r *Resolver) (string, error) {
	if r == nil {
		r = EnvOnly()
	}
	value := ""
	if b != nil && b.Node.ID != "" {
		value = strings.TrimSpace(r.Value(b.Node.ID))
	}
	if value == "" {
		value = strings.TrimSpace(r.Lookup(NodeIDEnvVar))
	}
	if value == "" {
		return DefaultNodeID, nil
	}
	if !nodeIDPattern.MatchString(value) {
		return "", fault("node.id", ErrUnknownValue,
			"%q must start alphanumeric and contain only letters, digits, "+
				"'.', '_' or '-' (max 64 chars)", value)
	}
	return value, nil
}

// NewIncarnation mints a holder identity for one running process:
// "{nodeID}:{random}".
//
// The counterpart to [ResolveNodeID] and deliberately the opposite
// property. A node id is stable across restarts because it names a
// PLACEMENT — a pod, a host — and what registers under it must survive a
// restart. A lease holder needs the reverse: it names one INCARNATION, so a
// replacement process is never mistaken for its predecessor. Conflating
// them is a real hole: a lease is renewable by its own owner string, so a
// restarted pod reusing a still-draining predecessor's identity would have
// both holding the seat at one fencing epoch — and with the default id
// being the shared constant node-0, so would any two engines started
// against one store.
//
// The Python engine cached this in a process global and needed a second
// function to bypass the cache, because two engines in one process are two
// holders and handing them one identity recreated exactly that hole. There
// is no cache here: each holder calls this once and keeps what it got,
// which is the property the cache was emulating. Minting a second one
// mid-run fences an engine out of its own seats.
func NewIncarnation(nodeID string) string {
	return nodeID + ":" + uuid.NewString()[:8]
}

// ---- store ----------------------------------------------------------- //

// DefaultStorePath is where a company with nothing configured keeps its
// database: one file, relative to the working directory, so `crewlet run`
// in an empty directory works.
const DefaultStorePath = "crewlet.db"

// StoreDriver selects the SQLite implementation behind the store.
type StoreDriver string

const (
	// StoreDriverTurso is the default: vector search and full-text search
	// are native, so recall does not fall back to a brute-force scan.
	StoreDriverTurso StoreDriver = "turso"
	// StoreDriverSQLite is mainline SQLite — the certified fallback, and
	// what the dual-driver CI job proves still works.
	StoreDriverSQLite StoreDriver = "sqlite"
)

// StoreDrivers is the closed set, shared by the validator and the schema.
var StoreDrivers = []StoreDriver{StoreDriverTurso, StoreDriverSQLite}

// Store is the local database this node materializes the stream into.
//
// It is a FILE, owned exclusively by this process for the life of the
// process. It is not a shared database and there is no DSN: two engines
// pointed at one file corrupt it, and a fleet's nodes each keep their own
// rebuildable copy (see the plan's D8 — sync truth, async cache).
type Store struct {
	// Path is the database file. Created if absent, along with its parent.
	Path string `yaml:"path,omitempty" json:"path,omitempty" desc:"Local database file this node owns exclusively."`

	// Driver selects the SQLite implementation. Empty consults
	// CREWLET_STORE_DRIVER and then defaults to turso. A mistyped name is
	// an error rather than a fallback: silently opening a different
	// storage engine is a data-loss shape, not a cosmetic one.
	Driver StoreDriver `yaml:"driver,omitempty" json:"driver,omitempty" js:"enum=turso|sqlite" desc:"turso (default) or sqlite."`

	// MaxOpenConns bounds the connection pool; 0 takes the store's own
	// default, which is sized to the dashboard's query concurrency.
	MaxOpenConns int `yaml:"max_open_conns,omitempty" json:"max_open_conns,omitempty" js:"min=0" desc:"Connection pool bound; 0 takes the store default."`

	// BusyTimeoutSeconds is how long a statement waits for the file lock
	// before giving up; 0 takes the store's own default.
	BusyTimeoutSeconds float64 `yaml:"busy_timeout_seconds,omitempty" json:"busy_timeout_seconds,omitempty" js:"min=0" desc:"Lock wait before a statement fails; 0 takes the store default."`
}

func (s *Store) validate(path string) error {
	var p problems
	if strings.TrimSpace(s.Path) == "" {
		p.add(at(path, "path"), ErrMissing,
			"the store is a local file this node owns; name one (e.g. %q)",
			DefaultStorePath)
	}
	if s.Driver != "" && !oneOf(s.Driver, StoreDrivers) {
		p.add(at(path, "driver"), ErrUnknownValue, "%q (want %s)",
			s.Driver, names(StoreDrivers))
	}
	if s.MaxOpenConns < 0 {
		p.add(at(path, "max_open_conns"), ErrOutOfRange,
			"must be 0 (the store default) or positive, got %d", s.MaxOpenConns)
	}
	if s.BusyTimeoutSeconds < 0 {
		p.add(at(path, "busy_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the store default) or positive, got %v", s.BusyTimeoutSeconds)
	}
	return p.err()
}

// BusyTimeout is the lock wait as a duration; zero means the store's own
// default.
func (s *Store) BusyTimeout() time.Duration {
	return time.Duration(s.BusyTimeoutSeconds * float64(time.Second))
}

// ---- stream ---------------------------------------------------------- //

// StreamType is the stream slot: where the durable event log lives.
type StreamType string

const (
	// StreamEmbedded runs a NATS JetStream server inside this process. The
	// single-binary topology: no listener, no port, no service to operate
	// — and in the solo case no socket at all, so the broker cannot be
	// reached from outside the process.
	StreamEmbedded StreamType = "embedded"
	// StreamNATS dials an external NATS cluster. The same client code
	// either way; the difference is where the server runs.
	StreamNATS StreamType = "nats"
	// StreamPulsar dials an external Pulsar estate — the multi-tenant
	// deployment, one tenant per company. Pulsar never fills the
	// coordination slot.
	StreamPulsar StreamType = "pulsar"
)

// StreamTypes is the closed set.
var StreamTypes = []StreamType{StreamEmbedded, StreamNATS, StreamPulsar}

// Stream is the durable event log every node writes through.
type Stream struct {
	// Type selects the slot. Default embedded — a company with nothing
	// configured runs with no external services at all.
	Type StreamType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=embedded|nats|pulsar" desc:"embedded (default), nats, or pulsar."`

	// URL is the external server to dial. Required for nats and pulsar,
	// and refused for embedded — a URL on an embedded stream is read by
	// nobody, which is the classic "I configured it and nothing happened".
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"External broker URL. Required for nats/pulsar, refused for embedded."`

	// StoreDir is where an EMBEDDED server persists its streams. Empty
	// selects an in-memory server, which is what a test wants and what a
	// stateless ingress-only node can use — and what a company that
	// expects to survive a restart must NOT leave unset.
	StoreDir string `yaml:"store_dir,omitempty" json:"store_dir,omitempty" desc:"Embedded stream persistence directory. Empty = in-memory (nothing survives a restart)."`

	// Cluster makes the embedded server join peers, which is the fleet
	// topology: every node embeds a member of one cluster.
	Cluster StreamCluster `yaml:"cluster,omitempty" json:"cluster,omitzero"`

	// Replicas is the stream replica count: 1 solo, 3 in a fleet, where it
	// is what makes a publish quorum-durable before it returns.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty" js:"min=0" desc:"Stream replica count: 1 solo, 3 in a fleet."`

	// EventRetentionHours bounds the event stream. 0 takes the queue's own
	// default. Unbounded is deliberately not expressible: the Python
	// engine's event table grew for the life of the deployment because
	// nothing ever swept it.
	EventRetentionHours float64 `yaml:"event_retention_hours,omitempty" json:"event_retention_hours,omitempty" js:"min=0" desc:"Event stream retention; 0 takes the queue default."`

	// Credentials is a path to a NATS credentials file.
	Credentials string `yaml:"credentials,omitempty" json:"credentials,omitempty" desc:"Path to a NATS credentials file for an external server."`

	// Token is a bearer token presented to an external server. Use ${VAR}
	// to read it from the environment.
	Token string `yaml:"token,omitempty" json:"token,omitempty" desc:"Bearer token for an external server; ${VAR} supported."`

	// Tenant and Namespace scope every subject on a PULSAR estate, which
	// is how one cluster serves many companies. Neither is ever
	// auto-created — a non-default tenant must exist before start.
	Tenant    string `yaml:"tenant,omitempty" json:"tenant,omitempty" js:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$" desc:"Pulsar tenant holding this company's topics."`
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty" js:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$" desc:"Pulsar namespace under the tenant."`
}

// StreamCluster is an embedded server's membership in a cluster.
type StreamCluster struct {
	// Name is the cluster every member shares.
	Name string `yaml:"name,omitempty" json:"name,omitempty" desc:"Cluster name shared by every member."`
	// Port is the route port this member listens on.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Route port for cluster traffic."`
	// Peers are the other members' route URLs.
	Peers []string `yaml:"peers,omitempty" json:"peers,omitempty" desc:"Route URLs of the other members."`
}

// IsZero lets an unset cluster block drop out of a JSON round trip.
func (c StreamCluster) IsZero() bool {
	return c.Name == "" && c.Port == 0 && len(c.Peers) == 0
}

var pulsarNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s *Stream) validate(path string) error {
	var p problems
	if s.Type != "" && !oneOf(s.Type, StreamTypes) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", s.Type, names(StreamTypes))
		return p.err() // the rest of the rules key on the type
	}
	external := s.Type == StreamNATS || s.Type == StreamPulsar
	switch {
	case external && strings.TrimSpace(s.URL) == "":
		p.add(at(path, "url"), ErrMissing, "an external %q stream needs a URL to dial", s.Type)
	case !external && s.URL != "":
		p.add(at(path, "url"), ErrConflict,
			"url only applies to an external stream; an embedded server has "+
				"no address. Remove it, or set type to %s", names([]StreamType{StreamNATS, StreamPulsar}))
	}
	if external && s.StoreDir != "" {
		p.add(at(path, "store_dir"), ErrConflict,
			"store_dir is where an EMBEDDED server persists; an external "+
				"cluster keeps its own storage")
	}
	if s.Replicas < 0 {
		p.add(at(path, "replicas"), ErrOutOfRange, "must not be negative, got %d", s.Replicas)
	}
	if s.EventRetentionHours < 0 {
		p.add(at(path, "event_retention_hours"), ErrOutOfRange,
			"must be 0 (the queue default) or positive, got %v", s.EventRetentionHours)
	}
	if s.Cluster.Port < 0 || s.Cluster.Port > 65535 {
		p.add(at(path, "cluster.port"), ErrOutOfRange, "must be 0..65535, got %d", s.Cluster.Port)
	}
	for _, field := range []struct {
		name  string
		value string
	}{{"tenant", s.Tenant}, {"namespace", s.Namespace}} {
		if field.value == "" {
			continue
		}
		if s.Type != StreamPulsar {
			p.add(at(path, field.name), ErrConflict,
				"%s scopes a Pulsar estate's subjects; this stream is type %q",
				field.name, s.Type)
			continue
		}
		if !pulsarNamePattern.MatchString(field.value) {
			p.add(at(path, field.name), ErrUnknownValue,
				"%q must start alphanumeric and contain only letters, digits, "+
					"'.', '_' or '-'", field.value)
		}
	}
	return p.err()
}

// EventRetention is the retention window as a duration; zero means the
// queue's own default.
func (s *Stream) EventRetention() time.Duration {
	return time.Duration(s.EventRetentionHours * float64(time.Hour))
}

// ---- coordination ---------------------------------------------------- //

// CoordinationType is the coordination slot: where leases, the shared
// counters and the ledgers live.
type CoordinationType string

const (
	// CoordinationLocal keeps coordination inside this process. Correct
	// for exactly one node, and catastrophic for more than one: every node
	// would hold every lease and run every seat.
	CoordinationLocal CoordinationType = "local"
	// CoordinationEmbeddedKV is the replicated KV a fleet coordinates
	// through. It needs a quorum, which is why two-node fleets are refused.
	CoordinationEmbeddedKV CoordinationType = "embedded-kv"
)

// CoordinationTypes is the closed set.
var CoordinationTypes = []CoordinationType{CoordinationLocal, CoordinationEmbeddedKV}

// Coordination is the coordination slot.
type Coordination struct {
	// Type selects the slot. Default local — one node, no quorum, no
	// network.
	Type CoordinationType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=local|embedded-kv" desc:"local (single node, default) or embedded-kv (a fleet)."`

	// LeaseTTLSeconds overrides how long a lease survives without a renew.
	// 0 takes the coordination layer's own measured default; shortening it
	// speeds up failover at the cost of shedding seats over a store blip.
	LeaseTTLSeconds float64 `yaml:"lease_ttl_seconds,omitempty" json:"lease_ttl_seconds,omitempty" js:"min=0" desc:"Lease TTL; 0 takes the measured default."`
}

func (c *Coordination) validate(path string) error {
	var p problems
	if c.Type != "" && !oneOf(c.Type, CoordinationTypes) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", c.Type, names(CoordinationTypes))
	}
	if c.LeaseTTLSeconds < 0 {
		p.add(at(path, "lease_ttl_seconds"), ErrOutOfRange,
			"must be 0 (the default) or positive, got %v", c.LeaseTTLSeconds)
	}
	return p.err()
}

// LeaseTTL is the override as a duration; zero means the measured default.
func (c *Coordination) LeaseTTL() time.Duration {
	return time.Duration(c.LeaseTTLSeconds * float64(time.Second))
}

// ---- api ------------------------------------------------------------- //

// DefaultAPIHost binds every interface, which is what a container needs.
const DefaultAPIHost = "0.0.0.0"

// API is the Tier A HTTP surface: host, port, auth posture.
type API struct {
	// Host is the bind address.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the HTTP surface."`

	// Port is the bind port. 0 serves no HTTP at all — no dashboard, no
	// REST API, and no webhook endpoint, so every integration goes deaf.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port; 0 disables the HTTP surface entirely."`

	Auth APIAuth `yaml:"auth,omitempty" json:"auth"`
}

func (a *API) validate(path string) error {
	var p problems
	// Python never bounded this, so a port of 70000 passed validation and
	// failed at bind, long after `crewlet validate` said the config was
	// good.
	if a.Port < 0 || a.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange,
			"must be 0 (no HTTP surface) or a port 1..65535, got %d", a.Port)
	}
	p.wrap(a.Auth.validate(at(path, "auth")))
	return p.err()
}

// APIAuth is the bearer-token policy for the HTTP surface.
//
// Writes and the whole /config surface always require a token. Reads are
// governed by AllowAnonymousRead, which defaults OPEN — reading is what a
// dashboard does, and the page that would prompt for a token is itself
// served unauthenticated, so requiring one by default puts a modal in front
// of every first load.
//
// Exempt from auth entirely, because they authenticate by other means or
// must be reachable to obtain a token at all: /health, /ready, the
// dashboard shell and its assets, /webhooks/* (HMAC-verified per source)
// and /otlp/* (signed per-run token).
type APIAuth struct {
	// Tokens are the accepted bearer tokens. An empty list is a real
	// posture, not an oversight: no token can match, so reads serve and
	// every write and all of /config is refused.
	Tokens []APIToken `yaml:"tokens,omitempty" json:"tokens,omitempty" desc:"Accepted bearer tokens. Empty refuses every write."`

	// Disabled serves every route without auth and logs a loud startup
	// warning. A local-development escape hatch, never a production one.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty" desc:"Local-dev only: serve every route unauthenticated."`

	// AllowAnonymousRead governs GET/HEAD outside /config. Default true.
	// It is a real exposure — the read surface carries LLM transcripts,
	// diary entries and the whole event stream — so the API states which
	// posture it took at startup, at WARNING when the bind host is not
	// loopback.
	AllowAnonymousRead bool `yaml:"allow_anonymous_read,omitempty" json:"allow_anonymous_read" desc:"Serve reads without a token (default true)."`

	// AllowedOrigins are the browser origins CORS permits. Empty means
	// SAME-ORIGIN ONLY: the dashboard is served by this process, so it
	// needs no entry. The previous default was "*", which let any site a
	// logged-in operator visited read every unauthenticated endpoint.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty" desc:"CORS origins. Empty = same-origin only."`
}

// APIToken is one bearer token gating writes and /config.
type APIToken struct {
	// ID is a short label stamped into revision audit rows (created_by):
	// "founder", "ops", "ci-pipeline".
	ID string `yaml:"id" json:"id" js:"required" desc:"Short label recorded as the author of writes made with this token."`

	// Token is the value, or a ${VAR} reference to it. Resolved once at
	// startup and never stored.
	Token string `yaml:"token" json:"token" js:"required" desc:"Token value or ${VAR} reference."`
}

func (a *APIAuth) validate(path string) error {
	var p problems
	seen := make(map[string]struct{}, len(a.Tokens))
	for i, t := range a.Tokens {
		tp := idx(at(path, "tokens"), i)
		if strings.TrimSpace(t.ID) == "" {
			p.add(at(tp, "id"), ErrMissing,
				"every token needs a label — it is what a revision's audit row records")
		}
		if strings.TrimSpace(t.Token) == "" {
			p.add(at(tp, "token"), ErrMissing, "token must not be empty")
		}
		if _, dup := seen[t.ID]; dup && t.ID != "" {
			// Two tokens sharing a label make the audit trail unreadable:
			// every write says "founder" and no one can tell which
			// credential made it, which is the whole reason the label
			// exists.
			p.add(at(tp, "id"), ErrConflict, "duplicate token id %q", t.ID)
		}
		seen[t.ID] = struct{}{}
	}
	return p.err()
}

// ---- secrets --------------------------------------------------------- //

// secretKeyIDPattern keeps a key id colon-free so the envelope
// (enc:v1:<id>:...) parses.
var secretKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Secrets is the Tier A encryption keyring for company secrets at rest.
//
// The keyring is the SOLE ROOT OF TRUST: the store holds only ciphertext,
// and the key material lives here — on disk or in the environment, never in
// the database it opens. Empty (the default) means secret encryption is
// disabled; the engine then fails closed only if the active revision
// actually contains sealed values.
type Secrets struct {
	// ActiveKeyID names the key that seals new writes. Required once keys
	// is non-empty.
	ActiveKeyID string `yaml:"active_key_id,omitempty" json:"active_key_id,omitempty" desc:"Which key seals new writes."`

	// Keys is the keyring. More than one entry supports online rotation:
	// the active key seals, every key decrypts.
	Keys []SecretKey `yaml:"keys,omitempty" json:"keys,omitempty" desc:"The keyring; several entries support online rotation."`
}

// SecretKey is one key in the keyring.
type SecretKey struct {
	// ID is stamped into every envelope this key seals (enc:v1:<id>:...),
	// and is kept across a rotation so old ciphertext still names a live
	// key.
	ID string `yaml:"id" json:"id" js:"required;pattern=^[A-Za-z0-9._-]+$" desc:"Stable key id, colon-free so the envelope parses."`

	// Material is a base64-encoded 32-byte key, or a ${VAR} reference to
	// one.
	Material string `yaml:"material" json:"material" js:"required" desc:"base64(32 bytes), or a ${VAR} reference to it."`
}

func (s *Secrets) validate(path string) error {
	var p problems
	ids := make(map[string]struct{}, len(s.Keys))
	for i, k := range s.Keys {
		kp := idx(at(path, "keys"), i)
		if strings.TrimSpace(k.ID) == "" {
			p.add(at(kp, "id"), ErrMissing, "every key needs an id")
		} else if !secretKeyIDPattern.MatchString(k.ID) {
			p.add(at(kp, "id"), ErrUnknownValue,
				"%q must contain only letters, digits, '.', '_' or '-' — it is "+
					"stamped into every envelope this key seals", k.ID)
		}
		if strings.TrimSpace(k.Material) == "" {
			p.add(at(kp, "material"), ErrMissing,
				"key material must not be empty (generate one with `crewlet secrets keygen`)")
		}
		if _, dup := ids[k.ID]; dup {
			p.add(at(kp, "id"), ErrConflict, "duplicate key id %q", k.ID)
		}
		ids[k.ID] = struct{}{}
	}
	if len(s.Keys) > 0 && s.ActiveKeyID == "" {
		p.add(at(path, "active_key_id"), ErrMissing,
			"required once keys is set — one key has to seal new writes")
	}
	if s.ActiveKeyID != "" {
		if _, ok := ids[s.ActiveKeyID]; !ok {
			p.add(at(path, "active_key_id"), ErrUnknownValue,
				"%q matches no configured key id", s.ActiveKeyID)
		}
	}
	return p.err()
}

// Enabled reports whether secret encryption is configured at all.
func (s *Secrets) Enabled() bool { return len(s.Keys) > 0 }

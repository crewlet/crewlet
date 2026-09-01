package config

import (
	"encoding/base64"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Bootstrap is Tier A — ops-owned, on disk, restart-only.
//
// It carries only what the engine needs to bring its own infrastructure up.
// Everything about the company lives in [Company], versioned in the store
// and delivered to a running engine.
//
// A Tier A naming a broker, a database DSN and a vector store under one
// `providers:` block is the obvious shape, and none of it is what this engine
// runs on: the stream is NATS JetStream (embedded by
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

	// Logging is how loud this node is, and in what shape.
	Logging Logging `yaml:"logging,omitempty" json:"logging"`
}

// Logging is Tier A's logging surface: the level this node emits at and the
// shape it writes.
//
// # Why the colour is NOT here
//
// It is $CREWLET_LOG_COLOR and $NO_COLOR instead, because colour is a
// property of the TERMINAL SOMEONE IS LOOKING AT rather than of the
// deployment. The same file is applied to a container with no terminal and
// run on a laptop with one, and a field that had to be edited between those
// two would be describing the reader rather than the node. Colour is the
// only one of these knobs with NO file form: `node.roles`, `api.host` and
// `logging.level` are all Tier A fields that `-roles`, `-api-host` and
// $CREWLET_LOG_LEVEL merely override for one invocation.
// The console format works this out from its sink; see internal/logging.
type Logging struct {
	// Level is how loud this node is. Empty is info.
	//
	// A VALUE THIS BUILD DOES NOT KNOW IS REFUSED, unlike the `-log-level`
	// flag which resolves a typo to info. The two differ on purpose: a
	// flag is typed once by a person who is watching the process start,
	// and a file is written once and deployed for months. `debug: true`
	// that did nothing is exactly how this subsystem was found broken.
	Level logging.Level `yaml:"level,omitempty" json:"level,omitempty" js:"enum=debug|info|warn|error" desc:"debug, info (default), warn or error."`

	// Format is the shape of a line. Empty is console.
	Format logging.Format `yaml:"format,omitempty" json:"format,omitempty" js:"enum=console|text|json" desc:"console (default, columns and colour for a person), text (slog key=value) or json (for a log shipper)."`
}

func (l *Logging) validate(path string) error {
	var p problems
	if l.Level != "" && !l.Level.Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q (want %s)",
			l.Level, names(logging.Levels))
	}
	if l.Format != "" && !l.Format.Valid() {
		p.add(at(path, "format"), ErrUnknownValue, "%q (want %s)",
			l.Format, names(logging.Formats))
	}
	return p.err()
}

// LogSettings is what this file asks the process to log at, and in what
// shape, with every default applied.
//
// ONE WAY TO SAY IT. There was a `debug: true` boolean beside this block —
// retired rather than wired up, because two keys setting one value is a
// state where they disagree and something has to arbitrate. `logging.level`
// says everything it said and three things it could not. The CLI's flags are
// layered on top of this by `crewlet run`, and only when actually given.
func (b *Bootstrap) LogSettings() (slog.Level, logging.Format) {
	level := slog.LevelInfo
	if b.Logging.Level != "" {
		level = b.Logging.Level.Slog()
	}
	format := logging.FormatConsole
	if b.Logging.Format != "" {
		format = b.Logging.Format
	}
	return level, format
}

// DefaultBootstrap is a Tier A config with every default applied: one node
// doing everything, an embedded in-process stream, local coordination, a
// store file beside the binary, and no API socket.
//
// Defaults live in a constructor rather than in per-field tags because the
// loader decodes INTO this value: a key absent from the file leaves the
// default in place, and a key present with no value under it (`api:`) reads
// as unset for the same reason. That is the shape a hand-written YAML uses
// for "empty", and it is what PUT /config can produce.
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
	p.wrap(b.Logging.validate("logging"))
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
	//
	// Counted over the STREAM's members, because that is where the leases
	// live: the coordination store rides the stream's own connection on
	// every topology, so the KV's quorum is the stream cluster's quorum.
	if b.Coordination.Type == CoordinationEmbeddedKV {
		if members := peers + 1; members == 2 {
			p.add("stream.cluster.peers", ErrConflict,
				"a two-node fleet has no coordination quorum: run one node "+
					"or three or more (this config names %d peer, so %d nodes)",
				peers, members)
		}
	}

	// Only an EMBEDDED stream is refused for this. Its members are the
	// peers named right here, so a replica count above their number is a
	// statement this file contradicts on its own.
	//
	// An external cluster's membership is not in this file and cannot be:
	// `stream.url` names an address, and how many servers answer behind it
	// is the operator's business. Refusing replicas there capped every
	// external-NATS fleet at one copy of everything — not only the streams
	// but the lease and fleet KV buckets, which take the same number (see
	// engine.attachCoordination) — so a deployment that ran three brokers
	// for availability kept its seat mailboxes and every lease on whichever
	// single server happened to hold them, and lost them with it.
	if b.Stream.Type != StreamNATS && b.Stream.Replicas > 1 && peers == 0 {
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

	// MaxConcurrent bounds how many agent turns this PROCESS runs at
	// once. Zero — the shape an absent key takes — means the engine's
	// own default, node.DefaultMaxConcurrent, which is where the number
	// and its rationale live: the layer that enforces a limit is the one
	// that gets to say what it is when nobody said. Same arrangement as
	// coordination.lease_ttl_seconds and seat.SeatLeaseTTL.
	//
	// Per node, deliberately, which is why it is Tier A: a fleet's ceiling
	// is N × this value, so it is sized against the host a process runs on
	// rather than against the company. It is the one knob a fleet genuinely
	// changes the meaning of.
	//
	// There is no "unbounded". Zero is unset rather than a setting — a cap
	// of zero turns is not a thing an operator can want — and somebody who
	// wants effectively no bound writes a large number they can see.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0" desc:"Agent turns this process runs at once; 0 takes the engine default. Per node, so a fleet's ceiling is N times this."`
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

	// Zero is unset and takes the engine's default. A NEGATIVE is refused
	// rather than treated the same way: it is a value somebody typed, and
	// silently running the default after being told -1 hides a config the
	// operator believes is in effect.
	if n.MaxConcurrent < 0 {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (the engine default) or a positive number of turns, "+
				"got %d — there is no \"unbounded\"; write a large number for "+
				"effectively no limit", n.MaxConcurrent)
	}

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
// Caching this in a process global needs a second function to bypass the
// cache, because two engines in one process are two holders and handing them
// one identity recreates exactly that hole. There is no cache here: each holder calls this once and keeps what it got,
// which is the property the cache was emulating. Minting a second one
// mid-run fences an engine out of its own seats.
// The uuid is carried WHOLE. It was cut to eight hex characters, which is 32
// bits — a birthday collision at a few tens of thousands of incarnations, on
// the one string the paragraph above spends nineteen lines explaining must
// never be confused between two of them. Nothing wants it short: it is
// compared for equality by the lease renewal and never rendered in a
// width-constrained slot.
func NewIncarnation(nodeID string) string {
	return nodeID + ":" + uuid.NewString()
}

// ---- store ----------------------------------------------------------- //

// DefaultStorePath is where a company with nothing configured keeps its
// database: one file, relative to the working directory, so `crewlet run`
// in an empty directory works.
const DefaultStorePath = "crewlet.db"

// Store is the local database this node materializes the stream into.
//
// It is a FILE, owned exclusively by this process for the life of the
// process. It is not a shared database and there is no DSN: two engines
// pointed at one file corrupt it, and a fleet's nodes each keep their own
// rebuildable copy: the store is the synchronous truth and the index an
// asynchronous cache of it.
//
// # There is no driver field
//
// There was one — `driver: turso | sqlite` — and it is retired. Turso is the
// database and the only driver, so the field selected between two
// implementations of which one exists. A file that still
// carries it is answered by name rather than as a misspelling; see
// retiredBootstrapFields in load.go.
type Store struct {
	// Path is the database file. Created if absent, along with its parent.
	Path string `yaml:"path,omitempty" json:"path,omitempty" desc:"Local database file this node owns exclusively."`

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
)

// StreamTypes is the closed set.
var StreamTypes = []StreamType{StreamEmbedded, StreamNATS}

// Stream is the durable event log every node writes through.
type Stream struct {
	// Type selects the slot. Default embedded — a company with nothing
	// configured runs with no external services at all.
	Type StreamType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=embedded|nats" desc:"embedded (default) or nats."`

	// URL is the external server to dial. Required for nats, and refused
	// for embedded — a URL on an embedded stream is read by nobody, which
	// is the classic "I configured it and nothing happened".
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"External NATS URL. Required for nats, refused for embedded."`

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
	// default. Unbounded is deliberately not expressible: an event table
	// nothing ever sweeps grows for the life of the deployment.
	EventRetentionHours float64 `yaml:"event_retention_hours,omitempty" json:"event_retention_hours,omitempty" js:"min=0" desc:"Event stream retention; 0 takes the queue default."`

	// Credentials is a path to a NATS credentials file.
	Credentials string `yaml:"credentials,omitempty" json:"credentials,omitempty" desc:"Path to a NATS credentials file for an external server."`

	// Token is a bearer token presented to an external server. Use ${VAR}
	// to read it from the environment.
	Token string `yaml:"token,omitempty" json:"token,omitempty" desc:"Bearer token for an external server; ${VAR} supported."`

	// TLS is the transport for an external NATS server: a private CA to
	// trust, and a client certificate to present. Without it a broker
	// configured the way a hardened NATS deployment is configured —
	// `tls { verify: true }`, which REQUIRES a client certificate — is
	// simply unreachable.
	TLS NATSTLS `yaml:"tls,omitempty" json:"tls,omitzero"`
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

func (s *Stream) validate(path string) error {
	var p problems
	if s.Type != "" && !oneOf(s.Type, StreamTypes) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", s.Type, names(StreamTypes))
		return p.err() // the rest of the rules key on the type
	}
	external := s.Type == StreamNATS
	switch {
	case external && strings.TrimSpace(s.URL) == "":
		p.add(at(path, "url"), ErrMissing, "an external %q stream needs a URL to dial", s.Type)
	case !external && s.URL != "":
		p.add(at(path, "url"), ErrConflict,
			"url only applies to an external stream; an embedded server has "+
				"no address. Remove it, or set type to %q", StreamNATS)
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
	p.wrap(s.TLS.validate(at(path, "tls")))
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

// NATSTLS is the transport material for an external NATS server.
//
// # Why it exists beside `credentials` and `token`
//
// Those authenticate the CLIENT to the server at the NATS protocol layer.
// This is the TCP layer underneath: which CA to trust for the server's own
// certificate, and which certificate to present when the server demands one.
// A NATS deployment configured with `tls { verify: true }` — the hardened
// default every operator guide recommends — rejects a connection that
// presents no client certificate, whatever credentials follow.
//
// # It cannot express "do not verify"
//
// Deliberately, and it is the one option a struct like this normally grows.
// A skip-verify switch is set once during a bring-up and never unset, and the
// connection it leaves behind carries every one of this company's events and
// its coordination traffic to whoever answers on that address. A private CA
// is one file and is the actual answer.
type NATSTLS struct {
	// CA is a PEM bundle to verify the server's certificate against.
	// Empty uses the host's root pool, which is right for a public CA and
	// wrong for the self-signed certificate most internal NATS estates
	// use.
	CA string `yaml:"ca,omitempty" json:"ca,omitempty" desc:"PEM CA bundle for the server certificate; empty uses the host roots."`

	// Cert and Key are the CLIENT certificate, for a server that requires
	// mutual TLS. Both or neither: half a keypair is a config that dials
	// and is refused by the broker with an error naming neither file.
	Cert string `yaml:"cert,omitempty" json:"cert,omitempty" desc:"Client certificate PEM, for a server requiring mutual TLS. Needs key."`
	Key  string `yaml:"key,omitempty" json:"key,omitempty" desc:"Client private key PEM. Needs cert."`
}

// IsZero lets an unset TLS block drop out of a JSON round trip.
func (t NATSTLS) IsZero() bool { return t.CA == "" && t.Cert == "" && t.Key == "" }

// validate refuses half a keypair.
func (t NATSTLS) validate(path string) error {
	var p problems
	switch {
	case t.Cert != "" && t.Key == "":
		p.add(at(path, "key"), ErrMissing,
			"cert is set, so key must be too: a client certificate with no "+
				"private key cannot be presented")
	case t.Key != "" && t.Cert == "":
		p.add(at(path, "cert"), ErrMissing,
			"key is set, so cert must be too: a private key with no "+
				"certificate cannot be presented")
	}
	return p.err()
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
	// Unbounded, a port of 70000 passes validation and fails at bind,
	// long after `crewlet validate` said the config was good.
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

	// "anonymous" is the attribution recorded when auth.disabled is true.
	// A real token carrying it would collide in an audit row with the
	// writes made while the guard was off — the one distinction those
	// rows exist to keep.
	if _, reserved := seen[ReservedOperatorID]; reserved {
		p.add(at(path, "tokens"), ErrConflict,
			"token id %q is reserved: it is the attribution recorded when "+
				"api.auth.disabled is true. Pick a different id",
			ReservedOperatorID)
	}

	// The pairing that leaves nothing reachable. No tokens means no
	// candidate can ever match, and with reads closed too every route is
	// guarded by a credential that does not exist — a process that starts
	// cleanly, binds its port, and answers 401 to everything including
	// its own dashboard.
	//
	// Checked HERE rather than at API startup, so `crewlet validate`
	// catches it on a laptop rather than a deployment catching it at
	// bind time.
	if len(a.Tokens) == 0 && !a.AllowAnonymousRead {
		p.add(at(path, "tokens"), ErrMissing,
			"allow_anonymous_read is false and no tokens are configured, so "+
				"every route is guarded by a token that does not exist and "+
				"nothing is reachable. Configure at least one token, or leave "+
				"allow_anonymous_read at its default to serve reads without one")
	}
	return p.err()
}

// ReservedOperatorID is the attribution stamped on writes made while the auth
// guard is disabled.
//
// Exported because two packages need the same answer: config refuses it as a
// token id, and the API stamps it on a disabled-mode request. A second copy of
// the string is how those two would come to disagree about which id is
// reserved — and the disagreement would be silent, because each side would
// still be self-consistent.
const ReservedOperatorID = "anonymous"

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

// Cipher builds the sealing cipher this Tier A configures, or nil when secret
// encryption is disabled.
//
// NIL IS A POSTURE, not a failure: a deployment with no keyring stores its
// company config in plaintext, which is the documented opt-out and the state
// every deployment starts in. What must fail is a keyring that is configured
// and unusable — key material that is not 32 bytes of base64 is an operator
// error, and booting past it would seal the next revision under a key nobody
// can reproduce.
//
// ${VAR} references in the material are already resolved: Tier A expands its
// document before decoding, because the values it carries are needed the
// instant the process starts.
func (s *Secrets) Cipher() (secrets.Cipher, error) {
	if !s.Enabled() {
		return nil, nil
	}
	ring := secrets.Keyring{
		ActiveID: s.ActiveKeyID,
		Keys:     make(map[string][]byte, len(s.Keys)),
	}
	for _, key := range s.Keys {
		material, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key.Material))
		if err != nil {
			// The ID reaches the message and the material never does.
			return nil, fault(at("secrets.keys", key.ID), ErrShape,
				"key material must be base64 (generate one with `crewlet secrets keygen`)")
		}
		ring.Keys[key.ID] = material
	}
	return secrets.NewCipher(ring)
}

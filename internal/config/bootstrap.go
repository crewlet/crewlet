package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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

	// Retention is the operator's own half of keeping this deployment's
	// history: who owns its backups. The trim's terms live under
	// `stream.tracker_retention`, beside the log they bound.
	Retention Retention `yaml:"retention,omitempty" json:"retention,omitzero"`
}

// Retention is what the operator owns about this deployment's history.
type Retention struct {
	// BackupOwner is who owns the backup: a person, a team, a scheduler's
	// name. Free text, because it is read by a human at the moment an
	// alarm names it and by nothing else.
	//
	// UNSET IS WARNED ABOUT rather than refused. A company that never
	// backs up never trims — the log is the only copy of what no node has
	// applied yet — so "who is responsible for this" is a question with a
	// real answer on every deployment that intends to keep working, and
	// nowhere to put it is how it goes unasked.
	BackupOwner string `yaml:"backup_owner,omitempty" json:"backup_owner,omitempty" desc:"Who owns this deployment's backups — a person, a team, a scheduler. Warned about when unset."`
}

// IsZero lets an unset retention block drop out of a JSON round trip.
func (r Retention) IsZero() bool { return strings.TrimSpace(r.BackupOwner) == "" }

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
				"got %d: there is no \"unbounded\"; write a large number for "+
				"effectively no limit", n.MaxConcurrent)
	}

	if n.ID != "" && !envref.Has(n.ID) && !nodeIDPattern.MatchString(n.ID) {
		// A reference is checked after resolution, in ResolveNodeID — the
		// YAML load path substitutes before this runs, so this branch only
		// catches a config built in code.
		p.add(at(path, "id"), ErrUnknownValue,
			"%q must start alphanumeric and contain only letters, digits, "+
				"'.', '_' or '-' (max 64 chars): it lands in log fields and "+
				"broker consumer names", n.ID)
	}

	// An explicitly empty list is refused rather than read as "every
	// role". The placement package reads an empty set as every role
	// because that is the only safe reading of a PEER's presence row, but
	// an operator who wrote `roles: []` did not mean "do everything" —
	// they meant something they then failed to name.
	if n.Roles != nil && len(n.Roles) == 0 {
		p.add(at(path, "roles"), ErrMissing,
			"name at least one of %s: a node with no roles does nothing at "+
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

	// SnapshotDir is where this node keeps its own snapshots of the
	// replicated estate — the file a peer joining the fleet copies instead
	// of replaying the whole log.
	//
	// DELIBERATELY NOT UNDER stream., because it is a disk fact rather
	// than a broker fact. Absolute, or relative to the store's directory.
	//
	// THE DEFAULT PUTS A FULL COPY OF THE ESTATE ON THE SAME VOLUME as the
	// live database and its write-ahead log, which is why the snapshot
	// loop carries a free-space precondition and refuses rather than
	// filling the disk the applier is committing to. A separate volume is
	// the production shape.
	SnapshotDir string `yaml:"snapshot_dir,omitempty" json:"snapshot_dir,omitempty" desc:"Where this node keeps snapshots of the replicated estate; empty is <dir of path>/snapshots."`

	// ReplicatedPath is the second database this node owns: everything a
	// state log's applier writes. Empty puts it beside Path, which is
	// what makes "back up the data directory" true.
	//
	// Separable because the two files have different appetites — the
	// replicated one is what a snapshot copies and what a node joining the
	// fleet writes at line rate — so an operator with a fast local disk
	// and a large network volume has a real reason to split them. Both are
	// still this node's alone, and neither is shared with a peer.
	ReplicatedPath string `yaml:"replicated_path,omitempty" json:"replicated_path,omitempty" desc:"Second local database, for replicated state; empty puts it beside path."`

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
	// THE SAME FILE TWICE IS TWO EXCLUSIVE LOCKS ON ONE PATH, which this
	// process would take and then deadlock nothing — it would simply
	// migrate one estate's schema into the other's database and run both
	// appliers against the audit log's file.
	if rp := strings.TrimSpace(s.ReplicatedPath); rp != "" && rp == strings.TrimSpace(s.Path) {
		p.add(at(path, "replicated_path"), ErrConflict,
			"is the same file as store.path; the two estates are two databases, "+
				"and one file holding both is neither")
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

	// Sync decides what an acknowledged publish has actually reached, and
	// it is the one Tier A field that changes what durability MEANS here.
	//
	// `always` fsyncs every write before acknowledging it. `<duration>`
	// declines the fsync and names the window instead — the most an
	// acknowledged write can be behind the disk. Unset takes `always`,
	// which is the strong value at every replica count, because the
	// alternative is a default that is silently weaker on exactly the
	// deployments that matter most.
	//
	// IT IS NOT INFERRED FROM replicas, and the inference it replaces is
	// the reason this field exists. "A replicated member has a quorum
	// instead of a disk" is true when one host loses power and false when
	// a rack does — and a three-node fleet in one rack, which is what a
	// first production deployment looks like, is exposed to the second by
	// construction. Three copies of the same unflushed page cache is one
	// copy.
	Sync string `yaml:"sync,omitempty" json:"sync,omitempty" desc:"always (default) fsyncs every write before acknowledging it; a duration (30s) declines the fsync and names the window an acknowledged write may be behind the disk."`

	// TrackerLogMaxBytes is the byte ceiling on the mutation log — the
	// ordered stream a state-log domain writes through.
	//
	// UNSET DERIVES IT from the volume the stream is stored on: a quarter
	// of its free space, clamped to 4 GiB..64 GiB. A fixed default is
	// wrong in both directions — the same number is five years of history
	// on the modelled write rate and one boot on a small disk — and the
	// value is recorded on the stream when it is created, so a node that
	// derived it can say what it derived it from.
	//
	// WHAT THIS FIELD DOES IS NARROWER THAN IT LOOKS. It is the value the
	// stream is CREATED with, and thereafter a DECLARATION the engine
	// checks the broker's actual ceiling against and reports on. Editing
	// it on a running fleet changes nothing by itself: a stream's
	// configuration has one writer and a booting node is not it, so
	// re-applying it at boot would let restart order decide a shared limit
	// and let a late node lower a ceiling an emergency grant had just
	// raised.
	//
	// CROSSING IT REFUSES; IT DOES NOT SHED. There is no age bound on this
	// stream, so a full log drops no history — the append is refused,
	// loudly, naming this field and whatever is blocking the trim.
	TrackerLogMaxBytes int64 `yaml:"tracker_log_max_bytes,omitempty" json:"tracker_log_max_bytes,omitempty" js:"min=1073741824;max=1099511627776" desc:"Byte ceiling on the mutation log; unset derives a quarter of the stream volume's free space, clamped to 4 GiB..64 GiB."`

	// TrackerVectorsMaxBytes is the byte ceiling on the vector changelog.
	//
	// SIZED FOR THE PEAK, NOT THE STEADY STATE, and the two differ by 93×.
	// The stream keeps one message per source and bounds their age, so a
	// week's minting is about 91 MB. But changing the embedding model or
	// its width rewrites EVERY source in a few hours, and for the
	// following week every source's current message is inside the window:
	// 8.46 GB at the modelled year-five corpus. The default is twice that.
	// Sizing this field from the steady state would refuse the one
	// operation it exists to survive.
	TrackerVectorsMaxBytes int64 `yaml:"tracker_vectors_max_bytes,omitempty" json:"tracker_vectors_max_bytes,omitempty" js:"min=1073741824;max=274877906944" desc:"Byte ceiling on the vector changelog; default 16 GiB, sized for a model change rather than the steady state."`

	// TrackerRetention is when the log may be trimmed, and it is the one
	// block here that can stop a fleet's log growing for ever — or stop it
	// trimming at all, deliberately, when a term it depends on is unknown.
	TrackerRetention TrackerRetention `yaml:"tracker_retention,omitempty" json:"tracker_retention,omitzero"`
}

// TrackerRetention is the operator's half of the log trim.
//
// # Why these are Tier A and not the company's config
//
// Every one of them is a statement about the OPERATOR's estate rather than
// about the company: how they back up, how long their disk should hold a
// replay window, how much I/O their machines can spend, how long they will
// wait for a node to become a complete replica. None is a policy a founder
// sets, and Tier B is edited live — so lowering a durability gate there would
// move it underneath a trim that had already computed against it.
//
// # Why the duration fields carry a Raw suffix
//
// The Go name and the YAML key are allowed to differ, and here they must: the
// operator writes `min_age: 7d` and every reader wants a [time.Duration], so
// the field holds the text and the method holds the value. A field and a
// method cannot share a name, and of the two the METHOD should have the plain
// one — the parsed value is what the engine uses everywhere and the text is
// read in exactly one place.
type TrackerRetention struct {
	// MinAgeRaw is the age floor no trim may cross, whatever the other terms
	// say. It can only make a trim MORE conservative, so it is a LOWER
	// bound on how long the log keeps a record and never a ceiling — and
	// it says nothing at all about any node's own store file.
	//
	// Seven days is the horizon this fleet already treats as how long a
	// node may be away, and a retention floor that disagreed with it would
	// be a second answer to one question.
	//
	// THE 24-HOUR FLOOR HAS THREE REASONS: a value below a day cannot
	// outlast a nightly backup cycle; it is the margin that keeps a quiet
	// object writable, because an object whose last record has been
	// trimmed away has to be recognised as trimmed rather than as absent;
	// and it is the age bound on the vector changelog, so lowering it
	// shortens the window a joining node's vector gap is refilled from.
	MinAgeRaw string `yaml:"min_age,omitempty" json:"min_age,omitempty" desc:"Age floor no trim may cross (default 7d, 24h..90d)."`

	// BackupMaxAge is how stale the newest complete backup may be before
	// the trim stops entirely.
	//
	// A COMPANY THAT NEVER BACKS UP NEVER TRIMS. The log is the only copy
	// of what no node has applied yet, and trimming past the newest backup
	// is deleting the last thing that could rebuild it. A day is the
	// cadence a nightly backup keeps against a trim that runs every
	// fifteen minutes, so a fleet with a working nightly never notices and
	// a fleet with a broken one stops within a day.
	BackupMaxAgeRaw string `yaml:"backup_max_age,omitempty" json:"backup_max_age,omitempty" desc:"How stale the newest backup may be before the trim stops (default 24h, 1h..30d)."`

	// BackupFloor is whose word the trim takes for what is backed up.
	//
	// `engine` follows the newest backup the engine itself wrote and
	// verified. `operator` follows an explicit acknowledgement, for a
	// company whose policy is "trim only what is off-site" — which the
	// engine cannot see for itself, because a backup is not a backup until
	// it leaves the host. Under `operator` the trim does not advance until
	// that acknowledgement has been given at least once, which is a state
	// worth being warned about rather than discovering.
	BackupFloor BackupFloor `yaml:"backup_floor,omitempty" json:"backup_floor,omitempty" js:"enum=engine|operator" desc:"Whose word the trim takes for what is backed up: engine (default) or operator."`

	// SnapshotInterval is how stale a node's newest snapshot may be before
	// it takes another.
	//
	// A day rather than six hours, and the arithmetic is the reason: a
	// snapshot is a full copy of the replicated estate — tens of gigabytes
	// at a mature company — so four a day is a day's worth of I/O to save
	// a joining node a replay it can do in under a minute.
	SnapshotIntervalRaw string `yaml:"snapshot_interval,omitempty" json:"snapshot_interval,omitempty" desc:"How stale a node's newest snapshot may be before it takes another (default 24h, 1h..7d)."`

	// RejoinWindow is the operator's budget for a node to become a
	// complete replica — what a join is measured against and reported on.
	//
	// A SETTING RATHER THAN A CONSTANT because the answer is a property of
	// the operator's disks and network, and the spread between a
	// conservative and a fast profile is more than twice.
	RejoinWindowRaw string `yaml:"rejoin_window,omitempty" json:"rejoin_window,omitempty" desc:"Budget for a node to become a complete replica (default 30m, 5m..24h)."`
}

// BackupFloor is whose word the trim takes for what is backed up.
type BackupFloor string

const (
	// BackupFloorEngine follows the newest backup the engine wrote and
	// verified itself.
	BackupFloorEngine BackupFloor = "engine"

	// BackupFloorOperator follows an explicit acknowledgement, for a
	// company that trims only what has left the host.
	BackupFloorOperator BackupFloor = "operator"
)

// BackupFloors is the closed set.
var BackupFloors = []BackupFloor{BackupFloorEngine, BackupFloorOperator}

// StreamSyncAlways is [Stream.Sync]'s strong value.
const StreamSyncAlways = "always"

// SyncAlways reports whether every write is fsynced before it is
// acknowledged. Unset means yes — see [Stream.Sync].
func (s Stream) SyncAlways() bool {
	v := strings.TrimSpace(s.Sync)
	return v == "" || v == StreamSyncAlways
}

// SyncInterval is the flush window when the fsync is declined, or 0 when it is
// not. Validation has already established the value parses.
func (s Stream) SyncInterval() time.Duration {
	if s.SyncAlways() {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(s.Sync))
	if err != nil {
		return 0
	}
	return d
}

// StreamCluster is an embedded server's membership in a cluster.
type StreamCluster struct {
	// Name is the cluster every member shares.
	Name string `yaml:"name,omitempty" json:"name,omitempty" desc:"Cluster name shared by every member."`
	// Port is the route port this member listens on.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Route port for cluster traffic."`
	// Peers are the other members' route URLs.
	Peers []string `yaml:"peers,omitempty" json:"peers,omitempty" desc:"Route URLs of the other members."`

	// Host is the interface the route listener binds. Empty binds every
	// one of them, which on a host with a public interface publishes
	// UNAUTHENTICATED CLUSTER ACCESS: a route port is how a member joins,
	// and joining is how it reads and writes every stream. Set it to the
	// private address the peers reach.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Interface the route listener binds. Empty binds every interface."`

	// Advertise is the address peers should dial for this member when it
	// differs from what the member binds — a mapped container port, a NAT,
	// a member behind a load balancer.
	//
	// It matters because a member's address TRAVELS: peers learn about
	// each other from the members they are already connected to, and dial
	// what they are told. Unset, that address is derived from the
	// connection's own remote address, which on a NAT'd host is either
	// unreachable or somebody else's. Host and port, or a bare host to
	// keep this member's own route port.
	Advertise string `yaml:"advertise,omitempty" json:"advertise,omitempty" desc:"host:port peers should dial for this member, when it differs from what it binds."`
}

// IsZero lets an unset cluster block drop out of a JSON round trip.
func (c StreamCluster) IsZero() bool {
	return c.Name == "" && c.Port == 0 && len(c.Peers) == 0 &&
		c.Host == "" && c.Advertise == ""
}

func (s *Stream) validate(path string) error {
	var p problems
	if s.Type != "" && !slices.Contains(StreamTypes, s.Type) {
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
	if err := s.validateSync(path, external); err != nil {
		p.wrap(err)
	}
	if s.EventRetentionHours < 0 {
		p.add(at(path, "event_retention_hours"), ErrOutOfRange,
			"must be 0 (the queue default) or positive, got %v", s.EventRetentionHours)
	}
	if s.Cluster.Port < 0 || s.Cluster.Port > 65535 {
		p.add(at(path, "cluster.port"), ErrOutOfRange, "must be 0..65535, got %d", s.Cluster.Port)
	}
	// Refused here rather than at the broker. nats-server validates an
	// advertise address while STARTING, logs it and shuts the server down
	// — which surfaces as a node that boots, fails and leaves the operator
	// reading broker logs for a typo in their own config file.
	bytesInRange(&p, path, "tracker_log_max_bytes", s.TrackerLogMaxBytes,
		TrackerLogMaxBytesFloor, TrackerLogMaxBytesCeiling)
	bytesInRange(&p, path, "tracker_vectors_max_bytes", s.TrackerVectorsMaxBytes,
		TrackerVectorsMaxBytesFloor, TrackerVectorsMaxBytesCeiling)
	p.wrap(s.TrackerRetention.validate(at(path, "tracker_retention")))
	if adv := strings.TrimSpace(s.Cluster.Advertise); adv != "" {
		if err := validateAdvertise(adv); err != nil {
			p.add(at(path, "cluster.advertise"), ErrShape, "%v", err)
		}
	}
	p.wrap(s.TLS.validate(at(path, "tls")))
	return p.err()
}

// validateAdvertise checks a cluster advertise address: a host, optionally
// with a port. A bare host keeps this member's own route port, which is the
// common case behind a NAT that maps the port through unchanged.
func validateAdvertise(adv string) error {
	host, port, err := net.SplitHostPort(adv)
	if err != nil {
		// No port at all is legitimate; anything else is not. An address
		// with a colon in it and no port is a bracket the operator left
		// off an IPv6 literal, and reporting that as "no port" would send
		// them looking in the wrong place.
		if strings.Contains(adv, ":") {
			return fmt.Errorf("%q is not a host or host:port: %w", adv, err)
		}
		return nil
	}
	if host == "" {
		return fmt.Errorf("%q names a port with no host: peers have nothing to dial", adv)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q: port must be 1..65535", adv)
	}
	return nil
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
	if c.Type != "" && !slices.Contains(CoordinationTypes, c.Type) {
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
				"every token needs a label: it is what a revision's audit row records")
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
				"%q must contain only letters, digits, '.', '_' or '-': it is "+
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
			"required once keys is set: one key has to seal new writes")
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

// validateSync checks stream.sync, and its three refusals are the cases where
// the value is a claim the deployment cannot make.
//
// The refusals are about MEANING rather than about syntax. Declining the fsync
// is a legitimate operator choice with a real cost, and each of these is a
// place where the choice would be recorded and then not honoured — which is
// worse than either answer, because the operator believes the number they
// wrote.
func (s *Stream) validateSync(path string, external bool) error {
	var p problems
	raw := strings.TrimSpace(s.Sync)
	if raw == "" || raw == StreamSyncAlways {
		// THE WARNING, not a refusal: an unset value takes `always`, and
		// on a replicated fleet that is a deliberate cost rather than an
		// accident. It is stated where the operator will read it — see
		// docs/guides/deployment.md — rather than made a validation
		// problem, because there is nothing here to fix.
		return nil
	}

	// (1) AN EXTERNAL CLUSTER'S DISK IS NOT THIS PROCESS'S TO CONFIGURE.
	// The field sets an option on the EMBEDDED server; against
	// `stream.type: nats` it is read by nobody, so accepting it would
	// record a durability decision that never reaches the thing storing
	// the data.
	if external {
		p.add(at(path, "sync"), ErrConflict,
			"stream.sync configures the EMBEDDED server's file store, and an "+
				"external NATS cluster stores its own data: set sync_interval "+
				"on that cluster instead, or remove this field")
		return p.err()
	}

	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		p.add(at(path, "sync"), ErrUnknownValue,
			"%q is neither %q nor a duration (30s, 2m): it names how far behind "+
				"the disk an acknowledged write may be", s.Sync, StreamSyncAlways)
		return p.err()
	case d <= 0:
		p.add(at(path, "sync"), ErrOutOfRange,
			"%q must be positive: a zero or negative window is %q said in a way "+
				"nothing reads", s.Sync, StreamSyncAlways)
		return p.err()
	}

	// (2) BELOW THREE REPLICAS THERE IS NO QUORUM TO SPEND INSTEAD. The
	// whole argument for declining the fsync is that a majority holds the
	// write; a solo member's disk is the only copy there is, so the
	// window is not a trade, it is a straight loss.
	if s.Replicas < 3 {
		p.add(at(path, "sync"), ErrConflict,
			"declining the fsync trades this member's disk for a quorum, and "+
				"replicas=%d has no quorum to trade for: the local disk is the "+
				"only copy, so %q would be a window with nothing behind it",
			max(s.Replicas, 1), s.Sync)
	}

	// (3) A SAME-HOST CLUSTER IS ONE FAILURE DOMAIN. Peers that resolve to
	// this host share its power, its kernel and its page cache, so the
	// majority the window is traded for dies with the member that has it.
	if sameHostCluster(s.Cluster.Peers) {
		p.add(at(path, "sync"), ErrConflict,
			"every peer in stream.cluster.peers is on this host, so the quorum "+
				"this window trades for shares one power supply and one page "+
				"cache: %q would be recorded and not honoured", s.Sync)
	}
	return p.err()
}

// sameHostCluster reports whether every peer address is on this machine.
//
// A HOST TEST rather than an address test: `localhost`, `127.0.0.1` and `::1`
// are the three spellings a compose file or a laptop fleet uses, and they are
// one failure domain however they are written. An empty peer list is not a
// cluster at all and answers false — the replica rule above is what covers it.
func sameHostCluster(peers []string) bool {
	if len(peers) == 0 {
		return false
	}
	for _, peer := range peers {
		host := strings.TrimSpace(peer)
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Hostname()
		} else if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			host = h
		}
		switch strings.ToLower(strings.Trim(host, "[]")) {
		case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		default:
			return false
		}
	}
	return true
}

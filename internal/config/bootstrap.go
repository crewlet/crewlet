package config

import (
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// Bootstrap is Tier A — ops-owned, on disk, restart-only. It carries only what
// the engine needs to bring its own infrastructure up; the company lives in
// [Company]. See [Bootstrap.Validate] for the refused combinations.
type Bootstrap struct {
	// Node is this process's identity within the fleet.
	Node Node `yaml:"node,omitempty" json:"node"`

	// Store is the local database file this node materializes into.
	Store Store `yaml:"store,omitempty" json:"store"`

	// Gitlet is how this node serves the company's repositories: its cache's
	// directory and budget and its git processes. Read only on a node with
	// the gitlet role whose company sets code.backend: gitlet.
	Gitlet Gitlet `yaml:"gitlet,omitempty" json:"gitlet,omitzero"`

	// Stream is the durable event log — the source of truth every node
	// writes through.
	Stream Stream `yaml:"stream,omitempty" json:"stream"`

	// Coordination is where leases and shared counters live.
	Coordination Coordination `yaml:"coordination,omitempty" json:"coordination"`

	// API is the HTTP surface: the dashboard, the REST API, the webhooks.
	API API `yaml:"api,omitempty" json:"api"`

	// Secrets is the fleet's keyring: the root of trust, living ONLY here,
	// never in the store it opens. Every node requires one.
	Secrets Secrets `yaml:"secrets,omitempty" json:"secrets"`

	// Logging is how loud this node is, and in what shape.
	Logging Logging `yaml:"logging,omitempty" json:"logging"`

	// Retention is the operator's own half of keeping this deployment's
	// history: who owns its backups. The trim's terms live under
	// `stream.tracker_retention`, beside the log they bound.
	Retention Retention `yaml:"retention,omitempty" json:"retention,omitzero"`

	// Artifacts is the plan quotas this node enforces on artifacts and their
	// datastores. See [Artifacts].
	Artifacts Artifacts `yaml:"artifacts,omitempty" json:"artifacts,omitzero"`

	// Dictation is this node's voice dictation (dictation.go).
	Dictation Dictation `yaml:"dictation,omitempty" json:"dictation,omitzero"`
}

// Retention is what the operator owns about this deployment's history.
type Retention struct {
	// BackupOwner is who owns the backup, free text a human reads when an alarm
	// names it. UNSET IS WARNED ABOUT rather than refused: a company that never
	// backs up never trims.
	BackupOwner string `yaml:"backup_owner,omitempty" json:"backup_owner,omitempty" desc:"Who owns this deployment's backups — a person, a team, a scheduler. Warned about when unset."`
}

// IsZero lets an unset retention block drop out of a JSON round trip.
func (r Retention) IsZero() bool { return strings.TrimSpace(r.BackupOwner) == "" }

// Logging is Tier A's logging surface: the level this node emits at and the
// shape it writes.
//
// # Why the colour is NOT here
//
// It is $CREWLET_LOG_COLOR and $NO_COLOR: colour belongs to the TERMINAL, not the
// deployment, so it is the one knob with NO file form (internal/logging).
type Logging struct {
	// Level is how loud this node is. Empty is info.
	//
	// A VALUE THIS BUILD DOES NOT KNOW IS REFUSED, unlike the `-log-level` flag,
	// which resolves a typo to info: a file is deployed for months.
	Level logging.Level `yaml:"level,omitempty" json:"level,omitempty" js:"enum=debug|info|warn|error" desc:"debug, info (default), warn or error."`

	// Format is the shape of a line. Empty is console.
	Format logging.Format `yaml:"format,omitempty" json:"format,omitempty" js:"enum=console|text|json" desc:"console (default, columns and colour for a person), text (slog key=value) or json (for a log shipper)."`

	// File is a log file this node writes IN ADDITION to stderr. Omit the
	// block (or leave `path` empty) and there is no file.
	File LogFile `yaml:"file,omitempty" json:"file,omitempty"`

	// Stderr is whether the ordinary log stream reaches stderr at all. Unset is
	// true; false needs a log file.
	//
	// # Why this exists rather than `2>/dev/null`
	//
	// Three kinds of line reach stderr without the configured handler: those
	// emitted before this document was read, the seat watchdog's hard-exit notice,
	// and the report that the log file cannot be written. A redirect discards all
	// three. A POINTER because `false` is also the zero value.
	Stderr *bool `yaml:"stderr,omitempty" json:"stderr,omitempty" desc:"Whether the ordinary log stream reaches stderr. Unset = true. false needs logging.file.path, and never silences a boot failure or the watchdog's exit notice."`
}

// LogFile is the durable copy of this node's log.
//
// Naming a file never silences stderr; see [Logging.Stderr].
//
// # Rotation is not optional, and there is no "never" spelling
//
// A log file with no ceiling fills the disk the store is on, so the caps default
// rather than switch off; logrotate(8) `copytruncate` means a size this process
// never reaches. See [logging.File].
type LogFile struct {
	// Path is the live log file. A relative path is relative to the
	// working directory, as `store.path` is. Missing directories are
	// created (0700); the file is 0600.
	//
	// EMPTY IS THE OFF SWITCH, a meaningful zero.
	Path string `yaml:"path,omitempty" json:"path,omitempty" desc:"Log file written in addition to stderr. Empty writes no file. Missing directories are created."`

	// Format is the shape written to the FILE, which may differ from the
	// terminal's (a person wants columns, a shipper `json`), so this is not
	// an io.MultiWriter. Empty follows `logging.format`.
	Format logging.Format `yaml:"format,omitempty" json:"format,omitempty" js:"enum=console|text|json" desc:"Shape written to the file. Empty follows logging.format; set json when a shipper reads it."`

	// Level is how loud the FILE is, which may differ from the terminal's. Empty
	// follows `logging.level`. When they differ the process admits the LOUDER of
	// the two and each destination filters, so a `debug` file's cost is paid on
	// every debug call site whatever the console says.
	Level logging.Level `yaml:"level,omitempty" json:"level,omitempty" js:"enum=debug|info|warn|error" desc:"How loud the file is. Empty follows logging.level."`

	// MaxSizeMB is the size the live file reaches before it rotates.
	// Unset is logging.DefaultMaxSizeMB, where the number and its
	// rationale live: the layer that enforces a limit says what it is.
	//
	// A POINTER, so an explicit 0 is refused by name rather than read as
	// "apply the default".
	MaxSizeMB *int `yaml:"max_size_mb,omitempty" json:"max_size_mb,omitempty" js:"min=1;max=1073741824" desc:"Size the live file reaches before rotating. Unset = 100. Rotation cannot be disabled, and the cap is bounded above because the byte arithmetic wraps."`

	// MaxBackups is how many rotated files (`<path>.1` newest) are kept.
	// Unset is logging.DefaultMaxBackups.
	//
	// A POINTER because 0 IS A SETTING and the zero value: the live file alone.
	MaxBackups *int `yaml:"max_backups,omitempty" json:"max_backups,omitempty" js:"min=0;max=1000" desc:"Rotated files kept beside the live one. Unset = 5; 0 keeps none. Each rotation renames every file it keeps, so the count is bounded."`
}

func (l *Logging) validate(path Path) error {
	var p problems
	if l.Level != "" && !l.Level.Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q (want %s)",
			l.Level, words.Or(logging.Levels))
	}
	if l.Format != "" && !l.Format.Valid() {
		p.add(at(path, "format"), ErrUnknownValue, "%q (want %s)",
			l.Format, words.Or(logging.Formats))
	}
	p.wrap(l.File.validate(at(path, "file")))
	// A NODE THAT LOGS NOWHERE: `stderr: false` says the file takes the
	// stream over, so without a file it asks for silence. Refused by name
	// rather than quietly overridden.
	if l.Stderr != nil && !*l.Stderr && l.File.Path == "" {
		p.add(at(path, "stderr"), ErrConflict,
			"false with no logging.file.path leaves this node writing its log "+
				"nowhere; name a file, or leave stderr unset")
	}
	return p.err()
}

func (f *LogFile) validate(path Path) error {
	var p problems
	if f.Format != "" && !f.Format.Valid() {
		p.add(at(path, "format"), ErrUnknownValue, "%q (want %s)",
			f.Format, words.Or(logging.Formats))
	}
	if f.Level != "" && !f.Level.Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q (want %s)",
			f.Level, words.Or(logging.Levels))
	}
	// THE CAPS ARE CHECKED WHETHER OR NOT A PATH IS SET, or a half-written
	// `max_size_mb: 0` is found on the deploy that turns the file on.
	// BOUNDED AT BOTH ENDS: at or above 1<<43 MB the byte-held size cap wraps
	// negative and the file rotates on EVERY line, and a huge backup count
	// stalls each rotation's renames. Checked again in logging.OpenFile, which
	// a `-log-file` run reaches.
	if f.MaxSizeMB != nil && (*f.MaxSizeMB < 1 || *f.MaxSizeMB > logging.MaxSizeMBCeiling) {
		p.add(at(path, "max_size_mb"), ErrOutOfRange,
			"%d (want 1..%d). Rotation cannot be disabled, since a log file with no "+
				"ceiling fills the disk the store is on, and a cap above %d MB "+
				"(a pebibyte) wraps in the byte arithmetic and rotates every line",
			*f.MaxSizeMB, logging.MaxSizeMBCeiling, logging.MaxSizeMBCeiling)
	}
	if f.MaxBackups != nil && (*f.MaxBackups < 0 || *f.MaxBackups > logging.MaxBackupsCeiling) {
		p.add(at(path, "max_backups"), ErrOutOfRange,
			"%d (want 0..%d). 0 keeps no rotated files; each rotation renames "+
				"every file it keeps, so a larger count stalls the rotation "+
				"rather than keeping more history",
			*f.MaxBackups, logging.MaxBackupsCeiling)
	}
	// A SHAPE, A LEVEL OR A CAP WITH NO PATH WRITES NOTHING, yet reads as a
	// node that logs to a file: refused rather than ignored.
	if f.Path == "" && (f.Format != "" || f.Level != "" || f.MaxSizeMB != nil || f.MaxBackups != nil) {
		p.add(at(path, "path"), ErrMissing,
			"the file block sets a shape, a level or a cap but names no file, so "+
				"nothing is written; set path, or remove the block")
	}
	return p.err()
}

// LogFileSettings is what Tier A says about the log file destination: how to
// open it, and how to write it once open.
//
// [logging.OpenFile] takes Open and [logging.SetFile] Sink.
type LogFileSettings struct {
	// Open is the path and the rotation caps.
	Open logging.FileOptions
	// Sink is the shape and the level, with the writer still to be filled
	// in by whoever opened the file.
	Sink logging.FileSink
}

// LogFileSettings is what this block asks for, with ok false when no file was
// configured at all.
//
// The CAPS ARE PASSED THROUGH AS WRITTEN, pointers included: internal/logging
// owns the numbers. The LEVEL is converted once, here, at the edge.
func (f *LogFile) LogFileSettings() (LogFileSettings, bool) {
	if f.Path == "" {
		return LogFileSettings{}, false
	}
	var level *slog.Level
	if f.Level != "" {
		resolved := f.Level.Slog()
		level = &resolved
	}
	return LogFileSettings{
		Open: logging.FileOptions{
			Path:       f.Path,
			MaxSizeMB:  f.MaxSizeMB,
			MaxBackups: f.MaxBackups,
		},
		Sink: logging.FileSink{Format: f.Format, Level: level},
	}, true
}

// StderrEnabled is whether the ordinary log stream reaches stderr. Unset is
// true — see [Logging.Stderr] for what it does not silence either way.
func (l *Logging) StderrEnabled() bool { return l.Stderr == nil || *l.Stderr }

// LogSettings is what this file asks the process to log at, and in what
// shape, with every default applied.
//
// `logging.level` is the only key that sets the level: `crewlet run` folds the
// flags actually given into the document before it asks.
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

// DefaultBootstrap is a Tier A config with every default applied.
//
// Defaults live in a constructor because the loader decodes INTO this value: an
// absent key, or one with no value (`api:`), leaves the default.
func DefaultBootstrap() Bootstrap {
	return Bootstrap{
		Node:         Node{},
		Store:        Store{Path: DefaultStorePath},
		Stream:       Stream{Type: StreamEmbedded, Replicas: 1},
		Coordination: Coordination{Type: CoordinationLocal},
		API:          API{Host: DefaultAPIHost},
	}
}

// Validate reports every Tier A rule this config breaks, joined.
//
// IT NORMALIZES FIRST, and so takes a pointer: [Bootstrap.normalize] trims every
// string, so what these rules check is what the engine will run. It is the one
// entry point into Tier A's rules.
func (b *Bootstrap) Validate() error {
	var p problems
	p.wrap(b.normalize())
	p.wrap(b.Node.validate(field("node")))
	p.wrap(b.Store.validate(field("store")))
	p.wrap(b.Gitlet.validate(field("gitlet"), b.Store))
	p.wrap(b.Stream.validate(field("stream")))
	p.wrap(b.Coordination.validate(field("coordination")))
	p.wrap(b.API.validate(field("api"), b.ServesAPI(), b.Profile("").RunsIngress()))
	p.wrap(b.Secrets.validate(field("secrets")))
	p.wrap(b.Logging.validate(field("logging")))
	p.wrap(b.Artifacts.validate(field("artifacts")))
	p.wrap(b.Dictation.validate(field("dictation")))
	p.wrap(b.validateObjectReservation())
	p.wrap(b.validateTopology())
	p.wrap(b.ValidateListeners())
	p.wrap(b.ValidateRoles())
	return p.err()
}

// listener is one socket this file has a node bind.
type listener struct {
	field string
	host  string
	port  int
}

// ValidateListeners refuses the sockets this node cannot open as configured.
//
// Exported for `crewlet run`, whose `-roles`, `-api-host` and `-api-port` change
// a loaded file after [Bootstrap.Validate]; run it once every flag is applied.
//
// A public listener on an ingress node needs api.port: 0 is the hard off switch
// and api.public MOVES routes off that surface, leaving no probe. Other nodes
// serve probes on api.port alone, so port 0 stays valid for them.
//
// Two listeners may not share a port unless their hosts are different specific
// addresses: a clash fails at bind, and api.public.port equal to api.port would
// make the admin API and the public routes one socket.
func (b *Bootstrap) ValidateListeners() error {
	var p problems
	// An unreadable role list is Node.validate's to report.
	roles, rolesErr := b.Node.RoleSet()
	ingress := rolesErr == nil && roles.Has(placement.RoleIngress)
	if ingress && b.API.Public.Port > 0 && b.API.Port == 0 {
		p.add(field("api.public.port"), ErrConflict,
			"api.port is 0, so this ingress node serves no HTTP surface, and the "+
				"public listener moves routes off api.port rather than serving "+
				"without it, so no probe would answer. Set api.port (bind it to a "+
				"private address to keep it unreachable from outside), or remove "+
				"api.public")
	}
	var open []listener
	if b.API.Port > 0 {
		open = append(open, listener{"api.port", b.API.Host, b.API.Port})
	}
	if b.API.Public.Port > 0 {
		open = append(open, listener{"api.public.port", b.API.Public.Host, b.API.Public.Port})
	}
	// THE ARTIFACT LISTENER, only where it is bound: on an ingress node.
	if ingress && b.API.Artifacts.Port > 0 {
		open = append(open, listener{"api.artifacts.port", b.API.Artifacts.Host, b.API.Artifacts.Port})
	}
	// THE EMBEDDED BROKER'S OWN LISTENERS, only where it opens them.
	if b.BrokerKind() == placement.BrokerMember {
		if b.Stream.Cluster.Port > 0 {
			open = append(open, listener{"stream.cluster.port", b.Stream.Cluster.Host, b.Stream.Cluster.Port})
		}
		if b.Stream.Leaf.Port > 0 {
			open = append(open, listener{"stream.leaf.port", b.Stream.Leaf.Host, b.Stream.Leaf.Port})
		}
	}
	for i, l := range open {
		for _, earlier := range open[:i] {
			if l.port != earlier.port || !bindsOverlap(l.host, earlier.host) {
				continue
			}
			p.add(field(l.field), ErrConflict,
				"port %d is also %s, and both bind %s: one node cannot open two "+
					"listeners on one port. Pick another port, or bind the two to "+
					"different addresses", l.port, earlier.field, bindLabel(l.host, earlier.host))
		}
	}
	return p.err()
}

// bindsOverlap reports whether two bind hosts can claim the same socket: they
// are the same address, or either is every interface.
func bindsOverlap(a, b string) bool {
	a, b = bindHost(a), bindHost(b)
	return a == "" || b == "" || a == b
}

// bindHost is a bind address in one spelling, "" for every interface.
func bindHost(host string) string {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	switch h {
	case "0.0.0.0", "::":
		return ""
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// bindLabel says which address two overlapping listeners share.
func bindLabel(a, b string) string {
	if bindHost(a) == "" || bindHost(b) == "" {
		return "every interface"
	}
	return strconv.Quote(strings.TrimSpace(a))
}

// ValidateRoles refuses the settings a node's roles contradict.
//
// Exported for `crewlet run -roles`, applied after [Bootstrap.Validate]: a flag
// turning a data node's roles into `seats` would boot a node that believes
// itself stateless on a store it has not agreed to lose.
//
// `data` is a promise about the DISK: a node without it keeps a scratch store,
// deleted at every boot, and no estate or object-store share. The BROKER is a
// separate question ([Bootstrap.BrokerKind]); [Bootstrap.checkRolesAndBroker]
// refuses the mismatches.
func (b *Bootstrap) ValidateRoles() error {
	var p problems
	roles, err := b.Node.RoleSet()
	if err != nil {
		//nolint:nilerr // Node.validate reports it, naming the field, and
		// the -roles flag parsed it before calling; every rule below keys
		// on the set, so none of them can be judged without one.
		return nil
	}
	if roles.Has(placement.RoleData) {
		if b.Store.Scratch {
			p.add(field("store.scratch"), ErrConflict,
				"this node holds data, and a scratch store is deleted at every "+
					"boot, and with it the company's Worklet and Doclet copies and "+
					"this node's record of its own turns. Remove it, or drop %q "+
					"from node.roles", placement.RoleData)
		}
	} else {
		if !b.Store.Scratch {
			p.add(field("store.scratch"), ErrMissing,
				"a node without %q keeps no durable state, so its store is "+
					"deleted at every boot. Set store.scratch: true to say so, "+
					"since it is the one setting here that deletes something",
				placement.RoleData)
		}
		if b.Store.ReplicatedPath != "" || b.Store.SnapshotDir != "" {
			p.add(field("store"), ErrConflict,
				"replicated_path and snapshot_dir are where a node keeps its copy "+
					"of the replicated estate, and a node without %q holds none",
				placement.RoleData)
		}
	}
	b.checkRolesAndBroker(&p, roles)
	return p.err()
}

// checkRolesAndBroker refuses the three pairings of roles and broker the estate
// cannot run on, and gitlet without ingress. One decision: every data node holds
// the whole estate as a MEMBER of the broker, and a node without data holds none.
func (b *Bootstrap) checkRolesAndBroker(p *problems, roles placement.RoleSet) {
	data := roles.Has(placement.RoleData)
	// GIT IS SERVED ON THE API LISTENER, which only an ingress node mounts.
	// Asked BEFORE the data rule below returns, so a data node is held to it
	// too. NOT a "gitlet needs data" rule: a node without data naming both is
	// refused once, by the ingress rule below.
	if roles.Has(placement.RoleGitlet) && !roles.Has(placement.RoleIngress) {
		p.add(field("node.roles"), ErrConflict,
			"%q needs %q: its routes are served on the API listener, which only a "+
				"node with %q mounts. Add %q, or drop %q", placement.RoleGitlet,
			placement.RoleIngress, placement.RoleIngress, placement.RoleIngress,
			placement.RoleGitlet)
	}
	switch kind := b.BrokerKind(); {
	case data && kind == placement.BrokerLeaf:
		p.add(field("stream.leaf.urls"), ErrConflict,
			"joins this data node to the fleet's broker as a leaf, and every data "+
				"node holds the whole estate as a MEMBER of the broker. Remove the "+
				"leaf urls, or drop %q from node.roles", placement.RoleData)
	case !data && kind == placement.BrokerMember:
		p.add(field("stream.leaf.urls"), ErrMissing,
			"a node without %q runs its embedded broker as a MEMBER here, and a "+
				"node without %q joins the fleet as a LEAF of the members' (no "+
				"JetStream, no replica, no vote). Name the members' leaf listeners "+
				"here, or add %q to node.roles", placement.RoleData,
			placement.RoleData, placement.RoleData)
	}
	if data {
		return
	}
	for _, role := range []placement.NodeRole{placement.RoleWorkers, placement.RoleIngress} {
		if !roles.Has(role) {
			continue
		}
		p.add(field("node.roles"), ErrConflict,
			"%q needs %q: %s. Add %q, or drop %q", role, placement.RoleData,
			needsData[role],
			placement.RoleData, role)
	}
}

// needsData is why each role a node without data cannot run needs `data` — see
// [Bootstrap.checkRolesAndBroker].
var needsData = map[placement.NodeRole]string{
	placement.RoleWorkers: "the company-wide duties (the log trim, the " +
		"embedding pass, the maintenance sweep, the scheduler) read and " +
		"write this node's own copy of the replicated estate directly",
	placement.RoleIngress: "the API's retention report, capacity, reanchor, " +
		"eviction and backup surfaces read this node's own copy of the " +
		"replicated estate and act on its own state log",
}

// otherMembers is how many OTHER broker members this file names, and the list
// they were counted from.
//
// Counted ONLY FOR AN EMBEDDED STREAM: an external cluster's membership is not in
// this file, so a peer list against one is refused by [Stream.validate].
// [Bootstrap.Warnings] names every entry [StreamCluster.members] discounts.
func (b *Bootstrap) otherMembers() (int, clusterMembers) {
	if b.Stream.Type == StreamNATS {
		return 0, clusterMembers{}
	}
	members := b.Stream.Cluster.members()
	return len(members.Others), members
}

// clustered reports whether this file puts this node in a FLEET: it names other
// members, a cluster name or an external broker; it is a leaf; or it is a member
// that opens a leaf listener.
//
// ONE PREDICATE for every rule asking it (ADR-0008); the `container` box warning
// asks a narrower question ([tierWarnings]). A member opening a leaf listener is
// in a fleet even with no peers: joining nodes hold presence leases a lease kept
// in this process could never show them.
func (b *Bootstrap) clustered() bool {
	peers, _ := b.otherMembers()
	leaf := b.BrokerKind() == placement.BrokerLeaf
	servesLeaves := b.BrokerKind() == placement.BrokerMember && b.Stream.Leaf.Port != 0
	return peers > 0 || b.Stream.Cluster.Name != "" || b.Stream.Type != StreamEmbedded || leaf || servesLeaves
}

// validateTopology refuses slot combinations that cannot work.
//
// Each pairing fails LATER as a different-looking problem: local coordination in
// a fleet has every node claim every seat; a two-node quorum wedges on a restart.
func (b *Bootstrap) validateTopology() error {
	var p problems

	// THE KEYRING IS REQUIRED ON EVERY NODE, whatever its roles or broker: every
	// state-log record is signed and verified under it, since the broker's
	// admission vouches for a connection, never a record (ADR-0031), and an
	// embedded fleet's broker admits only keys derived from it (ADR-0094). A node
	// WITHOUT data needs it as much: the company document is authenticated by its
	// seal, a seats node mints per-run tokens, and estate requests are signed
	// under it (internal/signedask).
	if len(b.Secrets.Keys) == 0 {
		p.add(field("secrets.keys"), ErrMissing,
			"required on every node, and every node in a fleet needs the same "+
				"keys. Generate one with `crewlet secrets keygen` and point "+
				"`material:` at a ${VAR}")
	}

	peers, members := b.otherMembers()
	clustered := b.clustered()
	leaf := b.BrokerKind() == placement.BrokerLeaf

	// NOT HERE: a broker MEMBER with no store directory, which [Stream.Durable]
	// refuses for every member, solo or in a fleet, via [Stream.validate].

	if b.Coordination.Type == CoordinationLocal && clustered {
		p.add(field("coordination.type"), ErrConflict,
			"local coordination holds its leases in this process, so every "+
				"node in a fleet would claim every seat. A fleet needs "+
				"coordination.type %q", CoordinationEmbeddedKV)
	}

	// A fleet is one node or three: two embedded KV members have no quorum without
	// each other. Counted over the STREAM's members, as the coordination store
	// lives on the stream's own broker.
	if b.Coordination.Type == CoordinationEmbeddedKV {
		if nodes := peers + 1; nodes == 2 {
			p.add(field("stream.cluster.peers"), ErrConflict,
				"a two-node fleet has no coordination quorum: run one node "+
					"or three or more (this config names %d other member, so %d "+
					"nodes%s)", peers, nodes, members.discountedClause())
		}
	}

	// A LIST THAT NAMES ONLY THIS NODE names no other member. Refused rather than
	// read as a solo seed: a seed lists no peers. The published schema's two-node
	// rule cannot compare entries with this node's address and would flag it in an
	// editor while the engine accepted it.
	if b.Stream.Type != StreamNATS && len(b.Stream.Cluster.Peers) > 0 && peers == 0 {
		p.add(field("stream.cluster.peers"), ErrConflict,
			"names no member but this one: %s. A member that seeds a cluster "+
				"lists no peers, and one that joins lists the members it joins. "+
				"Remove the list, or name the other members",
			members.discountedList())
	}

	// Only an EMBEDDED stream is refused: JetStream keeps one copy per member, so
	// more replicas than this node plus its peers would fail every stream and
	// bucket at boot. An external cluster's membership is not in this file, and
	// refusing there would cap every external-NATS fleet at one copy, the lease and
	// fleet KV buckets included (engine.attachCoordination). NOR A LEAF: it
	// provisions on the members it reaches.
	if b.Stream.Type != StreamNATS && !leaf && b.Stream.Replicas > peers+1 {
		if peers == 0 {
			p.add(field("stream.replicas"), ErrConflict,
				"replicas > 1 needs peers to replicate to; a solo node keeps 1")
		} else {
			p.add(field("stream.replicas"), ErrConflict,
				"%d copies need %d members, and this cluster names %d (this node "+
					"and the other members in stream.cluster.peers%s): a stream "+
					"keeps at most one copy per member. Lower replicas to %d, or "+
					"name the other members",
				b.Stream.Replicas, b.Stream.Replicas, peers+1,
				members.discountedClause(), peers+1)
		}
	}
	return p.err()
}

// clusterMembers is what an embedded member's peer list says about its
// cluster: the entries naming OTHER members, and every entry that does not.
type clusterMembers struct {
	// Others are the entries counted as another member, as written, in list
	// order.
	Others []string

	// Discounted are the entries that are not another member, in list order.
	Discounted []discountedPeer
}

// discountedPeer is a peer entry not counted as another member, and why.
type discountedPeer struct {
	// Index is the entry's position in stream.cluster.peers, and Shown what
	// it says with any userinfo dropped ([withoutUserinfo]); every sentence
	// naming a discounted entry quotes Shown.
	Index int
	Shown string

	// Self is set when the entry is this node's own route, and Matches names
	// the setting it matched. Otherwise it repeats the entry at RepeatOf.
	Self     bool
	Matches  string
	RepeatOf int
}

// members is who this peer list names besides this node.
//
// An entry is DISCOUNTED when it is recognisably this node's own route (as
// [routeAddress] normalises, equal to the route listener or to cluster.advertise
// with cluster.port) or repeats an earlier entry's address.
//
// RECOGNISABLY, only: an entry reaching this node under another name (a DNS
// alias, `localhost`) is COUNTED as another member. The broker is not misled
// (nats-server drops a route to itself); the rules counting members are. A
// non-URL entry is counted as written and refused by [Stream.validate].
func (c StreamCluster) members() clusterMembers {
	own := c.ownRoutes()
	var out clusterMembers
	seen := map[string]int{}
	for i, raw := range c.Peers {
		addr, ok := routeAddress(raw)
		if !ok {
			out.Others = append(out.Others, raw)
			continue
		}
		if setting, self := own[addr]; self {
			out.Discounted = append(out.Discounted, discountedPeer{
				Index: i, Shown: withoutUserinfo(raw), Self: true, Matches: setting,
			})
			continue
		}
		if first, repeat := seen[addr]; repeat {
			out.Discounted = append(out.Discounted, discountedPeer{
				Index: i, Shown: withoutUserinfo(raw), RepeatOf: first,
			})
			continue
		}
		seen[addr] = i
		out.Others = append(out.Others, raw)
	}
	return out
}

// ownRoutes is every address this member's own route is recognisably reached
// at, keyed by the normalised address, naming the setting that says so.
//
// THE LISTENER ONLY WHEN BOUND TO ONE ADDRESS: an unset or unspecified host binds
// every interface, and which a peer names this file cannot say. No port, no listener.
func (c StreamCluster) ownRoutes() map[string]string {
	own := map[string]string{}
	if c.Advertise != "" {
		host, port := c.Advertise, c.Port
		if h, p, err := net.SplitHostPort(c.Advertise); err == nil {
			host = h
			port, _ = strconv.Atoi(p)
		}
		if host != "" && port > 0 && port <= 65535 {
			own[joinRoute(host, port)] = "cluster.advertise"
		}
	}
	if host := strings.Trim(c.Host, "[]"); host != "" && c.Port > 0 {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			own[joinRoute(host, c.Port)] = "cluster.host and cluster.port"
		}
	}
	return own
}

// routeAddress is a peer entry as the address a dial reaches (host:port, scheme
// dropped, host lower-cased, IP literal canonical), and false for an entry the
// embedded server could never dial: not a URL, no host, or no port in 1..65535.
//
// A PORT IS REQUIRED: nats-server splits each route URL before dialling, and an
// entry without one fails on every attempt.
func routeAddress(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	return joinRoute(u.Hostname(), port), true
}

// carriesCredential reports whether a fleet URL (a peer's route or a member's
// leaf listener) carries a userinfo, `user:pass@` or a bare `token@`.
//
// REFUSED IN A FLEET URL: credentials are DERIVED from secrets.keys (ADR-0094), so
// a written one is overwritten, and a leaf link presenting an nkey would also send
// the userinfo in clear.
//
// ANY `@` IS ONE, read off the text, not a parse: `ops:s3cret@node-b:6222` parses
// as scheme `ops` with no userinfo, and others do not parse, which would fall
// through to a shape refusal quoting the credential. An empty userinfo too.
func carriesCredential(raw string) bool {
	return strings.Contains(raw, "@")
}

// withoutUserinfo is a fleet URL as a sentence may quote it: everything between
// its scheme and its last `@` dropped, or everything up to that `@` where the
// entry has no scheme.
//
// EVERY SENTENCE QUOTING A PEER OR LEAF URL GOES THROUGH THIS: they reach
// `crewlet validate`, a refused boot's stderr and a config_advisory log line.
//
// A SCHEME IS ONLY WHAT RFC 3986 CALLS ONE, not whatever precedes the first `://`:
// keeping that prefix of `ops:pa://ss@node-b:6222` would quote `ops:pa`.
func withoutUserinfo(raw string) string {
	entry := strings.TrimSpace(raw)
	at := strings.LastIndex(entry, "@")
	if at < 0 {
		return entry
	}
	if scheme := strings.Index(entry, "://"); scheme > 0 && scheme < at && isURLScheme(entry[:scheme]) {
		return entry[:scheme+len("://")] + entry[at+1:]
	}
	return entry[at+1:]
}

// isURLScheme reports whether s is a URL scheme as RFC 3986 §3.1 spells one:
// ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ).
func isURLScheme(s string) bool {
	for i, r := range s {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z':
		case i > 0 && ('0' <= r && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// presentsCredential answers whether an external stream's dial presents
// anything a server could authenticate: a credentials file, a token, a client
// certificate, or a userinfo in any entry of url.
//
// EVERY ENTRY, split as internal/queue/jetstream's serverAddresses splits
// stream.url: the client fails over and sends whichever entry's userinfo it dials.
func (s *Stream) presentsCredential() bool {
	if s.Credentials != "" || s.Token != "" || s.TLS.Cert != "" {
		return true
	}
	for _, entry := range strings.Split(s.URL, ",") {
		if entryCarriesUserinfo(strings.TrimSpace(entry)) {
			return true
		}
	}
	return false
}

// entryCarriesUserinfo answers whether one stream.url entry carries a non-empty
// userinfo (`user:password@`, or a bare `token@` the client sends as a bearer
// token). A BARE host:port IS THE CLIENT'S nats:// ONE and is given that scheme
// first, or `token@host:4222` parses as a path and its token goes uncounted.
func entryCarriesUserinfo(entry string) bool {
	if entry == "" {
		return false
	}
	if !strings.Contains(entry, "://") {
		entry = "nats://" + entry
	}
	u, err := url.Parse(entry)
	if err != nil || u.User == nil {
		return false
	}
	password, _ := u.User.Password()
	return u.User.Username() != "" || password != ""
}

// joinRoute is the one normal form both sides of the comparison are put in.
func joinRoute(host string, port int) string {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// discountedClause is the discounted entries as a clause a count's message
// can end on — ", not counting peers[0], which is this node's own route" —
// and empty when there are none.
func (m clusterMembers) discountedClause() string {
	if len(m.Discounted) == 0 {
		return ""
	}
	return ", not counting " + m.discountedList()
}

// discountedList names every discounted entry and why.
func (m clusterMembers) discountedList() string {
	parts := make([]string, 0, len(m.Discounted))
	for _, d := range m.Discounted {
		parts = append(parts, d.describe())
	}
	return strings.Join(parts, "; ")
}

// describe is one discounted entry, named by its place in the list.
func (d discountedPeer) describe() string {
	if d.Self {
		return fmt.Sprintf("peers[%d] %q, which is this node's own route (%s)",
			d.Index, d.Shown, d.Matches)
	}
	return fmt.Sprintf("peers[%d] %q, which repeats peers[%d]", d.Index, d.Shown, d.RepeatOf)
}

// MaxStreamReplicas is the most copies a stream may keep.
//
// FIVE, JetStream's own ceiling: a larger number fails at the broker's first
// create. Beyond five a fleet adds LEAVES: a sixth member holds no copy and only
// adds a voter to the metadata group ([Bootstrap.Warnings]).
const MaxStreamReplicas = 5

// ---- node ------------------------------------------------------------ //

// DefaultNodeID is what a process with nothing configured calls itself.
const DefaultNodeID = "node-0"

// NodeIDEnvVar injects a node id without templating the config file.
const NodeIDEnvVar = "CREWLET_NODE_ID"

// nodeIDPattern is what a node id may look like: it lands in log fields and
// broker consumer names. Compiled at first use, not at init: Gitlet's push hook
// is this binary and pays every package's init (cmd/crewlet's hookinit_test.go).
var nodeIDPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
})

// ValidNodeID reports whether id is one a node could run under, for a caller
// naming some OTHER node, such as the operator gesture that evicts one.
func ValidNodeID(id string) bool {
	return nodeIDPattern().MatchString(id) && reservedNodeID(id) == ""
}

// reservedNodeID is why id, which has a node id's shape, names something other
// than a node, and empty for every id a node may take.
//
// THE OPERATOR'S BACKUP OWNER ([coord.OperatorBackupOwner]) is the one: the backup
// register files the operator's acknowledgements under that word, so a node under
// it would announce its own copies as the off-host acknowledgement
// `backup_floor: operator` waits for. Nothing downstream can tell who wrote a row.
func reservedNodeID(id string) string {
	if id == coord.OperatorBackupOwner {
		return fmt.Sprintf("%q is reserved for the operator's backup "+
			"acknowledgements. Choose another id", id)
	}
	return ""
}

// Node is Tier A identity of THIS process within the company.
//
// The id must be STABLE ACROSS RESTARTS, so it comes from the deployment: per-node
// subscriptions, config-apply status and the lease `preferred` hint are keyed on
// it. In Kubernetes use the pod name. A lease HOLDER is the opposite: see
// [NewIncarnation].
type Node struct {
	// ID is this process's identity. Empty resolves via CREWLET_NODE_ID
	// and then DefaultNodeID, so nothing has to be set to run one engine.
	ID string `yaml:"id,omitempty" json:"id,omitempty" js:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" desc:"Stable identity of this process. Empty reads CREWLET_NODE_ID, then defaults to node-0."`

	// Roles is what this process is willing to do: data, gitlet, ingress, seats,
	// workers. Omit it to run every role.
	//
	// `data` is the one role that is a promise about the DISK (see
	// [Bootstrap.ValidateRoles]); it says nothing about the broker, which the
	// stream block decides ([Bootstrap.BrokerKind]).
	//
	// Subtracting a role subtracts it from THIS node, never the company: a fleet
	// with no workers node runs no scheduler or retention sweep. No single node's
	// config shows that, so the engine checks live node presence at runtime.
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty" desc:"What this node does: data, gitlet, ingress, seats, workers. Omit for all five. A node without data keeps no durable state and needs store.scratch; it joins an embedded fleet as a leaf, through stream.leaf.urls, and runs neither ingress nor workers. gitlet needs ingress."`

	// Labels are free-form facts about where this process runs (zone: eu),
	// matched exactly by a seat's role.placement selector.
	//
	// At most placement.MaxLabels, each value at most placement.MaxLabelValueBytes
	// printable bytes ([placement.CheckLabelValue]): they ride the presence lease
	// on every heartbeat, whose size the store bounds. The js tag states the count
	// literally, held to the constant by TestTheSchemaBoundsLabelsWhereTheValidatorDoes;
	// the value's bound is the validator's alone, since a schema counts characters.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty" js:"maxkeys=16" desc:"Free-form node facts a seat's placement selector matches on: at most 16, each value at most 64 bytes of printable text."`

	// MaxConcurrent bounds how many agent turns this PROCESS runs at once. Zero (an
	// absent key) means node.DefaultMaxConcurrent, where the number and its rationale
	// live. Per node, hence Tier A: a fleet's ceiling is N × this. There is no
	// "unbounded"; zero is unset.
	//
	// It bounds the turn SEGMENTS this process runs itself (onboarding, a launch, the
	// collect, the review), not the executor runs between them: a turn suspends at its
	// launch and frees its slot, and the seats this node holds bound those.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0" desc:"Turn segments this process runs at once (a seat's executor run is not one of them: each seat runs at most one); 0 takes the engine default. Per node, so a fleet's ceiling is N times this."`
}

// nodeRoleNames is the vocabulary for the error message and the schema,
// derived from placement so config and the seat host cannot disagree.
var nodeRoleNames = []string{
	string(placement.RoleData),
	string(placement.RoleGitlet),
	string(placement.RoleIngress),
	string(placement.RoleSeats),
	string(placement.RoleWorkers),
}

func (n *Node) validate(path Path) error {
	var p problems

	// Zero is unset and takes the engine's default. A NEGATIVE is refused,
	// not silently defaulted: it is a value somebody typed.
	if n.MaxConcurrent < 0 {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (the engine default) or a positive number of turns, "+
				"got %d: there is no \"unbounded\"; write a large number for "+
				"effectively no limit", n.MaxConcurrent)
	}

	if n.ID != "" && !envref.Has(n.ID) && !nodeIDPattern().MatchString(n.ID) {
		// A reference is checked after resolution, in ResolveNodeID.
		p.add(at(path, "id"), ErrUnknownValue,
			"%q must start alphanumeric and contain only letters, digits, "+
				"'.', '_' or '-' (max 64 chars): it lands in log fields and "+
				"broker consumer names", n.ID)
	} else if reserved := reservedNodeID(n.ID); reserved != "" {
		p.add(at(path, "id"), ErrUnknownValue, "%s", reserved)
	}

	// An explicitly empty list is refused rather than read as "every role", as
	// placement reads a PEER's presence row: `roles: []` did not mean "do
	// everything".
	if n.Roles != nil && len(n.Roles) == 0 {
		p.add(at(path, "roles"), ErrMissing,
			"name at least one of %s: a node with no roles does nothing at "+
				"all. Omit the key to run every role, which is the "+
				"single-process default", words.Or(nodeRoleNames))
	}
	if _, err := placement.ParseRoles(n.Roles); err != nil {
		p.add(at(path, "roles"), ErrUnknownValue, "%s (want %s)",
			err, words.Or(nodeRoleNames))
	}

	// THE KEY GRAMMAR IS THE PLACEMENT VOCABULARY'S: a key accepted here and
	// refused there would be a label no seat could be placed by. Sorted, so bad
	// keys report in the same order every run.
	//
	// THE COUNT AND VALUES ARE BOUNDED TOO: every label rides this node's presence
	// lease, held to coord.MaxLeaseMetaBytes, so an unbounded map would be a
	// presence the store refuses (this node gone from its peers' count).
	if len(n.Labels) > placement.MaxLabels {
		p.add(at(path, "labels"), ErrOutOfRange,
			"%d labels, at most %d: every label is re-sent on each heartbeat of this "+
				"node's presence, whose size is bounded; keep the facts a role's "+
				"placement selects on", len(n.Labels), placement.MaxLabels)
	}
	for _, key := range slices.Sorted(maps.Keys(n.Labels)) {
		switch err := placement.CheckLabelKey(key); {
		case key == "":
			p.add(at(path, "labels"), ErrMissing, "label keys must not be empty")
		case err != nil:
			p.add(entry(at(path, "labels"), key), ErrUnknownValue, "%v", err)
		}
		if key == "" {
			continue
		}
		if err := placement.CheckLabelValue(n.Labels[key]); err != nil {
			p.add(entry(at(path, "labels"), key), ErrUnknownValue, "%v", err)
		}
	}
	return p.err()
}

// RoleSet is what this node declared; an unset list is every role.
func (n *Node) RoleSet() (placement.RoleSet, error) {
	return placement.ParseRoles(n.Roles)
}

// Profile is this node as its peers see it on its presence lease. The id is
// passed in because it is RESOLVED (config, environment, default).
//
// THE BOOTSTRAP'S, NOT THE NODE BLOCK'S: the broker kind is the STREAM block's.
// The object store share (store.objects) rides the object store's own lease,
// never presence: see [placement.NodeProfile].
func (b *Bootstrap) Profile(id string) placement.NodeProfile {
	roles, _ := placement.ParseRoles(b.Node.Roles) // validated already
	labels := make(map[string]string, len(b.Node.Labels))
	for k, v := range b.Node.Labels {
		labels[k] = v
	}
	if len(labels) == 0 {
		labels = nil
	}
	return placement.NodeProfile{
		ID: id, Roles: roles, Labels: labels, Broker: b.BrokerKind(),
	}
}

// BrokerKind is how this node's broker takes part in the fleet's, derived from
// the stream block alone: `type: nats` is a client of another's cluster, an
// embedded broker joining through `leaf.urls` is a leaf, and every other embedded
// broker is a member, a solo node's included.
//
// THE ONE DERIVATION, so the broker rules, the advertised kind and the broker
// started cannot disagree. Never a setting.
func (b *Bootstrap) BrokerKind() placement.BrokerKind {
	switch {
	case b.Stream.Type == StreamNATS:
		return placement.BrokerClient
	case b.Stream.Leaf.Joins():
		return placement.BrokerLeaf
	default:
		return placement.BrokerMember
	}
}

// ResolveNodeID answers what this process calls itself.
//
// Precedence: node.id (resolved through r), then CREWLET_NODE_ID, then
// DefaultNodeID. A value failing the pattern is rejected HERE, not later as a
// malformed consumer name.
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
	if !nodeIDPattern().MatchString(value) {
		return "", fault(field("node.id"), ErrUnknownValue,
			"%q must start alphanumeric and contain only letters, digits, "+
				"'.', '_' or '-' (max 64 chars)", value)
	}
	if reserved := reservedNodeID(value); reserved != "" {
		return "", fault(field("node.id"), ErrUnknownValue, "%s", reserved)
	}
	return value, nil
}

// NewIncarnation mints a holder identity for one running process:
// "{nodeID}:{random}".
//
// The opposite of [ResolveNodeID]: a node id names a PLACEMENT; a lease holder
// names one INCARNATION. A lease renews by owner string, so a draining
// predecessor's identity would hold the seat beside it at one fencing epoch.
//
// No process-global cache: two engines in one process are two holders. Each calls
// this once, since a second mid-run fences it out of its own seats. The uuid is
// carried WHOLE: 32 bits collide at tens of thousands.
func NewIncarnation(nodeID string) string {
	return nodeID + incarnationSep + uuid.NewString()
}

// incarnationSep joins a node's id to an incarnation's own suffix. No node id
// holds it ([nodeIDPattern]), so the cut at its first occurrence is exact.
const incarnationSep = ":"

// IncarnationNode is the node an incarnation belongs to: the stable id
// [NewIncarnation] wrote before the suffix. ONE CUT, beside the one join, for
// every reader that holds a lease's owner and needs its node.
func IncarnationNode(owner string) string {
	node, _, _ := strings.Cut(owner, incarnationSep)
	return node
}

// ---- store ----------------------------------------------------------- //

// DefaultStorePath is where a company with nothing configured keeps its
// database, relative to the working directory.
const DefaultStorePath = "crewlet.db"

// Store is the local database this node materializes the stream into.
//
// A FILE owned exclusively by this process: two engines on one corrupt it, and a
// fleet's nodes each keep their own copy. No driver field: Turso is the only one
// (ADR-0007).
type Store struct {
	// Path is the database file. Created if absent, along with its parent.
	Path string `yaml:"path,omitempty" json:"path,omitempty" desc:"Local database file this node owns exclusively."`

	// SnapshotDir is where this node keeps its snapshots of the replicated estate,
	// which a joining peer copies instead of replaying the log. NOT UNDER stream.:
	// it is a disk fact. Absolute, or relative to the store's directory.
	//
	// THE DEFAULT PUTS A FULL COPY OF THE ESTATE ON THE LIVE DATABASE'S VOLUME, so
	// the snapshot loop refuses on low free space rather than fill the disk the
	// applier commits to. A separate volume is the production shape.
	SnapshotDir string `yaml:"snapshot_dir,omitempty" json:"snapshot_dir,omitempty" desc:"Where this node keeps snapshots of the replicated estate; empty is <dir of path>/snapshots."`

	// ReplicatedPath is where the replicated estate lives on this node: everything
	// a state log's applier writes. Empty puts it beside Path, which makes "back up
	// the data directory" true. Separable because a snapshot copies it and a joining
	// node writes it at line rate.
	ReplicatedPath string `yaml:"replicated_path,omitempty" json:"replicated_path,omitempty" desc:"Where the replicated estate's database file lives; empty puts it beside path."`

	// MaxOpenConns is the ORDINARY READERS of the node's database's pool, and of
	// the replicated estate's, which keeps at least two. What ordinary work may not
	// take (each file's identity connection, the estate's one pinned writer per
	// state log) is ADDED on top: taken out of the bound, it would queue a read
	// burst, or at or below the pin count leave the last apply loop waiting for a
	// connection none gives back. 0 takes max(8, GOMAXPROCS): two dashboards' query
	// concurrency on a small host, a reader per core on a large one.
	MaxOpenConns int `yaml:"max_open_conns,omitempty" json:"max_open_conns,omitempty" js:"min=0" desc:"Ordinary readers of the node's database pool, and of the replicated estate's (at least two there); the identity reserve and one pinned writer per log are added on top. 0 takes the store default, max(8, GOMAXPROCS)."`

	// BusyTimeoutSeconds is how long a statement waits for the file lock
	// before giving up; 0 takes the store's own default.
	//
	// ONE KNOB FOR BOTH HALVES OF THE SAME WAIT: it bounds a write's place in the
	// store's own queue too, and the replacement connection a pinned writer draws
	// after a transaction that did not end (internal/store's writequeue.go).
	BusyTimeoutSeconds float64 `yaml:"busy_timeout_seconds,omitempty" json:"busy_timeout_seconds,omitempty" js:"min=0" desc:"Lock wait before a write gives up, whether it waits in the driver or in the store's own queue, and the anchor for the one retry it then gets; 0 takes the store default."`

	// Scratch declares this node's store DISPOSABLE: the engine deletes whatever is
	// at `path` every boot, under the store's own lock, before opening it fresh.
	//
	// REQUIRED on a node without the `data` role and REFUSED on one with it: a data
	// node would lose the company's history to a restart. Explicit rather than
	// derived from roles because it is the one setting here that deletes something:
	// removing `data` must be said twice.
	Scratch bool `yaml:"scratch,omitempty" json:"scratch,omitempty" desc:"Delete this node's store at every boot. Required on a node without the data role, refused on one with it."`

	// Objects is where the company's files are kept — see [StoreObjects].
	// Every node carries the same block, data or not.
	Objects StoreObjects `yaml:"objects,omitempty" json:"objects,omitzero"`
}

func (s *Store) validate(path Path) error {
	var p problems
	if s.Path == "" {
		p.add(at(path, "path"), ErrMissing,
			"the node's own store is a local file this node owns; name one "+
				"(e.g. %q)",
			DefaultStorePath)
	}
	// THE SAME FILE TWICE would migrate one estate's schema into the other's
	// database and run both appliers against one file.
	if s.ReplicatedPath != "" && s.ReplicatedPath == s.Path {
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
	p.wrap(s.Objects.validate(at(path, "objects")))
	return p.err()
}

// BusyTimeout is the lock wait; zero means the store's default.
func (s *Store) BusyTimeout() time.Duration {
	return time.Duration(s.BusyTimeoutSeconds * float64(time.Second))
}

// ---- stream ---------------------------------------------------------- //

// StreamType is the stream slot: where the durable event log lives.
type StreamType string

const (
	// StreamEmbedded runs a NATS JetStream server inside this process; solo, it
	// has no socket at all.
	StreamEmbedded StreamType = "embedded"
	// StreamNATS dials an external NATS cluster.
	StreamNATS StreamType = "nats"
)

// StreamTypes is the closed set.
var StreamTypes = []StreamType{StreamEmbedded, StreamNATS}

// Stream is the durable event log every node writes through.
type Stream struct {
	// Type selects the slot. Default embedded.
	Type StreamType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=embedded|nats" desc:"embedded (default) or nats."`

	// URL is the external server to dial. Required for nats, refused for
	// embedded, where nobody would read it.
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"External NATS URL. Required for nats, refused for embedded."`

	// StoreDir is where an EMBEDDED broker MEMBER persists its streams, REQUIRED on
	// one, solo included ([Stream.Durable]): empty selects an in-memory server, but a
	// member keeps every domain's state logs on its stream from boot, so a restart
	// would recreate each empty and the node would refuse to serve for good. In a
	// fleet it also holds the streams of every node reaching it, leaves included.
	//
	// REFUSED ON A LEAF, which runs no JetStream and so has no stream store
	// ([StreamLeaf.validate]); an external client's streams are its cluster's.
	StoreDir string `yaml:"store_dir,omitempty" json:"store_dir,omitempty" desc:"Embedded stream persistence directory. Required on an embedded broker member, solo included: it keeps every state log from boot. Refused on a leaf."`

	// StoreMaxBytes is how much of that directory's volume this node's EMBEDDED
	// broker may hold: the ONE number every stream ceiling on it is bounded by,
	// divided by an operator when several engines share a filesystem.
	//
	// DECLARED, NOT INFERRED: a stream's ceiling is a RESERVATION the broker refuses
	// unless this limit backs it, and engines sizing from FREE SPACE each believed
	// they had the same space though the disk bounds their SUM (`insufficient
	// storage resources available`). UNSET, the broker takes three quarters of
	// `store_dir`'s free space plus what it occupies, READ BACK once at boot.
	StoreMaxBytes int64 `yaml:"store_max_bytes,omitempty" json:"store_max_bytes,omitempty" js:"min=6442450944;max=70368744177664" desc:"How much of store_dir's volume the EMBEDDED broker may hold; unset lets it take three quarters of that volume's free space at boot. Divide it when several engines share one filesystem."`

	// Cluster makes the embedded server a MEMBER of a cluster with its peers.
	Cluster StreamCluster `yaml:"cluster,omitempty" json:"cluster,omitzero"`

	// Leaf is how a node holding no data reaches the fleet's broker. See [StreamLeaf].
	Leaf StreamLeaf `yaml:"leaf,omitempty" json:"leaf,omitzero"`

	// Replicas is the stream replica count: 1 solo, 3 in a fleet, where it makes
	// a publish quorum-durable before it returns. It is the copies of every stream
	// AND of every coordination bucket.
	//
	// AT MOST [MaxStreamReplicas], and on an embedded cluster at most the members
	// it names. On the default `nats` object store it is also the copies of the
	// company's FILES, one more stream (ADR-0026); an `s3` bucket keeps whatever
	// its provider promises.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty" js:"min=0;max=5" desc:"Copies of every stream and coordination bucket, and of the company's files on the default nats object store, 1..5: 1 solo, 3 in a fleet, and on an embedded cluster at most its members."`

	// EventRetentionHours bounds the event stream. 0 takes the queue's default.
	// Unbounded is not expressible: nothing would ever sweep it.
	//
	// AT LEAST A QUARTER OF AN HOUR otherwise: the dead letters are kept by the
	// same age, and the broker refuses a stream age shorter than
	// [queue.DeadLetterWindow].
	EventRetentionHours float64 `yaml:"event_retention_hours,omitempty" json:"event_retention_hours,omitempty" js:"min=0" desc:"Event stream retention; 0 takes the queue default, otherwise at least 0.25 (the dead letters' fifteen-minute duplicate window)."`

	// Debug turns on the EMBEDDED broker's own debug logging.
	//
	// SEPARATE FROM `logging.level`: `-debug` would otherwise subscribe the operator
	// to nats-server's per-client lifecycle trace (a KV key listing writes two
	// "JetStream connection closed" lines, and duty loops list keys every 15 s).
	// It does NOT gate the broker's warnings and errors. Its lines are DEBUG, so they
	// need a destination at `debug`; [Bootstrap.Warnings] says so when none is.
	Debug bool `yaml:"debug,omitempty" json:"debug,omitempty" desc:"Turn on the EMBEDDED broker's own debug logging. Separate from logging.level, which is the engine's; refused for type: nats. Its warnings and errors are never gated by this."`

	// Credentials is a path to a NATS credentials file.
	//
	// type: nats needs one of Credentials, Token, TLS.Cert or a userinfo in URL
	// ([Stream.presentsCredential]).
	Credentials string `yaml:"credentials,omitempty" json:"credentials,omitempty" desc:"Path to a NATS credentials file for an external server. type: nats needs one of credentials, token, tls.cert or a credential in url."`

	// Token is a bearer token presented to an external server.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Bearer token for an external server; ${VAR} supported. type: nats needs one of credentials, token, tls.cert or a credential in url."`

	// TLS is the transport for an external NATS server: a private CA to trust and
	// a client certificate to present. Without it a hardened broker (`tls {
	// verify: true }` REQUIRES a client certificate) is unreachable.
	TLS NATSTLS `yaml:"tls,omitempty" json:"tls,omitzero"`

	// Sync decides what an acknowledged publish has reached: the one Tier A field
	// that changes what durability MEANS. `always` fsyncs every write first.
	// `<duration>` declines the fsync and names the most an acknowledged write can
	// be behind the disk. Unset takes `always`.
	//
	// NOT INFERRED FROM replicas: "a quorum instead of a disk" holds when one host
	// loses power and not when a rack does. Three copies of one unflushed page cache
	// is one copy.
	Sync string `yaml:"sync,omitempty" json:"sync,omitempty" desc:"always (default) fsyncs every write before acknowledging it; a duration (30s) declines the fsync and names the window an acknowledged write may be behind the disk. A duration is refused for type: nats and on a leaf, which have no file store of this node's to configure."`

	// TrackerLogMaxBytes is the byte ceiling on the mutation log.
	//
	// UNSET DERIVES IT from the volume the streams live on (StoreDir's with an
	// embedded broker, else Store.Path's: an external cluster's disk is not
	// measurable): a quarter of its free space, clamped to 4 GiB..64 GiB (boot logs
	// the source, `statelog_ceilings`). State logs share one budget, so derived
	// ceilings scale down together (engine.StreamBudgetShare); a set value never
	// does, and a boot that cannot reserve it names this field.
	// It is the value the stream is CREATED with, then a DECLARATION: re-applying
	// at boot would let restart order decide a shared limit. CROSSING IT REFUSES
	// THE APPEND, never sheds history.
	TrackerLogMaxBytes int64 `yaml:"tracker_log_max_bytes,omitempty" json:"tracker_log_max_bytes,omitempty" js:"min=1073741824;max=1099511627776" desc:"Byte ceiling on the mutation log; unset derives a quarter of the free space on the volume the streams live on - stream.store_dir where an embedded broker has one, store.path otherwise - clamped to 4 GiB..64 GiB."`

	// TrackerVectorsMaxBytes is the byte ceiling on the vector changelog.
	//
	// SIZED FOR THE PEAK, NOT THE STEADY STATE (93× apart): a week's minting is about
	// 91 MB at the reference company, but changing the embedding model or its width
	// rewrites EVERY source in hours: 8.46 GB at its fifth year (1.69 GB per 100
	// seats per year). Steady-state sizing would refuse the one operation this
	// field exists to survive.
	//
	// UNSET DERIVES FROM THE VOLUME as the mutation log does, never from that log's
	// ceiling, which bounds a trailing window. The 64 GiB clamp holds a model change
	// twice over for about 2 000 seat-years of corpus; a larger company sets it, at
	// about 34 MB per agent seat per year.
	TrackerVectorsMaxBytes int64 `yaml:"tracker_vectors_max_bytes,omitempty" json:"tracker_vectors_max_bytes,omitempty" js:"min=1073741824;max=2199023255552" desc:"Byte ceiling on the vector changelog, sized for a model change rather than the steady state; unset derives what the mutation log derives from the volume, a quarter of its free space clamped to 4 GiB..64 GiB, whatever tracker_log_max_bytes is set to."`

	// PagesLogMaxBytes is the byte ceiling on Doclet's log.
	//
	// UNSET DERIVES IT at a quarter of the mutation log's ceiling as set or derived
	// (1 GiB..16 GiB from a volume alone): a few thousand pages against a tracker's
	// hundreds of thousands of items grow the log at about a quarter of the rate.
	// Budget and refusals are TrackerLogMaxBytes's.
	PagesLogMaxBytes int64 `yaml:"pages_log_max_bytes,omitempty" json:"pages_log_max_bytes,omitempty" js:"min=1073741824;max=274877906944" desc:"Byte ceiling on the knowledge base's log; unset derives a quarter of the mutation log's ceiling, set or derived, 1 GiB..16 GiB from a volume alone."`

	// IamLogMaxBytes is the byte ceiling on the identity estate's log.
	//
	// UNSET TAKES A FLAT 512 MiB, not derived from the disk: this log grows with
	// HEADCOUNT and sign-ins (a session is one record at open and one at close);
	// at the reference company's three a day that is about 285 MB a year, so the
	// default is eighteen months of a COMPLETELY BLOCKED trim.
	//
	// THE FLOOR IS 64 MiB: the broker grants the whole ceiling at creation, and a
	// ten-person company should not need half a gibibyte free to boot. The ceiling is
	// a TYPO GUARD. Shedding here would shed the authentication trail.
	IamLogMaxBytes int64 `yaml:"iam_log_max_bytes,omitempty" json:"iam_log_max_bytes,omitempty" js:"min=67108864;max=17179869184" desc:"Byte ceiling on the identity estate's log; unset takes a flat 512 MiB, 64 MiB..16 GiB."`

	// UsageLogMaxBytes is the byte ceiling on the usage log, the compacted stream
	// every data node publishes its company days to, so spend history outlives the
	// node that spent it.
	//
	// SIZED BY A CENSUS, NOT A RATE: one message per (node, day, seat, schedule or
	// person) for 181 days: a three-node fleet of forty seats and twenty schedules
	// is about 320 MB, the 1 GiB default three times that. Unset takes the default,
	// not a disk share. THE FLOOR IS 64 MiB: a ten-seat node's census is about 18 MB
	// ([UsageLogMaxBytesFloor]).
	UsageLogMaxBytes int64 `yaml:"usage_log_max_bytes,omitempty" json:"usage_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on the usage log, the compacted stream each data node's company days replicate on; default 1 GiB, sized for 181 days of a fleet's seats and schedules, 64 MiB..64 GiB."`

	// ArtifactsLogMaxBytes is the byte ceiling on the artifacts log.
	//
	// UNSET DERIVES IT at a quarter of the mutation log's ceiling as set or derived,
	// for the knowledge base's reason: a publish is a human-scale event, so the log
	// grows at about a page's rate. Crossing it refuses the append.
	ArtifactsLogMaxBytes int64 `yaml:"artifacts_log_max_bytes,omitempty" json:"artifacts_log_max_bytes,omitempty" js:"min=1073741824;max=274877906944" desc:"Byte ceiling on the artifacts log; unset derives a quarter of the mutation log's ceiling, set or derived, 1 GiB..16 GiB from a volume alone."`

	// ArtifactDataLogMaxBytes is the byte ceiling on the datastore's log, where
	// every sheet edit is one record.
	//
	// UNSET DERIVES A QUARTER of the mutation log's ceiling, the largest share the
	// state logs' pool can give it beside a vector changelog that holds a model
	// change ([DerivedArtifactDataLogDivisor]). Thirty people on one review sheet
	// write five to twenty records a second, so a heavy-writing company sets it, up
	// to twice the artifacts log's bound.
	ArtifactDataLogMaxBytes int64 `yaml:"artifact_data_log_max_bytes,omitempty" json:"artifact_data_log_max_bytes,omitempty" js:"min=1073741824;max=549755813888" desc:"Byte ceiling on the artifact datastore's log; unset derives a quarter of the mutation log's ceiling, set or derived, 1 GiB..16 GiB from a volume alone."`

	// CodeLogMaxBytes is the byte ceiling on the code log.
	//
	// UNSET DERIVES IT at two thirds of the mutation log's ceiling, clamped to
	// 1 GiB..1 TiB ([DerivedCodeLogNumerator]). By the byte rate, two thirds
	// is the least of what a company whose seats only push writes here, and BELOW what
	// a company whose seats work through changes writes: its push, compaction, barrier
	// and change records take nearly twice to about eight times what its tracker
	// writes and its seats' reads take a day. Before the first boot neither log
	// exists, so it sets this field from the reference company's day: twice the
	// mutation log's ceiling, that day's least. Every company pays its ask: below
	// about 653 GiB free it comes out of the other logs' share (at 100 GiB the
	// mutation log is created at 9.0 GiB rather than 11.0; docs/guides/replication.md).
	CodeLogMaxBytes int64 `yaml:"code_log_max_bytes,omitempty" json:"code_log_max_bytes,omitempty" js:"min=1073741824;max=1099511627776" desc:"Byte ceiling on the code log, the ordered stream a repository's refs, packs and settings move on; unset derives two thirds of the mutation log's ceiling, set or derived, 1 GiB..1 TiB. Reserved whether or not the company hosts code: one that hosts none and is short of broker room sets 1073741824 before the first boot."`

	// AssistantLogMaxBytes is the byte ceiling on the Dashboard Assistant's log.
	//
	// UNSET TAKES 512 MiB ([DefaultAssistantLogMaxBytes]), sized by how many people
	// ask the assistant: about two weeks of a completely blocked trim at the
	// reference company's rate. A short broker scales it down to its 64 MiB floor.
	// Crossing it refuses the append.
	AssistantLogMaxBytes int64 `yaml:"assistant_log_max_bytes,omitempty" json:"assistant_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on Ask Crewlet's log; unset takes 512 MiB, scaled down to 64 MiB where the broker is short, 64 MiB..64 GiB."`

	// ChatLogMaxBytes is the byte ceiling on the chat log, the ordered stream every
	// Chatlet message, reaction and membership change goes through.
	// UNSET ASKS FOR 8 GiB, capped at a quarter of what an unset mutation log derives
	// ([DerivedChatLogDivisor]) and never below [ChatLogMaxBytesFloor]; the ask is then
	// scaled with the other derived asks to the one budget. It does NOT follow the
	// mutation log's ceiling: chat grows faster (about 32 MB a day at the census), so
	// that ratio made it the largest ask and starved the vector changelog of room for
	// a model change. 8 GiB is not the steady state (about 225 MB) but what a
	// COMPLETELY BLOCKED TRIM may spend before appends refuse, about eight months at
	// 20 000 messages a day; size this to match a longer tracker_log_max_bytes horizon.
	// Refuses, never sheds: pruning is a RECORD every data node applies.
	ChatLogMaxBytes int64 `yaml:"chat_log_max_bytes,omitempty" json:"chat_log_max_bytes,omitempty" js:"min=1073741824;max=274877906944" desc:"Byte ceiling on the chat log; unset asks for 8 GiB, capped at a sixteenth of the stream volume's free space (a quarter of what an unset tracker_log_max_bytes derives, so the whole 8 GiB is asked from 128 GiB free), 1 GiB..256 GiB, and does not follow tracker_log_max_bytes - size it for the same blocked-trim horizon, about 32 MB a day at the declared census."`

	// ExecutorLogMaxBytes is the byte ceiling on the executor log of seats'
	// executor sessions.
	//
	// UNSET TAKES 512 MiB ([DefaultExecutorLogMaxBytes]), sized by the turns the
	// seats run: about seventeen days of a completely blocked trim at the reference
	// company's rate. A short broker scales it down to its 64 MiB floor. Crossing it
	// refuses the append (the next launch starts fresh).
	ExecutorLogMaxBytes int64 `yaml:"executor_log_max_bytes,omitempty" json:"executor_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on the executor log, where seats' executor sessions are named; unset takes 512 MiB, scaled down to 64 MiB where the broker is short, 64 MiB..64 GiB."`

	// AccessLogMaxBytes is the byte ceiling on the access log.
	//
	// UNSET TAKES 128 MiB ([DefaultAccessLogMaxBytes]), sized by the records the
	// seats write, scaled down to its 64 MiB floor where the broker is short.
	// Crossing it refuses the append: a turn is told nothing new and an SSH command
	// is refused unrecorded.
	AccessLogMaxBytes int64 `yaml:"access_log_max_bytes,omitempty" json:"access_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on the access log, where what each seat was told about its access and every SSH certificate and command are recorded; unset takes 128 MiB, scaled down to 64 MiB where the broker is short, 64 MiB..64 GiB."`

	// AtlasLogMaxBytes is the byte ceiling on the atlas log.
	//
	// UNSET TAKES 128 MiB ([DefaultAtlasLogMaxBytes]), sized by what people record,
	// scaled down to its 64 MiB floor where the broker is short. Crossing it refuses
	// a person's edit.
	AtlasLogMaxBytes int64 `yaml:"atlas_log_max_bytes,omitempty" json:"atlas_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on the atlas log, where what people record about what the company runs on is kept with its history; unset takes 128 MiB, scaled down to 64 MiB where the broker is short, 64 MiB..64 GiB."`

	// AdvisorLogMaxBytes is the byte ceiling on the advisor log.
	//
	// UNSET TAKES 64 MiB ([DefaultAdvisorLogMaxBytes]), sized by what the Advisor
	// records. Crossing it refuses the append: the Advisor records nothing new and a
	// person's decision on a finding is refused.
	AdvisorLogMaxBytes int64 `yaml:"advisor_log_max_bytes,omitempty" json:"advisor_log_max_bytes,omitempty" js:"min=67108864;max=68719476736" desc:"Byte ceiling on the advisor log, where the Advisor's findings, runs, reports and alarm episodes are kept with their history; unset takes 64 MiB, 64 MiB..64 GiB."`

	// TrackerRetention is when the log may be trimmed; the trim stops, deliberately,
	// when a term it depends on is unknown.
	TrackerRetention TrackerRetention `yaml:"tracker_retention,omitempty" json:"tracker_retention,omitzero"`
}

// TrackerRetention is the operator's half of the log trim.
//
// Tier A, not the company's config: every term is about the OPERATOR's estate,
// and Tier B is edited live, so lowering a durability gate there would move it
// under a trim that had already computed against it.
//
// The duration fields carry a Raw suffix: the field holds the operator's text and
// the plainly named method holds the [time.Duration].
type TrackerRetention struct {
	// MinAgeRaw is the age floor no trim may cross, whatever the other terms say:
	// a LOWER bound on how long the log keeps a record, never a ceiling. Seven days
	// is the horizon the fleet already treats as how long a node may be away.
	//
	// THE 24-HOUR FLOOR HAS THREE REASONS: a value below a day cannot outlast a
	// nightly backup cycle; it is the margin that keeps a quiet object writable (one
	// whose last record was trimmed must be recognised as trimmed, not absent); and
	// it is the age bound on the vector changelog, so lowering it shortens the window
	// a joining node's vector gap is refilled from.
	MinAgeRaw string `yaml:"min_age,omitempty" json:"min_age,omitempty" desc:"Age floor no trim may cross (default 7d, 24h..90d)."`

	// BackupMaxAge is how stale the newest complete backup may be before the trim
	// stops entirely.
	//
	// A COMPANY THAT NEVER BACKS UP NEVER TRIMS: trimming past the newest backup
	// deletes the last thing that could rebuild the log. A day is the cadence of a
	// nightly backup against a trim that runs every fifteen minutes, so a working
	// nightly never notices and a broken one stops the trim within a day.
	BackupMaxAgeRaw string `yaml:"backup_max_age,omitempty" json:"backup_max_age,omitempty" desc:"How stale the newest backup may be before the trim stops (default 24h, 1h..30d)."`

	// BackupFloor is whose word the trim takes for what is backed up.
	//
	// PER LOG, under either value: each log's trim follows the newest counted
	// backup that covers THAT log. `engine` counts the backups the engine wrote and
	// verified. `operator` counts an explicit per-log acknowledgement, for a policy
	// of "trim only what is off-site", which the engine cannot see. Under
	// `operator` a log's trim does not advance until it has been acknowledged once,
	// a state warned about.
	BackupFloor BackupFloor `yaml:"backup_floor,omitempty" json:"backup_floor,omitempty" js:"enum=engine|operator" desc:"Whose word the trim takes for what is backed up: engine (default) or operator."`

	// SnapshotInterval is how stale a node's newest snapshot may be before it
	// takes another. A day rather than six hours: a snapshot is a full copy of the
	// replicated estate (tens of gigabytes at a mature company), so four a day is
	// a day's I/O to save a joining node a replay of under a minute.
	SnapshotIntervalRaw string `yaml:"snapshot_interval,omitempty" json:"snapshot_interval,omitempty" desc:"How stale a node's newest snapshot may be before it takes another (default 24h, 1h..7d)."`

	// RejoinWindow is the operator's budget for a node to become a complete
	// replica, what a join is measured against. A SETTING because it is a property
	// of the operator's disks and network, which differ more than twice over.
	RejoinWindowRaw string `yaml:"rejoin_window,omitempty" json:"rejoin_window,omitempty" desc:"Budget for a node to become a complete replica (default 30m, 5m..24h)."`
}

// BackupFloor is whose word the trim takes for what is backed up.
type BackupFloor string

const (
	// BackupFloorEngine follows, per log, the newest backup the engine wrote and
	// verified.
	BackupFloorEngine BackupFloor = "engine"

	// BackupFloorOperator follows, per log, an explicit acknowledgement.
	BackupFloorOperator BackupFloor = "operator"
)

// BackupFloors is the closed set.
var BackupFloors = []BackupFloor{BackupFloorEngine, BackupFloorOperator}

// StreamSyncAlways is [Stream.Sync]'s strong value.
const StreamSyncAlways = "always"

// SyncAlways reports whether every write is fsynced before it is acknowledged;
// unset means yes ([Stream.Sync]).
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
	// Peers are the OTHER members' route URLs, each host:port under a scheme
	// (nats://node-b.internal:6222): every rule that counts the cluster counts this
	// list. An entry recognisably this node's own route, or a repeat, is discounted
	// with a warning ([StreamCluster.members]); one reaching this node under an
	// unrecognisable name is counted as a member it is not.
	Peers []string `yaml:"peers,omitempty" json:"peers,omitempty" desc:"Route URLs of the OTHER members, e.g. nats://node-b.internal:6222. An entry that is this node's own route (cluster.host and port, or cluster.advertise) or a repeat is not counted as a member."`

	// Host is the interface the route listener binds. Empty binds every one.
	//
	// The listener admits only a member presenting a route credential derived from
	// a key of secrets.keys (ADR-0094), and that credential crosses the network IN
	// CLEAR once per connection (NATS routes carry a password or a certificate,
	// never a signature), as does every byte after it: the whole event history and
	// every lease. On a host with a public interface, empty publishes both. Set it
	// to the private address the peers reach.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Interface the route listener binds. Empty binds every interface. The listener admits only members presenting a key derived from secrets.keys; a route's credential and every byte after it cross the network in clear, so bind it to the private network the peers are on."`

	// Advertise is the address peers should dial for this member when it differs
	// from what it binds (a mapped container port, a NAT, a load balancer).
	//
	// A member's address TRAVELS: peers dial what the members they are connected to
	// tell them. Unset, it is derived from the connection's remote address, which
	// on a NAT'd host is unreachable or somebody else's. Host and port, or a bare
	// host to keep this member's own route port.
	Advertise string `yaml:"advertise,omitempty" json:"advertise,omitempty" desc:"host:port peers should dial for this member, when it differs from what it binds."`
}

// StreamLeaf is the embedded broker's side of a LEAF link: a leaf joins
// through it, and a member opens it for the leaves.
//
// ONE BLOCK, TWO SIDES: `urls` makes this node's broker a LEAF
// ([Bootstrap.BrokerKind]) and `port` opens a MEMBER's listener; a node is exactly
// one. A leaf runs no JetStream, holds no replica and is in no quorum. Only a node
// without `data` may be a leaf ([Bootstrap.ValidateRoles]).
type StreamLeaf struct {
	// URLs are the leaf listeners of the members this node may join, any one
	// of which will do. Setting them makes this node's broker a leaf.
	URLs []string `yaml:"urls,omitempty" json:"urls,omitempty" desc:"Leaf listeners of the members this node joins as a LEAF - no JetStream, no replica, no vote - e.g. nats-leaf://member-a.internal:7422. Any one that answers will do. Only a node without the data role may be a leaf, and an embedded node without it must be one."`

	// Port is the leaf listener a MEMBER opens for the fleet's leaves. Zero
	// opens none.
	//
	// It admits only a leaf whose link presents the nkey derived from a key of
	// secrets.keys and signs this member's nonce with it (ADR-0094), so no secret
	// crosses the network. What crosses after, everything the leaf's clients ask of
	// the fleet, is in clear, as the route port's is: both belong on the operator's
	// own network.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"A member's leaf listener, where the fleet's leaves join. Admits only a leaf whose key is derived from secrets.keys. A member with one is in a fleet, so it needs stream.store_dir."`

	// Host is the interface the leaf listener binds, empty for every one: the
	// exposure [StreamCluster.Host] names. A link is authenticated, never encrypted.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Interface the leaf listener binds. Empty binds every interface. A leaf's link is authenticated, not encrypted, so bind it to the private network the leaves are on."`

	// Advertise is the address leaves should dial for this member when it
	// differs from what it binds.
	Advertise string `yaml:"advertise,omitempty" json:"advertise,omitempty" desc:"host:port leaves should dial for this member, when it differs from what it binds."`
}

// validate refuses a leaf block that is neither side of a link, or both.
//
// Which roles may take which side is [Bootstrap.ValidateRoles]', and what every
// member has to persist is [Stream.Durable]'s.
func (l *StreamLeaf) validate(path Path, s *Stream, external bool) error {
	var p problems
	if l.IsZero() {
		return nil
	}
	if external {
		p.add(path, ErrConflict,
			"stream.leaf configures the EMBEDDED broker's leaf link, and a node "+
				"dialling an external %q cluster is already a plain client of it: "+
				"a node without the data role needs nothing more. Remove it, or "+
				"set type to %q", StreamNATS, StreamEmbedded)
		return p.err()
	}
	if l.Port < 0 || l.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange, "must be 0..65535, got %d", l.Port)
	}
	if l.Joins() {
		// ONE SIDE OF THE LINK: a leaf dials; a member's settings do not apply.
		if l.Port != 0 || l.Host != "" || l.Advertise != "" {
			p.add(at(path, "port"), ErrConflict,
				"port, host and advertise are a MEMBER's leaf listener, and "+
					"urls joins this node as a leaf, and a node is one side of "+
					"the link")
		}
		if !s.Cluster.IsZero() {
			p.add(field("stream.cluster"), ErrConflict,
				"a leaf is not a cluster member: it runs no JetStream and "+
					"holds no replica. Remove the cluster block, or the leaf urls")
		}
		if s.StoreDir != "" || s.StoreMaxBytes != 0 {
			p.add(field("stream.store_dir"), ErrConflict,
				"a leaf runs no JetStream, so it has no stream store to keep")
		}
		for i, raw := range l.URLs {
			if carriesCredential(raw) {
				// NAMING NO PART OF IT: the sentence would print the credential.
				p.add(at(path, fmt.Sprintf("urls[%d]", i)), ErrConflict,
					"carries a credential, and a fleet's credentials are derived "+
						"from secrets.keys: remove the user:pass@, because the link presents "+
						"this node's own key, and a userinfo would be sent beside it "+
						"in clear")
				continue
			}
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Host == "" || !slices.Contains(leafSchemes, u.Scheme) {
				p.add(at(path, fmt.Sprintf("urls[%d]", i)), ErrUnknownValue,
					"%q is not a member's leaf listener: want %s://host:port",
					withoutUserinfo(raw), leafSchemes[0])
			}
		}
		return p.err()
	}
	if l.Port == 0 {
		p.add(at(path, "port"), ErrMissing,
			"is required once host or advertise is set: they describe a leaf "+
				"LISTENER, and without a port this member opens none")
	}
	if l.Advertise != "" {
		if err := validateAdvertise(l.Advertise); err != nil {
			p.add(at(path, "advertise"), ErrShape, "%v", err)
		}
	}
	return p.err()
}

// leafSchemes are the URL schemes a leaf link may be dialled with.
var leafSchemes = []string{"nats-leaf", "nats", "tls"}

// IsZero lets an unset leaf block drop out of a JSON round trip.
func (l StreamLeaf) IsZero() bool {
	return len(l.URLs) == 0 && l.Port == 0 && l.Host == "" && l.Advertise == ""
}

// Joins reports whether this block joins the fleet as a leaf.
func (l StreamLeaf) Joins() bool { return len(l.URLs) > 0 }

// IsZero lets an unset cluster block drop out of a JSON round trip.
func (c StreamCluster) IsZero() bool {
	return c.Name == "" && c.Port == 0 && len(c.Peers) == 0 &&
		c.Host == "" && c.Advertise == ""
}

// Durable refuses an embedded broker MEMBER with nowhere to persist, naming
// `stream.store_dir`, and passes every other stream.
//
// With no store directory the streams live in MEMORY, so a restart recreates every
// state log empty and a node whose durable tables are ahead of them cannot tell
// "trimmed" from "a different log", and stays refused. Every member runs every
// domain's log from boot, solo included; a LEAF's streams are a member's and a
// CLIENT's are its cluster's. Asked by [Bootstrap.Validate] and the engine's door.
func (s *Stream) Durable() error { return s.durable(field("stream")) }

// durable is [Stream.Durable] at a caller's path.
func (s *Stream) durable(path Path) error {
	if s.Type == StreamNATS || s.Leaf.Joins() || strings.TrimSpace(s.StoreDir) != "" {
		return nil
	}
	var p problems
	p.add(at(path, "store_dir"), ErrMissing,
		"this node's broker is a MEMBER, and a member keeps the estate's state "+
			"logs on this stream from boot (the identity estate among them, "+
			"whatever company it runs, or none), and in a fleet the seats' "+
			"mailboxes and every coordination bucket too. An embedded stream with "+
			"no store directory keeps its streams in memory: a restart recreates "+
			"every log empty and this node refuses to serve them permanently. "+
			"Name a directory")
	return p.err()
}

func (s *Stream) validate(path Path) error {
	var p problems
	if s.Type != "" && !slices.Contains(StreamTypes, s.Type) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", s.Type, words.Or(StreamTypes))
		return p.err() // the rest of the rules key on the type
	}
	external := s.Type == StreamNATS
	switch {
	case external && s.URL == "":
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
	// AN EXTERNAL CLUSTER IS DIALLED WITH SOMETHING TO PRESENT: a node presenting
	// nothing can only be admitted by a server that authenticates nobody, whose
	// reachers read every event and write as any person. The dial refuses such a
	// server by what it announces (internal/queue/jetstream's
	// requiresAuthentication); this is the half a file can be held to beforehand.
	// A URL's USERINFO COUNTS (address.go in internal/queue/jetstream).
	if external && !s.presentsCredential() {
		p.add(at(path, "credentials"), ErrMissing,
			"this node dials an external NATS cluster and presents no "+
				"credential, so it can only reach one that authenticates nobody, "+
				"and anything that reaches such a broker reads every event of this "+
				"company and writes as any person on its estate. Set "+
				"stream.credentials (a .creds file), stream.token, or "+
				"stream.tls.cert, as the cluster's operator issues them")
	}
	p.wrap(s.durable(path))
	// THE SAME RULE: this is the limit handed to the server this process starts. An
	// external cluster's account limits are its operator's, read back by the
	// engine, so a number written here would bound nothing.
	if external && s.StoreMaxBytes != 0 {
		p.add(at(path, "store_max_bytes"), ErrConflict,
			"store_max_bytes is the EMBEDDED broker's own storage limit; an "+
				"external cluster's account limits are set by its operator, and "+
				"this node reads them back from the account. Remove it, or set "+
				"type to %q", StreamEmbedded)
	}
	// THE SAME RULE `url` AND `store_dir` KEEP: the flag is handed to the server
	// this process STARTS, so against an external cluster nobody reads it.
	if external && s.Debug {
		p.add(at(path, "debug"), ErrConflict,
			"debug turns on the EMBEDDED broker's own logging; an external "+
				"cluster logs wherever its operator configured it to. Remove "+
				"it, or set type to %q", StreamEmbedded)
	}
	if s.Replicas < 0 || s.Replicas > MaxStreamReplicas {
		p.add(at(path, "replicas"), ErrOutOfRange,
			"must be 0 (one copy) or 1..%d, got %d: JetStream refuses a stream "+
				"with more replicas than that, so every stream and bucket this "+
				"node provisions would fail at boot", MaxStreamReplicas, s.Replicas)
	}
	if err := s.validateSync(path, external, s.Leaf.Joins()); err != nil {
		p.wrap(err)
	}
	// AT LEAST THE DEAD LETTERS' DUPLICATE WINDOW: the broker refuses a stream
	// whose window outlasts its age, in words naming neither field.
	if s.EventRetentionHours < 0 || (s.EventRetentionHours > 0 && s.EventRetention() < queue.DeadLetterWindow) {
		p.add(at(path, "event_retention_hours"), ErrOutOfRange,
			"must be 0 (the queue default) or at least %v (%v): the dead letters are kept by "+
				"this age, and a keep of one thing is collapsed for %v, which a stream's age "+
				"must cover; got %v", queue.DeadLetterWindow.Hours(), queue.DeadLetterWindow,
			queue.DeadLetterWindow, s.EventRetentionHours)
	}
	if s.Cluster.Port < 0 || s.Cluster.Port > 65535 {
		p.add(at(path, "cluster.port"), ErrOutOfRange, "must be 0..65535, got %d", s.Cluster.Port)
	}
	// A CLUSTER BLOCK WITHOUT A NAME DOES NOTHING, silently: the embedded server's
	// options are built only when the cluster is NAMED, so the other fields are
	// dropped and the node starts solo while every other reading of the file calls
	// it clustered. ASKED through IsZero, the one place enumerating this struct.
	// THE SAME RULE `url`, `store_dir`, `debug` and `sync` keep once the stream is
	// EXTERNAL: no field is read, and validateTopology would COUNT external peers,
	// refusing two as a quorumless fleet. ONE problem, not also "name it".
	switch {
	case external && !s.Cluster.IsZero():
		p.add(at(path, "cluster"), ErrConflict,
			"stream.cluster configures the EMBEDDED server's own membership, "+
				"and an external %q cluster is formed by its own operator's "+
				"config: every field here would be read by nobody. Remove it, "+
				"or set type to %q", StreamNATS, StreamEmbedded)
	case s.Cluster.Name == "" && !s.Cluster.IsZero():
		p.add(at(path, "cluster.name"), ErrMissing,
			"is required once anything else under cluster is set (port, peers, "+
				"host or advertise): the embedded server takes all of them only "+
				"from a NAMED cluster, so this node would start solo and form no "+
				"cluster at all")
	}
	// EVERY PEER IS A ROUTE THE SERVER CAN DIAL, and one it cannot is quiet: an
	// unparsable entry is dropped, and one with no host or port (`b:6222` without
	// its scheme parses as scheme `b`) fails at debug on every attempt; the only
	// symptom is a cluster short of a member. Not asked of an external stream.
	if !external {
		for i, raw := range s.Cluster.Peers {
			switch _, ok := routeAddress(raw); {
			case carriesCredential(raw):
				// BEFORE THE SHAPE, naming no part of the entry: it would
				// print the credential.
				p.add(idx(at(path, "cluster.peers"), i), ErrConflict,
					"carries a credential, and a fleet's credentials are "+
						"derived from secrets.keys: remove the user:pass@, because "+
						"the engine presents this node's own on every route it dials")
			case !ok:
				p.add(idx(at(path, "cluster.peers"), i), ErrUnknownValue,
					"%q is not a member's route URL: want nats://host:port. "+
						"The embedded server dials each entry's host and port, "+
						"and one it cannot dial is never routed to", withoutUserinfo(raw))
			}
		}
	}
	p.wrap(s.Leaf.validate(at(path, "leaf"), s, external))
	bytesInRange(&p, path, "tracker_log_max_bytes", s.TrackerLogMaxBytes,
		TrackerLogMaxBytesFloor, TrackerLogMaxBytesCeiling)
	bytesInRange(&p, path, "tracker_vectors_max_bytes", s.TrackerVectorsMaxBytes,
		TrackerVectorsMaxBytesFloor, TrackerVectorsMaxBytesCeiling)
	bytesInRange(&p, path, "pages_log_max_bytes", s.PagesLogMaxBytes,
		PagesLogMaxBytesFloor, PagesLogMaxBytesCeiling)
	bytesInRange(&p, path, "iam_log_max_bytes", s.IamLogMaxBytes,
		IamLogMaxBytesFloor, IamLogMaxBytesCeiling)
	bytesInRange(&p, path, "usage_log_max_bytes", s.UsageLogMaxBytes,
		UsageLogMaxBytesFloor, UsageLogMaxBytesCeiling)
	bytesInRange(&p, path, "code_log_max_bytes", s.CodeLogMaxBytes,
		CodeLogMaxBytesFloor, CodeLogMaxBytesCeiling)
	bytesInRange(&p, path, "artifacts_log_max_bytes", s.ArtifactsLogMaxBytes,
		ArtifactsLogMaxBytesFloor, ArtifactsLogMaxBytesCeiling)
	bytesInRange(&p, path, "artifact_data_log_max_bytes", s.ArtifactDataLogMaxBytes,
		ArtifactDataLogMaxBytesFloor, ArtifactDataLogMaxBytesCeiling)
	bytesInRange(&p, path, "assistant_log_max_bytes", s.AssistantLogMaxBytes,
		AssistantLogMaxBytesFloor, AssistantLogMaxBytesCeiling)
	bytesInRange(&p, path, "chat_log_max_bytes", s.ChatLogMaxBytes,
		ChatLogMaxBytesFloor, ChatLogMaxBytesCeiling)
	bytesInRange(&p, path, "executor_log_max_bytes", s.ExecutorLogMaxBytes,
		ExecutorLogMaxBytesFloor, ExecutorLogMaxBytesCeiling)
	bytesInRange(&p, path, "access_log_max_bytes", s.AccessLogMaxBytes,
		AccessLogMaxBytesFloor, AccessLogMaxBytesCeiling)
	bytesInRange(&p, path, "atlas_log_max_bytes", s.AtlasLogMaxBytes,
		AtlasLogMaxBytesFloor, AtlasLogMaxBytesCeiling)
	bytesInRange(&p, path, "advisor_log_max_bytes", s.AdvisorLogMaxBytes,
		AdvisorLogMaxBytesFloor, AdvisorLogMaxBytesCeiling)
	bytesInRange(&p, path, "store_max_bytes", s.StoreMaxBytes,
		StoreMaxBytesFloor, StoreMaxBytesCeiling)
	// A LIMIT SMALLER THAN THE CEILINGS DECLARED INSIDE IT fails the boot: the broker
	// compares each stream's ceiling to it and the node fails naming whichever it
	// reached last. EVERY STATE LOG IS IN THE SUM: a log left out is a limit this
	// check passes and the boot refuses. Only EXPLICIT ceilings are summed; a derived
	// one is scaled to fit (engine.sizeCeilings).
	// NECESSARY, NOT SUFFICIENT: the state logs get a SHARE of what the broker can
	// grant (engine.StreamBudgetShare), and every other object on it (mailboxes,
	// event log, dead letters, memory changelog, coordination buckets) reserves
	// nothing at create time and grows against the same limit, so a limit exactly
	// filled still fails at boot, naming this limit and the field.
	declared := s.DeclaredLogCeilings()
	if s.StoreMaxBytes > 0 && declared > s.StoreMaxBytes {
		p.add(at(path, "store_max_bytes"), ErrConflict,
			"%d bytes is smaller than the stream ceilings declared inside it "+
				"(tracker_log_max_bytes %d + tracker_vectors_max_bytes %d + "+
				"pages_log_max_bytes %d + iam_log_max_bytes %d + "+
				"usage_log_max_bytes %d + artifacts_log_max_bytes %d + "+
				"artifact_data_log_max_bytes %d + assistant_log_max_bytes %d + "+
				"chat_log_max_bytes %d + executor_log_max_bytes %d + "+
				"code_log_max_bytes %d + access_log_max_bytes %d + "+
				"atlas_log_max_bytes %d + advisor_log_max_bytes %d = %d): the "+
				"broker refuses a stream "+
				"whose ceiling it cannot back, so this node would fail to "+
				"provision one of them. Raise store_max_bytes, or lower the "+
				"ceilings, and leave headroom, because the state logs reserve only "+
				"a share of this limit and every other stream on the broker grows "+
				"inside it",
			s.StoreMaxBytes, s.TrackerLogMaxBytes, s.TrackerVectorsMaxBytes,
			s.PagesLogMaxBytes, s.IamLogMaxBytes, s.UsageLogMaxBytes,
			s.ArtifactsLogMaxBytes, s.ArtifactDataLogMaxBytes, s.AssistantLogMaxBytes,
			s.ChatLogMaxBytes, s.ExecutorLogMaxBytes, s.CodeLogMaxBytes, s.AccessLogMaxBytes,
			s.AtlasLogMaxBytes, s.AdvisorLogMaxBytes, declared)
	}
	p.wrap(s.TrackerRetention.validate(at(path, "tracker_retention")))
	// Refused here rather than at the broker, which validates an advertise address
	// while STARTING and shuts down. Read RAW, not trimmed: [Bootstrap.Validate]
	// normalized it, and a second trim let `advertise: " "` validate as unset and
	// run as " ".
	if s.Cluster.Advertise != "" {
		if err := validateAdvertise(s.Cluster.Advertise); err != nil {
			p.add(at(path, "cluster.advertise"), ErrShape, "%v", err)
		}
	}
	p.wrap(s.TLS.validate(at(path, "tls")))
	return p.err()
}

// validateAdvertise checks a cluster advertise address: a host, optionally
// with a port. A bare host keeps this member's own route port.
func validateAdvertise(adv string) error {
	host, port, err := net.SplitHostPort(adv)
	if err != nil {
		// No port at all is legitimate; a colon with no port is an IPv6 literal
		// missing its brackets, which "no port" would misreport.
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

// EventRetention is the retention window; zero means the queue's default.
func (s *Stream) EventRetention() time.Duration {
	return time.Duration(s.EventRetentionHours * float64(time.Hour))
}

// ---- coordination ---------------------------------------------------- //

// CoordinationType is the coordination slot: where leases, the shared
// counters and the ledgers live.
type CoordinationType string

const (
	// CoordinationLocal keeps coordination inside this process: correct for one
	// node only, since every node would hold every lease and run every seat.
	CoordinationLocal CoordinationType = "local"
	// CoordinationEmbeddedKV is the replicated KV a fleet coordinates through. It
	// needs a quorum, so two-node fleets are refused.
	CoordinationEmbeddedKV CoordinationType = "embedded-kv"
)

// CoordinationTypes is the closed set.
var CoordinationTypes = []CoordinationType{CoordinationLocal, CoordinationEmbeddedKV}

// Coordination is the coordination slot.
type Coordination struct {
	// Type selects the slot. Default local.
	Type CoordinationType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=local|embedded-kv" desc:"local (single node, default) or embedded-kv (a fleet)."`

	// LeaseTTLSeconds overrides how long a seat or presence lease survives without a
	// renew. 0 takes the coordination layer's measured default; shortening speeds
	// failover at the cost of shedding seats over a store blip. Anything else is at
	// least [MinLeaseTTL]; a fraction is fine, read once at the edge.
	//
	// It bounds nothing else (coord.HeldToSeatTTL): a fleet duty's TTL follows its own
	// tick and a tracker walk's claim its own heartbeat, up to coord.MaxDutyTTL, which
	// is why every lease but a seat's and a presence lease has a bucket of its own.
	// The exception is by choice: an engine process's taker lease (coord.ClassTaker)
	// asks for this TTL so a stopped process's executor sessions settle as fast as
	// its seats move, held to coord.MaxDutyTTL (the sandbox asks for that instead
	// once this is longer).
	LeaseTTLSeconds float64 `yaml:"lease_ttl_seconds,omitempty" json:"lease_ttl_seconds,omitempty" js:"min=0" desc:"Seat and presence lease TTL, which each engine process's taker lease follows up to three hours; 0 takes the measured default, anything else is at least 15. Every other lease (a fleet duty, a Worklet walk's claim) sizes its own."`
}

// MinLeaseTTL is the shortest seat and presence lease TTL a deployment may set.
//
// THE TTL IS FOUR THINGS AT ONCE: THE WATCHDOG'S THRESHOLD (a node a TTL without
// a beat ends itself with status 75, internal/seat's Watchdog, so it must clear a
// busy healthy node's longest gap, 5.1 s measured under load; at 3 s a quarter of
// starts exceeded it); THE STORE OUTAGE A LEASE RIDES OUT (a third of the TTL
// plus two beats, 7 s here, covering the coordination bucket's leader election,
// four seconds by default and up to nine); THE BEAT, a full second from a 5 s
// TTL up; and FAILOVER, about 20 s here against 50 s at the shipped 45.
// A floor, not a warning: a shorter TTL makes a node end itself under load, and
// none has been measured safe.
const MinLeaseTTL = 15 * time.Second

// NATSTLS is the transport material for an external NATS server.
//
// `credentials` and `token` authenticate the CLIENT at the NATS protocol layer;
// this is the TCP layer under it. A server with `tls { verify: true }` rejects a
// connection without a client certificate whatever credentials follow.
//
// It cannot express "do not verify", deliberately: a skip-verify switch set once
// during a bring-up is never unset. A private CA is one file.
type NATSTLS struct {
	// CA is the PATH to a PEM bundle to verify the server's certificate against,
	// never its contents. Empty uses the host's root pool, wrong for the
	// self-signed certificate most internal NATS estates use.
	CA string `yaml:"ca,omitempty" json:"ca,omitempty" desc:"Path to a PEM CA bundle for the server certificate; empty uses the host roots."`

	// Cert and Key are the PATHS to the CLIENT certificate and its private key,
	// for a server that requires mutual TLS, never their contents. Both or
	// neither: half a keypair dials and is refused by an error naming neither file.
	Cert string `yaml:"cert,omitempty" json:"cert,omitempty" desc:"Path to the client certificate PEM, for a server requiring mutual TLS. Needs key."`
	Key  string `yaml:"key,omitempty" json:"key,omitempty" desc:"Path to the client private key PEM. Needs cert."`
}

// IsZero lets an unset TLS block drop out of a JSON round trip.
func (t NATSTLS) IsZero() bool { return t.CA == "" && t.Cert == "" && t.Key == "" }

// validate refuses half a keypair.
func (t NATSTLS) validate(path Path) error {
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

func (c *Coordination) validate(path Path) error {
	var p problems
	if c.Type != "" && !slices.Contains(CoordinationTypes, c.Type) {
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", c.Type, words.Or(CoordinationTypes))
	}
	// THE js TAG CANNOT EXPRESS "0 OR AT LEAST 15", so the schema publishes
	// min=0 and the floor lives here, in a message naming both forms.
	switch ttl := c.LeaseTTL(); {
	case c.LeaseTTLSeconds < 0:
		p.add(at(path, "lease_ttl_seconds"), ErrOutOfRange,
			"must be 0 (the default) or at least %v, got %v",
			MinLeaseTTL.Seconds(), c.LeaseTTLSeconds)
	case c.LeaseTTLSeconds > 0 && ttl < MinLeaseTTL:
		p.add(at(path, "lease_ttl_seconds"), ErrOutOfRange,
			"%v is below the %v-second floor: leave it unset to take the measured "+
				"default, or set at least %v: the lease TTL is also how long the node's "+
				"heartbeat may go without a beat before its watchdog ends it (exit 75), "+
				"and a busy but healthy node goes seconds",
			c.LeaseTTLSeconds, MinLeaseTTL.Seconds(), MinLeaseTTL.Seconds())
	}
	return p.err()
}

// LeaseTTL is the override; zero means the measured default.
func (c *Coordination) LeaseTTL() time.Duration {
	return time.Duration(c.LeaseTTLSeconds * float64(time.Second))
}

// ---- secrets --------------------------------------------------------- //

// secretKeyIDPattern keeps a key id colon-free so the envelope
// (enc:v1:<id>:...) parses.
var secretKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Secrets is the Tier A keyring every node of a fleet holds.
//
// The SOLE ROOT OF TRUST: the store holds only ciphertext. REQUIRED ON EVERY NODE
// ([Bootstrap.Validate]) because more than credentials rest on it: state-log
// records, the company document's seal, session cookies, per-run tokens and an
// embedded fleet's broker credentials (ADR-0094). No default: an invented key is
// one no other node holds.
type Secrets struct {
	// ActiveKeyID names the key that seals new writes; it must name one of Keys.
	ActiveKeyID string `yaml:"active_key_id,omitempty" json:"active_key_id,omitempty" desc:"Which key seals new writes."`

	// Keys is the keyring. Several entries support online rotation: the active
	// key seals, every key decrypts.
	Keys []SecretKey `yaml:"keys,omitempty" json:"keys,omitempty" desc:"The keyring; several entries support online rotation. Required on every node; an embedded fleet's broker credentials are derived from it beside everything else it signs."`
}

// SecretKey is one key in the keyring.
type SecretKey struct {
	// ID is stamped into every envelope this key seals (enc:v1:<id>:...) and kept
	// across a rotation so old ciphertext still names a live key.
	//
	// AT MOST [secrets.MaxStreamKeyIDBytes], the longest id a sealed stream's
	// header names: an id no header can name refuses the whole keyring at a
	// node's first executor-session capture, long after validation passed it.
	ID string `yaml:"id" json:"id" js:"required;pattern=^[A-Za-z0-9._-]+$;maxlen=255" desc:"Stable key id, colon-free so the envelope parses, at most 255 characters so a sealed stream's header can name it."`

	// Material is a base64-encoded 32-byte key, or a ${VAR} reference to one.
	Material string `secret:"true" yaml:"material" json:"material" js:"required" desc:"base64(32 bytes), or a ${VAR} reference to it."`
}

func (s *Secrets) validate(path Path) error {
	var p problems
	ids := make(map[string]struct{}, len(s.Keys))
	for i, k := range s.Keys {
		kp := idx(at(path, "keys"), i)
		switch {
		case k.ID == "":
			p.add(at(kp, "id"), ErrMissing, "every key needs an id")
		case !secretKeyIDPattern.MatchString(k.ID):
			p.add(at(kp, "id"), ErrUnknownValue,
				"%q must contain only letters, digits, '.', '_' or '-': it is "+
					"stamped into every envelope this key seals", k.ID)
		case len(k.ID) > secrets.MaxStreamKeyIDBytes:
			p.add(at(kp, "id"), ErrOutOfRange,
				"the id is %d characters and at most %d are allowed: a sealed "+
					"stream's header names its key in one length byte", len(k.ID),
				secrets.MaxStreamKeyIDBytes)
		}
		if k.Material == "" {
			p.add(at(kp, "material"), ErrMissing,
				"key material must not be empty (generate one with `crewlet secrets keygen`)")
		} else if _, err := secrets.DecodeKey(k.Material); err != nil {
			// THE CIPHER'S OWN CHECK, through the one decoder and the one
			// sentence [Secrets.Cipher] refuses with, so validation refuses
			// exactly what the boot would.
			p.add(at(kp, "material"), ErrShape, keyMaterialProblem, k.ID, err)
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

// Enabled reports whether this value carries a keyring at all; false answers
// only a value built in code, which [Secrets.Cipher] refuses.
func (s *Secrets) Enabled() bool { return len(s.Keys) > 0 }

// TokenMaterial is this keyring as a per-run token issuer reads it.
//
// ONE BUILDER FOR EVERY ISSUER (OTLP receiver, MCP bridge, CLI signer). The
// material is ALREADY RESOLVED: Tier A expands its document before decoding, so
// two nodes spelling one key differently (`${K}`, the literal) derive the same
// signing key.
func (s *Secrets) TokenMaterial() runtoken.Material {
	if s == nil {
		return runtoken.Material{}
	}
	out := runtoken.Material{
		ActiveID: s.ActiveKeyID,
		Keys:     make([]runtoken.KeyMaterial, 0, len(s.Keys)),
	}
	for _, key := range s.Keys {
		out.Keys = append(out.Keys, runtoken.KeyMaterial{ID: key.ID, Material: key.Material})
	}
	return out
}

// Cipher builds the sealing cipher this Tier A configures.
//
// AN EMPTY KEYRING IS REFUSED where built, not at every caller: a nil cipher
// would be the one way to reach plaintext (a revision written in the clear). An
// unusable keyring fails too: key material that is not 32 bytes of base64 would
// seal the next revision under a key nobody can reproduce.
func (s *Secrets) Cipher() (secrets.Cipher, error) {
	ring, err := s.Keyring()
	if err != nil {
		return nil, err
	}
	return secrets.NewCipher(ring)
}

// Keyring is the decoded keyring [Secrets.Cipher] seals under, for a caller that
// derives a key from it ([secrets.Keyring.Derive]): one decoding, so the two
// never disagree about which key is active.
func (s *Secrets) Keyring() (secrets.Keyring, error) {
	if !s.Enabled() {
		return secrets.Keyring{}, fault(field("secrets.keys"), ErrMissing,
			"this node has no keyring. Generate a key with `crewlet secrets "+
				"keygen` and add it to `secrets.keys`")
	}
	ring := secrets.Keyring{
		ActiveID: s.ActiveKeyID,
		Keys:     make(map[string][]byte, len(s.Keys)),
	}
	for i, key := range s.Keys {
		material, err := secrets.DecodeKey(key.Material)
		if err != nil {
			// The ID reaches the message and the material never does. The path
			// is the element's own material field, as `keys` is a list.
			return secrets.Keyring{}, fault(at(idx(field("secrets.keys"), i), "material"), ErrShape,
				keyMaterialProblem, key.ID, err)
		}
		ring.Keys[key.ID] = material
	}
	return ring, nil
}

// keyMaterialProblem is the ONE sentence unusable key material is refused with,
// by [Secrets.validate] and [Secrets.Cipher] alike. Its arguments are the key's
// id and [secrets.DecodeKey]'s error, neither carrying the material.
const keyMaterialProblem = "key %q: %v (generate one with `crewlet secrets keygen`)"

// validateSync checks stream.sync; its four refusals are about MEANING, not
// syntax: each is a place where declining the fsync would be recorded and not
// honoured, and the operator would believe the number they wrote.
func (s *Stream) validateSync(path Path, external, leaf bool) error {
	var p problems
	raw := s.Sync
	if raw == "" || raw == StreamSyncAlways {
		// Unset takes `always`, a deliberate cost on a replicated fleet, stated
		// in docs/guides/deployment.md: nothing here to fix.
		return nil
	}

	// (1) AN EXTERNAL CLUSTER'S DISK IS NOT THIS PROCESS'S TO CONFIGURE: the
	// field sets an option on the EMBEDDED server.
	if external {
		p.add(at(path, "sync"), ErrConflict,
			"stream.sync configures the EMBEDDED server's file store, and an "+
				"external NATS cluster stores its own data: set sync_interval "+
				"on that cluster instead, or remove this field")
		return p.err()
	}

	// (2) NOR IS A LEAF'S: it has no file store; the members it joins decide what
	// an acknowledged write has reached, in their own `stream.sync`.
	if leaf {
		p.add(at(path, "sync"), ErrConflict,
			"stream.sync configures the EMBEDDED server's file store, and a "+
				"leaf runs no JetStream, so it has none: what an acknowledged "+
				"write has reached is decided by the members it joins. Set "+
				"sync on those members instead, or remove this field")
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

	// (3) BELOW THREE REPLICAS THERE IS NO QUORUM TO SPEND INSTEAD: declining the
	// fsync relies on a majority holding the write, and a solo member's disk is
	// the only copy, so the window is a straight loss.
	if s.Replicas < 3 {
		p.add(at(path, "sync"), ErrConflict,
			"declining the fsync trades this member's disk for a quorum, and "+
				"replicas=%d has no quorum to trade for: the local disk is the "+
				"only copy, so %q would be a window with nothing behind it",
			max(s.Replicas, 1), s.Sync)
	}

	// (4) A SAME-HOST CLUSTER IS ONE FAILURE DOMAIN: peers on this host share its
	// power, kernel and page cache. Asked of the OTHER members
	// ([StreamCluster.members]): this node's own route is local by definition and
	// would hide a cluster whose other members are all local.
	if sameHostCluster(s.Cluster.members().Others) {
		p.add(at(path, "sync"), ErrConflict,
			"every other member in stream.cluster.peers is on this host, so the quorum "+
				"this window trades for shares one power supply and one page "+
				"cache: %q would be recorded and not honoured", s.Sync)
	}
	return p.err()
}

// sameHostCluster reports whether every peer address is on this machine.
//
// A HOST TEST: `localhost`, `127.0.0.1` and `::1` are one failure domain however
// written. An empty peer list answers false.
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

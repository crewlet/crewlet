package atlas

import "slices"

// Kind is what one entry of the atlas is. A closed set: a kind joins with the
// change that first fills it, its lane, its key grammar and its facts beside it.
type Kind string

const (
	// KindAccount is the company's account at a vendor; what the vendor does
	// for the company is its capabilities, never a kind per product.
	KindAccount Kind = "vendor_account"
	// KindDomain is a zone the company holds (acme.example).
	KindDomain Kind = "domain"
	// KindHostname is one fully qualified name (api.acme.example).
	KindHostname Kind = "hostname"
	// KindEndpoint is an address something answers on, without credentials.
	KindEndpoint Kind = "endpoint"
	// KindService is something the company runs: an app, an API, a worker.
	KindService Kind = "service"
	// KindHost is a server, a virtual machine or an instance.
	KindHost Kind = "host"
	// KindDatastore is a database or a cache.
	KindDatastore Kind = "datastore"
	// KindQueue is a broker, a topic or a queue.
	KindQueue Kind = "queue"
	// KindBucket is object storage.
	KindBucket Kind = "bucket"
	// KindRepository is a code repository, on Gitlet or a vendor's forge.
	KindRepository Kind = "repository"
	// KindPipeline is a check or a workflow that builds or deploys code.
	KindPipeline Kind = "pipeline"
	// KindMonitor is a monitor or an alert definition.
	KindMonitor Kind = "monitor"
	// KindModel is a model entry the engine calls, and its vendor.
	KindModel Kind = "model_provider"
	// KindToolServer is an MCP server the company's agents use.
	KindToolServer Kind = "tool_server"
)

// Kinds is every kind, in the order the map reads its lanes.
var Kinds = []Kind{
	KindDomain, KindHostname, KindEndpoint,
	KindService,
	KindHost,
	KindDatastore, KindQueue, KindBucket,
	KindAccount,
	KindRepository, KindPipeline,
	KindMonitor,
	KindToolServer, KindModel,
}

// Valid reports whether k is a member of the closed set.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Lane is the column of the map a kind is drawn in. The table is the engine's,
// so a kind with no lane fails a test instead of becoming a card with no column.
type Lane string

const (
	LaneEntry         Lane = "entry"
	LaneServices      Lane = "services"
	LaneRuntime       Lane = "runtime"
	LaneData          Lane = "data"
	LanePlatforms     Lane = "platforms"
	LaneCode          Lane = "code"
	LaneObservability Lane = "observability"
	LaneTools         Lane = "tools"
)

// Lanes is every lane: the five a request travels left to right, then the
// three of the band beneath them.
var Lanes = []Lane{
	LaneEntry, LaneServices, LaneRuntime, LaneData, LanePlatforms,
	LaneCode, LaneObservability, LaneTools,
}

// Valid reports whether l is a member of the closed set.
func (l Lane) Valid() bool { return slices.Contains(Lanes, l) }

// Band reports whether the lane sits in the band beneath the request path.
func (l Lane) Band() bool { return l == LaneCode || l == LaneObservability || l == LaneTools }

var laneOf = map[Kind]Lane{
	KindDomain:     LaneEntry,
	KindHostname:   LaneEntry,
	KindEndpoint:   LaneEntry,
	KindService:    LaneServices,
	KindHost:       LaneRuntime,
	KindDatastore:  LaneData,
	KindQueue:      LaneData,
	KindBucket:     LaneData,
	KindAccount:    LanePlatforms,
	KindRepository: LaneCode,
	KindPipeline:   LaneCode,
	KindMonitor:    LaneObservability,
	KindToolServer: LaneTools,
	KindModel:      LaneTools,
}

// Lane is the lane k is drawn in, "" for a kind outside the set.
func (k Kind) Lane() Lane { return laneOf[k] }

// rank orders kinds the way [Kinds] lists them.
func (k Kind) rank() int { return slices.Index(Kinds, k) }

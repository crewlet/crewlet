package atlas

import (
	"slices"
	"sync"
)

// Relation is how one entry relies on another. Every relation points FROM the
// thing that relies TO the thing relied on, so the map's arrows run one way and
// "what depends on X" is one walk against them.
type Relation string

// The relations, each named for what the entry it starts from does.
const (
	RelationRunsOn       Relation = "runs_on"
	RelationDependsOn    Relation = "depends_on"
	RelationExposes      Relation = "exposes"
	RelationResolvesTo   Relation = "resolves_to"
	RelationInZone       Relation = "in_zone"
	RelationServedBy     Relation = "served_by"
	RelationFrontedBy    Relation = "fronted_by"
	RelationBuiltFrom    Relation = "built_from"
	RelationDeployedBy   Relation = "deployed_by"
	RelationMonitoredBy  Relation = "monitored_by"
	RelationBelongsTo    Relation = "belongs_to"
	RelationDocumentedIn Relation = "documented_in"
	RelationOwnedBy      Relation = "owned_by"
)

// Relations is every relation, the structural ones first.
var Relations = []Relation{
	RelationRunsOn, RelationDependsOn, RelationExposes, RelationResolvesTo,
	RelationInZone, RelationFrontedBy,
	RelationServedBy, RelationBuiltFrom, RelationDeployedBy, RelationMonitoredBy,
	RelationBelongsTo, RelationDocumentedIn, RelationOwnedBy,
}

// Valid reports whether r is a member of the closed set.
func (r Relation) Valid() bool { return slices.Contains(Relations, r) }

// Structural reports whether the map draws r at rest. The others are drawn when
// a card is selected, so the map does not become a star around the monitoring
// vendor and the code host.
func (r Relation) Structural() bool {
	switch r {
	case RelationRunsOn, RelationDependsOn, RelationExposes, RelationResolvesTo,
		RelationInZone, RelationFrontedBy:
		return true
	}
	return false
}

// External is a target outside the atlas a relation may name. Each is an
// identity the engine already never changes in place: a seat's handle, a
// unit's key, a page's id.
type External string

// The identities outside the atlas a relation may name.
const (
	ExternalSeat External = "seat"
	ExternalUnit External = "unit"
	ExternalPage External = "page"
)

// Externals is every external target.
var Externals = []External{ExternalSeat, ExternalUnit, ExternalPage}

// Valid reports whether e is a member of the closed set.
func (e External) Valid() bool { return slices.Contains(Externals, e) }

// resourceKinds is every kind that can belong to an account.
var resourceKinds = []Kind{
	KindDomain, KindHostname, KindEndpoint, KindService, KindHost, KindDatastore,
	KindQueue, KindBucket, KindRepository, KindPipeline, KindMonitor, KindModel,
	KindToolServer,
}

// pair is what one relation may join: the kinds it may start from, and the
// kinds or the external targets it may end at.
type pair struct {
	from      []Kind
	to        []Kind
	externals []External
}

// pairs is what every relation joins, built at first use (see versionRE).
var pairs = sync.OnceValue(func() map[Relation]pair {
	return map[Relation]pair{
		RelationRunsOn: {
			from: []Kind{KindService, KindDatastore, KindQueue},
			to:   []Kind{KindHost, KindAccount},
		},
		RelationDependsOn: {
			from: []Kind{KindService},
			to: []Kind{KindService, KindDatastore, KindQueue, KindBucket, KindEndpoint,
				KindAccount, KindModel, KindToolServer},
		},
		RelationExposes:    {from: []Kind{KindService}, to: []Kind{KindEndpoint}},
		RelationResolvesTo: {from: []Kind{KindHostname}, to: []Kind{KindHost, KindEndpoint}},
		RelationInZone:     {from: []Kind{KindHostname}, to: []Kind{KindDomain}},
		RelationServedBy:   {from: []Kind{KindDomain}, to: []Kind{KindAccount}},
		RelationFrontedBy: {
			from: []Kind{KindHostname, KindEndpoint, KindService},
			to:   []Kind{KindAccount},
		},
		RelationBuiltFrom:  {from: []Kind{KindService, KindPipeline}, to: []Kind{KindRepository}},
		RelationDeployedBy: {from: []Kind{KindService}, to: []Kind{KindPipeline}},
		RelationMonitoredBy: {
			from: []Kind{KindService, KindEndpoint, KindHostname, KindHost, KindDatastore, KindQueue},
			to:   []Kind{KindMonitor, KindAccount},
		},
		RelationBelongsTo:    {from: resourceKinds, to: []Kind{KindAccount}},
		RelationDocumentedIn: {from: Kinds, externals: []External{ExternalPage}},
		RelationOwnedBy:      {from: Kinds, externals: []External{ExternalSeat, ExternalUnit}},
	}
})

// Joins reports whether r may run from an entry of kind from to an entry of
// kind to.
func (r Relation) Joins(from, to Kind) bool {
	p := pairs()[r]
	return slices.Contains(p.from, from) && slices.Contains(p.to, to)
}

// Names reports whether r may run from an entry of kind from to an external
// target of kind to.
func (r Relation) Names(from Kind, to External) bool {
	p := pairs()[r]
	return slices.Contains(p.from, from) && slices.Contains(p.externals, to)
}

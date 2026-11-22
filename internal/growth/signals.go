package growth

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Signals is what one node's products met at their vendors that an
// administrator must act on: a clock its vendor's disagrees with, and a vendor
// saying an API version this build calls is deprecated. In memory, per node,
// bounded, and published on the node's status record, so every node's notices
// read the fleet's (internal/notice). The zero value is not usable: [NewSignals].
type Signals struct {
	mu         sync.Mutex
	now        func() time.Time
	skews      map[string]ClockSkew
	deprecated map[string]Deprecation
}

// ClockSkewTolerance is how far a vendor's clock may be from the node's before
// a refused signed token is laid to it: a minute, past every vendor's own
// leeway on a token's issue time (Apple's and Meta's included) and far past
// NTP's drift on a synchronised host.
const ClockSkewTolerance = time.Minute

// DeprecationLife is how long a vendor's deprecation signal stands without a
// repeat: a day, the span in which the products' daily collection calls every
// vendor again, so a vendor that stopped saying it is heard to stop.
const DeprecationLife = 24 * time.Hour

// MaxSignals bounds each kind of signal a node keeps: one per product (nine),
// with room for a fleet's later ones, at a record size the status record
// carries well under its bound.
const MaxSignals = 32

// MaxSignalTextBytes bounds a signal's product, vendor and version names.
const MaxSignalTextBytes = 64

// ClockSkew is a vendor refusing a freshly signed token while its clock is
// more than [ClockSkewTolerance] from the node's.
type ClockSkew struct {
	// Product is the integration kind, Vendor the vendor's name as a person
	// reads it ("Apple").
	Product, Vendor string
	// Offset is the node's clock minus the vendor's (its Date header):
	// positive where the node is ahead.
	Offset time.Duration
	// Since is when the first refusal of this run was met.
	Since time.Time
}

// Deprecation is a vendor saying the API version a product calls is
// deprecated (a `Sunset` or `Deprecation` header, a version warning).
type Deprecation struct {
	Product, Version string
	// Retires is the date the vendor gave, zero where it gave none.
	Retires time.Time
	// Since is when it was first heard, Seen when last.
	Since, Seen time.Time
}

// NewSignals is a node's empty signals, read off now; nil is the wall clock.
func NewSignals(now func() time.Time) *Signals {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Signals{now: now, skews: map[string]ClockSkew{}, deprecated: map[string]Deprecation{}}
}

// cut is a name within [MaxSignalTextBytes], whole runes.
func cut(s string) string {
	s = strings.TrimSpace(s)
	for len(s) > MaxSignalTextBytes {
		s = s[:len(s)-1]
	}
	return strings.ToValidUTF8(s, "")
}

// ClockRefused records that product's vendor refused a freshly signed token
// while its clock was offset from the node's; an offset within
// [ClockSkewTolerance] is not the clock's doing, and is not kept.
func (s *Signals) ClockRefused(product, vendor string, offset time.Duration) {
	product, vendor = cut(product), cut(vendor)
	if product == "" || (offset <= ClockSkewTolerance && offset >= -ClockSkewTolerance) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	since := s.now()
	if was, ok := s.skews[product]; ok {
		since = was.Since
	}
	s.skews[product] = ClockSkew{Product: product, Vendor: vendor, Offset: offset, Since: since}
	trim(s.skews, func(c ClockSkew) time.Time { return c.Since })
}

// ClockAgreed records that a later signed request to product's vendor was
// accepted: the skew it refused is gone.
func (s *Signals) ClockAgreed(product string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.skews, cut(product))
}

// Deprecated records product's vendor saying the version it calls is
// deprecated, retiring on retires (zero where it said no date).
func (s *Signals) Deprecated(product, version string, retires time.Time) {
	product, version = cut(product), cut(version)
	if product == "" || version == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := product + "/" + version
	d := Deprecation{Product: product, Version: version, Retires: retires.UTC(), Since: now, Seen: now}
	if was, ok := s.deprecated[key]; ok {
		d.Since = was.Since
	}
	s.deprecated[key] = d
	trim(s.deprecated, func(d Deprecation) time.Time { return d.Seen })
}

// trim drops the oldest entries past [MaxSignals], by at.
func trim[T any](m map[string]T, at func(T) time.Time) {
	for len(m) > MaxSignals {
		oldest := ""
		for k, v := range m {
			if oldest == "" || at(v).Before(at(m[oldest])) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
}

// Snapshot is the node's standing signals, each list ordered by product: the
// clock skews, and the deprecations heard within [DeprecationLife].
func (s *Signals) Snapshot() ([]ClockSkew, []Deprecation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	skews := make([]ClockSkew, 0, len(s.skews))
	for _, c := range s.skews {
		skews = append(skews, c)
	}
	var deprecated []Deprecation
	for k, d := range s.deprecated {
		if now.Sub(d.Seen) > DeprecationLife {
			delete(s.deprecated, k)
			continue
		}
		deprecated = append(deprecated, d)
	}
	slices.SortFunc(skews, func(a, b ClockSkew) int { return strings.Compare(a.Product, b.Product) })
	slices.SortFunc(deprecated, func(a, b Deprecation) int {
		return strings.Compare(a.Product+"/"+a.Version, b.Product+"/"+b.Version)
	})
	return skews, deprecated
}

package googleauth

import (
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Product is one Google growth product that signs in as the core's
// identities: each seat's growth identity, and the company's reader.
type Product struct {
	// Kind is the product's integration kind, config key and seat lane.
	Kind integration.Kind

	// Name is what a person calls it ("Google Analytics").
	Name string

	// APIs are the services the Cloud Shell block enables in the identity
	// project for it, which the pass checks are on.
	APIs []string

	// Scopes are the scopes a seat's token carries at each lane level
	// ("read", "operate", "administer"), the narrowest the vendor offers.
	Scopes map[string][]string

	// ReaderScopes are the scopes the company's reader is minted with.
	ReaderScopes []string

	// On reports whether a company document turns the product on.
	On func(c *config.Company) bool
}

// registry is every registered product, appended to from each product
// package's init and read after. Guarded, since a test may register too.
//
//nolint:gochecknoglobals // a registry its products fill at init
var registry struct {
	sync.Mutex
	products []Product
}

// Register adds a product. It panics on a product with no kind, API or read
// scope, or one registered twice: a programming error the build must not
// ship.
func Register(p Product) {
	registry.Lock()
	defer registry.Unlock()
	switch {
	case p.Kind == "" || p.Name == "" || len(p.APIs) == 0 || len(p.Scopes["read"]) == 0 ||
		len(p.ReaderScopes) == 0 || p.On == nil:
		panic(fmt.Sprintf("googleauth: product %q needs its kind, name, APIs, read and reader scopes and On", p.Kind))
	case slices.ContainsFunc(registry.products, func(q Product) bool { return q.Kind == p.Kind }):
		panic(fmt.Sprintf("googleauth: product %q registered twice", p.Kind))
	}
	registry.products = append(registry.products, p)
}

// Products is every registered product, in the order registered.
func Products() []Product {
	registry.Lock()
	defer registry.Unlock()
	return slices.Clone(registry.products)
}

// ProductOf is the registered product of kind.
func ProductOf(kind integration.Kind) (Product, bool) {
	for _, p := range Products() {
		if p.Kind == kind {
			return p, true
		}
	}
	return Product{}, false
}

// EnabledProducts is every registered product c turns on.
func EnabledProducts(c *config.Company) []Product {
	var out []Product
	for _, p := range Products() {
		if c != nil && p.On(c) {
			out = append(out, p)
		}
	}
	return out
}

// GrowthKinds is the kind of every registered product: the lanes whose seats
// sign in as their growth identity.
func GrowthKinds() []integration.Kind {
	var out []integration.Kind
	for _, p := range Products() {
		out = append(out, p.Kind)
	}
	return out
}

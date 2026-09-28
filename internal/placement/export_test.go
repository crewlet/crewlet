package placement

// THE BALANCE'S OWN SUITE IS AN EXTERNAL TEST PACKAGE (balance_test.go), drawn
// over the object map's groups: its corpus, and every figure [Balance]'s doc
// quotes, were measured over groups that split at the count
// objstore/placement.TargetPGBits picks, and only an external test package
// can import that package, which imports this one. These are the internals
// that suite reads, exported to it alone.

// Entitlement is what [Balance] aims a draw at: entitle's answer, with its
// fields exported. Member is per member index; Domain, Lo, Hi and Count are per
// domain index, in which a member with no domain is one of its own.
type Entitlement struct {
	Member, Domain, Lo, Hi []float64
	Count                  []int
}

// Entitle is a draw's entitlement.
func Entitle(d Draw) Entitlement {
	e := entitle(newDrawer(d))
	return Entitlement{Member: e.member, Domain: e.domain, Lo: e.lo, Hi: e.hi, Count: e.count}
}

// Quantise is a share as a map stores it.
func Quantise(share float64) uint32 { return quantise(share) }

// ShareOne is a share of weight 1.0.
const ShareOne = shareOne

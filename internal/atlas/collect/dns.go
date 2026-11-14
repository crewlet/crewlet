package collect

import "github.com/crewlet/crewlet/internal/atlas"

// The record types a DNS source reads: the three that say where a name
// points. Every other type is skipped unread, TXT above all, which holds
// verification tokens and keys.
const (
	RecordA     = "A"
	RecordAAAA  = "AAAA"
	RecordCNAME = "CNAME"
)

// Record is one DNS record as a vendor lists it: its type, the full name it
// is for, and the address or the name it points at.
type Record struct {
	Type    string
	Name    string
	Content string

	// Hosts is the servers the same read already claims, by the key of
	// each one's public address ([atlas.AddressHostKey]); nil for a vendor
	// that holds none. An address record pointing at one resolves to it.
	Hosts map[string]atlas.Ref
}

// Name claims the name an A, AAAA or CNAME record of zone is for, that it
// sits in the zone and what it points at: the server at an address (one the
// read already holds there, else one known by the address alone), or another
// name, which sits in whichever of zones it is under. It answers the
// name, and false for any other record type and for a name that is withheld.
// One copy, so two DNS vendors cannot read the same record two ways.
func (b *Builder) Name(at string, zone atlas.Ref, zones []atlas.Ref, r Record) (atlas.Ref, bool) {
	if r.Type != RecordA && r.Type != RecordAAAA && r.Type != RecordCNAME {
		return atlas.Ref{}, false
	}
	host, ok := b.Ref(at+".name", atlas.KindHostname, r.Name)
	if !ok {
		return atlas.Ref{}, false
	}
	b.Relates(at+".name", host, atlas.RelationInZone, zone)
	if r.Type == RecordCNAME {
		b.PointsAtName(at+".content", host, r.Content, zones)
		return host, true
	}
	// A SERVER THIS READ HOLDS AT THE ADDRESS IS THE ORIGIN ITSELF: one
	// source saying an address twice is read as a shared address, so the
	// fold would never join the two. An address that is not one is left to
	// PointsAtAddress, which withholds it.
	if key, err := atlas.AddressHostKey(r.Content); err == nil {
		if known, held := r.Hosts[key]; held {
			b.Relates(at+".content", host, atlas.RelationResolvesTo, known)
			return host, true
		}
	}
	b.PointsAtAddress(at+".content", host, r.Content)
	return host, true
}

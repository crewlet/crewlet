package wiregate

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD DECLARES EVERY FLAG AND NUMBER AS THE ENGINE SENDS IT:
// required where it is always sent, so no screen writes a default for a key
// that is never missing, and optional where it is left out at zero, so no
// screen trusts a key that may be missing. protocol/types.ts names no Go type,
// so each interface of the wire's module is paired with the struct a client
// receives whose keys hold all its members, the one with fewest keys besides;
// candidates that tie and disagree leave it unjudged. What the module held
// when the gate arrived is declaredOptionalInventory, which only shrinks.
// Mutation: make WorkBlocker's `open` optional and this fails.
func TestTheDashboardDeclaresEveryFlagAndNumberAsTheEngineSendsIt(t *testing.T) {
	t.Parallel()
	w, err := loaded()
	if err != nil {
		t.Fatalf("reading the wire: %v", err)
	}
	ifaces, err := clientsource.Interfaces(clientsource.Tree(t), "protocol", clientsource.ContractDir)
	if err != nil {
		t.Fatalf("reading the wire's module: %v", err)
	}
	paired, found := w.mirror(ifaces)
	optional := map[string][]string{}
	for _, m := range found {
		if m.required {
			t.Errorf("%s declares %s required, and %s leaves it out at zero (`%s`): a screen "+
				"trusting the type reads a missing key as undefined. Declare it optional, or "+
				"send it always", m.iface, m.member, m.owner, m.options)
			continue
		}
		optional[m.iface] = append(optional[m.iface], m.member)
		if !slices.Contains(declaredOptionalInventory[m.iface], m.member) {
			t.Errorf("%s declares %s optional, and %s always sends it: declare it required, so "+
				"no screen writes a default for a key that is never missing", m.iface, m.member, m.owner)
		}
	}
	for _, iface := range slices.Sorted(maps.Keys(declaredOptionalInventory)) {
		for _, member := range declaredOptionalInventory[iface] {
			if !slices.Contains(optional[iface], member) {
				t.Errorf("declaredOptionalInventory lists %s.%s, which is declared as the engine "+
					"sends it now (or left the module): remove it from inventory_test.go", iface, member)
			}
		}
	}
	for _, iface := range slices.Sorted(maps.Keys(unpaired)) {
		if len(w.mirrored(ifaces[iface])) == 0 {
			t.Errorf("unpaired lists %s, which no struct's keys hold now (or left the module): "+
				"remove it", iface)
		}
	}
	t.Logf("paired %d of %d interfaces with the struct they mirror", paired, len(ifaces))
	if paired < 500 {
		t.Fatalf("paired %d interfaces: the pairing is not reading the wire's module", paired)
	}
}

// unpaired are the interfaces whose members a struct's keys hold though they
// mirror no answer of it, each with what it is.
var unpaired = map[string]string{
	"AssistantResource": "a resource in view that the dashboard sends the assistant, its version a string",
}

// mismatch is one member an interface declares otherwise than its struct
// sends it: required where the struct leaves it out at zero, or optional
// where it always sends it.
type mismatch struct {
	iface, member, owner, options string
	required                      bool
}

// mirror pairs each interface with the struct it mirrors and returns how
// many it paired and every flag or number member declared otherwise than
// the struct sends it.
func (w *wire) mirror(ifaces map[string][]clientsource.Member) (int, []mismatch) {
	paired := 0
	var out []mismatch
	for _, iface := range slices.Sorted(maps.Keys(ifaces)) {
		if _, ok := unpaired[iface]; ok {
			continue
		}
		members := ifaces[iface]
		owners := w.mirrored(members)
		if len(owners) == 0 {
			continue
		}
		judged, agree := w.agreed(owners, members)
		if !agree {
			continue
		}
		paired++
		for _, m := range members {
			f, ok := judged[m.Name]
			if !ok {
				continue
			}
			switch {
			case f.omitted() && !m.Optional:
				out = append(out, mismatch{iface: iface, member: m.Name, owner: f.owner,
					options: f.options, required: true})
			case !f.omitted() && m.Optional:
				out = append(out, mismatch{iface: iface, member: m.Name, owner: f.owner})
			}
		}
	}
	return paired, out
}

// mirrored is the structs whose objects hold every member, those with the
// fewest keys: the closest mirrors.
func (w *wire) mirrored(members []clientsource.Member) []string {
	if len(members) == 0 {
		return nil
	}
	var best []string
	fewest := 0
	for _, owner := range slices.Sorted(maps.Keys(w.objects)) {
		keys := w.objects[owner]
		if !slices.ContainsFunc(members, func(m clientsource.Member) bool { _, ok := keys[m.Name]; return !ok }) {
			switch {
			case best == nil || len(keys) < fewest:
				best, fewest = []string{owner}, len(keys)
			case len(keys) == fewest:
				best = append(best, owner)
			}
		}
	}
	return best
}

// agreed is the flag and number fields the members name, if every owner
// sends each member the same way: as the same kind, left out at zero or not.
func (w *wire) agreed(owners []string, members []clientsource.Member) (map[string]wireField, bool) {
	judged := map[string]wireField{}
	for _, m := range members {
		first := w.objects[owners[0]][m.Name]
		for _, owner := range owners[1:] {
			f := w.objects[owner][m.Name]
			if f.kind() != first.kind() || f.omitted() != first.omitted() {
				return nil, false
			}
		}
		if first.kind() != "" {
			judged[m.Name] = first
		}
	}
	return judged, true
}

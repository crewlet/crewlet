package sandbox

import "fmt"

// NewContainerBoxAt is a container box whose directory is root, for the file
// contract suite: a container box's reads and writes happen on the HOST side
// of its bind mount, so its file contract can be certified without a container
// runtime. Nothing it reaches runs a command. It answers the box and the host
// directory its home is — the one the mount would carry, which is not root
// itself, since root also holds the engine's records about the box.
func NewContainerBoxAt(root string) (Sandbox, string) {
	layout := boxLayout{id: "contract", root: root}
	return &containerBox{layout: layout, runtime: "docker", container: "contract"}, layout.home()
}

// CapReads holds a local box's whole reads to n bytes, for the file contract
// suite: it certifies the refusal one byte past the cap and the whole read at
// it, which is the same branch at any cap and two 32 MiB files per backend at
// the real one. The fake's own [FakeSandbox.CapReads] is exported, because the
// runner suites outside this package need it too.
func CapReads(box Sandbox, n int) Sandbox {
	switch b := box.(type) {
	case *directBox:
		b.readCap = n
	case *containerBox:
		b.readCap = n
	default:
		panic(fmt.Sprintf("CapReads: %T is not a local box", box))
	}
	return box
}

// ReadLimit is the most box reads whole.
func ReadLimit(box Sandbox) int {
	switch b := box.(type) {
	case *directBox:
		return readLimit(b.readCap)
	case *containerBox:
		return readLimit(b.readCap)
	case *FakeSandbox:
		return b.ReadCap()
	}
	panic(fmt.Sprintf("ReadLimit: %T reads by no cap this package holds", box))
}

// SetOtelBudget gives a receiver a decoding budget of n bytes, so a case can
// fill it with a few small exports rather than a quarter of a gibibyte.
func SetOtelBudget(r *OtelReceiver, n int64) { r.budget = newOtelBudget(n) }

// MeasureOtel is what the receiver measures an export to cost before it
// decodes it — its plain size and what decoding it holds — and the weight it
// reserves from the budget for it.
func MeasureOtel(e OtelExport) (plain, decoded, weight int64, err error) {
	mediaType, gzipped, err := exportForm(e)
	if err != nil {
		return 0, 0, 0, err
	}
	cost, err := measureExport(e.Body, gzipped, mediaType)
	return cost.plain, cost.decoded, cost.weight(), err
}

// OtelTraceMessageSizes is the Go size the receiver weighs each message of a
// traces export at, by its descriptor's full name.
func OtelTraceMessageSizes() map[string]int64 {
	out := map[string]int64{}
	for name, size := range otelStructSizes {
		out[string(name)] = size
	}
	return out
}

// OtelUnknownStructSize is what a message with no generated Go type is
// weighed at.
const OtelUnknownStructSize = otelUnknownStructSize

package s3obj

// CutParts sizes every part of b's multipart uploads but the last at n bytes,
// so a case reaches the multipart path with a body it can afford — see
// [Backend.partBytes].
func CutParts(b *Backend, n int) { b.cutParts(n) }

// MaxParts is [maxParts], for the case that pins the part arithmetic.
const MaxParts = maxParts

// PartsInFlight and SpareParts are [partsInFlight] and [spareParts], for the
// cases that hold an upload to them.
const (
	PartsInFlight = partsInFlight
	SpareParts    = spareParts
)

// PartBudget and CallBudget are [partBudget] and [callBudget], for the case
// that holds a part's deadline to its share of the link.
var PartBudget = partBudget

const CallBudget = callBudget

// SparesHeld is how many of the node's spare part buffers b's uploads hold
// now — none, once every upload has ended, whatever became of it.
func SparesHeld(b *Backend) int { return len(b.spare) }

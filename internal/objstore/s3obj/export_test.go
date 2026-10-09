package s3obj

// CutParts sizes every part of b's multipart uploads but the last at n bytes,
// so a case reaches the multipart path with a body it can afford — see
// [Backend.partBytes].
func CutParts(b *Backend, n int) { b.cutParts(n) }

// MaxParts is [maxParts], for the case that pins the part arithmetic.
const MaxParts = maxParts

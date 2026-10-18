// Package canon is the ONE canonical encoding of a datastore document and the
// store state digest built from it.
//
// Every node must hold byte-identical rows, so a body is encoded with sorted
// keys, no insignificant whitespace and numbers carried as their decimal text
// rather than through a float, which would rewrite a large integer. The state
// digest is the sum, modulo two to the 256th, of each live document's digest, so
// a write updates it by subtracting one document's digest and adding another's.
// Pure.
package canon

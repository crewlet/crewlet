package objstore

import "fmt"

// SizeWords is n bytes as a person reads a limit on stored bytes: in GiB or MiB
// where it is a whole number of them, in bytes otherwise — a project's file
// cap, an artifact's asset store.
//
// HERE, beside the store whose bytes every such limit counts, because two
// surfaces word one: a person told "104857601 bytes, and at most 104857600"
// reads two numbers they have to divide, and each surface kept its own copy
// of the conversion.
func SizeWords(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

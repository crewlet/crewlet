//go:build !unix

package sandbox

import "os"

// openHostRegular is refused where the local backend is: see
// [localSupported]. The provider refuses at construction, so nothing reaches
// this on a supported deployment.
func openHostRegular(string) (*os.File, error) {
	return nil, localErrorf("%s", unsupportedReason)
}

// openHostWritable is refused for the same reason as [openHostRegular].
func openHostWritable(string) (*os.File, error) {
	return nil, localErrorf("%s", unsupportedReason)
}

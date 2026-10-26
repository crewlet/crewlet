//go:build !unix

package sandbox

import "os"

// openRegularIn is refused where the local backend is: see
// [localSupported]. The provider refuses at construction, so nothing reaches
// this on a supported deployment.
func openRegularIn(*os.Root, string) (*os.File, error) {
	return nil, localErrorf("%s", unsupportedReason)
}

// openWritableIn is refused for the same reason as [openRegularIn].
func openWritableIn(*os.Root, string) (*os.File, error) {
	return nil, localErrorf("%s", unsupportedReason)
}

//go:build !unix

package sandbox

import (
	"io"
	"os"
)

// openRegularIn is refused where the local backend is: see
// [localSupported]. The provider refuses at construction, so nothing reaches
// this on a supported deployment.
func openRegularIn(*os.Root, string) (*os.File, error) {
	return nil, localErrorf("%s", unsupportedReason)
}

// replaceIn is refused for the same reason as [openRegularIn].
func replaceIn(*os.Root, string, []byte) error {
	return localErrorf("%s", unsupportedReason)
}

// replaceFrom is refused for the same reason as [openRegularIn].
func replaceFrom(*os.Root, string, io.Reader) error {
	return localErrorf("%s", unsupportedReason)
}

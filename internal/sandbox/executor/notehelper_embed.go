package executor

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
)

// The note helper's builds, one per box arch (internal/notehelper): committed
// generated files `make notehelper` writes and `make notehelper-check`
// holds to the source, gzipped so the engine binary carries less than half
// their size.
var (
	//go:embed notehelper/notehelper-linux-amd64.gz
	noteHelperAMD64 []byte
	//go:embed notehelper/notehelper-linux-arm64.gz
	noteHelperARM64 []byte
)

// noteHelperBinary is the note helper's executable for a linux box of arch,
// by GOARCH's names ("amd64", "arm64"), gunzipped: written into the box at
// each launch, since gunzip is not certain to be in a box.
func noteHelperBinary(arch string) ([]byte, error) {
	var gz []byte
	switch arch {
	case "amd64":
		gz = noteHelperAMD64
	case "arm64":
		gz = noteHelperARM64
	default:
		return nil, fmt.Errorf("executor: no note helper is built for a box of arch %q; the builds are amd64 and arm64", arch)
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("executor: the embedded note helper for %s is not gzip: %w", arch, err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("executor: the embedded note helper for %s does not gunzip: %w", arch, err)
	}
	return out, nil
}

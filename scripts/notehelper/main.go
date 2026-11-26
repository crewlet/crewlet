// Command notehelper builds the note helper for every box arch into a
// directory, by internal/notehelper/helperbuild's recipe:
//
//	go run ./scripts/notehelper -out build/notehelper
//
// `make notehelper` (beside ./crewlet), `make install`, `make image-context`
// and goreleaser's before hook (.goreleaser.yaml) run it, so the release archives, the image and a
// checkout carry builds of one recipe. It builds with the go that runs it
// (CREWLET_GO, else PATH's), the toolchain that builds the engine beside it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
)

func main() {
	out := flag.String("out", "", "the directory to write the builds into (required)")
	flag.Parse()
	if *out == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: notehelper -out DIR")
		os.Exit(2)
	}
	if err := helperbuild.Build(context.Background(), helperbuild.GoCommand(), *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

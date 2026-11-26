// Command notehelper builds the note helper for every box arch into a
// directory, by internal/notehelper/helperbuild's recipe:
//
//	go run ./scripts/notehelper -out build/notehelper
//
// `make notehelper` (beside ./crewlet), `make install`, `make image-context`
// and goreleaser's before hook (.goreleaser.yaml) run it, so the release
// archives, the image and a checkout carry builds of one recipe. It builds
// with the go that runs it (CREWLET_GO, else PATH's), the toolchain that
// builds the engine beside it.
//
//	go run ./scripts/notehelper -digest
//
// rewrites the committed digest of the helper's source
// (internal/notehelper/helpersource), run from the module's root after a
// change to that source, which helperbuild's suite otherwise fails on.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
)

func main() {
	out := flag.String("out", "", "the directory to write the builds into")
	digest := flag.Bool("digest", false, "rewrite the helper's source digest, "+helperbuild.DigestFile)
	flag.Parse()
	if (*out == "") == !*digest || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: notehelper -out DIR | notehelper -digest")
		os.Exit(2)
	}
	if err := run(context.Background(), *out, *digest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out string, digest bool) error {
	if !digest {
		return helperbuild.Build(ctx, helperbuild.GoCommand(), out)
	}
	d, err := helperbuild.SourceDigest(ctx, helperbuild.GoCommand())
	if err != nil {
		return err
	}
	return helperbuild.WriteDigest(".", d)
}

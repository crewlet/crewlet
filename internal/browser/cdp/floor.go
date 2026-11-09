package cdp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MinMajor is the oldest Chromium major the engine drives: a browser that
// answers Browser.getVersion with an older one is refused ([CheckVersion]).
//
// It is the newer of two bounds, each measured rather than chosen:
//
//   - The oldest major whose protocol describes every command, event and
//     field this package declares (its inventory): 142. The newest of them
//     by the protocol's history is Storage.getStorageKey, first described
//     in 142 (it replaces getStorageKeyForFrame, which both pins deprecate);
//     before it came TargetInfo's parentFrameId (141), Page.navigate's
//     isDownload (140) and Browser.downloadProgress' filePath (139). Read
//     on 2026-11-09 from the devtools-protocol package's description at
//     each major's Chrome for Testing revision, for every major from 138
//     to 145 and a dozen older ones back to 113.
//   - The oldest major Chrome for Testing publishes for all four platforms
//     the engine is released for: 153, its first build for linux-arm64
//     (Chrome for Testing's per-milestone listing, read 2026-11-08 and again
//     2026-11-10). A floor a supported development platform cannot fetch is
//     a gate that platform skips.
//
// The second is the newer, so it set the floor. scripts/chromium-pin.env
// pins the newest patch of that major beside Chrome for Testing's Stable. No
// build older than 153 was driven: none of them could be the floor.
//
// Raising it is argued here with what was measured, and moves the floor pin to
// the newest patch of the new major in the same change:
// TestThePinsMeetTheFloor fails while they disagree.
const MinMajor = 153

// Version is what a browser says it is: its product (Browser.getVersion's
// "HeadlessChrome/158.0.8127.18", "Chrome/…", "Chromium/…") and the major
// version read from it.
type Version struct {
	Product string
	Major   int
}

// CheckVersion asks the browser behind s what it is, and refuses one older
// than [MinMajor] with a [*VersionError] naming both versions. A product
// whose major cannot be read is refused too: a browser that cannot say what it
// is cannot be held to the floor.
func CheckVersion(ctx context.Context, s *Session) (Version, error) {
	r, err := Call(ctx, s, BrowserGetVersion, struct{}{})
	if err != nil {
		return Version{}, err
	}
	v, err := parseProduct(r.Product)
	if err != nil {
		return Version{}, err
	}
	if v.Major < MinMajor {
		return v, &VersionError{Product: v.Product, Major: v.Major}
	}
	return v, nil
}

// parseProduct reads the major version out of a Browser.getVersion product:
// the first number of the part after the slash.
func parseProduct(product string) (Version, error) {
	_, version, ok := strings.Cut(product, "/")
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if !ok || err != nil || n <= 0 {
		return Version{}, fmt.Errorf("cdp: the browser calls itself %q, which names no major version "+
			"(a product such as Chrome/157.0.8103.38)", product)
	}
	return Version{Product: product, Major: n}, nil
}

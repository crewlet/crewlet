package cdp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/browser"
)

// Version is what a browser says it is: its product (Browser.getVersion's
// "HeadlessChrome/158.0.8127.18", "Chrome/…", "Chromium/…") and the major
// version read from it.
type Version struct {
	Product string
	Major   int
}

// CheckVersion asks the browser behind s what it is, and refuses one older
// than [browser.MinChromiumMajor] with a [*VersionError] naming both versions. A product
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
	if v.Major < browser.MinChromiumMajor {
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

package oauth

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A VENDOR'S WORDS ARE THE OPENING OF THEIR WHOLE REDACTION: what is kept of
// an authorization server's text, and of an endpoint an error names, is the
// cut of the whole text redacted, wherever a credential sits against the cut.
func TestAVendorsWordsAreTheOpeningOfTheirWholeRedaction(t *testing.T) {
	t.Parallel()
	for _, s := range redacttest.Shapes() {
		for start := maxVendorWords - len(s.Sample) + 1; start < maxVendorWords; start++ {
			said := strings.Repeat("w", start-1) + " " + s.Sample + " and more"
			want := textcut.Bytes(redact.Secrets(strings.Join(strings.Fields(said), " ")), maxVendorWords)
			if got := vendorWords(said); got != want {
				t.Fatalf("%s at byte %d: kept %q; want %q", s.Name, start, got, want)
			}
		}
		if strings.ContainsAny(s.Sample, " \n") {
			continue
		}
		const host = "http://idp.example.com/"
		for start := maxVendorWords - len(s.Sample) + 1; start < maxVendorWords; start++ {
			raw := host + strings.Repeat("p", start-len(host)-1) + "/" + s.Sample + "/authorize"
			ep := &mcp.OAuthEndpoints{Server: "https://mcp.example.com/", Authorization: raw}
			_, err := AuthorizeURL(ep, Client{ID: "c"}, "r", "s", "c", nil)
			want := textcut.Bytes(redact.URL(raw), maxVendorWords)
			if err == nil || !strings.Contains(err.Error(), " "+want+" ") {
				t.Fatalf("%s at byte %d: refused with %v; want it to name %q", s.Name, start, err, want)
			}
		}
	}
}

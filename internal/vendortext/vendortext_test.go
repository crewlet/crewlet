package vendortext_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/vendortext"
)

const keys = "https://appstoreconnect.apple.com/access/integrations/api"

func TestATextTravelsAsItsPartsSoTheDashboardDrawsEachVendorName(t *testing.T) {
	t.Parallel()
	text := vendortext.Of(vendortext.W("Open "),
		vendortext.UI("Users and Access", "Integrations", "Team Keys").At(keys), vendortext.W("."))
	got, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"text":"Open "},{"ui":["Users and Access","Integrations","Team Keys"],"url":"` + keys + `"},{"text":"."}]`
	if string(got) != want {
		t.Errorf("wire:\n got %s\nwant %s", got, want)
	}
	if s := text.String(); s != "Open Users and Access > Integrations > Team Keys." {
		t.Errorf("String() = %q", s)
	}
	if u := text.URLs(); len(u) != 1 || u[0] != keys {
		t.Errorf("URLs() = %v", u)
	}
}

func TestPlainWordsOfNothingAreLeftOffTheWire(t *testing.T) {
	t.Parallel()
	type field struct {
		Hint vendortext.Text `json:"hint,omitempty"`
	}
	got, _ := json.Marshal(field{Hint: vendortext.Plain("")})
	if string(got) != "{}" {
		t.Errorf("an empty hint went on the wire: %s", got)
	}
}

func TestCheckRefusesWhatASentenceUsedToMarkByHand(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		text vendortext.Text
		want string
	}{
		"link mark":     {vendortext.Plain("Shown on {link}."), "{link}"},
		"markdown link": {vendortext.Plain("At [the apps page](https://example.com), choose it."), "]("},
		"bare address":  {vendortext.Plain("Open https://example.com."), "https://"},
		"code path":     {vendortext.Plain("Under `Basic Information > App Credentials`."), "Basic Information"},
		"bold":          {vendortext.Plain("Press **Add**."), "**"},
		"both":          {vendortext.Text{{Text: "Add", UI: []string{"Add"}}}, "both"},
		"empty level":   {vendortext.Of(vendortext.UI("Settings", "")), "empty"},
		"plain http":    {vendortext.Of(vendortext.W("page").At("http://example.com")), "https"},
	} {
		err := tc.text.Check()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Check() = %v, want an error naming %q", name, err, tc.want)
		}
	}
	ok := vendortext.Of(vendortext.W("Leave `https://gitlab.com`, then press "), vendortext.UI("Generate"), vendortext.W(" on "),
		vendortext.W("the keys page").At(keys), vendortext.W("."))
	if err := ok.Check(); err != nil {
		t.Errorf("a text of the one form was refused: %v", err)
	}
}

func TestAndSaysNamesAsAListInASentence(t *testing.T) {
	t.Parallel()
	for want, names := range map[string][]string{
		"":                     nil,
		"Edit":                 {"Edit"},
		"Edit and Read":        {"Edit", "Read"},
		"Edit, Read and Purge": {"Edit", "Read", "Purge"},
	} {
		got := vendortext.Of(vendortext.And(names...)...)
		if got.String() != want || len(got.Names()) != len(names) {
			t.Errorf("And(%v) = %q with %d names, want %q", names, got, len(got.Names()), want)
		}
	}
}

package googlecloud_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// guideTexts is every text the guide says, each step's words, result and
// troubles in order.
func guideTexts() []vendortext.Text {
	var out []vendortext.Text
	for _, step := range googlecloud.Guide().Prepare {
		out = append(out, step.Text, step.Result)
		for _, trouble := range step.Trouble {
			out = append(out, trouble.Sign, trouble.Fix)
		}
	}
	return out
}

// codeSpan is a value the guide quotes for a person to type or to recognise.
var codeSpan = regexp.MustCompile("`([^`]+)`")

// quoted is every value the guide quotes, in order.
func quoted() []string {
	var out []string
	for _, text := range guideTexts() {
		for _, m := range codeSpan.FindAllStringSubmatch(text.String(), -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// fullBlock is the block with every optional part on, as statements.
func fullBlock(t *testing.T) []string {
	t.Helper()
	_, pemText := fakeKey(t)
	text, err := googlecloud.CloudShellBlock(testCompany, identity, pemText, true)
	if err != nil {
		t.Fatal(err)
	}
	return statements(text)
}

// THE STEPS QUOTE ONLY WHAT THE BLOCK PRINTS: the line a step says the block
// stops with opens as the block's own stop line does, and every such line a
// step quotes in full is one the block can print; the provider a step shows
// is the name the block's last command prints, in the shape the provider
// field takes. Mutation: rename the block's stop prefix, and the step
// quoting it fails.
func TestTheStepsQuoteWhatTheBlockPrints(t *testing.T) {
	t.Parallel()
	block := fullBlock(t)
	joined := strings.Join(block, "\n")
	if !strings.Contains(joined, `echo "`+googlecloud.StopPrefix) {
		t.Fatalf("the block never prints a line opening %q", googlecloud.StopPrefix)
	}
	stops := 0
	for _, q := range quoted() {
		what, ok := strings.CutPrefix(q, googlecloud.StopPrefix)
		if !ok {
			continue
		}
		stops++
		if what == "..." {
			continue
		}
		reason, failed := strings.CutPrefix(what, "could not ")
		if failed && !strings.Contains(joined, `fail "`+reason+`"`) || !failed && !strings.Contains(joined, what) {
			t.Errorf("a step quotes %q, and the block never stops with it", q)
		}
	}
	if stops == 0 {
		t.Error("no step quotes the line the block stops with, so a person cannot tell a stop from its fix")
	}
	shown := googlecloud.ProviderName(googlecloud.ProjectNumber)
	if !slices.Contains(quoted(), shown) {
		t.Errorf("no step shows the provider the block prints, %s", shown)
	}
	last := block[len(block)-2]
	if !strings.HasPrefix(last, "gcloud iam workload-identity-pools providers describe "+googlecloud.ProviderID+" ") ||
		!strings.HasSuffix(last, "--format='value(name)'") || block[len(block)-1] != ")" {
		t.Errorf("the block's last command is %q, not the provider's full name the steps say it prints", last)
	}
	if !config.GoogleCloudProvider(googlecloud.ProviderName("123456789012")) {
		t.Error("the name the block prints is one the provider field refuses")
	}
}

// THE BLOCK READIES THE PROJECT THE FORM NAMES: the identity project typed is
// the one the block makes everything in, as the provisioner the steps say.
func TestTheBlockReadiesTheProjectTheFormNames(t *testing.T) {
	t.Parallel()
	joined := strings.Join(fullBlock(t), "\n")
	if !strings.Contains(joined, "IDENTITY="+identity) || !strings.Contains(joined, "PROV="+provisioner) {
		t.Error("the block does not make its accounts in the identity project the form names")
	}
}

// EVERY FIELD A STEP IN CREWLET NAMES IS ON THE FORM: a UI name in a step
// taken here that names no field is a control, which the dashboard's suite
// holds to the form (testdata/setup_steps.json).
func TestEveryFieldAStepNamesIsOnTheForm(t *testing.T) {
	t.Parallel()
	labels := map[string]bool{}
	for _, r := range googlecloud.Requirements(testCompany, nil, nil) {
		labels[r.Label] = true
	}
	for i, step := range googlecloud.Guide().Prepare {
		if step.Yields == "" {
			continue
		}
		var yielded string
		for _, r := range googlecloud.Requirements(testCompany, nil, nil) {
			if r.Field == step.Yields {
				yielded = r.Label
			}
		}
		names := append(step.Text.Names(), step.Result.Names()...)
		if !slices.Contains(names, yielded) {
			t.Errorf("step %d yields %s and does not name its field, %q", i+1, step.Yields, yielded)
		}
	}
}

// heading is a markdown heading line.
var heading = regexp.MustCompile(`(?m)^#{2,4} (.+)$`)

// anchorOf is a heading's anchor as the docs site renders it.
func anchorOf(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// docsPage is docs/integrations/google-cloud.md.
func docsPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "integrations", "google-cloud.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// EVERY DOCS SECTION A STEP LINKS IS ON THE PAGE, so a renamed heading cannot
// leave a step pointing at nothing.
func TestEveryDocsSectionTheStepsLinkIsOnThePage(t *testing.T) {
	t.Parallel()
	page := docsPage(t)
	anchors := map[string]bool{}
	for _, m := range heading.FindAllStringSubmatch(page, -1) {
		anchors[anchorOf(m[1])] = true
	}
	linked := 0
	for _, text := range guideTexts() {
		for _, url := range text.URLs() {
			anchor, ok := strings.CutPrefix(url, googlecloud.DocsURL+"#")
			if !ok {
				continue
			}
			linked++
			if !anchors[anchor] {
				t.Errorf("a step links %s, and the page has no such section", url)
			}
		}
	}
	if linked == 0 {
		t.Error("no step links the docs, so preparing a project and undoing the block are said nowhere")
	}
}

// section is the docs section under title, to the next heading.
func section(t *testing.T, page, title string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, "### "+title+"\n")
	if !ok {
		t.Fatalf("the page has no section %q", title)
	}
	if at := heading.FindStringIndex(rest); at != nil {
		rest = rest[:at[0]]
	}
	return rest
}

// THE UNDO SECTION REMOVES WHAT THE BLOCK MAKES: every account, pool,
// provider, custom role, role grant and identity API the block makes or turns
// on is named there. Mutation: add a role grant to the block, and this fails
// until the section says how to remove it.
func TestTheUndoSectionNamesEverythingTheBlockMakes(t *testing.T) {
	t.Parallel()
	undo := section(t, docsPage(t), "Undo the Cloud Shell block")
	joined := strings.Join(fullBlock(t), "\n")
	var made []string
	for _, pattern := range []string{
		`service-accounts create (\S+)`, `workload-identity-pools create (\S+)`, `create-oidc (\S+)`,
		`roles create (\S+)`, `(roles/[A-Za-z.]+)`, `projects create (\S+)`,
	} {
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(joined, -1) {
			made = append(made, m[1])
		}
	}
	for _, m := range regexp.MustCompile(`services enable ([a-z0-9. ]+?) --project="\$IDENTITY"`).
		FindAllStringSubmatch(joined, -1) {
		made = append(made, strings.Fields(m[1])...)
	}
	for _, thing := range made {
		switch {
		case thing == `"$IDENTITY"`:
			thing = `gcloud projects delete "$IDENTITY"`
		}
		if !strings.Contains(undo, thing) {
			t.Errorf("the block makes %s, and the undo section does not name it", thing)
		}
	}
}

package artifactsapi_test

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/datasetsapi"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/presence"
)

// THE DASHBOARD SPEAKS THE ARTIFACTS' VOCABULARY, IN THE ENGINE'S ORDER.
//
// The gallery filters by kind and scope and sorts by what the listing sorts
// by, the share dialog grants the roles a share may hold to the audiences a
// share may name, the viewer embeds a frame by the origin mode the engine
// minted it in, the presence chrome draws what a member is doing, and the
// history and the company trail word every op a history row is filed under. Every
// one of those is a closed set the engine owns, so `contract/artifacts.ts`
// (and `contract/brand.ts`, for a brand kit's) declares each once and this
// holds it: a value the engine grew that the
// dashboard lacks is a kind it cannot draw or a role nobody can grant; one the
// dashboard keeps that the engine dropped is a choice every write refuses.
// The ORDER is held too, because the role ladder is an order (each includes
// the ones before it) and the dashboard draws the others in the engine's.
// contractFile is the dashboard module a gated name is declared in: a brand
// kit's rows in `contract/brand.ts`, which only the brand's lazy screens read,
// every other artifacts row in `contract/artifacts.ts`.
func contractFile(name string) string {
	if strings.HasPrefix(name, "BRAND_") {
		return "contract/brand.ts"
	}
	return "contract/artifacts.ts"
}

func TestTheDashboardSpeaksTheArtifactsVocabulary(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"ARTIFACT_KINDS":        names(artifacts.Kinds),
		"ARTIFACT_CAPABILITIES": names(artifacts.Capabilities),
		"ARTIFACT_ROLES":        names(artifacts.Roles),
		"SHAREABLE_ROLES":       names(artifacts.ShareableRoles),
		"AUDIENCE_KINDS":        names(artifacts.AudienceKinds),
		"ORIGIN_MODES":          names(artifacts.OriginModes),
		"PRESENCE_ACTIVITIES":   names(presence.Activities),
		"ARTIFACT_LIST_SCOPES":  names(artifactsdomain.ListScopes),
		"ARTIFACT_LIST_SORTS":   names(artifactsdomain.ListSorts),
		"ARTIFACT_HISTORY_OPS":  names(artifactsdomain.HistoryOps),

		"ARTIFACT_FOLDER_VISIBILITIES": names(artifactsdomain.FolderVisibilities),
		"BRAND_KIT_FACES":              brandformat.KitFaces,
		"BRAND_SYSTEM_STACKS":          brandformat.SystemStacks,
		"BRAND_FONT_STYLES":            brandformat.FontStyles,
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		client := clientsource.Strings(body)
		if len(client) == 0 {
			t.Fatalf("%s names nothing, so this gate certifies nothing", name)
		}
		if !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q, in that order: "+
				"change %s to the engine's", name, client, engine, contractFile(name))
		}
	}
}

// THE DASHBOARD DRAWS EACH KIND AS THE KIND TABLE SAYS.
//
// The View tab, the side-by-side compare and the Data tab ask the kind's row
// rather than its name, so a row that disagreed with the engine's would frame
// a native kind the origin refuses to serve, draw a framed one here, or hide a
// datastore the engine keeps. Every kind is a row, in the engine's order, and
// each row's render and store are the engine's.
func TestTheDashboardDrawsEachKindAsTheKindTableSays(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ARTIFACT_KIND_SPECS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, names(artifacts.Kinds)) {
		t.Fatalf("ARTIFACT_KIND_SPECS is keyed %q and the engine's kinds are %q, in that order",
			keys, names(artifacts.Kinds))
	}
	for _, spec := range artifacts.KindSpecs() {
		row, err := clientsource.Property(body, string(spec.Kind))
		if err != nil {
			t.Fatal(err)
		}
		for field, engine := range map[string]string{
			"render": string(spec.Render), "store": string(spec.Store),
		} {
			if got := clientsource.Field(row, field); !slices.Equal(got, []string{engine}) {
				t.Errorf("ARTIFACT_KIND_SPECS.%s.%s is %q and the engine's is %q",
					spec.Kind, field, got, engine)
			}
		}
	}
}

// THE DASHBOARD BOUNDS WHAT A PERSON TYPES, AND WHAT IT TELLS A ROOM, AT THE
// ENGINE'S CAPS.
//
// The settings form and the version label dialog stop a title, a description,
// an icon word or a label at these, so a person is told at the field rather
// than refused after Save; a bound looser than the engine's is a form that
// accepts what the engine will refuse. And the presence chrome sends at most
// as many frames a second as a socket's room takes: the engine drops the rest,
// so a faster client spends its own CPU telling the room nothing.
func TestTheDashboardBoundsArtifactTextAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"ARTIFACT_TITLE_MAX_BYTES":       artifacts.MaxTitleBytes,
		"ARTIFACT_DESCRIPTION_MAX_BYTES": artifacts.MaxDescriptionBytes,
		"ARTIFACT_ICON_MAX_BYTES":        artifacts.MaxIconBytes,
		"ARTIFACT_LABEL_MAX_BYTES":       artifacts.MaxLabelBytes,
		"PRESENCE_FRAMES_PER_SECOND":     presence.ClientRate,
		"ARTIFACT_CHECK_MAX_FILES":       artifactupload.MaxCheckFiles,
		"ARTIFACT_CHECK_MAX_BYTES":       artifactupload.MaxCheckBytes,
		"DOC_MAX_TABS":                   docindex.MaxTabs,
		"DOC_TAB_TITLE_MAX_BYTES":        docindex.MaxTitleBytes,
		"DECK_PLAN_MAX_SLIDES":           slideformat.MaxPlanSlides,
		"DECK_SLIDE_TITLE_MAX_BYTES":     slideformat.MaxSlideTitleBytes,
		"ARTIFACT_TRASH_RETENTION_DAYS":  int(artifacts.TrashRetention / (24 * time.Hour)),
		"ARTIFACT_FOLDER_NAME_MAX_BYTES": artifacts.MaxFolderNameBytes,
		"ARTIFACT_FOLDER_MAX_DEPTH":      artifacts.MaxFolderDepth,
		"ARTIFACT_MOVE_MAX_ITEMS":        artifacts.MaxMovesPerCall,
		"BRAND_NAME_MAX_BYTES":           brandformat.MaxNameBytes,
		"BRAND_TAGLINE_MAX_BYTES":        brandformat.MaxTaglineBytes,
		"BRAND_VOICE_SUMMARY_MAX_BYTES":  brandformat.MaxVoiceSummaryBytes,
		"BRAND_VOICE_MAX_EXAMPLES":       brandformat.MaxVoiceExamples,
		"BRAND_VOICE_EXAMPLE_MAX_BYTES":  brandformat.MaxVoiceExampleBytes,
		"BRAND_FAMILY_NAME_MAX_BYTES":    brandformat.MaxFamilyNameBytes,
		"BRAND_FONT_FILES_MAX":           brandformat.MaxFontFilesPerFamily,
		"BRAND_FONT_FILE_MAX_BYTES":      brandformat.MaxFontFileBytes,
		"BRAND_SVG_MAX_BYTES":            brandformat.MaxSVGBytes,
		"BRAND_IMAGE_MAX_BYTES":          brandformat.MaxImageBytes,
		"BRAND_IMAGE_MAX_SIDE":           brandformat.MaxImageSide,
		"BRAND_FONT_WEIGHT_MIN":          brandformat.MinFontWeight,
		"BRAND_FONT_WEIGHT_MAX":          brandformat.MaxFontWeight,
		"BRAND_FONT_WEIGHT_STEP":         brandformat.FontWeightStep,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d and the engine at %d: change "+
				"%s to the engine's figure", name, client, engine, contractFile(name))
		}
	}
}

// THE DASHBOARD LISTS FOLDERS WITH THE ENGINE'S DELIMITER: a folder page asks
// for one level with `delimiter`, and a delimiter the engine does not take is a
// listing refused with a 400.
func TestTheDashboardListsWithTheEnginesDelimiter(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "ARTIFACT_LIST_DELIMITER")
	if err != nil {
		t.Fatal(err)
	}
	if got != artifactsdomain.ListDelimiter {
		t.Errorf("the dashboard lists with delimiter %q and the engine takes %q", got,
			artifactsdomain.ListDelimiter)
	}
}

// THE DASHBOARD JUDGES A BRAND'S CONTRAST AS THE ADOPTION DOES: the Brand
// page's badges block a Save on the pair that refuses an adoption, so they
// measure the same pairs, in the same order, on the same fallbacks. A pair the
// page skips is a Save it allows and an adoption refuses; a fallback that
// differs is a badge passing a text the adoption measures as unreadable.
func TestTheDashboardJudgesBrandContrastAsTheAdoptionDoes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	fallbacks, err := clientsource.Literal(tree, "BRAND_COLOR_FALLBACKS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(fallbacks)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"text": brandformat.Fallbacks.Text, "light_ground": brandformat.Fallbacks.LightGround,
		"dark_ground": brandformat.Fallbacks.DarkGround, "dark_text": brandformat.Fallbacks.DarkText,
	}
	if len(keys) != len(want) {
		t.Errorf("the dashboard declares fallbacks %q and the engine fills %d", keys, len(want))
	}
	for role, hex := range want {
		if got := clientsource.Scalars(fallbacks, role); len(got) != 1 || got[0] != hex {
			t.Errorf("the dashboard fills %s with %q and the engine with %q", role, got, hex)
		}
	}

	pairs, err := clientsource.Literal(tree, "BRAND_CONTRAST_PAIRS")
	if err != nil {
		t.Fatal(err)
	}
	var fg, ground, floor, refuses []string
	for _, p := range brandformat.ContrastPairs {
		fg = append(fg, p.Foreground)
		ground = append(ground, p.Ground)
		floor = append(floor, strconv.FormatFloat(p.Min, 'f', -1, 64))
		refuses = append(refuses, strconv.FormatBool(p.Refuses))
	}
	for key, want := range map[string][]string{
		"foreground": fg, "ground": ground, "min": floor, "refuses": refuses,
	} {
		if got := clientsource.Scalars(pairs, key); !slices.Equal(got, want) {
			t.Errorf("the dashboard's pairs read %s %q and the engine's %q", key, got, want)
		}
	}
}

// THE DASHBOARD DRAWS THE FACES THE BRAND STYLESHEET NAMES: the Brand page's
// preview sets each face from the dashboard's table, and every artifact page
// from the generated stylesheet, so a list that differs is a preview showing a
// face no page shows.
func TestTheDashboardDrawsTheFacesTheBrandStylesheetNames(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "BRAND_FACE_FAMILIES")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(brandformat.KitFaces), brandformat.SystemStacks...)
	if !slices.Equal(keys, want) {
		t.Errorf("the dashboard lists faces %q and the engine resolves %q", keys, want)
	}
	for _, name := range want {
		if got := clientsource.Scalars(body, name); len(got) != 1 || got[0] != brandformat.FaceFamilies[name] {
			t.Errorf("the dashboard draws %s as %q and the stylesheet as %q", name, got,
				brandformat.FaceFamilies[name])
		}
	}
}

// THE DASHBOARD NAMES A FAMILY BY THE ENGINE'S GRAMMAR, held by behaviour: the
// Typography editor checks an uploaded family's name against the pattern and
// the bound before it uploads a byte, so a name the two answer differently is
// refused after the upload, or refused at the field when the publish would
// take it.
func TestTheDashboardNamesAFamilyByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	source, err := clientsource.Scalar(tree, "BRAND_FAMILY_NAME_PATTERN")
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := regexp.Compile(source)
	if err != nil {
		t.Fatalf("BRAND_FAMILY_NAME_PATTERN does not compile: %v", err)
	}
	for _, name := range []string{
		"Acme", "Acme Sans", "Acme Sans 2", "IBM Plex-Sans", "a", "Z9",
		"", " Acme", "Acme ", "Acme  Sans", "2Acme", "-Acme", "Acme_Sans", "Acmé", "Acme\tSans",
		`A";}*{x:url(//y)}/*`, strings.Repeat("a", brandformat.MaxFamilyNameBytes),
		strings.Repeat("a", brandformat.MaxFamilyNameBytes+1),
	} {
		dashboard := pattern.MatchString(name) && len(name) <= brandformat.MaxFamilyNameBytes
		if engine := brandformat.ValidFamily(name); dashboard != engine {
			t.Errorf("family %q: the dashboard says %v and the engine %v", name, dashboard, engine)
		}
	}
}

// names is a closed set's values as the strings the dashboard declares.
func names[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}

// THE RENDERER DRAWS THE SLIDE SUBSET AND NOTHING ELSE.
//
// The engine refuses a slide outside the subset at publish, and the
// dashboard's element-constructing renderer allows the same elements,
// properties, drawing elements and attributes again, so a tree that reached
// it some other way is drawn as text rather than as whatever it names. The two
// lists are one list: a property the engine admits that the renderer drops
// would publish and then not show, and one the renderer draws that the engine
// refuses is an allowlist wider than its source.
func TestTheRendererDrawsTheSlideSubset(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"SLIDE_ELEMENTS":         slideformat.Elements(),
		"SLIDE_STYLE_PROPERTIES": slideformat.StyleProperties(),
		"SLIDE_SVG_ELEMENTS":     slideformat.SVGElements(),
		"SLIDE_SVG_ATTRIBUTES":   slideformat.SVGAttributes(),
		"SLIDE_TRANSITIONS":      slideformat.Transitions,
		"SLIDE_SHAPES":           slideformat.ShapeKinds,
		"SLIDE_ICONS":            slideformat.Icons,
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if client := clientsource.Strings(body); !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q: change contract/artifacts.ts "+
				"to the engine's", name, client, engine)
		}
	}
	for name, want := range map[string]int{"SLIDE_STAGE_WIDTH": slideformat.StageWidth,
		"SLIDE_STAGE_HEIGHT":       slideformat.StageHeight,
		"SLIDE_ICON_GLYPH_PERCENT": slideformat.IconGlyphPercent} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil || got != strconv.Itoa(want) {
			t.Errorf("%s is %q (%v), want %d", name, got, err, want)
		}
	}
}

// THE DASHBOARD DRAWS THE BOARD FORMAT THE ENGINE VALIDATES: every widget type
// a publish admits has a figure, every source kind and refresh mode a rail
// describes, every dataset status an answer carries, every fold and number
// format a stat writes, every column type a cell renders, the grid a board
// lays out on and the shortest interval a dataset refreshes at. A value the
// engine admits that the dashboard lacks is a board that publishes and draws
// nothing; one the dashboard keeps that the engine dropped is dead code.
func TestTheDashboardDrawsTheBoardFormat(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"DASHBOARD_WIDGET_TYPES": names(dashspec.WidgetTypes),
		"DASHBOARD_SOURCE_KINDS": names(dashspec.SourceKinds),
		"DASHBOARD_REFRESH_MODES": names([]dashspec.RefreshMode{dashspec.RefreshLive, dashspec.RefreshSignal,
			dashspec.RefreshInterval, dashspec.RefreshManual, dashspec.RefreshFixed, dashspec.RefreshDerived}),
		"DASHBOARD_DATASET_STATUSES": {datasetsapi.StatusReady, datasetsapi.StatusRefused, datasetsapi.StatusUnanswered},
		"DASHBOARD_AGGS":             names(dashspec.Aggs),
		"DASHBOARD_NUMBER_FORMATS":   names(dashspec.Formats),
		"DASHBOARD_COLUMN_TYPES":     names(dashspec.ColumnTypes),
		"DASHBOARD_COLUMN_WORDS":     names(dashspec.WordSets),
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if client := clientsource.Strings(body); !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q: change contract/artifacts.ts "+
				"to the engine's", name, client, engine)
		}
	}
	for name, want := range map[string]int{"DASHBOARD_COLUMNS": artifacts.DashboardColumns,
		"DASHBOARD_MIN_REFRESH_SECONDS": artifacts.MinRefreshSeconds,
		"DASHBOARD_MAX_WIDGETS":         artifacts.MaxDashboardWidgets,
		"DASHBOARD_MAX_DATASETS":        artifacts.MaxDashboardDatasets,
		"DASHBOARD_MAX_TABLE_COLUMNS":   artifacts.MaxTableColumns} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil || got != strconv.Itoa(want) {
			t.Errorf("%s is %q (%v), want %d", name, got, err, want)
		}
	}
}

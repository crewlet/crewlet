package wire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// closedSets is every closed set the dashboard declares, by the TypeScript
// union that holds it.
func closedSets() map[string][]string {
	return map[string][]string{
		"AssistantEventKind":       strs(wire.EventKinds),
		"AssistantTurnState":       strs(wire.TurnStates),
		"AssistantStatusState":     strs(wire.StatusStates),
		"AssistantStepState":       strs(wire.StepStates),
		"AssistantCardKind":        strs(wire.CardKinds),
		"AssistantProposalState":   strs(wire.ProposalStates),
		"AssistantProposalScope":   strs(wire.ProposalScopes),
		"AssistantProposalRisk":    strs(wire.ProposalRisks),
		"AssistantChangeKind":      strs(wire.ChangeKinds),
		"AssistantWordOp":          strs(wire.WordOps),
		"AssistantColumnType":      strs(wire.ColumnTypes),
		"AssistantAlign":           strs(wire.Aligns),
		"AssistantNoticeTone":      strs(wire.NoticeTones),
		"AssistantErrorCode":       strs(wire.ErrorCodes),
		"AssistantResourceKind":    strs(wire.ResourceKinds),
		"AssistantViewerReason":    strs(wire.ViewerReasons),
		"AssistantActionConfirm":   strs(wire.ActionConfirms),
		"AssistantActionState":     strs(wire.ActionStates),
		"AssistantMessageRole":     strs(wire.MessageRoles),
		"AssistantOpenReason":      strs(wire.OpenReasons),
		"AssistantDraftKind":       strs(wire.DraftKinds),
		"AssistantDraftFieldKind":  strs(wire.DraftFieldKinds),
		"AssistantRecordNoun":      strs(wire.RecordNouns),
		"AssistantTurnMode":        strs(wire.TurnModes),
		"AssistantBriefTone":       strs(wire.BriefTones),
		"AssistantBriefActionKind": strs(wire.BriefActionKinds),
		// --- deep work (im-deep) ---
		"AssistantDeepState":           strs(wire.DeepStates),
		"AssistantAttachmentMediaType": wire.AttachmentMediaTypes,
	}
}

// THE DASHBOARD KNOWS EVERY ASSISTANT CLOSED SET THE ENGINE SENDS, AND NO OTHER
// MEMBER.
//
// A member only the engine knows is a frame, a card or a state the drawer
// draws as nothing, or narrows wrongly; one only the dashboard knows is a
// branch nothing ever takes. Mutation: add a member to any Go set and its
// union fails here until contract/assistant.ts names it.
func TestTheDashboardKnowsEveryAssistantClosedSet(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for union, engine := range closedSets() {
		client, err := clientsource.Union(tree, union)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range engine {
			if !slices.Contains(client, m) {
				t.Errorf("the engine sends %s %q and contract/assistant.ts does not name it",
					union, m)
			}
		}
		for _, m := range client {
			if !slices.Contains(engine, m) {
				t.Errorf("contract/assistant.ts names %s %q, which the engine never sends",
					union, m)
			}
		}
	}
}

// THE DASHBOARD BOUNDS THE ASSISTANT AT THE ENGINE'S OWN LIMITS.
//
// The composer and the frame hold a question and a page context to these
// before anything is sent, so a person is told the bound where they are
// typing; a figure copied there and changed here would hold back what the
// engine takes, or send what it refuses. Mutation: change InputMaxRunes and
// ASSISTANT_INPUT_MAX_RUNES fails.
func TestTheDashboardBoundsTheAssistantAtTheEnginesLimits(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, want := range map[string]string{
		"ASSISTANT_PROTOCOL_VERSION":           strconv.Itoa(wire.ProtocolVersion),
		"ASSISTANT_INPUT_MAX_RUNES":            strconv.Itoa(wire.InputMaxRunes),
		"ASSISTANT_PAGE_CONTEXT_MAX_BYTES":     strconv.Itoa(wire.PageContextMaxBytes),
		"ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES": strconv.Itoa(wire.PageContextMaxResources),
		"ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS":   strconv.Itoa(wire.PageContextMaxAccepts),
		"ASSISTANT_PAGE_DRAFT_MAX_FIELDS":      strconv.Itoa(wire.PageDraftMaxFields),
		"ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES": strconv.Itoa(wire.PageDraftValueMaxRunes),
		"ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES":  strconv.Itoa(wire.PageDraftHelpMaxRunes),
		"ASSISTANT_PAGE_DRAFT_SECRET_PATH":     wire.PageDraftSecretPath,
		"ASSISTANT_AUDIT_REASON_MAX_RUNES":     strconv.Itoa(wire.AuditReasonMaxRunes),
		"ASSISTANT_KEEPALIVE_SECONDS":          strconv.Itoa(wire.KeepaliveSeconds),
		"ASSISTANT_ACTION_CONTEXT_INVALIDATE":  wire.ActionContextInvalidate,
		"ASSISTANT_DIGEST_PREFIX":              wire.DigestPrefix,
		// --- deep work (im-deep) ---
		"ASSISTANT_DEEP_ACTIVITY_LINES":     strconv.Itoa(wire.DeepActivityLines),
		"ASSISTANT_FILE_MAX_BYTES":          strconv.Itoa(wire.FileMaxBytes),
		"ASSISTANT_ATTACHMENTS_PER_TURN":    strconv.Itoa(wire.AttachmentsPerTurn),
		"ASSISTANT_DELIVERED_FILES_PER_RUN": strconv.Itoa(wire.DeliveredFilesPerRun),
	} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("contract/assistant.ts says %s = %s; the engine's is %s", name, got, want)
		}
	}
}

// shapes is every wire shape the dashboard declares, by its TypeScript name.
func shapes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"AssistantFrame":               reflect.TypeFor[wire.Frame](),
		"AssistantConversationEvent":   reflect.TypeFor[wire.ConversationEvent](),
		"AssistantPageContextEvent":    reflect.TypeFor[wire.PageContextEvent](),
		"AssistantPageChip":            reflect.TypeFor[wire.PageChip](),
		"AssistantStatusEvent":         reflect.TypeFor[wire.StatusEvent](),
		"AssistantDeltaEvent":          reflect.TypeFor[wire.DeltaEvent](),
		"AssistantStepEvent":           reflect.TypeFor[wire.StepEvent](),
		"AssistantStepRequest":         reflect.TypeFor[wire.StepRequest](),
		"AssistantRefusal":             reflect.TypeFor[wire.Refusal](),
		"AssistantCardEvent":           reflect.TypeFor[wire.CardEvent](),
		"AssistantActionEvent":         reflect.TypeFor[wire.ActionEvent](),
		"AssistantInvalidate":          reflect.TypeFor[wire.Invalidate](),
		"AssistantDoneEvent":           reflect.TypeFor[wire.DoneEvent](),
		"AssistantErrorEvent":          reflect.TypeFor[wire.ErrorEvent](),
		"AssistantSnapshotEvent":       reflect.TypeFor[wire.SnapshotEvent](),
		"AssistantCard":                reflect.TypeFor[wire.Card](),
		"AssistantTableCard":           reflect.TypeFor[wire.TableCard](),
		"AssistantColumn":              reflect.TypeFor[wire.Column](),
		"AssistantExports":             reflect.TypeFor[wire.Exports](),
		"AssistantLink":                reflect.TypeFor[wire.Link](),
		"AssistantEntityCard":          reflect.TypeFor[wire.EntityCard](),
		"AssistantFact":                reflect.TypeFor[wire.Fact](),
		"AssistantEntityItem":          reflect.TypeFor[wire.EntityItem](),
		"AssistantNoun":                reflect.TypeFor[wire.Noun](),
		"AssistantProposalCard":        reflect.TypeFor[wire.ProposalCard](),
		"AssistantChange":              reflect.TypeFor[wire.Change](),
		"AssistantWord":                reflect.TypeFor[wire.Word](),
		"AssistantReceipt":             reflect.TypeFor[wire.Receipt](),
		"AssistantPageActionRef":       reflect.TypeFor[wire.PageActionRef](),
		"AssistantTechnicalRequest":    reflect.TypeFor[wire.TechnicalRequest](),
		"AssistantFailure":             reflect.TypeFor[wire.Failure](),
		"AssistantOpenInAppCard":       reflect.TypeFor[wire.OpenInAppCard](),
		"AssistantErrorCard":           reflect.TypeFor[wire.ErrorCard](),
		"AssistantNoticeCard":          reflect.TypeFor[wire.NoticeCard](),
		"AssistantOversizeCard":        reflect.TypeFor[wire.OversizeCard](),
		"AssistantBriefCard":           reflect.TypeFor[wire.BriefCard](),
		"AssistantBriefScope":          reflect.TypeFor[wire.BriefScope](),
		"AssistantBriefNeed":           reflect.TypeFor[wire.BriefNeed](),
		"AssistantBriefAction":         reflect.TypeFor[wire.BriefAction](),
		"AssistantPageContext":         reflect.TypeFor[wire.PageContext](),
		"AssistantActionArgs":          reflect.TypeFor[wire.ActionArgs](),
		"AssistantResource":            reflect.TypeFor[wire.Resource](),
		"AssistantResourceRef":         reflect.TypeFor[wire.ResourceRef](),
		"AssistantPageDraft":           reflect.TypeFor[wire.PageDraft](),
		"AssistantDraftField":          reflect.TypeFor[wire.DraftField](),
		"AssistantDraftFileRef":        reflect.TypeFor[wire.DraftFileRef](),
		"AssistantVersioned":           reflect.TypeFor[wire.Versioned](),
		"AssistantTurnRequest":         reflect.TypeFor[wire.TurnRequest](),
		"AssistantTurnInput":           reflect.TypeFor[wire.TurnInput](),
		"AssistantTurnAccepted":        reflect.TypeFor[wire.TurnAccepted](),
		"AssistantProposalDecision":    reflect.TypeFor[wire.ProposalDecision](),
		"AssistantProposalAnswer":      reflect.TypeFor[wire.ProposalAnswer](),
		"AssistantProposalRef":         reflect.TypeFor[wire.ProposalRef](),
		"AssistantActionReport":        reflect.TypeFor[wire.ActionReport](),
		"AssistantActionAnswer":        reflect.TypeFor[wire.ActionAnswer](),
		"AssistantStopAnswer":          reflect.TypeFor[wire.StopAnswer](),
		"AssistantPurgeAnswer":         reflect.TypeFor[wire.PurgeAnswer](),
		"AssistantAuditReadRequest":    reflect.TypeFor[wire.AuditReadRequest](),
		"AssistantConversationList":    reflect.TypeFor[wire.ConversationList](),
		"AssistantConversationCreated": reflect.TypeFor[wire.ConversationCreated](),
		"AssistantConversationSummary": reflect.TypeFor[wire.ConversationSummary](),
		"AssistantTranscript":          reflect.TypeFor[wire.Transcript](),
		"AssistantMessage":             reflect.TypeFor[wire.Message](),
		"AssistantOpenTurn":            reflect.TypeFor[wire.OpenTurn](),
		"AssistantAuditRead":           reflect.TypeFor[wire.AuditRead](),
		"AssistantTurnOutcome":         reflect.TypeFor[wire.TurnOutcome](),
		"AssistantViewerAssistant":     reflect.TypeFor[wire.ViewerAssistant](),
		"AssistantModelNotice":         reflect.TypeFor[wire.ModelNotice](),
		// --- deep work (im-deep) ---
		"AssistantDeepProgress": reflect.TypeFor[wire.DeepProgress](),
		"AssistantFileRef":      reflect.TypeFor[wire.FileRef](),
		"AssistantFileCard":     reflect.TypeFor[wire.FileCard](),
		"AssistantFileStored":   reflect.TypeFor[wire.FileStored](),
	}
}

// member is one JSON member a Go shape carries, and whether it may be absent.
type member struct {
	name     string
	optional bool
}

// membersOf reads a struct's JSON members off its tags, by encoding/json's
// own rules: the tag's name, `omitempty` or `omitzero` as may-be-absent, and a
// field tagged "-" as no member at all.
func membersOf(t *testing.T, typ reflect.Type) []member {
	t.Helper()
	var out []member
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			t.Errorf("%s.%s carries no json tag, so its wire name is whatever Go "+
				"calls it", typ.Name(), f.Name)
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		optional := slices.Contains(strings.Split(opts, ","), "omitempty") ||
			slices.Contains(strings.Split(opts, ","), "omitzero")
		out = append(out, member{name: name, optional: optional})
	}
	return out
}

// THE DASHBOARD SPEAKS THE ASSISTANT PROTOCOL: every shape carries the same
// members, with the same optionality, on both sides.
//
// The drawer is drawn from these interfaces, so a member the engine renames is
// one the drawer reads as absent forever, and a member the engine sends
// optionally that the client declares required is a crash on the first frame
// that leaves it out. Mutation: rename a json tag, or drop an omitempty, and
// its interface fails here.
func TestTheDashboardSpeaksTheAssistantProtocol(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, typ := range shapes() {
		client, err := clientsource.Interface(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		declared := map[string]bool{}
		for _, m := range client {
			declared[m.Name] = m.Optional
		}
		engine := membersOf(t, typ)
		sent := map[string]bool{}
		for _, m := range engine {
			sent[m.name] = m.optional
			optional, ok := declared[m.name]
			switch {
			case !ok:
				t.Errorf("the engine sends %s.%s and contract/assistant.ts %s does not "+
					"declare it", typ.Name(), m.name, name)
			case optional != m.optional:
				t.Errorf("%s.%s is optional on the engine's side %v and on the "+
					"dashboard's %v; an omitempty field is an optional member",
					name, m.name, m.optional, optional)
			}
		}
		for _, m := range client {
			if _, ok := sent[m.Name]; !ok {
				t.Errorf("contract/assistant.ts %s declares %s, which the engine's %s "+
					"never sends", name, m.Name, typ.Name())
			}
		}
	}
}

// carriesJSON reports whether a struct is a wire shape: one whose fields name
// their JSON members. A struct with none, such as the stream decoder, is
// machinery rather than something sent.
func carriesJSON(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		if f.Tag != nil && strings.Contains(f.Tag.Value, `json:"`) {
			return true
		}
	}
	return false
}

// EVERY WIRE SHAPE IS HELD AGAINST THE CLIENT.
//
// The gate above holds the shapes it names, so a struct added to this package
// and left out of its table would reach the browser held by nothing. Read off
// the package's own source rather than a list, so the walk cannot share the
// table's omission. Mutation: add an exported struct to this package and this
// fails until the table names it.
func TestEveryWireShapeIsHeldAgainstTheClient(t *testing.T) {
	t.Parallel()
	held := map[string]bool{}
	for _, typ := range shapes() {
		held[typ.Name()] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil,
			parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, isStruct := ts.Type.(*ast.StructType)
				if !isStruct || !ts.Name.IsExported() || !carriesJSON(st) {
					continue
				}
				found++
				if !held[ts.Name.Name] {
					t.Errorf("wire.%s is a shape the engine sends and no gate holds it "+
						"against contract/assistant.ts: name it in shapes()", ts.Name.Name)
				}
			}
		}
	}
	if found < len(held) {
		t.Fatalf("the walk found %d shapes and the table holds %d, so it read less "+
			"than the package declares", found, len(held))
	}
}

package wiregate

import (
	"reflect"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	// The catalogue registers itself: imported, every payload type is listed.
	_ "github.com/crewlet/crewlet/internal/events/types"
)

// carried is what one opaque field holds: the types whose values it carries
// (by package within the module and name, an unexported one included), which
// the walk then reads as it reads a root, or none of this module's, and why.
type carried struct {
	types  []string
	reason string
}

// event is what a recorded event is on the wire: its envelope, flattened with
// one payload of the catalogue (events.Event's MarshalJSON).
func event() []string { return append([]string{"internal/events.envelope"}, catalogue()...) }

// catalogue is every payload type the events catalogue registers.
func catalogue() []string {
	var out []string
	for _, kind := range events.RegisteredTypes() {
		p, ok := events.PayloadFor(kind)
		if !ok {
			continue
		}
		t := reflect.TypeOf(p)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		out = append(out, strings.TrimPrefix(t.PkgPath(), module)+"."+t.Name())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

const (
	userJSON  = "JSON a person, an agent or a model wrote, whose shape is theirs"
	schema    = "a tool's JSON Schema, as its server declares it"
	modelArgs = "the arguments a model wrote for a tool call"
	webhook   = "a provider's webhook body, as the provider sent it"
	// A newer build's event type this one does not register, kept as written.
	unknownEvent = "the payload of an event type this build does not register, kept as its writer sent it"
)

// liveCall is what each of a live call's slices carries, on the call and on
// the two views of it a reader may be sent.
var liveCall = map[string]carried{
	"elided_calls":    {types: []string{"internal/events/types.ElidedCalls"}},
	"invocations":     {types: []string{"internal/events/types.PhaseInvocation"}},
	"prompt_messages": {types: []string{"internal/events/types.PromptMessage", "internal/events/types.PromptSection"}},
	"round_narration": {reason: "a round's narration, a map of scalars the runner writes"},
	"rounds":          {types: []string{"internal/events/types.PhaseRound"}},
	"running_calls":   {types: []string{"internal/events/types.RunningCall"}},
	"steers":          {types: []string{"internal/events/types.PhaseSteer"}},
	"tool_executions": {types: []string{"internal/mcp.ImageRecord"}},
	"partial_round":   {reason: "the round in flight, a map of scalars and narrations"},
	"trigger":         {types: []string{"internal/events/types.triggerWire"}},
}

// opaqueFields is every opaque field a client receives, keyed by its struct
// and its key on the wire.
var opaqueFields = func() map[string]carried {
	rows := map[string]carried{
		"internal/api/livestate.Envelope.payload":              {types: event()},
		"internal/store.EventRecord.payload":                   {types: event()},
		"internal/api/livestate.overlayView.live_call":         {types: []string{"internal/api/livestate.LiveCall", "internal/api/livestate.leanCall", "internal/api/livestate.withheldCall"}},
		"internal/api/operator.ActAnswer.receipt":              {types: []string{"internal/agent/builtin.markChatReadAnswer"}},
		"internal/api/queries.DecisionItem.run":                {types: []string{"internal/events/types.WorkItem", "internal/decision.Decision", "internal/sandbox.BridgeCall", "internal/sandbox.ElidedCall", "internal/api/queries.toldCall"}},
		"internal/api/queries.Immutable.Answer":                {types: []string{"internal/api/queries.CodePage", "internal/api/queries.CheckLogPage"}},
		"internal/api/queries.ModelCheck.check":                {types: []string{"internal/api/llmapi.CheckAnswer", "internal/api/llmapi.CheckLine"}},
		"internal/api/queries.SettingRow.value":                {reason: "a setting's value: a string, a list of them or a bool"},
		"internal/api/queries.SettingRow.default":              {reason: "a setting's default: a string, a list of them or a bool"},
		"internal/api/stream.Envelope.data":                    {reason: "built only in internal/api/stream, so the walk reads every value it holds as a root"},
		"internal/assistant/wire.PageActionRef.payload":        {reason: modelArgs},
		"internal/assistant/wire.TableCard.rows":               {reason: "a table's cells: strings, numbers and nulls"},
		"internal/assistant/wire.TableCard.total":              {reason: "a table's total row: strings, numbers and nulls"},
		"internal/coord.InsightRecord.payload":                 {types: []string{"internal/insights.recordJSON"}},
		"internal/inbox.Notice.extra":                          {types: []string{"internal/worklet.InboxNotice", "internal/gitlet.InboxNotice", "internal/artifactsdomain.InboxNotice"}},
		"internal/issuereport.Preview.payload":                 {types: []string{"internal/issuereport.Report"}},
		"internal/issuereport.Row.payload":                     {types: []string{"internal/issuereport.Report"}},
		"internal/backup.StreamArtifact.config":                {reason: "nats-server's own stream configuration, as it marshals it"},
		"internal/backup.StreamArtifact.state":                 {reason: "nats-server's own stream state, as it marshals it"},
		"internal/worklet.HistoryEntry.fields":                 {types: []string{"internal/worklet.Delta"}},
		"internal/api/mcpapi.ApprovalAnswer.revision":          {types: []string{"internal/config.Warning", "internal/api/configapi.checkedWarning", "internal/config.Derived"}},
		"internal/api/mcpapi.ChangesAnswer.revision":           {types: []string{"internal/config.Warning", "internal/api/configapi.checkedWarning", "internal/config.Derived"}},
		"internal/api/configapi.Change.from":                   {types: []string{"internal/config.Company"}},
		"internal/api/configapi.Change.to":                     {types: []string{"internal/config.Company"}},
		"internal/advisor.Change.set":                          {types: []string{"internal/org.Schedule"}},
		"internal/advisor.Change.merge":                        {types: []string{"internal/org.Schedule"}},
		"internal/advisor.Moved.was":                           {reason: "a fact's count, a severity or a fix's sentence, each a scalar"},
		"internal/advisor.Moved.now":                           {reason: "a fact's count, a severity or a fix's sentence, each a scalar"},
		"internal/api.RecheckSend.body":                        {types: []string{"internal/config.Company"}},
		"cmd/crewlet.validation.summary":                       {reason: "a validation's counts and boot settings, a map of scalars"},
		"internal/api/mcpapi.HeldAnswer.arguments":             {reason: modelArgs},
		"internal/api/queries.MCPApproval.args":                {reason: modelArgs},
		"internal/api/queries.MCPDefinitionView.input_schema":  {reason: schema},
		"internal/api/queries.MCPDefinitionView.output_schema": {reason: schema},
		"internal/api/queries.MCPToolView.input_schema":        {reason: schema},
		"internal/api/queries.MCPToolView.output_schema":       {reason: schema},
		"internal/api/queries.MemberMCPToolView.input_schema":  {reason: schema},
		"internal/api/queries.MemberMCPToolView.output_schema": {reason: schema},
		"internal/api/queries.ToolRow.input_schema":            {reason: schema},
		"internal/artifactdata.Doc.fields":                     {reason: userJSON},
		"internal/artifactdata.DocDelta.body":                  {reason: userJSON},
		"internal/artifactdata.FieldChange.a":                  {reason: userJSON},
		"internal/artifactdata.FieldChange.b":                  {reason: userJSON},
		"internal/artifactdata.StateEntry.value":               {reason: userJSON},
		"internal/artifacts.Column.default":                    {reason: userJSON},
		"internal/worklet.Task.fields":                         {reason: userJSON},
		"internal/worklet.FieldValue.value":                    {reason: userJSON},
		"internal/config.LLMProvider.overrides":                {reason: userJSON},
		"internal/config.Worker.output":                        {reason: userJSON},
		"internal/learning/memread.ProfileRow.traits":          {reason: userJSON},
		"internal/github.Manifest.hook_attributes":             {reason: "GitHub's app manifest attributes, as GitHub defines them"},
		"internal/events.envelope.payload":                     {reason: unknownEvent},
		"internal/events.Event.payload":                        {reason: unknownEvent},
		"internal/events/types.InboundDelivery.body":           {reason: webhook},
		"internal/events/types.RawWebhook.body":                {reason: webhook},
	}
	for key, row := range liveCall {
		rows["internal/api/livestate.LiveCall."+key] = row
	}
	for _, key := range []string{"elided_calls", "invocations", "prompt_messages", "round_narration", "rounds", "tool_executions"} {
		rows["internal/api/livestate.leanCall."+key] = liveCall[key]
	}
	for _, key := range []string{"invocations", "rounds"} {
		rows["internal/api/livestate.withheldCall."+key] = liveCall[key]
	}
	for _, payload := range []string{"AgentPhaseCompleted", "AgentTurnCompleted", "AgentTurnProgress"} {
		rows["internal/events/types."+payload+".tool_executions"] = liveCall["tool_executions"]
	}
	for _, payload := range []string{"AgentPhaseCompleted", "AgentTurnProgress"} {
		rows["internal/events/types."+payload+".round_narration"] = liveCall["round_narration"]
	}
	rows["internal/events/types.AgentTurnProgress.partial_round"] = liveCall["partial_round"]
	return rows
}()

// documents are the structs whose JSON is a document somebody writes, each
// with every struct its fields reach: a key left out of a document is the
// default its schema publishes, and one that defaults to anything but its
// zero is a pointer or an org.Toggle (CLAUDE.md, "Zero values must be
// meaningful"), so absent and zero are one value by the document's contract.
var documents = map[string]string{
	"internal/config.Company":              "the company's configuration",
	"internal/artifacts/dashspec.Spec":     "a dashboard artifact's spec",
	"internal/artifacts/brandformat.Brand": "a brand artifact's definition",
	"internal/artifacts.Collection":        "an artifact store's schema",
}

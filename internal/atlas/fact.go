package atlas

import (
	"fmt"
	"slices"
	"sync"
)

// Fact is one thing the atlas may say about an entry. A closed list, each with
// its value type, whether an entry holds one value or many, and the kinds it
// applies to.
type Fact string

// The facts, grouped by the kinds that carry them.
const (
	FactName        Fact = "name"
	FactPurpose     Fact = "purpose"
	FactEnvironment Fact = "environment"
	FactImportance  Fact = "importance"

	FactVendor      Fact = "vendor"
	FactCapability  Fact = "capability"
	FactAccountRef  Fact = "account_ref"
	FactAccountName Fact = "account_name"
	FactSite        Fact = "site"
	FactRegion      Fact = "region"
	FactIntegration Fact = "integration"
	FactInboundURL  Fact = "inbound_url"

	FactURL        Fact = "url"
	FactAddress    Fact = "address"
	FactPort       Fact = "port"
	FactContainers Fact = "containers"

	FactEngine        Fact = "engine"
	FactEngineVersion Fact = "engine_version"

	FactDefaultBranch Fact = "default_branch"
	FactVisibility    Fact = "visibility"
	FactStatus        Fact = "status"
	FactPullMirrorOf  Fact = "pull_mirror_of"
	FactPushMirrorTo  Fact = "push_mirror_to"
	FactImportedFrom  Fact = "imported_from"

	FactLastResult Fact = "last_result"
	FactRunner     Fact = "runner"
	FactRequired   Fact = "required"
	// FactPipelineState is whether a pipeline's vendor still runs it.
	FactPipelineState Fact = "pipeline_state"

	FactHarness       Fact = "harness"
	FactModel         Fact = "model"
	FactSignIn        Fact = "sign_in"
	FactChainPosition Fact = "chain_position"

	FactTransport     Fact = "transport"
	FactCommand       Fact = "command"
	FactShared        Fact = "shared"
	FactServerName    Fact = "server_name"
	FactServerVersion Fact = "server_version"
	FactToolCount     Fact = "tool_count"
	FactOAuthIssuer   Fact = "oauth_issuer"
	FactSignInExpires Fact = "sign_in_expires"

	FactCovers          Fact = "covers"
	FactExpires         Fact = "expires"
	FactIssuer          Fact = "issuer"
	FactValidity        Fact = "validity"
	FactCertificateType Fact = "certificate_type"

	FactMonitorState Fact = "monitor_state"
	FactStateSince   Fact = "state_since"
)

// Spec is what one fact admits.
type Spec struct {
	Type ValueType
	// Many is an entry holding several values at once, each its own claim.
	Many bool
	// Words is the closed set a word fact takes; nil for every other type.
	Words []string
	// Kinds is the kinds the fact applies to; nil for every kind.
	Kinds []Kind
	// Meaning is a fact only a person sets (owner, environment, importance,
	// purpose): an integration or an agent can only suggest it.
	Meaning bool
}

// The closed word sets the facts take.
var (
	yesNo        = []string{"yes", "no"}
	environments = []string{"production", "staging", "development", "test"}
	importances  = []string{"critical", "standard", "experimental"}
	onOff        = []string{"on", "off"}
	engines      = []string{"postgres", "mysql", "mariadb", "sqlite", "redis", "valkey", "mongodb",
		"elasticsearch", "opensearch", "clickhouse", "nats", "kafka", "rabbitmq", "other"}
	visibilities = []string{"company", "unit", "public", "private", "internal"}
	statuses     = []string{"importing", "active", "archived"}
	results      = []string{ResultQueued, ResultRunning, ResultPassed, ResultFailed, ResultErrored, ResultCancelled, ResultSkipped}
	runners      = []string{"engine", "external"}
	// A pipeline its vendor has switched off, by hand or for inactivity, is
	// disabled: it still exists, and runs nothing until somebody enables it.
	pipelineStates = []string{PipelineActive, PipelineDisabled}
	signIns        = []string{"subscription", "api_key", "inherit_env", "env"}
	transports     = []string{"stdio", "http"}
	// A certificate's validity, as the vendor reports it folded into five
	// words: pending covers every state on the way to active, failed every
	// step that timed out, and inactive one deactivated, deleted or on its
	// way out.
	validities       = []string{"active", "pending", "failed", "expired", "inactive"}
	certificateTypes = []string{"universal", "advanced", "custom"}
)

// A pipeline's last result: the state of its latest run or deployment. Failed
// is one that ran and did not pass, errored one that could not run to an end.
const (
	ResultQueued    = "queued"
	ResultRunning   = "running"
	ResultPassed    = "passed"
	ResultFailed    = "failed"
	ResultErrored   = "errored"
	ResultCancelled = "cancelled"
	ResultSkipped   = "skipped"
)

// The two states of a pipeline at its vendor.
const (
	PipelineActive   = "active"
	PipelineDisabled = "disabled"
)

// A monitor's state, as its vendor reports it folded into five words: unknown
// covers every state that says nothing about what it watches (skipped,
// ignored, not evaluated yet).
const (
	MonitorOK      = "ok"
	MonitorWarn    = "warn"
	MonitorAlert   = "alert"
	MonitorNoData  = "no_data"
	MonitorUnknown = "unknown"
)

var monitorStates = []string{MonitorOK, MonitorWarn, MonitorAlert, MonitorNoData, MonitorUnknown}

func wordsOf[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, s := range set {
		out[i] = string(s)
	}
	return out
}

var dataKinds = []Kind{KindDatastore, KindQueue}

// specs is what every fact admits, built at first use (see versionRE).
var specs = sync.OnceValue(func() map[Fact]Spec {
	return map[Fact]Spec{
		FactName:        {Type: TypeLabel},
		FactPurpose:     {Type: TypeLabel, Meaning: true},
		FactEnvironment: {Type: TypeWord, Words: environments, Meaning: true},
		FactImportance:  {Type: TypeWord, Words: importances, Meaning: true},

		FactVendor:      {Type: TypeWord, Words: wordsOf(Vendors), Kinds: []Kind{KindAccount, KindModel, KindToolServer}},
		FactCapability:  {Type: TypeWord, Many: true, Words: wordsOf(Capabilities), Kinds: []Kind{KindAccount}},
		FactAccountRef:  {Type: TypeAccountRef, Kinds: []Kind{KindAccount}},
		FactAccountName: {Type: TypeLabel, Kinds: []Kind{KindAccount}},
		FactSite:        {Type: TypeURL, Kinds: []Kind{KindAccount}},
		FactRegion:      {Type: TypeRegion, Many: true, Kinds: []Kind{KindAccount, KindHost, KindDatastore, KindQueue, KindBucket}},
		FactIntegration: {Type: TypeWord, Words: onOff, Kinds: []Kind{KindAccount}},
		FactInboundURL:  {Type: TypeURL, Many: true, Kinds: []Kind{KindAccount}},

		FactURL:        {Type: TypeURL, Many: true, Kinds: []Kind{KindHostname, KindModel, KindToolServer}},
		FactAddress:    {Type: TypeAddress, Many: true, Kinds: []Kind{KindHost}},
		FactPort:       {Type: TypePort, Kinds: []Kind{KindHost, KindDatastore, KindQueue}},
		FactContainers: {Type: TypeWord, Words: yesNo, Kinds: []Kind{KindHost}},

		FactEngine:        {Type: TypeWord, Words: engines, Kinds: dataKinds},
		FactEngineVersion: {Type: TypeVersion, Kinds: dataKinds},

		FactDefaultBranch: {Type: TypeBranch, Kinds: []Kind{KindRepository}},
		FactVisibility:    {Type: TypeWord, Words: visibilities, Kinds: []Kind{KindRepository}},
		FactStatus:        {Type: TypeWord, Words: statuses, Kinds: []Kind{KindRepository}},
		FactPullMirrorOf:  {Type: TypeURL, Kinds: []Kind{KindRepository}},
		FactPushMirrorTo:  {Type: TypeURL, Kinds: []Kind{KindRepository}},
		FactImportedFrom:  {Type: TypeURL, Kinds: []Kind{KindRepository}},

		FactLastResult:    {Type: TypeWord, Words: results, Kinds: []Kind{KindPipeline}},
		FactRunner:        {Type: TypeWord, Words: runners, Kinds: []Kind{KindPipeline}},
		FactRequired:      {Type: TypeWord, Words: yesNo, Kinds: []Kind{KindPipeline}},
		FactPipelineState: {Type: TypeWord, Words: pipelineStates, Kinds: []Kind{KindPipeline}},

		FactHarness:       {Type: TypeLabel, Kinds: []Kind{KindModel}},
		FactModel:         {Type: TypeLabel, Kinds: []Kind{KindModel}},
		FactSignIn:        {Type: TypeWord, Words: signIns, Kinds: []Kind{KindModel}},
		FactChainPosition: {Type: TypeInt, Kinds: []Kind{KindModel}},

		FactTransport:     {Type: TypeWord, Words: transports, Kinds: []Kind{KindToolServer}},
		FactCommand:       {Type: TypeLabel, Kinds: []Kind{KindToolServer}},
		FactShared:        {Type: TypeWord, Words: yesNo, Kinds: []Kind{KindToolServer}},
		FactServerName:    {Type: TypeLabel, Kinds: []Kind{KindToolServer}},
		FactServerVersion: {Type: TypeVersion, Kinds: []Kind{KindToolServer}},
		FactToolCount:     {Type: TypeInt, Kinds: []Kind{KindToolServer}},
		FactOAuthIssuer:   {Type: TypeURL, Kinds: []Kind{KindToolServer}},
		FactSignInExpires: {Type: TypeInstant, Kinds: []Kind{KindToolServer}},

		FactCovers:          {Type: TypeHostname, Many: true, Kinds: []Kind{KindCertificate}},
		FactExpires:         {Type: TypeInstant, Kinds: []Kind{KindCertificate}},
		FactIssuer:          {Type: TypeLabel, Kinds: []Kind{KindCertificate}},
		FactValidity:        {Type: TypeWord, Words: validities, Kinds: []Kind{KindCertificate}},
		FactCertificateType: {Type: TypeWord, Words: certificateTypes, Kinds: []Kind{KindCertificate}},

		FactMonitorState: {Type: TypeWord, Words: monitorStates, Kinds: []Kind{KindMonitor}},
		FactStateSince:   {Type: TypeInstant, Kinds: []Kind{KindMonitor}},
	}
})

// Facts is every fact, in the order a reader is shown them.
var Facts = []Fact{
	FactName, FactPurpose, FactEnvironment, FactImportance,
	FactVendor, FactCapability, FactAccountRef, FactAccountName, FactSite, FactRegion, FactIntegration, FactInboundURL,
	FactURL, FactAddress, FactPort, FactContainers,
	FactEngine, FactEngineVersion,
	FactDefaultBranch, FactVisibility, FactStatus, FactPullMirrorOf, FactPushMirrorTo, FactImportedFrom,
	FactLastResult, FactRunner, FactRequired, FactPipelineState,
	FactHarness, FactModel, FactSignIn, FactChainPosition,
	FactTransport, FactCommand, FactShared, FactServerName, FactServerVersion, FactToolCount,
	FactOAuthIssuer, FactSignInExpires,
	FactCovers, FactExpires, FactIssuer, FactValidity, FactCertificateType,
	FactMonitorState, FactStateSince,
}

// Valid reports whether f is a member of the closed list.
func (f Fact) Valid() bool { _, ok := specs()[f]; return ok }

// Spec is what f admits; the zero Spec for a fact outside the list.
func (f Fact) Spec() Spec {
	s := specs()[f]
	s.Words, s.Kinds = slices.Clone(s.Words), slices.Clone(s.Kinds)
	return s
}

// AppliesTo reports whether an entry of kind k may carry f.
func (f Fact) AppliesTo(k Kind) bool {
	s, ok := specs()[f]
	return ok && (s.Kinds == nil || slices.Contains(s.Kinds, k))
}

// Value reads raw as a value of f, refusing it as f's grammar would, with the
// fact named in the error.
func (f Fact) Value(raw string) (Value, error) {
	s, ok := specs()[f]
	if !ok {
		return Value{}, fmt.Errorf("%w: %q is not a fact the atlas keeps", ErrInvalid, f)
	}
	v, err := Parse(s.Type, raw, s.Words)
	if err != nil {
		return Value{}, fmt.Errorf("%s: %w", f, err)
	}
	return v, nil
}

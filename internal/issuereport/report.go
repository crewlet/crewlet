package issuereport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/version"
)

// SchemaVersion is the report format this build writes, and the relay reads it
// to pick the catalogue that holds the report.
const SchemaVersion = 2

// maxOccurrenceCount caps a report's count: past it, the number says nothing.
const maxOccurrenceCount = 10_000

// maxFrames bounds the frames a report carries: the place and a few callers are
// what a maintainer opens, and the rest of a stack adds names and no more help.
const maxFrames = 5

// maxFrameLine bounds a frame's line: past any file this module will hold.
const maxFrameLine = 1_000_000

// A Report is one typed record: what goes to the relay, and nothing else does.
// EVERY STRING IN IT IS A NAMED TYPE, on purpose: see the package doc.
type Report struct {
	Schema      int                   `json:"schema"`
	Class       Class                 `json:"class"`
	Fingerprint Fingerprint           `json:"fingerprint"`
	Component   Component             `json:"component"`
	Site        Site                  `json:"site"`
	SiteFile    SourceFile            `json:"site_file,omitempty"`
	Engine      Build                 `json:"engine"`
	Harness     *Harness              `json:"harness,omitempty"`
	Topology    Topology              `json:"topology"`
	Occurrence  Occurrence            `json:"occurrence"`
	Frames      []Frame               `json:"frames,omitempty"`
	Facts       map[FactKey]FactValue `json:"facts"`
}

// Fingerprint is a report's identity, the same for one defect on any engine
// and any version: see [FingerprintOf].
type Fingerprint string

// Component is an engine package path, as the catalogue names it.
type Component string

// Site is an engine function, from the engine's own public source.
type Site string

// Package is the package a site is in: a package segment never holds a dot.
func (s Site) Package() Component {
	pkg, _, _ := strings.Cut(string(s), ".")
	return Component(pkg)
}

// SourceFile is a file of the engine's public source: its package's directory
// and a base name, never a path as the build machine spelled it.
type SourceFile string

// Location is a site a signal found and the file that declares it, so the relay
// can read the declaration at the report's commit. [LocationOf] is the one way
// one is made: a site with no file is no site.
type Location struct {
	Site Site
	File SourceFile
}

// Frame is one module frame under a panic.
type Frame struct {
	Function Site       `json:"function"`
	File     SourceFile `json:"file"`
	Line     int        `json:"line"`
}

// VersionString is an engine version a strict pattern admitted.
type VersionString string

// CommitHash is a full commit id.
type CommitHash string

// GoVersion is the toolchain's version.
type GoVersion string

// OS is an operating system of the release matrix.
type OS string

// Arch is an architecture of the release matrix.
type Arch string

// Build is the engine that sent a report.
type Build struct {
	Version VersionString `json:"version"`
	Commit  CommitHash    `json:"commit"`
	Go      GoVersion     `json:"go"`
	OS      OS            `json:"os"`
	Arch    Arch          `json:"arch"`
}

// InertReason is why a binary sends no report, "" for one that may.
type InertReason string

// Why a binary is inert. A DEVELOPMENT BUILD'S DEFECT IS OFTEN ITS DEVELOPER'S
// OWN UNMERGED CHANGE, and a report of it would be a public issue about code
// nobody else has; a build with no commit could not say what to look at.
const (
	// InertDevelopment is a version that is no release tag and no module
	// pseudo-version: `dev`, `(devel)`, anything a pattern refused.
	InertDevelopment InertReason = "development"
	// InertModified is a build from a tree with uncommitted changes, whatever
	// its version says.
	InertModified InertReason = "modified"
	// InertNoCommit is a build that recorded no commit: `go install`'s.
	InertNoCommit InertReason = "no_commit"
)

// Origin is a binary as a report would name it, and why none may be sent from
// it, if so. [OriginOf] reads the running binary, and it is the only origin the
// engine ever uses: a hand-written one is a test's, and no document, flag or
// variable reaches it.
type Origin struct {
	Build Build
	// Inert is what the build recorded against it that its shapes cannot show.
	Inert InertReason
}

// InertReason is why this origin sends no report, "" for one that may: the one
// rule, so a zero or hand-written origin is held to it as [OriginOf]'s is.
func (o Origin) InertReason() InertReason {
	switch {
	case o.Inert != "":
		return o.Inert
	case !versionShape().MatchString(string(o.Build.Version)):
		return InertDevelopment
	case !commitShape().MatchString(string(o.Build.Commit)):
		return InertNoCommit
	}
	return ""
}

// HarnessName is a coding CLI with an in-box runner.
type HarnessName string

// HarnessVersion is a harness's release.
type HarnessVersion string

// Harness is the coding CLI a class involves.
type Harness struct {
	Name    HarnessName    `json:"name"`
	Version HarnessVersion `json:"version"`
}

// StreamKind is the value `stream.type` holds.
type StreamKind string

// CoordinationKind is the value `coordination.type` holds.
type CoordinationKind string

// SandboxKind is where a company's executors run, or `none`.
type SandboxKind string

// NodeBucket is how many nodes a fleet has, roughly: an exact count in a public
// issue says more about who is asking than about the defect.
type NodeBucket string

// Topology is how the engine runs: enums, its size among them.
type Topology struct {
	Nodes        NodeBucket       `json:"nodes"`
	Stream       StreamKind       `json:"stream"`
	Coordination CoordinationKind `json:"coordination"`
	Sandbox      SandboxKind      `json:"sandbox"`
}

// Occurrence is how often and over what span, to the hour.
type Occurrence struct {
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// EnumValue is one value of a closed set a fact declares.
type EnumValue string

// FactValue is an integer, a boolean or an enum value, and nothing else.
type FactValue struct {
	kind FactType
	i    int64
	b    bool
	e    EnumValue
}

func intFact(v int64) FactValue   { return FactValue{kind: FactInteger, i: v} }
func boolFact(v bool) FactValue   { return FactValue{kind: FactBoolean, b: v} }
func enumFact(v string) FactValue { return FactValue{kind: FactEnum, e: EnumValue(v)} }

// MarshalJSON writes the value alone.
func (f FactValue) MarshalJSON() ([]byte, error) {
	switch f.kind {
	case FactInteger:
		return json.Marshal(f.i)
	case FactBoolean:
		return json.Marshal(f.b)
	case FactEnum:
		return json.Marshal(f.e)
	}
	return nil, fmt.Errorf("issuereport: a fact of no type")
}

var (
	knownNodeBuckets  = []NodeBucket{"1", "2-3", "4-8", "9+"}
	knownOS           = []OS{"linux", "darwin", unknownValue}
	knownArch         = []Arch{"amd64", "arm64", unknownValue}
	knownHarnesses    = []HarnessName{"claude-code", "opencode"}
	knownStreams      = []StreamKind{"embedded", "nats", unknownValue}
	knownCoordination = []CoordinationKind{"local", "embedded-kv", unknownValue}
	knownSandboxes    = []SandboxKind{"none", "direct", "container", "e2b", unknownValue}

	// EACH PATTERN IS BUILT AT FIRST USE: every process links this package, a
	// push's hook among them, and a bounded repeat compiles to a program holding
	// every count (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit).
	//
	// A release tag or a clean module pseudo-version, and nothing else: the
	// relay holds a report to this too. A prerelease is `rc`, `alpha` or `beta`
	// and a number, because a suffix of any other words is how a private
	// build's name would get out.
	versionShape = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^v\d+\.\d+\.\d+(-(rc|alpha|beta)\.\d+(\.0\.\d{14}-[0-9a-f]{12})?|-(0\.)?\d{14}-[0-9a-f]{12})?$`)
	})
	commitShape  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{40}$`) })
	goShape      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(go\d+\.\d+(\.\d+)?|unknown)$`) })
	harnessShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d+\.\d+\.\d+$`) })
	// A package under internal/ and a function in it, a method's or a closure's
	// included. Bounded, as every string here is.
	siteShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^internal(/[a-z0-9_]+)+\.[A-Za-z0-9_().*\[\]-]+$`) })
	// A package under internal/ and one of its Go files.
	sourceFileShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^internal(/[a-z0-9_]+)+/[a-z0-9_]+\.go$`) })
)

const (
	modulePrefix = "github.com/crewlet/crewlet/"
	maxSiteBytes = 200
	// unknownValue stands for what a pattern or a closed set refused.
	unknownValue = "unknown"
)

// versionOf holds a version string to its shape. `dev` and `unknown` are what a
// preview says of an inert build, and no report ever carries either.
func versionOf(s string) VersionString {
	switch {
	case version.IsDevelopmentFor(s):
		return "dev"
	case len(s) <= 64 && versionShape().MatchString(s):
		return VersionString(s)
	}
	return unknownValue
}

// commitOf holds a commit to its shape, "" for anything else.
func commitOf(s string) CommitHash {
	if commitShape().MatchString(s) {
		return CommitHash(s)
	}
	return ""
}

// goVersionOf holds a toolchain's version to its shape.
func goVersionOf(s string) GoVersion {
	if goShape().MatchString(s) {
		return GoVersion(s)
	}
	return unknownValue
}

// harnessOf is the block a report carries for a harness, nil for a harness this
// build does not name or a release not of the shape a harness gives.
func harnessOf(name, release string) *Harness {
	if !slices.Contains(knownHarnesses, HarnessName(name)) || len(release) > 32 || !harnessShape().MatchString(release) {
		return nil
	}
	return &Harness{Name: HarnessName(name), Version: HarnessVersion(release)}
}

// SiteOf reads a function's name, as the runtime spells it, as a site: "" for
// a function outside this module or of any other shape.
func SiteOf(function string) Site {
	rest, ok := strings.CutPrefix(function, modulePrefix)
	if !ok || len(rest) > maxSiteBytes || !siteShape().MatchString(rest) {
		return ""
	}
	return Site(rest)
}

// LocationOf reads a function and its file as the runtime gave them, the zero
// Location for anything else: a function outside internal/, including the binary's
// own `main`, or one with no declaration line for the relay to find, as a package
// initialiser's closures have none. Only the file's base name is read, so a build
// without -trimpath cannot put its directory in a report.
func LocationOf(function, file string) Location {
	site := SiteOf(function)
	if site == "" || !declaredInSource(site) {
		return Location{}
	}
	source := string(site.Package()) + "/" + path.Base(file)
	if len(source) > maxSiteBytes || !sourceFileShape().MatchString(source) {
		return Location{}
	}
	return Location{Site: site, File: SourceFile(source)}
}

// declaredInSource refuses the symbols the compiler made up: `init`, its numbered
// and closure forms, and the older `glob..func1`.
func declaredInSource(site Site) bool {
	_, symbol, _ := strings.Cut(string(site), ".")
	return symbol != "init" && !strings.HasPrefix(symbol, "init.") && !strings.HasPrefix(symbol, "glob.")
}

// FrameOf reads one frame of a stack as the runtime gave it, false for one that
// is not a place in internal/.
func FrameOf(function, file string, line int) (Frame, bool) {
	at := LocationOf(function, file)
	if at.Site == "" || line < 1 || line > maxFrameLine {
		return Frame{}, false
	}
	return Frame{Function: at.Site, File: at.File, Line: line}, true
}

// OriginOf reads the running binary: its version, as internal/version resolved
// it, and the commit and tree state its build recorded.
func OriginOf(release string) Origin {
	var revision string
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	return originOf(release, revision, modified)
}

// originOf is [OriginOf] over what it read. It records only what a build's shapes
// cannot show, a modified tree, because a stamped version says nothing of the tree
// it was built from; [Origin.InertReason] judges the rest.
func originOf(release, revision string, modified bool) Origin {
	origin := Origin{Build: Build{
		Version: versionOf(release),
		Commit:  commitOf(revision),
		Go:      goVersionOf(runtime.Version()),
		OS:      closedOr(OS(runtime.GOOS), knownOS),
		Arch:    closedOr(Arch(runtime.GOARCH), knownArch),
	}}
	if modified || strings.Contains(release, "dirty") || strings.Contains(release, "modified") {
		origin.Inert = InertModified
	}
	return origin
}

// closedOr is v where the closed set holds it and "unknown" otherwise.
func closedOr[T ~string](v T, set []T) T {
	if slices.Contains(set, v) {
		return v
	}
	return T(unknownValue)
}

// NewTopology holds how the engine runs to the closed sets.
func NewTopology(nodes int, stream, coordination, sandbox string) Topology {
	return Topology{
		Nodes:        nodeBucketOf(nodes),
		Stream:       closedOr(StreamKind(stream), knownStreams),
		Coordination: closedOr(CoordinationKind(coordination), knownCoordination),
		Sandbox:      closedOr(SandboxKind(sandbox), knownSandboxes),
	}
}

// nodeBucketOf is the bucket a fleet of this many nodes falls in. A node that
// cannot count its peers is at least itself.
func nodeBucketOf(nodes int) NodeBucket {
	switch {
	case nodes <= 1:
		return "1"
	case nodes <= 3:
		return "2-3"
	case nodes <= 8:
		return "4-8"
	}
	return "9+"
}

// FingerprintOf is the identity of one defect. It carries no version and no
// value of any company's, so one defect is one fingerprint everywhere, and the
// relay recomputes it.
func FingerprintOf(class Class, component Component, site Site) Fingerprint {
	sum := sha256.Sum256([]byte("crewlet-report/v1\x00" + string(class) + "\x00" + string(component) + "\x00" + string(site)))
	return Fingerprint(hex.EncodeToString(sum[:]))
}

// SizeBucket is the bucket a session of size bytes falls in.
func SizeBucket(size int64) string {
	const kib = int64(1) << 10
	switch {
	case size <= 0:
		return "unknown"
	case size < 64*kib:
		return "0-64KiB"
	case size < 256*kib:
		return "64KiB-256KiB"
	case size < 512*kib:
		return "256KiB-512KiB"
	case size < 1024*kib:
		return "512KiB-1MiB"
	case size < 4*1024*kib:
		return "1MiB-4MiB"
	case size < 16*1024*kib:
		return "4MiB-16MiB"
	}
	return "16MiB+"
}

// checked holds facts to what the class declares: a key it does not declare, a
// value outside its set or a required fact left out is an error, so no mistake in
// a wired site can widen what a report says or send what the relay refuses.
func (e Entry) checked(facts map[FactKey]FactValue) (map[FactKey]FactValue, error) {
	for _, declared := range e.Facts {
		if _, present := facts[declared.Key]; !present && !declared.Optional {
			return nil, fmt.Errorf("issuereport: %s requires fact %q", e.ID, declared.Key)
		}
	}
	out := make(map[FactKey]FactValue, len(facts))
	for key, value := range facts {
		i := slices.IndexFunc(e.Facts, func(f Fact) bool { return f.Key == key })
		if i < 0 {
			return nil, fmt.Errorf("issuereport: %s declares no fact %q", e.ID, key)
		}
		declared := e.Facts[i]
		switch {
		case value.kind != declared.Type:
			return nil, fmt.Errorf("issuereport: fact %q of %s is not of type %s", key, e.ID, declared.Type)
		case declared.Type == FactEnum && !slices.Contains(declared.Enum, string(value.e)):
			return nil, fmt.Errorf("issuereport: fact %q of %s holds a value its set does not", key, e.ID)
		case declared.Type == FactInteger && (value.i < 0 || (declared.Max > 0 && value.i > declared.Max)):
			return nil, fmt.Errorf("issuereport: fact %q of %s is out of range", key, e.ID)
		}
		out[key] = value
	}
	return out, nil
}

// framesOf is what a report of this class carries of a stack: nothing unless the
// class takes frames and they start at the site it names, and then a bounded few.
func (e Entry) framesOf(at Location, frames []Frame) []Frame {
	if !e.Frames || len(frames) == 0 || frames[0].Function != at.Site {
		return nil
	}
	return slices.Clone(frames[:min(len(frames), maxFrames)])
}

// build makes the report of an occurrence. It is the one way one is made.
func build(p pending, engine Build, topology Topology) (Report, error) {
	e, harness := p.entry, p.harness
	if !e.Harness {
		harness = nil
	} else if harness == nil {
		// THE RELAY REQUIRES ONE: a run that recorded no release this build can
		// read has no report it would take.
		return Report{}, fmt.Errorf("issuereport: %s needs the harness that ran", e.ID)
	}
	component, at := e.placeOf(p.at)
	held, err := e.checked(p.facts)
	if err != nil {
		return Report{}, err
	}
	return Report{
		Schema:      SchemaVersion,
		Class:       e.ID,
		Fingerprint: FingerprintOf(e.ID, component, at.Site),
		Component:   component,
		Site:        at.Site,
		SiteFile:    at.File,
		Engine:      engine,
		Harness:     harness,
		Topology:    topology,
		Occurrence: Occurrence{
			Count:     min(max(p.count, 1), maxOccurrenceCount),
			FirstSeen: p.first.UTC().Truncate(time.Hour),
			LastSeen:  p.last.UTC().Truncate(time.Hour),
		},
		Frames: e.framesOf(at, p.frames),
		Facts:  held,
	}, nil
}

// Encode is a report's bytes, bounded by what the relay accepts.
func (r Report) Encode() ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("issuereport: encode a report: %w", err)
	}
	if len(body) > coord.MaxIssueReportPayloadBytes {
		return nil, errors.New("issuereport: a report is larger than the relay accepts")
	}
	return body, nil
}

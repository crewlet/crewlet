package issuereport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/version"
)

// SchemaVersion is the report format this build writes.
const SchemaVersion = 1

// maxOccurrenceCount caps a report's count: past it, the number says nothing.
const maxOccurrenceCount = 10_000

// maxTopologyNodes caps a report's node count: the largest fleet the docs
// describe is a dozen, and an exact large count says more about who is asking
// than about the defect.
const maxTopologyNodes = 32

// A Report is one typed record: what goes to the relay, and nothing else does.
// EVERY STRING IN IT IS A NAMED TYPE, on purpose: see the package doc.
type Report struct {
	Schema      int                   `json:"schema"`
	Class       Class                 `json:"class"`
	Fingerprint Fingerprint           `json:"fingerprint"`
	Component   Component             `json:"component"`
	Site        Site                  `json:"site"`
	Engine      Build                 `json:"engine"`
	Harness     *Harness              `json:"harness,omitempty"`
	Topology    Topology              `json:"topology"`
	Occurrence  Occurrence            `json:"occurrence"`
	Facts       map[FactKey]FactValue `json:"facts"`
}

// Fingerprint is a report's identity, the same for one defect on any engine
// and any version: see [FingerprintOf].
type Fingerprint string

// Component is an engine package path, as the catalogue names it.
type Component string

// Site is an engine function, from the engine's own public source.
type Site string

// VersionString is an engine version a strict pattern admitted.
type VersionString string

// CommitHash is a full commit id.
type CommitHash string

// GoVersion is the toolchain's version.
type GoVersion string

// OS and Arch are the release matrix's platforms.
type (
	OS   string
	Arch string
)

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
	// Inert is what the build recorded against it; [Origin.InertReason] holds
	// the build to its shapes as well, so a zero origin is inert.
	Inert InertReason
}

// InertReason is why this origin sends no report, "" for one that may.
func (o Origin) InertReason() InertReason {
	switch {
	case o.Inert != "":
		return o.Inert
	case !versionShape.MatchString(string(o.Build.Version)):
		return InertDevelopment
	case !commitShape.MatchString(string(o.Build.Commit)):
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

// The topology's closed sets, each the value its config field holds.
type (
	StreamKind       string
	CoordinationKind string
	SandboxKind      string
)

// Topology is how the engine runs: enums and a small count.
type Topology struct {
	Nodes        int              `json:"nodes"`
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
	knownOS           = []OS{"linux", "darwin", unknownValue}
	knownArch         = []Arch{"amd64", "arm64", unknownValue}
	knownHarnesses    = []HarnessName{"claude-code", "opencode"}
	knownStreams      = []StreamKind{"embedded", "nats", unknownValue}
	knownCoordination = []CoordinationKind{"local", "embedded-kv", unknownValue}
	knownSandboxes    = []SandboxKind{"none", "direct", "container", "e2b", unknownValue}

	// A release tag or a clean module pseudo-version, and nothing else: the
	// relay holds a report to this too. A prerelease is `rc`, `alpha` or `beta`
	// and a number, because a suffix of any other words is how a private
	// build's name would get out.
	versionShape = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(rc|alpha|beta)\.\d+(\.0\.\d{14}-[0-9a-f]{12})?|-(0\.)?\d{14}-[0-9a-f]{12})?$`)
	commitShape  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	goShape      = regexp.MustCompile(`^(go\d+\.\d+(\.\d+)?|unknown)$`)
	harnessShape = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// A package under internal/ and a function in it, a method's or a closure's
	// included. Bounded, as every string here is.
	siteShape = regexp.MustCompile(`^internal(/[a-z0-9_]+)+\.[A-Za-z0-9_().*\[\]-]+$`)
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
	case len(s) <= 64 && versionShape.MatchString(s):
		return VersionString(s)
	}
	return unknownValue
}

// commitOf holds a commit to its shape, "" for anything else.
func commitOf(s string) CommitHash {
	if commitShape.MatchString(s) {
		return CommitHash(s)
	}
	return ""
}

// goVersionOf holds a toolchain's version to its shape.
func goVersionOf(s string) GoVersion {
	if goShape.MatchString(s) {
		return GoVersion(s)
	}
	return unknownValue
}

// harnessOf is the block a report carries for a harness, nil for a harness this
// build does not name or a release not of the shape a harness gives.
func harnessOf(name, release string) *Harness {
	if !slices.Contains(knownHarnesses, HarnessName(name)) || len(release) > 32 || !harnessShape.MatchString(release) {
		return nil
	}
	return &Harness{Name: HarnessName(name), Version: HarnessVersion(release)}
}

// SiteOf reads a function's name, as the runtime spells it, as a site: "" for
// a function outside this module or of any other shape.
func SiteOf(function string) Site {
	rest, ok := strings.CutPrefix(function, modulePrefix)
	if !ok || len(rest) > maxSiteBytes || !siteShape.MatchString(rest) {
		return ""
	}
	return Site(rest)
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

// originOf is the rule, over what [OriginOf] read. A MODIFIED TREE IS CHECKED ON
// ITS OWN, because a stamped version says nothing of the tree it was built from.
func originOf(release, revision string, modified bool) Origin {
	origin := Origin{Build: Build{
		Version: versionOf(release),
		Commit:  commitOf(revision),
		Go:      goVersionOf(runtime.Version()),
		OS:      closedOr(OS(runtime.GOOS), knownOS),
		Arch:    closedOr(Arch(runtime.GOARCH), knownArch),
	}}
	switch {
	case modified || strings.Contains(release, "dirty") || strings.Contains(release, "modified"):
		origin.Inert = InertModified
	case !versionShape.MatchString(string(origin.Build.Version)):
		origin.Inert = InertDevelopment
	case origin.Build.Commit == "":
		origin.Inert = InertNoCommit
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
		Nodes:        min(max(nodes, 1), maxTopologyNodes),
		Stream:       closedOr(StreamKind(stream), knownStreams),
		Coordination: closedOr(CoordinationKind(coordination), knownCoordination),
		Sandbox:      closedOr(SandboxKind(sandbox), knownSandboxes),
	}
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

// checked holds facts to what the class declares: a key it does not declare or
// a value outside its set is an error, so no mistake in a wired site can widen
// what a report says.
func (e Entry) checked(facts map[FactKey]FactValue) (map[FactKey]FactValue, error) {
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

// build makes the report of an occurrence. It is the one way one is made.
func build(e Entry, site Site, engine Build, harness *Harness, topology Topology,
	count int, first, last time.Time, facts map[FactKey]FactValue,
) (Report, error) {
	if !e.Harness {
		harness = nil
	} else if harness == nil {
		// THE RELAY REQUIRES ONE: a run that recorded no release this build can
		// read has no report it would take.
		return Report{}, fmt.Errorf("issuereport: %s needs the harness that ran", e.ID)
	}
	if site == "" || !e.SiteVaries {
		site = e.Site
	}
	held, err := e.checked(facts)
	if err != nil {
		return Report{}, err
	}
	return Report{
		Schema:      SchemaVersion,
		Class:       e.ID,
		Fingerprint: FingerprintOf(e.ID, e.Component, site),
		Component:   e.Component,
		Site:        site,
		Engine:      engine,
		Harness:     harness,
		Topology:    topology,
		Occurrence: Occurrence{
			Count:     min(max(count, 1), maxOccurrenceCount),
			FirstSeen: first.UTC().Truncate(time.Hour),
			LastSeen:  last.UTC().Truncate(time.Hour),
		},
		Facts: held,
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

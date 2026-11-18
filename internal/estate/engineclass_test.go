package estate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE ENGINE CLASS: the executor and access logs' writes are the engine's
// record of what its own runs did and told, which no tool, person or operator
// surface makes ([originEngine]). Two gates hold it: the operation table
// declares every write on those logs the engine's and nothing else, and the
// source tree keeps every package serving a person or a model off their write
// side.

// EVERY WRITE ON THE EXECUTOR LOG IS THE ENGINE'S, AND NOTHING ELSE IS: one
// declared a surface's would land in SurfaceWriteDomains, a write no surface can
// make, and an engine write elsewhere rides a facade the tree guard does not
// read. executor.begin is one. No read is the engine's, but two hand over whom a
// write was made for (chat.turn_thread, code.queue_due), so the tree guard holds
// surfaces off them ([TestOnlyTheEngineReadsWhomAMessageWasWrittenFor],
// [TestOnlyTheEngineReadsWhomAQueuedChangeWasEnqueuedFor]).
//
// Mutation: drop byEngine() from executor.capture or executor.begin, or put it
// on a tracker write, and the operation is named; the planted table makes each
// of those moves and a read declared the engine's beside them.
func TestEveryExecutorWriteIsTheEngines(t *testing.T) {
	t.Parallel()
	for _, violation := range engineClassViolations(registry) {
		t.Error(violation)
	}
	var engine, executorWrites, accessWrites int
	for _, spec := range registry {
		if spec.origin == originEngine {
			engine++
		}
		if spec.domain == executorDomain && spec.class != opRead {
			executorWrites++
		}
		if spec.domain == accessDomain && spec.class != opRead {
			accessWrites++
		}
	}
	if engine == 0 || executorWrites == 0 || accessWrites == 0 {
		t.Fatalf("%d operations are the engine's, %d write the executor log and %d the access log: "+
			"the class holds nothing, so this gate certifies nothing", engine, executorWrites, accessWrites)
	}
	if spec, ok := registry["executor.begin"]; !ok || spec.class != opIdempotentWrite ||
		spec.domain != executorDomain || spec.origin != originEngine {
		t.Errorf("executor.begin is registered %v as %+v, want the engine's idempotent write on the "+
			"executor log", ok, spec)
	}

	planted := map[string]*opSpec{
		"executor.capture":    {class: opIdempotentWrite, domain: executorDomain},
		"executor.begin":      {class: opIdempotentWrite, domain: executorDomain},
		"executor.peek":       {class: opRead, domain: executorDomain, origin: originEngine},
		"tracker.repair":      {class: opIdempotentWrite, domain: trackerDomain, origin: originEngine},
		"executor.end":        {class: opIdempotentWrite, domain: executorDomain, origin: originEngine},
		"executor.head":       {class: opRead, domain: executorDomain},
		"tracker.create_task": {class: opIdempotentWrite, domain: trackerDomain},
		"access.tell":         {class: opIdempotentWrite, domain: accessDomain},
		"access.ran":          {class: opIdempotentWrite, domain: accessDomain, origin: originEngine},
	}
	violations := engineClassViolations(planted)
	var named []string
	for _, v := range violations {
		named = append(named, strings.Fields(v)[0])
	}
	want := []string{"access.tell", "executor.begin", "executor.capture", "executor.peek", "tracker.repair"}
	if !slices.Equal(named, want) {
		t.Errorf("the planted table's violations name %v, want %v:\n%s", named, want,
			strings.Join(violations, "\n"))
	}
}

// engineDomains are the logs whose every write is the engine's.
var engineDomains = []string{executorDomain, accessDomain}

// engineClassViolations is every operation in table that breaks the engine
// class, in the order of their names, each opening with the name.
func engineClassViolations(table map[string]*opSpec) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(table)) {
		spec := table[name]
		engine := spec.origin == originEngine
		switch {
		case spec.class == opRead && engine:
			out = append(out, name+" is a read declared the engine's: the engine's reads are "+
				"ordinary ones, which a surface may hold")
		case spec.class == opRead:
		case slices.Contains(engineDomains, spec.domain) && !engine:
			out = append(out, fmt.Sprintf("%s writes the %s log and is not declared the engine's: "+
				"nothing but the engine records what its runs did, so declare it byEngine()",
				name, spec.domain))
		case !slices.Contains(engineDomains, spec.domain) && engine:
			out = append(out, fmt.Sprintf("%s is declared the engine's and writes the %q domain: "+
				"the class is the executor and access logs' writes, the only ones the surface "+
				"guard (TestNoSurfaceHoldsTheEnginesExecutorWrites) holds", name, spec.domain))
		}
	}
	return out
}

// THE DOMAINS A SURFACE WRITES LEAVE THE ENGINE'S OUT, AND ONLY THE ENGINE'S:
// internal/agent/builtin holds its declaration of the domains a person's write
// surface classes refusals of and names objects of to this list
// (TestEveryRoutedDomainIsDeclaredToTheSurfaces). Keeping the executor log
// would demand a refusal class and an object argument for writes no surface
// makes; dropping a surface's domain would let its refusals reach a person as
// faults.
//
// Mutation: drop the origin from surfaceWrite and the executor log is listed;
// drop the class and a read is counted a surface's write.
func TestTheSurfacesWriteDomainsLeaveTheEnginesOut(t *testing.T) {
	t.Parallel()
	listed := SurfaceWriteDomains()
	for _, domain := range engineDomains {
		if slices.Contains(listed, domain) {
			t.Errorf("SurfaceWriteDomains lists %v, the %s log among them, whose writes are "+
				"the engine's alone", listed, domain)
		}
	}
	for _, domain := range []string{trackerDomain, pagesDomain, artifactsDomain, artifactDataDomain, chatDomain, codeDomain} {
		if !slices.Contains(listed, domain) {
			t.Errorf("SurfaceWriteDomains lists %v, without %s, which a surface writes", listed, domain)
		}
	}
	for name, tc := range map[string]struct {
		spec opSpec
		want bool
	}{
		"a surface's write":      {opSpec{class: opIdempotentWrite, domain: trackerDomain}, true},
		"a surface's once-write": {opSpec{class: opOnceWrite, domain: pagesDomain}, true},
		"the engine's write":     {opSpec{class: opIdempotentWrite, domain: executorDomain, origin: originEngine}, false},
		"a read":                 {opSpec{class: opRead, domain: pagesDomain}, false},
		"a read with no floor":   {opSpec{class: opRead}, false},
	} {
		if got := tc.spec.surfaceWrite(); got != tc.want {
			t.Errorf("%s: counted a surface's write = %v, want %v", name, got, tc.want)
		}
	}
}

// personSurfaces are the trees of the packages that serve a person or a model:
// a seat's tools, the API with its operator MCP, Ask Crewlet and the binary.
var personSurfaces = []string{"internal/agent/builtin", "internal/api", "internal/assistant", "cmd/crewlet"}

// engineDir is the engine package, whose exported hand-outs the guard derives.
const engineDir = "internal/engine"

// The import paths the guard reads names through, read off the types
// themselves so a moved package is followed.
var (
	estateImport   = reflect.TypeFor[Backend]().PkgPath()
	executorImport = reflect.TypeFor[executordomain.Writer]().PkgPath()
	accessImport   = reflect.TypeFor[accessdomain.Writer]().PkgPath()
)

// domainImports are the engine-class logs' own packages.
var domainImports = []string{executorImport, accessImport}

// engineWriteNames are the names the log's write side is reached by, matched
// on any operand: the router's writer facade and its accessor
// (ExecutorWrites), and the serving half's writer and the engine's own
// accessor of this node's writer (ExecutorWriter).
var engineWriteNames = []string{"ExecutorWrites", "ExecutorWriter", "AccessWrites", "AccessWriter"}

// writeSideTypes are this package's types that carry the log's write side, as
// a hand-out's results name them.
var writeSideTypes = []string{"ExecutorWrites", "ExecutorWriter", "ExecutorHalf",
	"AccessWrites", "AccessWriter", "AccessHalf"}

// domainWriteNames are the domain's own write side, matched through whatever
// name a file imports the domain under: its writer, how one is built, and the
// call every write takes and no read does.
var domainWriteNames = []string{"Writer", "NewWriter", "WriterDeps", "Call"}

// turnReadNames reach the engine's turn-start thread read, matched on any
// operand: the router's facade and accessor (TurnThreads), the serving read and
// its answer (TurnThread), Chatlet's reader built over the facade
// (NewThreadReader) and the engine's set of them (ChatThreads). A surface reads
// a thread through [Chat]'s Thread.
var turnReadNames = []string{"TurnThreads", "TurnThread", "NewThreadReader", "ChatThreads"}

// The import paths of the types that carry the turn-start read, as a
// hand-out's results name them.
var (
	chatletImport = reflect.TypeFor[chatlet.ThreadReader]().PkgPath()
	notifyImport  = reflect.TypeFor[notify.ThreadReaders]().PkgPath()
)

// queueReadNames reach the home's due-queue read, matched on any operand: the
// read and the page of queue work it answers, which share one name (QueueDue:
// [CodeSeam.QueueDue], Gitlet's reader and server, the home's estate), its
// answer (QueueDueAnswer) and an entry (QueueWorkEntry, which carries whom its
// enqueue was made for). A surface lists a queue through code.read's `queue`
// kind, whose rows leave that out.
var queueReadNames = []string{"QueueDue", "QueueDueAnswer", "QueueWorkEntry"}

// gitletImport is Gitlet's package, whose types carry the due-queue read, as
// a hand-out's results name them.
var gitletImport = reflect.TypeFor[gitlet.QueueDueAnswer]().PkgPath()

// NO SURFACE HOLDS THE ENGINE'S EXECUTOR WRITES: no package serving a person or
// a model ([personSurfaces]) reaches the write side. Each tree's non-test Go
// files, less testdata, are read for a road: the router's facade or the serving
// half's writer by name on any operand ([engineWriteNames]); the domain's write
// side under whatever name a file imports it ([domainWriteNames]), a dot-import
// of it or of this package refused outright as reaching every name unqualified;
// and every exported internal/engine function or method whose results name the
// write side, a road taken without naming the writer. It is syntactic: a writer
// reached through another type's value or reflection is review's to hold. A
// walk that read nothing would pass, so each surface must yield a Go file.
//
// Mutation: TestTheEngineWritesGuardFindsEveryRoad makes each move above.
func TestNoSurfaceHoldsTheEnginesExecutorWrites(t *testing.T) {
	t.Parallel()
	report, err := scanEngineWrites(sourcetree.Root(t), personSurfaces)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range report.failures(personSurfaces) {
		t.Error(failure)
	}
	if report.engineFiles == 0 {
		t.Errorf("the guard read no source of %s, so it derived no hand-out a surface could take",
			engineDir)
	}
}

// THE GUARD FINDS EVERY ROAD IT CLAIMS TO CLOSE: each case plants files in a
// tree whose surfaces otherwise hold a file taking no road, and scans it. A road
// is one failure naming its file and line, a lookalike is none, and the last
// case is a surface holding no Go file: a walk that read nothing.
//
// Mutation: match the facade only under the import name `estate`, match the
// domain's names on any operand, read the test files, or leave the engine's
// hand-outs underived, and the case for that road changes its answer.
func TestTheEngineWritesGuardFindsEveryRoad(t *testing.T) {
	t.Parallel()
	estate, executor := strconv.Quote(estateImport), strconv.Quote(executorImport)
	engine := strconv.Quote(path.Join(path.Dir(estateImport), "engine"))
	for name, tc := range map[string]struct {
		files map[string]string
		// want is a part of the one failure; empty, the files take no road.
		want string
	}{
		"the writer facade under an aliased import": {
			files: map[string]string{"internal/api/road.go": "package api\nimport est " + estate +
				"\nvar w est.ExecutorWrites\n"},
			want: "internal/api/road.go:3: ExecutorWrites",
		},
		"the router's accessor on any operand": {
			files: map[string]string{"internal/agent/builtin/road.go": "package builtin\n" +
				"func writes(r interface{ ExecutorWrites() any }) any {\n\treturn r.ExecutorWrites()\n}\n"},
			want: "internal/agent/builtin/road.go:3: ExecutorWrites",
		},
		"the serving half's writer in Ask Crewlet": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport " + estate +
				"\nvar w estate.ExecutorWriter\n"},
			want: "internal/assistant/road.go:3: ExecutorWriter",
		},
		"the engine's own writer, from the binary": {
			files: map[string]string{"cmd/crewlet/road.go": "package main\n" +
				"func writer(e interface{ ExecutorWriter() any }) any { return e.ExecutorWriter() }\n"},
			want: "cmd/crewlet/road.go:2: ExecutorWriter",
		},
		"the domain's writer under an aliased import": {
			files: map[string]string{"internal/api/road.go": "package api\nimport ed " + executor +
				"\nvar w *ed.Writer\n"},
			want: "internal/api/road.go:3: ed.Writer",
		},
		"a write's call built in the binary": {
			files: map[string]string{"cmd/crewlet/road.go": "package main\nimport " + executor +
				"\nvar c = executordomain.Call{OpID: \"op-1\"}\n"},
			want: "cmd/crewlet/road.go:3: executordomain.Call",
		},
		"the domain dot-imported": {
			files: map[string]string{"internal/api/road.go": "package api\nimport . " + executor +
				"\nvar w *Writer\n"},
			want: "internal/api/road.go:2: dot-imports",
		},
		"this package dot-imported": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport . " + estate +
				"\nvar r ExecutorReads\n"},
			want: "internal/assistant/road.go:2: dot-imports",
		},
		"a hand-out the engine exports under a name of its own": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport " + estate + "\ntype Engine struct{}\n" +
					"func Recorder(e *Engine) estate.ExecutorWrites { return estate.ExecutorWrites{} }\n",
				"internal/api/road.go": "package api\nimport " + engine +
					"\nfunc record(e *engine.Engine) { _ = engine.Recorder(e) }\n",
			},
			want: "internal/api/road.go:3: Recorder",
		},
		"a hand-out method of the domain's writer": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport ed " + executor + "\ntype Engine struct{}\n" +
					"func (e *Engine) Recorder() *ed.Writer { return nil }\n",
				"cmd/crewlet/road.go": "package main\n" +
					"func record(e interface{ Recorder() any }) any { return e.Recorder() }\n",
			},
			want: "cmd/crewlet/road.go:2: Recorder",
		},
		"the reads, which a surface may hold": {
			files: map[string]string{"internal/api/road.go": "package api\nimport " + estate + "\nimport " +
				executor + "\nvar r estate.ExecutorReads\nvar s executordomain.Session\n" +
				"var id = executordomain.EndOpID\n" +
				"func reads(x interface{ ExecutorReads() estate.ExecutorReads }) estate.ExecutorReads " +
				"{ return x.ExecutorReads() }\n"},
		},
		"another domain's call": {
			files: map[string]string{"internal/api/road.go": "package api\n" +
				"import \"github.com/crewlet/crewlet/internal/artifactsdomain\"\nvar c artifactsdomain.Call\n"},
		},
		"a surface's test": {
			files: map[string]string{"internal/api/road_test.go": "package api\nimport " + estate +
				"\nvar w estate.ExecutorWrites\n"},
		},
		"a surface's testdata": {
			files: map[string]string{"internal/api/testdata/road.go": "package api\nimport " + estate +
				"\nvar w estate.ExecutorWrites\n"},
		},
		"the engine holding its own writes": {
			files: map[string]string{"internal/engine/coordinator.go": "package engine\nimport " + estate +
				"\nvar writes estate.ExecutorWrites\nfunc unexported() estate.ExecutorWrites { return writes }\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := scanEngineWrites(plantTree(t, tc.files), personSurfaces)
			if err != nil {
				t.Fatal(err)
			}
			failures := report.failures(personSurfaces)
			switch {
			case tc.want == "" && len(failures) > 0:
				t.Errorf("files that take no road failed:\n%s", strings.Join(failures, "\n"))
			case tc.want != "" && (len(failures) != 1 || !strings.Contains(failures[0], tc.want)):
				t.Errorf("failures %q, want exactly one naming %q", failures, tc.want)
			}
		})
	}

	t.Run("a surface whose tree holds no Go file", func(t *testing.T) {
		t.Parallel()
		root := plantTree(t, map[string]string{"cmd/crewlet/README.md": "no source here\n"})
		if err := os.Remove(filepath.Join(root, "cmd", "crewlet", "quiet.go")); err != nil {
			t.Fatal(err)
		}
		report, err := scanEngineWrites(root, personSurfaces)
		if err != nil {
			t.Fatal(err)
		}
		failures := report.failures(personSurfaces)
		if len(failures) != 1 || !strings.HasPrefix(failures[0], "cmd/crewlet: ") {
			t.Errorf("failures %q, want exactly one saying cmd/crewlet was read empty", failures)
		}
	})
}

// ONLY THE ENGINE READS WHOM A MESSAGE WAS WRITTEN FOR (ADR-0109): the engine's
// turn-start thread read hands that over, so a turn works through only its own
// requester's waiting messages, and no reader of a room may see it. The surface
// guard ([TestNoSurfaceHoldsTheEnginesExecutorWrites]) holds the surfaces off
// it by name on any operand ([turnReadNames]) and through every engine hand-out
// whose results carry it. A planted road is one failure; the plain thread read
// a surface does hold is none.
//
// Mutation: drop a name from turnReadNames, or leave the read's hand-outs
// underived, and the case for that road changes its answer.
func TestOnlyTheEngineReadsWhomAMessageWasWrittenFor(t *testing.T) {
	t.Parallel()
	estate, chat := strconv.Quote(estateImport), strconv.Quote(chatletImport)
	notifyPath := strconv.Quote(notifyImport)
	engine := strconv.Quote(path.Join(path.Dir(estateImport), "engine"))
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"the engine's set of thread readers": {
			files: map[string]string{"internal/api/road.go": "package api\n" +
				"func read(e interface{ ChatThreads() any }) any { return e.ChatThreads() }\n"},
			want: "internal/api/road.go:2: ChatThreads",
		},
		"Chatlet's thread reader built over the chat facade": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport " + chat +
				"\nfunc reader(c chatlet.ThreadSource) any { return chatlet.NewThreadReader(c, nil) }\n"},
			want: "internal/assistant/road.go:3: NewThreadReader",
		},
		"the turn's read asked directly": {
			files: map[string]string{"internal/agent/builtin/road.go": "package builtin\n" +
				"func read(c interface{ TurnThread() any }) any { return c.TurnThread() }\n"},
			want: "internal/agent/builtin/road.go:2: TurnThread",
		},
		"the router's facade under an aliased import": {
			files: map[string]string{"cmd/crewlet/road.go": "package main\nimport est " + estate +
				"\nvar t est.TurnThreads\n"},
			want: "cmd/crewlet/road.go:3: TurnThreads",
		},
		"a hand-out the engine exports under a name of its own": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport " + notifyPath + "\ntype Engine struct{}\n" +
					"func Threads(e *Engine) *notify.ThreadReaders { return nil }\n",
				"internal/api/road.go": "package api\nimport " + engine +
					"\nfunc read(e *engine.Engine) { _ = engine.Threads(e) }\n",
			},
			want: "internal/api/road.go:3: Threads",
		},
		"the plain thread read, which a surface may hold": {
			files: map[string]string{"internal/api/road.go": "package api\nimport " + estate +
				"\nfunc read(c estate.Chat) any { return c.Thread }\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := scanEngineWrites(plantTree(t, tc.files), personSurfaces)
			if err != nil {
				t.Fatal(err)
			}
			failures := report.failures(personSurfaces)
			switch {
			case tc.want == "" && len(failures) > 0:
				t.Errorf("files that take no road failed:\n%s", strings.Join(failures, "\n"))
			case tc.want != "" && (len(failures) != 1 || !strings.Contains(failures[0], tc.want)):
				t.Errorf("failures %q, want exactly one naming %q", failures, tc.want)
			}
		})
	}
}

// ONLY THE ENGINE READS WHOM A QUEUED CHANGE WAS ENQUEUED FOR (ADR-0109): the
// home's due-queue read (code.queue_due) hands a repository's home whom each
// entry was enqueued for (gitlet.QueueWorkEntry.WorkingFor), so closing a
// landed change's work item is decided within their apps; no reader of a queue
// may see it. The surface guard holds the surfaces off it by name
// ([queueReadNames]), through every engine hand-out carrying it, and by
// refusing a dot-import of Gitlet. A planted road is one failure; the queue
// listing a surface holds is none.
//
// Mutation: drop a name from queueReadNames, leave the read's hand-outs
// underived, or let Gitlet be dot-imported, and the case for that road
// changes its answer.
func TestOnlyTheEngineReadsWhomAQueuedChangeWasEnqueuedFor(t *testing.T) {
	t.Parallel()
	gitletPath := strconv.Quote(gitletImport)
	engine := strconv.Quote(path.Join(path.Dir(estateImport), "engine"))
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"the code facade's queue read, from a seat's tools": {
			files: map[string]string{"internal/agent/builtin/road.go": "package builtin\n" +
				"func read(c interface{ QueueDue() any }) any { return c.QueueDue() }\n"},
			want: "internal/agent/builtin/road.go:2: QueueDue",
		},
		"its answer under an aliased import": {
			files: map[string]string{"internal/api/road.go": "package api\nimport g " + gitletPath +
				"\nvar due g.QueueDueAnswer\n"},
			want: "internal/api/road.go:3: QueueDueAnswer",
		},
		"an entry of the queue work, in Ask Crewlet": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport " + gitletPath +
				"\nfunc whom(e gitlet.QueueWorkEntry) any { return e.WorkingFor }\n"},
			want: "internal/assistant/road.go:3: QueueWorkEntry",
		},
		"a hand-out the engine exports under a name of its own": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport " + gitletPath + "\ntype Engine struct{}\n" +
					"func Owed(e *Engine) []gitlet.QueueWorkEntry { return nil }\n",
				"cmd/crewlet/road.go": "package main\nimport " + engine +
					"\nfunc read(e *engine.Engine) { _ = engine.Owed(e) }\n",
			},
			want: "cmd/crewlet/road.go:3: Owed",
		},
		"Gitlet dot-imported": {
			files: map[string]string{"internal/api/road.go": "package api\nimport . " + gitletPath +
				"\nvar due QueueDueAnswer\n"},
			want: "internal/api/road.go:2: dot-imports",
		},
		"the queue listing a seat reads, which a surface may hold": {
			files: map[string]string{"internal/agent/builtin/road.go": "package builtin\nimport " + gitletPath +
				"\nvar kind = gitlet.ReadQueue\nfunc rows(a gitlet.ReadAnswer) any { return a.Queue }\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := scanEngineWrites(plantTree(t, tc.files), personSurfaces)
			if err != nil {
				t.Fatal(err)
			}
			failures := report.failures(personSurfaces)
			switch {
			case tc.want == "" && len(failures) > 0:
				t.Errorf("files that take no road failed:\n%s", strings.Join(failures, "\n"))
			case tc.want != "" && (len(failures) != 1 || !strings.Contains(failures[0], tc.want)):
				t.Errorf("failures %q, want exactly one naming %q", failures, tc.want)
			}
		})
	}
}

// plantTree is a fresh tree holding files (a slash path below the root to its
// source), and a quiet Go file in every surface.
func plantTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, surface := range personSurfaces {
		write(surface+"/quiet.go", "package "+path.Base(surface)+"\n")
	}
	for rel, src := range files {
		write(rel, src)
	}
	return root
}

// engineWritesReport is one reading of the tree: how many Go files each
// surface's tree yielded, how many of the engine's the hand-outs were derived
// from, and every road a surface takes to the log's write side.
type engineWritesReport struct {
	read        map[string]int
	engineFiles int
	roads       []string
}

// failures is what the guard reports: every road, and every surface whose tree
// yielded no Go file.
func (r engineWritesReport) failures(surfaces []string) []string {
	out := slices.Clone(r.roads)
	for _, surface := range surfaces {
		if r.read[surface] == 0 {
			out = append(out, surface+": the guard read no Go file in this tree, so it "+
				"certifies nothing about it")
		}
	}
	return out
}

// scanEngineWrites reads the non-test Go files of each surface's tree under
// root, less testdata and hidden directories, for a road to the executor log's
// write side, to the engine's turn-start thread read or to the home's
// due-queue read, after deriving the engine's hand-outs of each.
func scanEngineWrites(root string, surfaces []string) (engineWritesReport, error) {
	report := engineWritesReport{read: map[string]int{}}
	doors, files, err := engineHandOuts(filepath.Join(root, filepath.FromSlash(engineDir)))
	if err != nil {
		return report, err
	}
	report.engineFiles = files
	names := roadNames{writes: map[string]bool{}, reads: map[string]bool{}, queue: map[string]bool{}}
	for _, name := range append(slices.Clone(engineWriteNames), doors.writes...) {
		names.writes[name] = true
	}
	for _, name := range append(slices.Clone(turnReadNames), doors.reads...) {
		names.reads[name] = true
	}
	for _, name := range append(slices.Clone(queueReadNames), doors.queue...) {
		names.queue[name] = true
	}
	fset := token.NewFileSet()
	for _, surface := range surfaces {
		err := sourcetree.Walk(filepath.Join(root, filepath.FromSlash(surface)),
			func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
					return nil
				}
				parsed, err := parser.ParseFile(fset, p, nil, 0)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				report.read[surface]++
				report.roads = append(report.roads, roadsIn(fset, filepath.ToSlash(rel), parsed, names)...)
				return nil
			})
		if err != nil {
			return report, fmt.Errorf("read %s: %w", surface, err)
		}
	}
	return report, nil
}

// roadNames are the names a surface may not take: writes, to the engine-class
// logs' write side; reads, to the engine's turn-start thread read; and queue,
// to the home's due-queue read.
type roadNames struct {
	writes, reads, queue map[string]bool
}

// roadsIn is every road one surface file takes to the log's write side, to
// the engine's turn-start thread read, or to the home's due-queue read.
func roadsIn(fset *token.FileSet, rel string, file *ast.File, names roadNames) []string {
	var out []string
	at := func(n ast.Node, what string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", rel, fset.Position(n.Pos()).Line, what))
	}
	for _, spec := range file.Imports {
		if imported, err := strconv.Unquote(spec.Path.Value); err == nil &&
			(imported == estateImport || imported == gitletImport || slices.Contains(domainImports, imported)) &&
			spec.Name != nil && spec.Name.Name == "." {
			at(spec, "dot-imports "+imported+", which reaches every name of it unqualified: "+
				"import it by name")
		}
	}
	domain := map[string]bool{}
	for _, imported := range domainImports {
		maps.Copy(domain, importNamesOf(file, imported))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if names.writes[sel.Sel.Name] {
			at(sel, sel.Sel.Name+" reaches the executor or access log's writes, which are the "+
				"engine's alone (internal/estate's engine-originated class); a surface holds "+
				"their reads, ExecutorReads and AccessReads")
		}
		if names.reads[sel.Sel.Name] {
			at(sel, sel.Sel.Name+" reaches the engine's turn-start thread read, which hands over "+
				"whom each chat message was written for and is the engine's alone (ADR-0109); a "+
				"surface reads a thread through estate.Chat's Thread")
		}
		if names.queue[sel.Sel.Name] {
			at(sel, sel.Sel.Name+" reaches the home's due-queue read, which hands over whom each "+
				"queued change's enqueue was made for and is the engine's alone (ADR-0109); a surface "+
				"lists a queue through code.read's queue kind")
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent && domain[x.Name] &&
			slices.Contains(domainWriteNames, sel.Sel.Name) {
			at(sel, x.Name+"."+sel.Sel.Name+" is an engine-class domain's write side, which "+
				"only the engine holds")
		}
		return true
	})
	return out
}

// handOuts are the engine's exported functions and methods whose results name
// the log's write side (writes), the turn-start thread read (reads), or the
// home's due-queue read (queue).
type handOuts struct {
	writes, reads, queue []string
}

// engineHandOuts is every exported function and method in dir — the engine
// package — whose results name the log's write side, the turn-start thread
// read or the home's due-queue read, and how many files it read: a door a
// surface could take any of them through without naming it.
func engineHandOuts(dir string) (handOuts, int, error) {
	var out handOuts
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return out, 0, err
	}
	files := 0
	fset := token.NewFileSet()
	for _, p := range matches {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return out, 0, err
		}
		files++
		estateNames, domainNames := importNamesOf(parsed, estateImport), map[string]bool{}
		for _, imported := range domainImports {
			maps.Copy(domainNames, importNamesOf(parsed, imported))
		}
		chatletNames, notifyNames := importNamesOf(parsed, chatletImport), importNamesOf(parsed, notifyImport)
		gitletNames := importNamesOf(parsed, gitletImport)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			if namesWriteSide(fn.Type.Results, estateNames, domainNames) {
				out.writes = append(out.writes, fn.Name.Name)
			}
			if namesTurnRead(fn.Type.Results, estateNames, chatletNames, notifyNames) {
				out.reads = append(out.reads, fn.Name.Name)
			}
			if namesQueueRead(fn.Type.Results, gitletNames) {
				out.queue = append(out.queue, fn.Name.Name)
			}
		}
	}
	return out, files, nil
}

// namesQueueRead reports whether a declaration's results name the home's
// due-queue read: one of Gitlet's types that carry it ([queueReadNames]).
func namesQueueRead(results *ast.FieldList, gitletNames map[string]bool) bool {
	found := false
	ast.Inspect(results, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent {
			found = gitletNames[x.Name] && slices.Contains(queueReadNames, sel.Sel.Name)
		}
		return !found
	})
	return found
}

// namesTurnRead reports whether a declaration's results name the engine's
// turn-start thread read: this package's facade over it, Chatlet's thread
// reader or its answer, or notify's set of thread readers, which holds it.
func namesTurnRead(results *ast.FieldList, estateNames, chatletNames, notifyNames map[string]bool) bool {
	found := false
	ast.Inspect(results, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent {
			found = (estateNames[x.Name] && sel.Sel.Name == "TurnThreads") ||
				(chatletNames[x.Name] && (sel.Sel.Name == "ThreadReader" || sel.Sel.Name == "TurnThread")) ||
				(notifyNames[x.Name] && sel.Sel.Name == "ThreadReaders")
		}
		return !found
	})
	return found
}

// importNamesOf is every name file imports want under, a dot-import and a
// blank one aside.
func importNamesOf(file *ast.File, want string) map[string]bool {
	out := map[string]bool{}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != want {
			continue
		}
		name := path.Base(imported)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "." && name != "_" {
			out[name] = true
		}
	}
	return out
}

// namesWriteSide reports whether a declaration's results name the log's write
// side: one of this package's types that carry it, or the domain's writer.
func namesWriteSide(results *ast.FieldList, estateNames, domainNames map[string]bool) bool {
	found := false
	ast.Inspect(results, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent {
			found = (estateNames[x.Name] && slices.Contains(writeSideTypes, sel.Sel.Name)) ||
				(domainNames[x.Name] && sel.Sel.Name == "Writer")
		}
		return !found
	})
	return found
}

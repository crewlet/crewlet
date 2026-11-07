package issuereport

import (
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/panics"
)

// FatalKind is how a Go crash ended the process, read from the shape of the
// output the runtime wrote and the names of its own frames, never a value.
//
// NOT THE RUNTIME'S MESSAGE: a fatal error's `fatal error: ...` line is printed
// before the runtime starts copying its output to the crash file, so the file
// holds only the trace. A panic's first line is there, and is read for its kind.
type FatalKind string

// The ways a crash is read.
const (
	// FatalPanic is a panic nothing recovered.
	FatalPanic FatalKind = "panic"
	// FatalConcurrentMap is a map written while another goroutine used it.
	FatalConcurrentMap FatalKind = "concurrent_map"
	// FatalSyncMisuse is a lock or a wait group used against its contract.
	FatalSyncMisuse FatalKind = "sync_misuse"
	// FatalStackOverflow is a goroutine's stack past its limit.
	FatalStackOverflow FatalKind = "stack_overflow"
	// FatalNativeFault is a signal from memory the program did not own: a native
	// library's fault, as the store driver's engine is one.
	FatalNativeFault FatalKind = "native_fault"
	// FatalDeadlock is every goroutine asleep.
	FatalDeadlock FatalKind = "deadlock"
	// FatalOther is any other fatal error the runtime raised.
	FatalOther FatalKind = "other"
)

var fatalKinds = []FatalKind{FatalPanic, FatalConcurrentMap, FatalSyncMisuse, FatalStackOverflow,
	FatalNativeFault, FatalDeadlock, FatalOther}

const (
	// maxCrashRead bounds what is read of a crash's output: the crashing
	// goroutine is printed first, and a deadlock's every goroutine after it adds
	// nothing a report may carry.
	maxCrashRead = 256 << 10
	// maxCrashFrames bounds the frames read of the crashing goroutine, past the
	// hundred the runtime prints of an overflowing stack.
	maxCrashFrames = 512
)

// crash is what a crash's output says, typed.
type crash struct {
	kind FatalKind
	// panicKind is a panic's kind, with [FatalPanic] alone.
	panicKind panics.Kind
	// frames are the crashing goroutine's module frames, innermost first: all
	// of them, since whether the crash is exposed reads every one.
	frames []panics.Frame
}

// readCrash reads a crash's output, false for one no report is made of: an
// empty file, a trace it cannot read, or a host running out of memory or
// threads, which no maintainer can mend.
func readCrash(r io.Reader) (crash, bool) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCrashRead))
	if err != nil {
		return crash{}, false
	}
	return parseCrash(string(raw))
}

// traceBlock is one stack the runtime printed: the system stack or a goroutine.
type traceBlock struct {
	system  bool
	running bool
	// functions are every frame's function, innermost first.
	functions []string
	module    []panics.Frame
}

func parseCrash(text string) (crash, bool) {
	lines := strings.Split(text, "\n")
	first := ""
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	if first == "" {
		return crash{}, false
	}
	var c crash
	switch {
	case strings.HasPrefix(first, "panic: "):
		c.kind, c.panicKind = FatalPanic, panics.KindOfMessage(strings.TrimPrefix(first, "panic: "))
	case strings.HasPrefix(first, "[signal "):
		c.kind = FatalNativeFault
	}
	blocks := traceBlocks(lines)
	var system, crashing *traceBlock
	for i := range blocks {
		switch b := &blocks[i]; {
		case b.system && system == nil:
			system = b
		case !b.system && b.running && crashing == nil:
			crashing = b
		}
	}
	thrower := crashing
	if system != nil {
		thrower = system
	}
	if thrower != nil && hostExhausted(thrower.functions) {
		return crash{}, false
	}
	if c.kind == "" {
		c.kind = fatalKindOf(system, crashing)
	}
	if crashing != nil {
		c.frames = crashing.module
	}
	return c, true
}

// fatalKindOf reads a fatal error's kind from the runtime frames that raised it.
func fatalKindOf(system, crashing *traceBlock) FatalKind {
	switch {
	case system != nil && slices.Contains(system.functions, "runtime.newstack"):
		return FatalStackOverflow
	case crashing == nil:
		return FatalDeadlock
	}
	switch innermost := firstOf(crashing.functions); innermost {
	case "internal/runtime/maps.fatal":
		return FatalConcurrentMap
	case "internal/sync.fatal", "sync.fatal":
		return FatalSyncMisuse
	}
	return FatalOther
}

// hostExhausted reports a throw the host's memory or threads ran out under: the
// runtime frames between the throw and the first frame outside the runtime are an
// allocation's or a thread's.
func hostExhausted(functions []string) bool {
	for _, fn := range functions {
		if !strings.HasPrefix(fn, "runtime.") {
			return false
		}
		for _, mark := range []string{"mallocgc", "mcache", "mheap", "sysMap", "sysUsed", "sysAlloc",
			"persistentalloc", "Arena", "arena", "newosproc", "newm"} {
			if strings.Contains(fn, mark) {
				return true
			}
		}
	}
	return false
}

// traceBlocks splits a crash's output into its stacks, each read for its frames.
func traceBlocks(lines []string) []traceBlock {
	var blocks []traceBlock
	var at *traceBlock
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == "runtime stack:":
			blocks = append(blocks, traceBlock{system: true})
			at = &blocks[len(blocks)-1]
			continue
		case strings.HasPrefix(line, "goroutine ") && strings.HasSuffix(line, ":"):
			blocks = append(blocks, traceBlock{running: strings.Contains(line, "[running")})
			at = &blocks[len(blocks)-1]
			continue
		case at == nil || line == "" || strings.HasPrefix(line, "\t"):
			if line == "" {
				at = nil
			}
			continue
		case strings.HasPrefix(line, "created by "):
			at = nil
			continue
		}
		function, ok := frameFunction(line)
		if !ok || len(at.functions) >= maxCrashFrames {
			continue
		}
		at.functions = append(at.functions, function)
		if i+1 < len(lines) && strings.HasPrefix(function, modulePrefix) {
			if file, number, ok := frameFile(lines[i+1]); ok {
				at.module = append(at.module, panics.Frame{Function: function, File: file, Line: number})
			}
		}
	}
	return blocks
}

// frameFunction is a frame line's function, its arguments cut off: the runtime
// prints them in parentheses at the end, and they are values.
func frameFunction(line string) (string, bool) {
	if !strings.HasSuffix(line, ")") {
		return "", false
	}
	depth := 0
	for i := len(line) - 1; i >= 0; i-- {
		switch line[i] {
		case ')':
			depth++
		case '(':
			if depth--; depth == 0 {
				if i == 0 {
					return "", false
				}
				return line[:i], true
			}
		}
	}
	return "", false
}

// frameFile is a frame's file and line, from the tab-led line under it.
func frameFile(line string) (string, int, bool) {
	location, ok := strings.CutPrefix(line, "\t")
	if !ok {
		return "", 0, false
	}
	location, _, _ = strings.Cut(location, " ")
	colon := strings.LastIndex(location, ":")
	if colon < 0 {
		return "", 0, false
	}
	number, err := strconv.Atoi(location[colon+1:])
	if err != nil || number < 1 {
		return "", 0, false
	}
	return location[:colon], number, true
}

func firstOf(functions []string) string {
	if len(functions) == 0 {
		return ""
	}
	return functions[0]
}

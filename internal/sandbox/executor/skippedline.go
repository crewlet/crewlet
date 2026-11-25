package executor

import (
	"bytes"
	"regexp"
	"strconv"
	"sync"
)

// A LINE PAST THE BOUND NEVER HIDES A MODEL REQUEST: its decoder reads the
// line's ends ([LongLine]) for the request marker its count reads, at the
// head for a runner whose marker comes before any payload, at the tail for one
// whose marker follows it (Pi's stopReason, Qwen Code's usage, Cursor's
// model_call_id). A line whose ends show it may hold a request and show no
// marker is undecided ([Decoder.Undecided]), and a capped run with one is
// never judged within its cap. These read a field off a window by its key,
// since a window is no whole JSON value.

// fieldPatterns is each key's pattern of a string or number field, compiled
// once ([fieldPattern]).
var fieldPatterns sync.Map

// fieldPattern is the pattern of a field named key.
func fieldPattern(key string) *regexp.Regexp {
	if re, ok := fieldPatterns.Load(key); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*("(?:[^"\\]|\\.)*"|-?[0-9]+)`)
	fieldPatterns.Store(key, re)
	return re
}

// windowField is the value of the first (last false) or last (last true)
// field named key in window, unquoted where it is a string; "" where none.
func windowField(window []byte, key string, last bool) string {
	all := fieldPattern(key).FindAllSubmatch(window, -1)
	if len(all) == 0 {
		return ""
	}
	value := all[0][1]
	if last {
		value = all[len(all)-1][1]
	}
	if s, err := strconv.Unquote(string(value)); err == nil {
		return s
	}
	return string(value)
}

// windowInt is [windowField] read as a number, and whether it read as one.
func windowInt(window []byte, key string, last bool) (int, bool) {
	n, err := strconv.Atoi(windowField(window, key, last))
	return n, err == nil
}

// lineType is a skipped line's event type, the first `type` its head holds.
func lineType(line LongLine) string { return windowField(line.Head, "type", false) }

// inEither reports whether the line's head or tail holds needle.
func inEither(line LongLine, needle string) bool {
	return bytes.Contains(line.Head, []byte(needle)) || bytes.Contains(line.Tail, []byte(needle))
}

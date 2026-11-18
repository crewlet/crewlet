package gitlet

import (
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Sentence is a refusal of this domain worded for whoever reads it: a
// person's write surface, a seat's tool, the assistant. A state log refusal
// is said in the framework's words ([statelog.Sentence]), as every surface
// that asks the state log first does (internal/agent/builtin's
// WriterSentence), so a caller asking only this one gets the same words.
// Otherwise it is the error without its "gitlet: " prefix, which is for a
// log; an error a caller wrapped in its own context is its whole message.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	return strings.TrimPrefix(err.Error(), "gitlet: ")
}

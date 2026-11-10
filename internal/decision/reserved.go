package decision

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// ReservedOptionPrefix begins the id of an option only the engine composes:
// a grant an agent asked for (ADR-0118). A reader takes
// such a choice as that grant, so no author's arguments may name one.
const ReservedOptionPrefix = "grant-"

type engineOptionsKey struct{}

// WithEngineOptions marks ctx as carrying a decision whose options the engine
// composed itself, the one way a reserved option id is admitted.
func WithEngineOptions(ctx context.Context) context.Context {
	return context.WithValue(ctx, engineOptionsKey{}, true)
}

// EngineOptions reports whether ctx was marked by [WithEngineOptions].
func EngineOptions(ctx context.Context) bool {
	marked, _ := ctx.Value(engineOptionsKey{}).(bool)
	return marked
}

// ReservedFaultAt refuses every option id under [ReservedOptionPrefix], each
// named under prefix, or is nil when there is none.
func (d Decision) ReservedFaultAt(prefix string) error {
	var found []error
	for i, option := range d.Options {
		if strings.HasPrefix(option.ID, ReservedOptionPrefix) {
			found = append(found, invalid("%s.id %q begins with %q, which only an "+
				"access request the engine builds may use: ask for access with %s",
				fmt.Sprintf("%soptions[%d]", prefix, i), option.ID, ReservedOptionPrefix,
				toolname.RequestAccess))
		}
	}
	return Join(found)
}

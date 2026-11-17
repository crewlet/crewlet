package queries

import (
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// pushFilterParams is a push history's narrowing a question names: `pusher_kind` and `pusher`, and `since` and
// `until` as RFC 3339 instants, each the reader's to judge; nil where it names none.
func pushFilterParams(p Params) (*gitlet.PushFilter, error) {
	f := gitlet.PushFilter{PusherKind: gitlet.PusherKind(strings.TrimSpace(p.String("pusher_kind"))),
		Pusher: strings.TrimSpace(p.String("pusher"))}
	var err error
	if f.Since, err = instantParam(p, "since"); err != nil {
		return nil, err
	}
	if f.Until, err = instantParam(p, "until"); err != nil {
		return nil, err
	}
	if f.PusherKind == "" && f.Pusher == "" && f.Since.IsZero() && f.Until.IsZero() {
		return nil, nil
	}
	return &f, nil
}

package jsapi

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

// EVERY STATUS A PULL ENDS WITH IS NAMED.
//
// A pull is answered with deliveries and with statuses, and a reader that
// reads the wrong one as the end of its pull either waits out a pull the
// server has finished or takes a status meant for one pull as the end of the
// next. So each status the server sends a pull is named for what it is, and
// every one but a heartbeat ends the pull: its expiry, the byte bound the next
// message would pass, the count filled with bytes to spare — and anything
// else, which is the pull refused, carrying its code and its text.
//
// Mutation: read a heartbeat as an end, or a refusal as an expiry, and its row
// goes red.
func TestEveryStatusAPullEndsWithIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code, description string
		kind              EventKind
		ended             bool
	}{
		{"100", "Idle Heartbeat", Heartbeat, false},
		{"408", "Request Timeout", Expired, true},
		{"409", "Message Size Exceeds MaxBytes", PastMaxBytes, true},
		{"409", "Batch Completed", BatchCompleted, true},
		{"409", "Consumer Deleted", Refused, true},
		{"409", "Leadership Change", Refused, true},
		{"503", "", Refused, true},
	} {
		p := &Pulls{open: openPull{count: 4, bytesLeft: 1 << 20, bounded: true}}
		ev, err := p.read(&nats.Msg{Subject: "_INBOX.case", Header: nats.Header{
			statusHeader: {tc.code}, descriptionHeader: {tc.description}}})
		if err != nil {
			t.Errorf("%s %s: %v", tc.code, tc.description, err)
			continue
		}
		if ev.Kind != tc.kind || ev.Ended != tc.ended || !ev.Kind.Valid() {
			t.Errorf("%s %s = %q, ended %t; want %q, ended %t", tc.code, tc.description,
				ev.Kind, ev.Ended, tc.kind, tc.ended)
		}
		if ev.Code != tc.code || ev.Description != tc.description {
			t.Errorf("%s %s carried %q %q, want the status line's own", tc.code, tc.description,
				ev.Code, ev.Description)
		}
		refused := errors.Is(ev.Refusal(), ErrPullRefused)
		if refused != (tc.kind == Refused) {
			t.Errorf("%s %s: refusal %v, want one wrapping ErrPullRefused only for a refused pull",
				tc.code, tc.description, ev.Refusal())
		}
		if open := p.open.count > 0; open == tc.ended {
			t.Errorf("%s %s left the pull open %t, want it open only where the status does not "+
				"end it", tc.code, tc.description, open)
		}
	}
}

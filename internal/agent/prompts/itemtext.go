package prompts

import "strings"

// carried is an item as the engine hands it to a running model ([SteerMessage],
// [SteerMessageSentBefore], [LateCallArrived], [WakeMessage]): every C0 and C1
// control character but tab and newline, and U+2028 and U+2029, drawn as
// U+FFFD, so an item keeps to characters every harness carries. Each is one
// UTF-16 unit, as U+FFFD is, so no item's length moves; and JSON, which every
// hook answer and poll message is, then escapes nothing but the quote, the
// backslash, tab and newline (two bytes each), so an item of N units is at
// most 3N bytes beside its framing. Only the item changes: the text the
// engine keeps stays as written.
func carried(item string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return r
		case r < 0x20, r >= 0x80 && r <= 0x9f, r == ' ', r == ' ':
			return '�'
		}
		return r
	}, item)
}

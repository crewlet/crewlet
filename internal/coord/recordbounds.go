package coord

import (
	"fmt"
	"regexp"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT A RECORD'S FIELDS MAY HOLD, shared by the positions register's
// classes: a record's widest is the sum of its fields' widest as the store
// encodes them, so each field a record carries is bounded where it is
// validated, and every class is held to a widest its writers cannot pass
// (internal/coord/kv's positions holding, ADR-0175).

// MaxNodeIDBytes is the longest node id: internal/config's node id grammar,
// a letter or digit and at most 63 more of letters, digits, `.`, `_` or `-`,
// each of which a record holds as itself.
const MaxNodeIDBytes = 64

// MaxIncarnationBytes is the longest process incarnation: a node id, a colon
// and a UUID (internal/config's NewIncarnation), 101 bytes, rounded up.
const MaxIncarnationBytes = 128

// MaxActorBytes is the longest name a record's author fields (By, OperatorID)
// hold: every name [iam.ActorFor] records is a login or seat handle of at most
// [iam.MaxLogin] ASCII bytes, or a credential's name, a class and a UUID of at
// most 44.
const MaxActorBytes = iam.MaxLogin

// MaxStreamNameBytes is the longest state log's stream name a record names:
// every log is CREWLET_<DOMAIN>_LOG (internal/statelog's StreamSpec), a few
// dozen bytes, and internal/engine holds every registered one to this.
const MaxStreamNameBytes = 64

// MaxDomainNameBytes is the longest state log's domain name, and the longest
// feed group a domain declares; internal/engine holds every registered one to
// it.
const MaxDomainNameBytes = 64

// MaxLogDomains is the most state logs a record names one per entry (a node's
// positions, a hold's or a backup's streams). REASONED: the build registers
// fourteen, and internal/engine holds the register to this, so a build that
// outgrows it moves this bound and its widest-record tests with it.
const MaxLogDomains = 24

// logToken is the grammar of a stream, domain or group name: letters, digits,
// `_` and `-`, each a byte a record and a key hold as themselves.
var logToken = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_-]+$`) })

// ValidStreamName reports whether s can name a state log's stream in a
// record: at most [MaxStreamNameBytes] of letters, digits, `_` and `-`.
func ValidStreamName(s string) bool {
	return len(s) <= MaxStreamNameBytes && logToken().MatchString(s)
}

// ValidDomainName reports whether s can name a state log's domain, or a feed
// group, in a record: at most [MaxDomainNameBytes] of letters, digits, `_`
// and `-`.
func ValidDomainName(s string) bool {
	return len(s) <= MaxDomainNameBytes && logToken().MatchString(s)
}

// plainWithin reports whether s is at most max bytes, each a byte JSON writes
// as itself: an identifier a record's widest counts once, never escaped.
func plainWithin(s string, max int) bool { return len(s) <= max && encodedText(s) == len(s) }

// checkActor refuses an author's names past [MaxActorBytes], or holding a
// byte JSON escapes, naming the record they were to be written into.
func checkActor(record, by, operatorID string) error {
	if !plainWithin(by, MaxActorBytes) || !plainWithin(operatorID, MaxActorBytes) {
		return fmt.Errorf("coord: %s names an author or credential that is no name iam.ActorFor "+
			"records: each is at most %d plain bytes", record, MaxActorBytes)
	}
	return nil
}

// TextBytes is how many bytes s takes inside a stored record, its quotes left
// out: a byte JSON escapes (`<`, a control byte, an invalid one) takes six.
func TextBytes(s string) int { return encodedText(s) }

// FitText is s whole where it takes at most max bytes inside a stored record
// ([TextBytes]), else its longest opening that does with "…" after it, cut on
// a rune boundary: the cut an engine-written sentence takes to fit its bound
// however many of its bytes JSON escapes.
func FitText(s string, max int) string {
	if encodedText(s) <= max {
		return s
	}
	const mark = "…"
	room := max - len(mark)
	used, end := 0, 0
	for end < len(s) {
		_, size := utf8.DecodeRuneInString(s[end:])
		cost := encodedText(s[end : end+size])
		if used+cost > room {
			break
		}
		used += cost
		end += size
	}
	return s[:end] + mark
}

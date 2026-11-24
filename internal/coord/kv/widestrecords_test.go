package kv

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// widestInstant is an instant at its longest as a record encodes it: UTC, to
// the nanosecond.
var widestInstant = time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)

// widestText is text JSON writes in exactly n bytes, every byte it can as six:
// the widest a field held to n bytes as stored takes.
func widestText(n int) string { return strings.Repeat("<", n/6) + strings.Repeat("a", n%6) }

// widestLeaseName is a lease's name at its bound as a lease's record stores
// it, every byte it can as six.
func widestLeaseName() string {
	return strings.Repeat("\x01", coord.MaxLeaseNameBytes/6) + strings.Repeat("a", coord.MaxLeaseNameBytes%6)
}

// widestRecord is one record a small bucket holds at its widest, as this
// backend encodes it, under the key it is written to.
type widestRecord struct {
	name    string
	holding holding
	key     string
	value   []byte
}

// widestFleetRecords is every record a bucket of small values holds, each
// field at the bound its writer is held to.
func widestFleetRecords(t *testing.T) []widestRecord {
	t.Helper()
	must := func(raw []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var tally coord.Tally
	for i, w := range coord.WindowsAt(time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC), time.UTC) {
		tally.Slots[i] = coord.Slot{Label: w.Label, Used: math.MinInt, RefusedAt: widestInstant}
	}
	tally.At = widestInstant
	party := widestText(iam.MaxLogin)
	name := widestText(coord.MaxApplyNameBytes)
	meta := map[string]any{"pad": strings.Repeat("\x01", (coord.MaxLeaseMetaBytes-len(`{"pad":""}`))/6)}
	if n, err := coord.LeaseMetaBytes(meta); err != nil || n > coord.MaxLeaseMetaBytes ||
		n < coord.MaxLeaseMetaBytes-6 {
		t.Fatalf("premise: the widest Meta is %d bytes (%v), want within 6 of %d", n, err, coord.MaxLeaseMetaBytes)
	}
	lease := leaseValue{Resource: widestLeaseName(), Owner: widestLeaseName(), Epoch: math.MinInt64,
		Claim: uuid.NewString(), TTLNanos: math.MinInt64, AcquiredAt: widestInstant,
		Preferred: widestLeaseName(), Protocol: math.MinInt, Meta: meta}
	part := widestText(coord.MaxSessionClaimPartBytes)
	node := strings.Repeat("n", coord.MaxNodeIDBytes)
	s3 := config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
		Endpoint: "http://" + strings.Repeat("😀", config.MaxExternalURLLength-len("http://")),
		Bucket:   strings.Repeat("😀", config.MaxS3BucketLength),
		Prefix:   strings.Repeat("😀", config.MaxS3PrefixLength)}}
	closed := widestInstant
	return []widestRecord{
		{"a rate window", counterHolding, counterKey("valve", time.Hour, widestInstant),
			must(encodeRate(rateRecord{Count: math.MinInt}))},
		{"a token counter", counterHolding, encodeKey(coord.OrgScope), must(encodeTally(tally))},
		{"a delivery claim", claimHolding, encodeKey("delivery"),
			mustEncodeClaim(claimRecord{At: widestInstant, Until: widestInstant})},
		{"a scheduled fire", claimHolding, fireKey("minute", "runner"),
			must(json.Marshal(fireRecord{At: widestInstant}))},
		{"a rebase", claimHolding, encodeKey("seed"), must(json.Marshal(rebaseRecord{At: widestInstant}))},
		{"a custody claim", claimHolding, encodeKey("batch"),
			must(json.Marshal(custodyRecord{Node: node, At: widestInstant}))},
		{"a completion", ledgerHolding, ledgerKey("scope", "key"),
			must(json.Marshal(ledgerRecord{Detail: widestText(coord.MaxLedgerDetailBytes), At: widestInstant}))},
		{"a seen-through mark", ledgerHolding, seenKey("scope", "room"),
			[]byte(strings.Repeat("m", coord.MaxSeenMarkBytes))},
		{"an apply status", applyStatusHolding, encodeKey(node), must(json.Marshal(applyRecord{
			Epoch: math.MinInt64, RevisionID: name, Applied: math.MinInt64, Serving: name, Boots: name,
			Status: name, Error: coord.TruncateApplyError(widestText(6 * 4 * coord.MaxApplyErrorLength)),
			UpdatedAt: widestInstant}))},
		{"a channel", chatterHolding, encodeKey("channel"), must(encodeChannel(coord.Channel{
			Requester: party, Target: party, Messages: math.MinInt, OpenedAt: widestInstant,
			LastAt: widestInstant, ClosedAt: closed}))},
		{"a follow", chatterHolding, encodeKey("follow"), must(json.Marshal(followRecord{
			Reason: widestText(coord.MaxFollowReasonBytes), At: widestInstant}))},
		{"a mailbox record", chatterHolding, encodeKey(uuid.NewString()), must(encodeMailbox(
			coord.MailboxRecord{Handle: party, AbsentSince: widestInstant, RetiringSince: widestInstant}))},
		{"the agreed object store", objectsHolding, objectBackendKey, []byte(s3.Identity())},
		{"a lease", leaseHolding, encodeResource(lease.Resource), must(encodeValue(lease))},
		{"an epoch", epochsHolding, encodeResource(lease.Resource), must(encodeValue(resourceValue{
			Resource: lease.Resource, Epoch: math.MinInt64, Preferred: lease.Preferred}))},
		{"a session claim", runsHolding, coord.SessionClaimKey(part), must(json.Marshal(coord.SessionClaim{
			Session: part, Turn: part, Launch: part}))},
	}
}

// EVERY SMALL RECORD'S WIDEST FITS ITS CLASS, as this backend encodes it under
// the key it is written to: each field at the bound its writer holds it to,
// every byte of text one JSON writes as six. A lease names three names at
// their bound beside a Meta at its own and is 9,190 bytes; an apply status's
// error is cut to 2,000 bytes JSON may write as 12,000; the agreed object
// store is 13,292 bytes. Each class is a small value read at the term.
//
// Mutation: raise coord.MaxLeaseNameBytes to 4096, or config.MaxS3PrefixLength
// to 2048, and this is red.
func TestEveryFleetRecordsWidestFitsItsClass(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, w := range widestFleetRecords(t) {
		class := w.holding.of(w.key)
		seen[w.name] = true
		if len(w.value) > class.bound {
			t.Errorf("%s at its widest is %d bytes, past its class's %d", w.name, len(w.value), class.bound)
		}
		if class.reply == replyTerm && class.bound > coord.SmallValueBytes {
			t.Errorf("%s is read at the term in a class of %d bytes", w.name, class.bound)
		}
		t.Logf("%s: %d bytes at its widest, its class %d", w.name, len(w.value), class.bound)
	}
	if len(seen) < 16 {
		t.Fatalf("premise: %d records at their widest, want every small one", len(seen))
	}
}

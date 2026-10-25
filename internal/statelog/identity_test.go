package statelog_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A STREAM IS IDENTIFIED BY THE INSTANT ITS CREATOR STAMPED, never by the
// broker's own when it carries one.
//
// The broker dates a stream made by a snapshot restore to the restore, while
// the configuration the restore was handed — the stamp in it — is the
// original's; read off the broker, every restored log was a recreated one. A
// stream with no stamp is read exactly as it always was, and a stamp nobody can
// parse identifies nothing: unknown is not same.
func TestAStreamIsIdentifiedByTheInstantItsCreatorStamped(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2031, 4, 2, 3, 0, 0, 418_226_517, time.UTC)
	restored := stamped.Add(90 * 24 * time.Hour)
	info := func(metadata map[string]string, created time.Time) *jetstream.StreamInfo {
		return &jetstream.StreamInfo{
			Config:  jetstream.StreamConfig{Name: "CREWLET_PROBE_LOG", Metadata: metadata},
			Created: created,
		}
	}
	for name, tc := range map[string]struct {
		info    *jetstream.StreamInfo
		want    time.Time
		refused string
	}{
		"a stream its creator stamped, restored since": {
			info: info(statelog.StampCreatedAt(map[string]string{"_nats.ver": "2.15.0"}, stamped), restored),
			want: stamped,
		},
		"a stream with no stamp": {
			info: info(map[string]string{"_nats.ver": "2.15.0"}, restored),
			want: restored,
		},
		"a stamp in another zone": {
			info: info(map[string]string{
				statelog.CreatedAtKey: stamped.In(time.FixedZone("x", 3600)).Format(time.RFC3339Nano),
			}, restored),
			want: stamped,
		},
		"a stamp nobody can read": {
			info:    info(map[string]string{statelog.CreatedAtKey: "yesterday"}, restored),
			refused: statelog.CreatedAtKey,
		},
		"no stream information at all": {
			info:    nil,
			refused: "no stream information",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := statelog.StreamCreatedAt(tc.info)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("StreamCreatedAt = (%v, %v), want a refusal naming %q — an "+
						"instant nobody can read identifies nothing, and read as the "+
						"broker's it would call a restored stream recreated", got, err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("StreamCreatedAt: %v", err)
			}
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("StreamCreatedAt = %v, want %v in UTC", got, tc.want)
			}
		})
	}

	// AND A STAMP READ BACK IS THE SAME STREAM at the resolution a checkpoint
	// row keeps, which is what every comparison of it is.
	back, err := statelog.StreamCreatedAt(info(statelog.StampCreatedAt(nil, stamped), restored))
	if err != nil {
		t.Fatalf("read a stamp back: %v", err)
	}
	if got := statelog.IdentityOf(stamped, back, true); got != statelog.StreamSame {
		t.Fatalf("a stamp read back is %s against the instant it stamped, want same", got)
	}
	if got := statelog.IdentityOf(stamped, restored, true); got != statelog.StreamRecreated {
		t.Fatalf("the broker's instant for the restored stream is %s, want recreated — "+
			"the case above proves nothing unless the broker's instant alone would "+
			"have refused it", got)
	}
}

// THE STAMP IS ADDED TO A COPY. A stream specification is a value several
// creates share, and writing the caller's map would hand the first create's
// instant to every later one.
func TestAStampLeavesTheCallersMetadataAlone(t *testing.T) {
	t.Parallel()
	mine := map[string]string{"owner": "probe"}
	kept := maps.Clone(mine)
	at := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	got := statelog.StampCreatedAt(mine, at)
	if !maps.Equal(mine, kept) {
		t.Fatalf("StampCreatedAt wrote the caller's metadata: %v", mine)
	}
	if got["owner"] != "probe" || got[statelog.CreatedAtKey] != at.Format(time.RFC3339Nano) {
		t.Fatalf("StampCreatedAt = %v, want the caller's keys and the stamp", got)
	}
	if statelog.StampCreatedAt(nil, at)[statelog.CreatedAtKey] == "" {
		t.Fatal("StampCreatedAt with no metadata stamped nothing")
	}
	if _, err := statelog.StreamCreatedAt(&jetstream.StreamInfo{}); err != nil {
		t.Fatalf("a stream with no metadata at all is refused: %v", err)
	}
}

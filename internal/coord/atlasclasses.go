package coord

// The atlas buckets' key classes (ADR-0175, "The atlas buckets declare their
// classes").
//
// A source's standing is two records. The STANDING is what the collect duty's
// tick and every report read, every source's at once: when the source was
// read, what it cost and refused, and how many entries it has stopped seeing.
// Its MISSES are those entries' ids, which only the duty's visit of that one
// source reads, by its key. Kept apart because the misses are most of a
// standing's widest form, and a tick or a report listing every source at its
// widest would wait the crossing of thirteen times that.
const (
	// AtlasSourceStandingClass keys a source's standing, `standing.<kind>`.
	AtlasSourceStandingClass = "standing"
	// AtlasSourceMissesClass keys a source's misses, `missing.<kind>`.
	AtlasSourceMissesClass = "missing"
)

// MaxAtlasSourceStandingBytes bounds a source's standing as stored. Its widest
// plain form, 200 scopes (one per server internal/config admits) of a 40-byte
// id, 16 refused parts of three 512-byte texts, a 2,000-byte error and every
// count at 19 digits, is 53,043 bytes (TestTheWidestAtlasStandingFitsItsBound);
// its writer cuts text a vendor makes wider by escaping until it fits. Its
// read decides a person's ask for a read, so it waits its reply's crossing:
// 2.5 s at the floor, a typical standing of 2 KB inside the term.
const MaxAtlasSourceStandingBytes = 64 << 10

// MaxAtlasSourceMissesBytes bounds a source's misses as stored. Its widest,
// internal/atlas/collect's MaxMissing of 8,192 entry ids each missed at an
// instant of its own, is 770,092 bytes (TestTheWidestAtlasMissesFitTheirBound).
// Only the duty reads it, and no caller waits on that read: its arrival is
// 31.0 s at the floor, once per visit of a source that has stopped seeing
// thousands of entries, and none for one that has not.
const MaxAtlasSourceMissesBytes = 768 << 10

// AtlasSourceStandingKey is kind's standing key.
func AtlasSourceStandingKey(kind string) string { return DocumentKey(AtlasSourceStandingClass, kind) }

// AtlasSourceMissesKey is kind's misses key.
func AtlasSourceMissesKey(kind string) string { return DocumentKey(AtlasSourceMissesClass, kind) }

// AtlasSourceOf recovers the source a key of class names, reporting false for
// a key of another class and one this grammar did not write.
func AtlasSourceOf(key, class string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != class || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// The atlas code scans' keys: a repository's standing under the host it is
// read from, `repo.<host>.<repo>`, and a host's own standing of its last pass,
// `host.<host>`. A host is [AtlasCodeGitlet] or a forge's integration kind, so
// a reader of one host's repositories lists them alone, never every other
// host's. Every standing is a few KiB of counts, a tip and an error, read at
// the term (coord.SmallValueBytes).
const (
	// AtlasCodeRepoClass keys a repository's standing.
	AtlasCodeRepoClass = "repo"
	// AtlasCodeHostClass keys a host's own standing.
	AtlasCodeHostClass = "host"
	// AtlasCodeGitlet is the host of every Gitlet repository, keyed by its
	// id; no integration's kind spells it.
	AtlasCodeGitlet = "gitlet"
)

// AtlasCodeScanKey is the key of host's repository repo, or of host's own
// standing for repo "".
func AtlasCodeScanKey(host, repo string) string {
	if repo == "" {
		return DocumentKey(AtlasCodeHostClass, host)
	}
	return DocumentKey(AtlasCodeRepoClass, host, repo)
}

// AtlasCodeScanFilter selects the standing of every repository of host.
func AtlasCodeScanFilter(host string) string { return DocumentFilter(AtlasCodeRepoClass, host) }

// AtlasCodeRepoOf recovers the repository a key of host's repositories names,
// reporting false for any other key and one this grammar did not write.
func AtlasCodeRepoOf(key, host string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != AtlasCodeRepoClass || segments[1] != host ||
		segments[2] == "" {
		return "", false
	}
	return segments[2], true
}

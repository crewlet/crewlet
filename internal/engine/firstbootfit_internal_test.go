package engine

import (
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// yearFivePeak is the reference company's fifth-year corpus, 8.46 GB, which a
// model change puts in the vector changelog's window at once.
const yearFivePeak = int64(8_460_000_000)

// firstBootBroker is the broker a fresh node's first boot sizes its state logs
// against on a stream volume with free bytes free, in the deployment every page
// prints its figures for: an embedded broker, whose limit is three quarters of
// the free space, holding nothing yet but the `nats` object bucket's derived
// reservation, which is spent before the logs are sized ([objectsReservation]).
func firstBootBroker(stream config.Stream, free int64) sizingHost {
	limit := free / embeddedShareDen * embeddedShareNum
	return sizingHost{budget: jetstream.StorageBudget{Limit: limit,
		Committed: derivedObjectsReservation(limit, stream.DeclaredLogCeilings()),
		Source:    jetstream.BudgetServerStore}}
}

// firstBootAsks is what each state log asks for on a first boot on free bytes
// free, by domain, leaving out the registered domain without names ("" for
// none): what the logs of a build that did not register it would ask.
func firstBootAsks(t *testing.T, stream config.Stream, free int64, without string) map[string]domainCeiling {
	t.Helper()
	asked := map[string]domainCeiling{}
	for _, domain := range registeredDomains() {
		if domain.Name() == without {
			continue
		}
		ceiling, err := tierACeiling(stream, domain, free)
		if err != nil {
			t.Fatalf("tierACeiling(%s): %v", domain.Name(), err)
		}
		asked[domain.Name()] = ceiling
	}
	return asked
}

// firstBootFit is what a first boot on free bytes free creates each state log
// with, by domain, leaving out the registered domain without names: every log
// is missing, so it is [fitCeilings] of the asks into the logs' share of what
// [firstBootBroker] can grant — [sizeCeilings]'s arithmetic, without a broker
// to read or a line to log on each of the hundreds of boots a scan below sizes
// (TestTheFitThePagesPrintIsTheBootsOwn holds the two to each other).
func firstBootFit(t *testing.T, stream config.Stream, free int64, without string) map[string]domainCeiling {
	t.Helper()
	broker := firstBootBroker(stream, free)
	pool := int64(float64(logsGrantable(broker.budget, nil, free, 0)) * StreamBudgetShare)
	return fitCeilings(firstBootAsks(t, stream, free, without), map[string]int64{}, pool)
}

// THE FIT THE PAGES PRINT IS THE BOOT'S OWN: on every volume and setting the
// pages print a figure for, [firstBootFit] creates exactly what the boot's own
// sizing creates against the same broker — so a step the sizing gains fails
// here rather than leaving every figure computed from the fit behind it.
func TestTheFitThePagesPrintIsTheBootsOwn(t *testing.T) {
	t.Parallel()
	for name, stream := range map[string]config.Stream{
		"every field unset":         {},
		"the code log at its floor": {CodeLogMaxBytes: config.CodeLogMaxBytesFloor},
		"the code log at 5 GiB":     {CodeLogMaxBytes: 5 * gib},
	} {
		for _, free := range []int64{14 * gib, 50 * gib, 100 * gib, 256 * gib, 600 * gib, 700 * gib} {
			booted, err := sizeCeilings(t.Context(), firstBootBroker(stream, free), stream, free,
				"/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("%s on %d bytes free: sizeCeilings: %v", name, free, err)
			}
			if fit := firstBootFit(t, stream, free, ""); !maps.Equal(fit, booted) {
				t.Errorf("%s on %d bytes free: the pages' fit creates %+v and the boot %+v",
					name, free, fit, booted)
			}
		}
	}
}

// THE PAGES PRINT THE CEILINGS A FIRST BOOT CREATES: every figure of the fit —
// what a fresh node's first boot creates each state log with on a given volume
// under the embedded broker and the default `nats` bucket, with every field
// unset, without the code log, and with the code log at its floor — is
// computed here by the boot's own arithmetic ([firstBootFit]) and held on
// every page that prints it: docs/guides/replication.md's table of what the
// code log's reservation costs and the prose around it, docs/guides/
// deployment.md's bullets, docs/guides/retention.md's re-embedding excursion,
// Tier A's code log field and [StreamBudgetShare]'s reason.
//
// Nothing held them before, and when the share moved from a half to five
// eighths the pages were restated by hand and the field's own doc was missed:
// it went on pricing the code log's reservation at 500 GiB and 12.5 GiB
// rather than 626 GiB and 9.9 (#379). Now a change to the register, an ask,
// the share, the bucket or the floor fails here naming each figure to restate.
//
// Mutation: move [StreamBudgetShare], [MinDomainCeiling],
// config.DerivedCodeLogNumerator or the bucket's quarter, or restate one
// figure on one page, and a page no longer says what a first boot creates.
func TestThePagesPrintTheCeilingsAFirstBootCreates(t *testing.T) {
	t.Parallel()
	unset := config.Stream{}
	floored := config.Stream{CodeLogMaxBytes: config.CodeLogMaxBytesFloor}
	trackerLog, vectors, codeLog := tracker.Domain{}.Name(), search.Domain{}.Name(), code.Domain{}.Name()
	quarters := []string{pages.Domain{}.Name(), artifactsdomain.Domain{}.Name(),
		artifactdata.Domain{}.Name()}
	// THE ORDER THE DEPLOYMENT GUIDE ADDS THE ASKS UP IN, which must name
	// every log the register sizes: a log registered beside them is an ask
	// the sum leaves out.
	order := []string{trackerLog, vectors, pages.Domain{}.Name(), artifactsdomain.Domain{}.Name(),
		artifactdata.Domain{}.Name(), codeLog, usage.Domain{}.Name(), iamdomain.Domain{}.Name()}
	var registered []string
	for _, domain := range registeredDomains() {
		registered = append(registered, domain.Name())
	}
	if !slices.Equal(slices.Sorted(slices.Values(order)), slices.Sorted(slices.Values(registered))) {
		t.Fatalf("the pages add up the asks of %v, and the register sizes %v: name every log "+
			"the register sizes, here and in docs/guides/deployment.md's sum", order, registered)
	}
	fit := func(stream config.Stream, free int64, without string) map[string]domainCeiling {
		return firstBootFit(t, stream, free, without)
	}
	// SCALED: whether a first boot on free creates any log below its ask.
	scaled := func(stream config.Stream, without string) func(int64) bool {
		return func(free int64) bool {
			return !maps.Equal(fit(stream, free, without), firstBootAsks(t, stream, free, without))
		}
	}
	// HOLDS: whether a first boot on free creates the vector changelog with at
	// least the room it needs.
	holds := func(stream config.Stream, without string, room float64) func(int64) bool {
		return func(free int64) bool {
			return float64(fit(stream, free, without)[vectors].Bytes) >= room
		}
	}
	// THE PEAK, AND THE PEAK WITH THE ALARM'S TENTH OF THE CEILING FREE: below
	// the first a model change does not fit, and below the second it fits and
	// raises log_headroom.
	peak := float64(yearFivePeak)
	alarmed := peak / (1 - statelog.HeadroomAlarmFraction)
	left := func(ceiling int64) float64 { return 1 - peak/float64(ceiling) }

	everyAsk := leastFree(t, scaled(unset, ""), false)
	everyAskWithout := leastFree(t, scaled(unset, codeLog), false)
	alarmLine := leastFree(t, holds(unset, "", alarmed), true)
	alarmLineWithout := leastFree(t, holds(unset, codeLog, alarmed), true)
	alarmLineFloored := leastFree(t, holds(floored, "", alarmed), true)
	fitLine := leastFree(t, holds(unset, "", peak), true)
	fitLineWithout := leastFree(t, holds(unset, codeLog, peak), true)

	// A FIFTH SHORTER: how much shorter the code log makes the mutation log a
	// first boot creates, which is the same fraction wherever the same logs
	// are scaled with it and without it, and the range of volumes it holds
	// on — the whole gibibytes on which it prints as it does at 100 GiB.
	shorter := func(free int64) float64 {
		return 1 - float64(fit(unset, free, "")[trackerLog].Bytes)/
			float64(fit(unset, free, codeLog)[trackerLog].Bytes)
	}
	band := fmt.Sprintf("%.1f", 100*shorter(100*gib))
	var from, to int64
	for free := 16 * gib; free <= 700*gib; free += gib {
		if fmt.Sprintf("%.1f", 100*shorter(free)) != band {
			continue
		}
		if from == 0 {
			from = free
		} else if free != to+gib {
			t.Errorf("the mutation log is %s %% shorter on %d GiB and on %d GiB free and not "+
				"between them, so no one range of volumes has it", band, to/gib, free/gib)
		}
		to = free
	}

	// THE WORDS, each held to the range it is true over.
	for _, w := range []struct {
		words    string
		value    float64
		low, top float64
	}{
		{"the code log makes the mutation log about a fifth shorter", shorter(100 * gib), 0.175, 0.225},
		{"a little more below that range", shorter(from-gib) - shorter(from), 1e-4, 0.05},
		{"and less above it", shorter(to) - shorter(to+gib), 1e-4, 0.05},
		{"a 64 GiB volume's vector changelog is less than the corpus",
			float64(fit(unset, 64*gib, "")[vectors].Bytes) / peak, 0, 1},
	} {
		if w.value < w.low || w.value > w.top {
			t.Errorf("the pages say %s, and a first boot makes it %.4f: reword it where this "+
				"test holds it", w.words, w.value)
		}
	}

	at := func(stream config.Stream, free int64, without, domain string) string {
		return gibText(fit(stream, free, without)[domain].Bytes)
	}
	percentLeft := func(stream config.Stream, free int64, without string) int {
		return int(math.Round(100 * left(fit(stream, free, without)[vectors].Bytes)))
	}
	about := func(gibs float64) int64 { return int64(math.Round(gibs)) }
	// THE ASKS AT THE CLAMP, a volume past every log's largest derived ask.
	clamped := firstBootAsks(t, unset, 1<<50, "")
	var sum int64
	var addends []string
	for _, name := range order {
		sum += clamped[name].Bytes
		addends = append(addends, gibText(clamped[name].Bytes))
	}
	share := 100 * float64(int64(float64(logsGrantable(firstBootBroker(unset, 100*gib).budget, nil,
		100*gib, 0))*StreamBudgetShare)) / float64(100*gib)
	codeAsks := fmt.Sprintf("(%s..%s GiB from a volume alone)",
		gibText(firstBootAsks(t, unset, 0, "")[codeLog].Bytes), gibText(clamped[codeLog].Bytes))

	for file, wants := range map[string][]string{
		"docs/guides/replication.md": {
			fmt.Sprintf("below about %d GiB free its ask is room the others give up", about(everyAsk)),
			codeAsks,
			fmt.Sprintf("with less than about %d GiB free the %s asks are more than the logs' share "+
				"of it", about(everyAsk), numberWord(t, len(order))),
			fmt.Sprintf("its quarter of the broker's limit, about %.0f %% of the free space", share),
			fmt.Sprintf("about a fifth (%s %%) shorter on any first boot between about %d and %d GiB "+
				"free, a little more below that and less above it", band, from/gib, to/gib),
			fmt.Sprintf("The changelog is created at %s GiB on 100 GiB free, where it would be %s GiB "+
				"without the code log: that re-embedding then leaves %d %% of it free, where "+
				"without the code log it leaves about %d %%.", at(unset, 100*gib, "", vectors),
				at(unset, 100*gib, codeLog, vectors), percentLeft(unset, 100*gib, ""),
				percentLeft(unset, 100*gib, codeLog)),
			fmt.Sprintf("on any first boot below about %d GiB free, where the line would be about "+
				"%d GiB without the code log, and it does not fit below about %d GiB (about %d GiB "+
				"without the code log)", about(alarmLine), about(alarmLineWithout), about(fitLine),
				about(fitLineWithout)),
			fmt.Sprintf("(at 100 GiB free, the mutation log and the vector changelog are each created "+
				"at %s GiB, which a year-five re-embedding leaves about %d %% free, and the line "+
				"below which it alarms moves to about %d GiB)", at(floored, 100*gib, "", vectors),
				percentLeft(floored, 100*gib, ""), about(alarmLineFloored)),
		},
		"docs/guides/deployment.md": {
			fmt.Sprintf("its quarter of it, about %.0f %% of the free space on a first boot — which is "+
				"every volume with less than about %d GiB free (with the mutation log at its %s GiB "+
				"clamp the %s ask for about %d GiB: %s)", share, about(everyAsk),
				gibText(clamped[trackerLog].Bytes), numberWord(t, len(order)),
				about(float64(sum)/float64(gib)), strings.Join(addends, " + ")),
			fmt.Sprintf("Below about %d GiB free it comes out of the other logs' share: at 100 GiB "+
				"free the mutation log and the vector changelog are created at %s GiB each rather "+
				"than %s, at 256 GiB at %s rather than %s — windows about a fifth shorter",
				about(everyAsk), at(unset, 100*gib, "", trackerLog),
				at(unset, 100*gib, codeLog, trackerLog), at(unset, 256*gib, "", trackerLog),
				at(unset, 256*gib, codeLog, trackerLog)),
		},
		"docs/guides/retention.md": {
			fmt.Sprintf("about %d %% of the %s GiB clamp, which a volume of about %d GiB free or more "+
				"gets; about %d %% of the %s GiB a 200 GiB volume gets; about %d %% of the %s GiB a "+
				"128 GiB volume gets; about %d %% of the %s GiB a 100 GiB volume gets; and on a "+
				"64 GiB volume, whose %s GiB is less than the corpus, it does not fit at all",
				int(math.Round(100*left(clamped[vectors].Bytes))), gibText(clamped[vectors].Bytes),
				about(everyAsk), percentLeft(unset, 200*gib, ""), at(unset, 200*gib, "", vectors),
				percentLeft(unset, 128*gib, ""), at(unset, 128*gib, "", vectors),
				percentLeft(unset, 100*gib, ""), at(unset, 100*gib, "", vectors),
				at(unset, 64*gib, "", vectors)),
			fmt.Sprintf("**on a volume of less than about %d GiB free, a year-five re-embedding raises "+
				"`log_headroom`**, and below about %d GiB the log refuses it. The line would be "+
				"about %d GiB without the code log's share", about(alarmLine), about(fitLine),
				about(alarmLineWithout)),
			fmt.Sprintf("which moves the line to about %d GiB and leaves a 100 GiB volume's year-five "+
				"re-embedding about %d %% free", about(alarmLineFloored), percentLeft(floored, 100*gib, "")),
		},
		"internal/config/bootstrap.go": {
			fmt.Sprintf("below about %d GiB free it comes out of the other logs' share (at 100 GiB "+
				"the mutation log is created at %s GiB rather than %s;", about(everyAsk),
				at(unset, 100*gib, "", trackerLog), at(unset, 100*gib, codeLog, trackerLog)),
		},
		"internal/engine/statelogceilings.go": {
			fmt.Sprintf("the same re-embedding fits from about %d GiB free", about(fitLine)),
		},
	} {
		page := prose(t, file)
		for _, want := range wants {
			if !strings.Contains(page, want) {
				t.Errorf("%s does not say %q", file, want)
			}
		}
	}

	// THE TABLE, row by row: what a first boot creates on each volume it
	// names, without the code log and with it, and from about where nothing
	// is scaled at all.
	rows := fitTable(t)
	for _, row := range rows {
		var want string
		if row.andUp {
			want = fmt.Sprintf("| %d GiB and up | %s GiB | %s GiB | %s GiB |", about(everyAsk),
				gibText(clamped[trackerLog].Bytes), gibText(clamped[quarters[0]].Bytes),
				gibText(clamped[codeLog].Bytes))
		} else {
			want = fmt.Sprintf("| %d GiB | %s → %s GiB | %s → %s GiB | %s GiB |", row.free/gib,
				at(unset, row.free, codeLog, trackerLog), at(unset, row.free, "", trackerLog),
				at(unset, row.free, codeLog, quarters[0]), at(unset, row.free, "", quarters[0]),
				at(unset, row.free, "", codeLog))
		}
		if row.line != want {
			t.Errorf("replication.md's table says\n\t%s\nand a first boot creates\n\t%s", row.line, want)
		}
		// EACH, as the columns say: the mutation log and the vector
		// changelog are created alike, and so are the three quarter-sized
		// logs.
		created := fit(unset, row.free, "")
		if created[vectors].Bytes != created[trackerLog].Bytes {
			t.Errorf("on %d GiB free the vector changelog is created at %d bytes and the mutation "+
				"log at %d, which the table prints as one figure", row.free/gib,
				created[vectors].Bytes, created[trackerLog].Bytes)
		}
		for _, name := range quarters[1:] {
			if created[name].Bytes != created[quarters[0]].Bytes {
				t.Errorf("on %d GiB free %s's log is created at %d bytes and %s's at %d, which the "+
					"table prints as one figure", row.free/gib, name, created[name].Bytes,
					quarters[0], created[quarters[0]].Bytes)
			}
		}
	}
	if len(rows) == 0 || !rows[len(rows)-1].andUp {
		t.Errorf("replication.md's table ends on no row of the volumes on which nothing is scaled")
	}
	if about(everyAskWithout) != to/gib {
		t.Errorf("the mutation log stops being a fifth shorter at %d GiB free, and without the code "+
			"log nothing is scaled from about %d GiB: the band the guide prints ends where the "+
			"logs without the code log stop being scaled", to/gib, about(everyAskWithout))
	}
}

// fitRow is one row of replication.md's table of what the code log's
// reservation costs: the volume it is for, whether it is the row of every
// volume from there up, and the line as the guide writes it.
type fitRow struct {
	free  int64
	andUp bool
	line  string
}

// fitTable reads replication.md's table of what a first boot creates each log
// with, without the code log and with it.
func fitTable(t *testing.T) []fitRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "replication.md"))
	if err != nil {
		t.Fatalf("read the replication guide: %v", err)
	}
	const header = "| Free on the stream volume | Mutation log and vector changelog, each | " +
		"Knowledge base's log, artifacts log and artifact datastore's log, each | Code log |"
	_, table, found := strings.Cut(string(raw), header+"\n")
	if !found {
		t.Fatalf("the replication guide has no table headed %q", header)
	}
	var rows []fitRow
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		volume := strings.TrimSpace(strings.Split(strings.Trim(line, "|"), "|")[0])
		figure, andUp := strings.CutSuffix(volume, " and up")
		gibs, err := strconv.ParseInt(strings.TrimSuffix(figure, " GiB"), 10, 64)
		if err != nil || !strings.HasSuffix(figure, " GiB") {
			t.Fatalf("a row of the replication guide's table names %q, which is no volume in "+
				"whole gibibytes", volume)
		}
		rows = append(rows, fitRow{free: gibs * gib, andUp: andUp, line: line})
	}
	return rows
}

// leastFree is the least free space, in gibibytes and to the mebibyte, from
// which at answers to about a first boot's fit: the volume a page's "about N
// GiB" rounds. The search runs from about a fresh node's floor to a tebibyte,
// over a fit that turns once in it — every figure the pages print grows with
// the volume — and fails rather than guess where at does not turn.
func leastFree(t *testing.T, at func(free int64) bool, to bool) float64 {
	t.Helper()
	lo, hi := 14*gib, int64(1)<<40
	if at(lo) == to || at(hi) != to {
		t.Fatalf("a first boot's fit does not turn between %d and %d bytes free", lo, hi)
	}
	for hi-lo > 1<<20 {
		mid := lo + (hi-lo)/2
		if at(mid) == to {
			hi = mid
		} else {
			lo = mid
		}
	}
	return float64(hi) / float64(gib)
}

// gibText spells a ceiling as the pages do: a whole number of gibibytes, or a
// half or a third of one past it, as a whole and a vulgar fraction (64, ½,
// 42⅔) — the figures an ask is derived as — and otherwise to a tenth (9.9).
func gibText(b int64) string {
	for _, part := range []struct {
		of        int64
		fractions []string
	}{{3, []string{"", "⅓", "⅔"}}, {2, []string{"", "½"}}} {
		n := (part.of*b + gib/2) / gib
		if d := part.of*b - n*gib; d > -part.of && d < part.of {
			whole, frac := n/part.of, part.fractions[n%part.of]
			if whole == 0 && frac != "" {
				return frac
			}
			return strconv.FormatInt(whole, 10) + frac
		}
	}
	return strconv.FormatFloat(float64(b)/float64(gib), 'f', 1, 64)
}

// numberWord is n as the pages write a count of logs.
func numberWord(t *testing.T, n int) string {
	t.Helper()
	words := []string{"", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve"}
	if n < 1 || n >= len(words) {
		t.Fatalf("the pages count %d logs, which this test has no word for", n)
	}
	return words[n]
}

// prose is a file of this tree as a reader reads its words: a comment's
// markers taken off each line and every run of white space one space, so a
// sentence is found however a page or a comment wraps it.
func prose(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimSpace(line), "//")
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

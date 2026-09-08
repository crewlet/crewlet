package kv

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE TABLE, THE RATIONALE AND EVERY COUNT IN THE FILE ARE ONE ASSERTION.
//
// Three things in this file say how many buckets there are, and they drifted:
// the `open` table had grown, the "# Why ELEVEN buckets" header had not, and a
// third comment on the replica count said "all eleven" — so a reader learning
// the estate from the doc learned a number the code disagreed with, and the
// rationale that made each bucket's retention a DECISION was missing entries
// for four of them.
//
// One test rather than three, because the failure is always the same failure:
// a bucket was added and the prose was not. It fails NAMING which of the three
// is behind.
func TestEveryBucketHasALifetimeClass(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("fleet.go")
	if err != nil {
		t.Fatalf("read fleet.go: %v", err)
	}
	text := string(src)

	// (1) THE TABLE is the authority: it is what runs.
	rows := regexp.MustCompile(`\{&store\.\w+, \w+Suffix,`).FindAllString(text, -1)
	if len(rows) == 0 {
		t.Fatal("no bucket rows found in the open table: this test is measuring " +
			"its own regexp rather than the estate")
	}

	// (2) THE RATIONALE names each bucket and why its retention is what it
	// is. An entry missing means a bucket whose lifetime nobody decided.
	rationale, ok := section(text, "// # Why ", "// Putting two of those in one bucket")
	if !ok {
		t.Fatal("the bucket rationale block is gone: it is what makes each " +
			"retention a decision rather than a default")
	}
	for _, row := range rows {
		field := regexp.MustCompile(`&store\.(\w+),`).FindStringSubmatch(row)[1]
		if !strings.Contains(rationale, entryName(field)) {
			t.Errorf("bucket %q has a row in the open table and no entry in the "+
				"rationale: every bucket's retention is a decision, and one "+
				"with no entry is one nobody made", field)
		}
	}

	// (3) NO COMMENT IN THE FILE STATES A COUNT THAT DISAGREES. This is the
	// half that caught ":123" — a sentence about the replica count that
	// happened to carry a bucket total.
	words := map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
		"twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
		"sixteen": 16, "seventeen": 17, "eighteen": 18,
	}
	// ADJACENT AND PLURAL. "two of those in one bucket" is prose about a
	// pair, not a count of the estate, and a looser match measures the
	// English rather than the number.
	counts := regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen) buckets\b`)
	for _, m := range counts.FindAllString(text, -1) {
		word := strings.ToLower(regexp.MustCompile(`(?i)^\w+`).FindString(m))
		n, known := words[word]
		if !known {
			continue
		}
		if n != len(rows) {
			t.Errorf("a comment says %q while the open table has %d rows: "+
				"a reader learning this estate from the doc learns a number "+
				"the code disagrees with", strings.TrimSpace(m), len(rows))
		}
	}
	t.Logf("%d buckets, each with a rationale entry", len(rows))
}

// section returns the text between the first start and the following end.
func section(text, start, end string) (string, bool) {
	i := strings.Index(text, start)
	if i < 0 {
		return "", false
	}
	j := strings.Index(text[i:], end)
	if j < 0 {
		return "", false
	}
	return text[i : i+j], true
}

// entryName maps a struct field to the name the rationale uses for it.
func entryName(field string) string {
	switch field {
	case "cooldowns":
		return "cooldowns"
	case "kbVectors":
		return "kbVectors"
	default:
		return field
	}
}

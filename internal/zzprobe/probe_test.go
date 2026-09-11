package zzprobe

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "turso.tech/database/tursogo"
)

func TestLikeSemantics(t *testing.T) {
	db, err := sql.Open("turso", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mk := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mk(`CREATE TABLE tracker_tasks (id TEXT PRIMARY KEY, key TEXT NOT NULL, title TEXT NOT NULL, removed_at INTEGER)`)
	mk(`CREATE INDEX tracker_tasks_key_idx ON tracker_tasks (key)`)
	mk(`INSERT INTO tracker_tasks VALUES ('1','ENG-7','Fix the Deploy pipeline',NULL)`)
	mk(`INSERT INTO tracker_tasks VALUES ('2','ENG-12','100% coverage push',NULL)`)
	mk(`INSERT INTO tracker_tasks VALUES ('3','OPS-3','a3b weirdness',NULL)`)

	probe := func(label, pat1, pat2 string) {
		rows, err := db.Query(`SELECT key FROM tracker_tasks WHERE (key LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`, pat1, pat2)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			got = append(got, k)
		}
		t.Logf("%-30s -> %v", label, got)
	}
	// case: lowercase find against an uppercase key
	probe("q=eng-7 (lowercase)", "eng-7%", "%eng-7%")
	probe("q=ENG-7", "ENG-7%", "%ENG-7%")
	// case: lowercase find against a capitalised title word
	probe("q=deploy (title, lowercase)", "deploy%", "%deploy%")
	// escaped metacharacters
	probe(`q=100\% (escaped)`, `100\%%`, `%100\%%`)
	probe(`q=a\_b (escaped)`, `a\_b%`, `%a\_b%`)

	for _, q := range []string{
		`EXPLAIN QUERY PLAN SELECT id FROM tracker_tasks WHERE key LIKE 'ENG-7%'`,
		`EXPLAIN QUERY PLAN SELECT id FROM tracker_tasks WHERE (key LIKE 'ENG-7%' ESCAPE '\' OR title LIKE '%eng%' ESCAPE '\')`,
		`EXPLAIN QUERY PLAN SELECT id FROM tracker_tasks WHERE key = 'ENG-7'`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Logf("PLAN ERR %s: %v", q, err)
			continue
		}
		for rows.Next() {
			cols, _ := rows.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Log(err)
				continue
			}
			t.Logf("PLAN %.60s => %v", q[25:], vals)
		}
		rows.Close()
	}
	var csl any
	if err := db.QueryRow(`PRAGMA case_sensitive_like`).Scan(&csl); err != nil {
		t.Logf("pragma case_sensitive_like: %v", err)
	} else {
		t.Logf("case_sensitive_like = %v", csl)
	}
}

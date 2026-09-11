package zzprobe

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUnescaped(t *testing.T) {
	db, _ := sql.Open("turso", filepath.Join(t.TempDir(), "p2.db"))
	defer db.Close()
	db.Exec(`CREATE TABLE t (title TEXT)`)
	for _, v := range []string{"a3b weirdness", "100% coverage", "totally unrelated"} {
		db.Exec(`INSERT INTO t VALUES (?)`, v)
	}
	for _, pat := range []string{"%a_b%", "%100%%", `%a\_b%`} {
		rows, err := db.Query(`SELECT title FROM t WHERE title LIKE ?`, pat)
		if err != nil {
			t.Log(pat, err)
			continue
		}
		var got []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			got = append(got, s)
		}
		rows.Close()
		t.Logf("unescaped %-10s -> %v", pat, got)
	}
}

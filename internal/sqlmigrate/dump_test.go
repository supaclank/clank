package sqlmigrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestNormalizeDDL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{
			name: "comment stripped",
			in:   "CREATE INDEX i ON t (a) -- trailing note\n WHERE a != ''",
			want: "CREATE INDEX i ON t (a) WHERE a != ''",
		},
		{
			name: "if not exists and quoting removed",
			in:   "CREATE TABLE IF NOT EXISTS `t` (\"a\" TEXT)",
			want: "CREATE TABLE t (a TEXT)",
		},
		{
			// Regression: a -- inside a quoted literal is content, not a
			// comment; truncating there could mask drift living after it.
			name: "dashes inside literal preserved",
			in:   "CREATE INDEX i ON t (a) WHERE a != 'x--y' -- real comment",
			want: "CREATE INDEX i ON t (a) WHERE a != 'x--y'",
		},
		{
			name: "escaped quote does not end the literal",
			in:   "CREATE INDEX i ON t (a) WHERE a != 'it''s--fine' -- note",
			want: "CREATE INDEX i ON t (a) WHERE a != 'it''s--fine'",
		},
		{
			name: "dashes inside quoted identifier preserved",
			in:   "CREATE INDEX \"i--x\" ON t (a)",
			want: "CREATE INDEX i--x ON t (a)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeDDL(c.in); got != c.want {
				t.Errorf("normalizeDDL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestDumpSchema_SeesDriftAfterLiteralDashes pins the end-to-end
// property the normalization exists for: two schemas whose only
// difference sits after a `--` inside a string literal must still dump
// differently.
func TestDumpSchema_SeesDriftAfterLiteralDashes(t *testing.T) {
	t.Parallel()
	dump := func(ddl string) string {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("exec %q: %v", ddl, err)
		}
		got, err := DumpSchema(db)
		if err != nil {
			t.Fatalf("DumpSchema: %v", err)
		}
		return got
	}

	a := dump(`CREATE TABLE t (a TEXT); CREATE INDEX i ON t (a) WHERE a != 'x--left';`)
	b := dump(`CREATE TABLE t (a TEXT); CREATE INDEX i ON t (a) WHERE a != 'x--right';`)
	if a == b {
		t.Errorf("schemas differing after a literal '--' dumped identically:\n%s", a)
	}
}

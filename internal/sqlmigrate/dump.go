package sqlmigrate

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DumpSchema returns a normalized, deterministic description of db's
// user-visible schema: one line per table column and per index. Built
// for the per-store schema-identity tests (goose-migrated database ≡
// schema.sql), which is what keeps sqlc's input honest against what
// actually runs.
//
// Tables are described structurally (pragma_table_info) because SQLite
// rewrites a table's stored CREATE text on every ALTER, making text
// comparison meaningless once real migrations exist. Index DDL is never
// rewritten, so indexes compare by normalized statement text — which
// also covers partial-index WHERE clauses that pragmas can't see.
// CHECK constraints are invisible to both views; the Atlas sync check
// in CI (`make migrations-check`) covers those semantically.
func DumpSchema(db *sql.DB) (string, error) {
	var lines []string

	tables, err := userObjects(db, "table")
	if err != nil {
		return "", err
	}
	for _, table := range tables {
		cols, err := db.Query(fmt.Sprintf(`SELECT name, type, "notnull", COALESCE(dflt_value, ''), pk FROM pragma_table_info('%s')`, table))
		if err != nil {
			return "", fmt.Errorf("table_info %s: %w", table, err)
		}
		for cols.Next() {
			var name, typ, dflt string
			var notNull, pk int
			if err := cols.Scan(&name, &typ, &notNull, &dflt, &pk); err != nil {
				cols.Close()
				return "", fmt.Errorf("scan table_info %s: %w", table, err)
			}
			lines = append(lines, fmt.Sprintf("table %s column %s type=%s notnull=%d default=%s pk=%d",
				table, name, strings.ToUpper(typ), notNull, dflt, pk))
		}
		if err := cols.Err(); err != nil {
			cols.Close()
			return "", fmt.Errorf("iterate table_info %s: %w", table, err)
		}
		cols.Close()
	}

	indexes, err := userObjects(db, "index")
	if err != nil {
		return "", err
	}
	for _, index := range indexes {
		var ddl sql.NullString
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&ddl); err != nil {
			return "", fmt.Errorf("index ddl %s: %w", index, err)
		}
		lines = append(lines, "index "+normalizeDDL(ddl.String))
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n"), nil
}

// userObjects lists names of the given sqlite_master type, excluding
// SQLite internals (sqlite_autoindex_* etc.) and goose's bookkeeping.
func userObjects(db *sql.DB, objType string) ([]string, error) {
	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = ?
		  AND name NOT LIKE 'sqlite_%'
		  AND name NOT LIKE 'goose_%'
		  AND tbl_name NOT LIKE 'goose_%'
		ORDER BY name`, objType)
	if err != nil {
		return nil, fmt.Errorf("list %ss: %w", objType, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s name: %w", objType, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// normalizeDDL strips the formatting-only differences between
// hand-written and stored/generated DDL: line comments, IF NOT EXISTS,
// identifier quoting, and whitespace runs.
func normalizeDDL(ddl string) string {
	s := stripLineComments(ddl)
	s = strings.ReplaceAll(s, "IF NOT EXISTS ", "")
	s = strings.NewReplacer("`", "", `"`, "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// stripLineComments removes SQL `--` line comments while respecting
// quoted regions ('literals', "identifiers", `identifiers`, with
// doubled-quote escapes), so a `--` inside a string — e.g. a partial-
// index predicate — never truncates the DDL being compared.
func stripLineComments(s string) string {
	var b strings.Builder
	var quote rune
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				if i+1 < len(runes) && runes[i+1] == quote {
					b.WriteRune(r)
					i++
				} else {
					quote = 0
				}
			}
			b.WriteRune(r)
		case r == '\'' || r == '"' || r == '`':
			quote = r
			b.WriteRune(r)
		case r == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			if i < len(runes) {
				b.WriteRune('\n')
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

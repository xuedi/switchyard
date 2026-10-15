// Package sqlcheck reads the SQL of the pending migrations and names every statement that would
// break the release still serving. During a deploy the old code keeps running on the migrated
// database (and a rollback goes back to such code later), so a migration may only add: new
// tables, nullable columns or ones with a default, indexes, foreign keys, backfills.
// A line "-- Version <name>" (Doctrine's --write-sql) or "-- Migration <name>" names the
// migration of the statements below it.
package sqlcheck

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	notAColumn = regexp.MustCompile(`^(INDEX|KEY|FOREIGN KEY|PRIMARY KEY|CONSTRAINT|CHECK|UNIQUE|FULLTEXT|SPATIAL)( |$)`)
	dropTable  = regexp.MustCompile(`^DROP (TABLE|VIEW|SCHEMA|DATABASE)( |$)`)
	truncate   = regexp.MustCompile(`^TRUNCATE( |$)`)
	renameTo   = regexp.MustCompile(`^RENAME (TO|AS)( |$)`)
	alterType  = regexp.MustCompile(`^ALTER (COLUMN )?[^ ]+ (TYPE|SET DATA TYPE|SET NOT NULL)( |$)`)
	space      = regexp.MustCompile(`\s+`)
)

// Check returns one "<migration>: <what>: <statement>" per destructive statement, and a summary.
func Check(sql string) (blocked []string, summary string) {
	migration, statement := "?", ""
	count, seen := 0, map[string]bool{}
	for _, line := range strings.Split(sql, "\n") {
		t := strings.TrimSpace(line)
		if f := strings.Fields(t); len(f) >= 3 && f[0] == "--" && (f[1] == "Version" || f[1] == "Migration") {
			migration = f[2]
			seen[migration] = true
			continue
		}
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		statement += " " + t
		if !strings.HasSuffix(t, ";") {
			continue
		}
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
		s = space.ReplaceAllString(s, " ")
		statement = ""
		count++
		short := s
		if len(short) > 160 {
			short = short[:157] + "..."
		}
		for _, p := range problems(s) {
			blocked = append(blocked, migration+": "+p+": "+short)
		}
	}
	switch {
	case count == 0:
		summary = "no pending migrations"
	case len(blocked) > 0:
		summary = fmt.Sprintf("pending: %d statements in %d migrations, %d destructive", count, len(seen), len(blocked))
	default:
		summary = fmt.Sprintf("pending: %d statements in %d migrations, none destructive", count, len(seen))
	}
	return blocked, summary
}

func first(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func bare(s string) string { return strings.ToLower(strings.NewReplacer("`", "", `"`, "").Replace(s)) }

// has: word stands on its own in U.
func has(U, word string) bool {
	return regexp.MustCompile(`[^A-Z0-9_]` + regexp.QuoteMeta(word) + `[^A-Z0-9_]`).MatchString(" " + U + " ")
}

// skip drops the leading words from s (case-insensitive) when they are there.
func skip(s, words string) string {
	if strings.HasPrefix(strings.ToUpper(s), words+" ") {
		return s[len(words)+1:]
	}
	return s
}

func after(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

// clauses splits an ALTER TABLE's clauses at top-level commas: "DROP a, ADD b DECIMAL(10, 2)".
func clauses(sql string) []string {
	var out []string
	depth, quote, cur := 0, byte(0), []byte{}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(string(cur)))
			cur = cur[:0]
			continue
		}
		cur = append(cur, c)
	}
	return append(out, strings.TrimSpace(string(cur)))
}

func problems(sql string) []string {
	U := strings.ToUpper(sql)
	switch {
	case dropTable.MatchString(U):
		return []string{"drops a table"}
	case truncate.MatchString(U):
		return []string{"empties a table"}
	case strings.HasPrefix(U, "RENAME TABLE "):
		return []string{"renames a table"}
	case strings.HasPrefix(U, "DELETE FROM "):
		return []string{"deletes rows"}
	case !strings.HasPrefix(U, "ALTER TABLE "):
		return nil
	}
	rest := skip(skip(after(sql, 12), "IF EXISTS"), "ONLY")
	i := strings.IndexByte(rest, ' ')
	if i < 0 {
		return nil
	}
	var found []string
	for _, c := range clauses(rest[i+1:]) {
		C := strings.ToUpper(c)
		switch {
		case strings.HasPrefix(C, "DROP ") && !notAColumn.MatchString(after(C, 5)):
			found = append(found, "drops column "+first(skip(skip(after(c, 5), "COLUMN"), "IF EXISTS")))
		case renameTo.MatchString(C):
			found = append(found, "renames the table")
		case strings.HasPrefix(C, "RENAME COLUMN "):
			found = append(found, "renames column "+first(after(c, 14)))
		case strings.HasPrefix(C, "CHANGE "):
			a := strings.Fields(skip(after(c, 7), "COLUMN"))
			if len(a) >= 2 && bare(a[0]) != bare(a[1]) {
				found = append(found, "renames column "+a[0]+" to "+a[1])
			} else if len(a) > 0 {
				found = append(found, "changes column "+a[0]+" (may change its type)")
			}
		case strings.HasPrefix(C, "MODIFY "):
			found = append(found, "changes column "+first(skip(after(c, 7), "COLUMN"))+" (may change its type)")
		case alterType.MatchString(C):
			found = append(found, "changes column "+first(skip(after(c, 6), "COLUMN")))
		case strings.HasPrefix(C, "ADD ") && !notAColumn.MatchString(after(C, 4)) && has(C, "NOT NULL") && !has(C, "DEFAULT") &&
			!has(C, "AUTO_INCREMENT") && !has(C, "GENERATED") && !has(C, "SERIAL") && !has(C, "BIGSERIAL") && !strings.Contains(C, " AS ("):
			found = append(found, "adds column "+first(skip(skip(after(c, 4), "COLUMN"), "IF NOT EXISTS"))+" NOT NULL without a default (the old code's inserts fail)")
		}
	}
	return found
}

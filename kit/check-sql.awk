# check-sql.awk: reads the SQL of the pending migrations and names every statement that would
# break the release still serving. During a deploy the old code keeps running on the migrated
# database (and a rollback goes back to such code later), so a migration may only add: new
# tables, nullable columns or ones with a default, indexes, foreign keys, backfills.
# Prints one "BLOCKED <migration>: <what>: <statement>" line per hit and exits 1, or a summary
# and 0. Run by switchyard.sh in the build Job, on what the app's pending-sql hook printed.
#   awk -f check-sql.awk pending.sql
# A line "-- Version <name>" (Doctrine's --write-sql) or "-- Migration <name>" names the
# migration of the statements below it. POSIX awk: mawk, busybox and gawk.

function trim(s) {
    sub(/^[ \t\r\n]+/, "", s)
    sub(/[ \t\r\n]+$/, "", s)
    return s
}

# clauses splits an ALTER TABLE's clauses at top-level commas: "DROP a, ADD b DECIMAL(10, 2)"
function clauses(sql, out,    n, i, c, depth, quote, cur) {
    n = 0; depth = 0; quote = ""; cur = ""
    for (i = 1; i <= length(sql); i++) {
        c = substr(sql, i, 1)
        if (quote != "") {
            if (c == quote) quote = ""
        } else if (c == "'" || c == "\"" || c == "`") {
            quote = c
        } else if (c == "(") {
            depth++
        } else if (c == ")") {
            depth--
        } else if (c == "," && depth == 0) {
            out[++n] = trim(cur); cur = ""
            continue
        }
        cur = cur c
    }
    out[++n] = trim(cur)
    return n
}

function first(s,    a) { split(s, a, " "); return a[1] }
function bare(s) { gsub(/[`"]/, "", s); return tolower(s) }
function has(U, word) { return (" " U " ") ~ ("[^A-Z0-9_]" word "[^A-Z0-9_]") }
function after(s, n) { return substr(s, n + 1) }

# skip drops the given leading words from s (case-insensitive), when they are there
function skip(s, words,    U) {
    U = toupper(s)
    if (index(U, words " ") == 1) return substr(s, length(words) + 2)
    return s
}

# problems fills found[1..nf] with what the statement breaks
function problems(sql,    U, rest, cl, n, i, c, C, w, a) {
    nf = 0
    U = toupper(sql)
    if (U ~ /^DROP (TABLE|VIEW|SCHEMA|DATABASE)( |$)/) { found[++nf] = "drops a table"; return }
    if (U ~ /^TRUNCATE( |$)/) { found[++nf] = "empties a table"; return }
    if (U ~ /^RENAME TABLE /) { found[++nf] = "renames a table"; return }
    if (U ~ /^DELETE FROM /) { found[++nf] = "deletes rows"; return }
    if (U !~ /^ALTER TABLE /) return
    rest = skip(skip(after(sql, 12), "IF EXISTS"), "ONLY")
    i = index(rest, " ")
    if (!i) return
    n = clauses(after(rest, i), cl)
    for (i = 1; i <= n; i++) {
        c = cl[i]; C = toupper(c)
        if (C ~ /^DROP / && after(C, 5) !~ NOT_A_COLUMN) {
            found[++nf] = "drops column " first(skip(skip(after(c, 5), "COLUMN"), "IF EXISTS"))
        } else if (C ~ /^RENAME (TO|AS)( |$)/) {
            found[++nf] = "renames the table"
        } else if (C ~ /^RENAME COLUMN /) {
            found[++nf] = "renames column " first(after(c, 14))
        } else if (C ~ /^CHANGE /) {
            split(skip(after(c, 7), "COLUMN"), a, " ")
            found[++nf] = bare(a[1]) != bare(a[2]) ? "renames column " a[1] " to " a[2] : "changes column " a[1]
        } else if (C ~ /^MODIFY /) {
            found[++nf] = "changes column " first(skip(after(c, 7), "COLUMN"))
        } else if (C ~ /^ALTER (COLUMN )?[^ ]+ (TYPE|SET DATA TYPE|SET NOT NULL)( |$)/) {
            found[++nf] = "changes column " first(skip(after(c, 6), "COLUMN"))
        } else if (C ~ /^ADD / && after(C, 4) !~ NOT_A_COLUMN && has(C, "NOT NULL") && !has(C, "DEFAULT") \
            && !has(C, "AUTO_INCREMENT") && !has(C, "GENERATED") && !has(C, "SERIAL") && !has(C, "BIGSERIAL") && C !~ / AS \(/) {
            found[++nf] = "adds column " first(skip(skip(after(c, 4), "COLUMN"), "IF NOT EXISTS")) " NOT NULL without a default (the old code's inserts fail)"
        }
    }
}

BEGIN {
    NOT_A_COLUMN = "^(INDEX|KEY|FOREIGN KEY|PRIMARY KEY|CONSTRAINT|CHECK|UNIQUE|FULLTEXT|SPATIAL)( |$)"
    migration = "?"; statement = ""; count = 0; blocked = 0; migrations = 0
}

/^-- (Version|Migration) / {
    migration = $3
    if (!(migration in seen)) { seen[migration] = 1; migrations++ }
    next
}

{ t = trim($0) }
t == "" || substr(t, 1, 2) == "--" { next }

{
    statement = statement " " t
    if (t !~ /;$/) next
    sql = trim(statement); statement = ""
    sub(/;$/, "", sql)
    sql = trim(sql)
    gsub(/[ \t\r\n]+/, " ", sql)
    count++
    problems(sql)
    for (i = 1; i <= nf; i++) {
        blocked++
        print "BLOCKED " migration ": " found[i] ": " (length(sql) > 160 ? substr(sql, 1, 157) "..." : sql)
    }
}

END {
    if (count == 0) print "no pending migrations"
    else printf "pending: %d statements in %d migrations, %s\n", count, migrations, blocked ? blocked " destructive" : "none destructive"
    exit (blocked ? 1 : 0)
}

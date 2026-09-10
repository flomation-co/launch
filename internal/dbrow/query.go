package dbrow

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Supported dialects. These match the executor node's `dialect` dropdown values
// (actions/trigger/database_row).
const (
	dialectPostgres  = "postgresql"
	dialectMySQL     = "mysql"
	dialectSQLServer = "sqlserver"
)

// identifierRe matches a single safe SQL identifier: a letter or underscore
// followed by letters, digits or underscores. Table and column names come from
// the flow author, but we still refuse anything that isn't a bare identifier so
// a stray value can never be interpolated as SQL — identifiers cannot be bound
// as query parameters, so validation + quoting is the only defence.
var identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validIdentifier reports whether s is a single safe SQL identifier.
func validIdentifier(s string) bool {
	return identifierRe.MatchString(s)
}

// validTableName reports whether s is a safe (optionally schema-qualified) table
// name, e.g. "orders" or "public.orders". Each dotted part must be a valid
// identifier.
func validTableName(s string) bool {
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !validIdentifier(p) {
			return false
		}
	}
	return true
}

// quoteIdentifier wraps a validated identifier in the dialect's delimiter,
// quoting each dotted part of a schema-qualified name separately. Callers MUST
// have validated the identifier first (validIdentifier / validTableName); this
// only applies delimiters, it is not a sanitiser.
func quoteIdentifier(dialect, ident string) string {
	var open, close string
	switch dialect {
	case dialectMySQL:
		open, close = "`", "`"
	case dialectSQLServer:
		open, close = "[", "]"
	default: // postgres
		open, close = `"`, `"`
	}
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		parts[i] = open + p + close
	}
	return strings.Join(parts, ".")
}

// placeholder returns the positional bind placeholder for parameter n (1-based)
// in the given dialect: $1 (postgres), ? (mysql), @p1 (sqlserver).
func placeholder(dialect string, n int) string {
	switch dialect {
	case dialectMySQL:
		return "?"
	case dialectSQLServer:
		return "@p" + strconv.Itoa(n)
	default:
		return "$" + strconv.Itoa(n)
	}
}

// driverName maps a dialect to its registered database/sql driver name.
func driverName(dialect string) (string, error) {
	switch dialect {
	case dialectPostgres:
		return "postgres", nil
	case dialectMySQL:
		return "mysql", nil
	case dialectSQLServer:
		return "sqlserver", nil
	default:
		return "", fmt.Errorf("unsupported dialect %q", dialect)
	}
}

// buildDSN assembles the driver name and connection string for a dialect from
// already variable-resolved connection fields. sslMode is the node's generic
// disable/require/verify choice, mapped to each driver's own encryption knob.
//
// An unset mode means "require". It used to mean "disable", which is the wrong
// way round twice over: it put the database password on the wire in clear
// whenever the author did not notice the field, and it fails outright against
// every managed Postgres, because they all refuse an unencrypted connection.
// The failure was invisible too — the trigger stayed registered and leased,
// polling once a minute forever, and the only trace was a pg_hba line in the
// Launch log. Turning encryption off is still available, but has to be chosen.
func buildDSN(dialect, host, port, user, pass, database, sslMode string) (string, string, error) {
	drv, err := driverName(dialect)
	if err != nil {
		return "", "", err
	}
	if host == "" || port == "" || database == "" {
		return "", "", fmt.Errorf("host, port and database are required")
	}

	switch dialect {
	case dialectPostgres:
		mode := "require"
		switch sslMode {
		case "disable":
			mode = "disable"
		case "verify":
			mode = "verify-full"
		}
		dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
			url.QueryEscape(user), url.QueryEscape(pass), host, port, url.QueryEscape(database), mode)
		return drv, dsn, nil

	case dialectMySQL:
		tls := "skip-verify"
		switch sslMode {
		case "disable":
			tls = "false"
		case "verify":
			tls = "true"
		}
		// parseTime=true makes DATE/DATETIME/TIMESTAMP scan into time.Time so the
		// cursor and row values normalise consistently.
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?tls=%s&parseTime=true",
			user, pass, host, port, database, tls)
		return drv, dsn, nil

	case dialectSQLServer:
		q := url.Values{}
		q.Set("database", database)
		switch sslMode {
		case "disable":
			q.Set("encrypt", "disable")
		case "verify":
			q.Set("encrypt", "true")
		default:
			q.Set("encrypt", "true")
			q.Set("trustservercertificate", "true")
		}
		dsn := fmt.Sprintf("sqlserver://%s:%s@%s:%s?%s",
			url.QueryEscape(user), url.QueryEscape(pass), host, port, q.Encode())
		return drv, dsn, nil
	}
	return "", "", fmt.Errorf("unsupported dialect %q", dialect)
}

// buildMaxQuery returns "SELECT MAX(cursor) FROM table [WHERE filter = ?]",
// used on the first poll to record the baseline watermark. Identifiers must be
// pre-validated. When filterCol is non-empty the filter value binds to
// placeholder #1.
func buildMaxQuery(dialect, table, cursorCol, filterCol string) string {
	q := fmt.Sprintf("SELECT MAX(%s) FROM %s",
		quoteIdentifier(dialect, cursorCol), quoteIdentifier(dialect, table))
	if filterCol != "" {
		q += fmt.Sprintf(" WHERE %s = %s", quoteIdentifier(dialect, filterCol), placeholder(dialect, 1))
	}
	return q
}

// buildSelectQuery returns the query that fetches new rows in cursor order,
// capped at limit. Placeholders are numbered in the order the caller must supply
// args: the watermark first (when useCursor), then the filter value (when
// filterCol is set). SQL Server has no LIMIT, so TOP is used instead.
func buildSelectQuery(dialect, table, cursorCol, filterCol string, useCursor bool, limit int) string {
	qCursor := quoteIdentifier(dialect, cursorCol)
	qTable := quoteIdentifier(dialect, table)

	var where []string
	n := 0
	if useCursor {
		n++
		where = append(where, fmt.Sprintf("%s > %s", qCursor, placeholder(dialect, n)))
	}
	if filterCol != "" {
		n++
		where = append(where, fmt.Sprintf("%s = %s", quoteIdentifier(dialect, filterCol), placeholder(dialect, n)))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	if dialect == dialectSQLServer {
		return fmt.Sprintf("SELECT TOP (%d) * FROM %s%s ORDER BY %s ASC", limit, qTable, whereClause, qCursor)
	}
	return fmt.Sprintf("SELECT * FROM %s%s ORDER BY %s ASC LIMIT %d", qTable, whereClause, qCursor, limit)
}

// normaliseValue converts a raw database/sql scan value into something that
// JSON-marshals cleanly for the trigger payload: []byte → string, time.Time →
// RFC3339. Everything else (int64, float64, bool, nil) passes through.
func normaliseValue(v interface{}) interface{} {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// cursorToString renders a normalised cursor value as the string we persist as
// the watermark and rebind on the next poll.
func cursorToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

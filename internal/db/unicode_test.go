package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUnicodeLowerAvailableOnPooledAndReplacementConnections(t *testing.T) {
	ctx := context.Background()
	pool, err := Open(ctx, filepath.Join(t.TempDir(), "unicode.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(0)
	check := func(conn *sql.Conn) {
		t.Helper()
		var folded, builtin string
		var empty sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT dailydocs_lower(?), lower(?), dailydocs_lower(NULL)", "École Ω SDK 日本語", "École Ω SDK 日本語").Scan(&folded, &builtin, &empty); err != nil {
			t.Fatal(err)
		}
		if folded != "école ω sdk 日本語" || builtin != "École Ω sdk 日本語" || empty.Valid {
			t.Fatal("Unicode helper changed text or the SQLite built-in", folded, builtin, empty)
		}
	}
	// Holding the first connection forces database/sql to open an independent
	// second connection instead of reusing the one configured by Open.
	first, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	check(first)
	check(second)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// No idle connections are retained, so this is a newly opened connection.
	replacement, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	check(replacement)
}

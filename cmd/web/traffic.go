package main

import (
	"database/sql"

	"github.com/ernestns/daily-docs/internal/traffic"
)

func newTrafficCollector(conn *sql.DB) *traffic.Collector {
	// Keep app read-then-write transactions and analytics commits on one SQLite
	// connection: a concurrent WAL commit would otherwise invalidate a reader
	// snapshot when it later assigns a daily reading. Provider calls hold no tx.
	conn.SetMaxOpenConns(1)
	return traffic.New(conn)
}

package main

import (
	"database/sql"

	"github.com/ernestns/daily-docs/internal/traffic"
)

func newTrafficCollector(conn *sql.DB) *traffic.Collector {
	// db.Open uses immediate writer transactions to protect read-then-write
	// snapshots. Keep pooled connections available so a writer waiting on SQLite
	// does not also block saved-reading and catalog reads.
	return traffic.New(conn)
}

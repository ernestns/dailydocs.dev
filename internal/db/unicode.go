package db

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

func init() {
	// SQLite's built-in lower only handles ASCII. Register once before opening
	// any connections so name matching uses the same Unicode rules as topic
	// input and slug generation, including on replacement pooled connections.
	// Keep the built-in function and stored names/URLs unchanged.
	sqlite.MustRegisterDeterministicScalarFunction("dailydocs_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0] == nil {
			return nil, nil
		}
		value, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("dailydocs_lower requires text")
		}
		return strings.ToLower(value), nil
	})
}

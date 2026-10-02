package store

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

// sqliteUnique is SQLITE_CONSTRAINT_UNIQUE (primary 19, extended 2067).
const sqliteUnique = 2067

func isUniqueConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == sqliteUnique
	}
	return false
}

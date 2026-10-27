package migrations

import (
	"os"
	"strings"
	"testing"
)

// A migration-time baseline must not race a writer before the trigger exists.
func TestP5SystemTimeBaselineHoldsWriteConflictingLockUntilTrigger(t *testing.T) {
	raw, err := os.ReadFile("000082_memory_system_events.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToUpper(string(raw))
	lock := strings.Index(sql, "LOCK TABLE MEMORY_ITEMS IN SHARE ROW EXCLUSIVE MODE")
	baseline := strings.Index(sql, "INSERT INTO MEMORY_SYSTEM_EVENTS (")
	trigger := strings.Index(sql, "CREATE TRIGGER MEMORY_ITEMS_SYSTEM_EVENT")
	commit := strings.LastIndex(sql, "COMMIT;")
	if lock < 0 || baseline <= lock || trigger <= baseline || commit <= trigger {
		t.Fatal("baseline/trigger must share a write-blocking migration transaction")
	}
}

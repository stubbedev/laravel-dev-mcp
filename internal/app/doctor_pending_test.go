package app

import "testing"

func TestCountPendingMigrations(t *testing.T) {
	modern := `  Migration name ........................ Batch / Status
  0001_01_01_000000_create_users_table .. [1] Ran
  2024_05_01_000000_create_foo_table .... Pending
  2024_05_02_000000_create_bar_table .... Pending`
	if got := countPendingMigrations(modern); got != 2 {
		t.Errorf("modern: got %d, want 2", got)
	}

	legacy := `+------+-----------------+-------+
| Ran? | Migration       | Batch |
+------+-----------------+-------+
| Yes  | create_users    | 1     |
| No   | create_foo      |       |
+------+-----------------+-------+`
	if got := countPendingMigrations(legacy); got != 1 {
		t.Errorf("legacy: got %d, want 1", got)
	}

	if got := countPendingMigrations("nothing pending here"); got != 0 {
		t.Errorf("none: got %d, want 0", got)
	}
}

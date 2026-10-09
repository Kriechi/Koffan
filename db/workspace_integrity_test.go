package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDeleteUserPreservesLastAdministrator(t *testing.T) {
	initOfflineDatabase(t)
	admin, err := CreateUser("admin", "Admin", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateUserSession("admin-session", 9999999999, admin.ID, DefaultWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUser(admin.ID); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deleting the last administrator: %v", err)
	}
	if _, err := GetUserByID(admin.ID); err != nil {
		t.Errorf("administrator was removed: %v", err)
	}
	if _, err := GetSession("admin-session"); err != nil {
		t.Errorf("refused deletion removed the session: %v", err)
	}
}

func TestBootstrapAdminRollsBackMembershipFailure(t *testing.T) {
	initOfflineDatabase(t)
	if _, err := DB.Exec(`CREATE TRIGGER fail_membership BEFORE INSERT ON workspace_members
		BEGIN SELECT RAISE(FAIL, 'membership failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBootstrapAdmin("admin", "correct horse battery"); err == nil || !strings.Contains(err.Error(), "membership failure") {
		t.Fatalf("expected membership failure, got %v", err)
	}
	if n := CountUsers(); n != 0 {
		t.Errorf("failed bootstrap left %d users", n)
	}
	if _, err := DB.Exec(`DROP TRIGGER fail_membership`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBootstrapAdmin("admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	admin, err := Authenticate("admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !admin.IsAdmin || CountUsers() != 1 || MemberRole(DefaultWorkspaceID, admin.ID) != RoleOwner {
		t.Fatalf("bootstrap did not create Home's administrator and owner: %#v", admin)
	}
}

func TestHistoryMigrationPreservesSequence(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		deleteSQL string
		wantID    int64
	}{
		{"deleted highest ID", `DELETE FROM item_history WHERE id = 6`, 7},
		{"empty table", `DELETE FROM item_history`, 7},
		{"missing sequence", `DELETE FROM sqlite_sequence WHERE name = 'item_history'`, 7},
		{"empty missing sequence", `DELETE FROM item_history; DELETE FROM sqlite_sequence WHERE name = 'item_history'`, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			initOfflineDatabase(t)
			if _, err := DB.Exec(`DROP TABLE item_history;
				CREATE TABLE item_history (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					name TEXT NOT NULL UNIQUE COLLATE NOCASE,
					last_section_id INTEGER,
					usage_count INTEGER DEFAULT 1,
					last_used_at INTEGER DEFAULT (strftime('%s', 'now'))
				)`); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 6; i++ {
				if _, err := DB.Exec(`INSERT INTO item_history (name) VALUES (?)`, fmt.Sprintf("legacy-%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DB.Exec(scenario.deleteSQL); err != nil {
				t.Fatal(err)
			}
			migrateHistoryWorkspace()
			if err := SaveItemHistory(DefaultWorkspaceID, "new purchase", 0); err != nil {
				t.Fatal(err)
			}
			var gotID int64
			if err := DB.QueryRow(`SELECT id FROM item_history WHERE name = 'new purchase'`).Scan(&gotID); err != nil {
				t.Fatal(err)
			}
			if gotID != scenario.wantID {
				t.Fatalf("new history ID = %d, want %d", gotID, scenario.wantID)
			}
		})
	}
}

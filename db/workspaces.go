package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// DefaultWorkspaceID is the workspace every pre-existing list belongs to.
// Without MULTI_USER the application only ever uses this one.
const DefaultWorkspaceID int64 = 1

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserExists         = errors.New("user already exists")
	ErrLastOwner          = errors.New("a workspace needs at least one owner")
	ErrLastAdmin          = errors.New("the last administrator cannot be deleted")
	ErrSoleOwner          = errors.New("the account is the only owner of a workspace that has other members")
	ErrPasswordTooLong    = errors.New("password is longer than bcrypt can use")
)

// MaxPasswordBytes is what bcrypt looks at: it ignores everything after the
// 72nd byte, so longer passwords are refused instead of silently truncated.
const MaxPasswordBytes = 72

// User is an account that can sign in when MULTI_USER is enabled.
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

// Workspace groups lists and the people who share them (a household, a
// parent's shopping, ...).
type Workspace struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Role        string `json:"role,omitempty"` // the caller's role, when listed for a user
	MemberCount int    `json:"member_count"`
}

// DefaultWorkspaceIcon is shown for workspaces that were never given an icon.
const DefaultWorkspaceIcon = "🏠"

// memberCountSQL counts the people in the workspace aliased as w.
const memberCountSQL = `(SELECT COUNT(*) FROM workspace_members c WHERE c.workspace_id = w.id)`

// Member is a user together with their role in one workspace.
type Member struct {
	User
	Role string `json:"role"`
}

// migrateWorkspaces adds users, workspaces and the columns that attach
// existing data to the default workspace. It is idempotent. Apart from
// rebuilding item_history once (see migrateHistoryWorkspace) it only adds
// structure, so a database created by an older release keeps working here.
// The reverse does not hold for item history: an older release can no longer
// save history on a migrated database.
func migrateWorkspaces() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			is_admin BOOLEAN NOT NULL DEFAULT FALSE,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
		)`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
		)`,
		`CREATE TABLE IF NOT EXISTS workspace_members (
			workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role TEXT NOT NULL DEFAULT 'member',
			PRIMARY KEY (workspace_id, user_id)
		)`,
		`INSERT OR IGNORE INTO workspaces (id, name) VALUES (1, 'Home')`,
	}
	for _, stmt := range stmts {
		if _, err := DB.Exec(stmt); err != nil {
			log.Fatal("Workspace migration failed:", err)
		}
	}

	addColumn("workspaces", "icon", "TEXT NOT NULL DEFAULT '"+DefaultWorkspaceIcon+"'")
	addColumn("sessions", "user_id", "INTEGER")
	addColumn("sessions", "workspace_id", "INTEGER")
	addColumn("lists", "workspace_id", "INTEGER NOT NULL DEFAULT 1")
	addColumn("templates", "workspace_id", "INTEGER NOT NULL DEFAULT 1")
	DB.Exec(`CREATE INDEX IF NOT EXISTS idx_lists_workspace ON lists(workspace_id, sort_order)`)
	DB.Exec(`CREATE INDEX IF NOT EXISTS idx_templates_workspace ON templates(workspace_id, sort_order)`)

	migrateHistoryWorkspace()
}

func addColumn(table, column, definition string) {
	var count int
	q := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", table)
	if err := DB.QueryRow(q, column).Scan(&count); err != nil || count > 0 {
		return
	}
	if _, err := DB.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		log.Fatalf("Workspace migration failed (%s.%s): %v", table, column, err)
	}
}

// migrateHistoryWorkspace rebuilds item_history so that autocomplete history is
// unique per workspace instead of globally (SQLite cannot alter a UNIQUE
// constraint in place).
func migrateHistoryWorkspace() {
	var count int
	if err := DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info('item_history') WHERE name = 'workspace_id'").Scan(&count); err != nil || count > 0 {
		return
	}
	log.Println("Running migration: scoping item_history to workspaces...")
	tx, err := DB.Begin()
	if err != nil {
		log.Fatal("History migration failed:", err)
	}
	defer tx.Rollback()
	var oldSequence int64
	if err := tx.QueryRow(`SELECT COALESCE(seq, 0) FROM sqlite_sequence WHERE name = 'item_history'`).Scan(&oldSequence); err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatal("History migration failed:", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE item_history_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id INTEGER NOT NULL DEFAULT 1,
			name TEXT NOT NULL COLLATE NOCASE,
			last_section_id INTEGER,
			usage_count INTEGER DEFAULT 1,
			last_used_at INTEGER DEFAULT (strftime('%s', 'now')),
			UNIQUE(workspace_id, name COLLATE NOCASE)
		)`,
		`INSERT INTO item_history_new (id, workspace_id, name, last_section_id, usage_count, last_used_at)
			SELECT h.id, COALESCE((SELECT l.workspace_id FROM sections s JOIN lists l ON l.id = s.list_id WHERE s.id = h.last_section_id), 1),
			       h.name, h.last_section_id, h.usage_count, h.last_used_at
			FROM item_history h`,
		`DROP TABLE item_history`,
		`ALTER TABLE item_history_new RENAME TO item_history`,
		`CREATE INDEX IF NOT EXISTS idx_item_history_name ON item_history(name COLLATE NOCASE)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			log.Fatal("History migration failed:", err)
		}
	}
	var maxID int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM item_history`).Scan(&maxID); err != nil {
		log.Fatal("History migration failed:", err)
	}
	sequence := max(oldSequence, maxID)
	res, err := tx.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'item_history'`, sequence)
	if err != nil {
		log.Fatal("History migration failed:", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		log.Fatal("History migration failed:", err)
	}
	if n == 0 {
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('item_history', ?)`, sequence); err != nil {
			log.Fatal("History migration failed:", err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatal("History migration failed:", err)
	}
	log.Println("Migration completed: item_history is now scoped to workspaces")
}

// ==================== USERS ====================

// CreateUser stores a new account with a bcrypt-hashed password.
func CreateUser(username, displayName, password string, isAdmin bool) (*User, error) {
	tx, err := DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	user, err := createUser(tx, username, displayName, password, isAdmin)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

func createUser(tx *sql.Tx, username, displayName, password string, isAdmin bool) (*User, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = username
	}
	if len(password) > MaxPasswordBytes {
		return nil, ErrPasswordTooLong
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO users (username, display_name, password_hash, is_admin) VALUES (?, ?, ?, ?)`,
		username, displayName, string(hash), isAdmin)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, DisplayName: displayName, IsAdmin: isAdmin}, nil
}

// Authenticate checks a username and password.
func Authenticate(username, password string) (*User, error) {
	const dummyHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoOa5Yq5Z8Zr1Q9v3m0j0b5b1x3Q9w1s5a"
	// bcrypt only compares the first 72 bytes, so a longer input would match a
	// stored password it merely starts with.
	if len(password) > MaxPasswordBytes {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password[:MaxPasswordBytes]))
		return nil, ErrInvalidCredentials
	}
	var u User
	var hash string
	err := DB.QueryRow(`SELECT id, username, display_name, is_admin, password_hash FROM users WHERE username = ?`,
		strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &hash)
	if err == sql.ErrNoRows {
		// Spend the same time as a real check so usernames cannot be probed.
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	return &u, nil
}

// CheckPassword reports whether password is the account's current password.
func CheckPassword(userID int64, password string) bool {
	var hash string
	if err := DB.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return false
	}
	return len(password) <= MaxPasswordBytes && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// SetPassword gives an account a new password and signs it out everywhere
// except keepSession (pass "" to sign it out everywhere).
func SetPassword(userID int64, password, keepSession string) error {
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

func GetUserByID(id int64) (*User, error) {
	var u User
	err := DB.QueryRow(`SELECT id, username, display_name, is_admin FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByUsername(username string) (*User, error) {
	var u User
	err := DB.QueryRow(`SELECT id, username, display_name, is_admin FROM users WHERE username = ?`, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func ListUsers() ([]User, error) {
	rows, err := DB.Query(`SELECT id, username, display_name, is_admin FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func CountUsers() int {
	var n int
	DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// DeleteUser removes an account, its sessions and its memberships.
func DeleteUser(id int64) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Deleting the only owner of a workspace that still has people in it would
	// leave them without anyone who can manage it.
	var orphaned int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM workspace_members m
		WHERE m.user_id = ? AND m.role = 'owner'
		  AND NOT EXISTS (SELECT 1 FROM workspace_members o
		                  WHERE o.workspace_id = m.workspace_id AND o.role = 'owner' AND o.user_id <> m.user_id)
		  AND EXISTS (SELECT 1 FROM workspace_members x
		              WHERE x.workspace_id = m.workspace_id AND x.user_id <> m.user_id)`, id).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned > 0 {
		return ErrSoleOwner
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ? AND
		(is_admin = FALSE OR EXISTS (SELECT 1 FROM users o WHERE o.is_admin = TRUE AND o.id <> ?))`, id, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM users WHERE id = ?)`, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureBootstrapAdmin creates the first administrator on a fresh multi-user
// install so the operator can sign in. Existing lists stay in the default
// workspace, which the administrator owns.
func EnsureBootstrapAdmin(username, password string) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	user, err := createUser(tx, username, username, password, true)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, ?)`,
		DefaultWorkspaceID, user.ID, RoleOwner); err != nil {
		return err
	}
	return tx.Commit()
}

// ==================== WORKSPACES ====================

// CreateWorkspace creates a workspace with ownerID as its first owner.
func CreateWorkspace(name, icon string, ownerID int64) (*Workspace, error) {
	if icon == "" {
		icon = DefaultWorkspaceIcon
	}
	tx, err := DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO workspaces (name, icon) VALUES (?, ?)`, strings.TrimSpace(name), icon)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, ?)`, id, ownerID, RoleOwner); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Workspace{ID: id, Name: strings.TrimSpace(name), Icon: icon, Role: RoleOwner}, nil
}

// UpdateWorkspace renames a workspace and changes its icon.
func UpdateWorkspace(id int64, name, icon string) error {
	if icon == "" {
		icon = DefaultWorkspaceIcon
	}
	_, err := DB.Exec(`UPDATE workspaces SET name = ?, icon = ? WHERE id = ?`, strings.TrimSpace(name), icon, id)
	return err
}

func GetWorkspace(id int64) (*Workspace, error) {
	var w Workspace
	err := DB.QueryRow(`SELECT id, name, icon FROM workspaces WHERE id = ?`, id).Scan(&w.ID, &w.Name, &w.Icon)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// WorkspacesForUser returns the workspaces a user belongs to, with their role.
func WorkspacesForUser(userID int64) ([]Workspace, error) {
	rows, err := DB.Query(`
		SELECT w.id, w.name, w.icon, m.role, `+memberCountSQL+` FROM workspaces w
		JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = ? ORDER BY w.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Icon, &w.Role, &w.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AllWorkspaces is used by administrators.
func AllWorkspaces() ([]Workspace, error) {
	rows, err := DB.Query(`SELECT w.id, w.name, w.icon, ` + memberCountSQL + ` FROM workspaces w ORDER BY w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Icon, &w.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MemberRole returns the user's role in a workspace, or "" if not a member.
func MemberRole(workspaceID, userID int64) string {
	var role string
	DB.QueryRow(`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID).Scan(&role)
	return role
}

func WorkspaceMembers(workspaceID int64) ([]Member, error) {
	rows, err := DB.Query(`
		SELECT u.id, u.username, u.display_name, u.is_admin, m.role
		FROM workspace_members m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? ORDER BY m.role DESC, u.display_name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Username, &m.DisplayName, &m.IsAdmin, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember puts a user in a workspace. Adding someone who is already in it can
// promote them to owner but never demotes an owner, so re-adding a person (or
// yourself) cannot leave a workspace without an owner.
func AddMember(workspaceID, userID int64, role string) error {
	if role != RoleOwner {
		role = RoleMember
	}
	_, err := DB.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, ?)
		ON CONFLICT(workspace_id, user_id) DO UPDATE SET role = CASE WHEN excluded.role = 'owner' THEN 'owner' ELSE role END`,
		workspaceID, userID, role)
	return err
}

// RemoveMember drops a user from a workspace but refuses to leave it ownerless.
func RemoveMember(workspaceID, userID int64) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	if err := tx.QueryRow(`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID).Scan(&role); err != nil {
		return err
	}
	if role == RoleOwner {
		var owners int
		tx.QueryRow(`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND role = 'owner'`, workspaceID).Scan(&owners)
		if owners <= 1 {
			return ErrLastOwner
		}
	}
	if _, err := tx.Exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID); err != nil {
		return err
	}
	// A session that was viewing this workspace falls back to another one.
	if _, err := tx.Exec(`UPDATE sessions SET workspace_id = NULL WHERE user_id = ? AND workspace_id = ?`, userID, workspaceID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteWorkspace removes a workspace with all its lists, templates and history.
func DeleteWorkspace(id int64) error {
	if id == DefaultWorkspaceID {
		return errors.New("the default workspace cannot be deleted")
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM lists WHERE workspace_id = ?`,
		`DELETE FROM templates WHERE workspace_id = ?`,
		`DELETE FROM item_history WHERE workspace_id = ?`,
		`UPDATE sessions SET workspace_id = NULL WHERE workspace_id = ?`,
		`DELETE FROM workspaces WHERE id = ?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ==================== OWNERSHIP LOOKUPS ====================
// These map a resource ID to the workspace that owns it. Handlers use them to
// refuse requests that reference another workspace's data. A missing resource
// returns 0.

func workspaceOf(query string, id int64) int64 {
	var ws int64
	if err := DB.QueryRow(query, id).Scan(&ws); err != nil {
		return 0
	}
	return ws
}

func WorkspaceOfList(id int64) int64 {
	return workspaceOf(`SELECT workspace_id FROM lists WHERE id = ?`, id)
}

func WorkspaceOfSection(id int64) int64 {
	return workspaceOf(`SELECT l.workspace_id FROM sections s JOIN lists l ON l.id = s.list_id WHERE s.id = ?`, id)
}

func WorkspaceOfItem(id int64) int64 {
	return workspaceOf(`SELECT l.workspace_id FROM items i JOIN sections s ON s.id = i.section_id JOIN lists l ON l.id = s.list_id WHERE i.id = ?`, id)
}

func WorkspaceOfTemplate(id int64) int64 {
	return workspaceOf(`SELECT workspace_id FROM templates WHERE id = ?`, id)
}

func WorkspaceOfTemplateItem(id int64) int64 {
	return workspaceOf(`SELECT t.workspace_id FROM template_items ti JOIN templates t ON t.id = ti.template_id WHERE ti.id = ?`, id)
}

func WorkspaceOfHistory(id int64) int64 {
	return workspaceOf(`SELECT workspace_id FROM item_history WHERE id = ?`, id)
}

// ResolveSessionWorkspace resolves the workspace a signed-in user should see: the
// one stored on the session when they are still a member, otherwise their
// first membership.
func ResolveSessionWorkspace(userID, preferred int64) (int64, error) {
	if preferred != 0 && MemberRole(preferred, userID) != "" {
		return preferred, nil
	}
	workspaces, err := WorkspacesForUser(userID)
	if err != nil || len(workspaces) == 0 {
		return 0, err
	}
	return workspaces[0].ID, nil
}

// SetSessionWorkspace remembers the workspace a session is currently viewing.
func SetSessionWorkspace(sessionID string, workspaceID int64) error {
	_, err := DB.Exec(`UPDATE sessions SET workspace_id = ? WHERE id = ?`, workspaceID, sessionID)
	return err
}

// CreateUserSession stores a session that belongs to a user.
func CreateUserSession(id string, expiresAt, userID, workspaceID int64) error {
	_, err := DB.Exec(`INSERT INTO sessions (id, expires_at, user_id, workspace_id) VALUES (?, ?, ?, ?)`,
		id, expiresAt, userID, workspaceID)
	return err
}

// ClearWorkspaceData deletes every list, template and history entry of one
// workspace (the "clear database" action, scoped so it cannot touch others).
func ClearWorkspaceData(workspaceID int64) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM lists WHERE workspace_id = ?`,
		`DELETE FROM templates WHERE workspace_id = ?`,
		`DELETE FROM item_history WHERE workspace_id = ?`,
	} {
		if _, err := tx.Exec(stmt, workspaceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

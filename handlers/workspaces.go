package handlers

import (
	"database/sql"
	"errors"
	"log"
	"net/url"
	"shopping-list/db"
	"strconv"
	"strings"
	"unicode"

	"github.com/gofiber/fiber/v2"
)

const (
	MaxWorkspaceNameLength = 60
	MaxUsernameLength      = 40
	MaxDisplayNameLength   = 60
	MinPasswordLength      = 8
)

// WorkspaceIcons are the icons offered when creating or editing a workspace.
// Every entry must fit in MaxIconLength bytes.
var WorkspaceIcons = []string{
	"🏠", "🏡", "👨‍👩‍👧", "👵", "👴", "👶", "❤️", "🎓",
	"💼", "🏢", "🏕️", "✈️", "🐕", "🐈", "🎉", "🛒",
}

// workspaceFormValues reads and validates the name and icon of a workspace
// form. The second result is an error code for the page, or "".
func workspaceFormValues(c *fiber.Ctx) (name, icon, errorCode string) {
	name = strings.TrimSpace(c.FormValue("name"))
	icon = strings.TrimSpace(c.FormValue("icon"))
	switch {
	case name == "":
		return "", "", "name_required"
	case len(name) > MaxWorkspaceNameLength:
		return "", "", "name_too_long"
	case len(icon) > MaxIconLength:
		return "", "", "icon_too_long"
	}
	if icon == "" {
		icon = db.DefaultWorkspaceIcon
	}
	return name, icon, ""
}

// addWorkspaceContext gives full pages what the workspace switcher needs.
func addWorkspaceContext(c *fiber.Ctx, data fiber.Map) {
	data["MultiUser"] = MultiUser()
	user := CurrentUser(c)
	if !MultiUser() || user == nil {
		return
	}
	data["User"] = user
	workspaces, _ := db.WorkspacesForUser(user.ID)
	data["Workspaces"] = workspaces
	current := WorkspaceID(c)
	for _, w := range workspaces {
		if w.ID == current {
			data["Workspace"] = w
		}
	}
	if current > 0 {
		data["CanManageWorkspace"] = canManage(user, current)
	}
}

// RequireMultiUser hides the management routes when accounts are disabled.
func RequireMultiUser(c *fiber.Ctx) error {
	if !MultiUser() {
		return notFound(c)
	}
	return c.Next()
}

// canManage: workspace owners, and administrators, may change membership.
func canManage(user *db.User, workspaceID int64) bool {
	return user.IsAdmin || db.MemberRole(workspaceID, user.ID) == db.RoleOwner
}

// workspaceErrors are the codes the page has a message for; anything else in
// ?error= is ignored instead of showing a raw translation key.
var workspaceErrors = map[string]bool{
	"name_required": true, "name_too_long": true, "icon_too_long": true, "failed": true,
	"user_not_found": true, "last_owner": true, "cannot_delete_default": true,
	"invalid_username": true, "username_too_long": true, "password_too_short": true,
	"user_exists": true, "cannot_delete_self": true, "password_too_long": true, "sole_owner": true,
	"wrong_password": true, "rate_limited": true, "cannot_delete_api": true, "last_admin": true,
}

// workspaceNotices are the confirmations the page can show after a change.
var workspaceNotices = map[string]bool{"password_changed": true, "password_reset": true, "account_created": true}

// ValidUsername refuses anything that cannot be typed and compared reliably:
// spaces, commas, control and invisible format characters.
func ValidUsername(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r == ',' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// cleanDisplayName drops control and invisible characters from a shown name.
func cleanDisplayName(name string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name))
}

func knownCode(known map[string]bool, code string) string {
	if known[code] {
		return code
	}
	return ""
}

func workspaceRedirect(c *fiber.Ctx, errorCode string) error {
	target := "/workspaces"
	if errorCode != "" {
		target += "?error=" + url.QueryEscape(errorCode)
	}
	return c.Redirect(target, fiber.StatusSeeOther)
}

func workspaceNotice(c *fiber.Ctx, notice string) error {
	return c.Redirect("/workspaces?notice="+url.QueryEscape(notice), fiber.StatusSeeOther)
}

// GetWorkspacesPage shows the caller's workspaces, the members of the current
// one, and (for administrators) all accounts.
func GetWorkspacesPage(c *fiber.Ctx) error {
	user := CurrentUser(c)
	current := WorkspaceID(c)

	workspaces, err := db.WorkspacesForUser(user.ID)
	if err != nil {
		return sendError(c, 500, "error.fetch_failed")
	}

	// Administrators can see every workspace so they can add themselves to it.
	var others []db.Workspace
	if user.IsAdmin {
		all, _ := db.AllWorkspaces()
		member := map[int64]bool{}
		for _, w := range workspaces {
			member[w.ID] = true
		}
		for _, w := range all {
			if !member[w.ID] {
				others = append(others, w)
			}
		}
	}

	var members []db.Member
	var currentWorkspace *db.Workspace
	canManageCurrent := false
	if current > 0 {
		members, _ = db.WorkspaceMembers(current)
		currentWorkspace, _ = db.GetWorkspace(current)
		canManageCurrent = canManage(user, current)
	}

	var users []db.User
	if user.IsAdmin {
		users, _ = db.ListUsers()
	}

	return c.Render("workspaces", withPageI18n(c, fiber.Map{
		"Error":            knownCode(workspaceErrors, c.Query("error")),
		"Notice":           knownCode(workspaceNotices, c.Query("notice")),
		"Others":           others,
		"Members":          members,
		"Current":          currentWorkspace,
		"CanManageCurrent": canManageCurrent,
		"CanDeleteCurrent": canManageCurrent && current != db.DefaultWorkspaceID && !apiPinnedWorkspace(current),
		"AllUsers":         users,
		"MinPassword":      MinPasswordLength,
		"Icons":            WorkspaceIcons,
	}))
}

// CreateWorkspace makes a new workspace owned by the caller and switches to it.
func CreateWorkspace(c *fiber.Ctx) error {
	user := CurrentUser(c)
	name, icon, errorCode := workspaceFormValues(c)
	if errorCode != "" {
		return workspaceRedirect(c, errorCode)
	}
	workspace, err := db.CreateWorkspace(name, icon, user.ID)
	if err != nil {
		log.Printf("Create workspace failed: %v", err)
		return workspaceRedirect(c, "failed")
	}
	db.SetSessionWorkspace(c.Cookies(SessionCookieName), workspace.ID)
	return switchingPage(c, "/")
}

// UpdateWorkspace renames a workspace and changes its icon. Owners (and
// administrators) only, like every other change to a workspace.
func UpdateWorkspace(c *fiber.Ctx) error {
	user := CurrentUser(c)
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || (db.MemberRole(id, user.ID) == "" && !user.IsAdmin) {
		return notFound(c)
	}
	if !canManage(user, id) {
		return sendError(c, 403, "error.forbidden")
	}
	name, icon, errorCode := workspaceFormValues(c)
	if errorCode != "" {
		return workspaceRedirect(c, errorCode)
	}
	if err := db.UpdateWorkspace(id, name, icon); err != nil {
		log.Printf("Update workspace failed: %v", err)
		return workspaceRedirect(c, "failed")
	}
	return workspaceRedirect(c, "")
}

// SwitchWorkspace changes the workspace this session views. Only members can.
func SwitchWorkspace(c *fiber.Ctx) error {
	user := CurrentUser(c)
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || db.MemberRole(id, user.ID) == "" {
		return notFound(c)
	}
	db.SetSessionWorkspace(c.Cookies(SessionCookieName), id)
	return switchingPage(c, "/")
}

// switchingPage discards the browser's offline copy of the previous workspace
// (IndexedDB and the service worker's page cache) before showing the new one,
// so one workspace's lists or queued changes never leak into another.
func switchingPage(c *fiber.Ctx, next string) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Type("html")
	return c.SendString(`<!doctype html><meta charset="utf-8"><title>Koffan</title>
<body style="font-family:system-ui;color:#78716c;text-align:center;padding-top:20vh">…
<script>
(async function () {
  try { if (window.caches) for (const key of await caches.keys()) await caches.delete(key); } catch (e) {}
  try { indexedDB.deleteDatabase('koffan-offline'); } catch (e) {}
  location.replace(` + strconv.Quote(next) + `);
})();
</script></body>`)
}

// AddWorkspaceMember adds an existing account to a workspace.
func AddWorkspaceMember(c *fiber.Ctx) error {
	user := CurrentUser(c)
	workspaceID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || (db.MemberRole(workspaceID, user.ID) == "" && !user.IsAdmin) {
		return notFound(c)
	}
	if !canManage(user, workspaceID) {
		return sendError(c, 403, "error.forbidden")
	}
	target, err := db.GetUserByUsername(c.FormValue("username"))
	if err != nil {
		return workspaceRedirect(c, "user_not_found")
	}
	role := db.RoleMember
	if c.FormValue("role") == db.RoleOwner {
		role = db.RoleOwner
	}
	if err := db.AddMember(workspaceID, target.ID, role); err != nil {
		return workspaceRedirect(c, "failed")
	}
	return workspaceRedirect(c, "")
}

// RemoveWorkspaceMember removes a member. People may always leave themselves.
func RemoveWorkspaceMember(c *fiber.Ctx) error {
	user := CurrentUser(c)
	workspaceID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	targetID, err2 := strconv.ParseInt(c.Params("userId"), 10, 64)
	if err != nil || err2 != nil || (db.MemberRole(workspaceID, user.ID) == "" && !user.IsAdmin) {
		return notFound(c)
	}
	if targetID != user.ID && !canManage(user, workspaceID) {
		return sendError(c, 403, "error.forbidden")
	}
	if err := db.RemoveMember(workspaceID, targetID); err != nil {
		if errors.Is(err, db.ErrLastOwner) {
			return workspaceRedirect(c, "last_owner")
		}
		return workspaceRedirect(c, "failed")
	}
	closeWebSockets(func(client *webSocketClient) bool {
		return client.userID == targetID && client.workspaceID == workspaceID
	})
	if targetID == user.ID {
		return switchingPage(c, "/workspaces")
	}
	return workspaceRedirect(c, "")
}

// DeleteWorkspace deletes a workspace and everything in it.
func DeleteWorkspace(c *fiber.Ctx) error {
	user := CurrentUser(c)
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || (db.MemberRole(id, user.ID) == "" && !user.IsAdmin) {
		return notFound(c)
	}
	if !canManage(user, id) {
		return sendError(c, 403, "error.forbidden")
	}
	if apiPinnedWorkspace(id) {
		return workspaceRedirect(c, "cannot_delete_api")
	}
	if err := db.DeleteWorkspace(id); err != nil {
		return workspaceRedirect(c, "cannot_delete_default")
	}
	closeWebSockets(func(client *webSocketClient) bool { return client.workspaceID == id })
	return switchingPage(c, "/workspaces")
}

// RequireAdmin limits account management to administrators.
func RequireAdmin(c *fiber.Ctx) error {
	if user := CurrentUser(c); user == nil || !user.IsAdmin {
		return sendError(c, 403, "error.forbidden")
	}
	return c.Next()
}

// CreateUserAccount lets an administrator create an account. There is no
// e-mail or self-service sign-up: the administrator hands over the password.
func CreateUserAccount(c *fiber.Ctx) error {
	username := strings.TrimSpace(c.FormValue("username"))
	password := c.FormValue("password")
	if len(username) > MaxUsernameLength {
		return workspaceRedirect(c, "username_too_long")
	}
	if !ValidUsername(username) {
		return workspaceRedirect(c, "invalid_username")
	}
	displayName := cleanDisplayName(c.FormValue("display_name"))
	if len(displayName) > MaxDisplayNameLength {
		return workspaceRedirect(c, "name_too_long")
	}
	if len(password) < MinPasswordLength {
		return workspaceRedirect(c, "password_too_short")
	}
	if len(password) > db.MaxPasswordBytes {
		return workspaceRedirect(c, "password_too_long")
	}
	user, err := db.CreateUser(username, displayName, password, c.FormValue("is_admin") == "on")
	if err != nil {
		if errors.Is(err, db.ErrUserExists) {
			return workspaceRedirect(c, "user_exists")
		}
		if errors.Is(err, db.ErrPasswordTooLong) {
			return workspaceRedirect(c, "password_too_long")
		}
		return workspaceRedirect(c, "failed")
	}
	// Optionally put the new person straight into the workspace being viewed.
	if c.FormValue("add_to_current") == "on" {
		db.AddMember(WorkspaceID(c), user.ID, db.RoleMember)
	}
	return workspaceNotice(c, "account_created")
}

// DeleteUserAccount removes an account (never your own).
func DeleteUserAccount(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id == CurrentUser(c).ID {
		return workspaceRedirect(c, "cannot_delete_self")
	}
	if err := db.DeleteUser(id); err != nil {
		if errors.Is(err, db.ErrLastAdmin) {
			return workspaceRedirect(c, "last_admin")
		}
		if errors.Is(err, db.ErrSoleOwner) {
			return workspaceRedirect(c, "sole_owner")
		}
		return workspaceRedirect(c, "failed")
	}
	closeWebSockets(func(client *webSocketClient) bool { return client.userID == id })
	return workspaceRedirect(c, "")
}

// ChangeOwnPassword lets anyone signed in change their password. It asks for
// the current one, so a session left open on someone else's device cannot be
// used to lock the owner out, and wrong guesses count against the login limit.
func ChangeOwnPassword(c *fiber.Ctx) error {
	user := CurrentUser(c)
	ip := c.IP()
	if loginLimiter != nil {
		if blocked, _ := loginLimiter.IsBlocked(ip); blocked {
			return workspaceRedirect(c, "rate_limited")
		}
	}
	if !db.CheckPassword(user.ID, c.FormValue("current_password")) {
		if loginLimiter != nil && loginLimiter.RecordAttempt(ip) {
			return workspaceRedirect(c, "rate_limited")
		}
		return workspaceRedirect(c, "wrong_password")
	}
	return setPassword(c, user.ID, c.FormValue("new_password"), "password_changed")
}

// ResetUserPassword lets an administrator give an account a new password, for
// someone who forgot theirs. There is no e-mail reset: the administrator hands
// the new password over, as when creating the account.
func ResetUserPassword(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return workspaceRedirect(c, "user_not_found")
	}
	return setPassword(c, id, c.FormValue("password"), "password_reset")
}

// setPassword stores a new password and signs the account out on every other
// device, closing their live connections too. The caller's own session stays.
func setPassword(c *fiber.Ctx, userID int64, password, notice string) error {
	if len(password) < MinPasswordLength {
		return workspaceRedirect(c, "password_too_short")
	}
	if len(password) > db.MaxPasswordBytes {
		return workspaceRedirect(c, "password_too_long")
	}
	keep := ""
	if userID == CurrentUser(c).ID {
		keep = c.Cookies(SessionCookieName)
	}
	if err := db.SetPassword(userID, password, keep); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workspaceRedirect(c, "user_not_found")
		}
		log.Printf("Set password failed: %v", err)
		return workspaceRedirect(c, "failed")
	}
	closeWebSockets(func(client *webSocketClient) bool {
		return client.userID == userID && client.sessionID != keep
	})
	return workspaceNotice(c, notice)
}

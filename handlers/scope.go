package handlers

import (
	"database/sql"
	"errors"
	"os"
	"shopping-list/db"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// Multi-user mode is opt-in. Without MULTI_USER=true the application keeps the
// single shared password and a single implicit workspace, exactly as before.
//
// Every request carries a workspace (and, in multi-user mode, a user). The
// guards below reject any request that references a list, section, item or
// template outside the caller's current workspace, so isolation does not
// depend on each handler remembering to check.

const (
	localWorkspace = "workspace_id"
	localUser      = "user"
	localSession   = "session"

	// WorkspaceHeader is sent by the browser with every request and names the
	// workspace the page was rendered for. WorkspaceMismatchHeader marks the
	// 409 answer when that is no longer the session's workspace.
	WorkspaceHeader         = "X-Koffan-Workspace"
	WorkspaceMismatchHeader = "X-Workspace-Mismatch"
)

// MultiUser reports whether accounts and workspaces are enabled.
func MultiUser() bool {
	return os.Getenv("MULTI_USER") == "true" && !isAuthDisabled()
}

// WorkspaceID returns the workspace the request operates on. In multi-user
// mode a request without one matches nothing (fail closed) instead of falling
// back to the default workspace.
func WorkspaceID(c *fiber.Ctx) int64 {
	return workspaceFromLocals(c.Locals(localWorkspace))
}

// workspaceFromLocals reads a workspace stored by AuthMiddleware or
// UseAPIWorkspace, from a request or from a WebSocket connection.
func workspaceFromLocals(value interface{}) int64 {
	if id, ok := value.(int64); ok && id != 0 {
		return id
	}
	if MultiUser() {
		return -1
	}
	return db.DefaultWorkspaceID
}

// CurrentUser returns the signed-in user, or nil (single-password mode, API).
func CurrentUser(c *fiber.Ctx) *db.User {
	user, _ := c.Locals(localUser).(*db.User)
	return user
}

// APIWorkspaceID is the workspace the REST API token operates on. It defaults
// to the original workspace so existing integrations keep working, and
// API_WORKSPACE_ID only counts when accounts are enabled.
func APIWorkspaceID() int64 {
	if !MultiUser() {
		return db.DefaultWorkspaceID
	}
	if v, err := strconv.ParseInt(os.Getenv("API_WORKSPACE_ID"), 10, 64); err == nil && v > 0 {
		return v
	}
	return db.DefaultWorkspaceID
}

func apiPinnedWorkspace(id int64) bool {
	return MultiUser() && os.Getenv("API_TOKEN") != "" && id == APIWorkspaceID()
}

// UseAPIWorkspace pins REST API requests (which authenticate with a token, not
// a user) to APIWorkspaceID.
func UseAPIWorkspace(c *fiber.Ctx) error {
	id := APIWorkspaceID()
	if MultiUser() {
		if _, err := db.GetWorkspace(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
					"error": "api_workspace_missing", "message": "API_WORKSPACE_ID names a workspace that does not exist",
				})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "db_error", "message": "Failed to fetch workspace",
			})
		}
	}
	c.Locals(localWorkspace, id)
	return c.Next()
}

// ==================== GUARDS ====================

func notFound(c *fiber.Ctx) error {
	return sendError(c, fiber.StatusNotFound, "error.not_found")
}

// notFoundFor answers 404. The REST API keeps its JSON error shape, with the
// same messages its own handlers use for a missing resource.
func notFoundFor(c *fiber.Ctx, what string) error {
	if strings.HasPrefix(c.Path(), "/api/v1/") {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found", "message": what + " not found"})
	}
	return notFound(c)
}

// ownedBy reports whether a resource exists in the request's workspace.
func ownedBy(c *fiber.Ctx, lookup func(int64) int64, id int64) bool {
	return lookup(id) == WorkspaceID(c)
}

func OwnsList(c *fiber.Ctx, id int64) bool    { return ownedBy(c, db.WorkspaceOfList, id) }
func OwnsSection(c *fiber.Ctx, id int64) bool { return ownedBy(c, db.WorkspaceOfSection, id) }

// guardParam checks a numeric route parameter. Unknown and foreign IDs get the
// same answer, so the response does not reveal that another workspace has them.
func guardParam(param, what string, lookup func(int64) int64) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params(param), 10, 64)
		if err != nil {
			return c.Next() // the handler reports the malformed ID
		}
		if !ownedBy(c, lookup, id) {
			return notFoundFor(c, what)
		}
		return c.Next()
	}
}

// guardParamQuiet is guardParam for deletes, which were idempotent before
// workspaces existed (deleting something already gone answered 200). A missing
// or foreign id is answered with the same empty 200 and nothing is touched, so
// a second device deleting the same list keeps working and nothing is revealed.
func guardParamQuiet(param string, lookup func(int64) int64) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params(param), 10, 64)
		if err != nil {
			return c.Next()
		}
		if !ownedBy(c, lookup, id) {
			return c.SendString("")
		}
		return c.Next()
	}
}

// RequireList, RequireSection, RequireItem and RequireTemplate guard routes
// with an :id parameter.
var (
	RequireList         = guardParam("id", "List", db.WorkspaceOfList)
	RequireSection      = guardParam("id", "Section", db.WorkspaceOfSection)
	RequireItem         = guardParam("id", "Item", db.WorkspaceOfItem)
	RequireTemplate     = guardParam("id", "Template", db.WorkspaceOfTemplate)
	RequireTemplateItem = guardParam("itemId", "Template item", db.WorkspaceOfTemplateItem)
	RequireHistory      = guardParam("id", "History entry", db.WorkspaceOfHistory)

	// Quiet variants for idempotent DELETE routes.
	RequireListQuiet         = guardParamQuiet("id", db.WorkspaceOfList)
	RequireSectionQuiet      = guardParamQuiet("id", db.WorkspaceOfSection)
	RequireTemplateQuiet     = guardParamQuiet("id", db.WorkspaceOfTemplate)
	RequireTemplateItemQuiet = guardParamQuiet("itemId", db.WorkspaceOfTemplateItem)
)

// RequireListPage is RequireList for browser pages: it sends the visitor home
// instead of showing an error.
func RequireListPage(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || !OwnsList(c, id) {
		return c.Redirect("/")
	}
	return c.Next()
}

// guardValue checks an ID passed in the query string or a form field. An
// absent value is left to the handler.
func guardValue(source func(*fiber.Ctx, string) string, field, what string, lookup func(int64) int64) fiber.Handler {
	return func(c *fiber.Ctx) error {
		raw := source(c, field)
		if raw == "" {
			return c.Next()
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.Next()
		}
		if !ownedBy(c, lookup, id) {
			return notFoundFor(c, what)
		}
		return c.Next()
	}
}

func formField(c *fiber.Ctx, field string) string  { return c.FormValue(field) }
func queryField(c *fiber.Ctx, field string) string { return c.Query(field) }

func RequireFormSection(field string) fiber.Handler {
	return guardValue(formField, field, "Section", db.WorkspaceOfSection)
}

func RequireQueryList(field string) fiber.Handler {
	return guardValue(queryField, field, "List", db.WorkspaceOfList)
}

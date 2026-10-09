package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

// RegisterRoutes attaches every route that sits behind authentication. It must
// be called after AuthMiddleware. Each route that names a list, section, item
// or template is guarded so it can only reach the caller's own workspace.
func RegisterRoutes(app *fiber.App) {
	// WebSocket upgrade middleware
	app.Use("/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	// WebSocket endpoint
	app.Get("/ws", websocket.New(WebSocketHandler))

	// Main page - shows all lists
	app.Get("/", GetListsPage)
	app.Get("/offline/list", GetOfflineListShell)
	app.Get("/api/offline/snapshot", GetOfflineSnapshot)
	app.Post("/api/offline/sync", SyncOffline)

	// Single list view - shows items
	app.Get("/lists/:id", RequireListPage, GetListView)

	// Sections API
	app.Get("/sections/list", RequireQueryList("list_id"), GetSectionsListForModal)
	app.Get("/sections/:id/html", RequireSection, GetSectionHTML)
	app.Post("/sections", CreateSection)
	app.Put("/sections/:id", RequireSection, RequireQueryList("list_id"), UpdateSection)
	app.Delete("/sections/:id", RequireSectionQuiet, DeleteSection)
	app.Post("/sections/:id/move-up", RequireSection, RequireQueryList("list_id"), MoveSectionUp)
	app.Post("/sections/:id/move-down", RequireSection, RequireQueryList("list_id"), MoveSectionDown)
	app.Post("/sections/:id/check-all", RequireSection, CheckAllItems)
	app.Post("/sections/:id/uncheck-all", RequireSection, UncheckAllItems)
	app.Post("/sections/:id/sort-mode", RequireSection, UpdateSectionSortMode)

	// Lists API
	app.Get("/lists", GetLists)
	app.Post("/lists", CreateList)
	app.Put("/lists/:id", RequireList, UpdateList)
	app.Delete("/lists/:id", RequireListQuiet, DeleteList)
	app.Post("/lists/:id/activate", RequireList, SetActiveList)
	app.Get("/lists/:id/activate", RequireList, SetActiveList)
	app.Post("/lists/:id/move-up", RequireList, MoveListUp)
	app.Post("/lists/:id/move-down", RequireList, MoveListDown)
	app.Post("/lists/:id/toggle-completed", RequireList, ToggleShowCompleted)

	// Templates API
	app.Get("/templates", GetTemplates)
	app.Get("/templates/:id", RequireTemplate, GetTemplate)
	app.Post("/templates", CreateTemplate)
	app.Put("/templates/:id", RequireTemplate, UpdateTemplate)
	app.Delete("/templates/:id", RequireTemplateQuiet, DeleteTemplate)
	app.Post("/templates/:id/items", RequireTemplate, AddTemplateItem)
	app.Put("/templates/:id/items/:itemId", RequireTemplate, RequireTemplateItem, UpdateTemplateItem)
	app.Delete("/templates/:id/items/:itemId", RequireTemplateQuiet, RequireTemplateItemQuiet, DeleteTemplateItem)
	app.Post("/templates/:id/apply", RequireTemplate, ApplyTemplate)
	app.Post("/templates/from-list", CreateTemplateFromList)

	// Items API
	app.Get("/items/:id/html", RequireItem, GetItemHTML)
	app.Post("/items", RequireFormSection("section_id"), CreateItem)
	app.Post("/items/delete-completed", DeleteCompletedItems)
	app.Put("/items/:id", RequireItem, UpdateItem)
	app.Delete("/items/:id", RequireItem, DeleteItem)
	app.Post("/items/:id/toggle", RequireItem, ToggleItem)
	app.Post("/items/:id/quantity", RequireItem, AdjustItemQuantity)
	app.Post("/items/:id/uncertain", RequireItem, ToggleUncertain)
	app.Post("/items/:id/move", RequireItem, RequireFormSection("section_id"), MoveItemToSection)
	app.Post("/items/:id/move-up", RequireItem, MoveItemUp)
	app.Post("/items/:id/move-down", RequireItem, MoveItemDown)

	// Stats API
	app.Get("/stats", RequireQueryList("list_id"), GetStats)

	// Offline data API
	app.Get("/api/data", RequireQueryList("list_id"), GetAllData)
	app.Get("/api/item/:id/version", RequireItem, GetItemVersion)
	app.Get("/api/suggestions", GetSuggestions)

	// History management API
	app.Get("/api/history", GetHistory)
	app.Delete("/api/history/:id", RequireHistory, DeleteHistoryItem)
	app.Post("/api/history/batch-delete", BatchDeleteHistory)

	// Batch operations
	app.Post("/sections/batch-delete", RequireQueryList("list_id"), BatchDeleteSections)

	// Import/Export
	app.Get("/export", ExportAllData)
	app.Get("/export/list/:id", RequireList, ExportSingleList)
	app.Get("/export/preview", GetExportPreview)
	app.Post("/import", ImportData)
	app.Post("/import/preview", PreviewImport)

	// Workspaces and accounts (only with MULTI_USER=true)
	app.Get("/workspaces", RequireMultiUser, GetWorkspacesPage)
	app.Post("/workspaces", RequireMultiUser, CreateWorkspace)
	app.Post("/workspaces/:id/switch", RequireMultiUser, SwitchWorkspace)
	app.Post("/workspaces/:id/update", RequireMultiUser, UpdateWorkspace)
	app.Post("/workspaces/:id/delete", RequireMultiUser, DeleteWorkspace)
	app.Post("/workspaces/:id/members", RequireMultiUser, AddWorkspaceMember)
	app.Post("/workspaces/:id/members/:userId/remove", RequireMultiUser, RemoveWorkspaceMember)
	app.Post("/admin/users", RequireMultiUser, RequireAdmin, CreateUserAccount)
	app.Post("/admin/users/:id/delete", RequireMultiUser, RequireAdmin, DeleteUserAccount)
	app.Post("/admin/users/:id/password", RequireMultiUser, RequireAdmin, ResetUserPassword)
	app.Post("/account/password", RequireMultiUser, ChangeOwnPassword)

	// Database management
	app.Get("/api/database/csrf-token", GenerateCSRFToken)
	app.Post("/api/database/clear", ClearDatabase)
}

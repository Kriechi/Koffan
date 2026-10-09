package handlers

import (
	"shopping-list/db"

	"github.com/gofiber/fiber/v2"
)

// GetOfflineListShell provides the cached UI for newly created offline lists.
// The local model selects the actual list from the URL, never from this shell.
func GetOfflineListShell(c *fiber.Ctx) error {
	snapshot, err := loadOfflineSnapshot(WorkspaceID(c))
	if err != nil {
		return sendError(c, 500, "error.fetch_failed")
	}
	return c.Render("list", withPageI18n(c, fiber.Map{
		"List": &db.List{ShowCompleted: true}, "Lists": snapshot.Lists,
		"Sections": []db.Section{}, "Stats": db.Stats{}, "ShowCompleted": true,
		"OfflineSnapshot": snapshot,
	}))
}

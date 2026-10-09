package api

import (
	"log"
	"shopping-list/handlers"

	"github.com/gofiber/fiber/v2"
)

// Register conditionally registers the API routes if API_TOKEN is set
func Register(app *fiber.App) {
	if !IsAPIEnabled() {
		log.Println("REST API is disabled (API_TOKEN not set)")
		// Register catch-all handler that returns 503 for all API requests
		app.All("/api/v1/*", func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusServiceUnavailable).JSON(ErrorResponse{
				Error:   "api_disabled",
				Message: "API is not enabled on this server",
			})
		})
		return
	}

	log.Println("REST API is enabled")

	// Create API group with version prefix and token auth middleware
	v1 := app.Group("/api/v1", TokenAuthMiddleware, handlers.UseAPIWorkspace)

	// Lists endpoints
	v1.Get("/lists", GetLists)
	v1.Get("/lists/:id", handlers.RequireList, GetList)
	v1.Post("/lists", CreateList)
	v1.Put("/lists/:id", handlers.RequireList, UpdateList)
	v1.Delete("/lists/:id", handlers.RequireList, DeleteList)
	v1.Get("/lists/:id/sections", handlers.RequireList, GetListSections)
	v1.Post("/lists/:id/move-up", handlers.RequireList, MoveListUp)
	v1.Post("/lists/:id/move-down", handlers.RequireList, MoveListDown)

	// Sections endpoints
	v1.Get("/sections/:id", handlers.RequireSection, GetSection)
	v1.Post("/sections", CreateSection)
	v1.Put("/sections/:id", handlers.RequireSection, UpdateSection)
	v1.Delete("/sections/:id", handlers.RequireSection, DeleteSection)
	v1.Get("/sections/:id/items", handlers.RequireSection, GetSectionItems)
	v1.Post("/sections/:id/move-up", handlers.RequireSection, MoveSectionUp)
	v1.Post("/sections/:id/move-down", handlers.RequireSection, MoveSectionDown)
	v1.Post("/sections/:id/check-all", handlers.RequireSection, CheckAllItems)
	v1.Post("/sections/:id/uncheck-all", handlers.RequireSection, UncheckAllItems)
	v1.Post("/sections/:id/sort-mode", handlers.RequireSection, UpdateSectionSortMode)

	// Items endpoints
	v1.Get("/items/:id", handlers.RequireItem, GetItem)
	v1.Post("/items", CreateItem)
	v1.Put("/items/:id", handlers.RequireItem, UpdateItem)
	v1.Delete("/items/:id", handlers.RequireItem, DeleteItem)
	v1.Post("/items/:id/toggle", handlers.RequireItem, ToggleItemCompleted)
	v1.Post("/items/:id/uncertain", handlers.RequireItem, ToggleItemUncertain)
	v1.Post("/items/:id/quantity", handlers.RequireItem, AdjustItemQuantity)
	v1.Post("/items/:id/move", handlers.RequireItem, MoveItem)
	v1.Post("/items/:id/move-up", handlers.RequireItem, MoveItemUp)
	v1.Post("/items/:id/move-down", handlers.RequireItem, MoveItemDown)

	// Batch endpoint
	v1.Post("/batch", BatchCreate)

	// History endpoints (suggestions)
	v1.Get("/history", GetHistory)
	v1.Post("/history", CreateHistory)
	v1.Delete("/history/:id", handlers.RequireHistory, DeleteHistory)
	v1.Post("/history/batch-delete", BatchDeleteHistory)
}

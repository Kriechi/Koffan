package handlers

import (
	"errors"
	"fmt"
	"shopping-list/db"
	"shopping-list/webhook"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
)

var ErrInvalidListID = errors.New("invalid list_id")

// ErrListNotFound covers lists that do not exist and lists of another
// workspace: the two are indistinguishable to the caller.
var ErrListNotFound = errors.New("list not found")

// GetSectionHTML returns a single section rendered as HTML partial
func GetSectionHTML(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	section, err := db.GetSectionByID(id)
	if err != nil {
		return sendError(c, 404, "error.section_not_found")
	}

	return c.Render("partials/section", sectionRenderMap(c, section), "")
}

// GetSections returns all sections with items (for full page render)
func GetSections(c *fiber.Ctx) error {
	sections, err := db.GetAllSections(WorkspaceID(c))
	if err != nil {
		return sendError(c, 500, "error.fetch_failed")
	}

	stats := db.GetStats(WorkspaceID(c))

	// Get lists for dropdown
	lists, _ := db.GetAllLists(WorkspaceID(c))
	activeList, _ := db.GetActiveList(WorkspaceID(c))

	return c.Render("list", withPageI18n(c, fiber.Map{
		"Sections":   sections,
		"Stats":      stats,
		"Lists":      lists,
		"ActiveList": activeList,
	}))
}

// CreateSection creates a new section
func CreateSection(c *fiber.Ctx) error {
	name := c.FormValue("name")
	if name == "" {
		return sendError(c, 400, "error.name_required")
	}
	if len(name) > MaxSectionNameLength {
		return sendError(c, 400, "error.name_too_long")
	}
	if name == "[HISTORY]" {
		return sendError(c, 400, "common.reserved_name")
	}

	section, err := db.CreateSection(WorkspaceID(c), name)
	if err != nil {
		return sendError(c, 500, "error.create_failed")
	}

	// Broadcast to WebSocket clients
	BroadcastUpdate(c, "section_created", section)

	// Return the new section partial for HTMX
	return c.Render("partials/section", sectionRenderMap(c, section), "")
}

// UpdateSection updates a section's name
func UpdateSection(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	name := c.FormValue("name")
	if name == "" {
		return sendError(c, 400, "error.name_required")
	}
	if len(name) > MaxSectionNameLength {
		return sendError(c, 400, "error.name_too_long")
	}
	if name == "[HISTORY]" {
		return sendError(c, 400, "common.reserved_name")
	}

	section, err := db.UpdateSection(id, name)
	if err != nil {
		return sendError(c, 500, "error.update_failed")
	}

	// Broadcast to WebSocket clients
	BroadcastUpdate(c, "section_updated", section)

	// Return appropriate partial based on context
	if c.Get("HX-Target") == "manage-sections-list" {
		return returnSectionsForModal(c)
	}

	// Return updated section partial for main list
	return c.Render("partials/section", sectionRenderMap(c, section), "")
}

// DeleteSection deletes a section and all its items
func DeleteSection(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	preparedWebhooks := PrepareSectionItemWebhooks(webhook.EventItemDeleted, id)
	err = db.DeleteSection(id)
	if err != nil {
		return sendError(c, 500, "error.delete_failed")
	}

	// Broadcast to WebSocket clients
	BroadcastUpdate(c, "section_deleted", map[string]int64{"id": id})
	NotifyPreparedItemWebhooks(webhook.EventItemDeleted, preparedWebhooks)

	// Return empty string (HTMX will remove the element)
	return c.SendString("")
}

// MoveSectionUp moves a section up in order
func MoveSectionUp(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	err = db.MoveSectionUp(id)
	if err != nil {
		return sendError(c, 500, "error.move_failed")
	}

	BroadcastUpdate(c, "sections_reordered", nil)

	// Modal expects full list, main page handles reorder via WS
	if c.Get("HX-Target") == "manage-sections-list" {
		return returnSectionsForModal(c)
	}
	return c.SendStatus(200)
}

// MoveSectionDown moves a section down in order
func MoveSectionDown(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	err = db.MoveSectionDown(id)
	if err != nil {
		return sendError(c, 500, "error.move_failed")
	}

	BroadcastUpdate(c, "sections_reordered", nil)

	if c.Get("HX-Target") == "manage-sections-list" {
		return returnSectionsForModal(c)
	}
	return c.SendStatus(200)
}

// UpdateSectionSortMode updates the sort mode of a section
func UpdateSectionSortMode(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return sendError(c, 400, "error.invalid_id")
	}

	sortMode := c.FormValue("sort_mode")
	if sortMode == "" {
		return sendError(c, 400, "error.sort_mode_required")
	}

	section, err := db.UpdateSectionSortMode(id, sortMode)
	if err != nil {
		return sendError(c, 500, "error.update_failed")
	}

	BroadcastUpdate(c, "section_sort_changed", map[string]interface{}{"section_id": id, "sort_mode": sortMode})

	return c.Render("partials/section", sectionRenderMap(c, section), "")
}

// Helper to get sections for dropdown
func getSectionsForDropdown(c *fiber.Ctx) []db.Section {
	sections, _ := db.GetAllSections(WorkspaceID(c))
	return sections
}

// BatchDeleteSections deletes multiple sections
func BatchDeleteSections(c *fiber.Ctx) error {
	// Get IDs from form (comma-separated or multiple values)
	idsStr := c.FormValue("ids")
	if idsStr == "" {
		return sendError(c, 400, "error.no_ids")
	}

	// Parse IDs. Sections that are already gone (another device deleted them) or
	// belong to another workspace are skipped the same way: the rest of the
	// batch still goes through and nothing tells the two cases apart.
	var ids []int64
	parsed := 0
	for _, idStr := range splitAndTrimCSV(idsStr) {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		parsed++
		if OwnsSection(c, id) {
			ids = append(ids, id)
		}
	}

	if parsed == 0 {
		return sendError(c, 400, "error.no_valid_ids")
	}

	if len(ids) > 0 {
		var preparedWebhooks []PreparedItemWebhook
		for _, id := range ids {
			preparedWebhooks = append(preparedWebhooks, PrepareSectionItemWebhooks(webhook.EventItemDeleted, id)...)
		}
		err := db.DeleteSections(ids)
		if err != nil {
			return sendError(c, 500, "error.delete_failed")
		}

		// Broadcast to WebSocket clients
		BroadcastUpdate(c, "sections_deleted", map[string]interface{}{"ids": ids})
		NotifyPreparedItemWebhooks(webhook.EventItemDeleted, preparedWebhooks)
	}

	// Return updated sections list for modal
	return returnSectionsForModal(c)
}

// splitAndTrimCSV splits a comma-separated string and returns non-empty trimmed parts.
func splitAndTrimCSV(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// getSectionsForList returns sections for a specific list (by list_id query param) or falls back to the active list.
func getSectionsForList(c *fiber.Ctx) ([]db.Section, error) {
	if listIDStr := c.Query("list_id"); listIDStr != "" {
		listID, err := strconv.ParseInt(listIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidListID, listIDStr)
		}
		if !OwnsList(c, listID) {
			return nil, ErrListNotFound
		}
		return db.GetSectionsByList(listID)
	}
	return db.GetAllSections(WorkspaceID(c))
}

// Helper to return sections for modal
func returnSectionsForModal(c *fiber.Ctx) error {
	sections, err := getSectionsForList(c)
	if err != nil {
		if errors.Is(err, ErrInvalidListID) {
			return sendError(c, 400, "error.invalid_list_id")
		}
		if errors.Is(err, ErrListNotFound) {
			return notFound(c)
		}
		return sendError(c, 500, "error.fetch_failed")
	}

	return c.Render("partials/manage_sections_list", fiber.Map{
		"Sections": sections,
	}, "")
}

// GetSectionsListForModal returns sections list for the management modal
func GetSectionsListForModal(c *fiber.Ctx) error {
	// Check if JSON format is requested
	if c.Query("format") == "json" {
		sections, err := getSectionsForList(c)
		if err != nil {
			if errors.Is(err, ErrInvalidListID) {
				return c.Status(400).JSON(fiber.Map{"error": "Invalid list_id parameter"})
			}
			if errors.Is(err, ErrListNotFound) {
				return c.Status(404).JSON(fiber.Map{"error": "List not found"})
			}
			return c.Status(500).JSON(fiber.Map{"error": "Failed to fetch sections"})
		}
		// Return simplified JSON for select options
		type SectionOption struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		var options []SectionOption
		for _, s := range sections {
			options = append(options, SectionOption{ID: s.ID, Name: s.Name})
		}
		return c.JSON(options)
	}
	return returnSectionsForModal(c)
}

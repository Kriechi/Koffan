package api

import (
	"database/sql"
	"shopping-list/db"
	"shopping-list/handlers"

	"github.com/gofiber/fiber/v2"
)

// ownedList and ownedSection replace db.GetListByID / db.GetSectionByID where
// the ID comes from the request body. IDs outside the token's workspace look
// exactly like missing ones.
func ownedList(c *fiber.Ctx, id int64) (*db.List, error) {
	if !handlers.OwnsList(c, id) {
		return nil, sql.ErrNoRows
	}
	return db.GetListByID(id)
}

func ownedSection(c *fiber.Ctx, id int64) (*db.Section, error) {
	if !handlers.OwnsSection(c, id) {
		return nil, sql.ErrNoRows
	}
	return db.GetSectionByID(id)
}

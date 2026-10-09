package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"shopping-list/db"
	"shopping-list/webhook"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// loadOfflineSnapshot reads the snapshot sent to clients, stamped with the
// time it was generated.
func loadOfflineSnapshot(workspaceID int64) (db.OfflineSnapshot, error) {
	snapshot, err := db.GetOfflineSnapshot(workspaceID)
	snapshot.GeneratedAt = time.Now().UnixMilli()
	return snapshot, err
}

// GetOfflineSnapshot returns all lists so an offline device can navigate its
// cached lists independently of another device's current list.
func GetOfflineSnapshot(c *fiber.Ctx) error {
	snapshot, err := loadOfflineSnapshot(WorkspaceID(c))
	if err != nil {
		log.Printf("Offline snapshot failed: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to load offline snapshot"})
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(snapshot)
}

// SyncOffline acknowledges a batch only after its mutations and replay receipts
// have committed. Authentication is provided by the regular API middleware.
func SyncOffline(c *fiber.Ctx) error {
	var request db.OfflineSyncRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid offline sync request"})
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid offline sync request"})
	}
	response, err := db.ApplyOfflineSync(WorkspaceID(c), request, enqueueOfflineWebhook)
	if err != nil {
		var inputErr *db.OfflineError
		if errors.As(err, &inputErr) {
			status := fiber.StatusBadRequest
			if inputErr.Conflict {
				status = fiber.StatusConflict
			}
			return c.Status(status).JSON(fiber.Map{"error": inputErr.Message, "operation_id": inputErr.OperationID})
		}
		log.Printf("Offline sync failed: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Failed to save offline changes"})
	}
	if response.Changed {
		BroadcastUpdate(c, "offline_sync", nil)
	}
	response.Snapshot.GeneratedAt = time.Now().UnixMilli()
	c.Set("Cache-Control", "no-store")
	return c.JSON(response)
}

// Use the sync transaction directly so webhook delivery survives a process exit
// between the database commit and HTTP acknowledgement. The dispatcher polls
// the outbox and can only observe these rows after the sync transaction commits.
func enqueueOfflineWebhook(tx *sql.Tx, change db.OfflineItemEvent) error {
	if !webhook.Accepts(change.Event) {
		return nil
	}
	payload, err := json.Marshal(webhook.Event{
		ID: uuid.NewString(), Event: change.Event, Timestamp: time.Now().UTC(),
		Data: fiber.Map{"item": change.Item, "section": change.Section, "list": change.List},
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO webhook_outbox (event, payload) VALUES (?, ?)", change.Event, payload)
	return err
}

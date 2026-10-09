package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"shopping-list/db"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// Return the rendered item as JSON so these tests exercise the real HTTP
// handler and persistence without coupling completion semantics to templates.
type completionTestViews struct{}

func (completionTestViews) Load() error { return nil }
func (completionTestViews) Render(out io.Writer, _ string, binding interface{}, _ ...string) error {
	return json.NewEncoder(out).Encode(binding.(fiber.Map)["Item"])
}

func TestItemCompletionRetriesPreserveDesiredState(t *testing.T) {
	initTestDatabase(t)
	list, err := db.CreateList(db.DefaultWorkspaceID, "Shopping", "cart")
	if err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSectionForList(list.ID, "Food")
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateItem(section.ID, "Milk", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{Views: completionTestViews{}})
	app.Post("/items/:id/toggle", ToggleItem)
	path := "/items/" + strconv.FormatInt(item.ID, 10) + "/toggle"

	for _, test := range []struct {
		name   string
		body   string
		want   bool
		status int
	}{
		{"complete", "completed=true", true, http.StatusOK},
		{"retry completed after lost response", "completed=true", true, http.StatusOK},
		{"uncomplete", "completed=false", false, http.StatusOK},
		{"retry uncompleted after lost response", "completed=false", false, http.StatusOK},
		{"invalid state", "completed=maybe", false, http.StatusBadRequest},
		{"empty state", "completed=", false, http.StatusBadRequest},
		{"legacy toggle", "", true, http.StatusOK},
		{"legacy toggle again", "", false, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != test.status {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, test.status, body)
			}
			current, err := db.GetItemByID(item.ID)
			if err != nil || current.Completed != test.want {
				t.Fatalf("item = %#v, err = %v; want completed %v", current, err, test.want)
			}
		})
	}
}

func TestSetItemCompletedNoOpDoesNotChangeVersion(t *testing.T) {
	initTestDatabase(t)
	list, err := db.CreateList(db.DefaultWorkspaceID, "Shopping", "cart")
	if err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSectionForList(list.ID, "Food")
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateItem(section.ID, "Milk", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("UPDATE items SET updated_at = 123 WHERE id = ?", item.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, changed, err := db.SetItemCompleted(item.ID, false)
	if err != nil || changed || unchanged.UpdatedAt != 123 {
		t.Fatalf("no-op = %#v, changed = %v, err = %v", unchanged, changed, err)
	}
	updated, changed, err := db.SetItemCompleted(item.ID, true)
	if err != nil || !changed || !updated.Completed || updated.UpdatedAt <= 123 {
		t.Fatalf("update = %#v, changed = %v, err = %v", updated, changed, err)
	}
}

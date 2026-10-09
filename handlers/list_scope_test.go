package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"shopping-list/db"
	"strconv"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestListSnapshotsAndStatsRemainScopedWhenAnotherTabChangesActiveList(t *testing.T) {
	initTestDatabase(t)
	requested, err := db.CreateList(db.DefaultWorkspaceID, "Open on phone", "cart")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateList(db.DefaultWorkspaceID, "Open on another phone", "cart")
	if err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSectionForList(requested.ID, "Groceries")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Milk", "Bread"} {
		item, err := db.CreateItem(section.ID, name, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if name == "Milk" {
			if _, _, err := db.SetItemCompleted(item.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.SetActiveList(other.ID); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Get("/stats", GetStats)
	app.Get("/api/data", GetAllData)
	query := "?list_id=" + strconv.FormatInt(requested.ID, 10)
	want := db.Stats{TotalItems: 2, CompletedItems: 1, Percentage: 50}

	for _, endpoint := range []string{"/stats", "/api/data"} {
		for _, scoped := range []bool{true, false} {
			path := endpoint
			if scoped {
				path += query
			}
			t.Run(path, func(t *testing.T) {
				resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				var stats db.Stats
				if endpoint == "/stats" {
					if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
						t.Fatal(err)
					}
				} else {
					var snapshot struct {
						Sections []db.Section `json:"sections"`
						Stats    db.Stats     `json:"stats"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
						t.Fatal(err)
					}
					stats = snapshot.Stats
					if scoped {
						if len(snapshot.Sections) != 1 || snapshot.Sections[0].ID != section.ID || len(snapshot.Sections[0].Items) != 2 {
							t.Fatalf("wrong list snapshot: %#v", snapshot.Sections)
						}
					} else if len(snapshot.Sections) != 0 {
						t.Fatalf("legacy endpoint should use empty active list: %#v", snapshot.Sections)
					}
				}
				expected := db.Stats{}
				if scoped {
					expected = want
				}
				if stats != expected {
					t.Fatalf("stats = %#v, want %#v", stats, expected)
				}
			})
		}
	}
	active, err := db.GetActiveList(db.DefaultWorkspaceID)
	if err != nil || active.ID != other.ID {
		t.Fatalf("scoped reads changed active list: %#v, %v", active, err)
	}
}

func TestListScopedEndpointsRejectInvalidIDs(t *testing.T) {
	app := fiber.New()
	app.Get("/stats", GetStats)
	app.Get("/api/data", GetAllData)
	for _, endpoint := range []string{"/stats", "/api/data"} {
		for _, value := range []string{"", "abc", "0", "-1", "9223372036854775808"} {
			path := endpoint + "?list_id=" + value
			t.Run(path, func(t *testing.T) {
				resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", resp.StatusCode)
				}
			})
		}
	}
}

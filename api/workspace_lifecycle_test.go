package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"shopping-list/db"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestMissingAPIWorkspaceRejectsReadsAndWrites(t *testing.T) {
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "api.db"))
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	t.Setenv("API_TOKEN", "secret-token")
	db.Init()
	t.Cleanup(db.Close)
	workspace := newAPIWorkspace(t, "pinned")
	t.Setenv("API_WORKSPACE_ID", id(workspace.id))
	app := fiber.New()
	Register(app)
	if _, err := db.DB.Exec(`DELETE FROM workspaces WHERE id = ?`, workspace.id); err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM lists`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method, path string
		body         interface{}
	}{
		{"GET", "/api/v1/lists", nil},
		{"POST", "/api/v1/lists", fiber.Map{"name": "orphan list"}},
		{"POST", "/api/v1/batch", fiber.Map{"list": fiber.Map{"name": "orphan batch"}}},
	} {
		status, body := apiCall(t, app, request.method, request.path, request.body)
		var response ErrorResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusServiceUnavailable || response.Error != "api_workspace_missing" ||
			response.Message != "API_WORKSPACE_ID names a workspace that does not exist" {
			t.Errorf("%s %s: %d %s", request.method, request.path, status, body)
		}
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM lists`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("missing workspace accepted writes: lists %d -> %d", before, after)
	}
}

func TestSharedPasswordAPIIgnoresMissingWorkspace(t *testing.T) {
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "api.db"))
	t.Setenv("MULTI_USER", "false")
	t.Setenv("API_TOKEN", "secret-token")
	t.Setenv("API_WORKSPACE_ID", "999999")
	db.Init()
	t.Cleanup(db.Close)
	app := fiber.New()
	Register(app)
	status, body := apiCall(t, app, "POST", "/api/v1/lists", fiber.Map{"name": "Home list"})
	var list db.List
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated || db.WorkspaceOfList(list.ID) != db.DefaultWorkspaceID {
		t.Fatalf("shared mode creation: %d %s", status, body)
	}
	if status, body := apiCall(t, app, "GET", "/api/v1/lists", nil); status != http.StatusOK {
		t.Fatalf("shared mode read: %d %s", status, body)
	}
}

func TestAPIWorkspaceLookupFailureReturnsDatabaseError(t *testing.T) {
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "api.db"))
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	t.Setenv("API_TOKEN", "secret-token")
	db.Init()
	t.Cleanup(db.Close)
	app := fiber.New()
	Register(app)
	if _, err := db.DB.Exec(`DROP TABLE workspaces`); err != nil {
		t.Fatal(err)
	}
	status, body := apiCall(t, app, "GET", "/api/v1/lists", nil)
	var response ErrorResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusInternalServerError || response.Error != "db_error" || response.Message != "Failed to fetch workspace" {
		t.Fatalf("database failure: %d %s", status, body)
	}
}

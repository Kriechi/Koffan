package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"shopping-list/db"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func newOfflineTestApp(t *testing.T) *fiber.App {
	t.Helper()
	initTestDatabase(t)
	if err := db.ClearWorkspaceData(db.DefaultWorkspaceID); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Get("/api/offline/snapshot", GetOfflineSnapshot)
	app.Post("/api/offline/sync", SyncOffline)
	return app
}

func postOfflineJSON(t *testing.T, app *fiber.App, body string, status int) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/offline/sync", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("status=%d, want%d: %s", response.StatusCode, status, data)
	}
	if status == http.StatusOK && response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("sync responses must not be cached")
	}
	return data
}

func TestOfflineSyncHTTPReplayAndConflicts(t *testing.T) {
	app := newOfflineTestApp(t)
	clientID, operationID := uuid.NewString(), uuid.NewString()
	body := `{"client_id":"` + clientID + `","operations":[{"id":"` + operationID + `","entity":"list","action":"create","entity_id":-1,"values":{"name":"Weekend"}}]}`
	first := postOfflineJSON(t, app, body, http.StatusOK)
	second := postOfflineJSON(t, app, body, http.StatusOK)
	var result, retried db.OfflineSyncResponse
	if err := json.Unmarshal(first, &result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &retried); err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.GeneratedAt <= 0 || retried.Snapshot.GeneratedAt <= 0 {
		t.Fatalf("snapshots must carry their generation time:\n%s\n%s", first, second)
	}
	// Only the generation time may differ between a request and its retry.
	result.Snapshot.GeneratedAt, retried.Snapshot.GeneratedAt = 0, 0
	if !reflect.DeepEqual(result, retried) {
		t.Fatalf("lost-ACK retry response changed:\n%s\n%s", first, second)
	}
	if len(result.Results) != 1 || result.Results[0].EntityID != -1 || result.Results[0].ServerID <= 0 || len(result.Snapshot.Lists) != 1 {
		t.Fatalf("wrong protocol response: %s", first)
	}
	changed := strings.Replace(body, "Weekend", "Different", 1)
	conflict := postOfflineJSON(t, app, changed, http.StatusConflict)
	if !bytes.Contains(conflict, []byte(operationID)) {
		t.Fatalf("conflict omitted operation ID: %s", conflict)
	}
	missingOp := uuid.NewString()
	missing := `{"client_id":"` + clientID + `","operations":[{"id":"` + missingOp + `","entity":"item","action":"update","entity_id":999999,"values":{"completed":true}}]}`
	conflict = postOfflineJSON(t, app, missing, http.StatusConflict)
	if !bytes.Contains(conflict, []byte(missingOp)) {
		t.Fatalf("missing-target conflict omitted operation ID: %s", conflict)
	}
}

func TestOfflineSyncHTTPRejectsMalformedEnvelopes(t *testing.T) {
	app := newOfflineTestApp(t)
	valid := `{"client_id":"` + uuid.NewString() + `","operations":[]}`
	for _, body := range []string{
		"", "null", `{`, `{}`, valid + `{}`, valid + `garbage`, strings.Replace(valid, `"operations":[]`, `"operations":[],"unexpected":true`, 1),
		`{"client_id":"` + uuid.NewString() + `","operations":[{"id":"` + uuid.NewString() + `","entity":"list","action":"create","entity_id":-1,"values":{"name":"X"},"unexpected":1}]}`,
	} {
		t.Run(strconv.Itoa(len(body))+body, func(t *testing.T) { postOfflineJSON(t, app, body, http.StatusBadRequest) })
	}
	postOfflineJSON(t, app, valid, http.StatusOK)
}

func TestOfflineSnapshotHTTPIncludesEveryListRegardlessOfActiveList(t *testing.T) {
	app := newOfflineTestApp(t)
	for _, name := range []string{"First phone", "Second phone"} {
		list, err := db.CreateList(db.DefaultWorkspaceID, name, "cart")
		if err != nil {
			t.Fatal(err)
		}
		section, err := db.CreateSectionForList(list.ID, name+" section")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateItem(section.ID, name+" item", "", 1); err != nil {
			t.Fatal(err)
		}
		if err := db.SetActiveList(list.ID); err != nil {
			t.Fatal(err)
		}
	}
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/offline/snapshot", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d, %s", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var snapshot db.OfflineSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Lists) != 2 || len(snapshot.Sections) != 2 || len(snapshot.Items) != 2 {
		t.Fatalf("snapshot depends on another phone's active list: %#v", snapshot)
	}
}

func TestOfflineSyncWebhookOutboxIsAtomicAndDoesNotDuplicateOnReplay(t *testing.T) {
	app := newOfflineTestApp(t)
	// An unreachable loopback destination keeps events in the durable outbox;
	// no listener or successful external delivery is needed for this test.
	configureTestWebhook(t, "http://127.0.0.1:1", "item.created,item.completed,item.deleted")
	clientID := uuid.NewString()
	operations := []map[string]interface{}{
		{"id": uuid.NewString(), "entity": "list", "action": "create", "entity_id": -1, "values": map[string]interface{}{"name": "Shopping"}},
		{"id": uuid.NewString(), "entity": "section", "action": "create", "entity_id": -1, "values": map[string]interface{}{"name": "Dairy", "list_id": -1}},
		{"id": uuid.NewString(), "entity": "item", "action": "create", "entity_id": -1, "values": map[string]interface{}{"name": "Milk", "section_id": -1}},
		{"id": uuid.NewString(), "entity": "item", "action": "update", "entity_id": -1, "values": map[string]interface{}{"completed": true}},
		{"id": uuid.NewString(), "entity": "list", "action": "delete", "entity_id": -1, "values": map[string]interface{}{}},
	}
	body, _ := json.Marshal(map[string]interface{}{"client_id": clientID, "operations": operations})
	postOfflineJSON(t, app, string(body), http.StatusOK)
	postOfflineJSON(t, app, string(body), http.StatusOK)
	rows, err := db.DB.Query("SELECT event,payload FROM webhook_outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var event string
		var payload []byte
		if err := rows.Scan(&event, &payload); err != nil {
			t.Fatal(err)
		}
		var decoded capturedItemWebhook
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Data.Item.Name != "Milk" || decoded.Data.Section.Name != "Dairy" || decoded.Data.List.Name != "Shopping" {
			t.Fatalf("lost cascade webhook context: %s", payload)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "item.created,item.completed,item.deleted" {
		t.Fatalf("events=%v", events)
	}
}

func TestOfflineSyncAcceptsBrowserDefaultsAndTemporaryChildChain(t *testing.T) {
	app := newOfflineTestApp(t)
	operations := []map[string]interface{}{
		{"id": uuid.NewString(), "entity": "list", "action": "create", "entity_id": -1, "values": map[string]interface{}{"name": "Browser groceries", "icon": "🛒", "sort_order": 0, "is_active": false, "show_completed": true}},
		{"id": uuid.NewString(), "entity": "section", "action": "create", "entity_id": -2, "values": map[string]interface{}{"name": "Dairy", "list_id": -1, "sort_order": 0, "sort_mode": "manual"}},
		{"id": uuid.NewString(), "entity": "item", "action": "create", "entity_id": -3, "values": map[string]interface{}{"name": "Milk", "section_id": -2, "sort_order": 0, "description": "", "quantity": 0, "completed": false, "uncertain": false}},
		{"id": uuid.NewString(), "entity": "item", "action": "update", "entity_id": -3, "values": map[string]interface{}{"name": "Oat milk", "quantity": 2, "description": "Unsweetened"}},
		{"id": uuid.NewString(), "entity": "item", "action": "update", "entity_id": -3, "values": map[string]interface{}{"completed": true}},
	}
	body, err := json.Marshal(map[string]interface{}{"client_id": uuid.NewString(), "operations": operations})
	if err != nil {
		t.Fatal(err)
	}
	data := postOfflineJSON(t, app, string(body), http.StatusOK)
	var response db.OfflineSyncResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	state := response.Snapshot
	if len(response.Results) != 5 || len(state.Lists) != 1 || len(state.Sections) != 1 || len(state.Items) != 1 {
		t.Fatalf("child chain failed: %s", data)
	}
	if state.Lists[0].IsActive || !state.Lists[0].ShowCompleted || state.Sections[0].ListID != state.Lists[0].ID || state.Items[0].SectionID != state.Sections[0].ID || state.Items[0].Name != "Oat milk" || state.Items[0].Quantity != 2 || !state.Items[0].Completed {
		t.Fatalf("browser defaults or queued edits changed: %s", data)
	}
	postOfflineJSON(t, app, string(body), http.StatusOK)
}

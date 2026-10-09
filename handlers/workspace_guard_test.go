package handlers

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"shopping-list/db"

	"github.com/gofiber/fiber/v2"
)

type guardHousehold struct {
	workspace db.Workspace
	user      *db.User
	list      *db.List
	section   *db.Section
	item      *db.Item
	template  *db.Template
	templItem *db.TemplateItem
	history   int64
}

func newGuardHousehold(t *testing.T, name string) guardHousehold {
	t.Helper()
	user, err := db.CreateUser(name, name, "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := db.CreateWorkspace(name, "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateList(workspace.ID, name+" list", "")
	if err != nil {
		t.Fatal(err)
	}
	section, err := db.CreateSectionForList(list.ID, name+" section")
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateItem(section.ID, name+" milk", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveItemHistory(workspace.ID, item.Name, section.ID); err != nil {
		t.Fatal(err)
	}
	template, err := db.CreateTemplate(workspace.ID, name+" template", "")
	if err != nil {
		t.Fatal(err)
	}
	templItem, err := db.AddTemplateItem(template.ID, "Section", "Bread", "")
	if err != nil {
		t.Fatal(err)
	}
	history, _ := db.GetItemHistoryList(workspace.ID)
	return guardHousehold{*workspace, user, list, section, item, template, templItem, history[0].ID}
}

// guardApp is the real authenticated route table with a stand-in for
// AuthMiddleware that picks the workspace from a test header.
func guardApp() *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		workspace, _ := strconv.ParseInt(c.Get("X-Test-Workspace"), 10, 64)
		userID, _ := strconv.ParseInt(c.Get("X-Test-User"), 10, 64)
		c.Locals(localWorkspace, workspace)
		if user, err := db.GetUserByID(userID); err == nil {
			c.Locals(localUser, user)
		}
		return c.Next()
	})
	RegisterRoutes(app)
	return app
}

func guardRequest(t *testing.T, app *fiber.App, who guardHousehold, method, path string, form url.Values) (int, string, http.Header) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("X-Test-Workspace", strconv.FormatInt(who.workspace.ID, 10))
	req.Header.Set("X-Test-User", strconv.FormatInt(who.user.ID, 10))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func TestRoutesRefuseAnotherWorkspacesData(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	app := guardApp()

	form := func(kv ...string) url.Values {
		values := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			values.Set(kv[i], kv[i+1])
		}
		return values
	}
	attacks := []struct {
		name, method, path string
		form               url.Values
	}{
		{"read item html", "GET", "/items/" + id(b.item.ID) + "/html", nil},
		{"toggle item", "POST", "/items/" + id(b.item.ID) + "/toggle", nil},
		{"edit item", "PUT", "/items/" + id(b.item.ID), form("name", "hijacked")},
		{"delete item", "DELETE", "/items/" + id(b.item.ID), nil},
		{"item quantity", "POST", "/items/" + id(b.item.ID) + "/quantity", form("delta", "1")},
		{"item uncertain", "POST", "/items/" + id(b.item.ID) + "/uncertain", nil},
		{"item move-up", "POST", "/items/" + id(b.item.ID) + "/move-up", nil},
		{"item version", "GET", "/api/item/" + id(b.item.ID) + "/version", nil},
		{"add item to foreign section", "POST", "/items", form("section_id", id(b.section.ID), "name", "planted")},
		{"move own item into foreign section", "POST", "/items/" + id(a.item.ID) + "/move", form("section_id", id(b.section.ID))},
		{"read section html", "GET", "/sections/" + id(b.section.ID) + "/html", nil},
		{"rename section", "PUT", "/sections/" + id(b.section.ID), form("name", "hijacked")},
		{"delete section", "DELETE", "/sections/" + id(b.section.ID), nil},
		{"check all in section", "POST", "/sections/" + id(b.section.ID) + "/check-all", nil},
		{"sort section", "POST", "/sections/" + id(b.section.ID) + "/sort-mode", form("sort_mode", "alphabetical")},
		{"list sections of foreign list", "GET", "/sections/list?list_id=" + id(b.list.ID) + "&format=json", nil},
		{"list data of foreign list", "GET", "/api/data?list_id=" + id(b.list.ID), nil},
		{"rename list", "PUT", "/lists/" + id(b.list.ID), form("name", "hijacked")},
		{"delete list", "DELETE", "/lists/" + id(b.list.ID), nil},
		{"activate list", "POST", "/lists/" + id(b.list.ID) + "/activate", nil},
		{"activate list via GET", "GET", "/lists/" + id(b.list.ID) + "/activate", nil},
		{"move list", "POST", "/lists/" + id(b.list.ID) + "/move-up", nil},
		{"toggle completed on list", "POST", "/lists/" + id(b.list.ID) + "/toggle-completed", nil},
		{"export foreign list", "GET", "/export/list/" + id(b.list.ID), nil},
		{"read template", "GET", "/templates/" + id(b.template.ID), nil},
		{"edit template", "PUT", "/templates/" + id(b.template.ID), form("name", "hijacked")},
		{"delete template", "DELETE", "/templates/" + id(b.template.ID), nil},
		{"add item to template", "POST", "/templates/" + id(b.template.ID) + "/items", form("section_name", "S", "name", "planted")},
		{"apply foreign template", "POST", "/templates/" + id(b.template.ID) + "/apply", nil},
		{"edit foreign template item via own template", "PUT", "/templates/" + id(a.template.ID) + "/items/" + id(b.templItem.ID), form("section_name", "S", "name", "hijacked")},
		{"delete foreign template item via own template", "DELETE", "/templates/" + id(a.template.ID) + "/items/" + id(b.templItem.ID), nil},
		{"delete foreign history", "DELETE", "/api/history/" + id(b.history), nil},
	}
	// Deleting a list, section or template answered 200 even when it was already
	// gone, so a second device (or a retry) keeps working. Foreign ids get the
	// same empty 200 and nothing is deleted.
	quietDeletes := map[string]bool{"delete section": true, "delete list": true, "delete template": true,
		"delete foreign template item via own template": true}
	for _, attack := range attacks {
		want := http.StatusNotFound
		if quietDeletes[attack.name] {
			want = http.StatusOK
		}
		status, _, _ := guardRequest(t, app, a, attack.method, attack.path, attack.form)
		if status != want {
			t.Errorf("%s: %s %s returned %d, want %d", attack.name, attack.method, attack.path, status, want)
		}
	}

	// Nothing of workspace B changed.
	snapshot, err := db.GetOfflineSnapshot(b.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Lists) != 1 || snapshot.Lists[0].Name != "beta list" ||
		len(snapshot.Sections) != 1 || snapshot.Sections[0].Name != "beta section" ||
		len(snapshot.Items) != 1 || snapshot.Items[0].Name != "beta milk" || snapshot.Items[0].Completed {
		t.Fatalf("workspace B was modified by the attacks: %#v", snapshot)
	}
	if templates, _ := db.GetAllTemplates(b.workspace.ID); len(templates) != 1 || templates[0].Name != "beta template" || len(templates[0].Items) != 1 || templates[0].Items[0].Name != "Bread" {
		t.Fatalf("workspace B templates were modified: %#v", templates)
	}
	if history, _ := db.GetItemHistoryList(b.workspace.ID); len(history) != 1 {
		t.Fatal("workspace B history was modified")
	}
	if a.item.SectionID != a.section.ID {
		t.Fatal("test setup is off")
	}
	if moved, _ := db.GetItemByID(a.item.ID); moved.SectionID != a.section.ID {
		t.Fatal("an item was moved into another workspace")
	}
}

func TestRoutesStillWorkInsideTheOwnWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := guardApp()

	// These handlers answer without rendering a template (the test app has no
	// view engine), so a 200 proves the guard let the request through.
	for _, request := range []struct {
		method, path string
		form         url.Values
	}{
		{"GET", "/api/item/" + id(a.item.ID) + "/version", nil},
		{"GET", "/api/data?list_id=" + id(a.list.ID), nil},
		{"GET", "/api/offline/snapshot", nil},
		{"GET", "/stats", nil},
	} {
		if status, body, _ := guardRequest(t, app, a, request.method, request.path, request.form); status != http.StatusOK {
			t.Errorf("%s %s returned %d (%s)", request.method, request.path, status, body)
		}
	}

	// Adding an item through the form lands in the caller's own workspace. The
	// response needs a template the test app lacks, but the item is stored first.
	guardRequest(t, app, a, "POST", "/items", url.Values{"section_id": {id(a.section.ID)}, "name": {"Eggs"}})
	if snapshot, err := db.GetOfflineSnapshot(a.workspace.ID); err != nil || len(snapshot.Items) != 2 {
		t.Fatalf("snapshot: %v, items = %d", err, len(snapshot.Items))
	}

	// A list page for another workspace's list sends the visitor home.
	b := newGuardHousehold(t, "beta")
	status, _, headers := guardRequest(t, app, a, "GET", "/lists/"+id(b.list.ID), nil)
	if status != http.StatusFound || headers.Get("Location") != "/" {
		t.Errorf("foreign list page: %d -> %q", status, headers.Get("Location"))
	}
}

func TestReadEndpointsOnlyReturnTheCallersWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	app := guardApp()

	for _, path := range []string{
		"/api/offline/snapshot",
		"/api/suggestions?q=milk",
		"/api/suggestions?limit=100",
		"/api/history",
		"/lists?format=json",
		"/templates?format=json",
		"/export?format=json",
		"/export/preview",
	} {
		status, body, _ := guardRequest(t, app, a, "GET", path, nil)
		if status != http.StatusOK {
			t.Errorf("%s returned %d", path, status)
			continue
		}
		if strings.Contains(body, "beta") {
			t.Errorf("%s leaked workspace B: %s", path, body)
		}
		if path != "/export/preview" && !strings.Contains(body, "alpha") {
			t.Errorf("%s did not return the caller's own data: %s", path, body)
		}
	}
	_ = b
}

func TestOfflineSyncEndpointIsScoped(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	app := guardApp()

	payload := `{"client_id":"3f6c2d3e-8f0a-4e4e-9d5b-0a1b2c3d4e5f","operations":[{"id":"4a6c2d3e-8f0a-4e4e-9d5b-0a1b2c3d4e5f","entity":"item","action":"update","entity_id":` +
		id(b.item.ID) + `,"values":{"name":"hijacked"}}]}`
	req := httptest.NewRequest("POST", "/api/offline/sync", strings.NewReader(payload))
	req.Header.Set("X-Test-Workspace", id(a.workspace.ID))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if item, _ := db.GetItemByID(b.item.ID); item.Name != "beta milk" {
		t.Fatalf("foreign item was changed to %q", item.Name)
	}
}

func TestClearingADatabaseOnlyAffectsTheCallersWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	if err := db.ClearWorkspaceData(a.workspace.ID); err != nil {
		t.Fatal(err)
	}
	if lists, _ := db.GetAllLists(b.workspace.ID); len(lists) != 1 {
		t.Fatal("workspace B lost its data")
	}
	if lists, _ := db.GetAllLists(a.workspace.ID); len(lists) != 0 {
		t.Fatal("workspace A kept its data")
	}
}

func TestWorkspaceIDFailsClosedWithAccounts(t *testing.T) {
	var got int64
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error { got = WorkspaceID(c); return c.SendStatus(200) })

	t.Setenv("MULTI_USER", "true")
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	resp.Body.Close()
	if got != -1 {
		t.Fatalf("with accounts a request without a workspace got %d, want -1 (matches nothing)", got)
	}
	t.Setenv("MULTI_USER", "")
	resp, _ = app.Test(httptest.NewRequest("GET", "/", nil))
	resp.Body.Close()
	if got != db.DefaultWorkspaceID {
		t.Fatalf("single-password mode got workspace %d", got)
	}
}

func TestDeletingSomethingAlreadyGoneStillSucceeds(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := guardApp()
	for _, path := range []string{"/lists/9999", "/sections/9999", "/templates/9999", "/templates/" + id(a.template.ID) + "/items/9999"} {
		if status, _, _ := guardRequest(t, app, a, "DELETE", path, nil); status != http.StatusOK {
			t.Errorf("DELETE %s returned %d, want 200 (idempotent)", path, status)
		}
	}
	// ... and the same delete twice on a real list.
	if status, _, _ := guardRequest(t, app, a, "DELETE", "/lists/"+id(a.list.ID), nil); status != http.StatusOK {
		t.Fatalf("first delete: %d", status)
	}
	if status, _, _ := guardRequest(t, app, a, "DELETE", "/lists/"+id(a.list.ID), nil); status != http.StatusOK {
		t.Fatalf("second delete: %d", status)
	}
}

func TestDefaultModeDeletesAreIdempotentToo(t *testing.T) {
	// No MULTI_USER: the single implicit workspace, exactly like before.
	initTestDatabase(t)
	list, err := db.CreateList(db.DefaultWorkspaceID, "Solo", "")
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	RegisterRoutes(app)
	for i := 0; i < 2; i++ {
		resp, err := app.Test(httptest.NewRequest("DELETE", "/lists/"+id(list.ID), nil))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete #%d returned %d", i+1, resp.StatusCode)
		}
	}
}

func multipartImport(t *testing.T, who guardHousehold, filename, contents string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", filename)
	io.WriteString(part, contents)
	writer.WriteField("conflict_resolution", "skip")
	writer.Close()
	req := httptest.NewRequest("POST", "/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Test-Workspace", id(who.workspace.ID))
	req.Header.Set("X-Test-User", id(who.user.ID))
	resp, err := guardApp().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestImportStaysInsideTheImportersWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	// Both workspaces have a section named "alpha section"; beta's history
	// entries must attach to its own, and entries without a section must not
	// end up in workspace 1 or in alpha.
	if _, err := db.CreateSectionForList(b.list.ID, "alpha section"); err != nil {
		t.Fatal(err)
	}
	payload := `{"version":"1.0","app":"koffan","data":{
	  "lists":[{"name":"Imported","icon":"x","sections":[{"name":"alpha section","items":[{"name":"Imported milk"}]}]}],
	  "templates":[{"name":"Imported template","items":[{"section_name":"S","name":"T"}]}],
	  "history":[{"name":"remembered in section","last_section":"alpha section","usage_count":3},
	             {"name":"remembered without section","last_section":"","usage_count":2}]}}`
	resp := multipartImport(t, b, "backup.json", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("import returned %d: %s", resp.StatusCode, data)
	}

	names := func(workspaceID int64) map[string]bool {
		out := map[string]bool{}
		history, _ := db.GetItemHistoryList(workspaceID)
		for _, h := range history {
			out[h.Name] = true
		}
		return out
	}
	for _, name := range []string{"remembered in section", "remembered without section"} {
		if !names(b.workspace.ID)[name] {
			t.Errorf("%q missing from the importer's history", name)
		}
		if names(a.workspace.ID)[name] || names(db.DefaultWorkspaceID)[name] {
			t.Errorf("%q leaked into another workspace's history", name)
		}
	}
	if lists, _ := db.GetAllLists(a.workspace.ID); len(lists) != 1 {
		t.Errorf("alpha has %d lists after beta's import", len(lists))
	}
	if lists, _ := db.GetAllLists(b.workspace.ID); len(lists) != 2 {
		t.Errorf("beta has %d lists after its import", len(lists))
	}
	if templates, _ := db.GetAllTemplates(a.workspace.ID); len(templates) != 1 {
		t.Errorf("alpha has %d templates after beta's import", len(templates))
	}
	// The suggestions join must not expose alpha's section either.
	for _, s := range mustSuggestions(t, b.workspace.ID) {
		if s.Name == "remembered in section" && s.LastSectionID != 0 && db.WorkspaceOfSection(s.LastSectionID) != b.workspace.ID {
			t.Errorf("suggestion points at a section of another workspace: %#v", s)
		}
	}
}

func mustSuggestions(t *testing.T, workspaceID int64) []db.ItemSuggestion {
	t.Helper()
	suggestions, err := db.GetAllItemSuggestions(workspaceID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return suggestions
}

// The list_id query parameter names a list the caller wants to look at. It
// used to be trusted on the routes that only guard the section id, so a valid
// section of your own combined with another workspace's list id read that
// list's sections (and statistics).
func TestListIDQueryParameterCannotReachAnotherWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	app := guardApp()
	foreign := "list_id=" + id(b.list.ID)
	own := "list_id=" + id(a.list.ID)
	secretSection := b.section.Name

	do := func(method, path string, form url.Values, modal bool) (int, string) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("X-Test-Workspace", id(a.workspace.ID))
		req.Header.Set("X-Test-User", id(a.user.ID))
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if modal {
			req.Header.Set("HX-Target", "manage-sections-list")
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	spare, err := db.CreateSectionForList(a.list.ID, "spare")
	if err != nil {
		t.Fatal(err)
	}

	attacks := []struct {
		name, method, path string
		form               url.Values
		modal              bool
	}{
		{"stats of a foreign list", "GET", "/stats?" + foreign, nil, false},
		{"sections of a foreign list", "GET", "/sections/list?" + foreign + "&format=json", nil, false},
		{"all data of a foreign list", "GET", "/api/data?" + foreign, nil, false},
		{"rename own section, foreign list_id", "PUT", "/sections/" + id(a.section.ID) + "?" + foreign, url.Values{"name": {"renamed"}}, true},
		{"move own section up, foreign list_id", "POST", "/sections/" + id(a.section.ID) + "/move-up?" + foreign, nil, true},
		{"move own section down, foreign list_id", "POST", "/sections/" + id(a.section.ID) + "/move-down?" + foreign, nil, true},
		{"batch delete own section, foreign list_id", "POST", "/sections/batch-delete?" + foreign, url.Values{"ids": {id(spare.ID)}}, true},
	}
	for _, attack := range attacks {
		status, body := do(attack.method, attack.path, attack.form, attack.modal)
		if status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", attack.name, status)
		}
		if strings.Contains(body, secretSection) || strings.Contains(body, "total_items") {
			t.Errorf("%s leaked the other workspace's data: %s", attack.name, body)
		}
	}
	// Refused means refused: nothing of the caller's own may have changed either.
	if got, _ := db.GetSectionByID(a.section.ID); got == nil || got.Name != a.section.Name {
		t.Errorf("a refused rename still changed the section: %+v", got)
	}
	if _, err := db.GetSectionByID(spare.ID); err != nil {
		t.Errorf("a refused batch delete still deleted the section: %v", err)
	}

	// The same calls with the caller's own list keep working.
	if status, _ := do("GET", "/stats?"+own, nil, false); status != http.StatusOK {
		t.Errorf("own stats: %d", status)
	}
	// (Without HX-Target: rendering the modal needs the template engine, which
	// this test app does not have.)
	if status, body := do("POST", "/sections/"+id(a.section.ID)+"/move-up?"+own, nil, false); status != http.StatusOK || strings.Contains(body, secretSection) {
		t.Errorf("own section move: %d %s", status, body)
	}
	if status, _ := do("GET", "/sections/list?"+own+"&format=json", nil, false); status != http.StatusOK {
		t.Errorf("own sections: %d", status)
	}
}

// A batch delete goes through for the sections that are the caller's. Ones
// that are already gone (another device deleted them) and ones of another
// workspace are skipped in the same way, so one stale id no longer makes the
// whole request fail and nothing reveals whether a foreign id exists.
func TestBatchDeleteSkipsSectionsThatAreNotTheCallers(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	app := guardApp()
	spare, err := db.CreateSectionForList(a.list.ID, "spare")
	if err != nil {
		t.Fatal(err)
	}
	ids := id(spare.ID) + "," + id(b.section.ID) + ",999999"
	// (Rendering the modal needs the template engine, so only the effect is checked.)
	guardRequest(t, app, a, "POST", "/sections/batch-delete", url.Values{"ids": {ids}})

	if _, err := db.GetSectionByID(spare.ID); err == nil {
		t.Error("the caller's own section was not deleted")
	}
	if _, err := db.GetSectionByID(b.section.ID); err != nil {
		t.Error("another workspace's section was deleted")
	}
	if _, err := db.GetSectionByID(a.section.ID); err != nil {
		t.Error("a section that was not in the batch was deleted")
	}
	// Only foreign or missing ids: nothing happens, and it is not an error.
	status, _, _ := guardRequest(t, app, a, "POST", "/sections/batch-delete", url.Values{"ids": {id(b.section.ID) + ",999999"}})
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		t.Errorf("only foreign or missing ids answered %d, want the quiet modal answer", status)
	}
	if _, err := db.GetSectionByID(b.section.ID); err != nil {
		t.Error("another workspace's section was deleted")
	}
	// Nothing parseable is still a bad request.
	if status, _, _ := guardRequest(t, app, a, "POST", "/sections/batch-delete", url.Values{"ids": {"abc"}}); status != http.StatusBadRequest {
		t.Errorf("no valid ids answered %d, want 400", status)
	}
}

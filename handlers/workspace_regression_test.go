package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"shopping-list/db"
	"shopping-list/i18n"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"
)

func TestTemplateFromListStaysInWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	h := newGuardHousehold(t, "private")
	if h.workspace.ID != 2 {
		t.Fatalf("workspace = %d, want 2", h.workspace.ID)
	}
	if err := db.SetActiveList(h.list.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := db.CreateItem(h.section.ID, "already bought", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ToggleItemCompleted(completed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTemplate(1, "Home template", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE templates SET sort_order = 100 WHERE workspace_id = 1`); err != nil {
		t.Fatal(err)
	}
	engine := html.New("", ".html")
	engine.Templates, err = template.ParseFiles("../templates/partials/template_item.html")
	if err != nil {
		t.Fatal(err)
	}
	engine.Loaded = true
	app := fiber.New(fiber.Config{Views: engine})
	app.Use(AuthMiddleware)
	RegisterRoutes(app)
	r := authDo(t, app, sessionFor(t, h.user, h.workspace.ID), "POST", "/templates/from-list",
		url.Values{"name": {"Private weekly plan"}, "description": {"Open items"}}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("creation: %d %s", r.status, r.body)
	}
	var templateID int64
	if err := db.DB.QueryRow(`SELECT id FROM templates WHERE name = 'Private weekly plan'`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	created, err := db.GetTemplateByID(templateID)
	if err != nil {
		t.Fatal(err)
	}
	if db.WorkspaceOfTemplate(templateID) != 2 || created.SortOrder != 1 || created.Description != "Open items" ||
		len(created.Items) != 1 || created.Items[0].Name != h.item.Name {
		t.Errorf("template escaped workspace or copied the wrong items/order: %#v, workspace=%d", created, db.WorkspaceOfTemplate(templateID))
	}
	viewer, err := db.CreateUser("home-viewer", "Viewer", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMember(1, viewer.ID, db.RoleMember); err != nil {
		t.Fatal(err)
	}
	session := sessionFor(t, viewer, 1)
	list := authDo(t, app, session, "GET", "/templates?format=json", nil, nil)
	if list.status != http.StatusOK || strings.Contains(list.body, created.Name) || strings.Contains(list.body, h.item.Name) {
		t.Errorf("Home sees private template: %#v", list)
	}
	if r := authDo(t, app, session, "GET", "/templates/"+id(templateID), nil, nil); r.status != http.StatusNotFound {
		t.Errorf("Home reads private template: %d %s", r.status, r.body)
	}
}

func TestAPIWorkspaceDeletionPolicy(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	t.Setenv("API_TOKEN", "secret-token")
	initTestDatabase(t)
	pinned := newGuardHousehold(t, "pinned")
	other := newGuardHousehold(t, "other")
	t.Setenv("API_WORKSPACE_ID", id(pinned.workspace.ID))
	app := authApp()
	session := sessionFor(t, pinned.user, pinned.workspace.ID)
	r := authDo(t, app, session, "POST", "/workspaces/"+id(pinned.workspace.ID)+"/delete", nil, nil)
	if r.status != http.StatusSeeOther || r.location != "/workspaces?error=cannot_delete_api" {
		t.Errorf("pinned deletion: %d %q", r.status, r.location)
	}
	if _, err := db.GetWorkspace(pinned.workspace.ID); err != nil {
		t.Errorf("pinned workspace was removed: %v", err)
	}
	if _, err := db.GetListByID(pinned.list.ID); err != nil {
		t.Errorf("pinned list was removed: %v", err)
	}
	r = authDo(t, app, sessionFor(t, other.user, other.workspace.ID), "POST", "/workspaces/"+id(other.workspace.ID)+"/delete", nil, nil)
	if r.status != http.StatusOK {
		t.Errorf("other deletion: %d %s", r.status, r.body)
	}
	if _, err := db.GetWorkspace(other.workspace.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("other workspace remains: %v", err)
	}
	t.Setenv("API_TOKEN", "")
	r = authDo(t, app, session, "POST", "/workspaces/"+id(pinned.workspace.ID)+"/delete", nil, nil)
	if r.status != http.StatusOK {
		t.Errorf("disabled API deletion: %d %s", r.status, r.body)
	}
	if _, err := db.GetWorkspace(pinned.workspace.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("workspace remains with API disabled: %v", err)
	}
}

func TestWorkspacePageProtectsAPIAndConfirmsAccounts(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	t.Setenv("API_TOKEN", "secret-token")
	initTestDatabase(t)
	h := newGuardHousehold(t, "pinned")
	t.Setenv("API_WORKSPACE_ID", id(h.workspace.ID))
	if _, err := db.DB.Exec(`UPDATE users SET is_admin = TRUE WHERE id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := i18n.Init(); err != nil {
		t.Fatal(err)
	}
	i18n.SetDefaultLang("en")
	engine := html.New("", ".html")
	var err error
	engine.Templates, err = template.New("workspaces").Funcs(template.FuncMap{
		"initial": func(name string) string { return strings.ToUpper(string([]rune(name)[0])) },
		"toJSON": func(value interface{}) (template.JS, error) {
			data, err := json.Marshal(value)
			return template.JS(data), err
		},
		"dict": func(values ...interface{}) map[string]interface{} {
			data := make(map[string]interface{})
			for i := 0; i < len(values); i += 2 {
				data[values[i].(string)] = values[i+1]
			}
			return data
		},
	}).ParseFiles("../templates/workspaces.html")
	if err != nil {
		t.Fatal(err)
	}
	engine.Loaded = true
	app := fiber.New(fiber.Config{Views: engine})
	app.Use(AuthMiddleware)
	RegisterRoutes(app)
	session := sessionFor(t, h.user, h.workspace.ID)
	deleteAction := `action="/workspaces/` + id(h.workspace.ID) + `/delete"`
	page := authDo(t, app, session, "GET", "/workspaces", nil, nil)
	if page.status != http.StatusOK || strings.Contains(page.body, deleteAction) {
		t.Errorf("pinned workspace offers deletion: %d %s", page.status, page.body)
	}
	t.Setenv("API_TOKEN", "")
	page = authDo(t, app, session, "GET", "/workspaces", nil, nil)
	if page.status != http.StatusOK || !strings.Contains(page.body, deleteAction) {
		t.Errorf("disabled API hides deletion: %d", page.status)
	}
	r := authDo(t, app, session, "POST", "/admin/users",
		url.Values{"username": {"new-person"}, "password": {"correct horse battery"}}, nil)
	if r.status != http.StatusSeeOther || r.location != "/workspaces?notice=account_created" {
		t.Errorf("account confirmation redirect: %d %q", r.status, r.location)
	}
	page = authDo(t, app, session, "GET", "/workspaces?notice=account_created", nil, nil)
	if page.status != http.StatusOK || !strings.Contains(page.body, `data-notice="account_created"`) ||
		!strings.Contains(page.body, `t('workspaces.notice_' + $el.dataset.notice)`) {
		t.Errorf("account confirmation is absent from the rendered notice: %d", page.status)
	}
	if text := i18n.Get("en", "workspaces.notice_account_created"); text != "Account created. Give the person their username and password." {
		t.Errorf("account confirmation text = %q", text)
	}
}

func TestConcurrentAdministratorsCannotDeleteLastAdmin(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	a, err := db.CreateUser("admin-a", "A", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateUser("admin-b", "B", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("ordinary", "Ordinary", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	sa, sb := sessionFor(t, a, 0), sessionFor(t, b, 0)
	app := fiber.New()
	app.Use(AuthMiddleware)
	admitted, release := make(chan struct{}), make(chan struct{})
	// B authenticates before A deletes B, then its authorized handler resumes.
	app.Use(func(c *fiber.Ctx) error {
		if c.Cookies(SessionCookieName) == sb {
			close(admitted)
			<-release
		}
		return c.Next()
	})
	RegisterRoutes(app)
	done := make(chan authResult, 1)
	go func() { done <- authDo(t, app, sb, "POST", "/admin/users/"+id(a.ID)+"/delete", nil, nil) }()
	<-admitted
	first := authDo(t, app, sa, "POST", "/admin/users/"+id(b.ID)+"/delete", nil, nil)
	close(release)
	second := <-done
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = TRUE`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || first.status != http.StatusSeeOther || first.location != "/workspaces" ||
		second.status != http.StatusSeeOther || second.location != "/workspaces?error=last_admin" {
		t.Fatalf("admins=%d, redirects=%d %q, %d %q", count, first.status, first.location, second.status, second.location)
	}
}

func TestOversizedPasswordLoginTimingDoesNotRevealUsername(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	newGuardHousehold(t, "known")
	original := loginLimiter
	loginLimiter = nil
	t.Cleanup(func() { loginLimiter = original })
	app := authApp()
	var known, unknown time.Duration
	for i := 0; i < 5; i++ {
		for _, username := range []string{"known", "unknown"} {
			start := time.Now()
			r := authDo(t, app, "", "POST", "/login", url.Values{
				"username": {username}, "password": {strings.Repeat("x", 73)},
			}, nil)
			elapsed := time.Since(start)
			if r.status != http.StatusFound || r.location != "/login?error=1" {
				t.Fatalf("login response: %#v", r)
			}
			if username == "known" {
				known += elapsed
			} else {
				unknown += elapsed
			}
		}
	}
	t.Logf("five oversized logins: existing=%s, unknown=%s", known, unknown)
	if known > 4*unknown || unknown > 4*known {
		t.Fatalf("login durations differ by more than a factor of 4: existing=%s, unknown=%s", known, unknown)
	}
}

package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"shopping-list/db"

	"github.com/gofiber/fiber/v2"
)

// authApp is the real stack: login, AuthMiddleware and the route table, with
// sessions stored in the database exactly as in production.
func authApp() *fiber.App {
	app := fiber.New()
	app.Post("/login", Login)
	app.Post("/logout", Logout)
	app.Use(AuthMiddleware)
	RegisterRoutes(app)
	return app
}

// sessionFor signs a user in without going through the form.
func sessionFor(t *testing.T, user *db.User, workspaceID int64) string {
	t.Helper()
	sessionID := generateSessionID()
	if err := db.CreateUserSession(sessionID, time.Now().Add(time.Hour).Unix(), user.ID, workspaceID); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

type authResult struct {
	status   int
	body     string
	location string
	header   http.Header
}

func authDo(t *testing.T, app *fiber.App, sessionID, method, path string, form url.Values, headers map[string]string) authResult {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if sessionID != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Password routes run bcrypt twice, which under -race takes longer than
	// Fiber's default one-second test timeout.
	resp, err := app.Test(req, 30_000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return authResult{resp.StatusCode, string(data), resp.Header.Get("Location"), resp.Header}
}

func TestLoginCreatesAUserSessionInTheirWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := authApp()

	bad := authDo(t, app, "", "POST", "/login", url.Values{"username": {"alpha"}, "password": {"wrong password"}}, nil)
	if bad.status != http.StatusFound || bad.location != "/login?error=1" || bad.header.Get("Set-Cookie") != "" {
		t.Fatalf("wrong password: %d %q cookie=%q", bad.status, bad.location, bad.header.Get("Set-Cookie"))
	}
	// The old shared-password form must not work with accounts.
	shared := authDo(t, app, "", "POST", "/login", url.Values{"password": {"shopping123"}}, nil)
	if shared.location != "/login?error=1" {
		t.Fatalf("shared password accepted with accounts: %d %q", shared.status, shared.location)
	}

	ok := authDo(t, app, "", "POST", "/login", url.Values{"username": {"ALPHA"}, "password": {"correct horse battery"}}, nil)
	if ok.status != http.StatusFound || ok.location != "/" {
		t.Fatalf("login: %d %q", ok.status, ok.location)
	}
	cookie := ok.header.Get("Set-Cookie")
	if !strings.HasPrefix(cookie, SessionCookieName+"=") || !strings.Contains(cookie, "HttpOnly") {
		t.Fatalf("session cookie = %q", cookie)
	}
	sessionID := strings.TrimPrefix(strings.SplitN(cookie, ";", 2)[0], SessionCookieName+"=")
	session, err := db.GetSession(sessionID)
	if err != nil || session.UserID != a.user.ID || session.WorkspaceID != a.workspace.ID {
		t.Fatalf("session = %#v (%v), want user %d workspace %d", session, err, a.user.ID, a.workspace.ID)
	}
}

func TestSessionsWithoutAUserAreRejectedWithAccounts(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	newGuardHousehold(t, "alpha")
	app := authApp()

	// A shared-password session from before accounts were enabled.
	legacy := generateSessionID()
	if err := db.CreateSession(legacy, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	result := authDo(t, app, legacy, "GET", "/api/offline/snapshot", nil, nil)
	if result.status != http.StatusFound || result.location != "/login" {
		t.Fatalf("legacy session: %d %q", result.status, result.location)
	}
	if _, err := db.GetSession(legacy); err == nil {
		t.Fatal("the legacy session should have been deleted")
	}
	if !strings.Contains(result.header.Get("Set-Cookie"), SessionCookieName+"=;") {
		t.Fatalf("the cookie was not cleared: %q", result.header.Get("Set-Cookie"))
	}
	// HTMX requests get the HX-Redirect form of the same answer.
	other := generateSessionID()
	db.CreateSession(other, time.Now().Add(time.Hour).Unix())
	htmx := authDo(t, app, other, "POST", "/items", url.Values{"name": {"x"}}, map[string]string{"HX-Request": "true"})
	if htmx.status != http.StatusUnauthorized || htmx.header.Get("HX-Redirect") != "/login" {
		t.Fatalf("htmx: %d %q", htmx.status, htmx.header.Get("HX-Redirect"))
	}
}

func TestPeopleWithoutAWorkspaceOnlyReachTheWorkspacePages(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	newGuardHousehold(t, "alpha")
	loner, err := db.CreateUser("loner", "Loner", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.CreateUser("boss", "Boss", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	app := authApp()
	lonerSession, adminSession := sessionFor(t, loner, 0), sessionFor(t, admin, 0)

	if r := authDo(t, app, lonerSession, "GET", "/api/offline/snapshot", nil, nil); r.status != http.StatusFound || r.location != "/workspaces" {
		t.Fatalf("browser navigation: %d %q", r.status, r.location)
	}
	if r := authDo(t, app, lonerSession, "POST", "/items", url.Values{"section_id": {"1"}, "name": {"x"}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("write without a workspace: %d", r.status)
	}
	// They can create a workspace to get started...
	if r := authDo(t, app, lonerSession, "POST", "/workspaces", url.Values{"name": {"Mine"}}, nil); r.status != http.StatusOK {
		t.Fatalf("create workspace: %d %s", r.status, r.body)
	}
	if workspaces, _ := db.WorkspacesForUser(loner.ID); len(workspaces) != 1 || workspaces[0].Role != db.RoleOwner {
		t.Fatalf("workspaces = %#v", workspaces)
	}
	// ... and an administrator without one can still manage accounts.
	r := authDo(t, app, adminSession, "POST", "/admin/users", url.Values{"username": {"newbie"}, "password": {"long enough pw"}}, nil)
	if r.status != http.StatusSeeOther || r.location != "/workspaces?notice=account_created" {
		t.Fatalf("admin without a workspace creating an account: %d %q %s", r.status, r.location, r.body)
	}
	if _, err := db.GetUserByUsername("newbie"); err != nil {
		t.Fatalf("account was not created: %v", err)
	}
}

func TestAPageForAnotherWorkspaceIsRefused(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	second, err := db.CreateWorkspace("second", "", a.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := authApp()
	sessionID := sessionFor(t, a.user, a.workspace.ID)
	header := func(workspaceID int64) map[string]string {
		return map[string]string{WorkspaceHeader: id(workspaceID)}
	}

	if r := authDo(t, app, sessionID, "GET", "/api/offline/snapshot", nil, header(a.workspace.ID)); r.status != http.StatusOK {
		t.Fatalf("matching workspace: %d", r.status)
	}
	if r := authDo(t, app, sessionID, "GET", "/api/offline/snapshot", nil, nil); r.status != http.StatusOK {
		t.Fatalf("no header (page navigation, older client): %d", r.status)
	}

	// The user switches workspace (in another tab). The first tab's requests,
	// which still name the old workspace, are refused instead of writing to the
	// new one.
	if r := authDo(t, app, sessionID, "POST", "/workspaces/"+id(second.ID)+"/switch", nil, nil); r.status != http.StatusOK {
		t.Fatalf("switch: %d", r.status)
	}
	for _, request := range []struct{ method, path string }{
		{"GET", "/api/offline/snapshot"},
		{"POST", "/lists"},
		{"POST", "/api/offline/sync"},
	} {
		r := authDo(t, app, sessionID, request.method, request.path, url.Values{"name": {"stray list"}}, header(a.workspace.ID))
		if r.status != http.StatusConflict || r.header.Get(WorkspaceMismatchHeader) != "1" {
			t.Errorf("%s %s from the stale tab: %d (mismatch header %q)", request.method, request.path, r.status, r.header.Get(WorkspaceMismatchHeader))
		}
	}
	if lists, _ := db.GetAllLists(second.ID); len(lists) != 0 {
		t.Fatalf("the stale tab created a list in the new workspace: %#v", lists)
	}
	if r := authDo(t, app, sessionID, "GET", "/api/offline/snapshot", nil, header(second.ID)); r.status != http.StatusOK {
		t.Fatalf("a reloaded page names the new workspace: %d", r.status)
	}
}

func TestWorkspaceRoles(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	owner := newGuardHousehold(t, "owner")
	member, _ := db.CreateUser("member", "Member", "correct horse battery", false)
	stranger, _ := db.CreateUser("stranger", "Stranger", "correct horse battery", false)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	if err := db.AddMember(owner.workspace.ID, member.ID, db.RoleMember); err != nil {
		t.Fatal(err)
	}
	app := authApp()
	ws := owner.workspace.ID
	ownerSession, memberSession := sessionFor(t, owner.user, ws), sessionFor(t, member, ws)
	strangerSession := sessionFor(t, stranger, 0)
	adminSession := sessionFor(t, admin, 0)
	base := "/workspaces/" + id(ws)

	// Members cannot manage the workspace...
	for name, request := range map[string]struct {
		path string
		form url.Values
	}{
		"add a person":         {base + "/members", url.Values{"username": {"stranger"}}},
		"remove the owner":     {base + "/members/" + id(owner.user.ID) + "/remove", nil},
		"delete the workspace": {base + "/delete", nil},
	} {
		if r := authDo(t, app, memberSession, "POST", request.path, request.form, nil); r.status != http.StatusForbidden {
			t.Errorf("member tried to %s: %d, want 403", name, r.status)
		}
	}
	if db.MemberRole(ws, stranger.ID) != "" || db.MemberRole(ws, owner.user.ID) != db.RoleOwner {
		t.Fatal("a member changed the membership")
	}
	// ... nor wipe its data.
	token := func(session string) string {
		r := authDo(t, app, session, "GET", "/api/database/csrf-token", nil, nil)
		var out struct {
			Token string `json:"csrf_token"`
		}
		json.Unmarshal([]byte(r.body), &out)
		return out.Token
	}
	clear := func(session string) int {
		req := httptest.NewRequest("POST", "/api/database/clear", strings.NewReader(`{"csrf_token":"`+token(session)+`","confirmation":"DELETE"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: session})
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := clear(memberSession); status != http.StatusForbidden {
		t.Fatalf("a member cleared the workspace: %d", status)
	}
	if lists, _ := db.GetAllLists(ws); len(lists) != 1 {
		t.Fatal("the workspace lost its data")
	}

	// People outside it cannot even tell it exists.
	for _, path := range []string{base + "/members", base + "/switch", base + "/delete"} {
		if r := authDo(t, app, strangerSession, "POST", path, url.Values{"username": {"stranger"}}, nil); r.status != http.StatusNotFound {
			t.Errorf("stranger POST %s: %d, want 404", path, r.status)
		}
	}
	if db.MemberRole(ws, stranger.ID) != "" {
		t.Fatal("a stranger joined the workspace")
	}

	// The owner can add people, and cannot remove the last owner (themselves).
	if r := authDo(t, app, ownerSession, "POST", base+"/members", url.Values{"username": {"stranger"}}, nil); r.status != http.StatusSeeOther {
		t.Fatalf("owner adding a person: %d", r.status)
	}
	if db.MemberRole(ws, stranger.ID) != db.RoleMember {
		t.Fatal("the person was not added")
	}
	if r := authDo(t, app, ownerSession, "POST", base+"/members", url.Values{"username": {"nobody"}}, nil); !strings.Contains(r.location, "error=user_not_found") {
		t.Fatalf("unknown username: %q", r.location)
	}
	if r := authDo(t, app, ownerSession, "POST", base+"/members/"+id(owner.user.ID)+"/remove", nil, nil); !strings.Contains(r.location, "error=last_owner") {
		t.Fatalf("removing the last owner: %d %q", r.status, r.location)
	}
	// Re-adding themselves without "owner" must not demote them either.
	authDo(t, app, ownerSession, "POST", base+"/members", url.Values{"username": {"owner"}}, nil)
	if db.MemberRole(ws, owner.user.ID) != db.RoleOwner {
		t.Fatal("re-adding the owner as a member demoted them")
	}

	// A member can leave on their own.
	if r := authDo(t, app, memberSession, "POST", base+"/members/"+id(member.ID)+"/remove", nil, nil); r.status != http.StatusOK {
		t.Fatalf("leaving: %d", r.status)
	}
	if db.MemberRole(ws, member.ID) != "" {
		t.Fatal("the member is still in the workspace")
	}

	// Administrators manage accounts; ordinary users do not.
	form := url.Values{"username": {"fresh"}, "password": {"long enough pw"}}
	if r := authDo(t, app, ownerSession, "POST", "/admin/users", form, nil); r.status != http.StatusForbidden {
		t.Fatalf("a non-admin created an account: %d", r.status)
	}
	if _, err := db.GetUserByUsername("fresh"); err == nil {
		t.Fatal("the account exists")
	}
	if r := authDo(t, app, adminSession, "POST", "/admin/users", form, nil); r.status != http.StatusSeeOther || strings.Contains(r.location, "error") {
		t.Fatalf("admin creating an account: %d %q", r.status, r.location)
	}
	for name, bad := range map[string]url.Values{
		"password_too_short": {"username": {"short"}, "password": {"tiny"}},
		"user_exists":        {"username": {"FRESH"}, "password": {"long enough pw"}},
		"invalid_username":   {"username": {"has space"}, "password": {"long enough pw"}},
	} {
		if r := authDo(t, app, adminSession, "POST", "/admin/users", bad, nil); !strings.Contains(r.location, "error="+name) {
			t.Errorf("expected error=%s, got %q", name, r.location)
		}
	}
	// An administrator can add themselves to any workspace, and then clear it.
	if r := authDo(t, app, adminSession, "POST", base+"/members", url.Values{"username": {"boss"}}, nil); r.status != http.StatusSeeOther {
		t.Fatalf("admin adding themselves: %d", r.status)
	}
	if db.MemberRole(ws, admin.ID) != db.RoleMember {
		t.Fatal("the administrator is not a member")
	}
	if status := clear(ownerSession); status != http.StatusOK {
		t.Fatalf("the owner could not clear their own workspace: %d", status)
	}
	if lists, _ := db.GetAllLists(ws); len(lists) != 0 {
		t.Fatal("the owner's clear did not remove the lists")
	}
}

func TestWorkspaceIconsCanBeChosenAndEdited(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	owner := newGuardHousehold(t, "owner")
	member, _ := db.CreateUser("member", "Member", "correct horse battery", false)
	stranger, _ := db.CreateUser("stranger", "Stranger", "correct horse battery", false)
	db.AddMember(owner.workspace.ID, member.ID, db.RoleMember)
	app := authApp()
	ws := owner.workspace.ID
	ownerSession, memberSession := sessionFor(t, owner.user, ws), sessionFor(t, member, ws)
	strangerSession := sessionFor(t, stranger, 0)
	update := "/workspaces/" + id(ws) + "/update"
	current := func() db.Workspace {
		w, err := db.GetWorkspace(ws)
		if err != nil {
			t.Fatal(err)
		}
		return *w
	}

	// Creating: the chosen icon is stored, a missing one falls back to the default.
	if r := authDo(t, app, ownerSession, "POST", "/workspaces", url.Values{"name": {"Grandma"}, "icon": {"👵"}}, nil); r.status != http.StatusOK {
		t.Fatalf("create: %d", r.status)
	}
	if r := authDo(t, app, ownerSession, "POST", "/workspaces", url.Values{"name": {"Plain"}}, nil); r.status != http.StatusOK {
		t.Fatalf("create without icon: %d", r.status)
	}
	created := map[string]string{}
	workspaces, _ := db.WorkspacesForUser(owner.user.ID)
	for _, w := range workspaces {
		created[w.Name] = w.Icon
	}
	if created["Grandma"] != "👵" || created["Plain"] != db.DefaultWorkspaceIcon {
		t.Fatalf("created icons: %v", created)
	}
	if r := authDo(t, app, ownerSession, "POST", "/workspaces", url.Values{"name": {"Long"}, "icon": {strings.Repeat("x", MaxIconLength+1)}}, nil); !strings.Contains(r.location, "error=icon_too_long") {
		t.Fatalf("oversized icon: %d %q", r.status, r.location)
	}

	// Editing is for owners only, and strangers cannot tell the workspace exists.
	form := url.Values{"name": {"Renamed"}, "icon": {"🎓"}}
	if r := authDo(t, app, memberSession, "POST", update, form, nil); r.status != http.StatusForbidden {
		t.Errorf("member edited the workspace: %d, want 403", r.status)
	}
	if r := authDo(t, app, strangerSession, "POST", update, form, nil); r.status != http.StatusNotFound {
		t.Errorf("stranger edited the workspace: %d, want 404", r.status)
	}
	if w := current(); w.Name != "owner" || w.Icon != db.DefaultWorkspaceIcon {
		t.Fatalf("a non-owner changed the workspace: %+v", w)
	}

	if r := authDo(t, app, ownerSession, "POST", update, form, nil); r.status != http.StatusSeeOther || r.location != "/workspaces" {
		t.Fatalf("owner edit: %d %q", r.status, r.location)
	}
	if w := current(); w.Name != "Renamed" || w.Icon != "🎓" {
		t.Fatalf("after the owner's edit: %+v", w)
	}
	for name, bad := range map[string]url.Values{
		"empty name":    {"name": {"   "}, "icon": {"🎓"}},
		"long name":     {"name": {strings.Repeat("n", MaxWorkspaceNameLength+1)}, "icon": {"🎓"}},
		"oversize icon": {"name": {"Renamed"}, "icon": {strings.Repeat("x", MaxIconLength+1)}},
	} {
		if r := authDo(t, app, ownerSession, "POST", update, bad, nil); !strings.Contains(r.location, "error=") {
			t.Errorf("%s was accepted: %d %q", name, r.status, r.location)
		}
	}
	if w := current(); w.Name != "Renamed" || w.Icon != "🎓" {
		t.Fatalf("a rejected edit changed the workspace: %+v", w)
	}
}

func TestAccountSessionsStopWorkingWhenAccountsAreSwitchedOff(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := authApp()
	account := sessionFor(t, a.user, a.workspace.ID)

	if r := authDo(t, app, account, "GET", "/api/database/csrf-token", nil, nil); r.status != http.StatusOK {
		t.Fatalf("with accounts on: %d", r.status)
	}

	// Accounts are switched off again: the shared password is back, and a
	// session that was issued to an account must not get in without it.
	t.Setenv("MULTI_USER", "")
	if r := authDo(t, app, account, "GET", "/api/database/csrf-token", nil, nil); r.status == http.StatusOK {
		t.Fatal("an account session opened the shared workspace after accounts were switched off")
	}
	if _, err := db.GetSession(account); err == nil {
		t.Fatal("the account session should have been deleted")
	}

	// A session from the shared-password login keeps working.
	shared := "shared-password-session"
	if err := db.CreateSession(shared, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if r := authDo(t, app, shared, "GET", "/api/database/csrf-token", nil, nil); r.status != http.StatusOK {
		t.Fatalf("shared-password session: %d", r.status)
	}
}

func TestAPIWorkspaceOnlyCountsWithAccounts(t *testing.T) {
	t.Setenv("API_WORKSPACE_ID", "5")
	t.Setenv("MULTI_USER", "")
	if got := APIWorkspaceID(); got != db.DefaultWorkspaceID {
		t.Fatalf("without accounts the API must stay on the default workspace, got %d", got)
	}
	t.Setenv("MULTI_USER", "true")
	if got := APIWorkspaceID(); got != 5 {
		t.Fatalf("with accounts API_WORKSPACE_ID should apply, got %d", got)
	}
	t.Setenv("API_WORKSPACE_ID", "abc")
	if got := APIWorkspaceID(); got != db.DefaultWorkspaceID {
		t.Fatalf("an unusable value falls back to the default workspace, got %d", got)
	}
}

func TestOnlyKnownWorkspaceMessagesAreShown(t *testing.T) {
	if knownCode(workspaceErrors, "x") != "" || knownCode(workspaceErrors, "") != "" || knownCode(workspaceNotices, "failed") != "" {
		t.Fatal("an unknown ?error= or ?notice= must not reach the page")
	}
	data, err := os.ReadFile("../i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var en struct {
		Workspaces map[string]string `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &en); err != nil {
		t.Fatal(err)
	}
	for code := range workspaceErrors {
		if knownCode(workspaceErrors, code) != code {
			t.Errorf("%s is not passed through", code)
		}
		if en.Workspaces["error_"+code] == "" {
			t.Errorf("workspaces.error_%s has no English text", code)
		}
	}
	for code := range workspaceNotices {
		if en.Workspaces["notice_"+code] == "" {
			t.Errorf("workspaces.notice_%s has no English text", code)
		}
	}
}

func TestAccountCreationExplainsWhyAUsernameIsRejected(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	app := authApp()
	session := sessionFor(t, admin, db.DefaultWorkspaceID)
	create := func(username string) string {
		r := authDo(t, app, session, "POST", "/admin/users", url.Values{"username": {username}, "password": {"correct horse battery"}}, nil)
		return r.location
	}
	if loc := create(strings.Repeat("u", MaxUsernameLength+1)); !strings.Contains(loc, "error=username_too_long") {
		t.Errorf("too long: %q", loc)
	}
	if loc := create("two words"); !strings.Contains(loc, "error=invalid_username") {
		t.Errorf("with a space: %q", loc)
	}
	if loc := create("fine-name"); strings.Contains(loc, "error=") {
		t.Errorf("valid name rejected: %q", loc)
	}
}

func TestAccountFormsRejectInputThatCannotBeTrustedLater(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	app := authApp()
	session := sessionFor(t, admin, db.DefaultWorkspaceID)
	create := func(username, display, password string) string {
		form := url.Values{"username": {username}, "display_name": {display}, "password": {password}}
		return authDo(t, app, session, "POST", "/admin/users", form, nil).location
	}
	good := "correct horse battery"
	for name, loc := range map[string]string{
		"zero-width space in the username": create("ann​", "", good),
		"control character":                create("a\x01b", "", good),
		"comma":                            create("a,b", "", good),
	} {
		if !strings.Contains(loc, "error=invalid_username") {
			t.Errorf("%s: %q", name, loc)
		}
	}
	if loc := create("longname", strings.Repeat("n", MaxDisplayNameLength+1), good); !strings.Contains(loc, "error=name_too_long") {
		t.Errorf("long display name: %q", loc)
	}
	if loc := create("longpass", "", strings.Repeat("p", db.MaxPasswordBytes+1)); !strings.Contains(loc, "error=password_too_long") {
		t.Errorf("long password: %q", loc)
	}
	// Invisible characters in a display name are dropped, not stored.
	if loc := create("cleaned", "Ann​\u0007", good); strings.Contains(loc, "error=") {
		t.Fatalf("display name with invisible characters: %q", loc)
	}
	user, err := db.GetUserByUsername("cleaned")
	if err != nil || user.DisplayName != "Ann" {
		t.Fatalf("stored display name: %+v (%v)", user, err)
	}
}

func TestDeletingAnAccountThatSolelyOwnsASharedWorkspaceIsRefused(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	owner := newGuardHousehold(t, "owner")
	member, _ := db.CreateUser("member", "Member", "correct horse battery", false)
	db.AddMember(owner.workspace.ID, member.ID, db.RoleMember)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	app := authApp()
	session := sessionFor(t, admin, db.DefaultWorkspaceID)

	if r := authDo(t, app, session, "POST", "/admin/users/"+id(owner.user.ID)+"/delete", nil, nil); !strings.Contains(r.location, "error=sole_owner") {
		t.Fatalf("deleting the only owner: %d %q", r.status, r.location)
	}
	if _, err := db.GetUserByID(owner.user.ID); err != nil {
		t.Fatal("the account was deleted anyway")
	}
	if r := authDo(t, app, session, "POST", "/admin/users/"+id(member.ID)+"/delete", nil, nil); strings.Contains(r.location, "error=") {
		t.Fatalf("deleting a plain member: %q", r.location)
	}
}

// SameSite=Lax is the session cookie's only defence against another site
// making the browser post to Koffan, so with accounts (where one request can
// mint an administrator) the browser's own Sec-Fetch-Site is honoured too.
func TestBrowserMarkedCrossSiteWritesAreRefusedWithAccounts(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	db.AddMember(db.DefaultWorkspaceID, admin.ID, db.RoleOwner)
	app := authApp()
	session := sessionFor(t, admin, db.DefaultWorkspaceID)
	form := func(name string) url.Values {
		return url.Values{"username": {name}, "password": {"correct horse battery"}, "is_admin": {"on"}}
	}
	for _, site := range []string{"cross-site", "same-site"} {
		r := authDo(t, app, session, "POST", "/admin/users", form("evil-"+site), map[string]string{"Sec-Fetch-Site": site})
		if r.status != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %s answered %d, want 403", site, r.status)
		}
		if _, err := db.GetUserByUsername("evil-" + site); err == nil {
			t.Errorf("a %s request created an account", site)
		}
	}
	// Same-origin pages, direct navigation and programs that send no such
	// header keep working, and reading is never blocked.
	for _, site := range []string{"same-origin", "none", ""} {
		headers := map[string]string{}
		if site != "" {
			headers["Sec-Fetch-Site"] = site
		}
		r := authDo(t, app, session, "POST", "/admin/users", form("ok-"+site+"x"), headers)
		if r.status != http.StatusSeeOther || strings.Contains(r.location, "error=") {
			t.Errorf("Sec-Fetch-Site %q: %d %q", site, r.status, r.location)
		}
	}
	if r := authDo(t, app, session, "GET", "/api/database/csrf-token", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusOK {
		t.Errorf("a cross-site read was blocked: %d", r.status)
	}
}

func TestSignedInResponsesAreNotCacheable(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := authApp()
	r := authDo(t, app, sessionFor(t, a.user, a.workspace.ID), "GET", "/api/database/csrf-token", nil, nil)
	if got := r.header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control %q, want private, no-store", got)
	}
}

func TestPeopleCanChangeTheirOwnPassword(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	useFakeSockets(t)
	originalLimiter := loginLimiter
	loginLimiter = nil
	t.Cleanup(func() { loginLimiter = originalLimiter })
	a := newGuardHousehold(t, "alpha")
	app := authApp()
	here := sessionFor(t, a.user, a.workspace.ID)
	phone := sessionFor(t, a.user, a.workspace.ID)
	later := time.Now().Add(time.Hour).Unix()
	hereSocket := connectFakeSocket(a.workspace.ID, a.user.ID, here, later)
	phoneSocket := connectFakeSocket(a.workspace.ID, a.user.ID, phone, later)
	change := func(current, next string) authResult {
		return authDo(t, app, here, "POST", "/account/password", url.Values{"current_password": {current}, "new_password": {next}}, nil)
	}

	if r := change("wrong password", "brand new secret"); !strings.Contains(r.location, "error=wrong_password") {
		t.Fatalf("a wrong current password: %d %q", r.status, r.location)
	}
	if r := change("correct horse battery", "short"); !strings.Contains(r.location, "error=password_too_short") {
		t.Fatalf("a short new password: %q", r.location)
	}
	if r := change("correct horse battery", strings.Repeat("x", db.MaxPasswordBytes+1)); !strings.Contains(r.location, "error=password_too_long") {
		t.Fatalf("a new password bcrypt would truncate: %q", r.location)
	}
	if _, err := db.Authenticate("alpha", "correct horse battery"); err != nil {
		t.Fatal("a refused change altered the password")
	}

	if r := change("correct horse battery", "brand new secret"); r.location != "/workspaces?notice=password_changed" {
		t.Fatalf("changing the password: %d %q", r.status, r.location)
	}
	if _, err := db.Authenticate("alpha", "brand new secret"); err != nil {
		t.Fatal("the new password does not work")
	}
	if _, err := db.Authenticate("alpha", "correct horse battery"); err == nil {
		t.Fatal("the old password still works")
	}
	if _, err := db.GetSession(here); err != nil {
		t.Fatal("changing the password signed out the device it was changed on")
	}
	if _, err := db.GetSession(phone); err == nil {
		t.Fatal("other devices should be signed out")
	}
	if hereSocket.closed() || !phoneSocket.closed() {
		t.Fatal("only the other devices' live connections should close")
	}

	t.Setenv("MULTI_USER", "false")
	shared := generateSessionID()
	if err := db.CreateSession(shared, later); err != nil {
		t.Fatal(err)
	}
	if r := authDo(t, app, shared, "POST", "/account/password", url.Values{}, nil); r.status != http.StatusNotFound {
		t.Fatalf("without MULTI_USER there are no accounts to change a password for, got %d", r.status)
	}
}

// Guessing the current password through a stolen session counts against the
// same limit as the login form.
func TestWrongCurrentPasswordsAreRateLimited(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	originalLimiter := loginLimiter
	loginLimiter = &LoginRateLimiter{
		config:   RateLimitConfig{MaxAttempts: 2, WindowDuration: time.Minute, LockoutDuration: time.Minute},
		attempts: map[string]*LoginAttempt{},
	}
	t.Cleanup(func() { loginLimiter = originalLimiter })
	a := newGuardHousehold(t, "alpha")
	app := authApp()
	session := sessionFor(t, a.user, a.workspace.ID)
	change := func(current string) string {
		return authDo(t, app, session, "POST", "/account/password", url.Values{"current_password": {current}, "new_password": {"brand new secret"}}, nil).location
	}
	for i := 0; i < 2; i++ {
		if got := change("guess"); !strings.Contains(got, "error=wrong_password") {
			t.Fatalf("guess %d: %q", i+1, got)
		}
	}
	if got := change("guess"); !strings.Contains(got, "error=rate_limited") {
		t.Fatalf("the guess over the limit: %q", got)
	}
	if got := change("correct horse battery"); !strings.Contains(got, "error=rate_limited") {
		t.Fatalf("while locked out even the right password is refused: %q", got)
	}
	if _, err := db.Authenticate("alpha", "correct horse battery"); err != nil {
		t.Fatal("the password changed during the lockout")
	}
}

func TestAdministratorsCanResetPasswords(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	useFakeSockets(t)
	a := newGuardHousehold(t, "alpha")
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	db.AddMember(db.DefaultWorkspaceID, admin.ID, db.RoleOwner)
	app := authApp()
	adminSession := sessionFor(t, admin, db.DefaultWorkspaceID)
	memberSession := sessionFor(t, a.user, a.workspace.ID)
	memberSocket := connectFakeSocket(a.workspace.ID, a.user.ID, memberSession, time.Now().Add(time.Hour).Unix())
	reset := func(session string, userID int64, password string) authResult {
		return authDo(t, app, session, "POST", "/admin/users/"+id(userID)+"/password", url.Values{"password": {password}}, nil)
	}

	if r := reset(memberSession, admin.ID, "taken over now"); r.status != http.StatusForbidden {
		t.Fatalf("a member reset someone's password: %d %q", r.status, r.location)
	}
	if r := reset(adminSession, a.user.ID, "short"); !strings.Contains(r.location, "error=password_too_short") {
		t.Fatalf("a short password: %q", r.location)
	}
	if r := reset(adminSession, 9999, "brand new secret"); !strings.Contains(r.location, "error=user_not_found") {
		t.Fatalf("an unknown account: %q", r.location)
	}

	if r := reset(adminSession, a.user.ID, "brand new secret"); r.location != "/workspaces?notice=password_reset" {
		t.Fatalf("resetting a password: %d %q", r.status, r.location)
	}
	if _, err := db.Authenticate("alpha", "brand new secret"); err != nil {
		t.Fatal("the new password does not work")
	}
	if _, err := db.GetSession(memberSession); err == nil {
		t.Fatal("the account should be signed out everywhere")
	}
	if !memberSocket.closed() {
		t.Fatal("the account's live connections should close")
	}

	// Resetting your own password keeps you signed in on this device.
	if r := reset(adminSession, admin.ID, "another new secret"); r.location != "/workspaces?notice=password_reset" {
		t.Fatalf("resetting your own password: %q", r.location)
	}
	if _, err := db.GetSession(adminSession); err != nil {
		t.Fatal("resetting your own password signed you out")
	}
}

// Someone who is in no workspace yet can still reach their account settings.
func TestPeopleWithoutAWorkspaceCanChangeTheirPassword(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	originalLimiter := loginLimiter
	loginLimiter = nil
	t.Cleanup(func() { loginLimiter = originalLimiter })
	loner, _ := db.CreateUser("loner", "Loner", "correct horse battery", false)
	app := authApp()
	r := authDo(t, app, sessionFor(t, loner, 0), "POST", "/account/password",
		url.Values{"current_password": {"correct horse battery"}, "new_password": {"brand new secret"}}, nil)
	if r.location != "/workspaces?notice=password_changed" {
		t.Fatalf("%d %q", r.status, r.location)
	}
}

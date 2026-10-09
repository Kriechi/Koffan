package handlers

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"shopping-list/db"

	"github.com/gofiber/fiber/v2"
)

func TestCrossOriginAccountWrites(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	admin, err := db.CreateUser("admin", "Admin", "correct horse battery", true)
	if err != nil {
		t.Fatal(err)
	}
	app := authApp()
	session := sessionFor(t, admin, 0)
	for i, tc := range []struct {
		name, host, site, origin, forwardedHost string
		status                                  int
	}{
		{name: "same origin metadata", site: "same-origin", status: 303},
		{name: "direct navigation metadata", site: "none", status: 303},
		{name: "same site metadata", site: "same-site", status: 403},
		{name: "cross site metadata", site: "cross-site", status: 403},
		{name: "unknown metadata", site: "bogus", status: 403},
		{name: "no browser headers", status: 303},
		{name: "matching origin", origin: "http://koffan.example.com", status: 303},
		{name: "foreign origin", origin: "http://evil.example.com", status: 403},
		{name: "matching LAN port", host: "192.168.1.10:8080", origin: "http://192.168.1.10:8080", status: 303},
		{name: "foreign LAN port", host: "192.168.1.10:8080", origin: "http://192.168.1.10:9000", status: 403},
		{name: "forwarded host", host: "koffan:8080", origin: "https://shop.example.com", forwardedHost: "shop.example.com", status: 303},
		{name: "first forwarded host trimmed", host: "koffan:8080", origin: "https://shop.example.com", forwardedHost: " SHOP.EXAMPLE.COM , proxy.internal", status: 303},
		{name: "later forwarded host ignored", host: "koffan:8080", origin: "https://shop.example.com", forwardedHost: "proxy.internal, shop.example.com", status: 403},
		{name: "request host still accepted", origin: "http://koffan.example.com", forwardedHost: "proxy.internal", status: 303},
		{name: "null origin", origin: "null", status: 403},
		{name: "malformed origin", origin: "http://%zz", status: 403},
		{name: "hostless origin", origin: "http:///koffan.example.com", status: 403},
		{name: "case insensitive origin", origin: "HTTP://KOFFAN.EXAMPLE.COM", status: 303},
		{name: "same origin metadata takes precedence", site: "same-origin", origin: "http://evil.example.com", status: 303},
		{name: "none metadata takes precedence", site: "none", origin: "null", status: 303},
		{name: "same site metadata takes precedence", site: "same-site", origin: "http://koffan.example.com", status: 403},
		{name: "unknown metadata takes precedence", site: "bogus", origin: "http://koffan.example.com", status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = "koffan.example.com"
			}
			headers := map[string]string{}
			for key, value := range map[string]string{"Sec-Fetch-Site": tc.site, "Origin": tc.origin, "X-Forwarded-Host": tc.forwardedHost} {
				if value != "" {
					headers[key] = value
				}
			}
			username := fmt.Sprintf("created-%d", i)
			form := url.Values{"username": {username}, "password": {"correct horse battery"}, "is_admin": {"on"}}
			var logs bytes.Buffer
			originalOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(originalOutput) })
			r := authDo(t, app, session, "POST", "http://"+host+"/admin/users", form, headers)
			if r.status != tc.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tc.status, r.body)
			}
			user, err := db.GetUserByUsername(username)
			if tc.status == http.StatusForbidden {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("refused request created an account or lookup failed: %#v, %v", user, err)
				}
			} else if err != nil || !user.IsAdmin || strings.Contains(r.location, "error=") {
				t.Fatalf("allowed request did not create administrator: %#v, %v, redirect %q", user, err, r.location)
			}
			if tc.status == http.StatusForbidden && tc.site == "" {
				if strings.Count(logs.String(), "\n") != 1 {
					t.Fatalf("want one rejection log line, got %q", logs.String())
				}
				for _, field := range []string{`method="POST"`, `path="/admin/users"`, fmt.Sprintf("Origin=%q", tc.origin), fmt.Sprintf("Host=%q", host), "X-Forwarded-Host=", "proxy_set_header Host $host"} {
					if !strings.Contains(logs.String(), field) {
						t.Errorf("log lacks %q: %s", field, logs.String())
					}
				}
				if strings.Contains(logs.String(), session) || strings.Contains(logs.String(), form.Get("password")) || strings.Contains(logs.String(), username) {
					t.Fatal("rejection log contains cookie or form data")
				}
			}
		})
	}
}

func TestCrossOriginWorkspaceMembershipIsRefused(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	owner := newGuardHousehold(t, "owner")
	attacker := newGuardHousehold(t, "attacker")
	before, err := db.WorkspaceMembers(owner.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := authDo(t, authApp(), sessionFor(t, owner.user, owner.workspace.ID), "POST",
		"http://koffan.example.com/workspaces/"+id(owner.workspace.ID)+"/members",
		url.Values{"username": {attacker.user.Username}, "role": {"owner"}}, map[string]string{"Origin": "http://evil.example.com"})
	if r.status != http.StatusForbidden {
		t.Fatalf("membership request answered %d, want 403", r.status)
	}
	after, err := db.WorkspaceMembers(owner.workspace.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("membership changed: before %#v, after %#v, error %v", before, after, err)
	}
}

func TestCrossOriginReadsAreAllowed(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	owner := newGuardHousehold(t, "owner")
	app := authApp()
	session := sessionFor(t, owner.user, owner.workspace.ID)
	// The existing auth harness has no template engine, so use the lists JSON page.
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		r := authDo(t, app, session, method, "http://koffan.example.com/lists?format=json", nil, map[string]string{"Origin": "http://evil.example.com"})
		want := http.StatusOK
		if method == "OPTIONS" {
			want = http.StatusMethodNotAllowed
		}
		if r.status != want {
			t.Fatalf("%s answered %d, want %d: %s", method, r.status, want, r.body)
		}
		if method == "GET" && !strings.Contains(r.body, owner.list.Name) {
			t.Fatalf("GET did not serve the user's lists: %s", r.body)
		}
	}
}

func TestCrossOriginWebSocketHandshakes(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	useFakeSockets(t)
	owner := newGuardHousehold(t, "owner")
	session := sessionFor(t, owner.user, owner.workspace.ID)
	app := authApp()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.Listener(listener) }()
	t.Cleanup(func() {
		if err := app.ShutdownWithTimeout(3 * time.Second); err != nil {
			t.Error(err)
		}
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name, origin string
		status       int
	}{
		{"foreign origin", "http://evil.example.com", http.StatusForbidden},
		{"matching origin", "http://koffan.example.com", http.StatusSwitchingProtocols},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err = fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: koffan.example.com\r\nOrigin: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nCookie: %s=%s\r\n\r\n", tc.origin, SessionCookieName, session)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status {
				t.Fatalf("handshake answered %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == http.StatusSwitchingProtocols {
				for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
					clientsMu.RLock()
					registered := len(clients)
					clientsMu.RUnlock()
					if registered == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("accepted socket never registered")
					}
				}
			}
		})
	}
}

func TestCrossOriginLogin(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("DISABLE_AUTH", "false")
	initTestDatabase(t)
	user := newGuardHousehold(t, "account")
	originalLimiter := loginLimiter
	loginLimiter = &LoginRateLimiter{
		config:   RateLimitConfig{MaxAttempts: 2, WindowDuration: time.Minute, LockoutDuration: time.Minute},
		attempts: map[string]*LoginAttempt{},
	}
	t.Cleanup(func() { loginLimiter = originalLimiter })
	app := fiber.New()
	app.Post("/login", LoginRateLimitMiddleware, Login)
	for _, tc := range []struct {
		name, site, origin, password string
		status                       int
	}{
		{name: "cross site metadata", site: "cross-site", status: 403},
		{name: "foreign origin without metadata", origin: "http://evil.example.com", status: 403},
		{name: "refused invalid credentials do not count", origin: "http://evil.example.com", password: "wrong password", status: 403},
		{name: "same origin metadata", site: "same-origin", status: 302},
		{name: "matching origin without metadata", origin: "http://koffan.example.com", status: 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before int
			if err := db.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&before); err != nil {
				t.Fatal(err)
			}
			password := tc.password
			if password == "" {
				password = "correct horse battery"
			}
			r := authDo(t, app, "", "POST", "http://koffan.example.com/login",
				url.Values{"username": {user.user.Username}, "password": {password}},
				map[string]string{"Sec-Fetch-Site": tc.site, "Origin": tc.origin})
			if r.status != tc.status {
				t.Fatalf("login answered %d, want %d: %s", r.status, tc.status, r.body)
			}
			var after int
			if err := db.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&after); err != nil {
				t.Fatal(err)
			}
			cookies := (&http.Response{Header: r.header}).Cookies()
			if tc.status == http.StatusForbidden {
				if after != before {
					t.Fatalf("refused login changed session count from %d to %d", before, after)
				}
				for _, cookie := range cookies {
					if cookie.Name == SessionCookieName {
						t.Fatal("refused login set a session cookie")
					}
				}
			} else {
				if after != before+1 || r.location != "/" {
					t.Fatalf("allowed login: sessions before=%d after=%d, redirect=%q", before, after, r.location)
				}
				if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value == "" {
					t.Fatalf("allowed login did not set a session cookie: %#v", cookies)
				}
				stored, err := db.GetSession(cookies[0].Value)
				if err != nil || stored.UserID != user.user.ID {
					t.Fatalf("allowed login session: %#v, %v", stored, err)
				}
			}
			loginLimiter.mu.RLock()
			attempts := len(loginLimiter.attempts)
			loginLimiter.mu.RUnlock()
			if attempts != 0 {
				t.Fatalf("login recorded attempts for %d IPs", attempts)
			}
		})
	}
}

func TestCrossOriginSharedPasswordModeIsUnchanged(t *testing.T) {
	t.Setenv("MULTI_USER", "")
	if err := os.Unsetenv("MULTI_USER"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISABLE_AUTH", "false")
	t.Setenv("APP_PASSWORD", "shared secret password")
	initTestDatabase(t)
	originalLimiter := loginLimiter
	loginLimiter = nil
	t.Cleanup(func() { loginLimiter = originalLimiter })
	app := authApp()
	r := authDo(t, app, "", "POST", "http://koffan.example.com/login",
		url.Values{"password": {"shared secret password"}},
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example.com"})
	if r.status != http.StatusFound || r.location != "/" {
		t.Fatalf("shared password login answered %d, redirect %q", r.status, r.location)
	}
	cookies := (&http.Response{Header: r.header}).Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value == "" {
		t.Fatalf("shared password login did not set a session cookie: %#v", cookies)
	}
	session := cookies[0].Value
	stored, err := db.GetSession(session)
	if err != nil || stored.UserID != 0 {
		t.Fatalf("shared password session: %#v, %v", stored, err)
	}
	list, err := db.CreateList(db.DefaultWorkspaceID, "Shared list", "")
	if err != nil {
		t.Fatal(err)
	}
	r = authDo(t, app, session, "POST", "http://koffan.example.com/lists/"+id(list.ID)+"/activate", nil,
		map[string]string{"Origin": "http://evil.example.com"})
	if r.status != http.StatusFound || r.location != "/lists/"+id(list.ID) {
		t.Fatalf("shared password write answered %d, redirect %q", r.status, r.location)
	}
	active, err := db.GetActiveList(db.DefaultWorkspaceID)
	if err != nil || active == nil || active.ID != list.ID {
		t.Fatalf("shared password write did not activate list: %#v, %v", active, err)
	}
}

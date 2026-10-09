package handlers

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shopping-list/db"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

// useFakeSockets swaps the registry of live WebSocket clients for an empty one
// for the duration of the test.
func useFakeSockets(t *testing.T) {
	t.Helper()
	clientsMu.Lock()
	original := clients
	clients = map[*websocket.Conn]*webSocketClient{}
	clientsMu.Unlock()
	t.Cleanup(func() {
		clientsMu.Lock()
		registered := clients
		clients = original
		clientsMu.Unlock()
		for _, client := range registered {
			_ = client.close()
			<-client.stopped
		}
	})
}

type fakeSocket struct {
	client *webSocketClient
	writer *recordingWebSocketWriter
}

// connectFakeSocket registers a socket as WebSocketHandler would for a page
// signed in with sessionID and viewing workspaceID.
func connectFakeSocket(workspaceID, userID int64, sessionID string, expiresAt int64) fakeSocket {
	writer := &recordingWebSocketWriter{messages: make(chan []byte, 8)}
	client := newWebSocketClient(writer)
	client.workspaceID, client.userID, client.sessionID, client.expiresAt = workspaceID, userID, sessionID, expiresAt
	clientsMu.Lock()
	clients[new(websocket.Conn)] = client
	clientsMu.Unlock()
	return fakeSocket{client, writer}
}

func (s fakeSocket) closed() bool {
	select {
	case <-s.client.done:
		return true
	default:
		return false
	}
}

// next returns the next message the socket received, or "" if none arrives.
func (s fakeSocket) next(wait time.Duration) string {
	select {
	case data := <-s.writer.messages:
		return string(data)
	case <-time.After(wait):
		return ""
	}
}

func TestBroadcastsOnlyReachTheirWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	useFakeSockets(t)
	later := time.Now().Add(time.Hour).Unix()
	alpha := connectFakeSocket(2, 1, "a", later)
	beta := connectFakeSocket(3, 2, "b", later)
	nowhere := connectFakeSocket(-1, 3, "c", later)
	expired := connectFakeSocket(2, 4, "d", time.Now().Add(-time.Minute).Unix())

	broadcast(2, "item_created", map[string]string{"name": "alpha milk"})

	got := alpha.next(time.Second)
	var message WebSocketMessage
	if err := json.Unmarshal([]byte(got), &message); err != nil || message.Type != "item_created" || !strings.Contains(got, "alpha milk") {
		t.Fatalf("the workspace's own socket should get the event with its data, got %q", got)
	}
	for name, socket := range map[string]fakeSocket{"another workspace": beta, "no workspace": nowhere, "an expired session": expired} {
		if got := socket.next(50 * time.Millisecond); got != "" {
			t.Errorf("%s received %s", name, got)
		}
	}
	if !expired.closed() {
		t.Error("a socket whose session has expired should be closed")
	}

	// A request without a workspace reaches nobody.
	broadcast(-1, "item_created", nil)
	broadcast(0, "item_created", nil)
	if got := nowhere.next(50 * time.Millisecond); got != "" {
		t.Errorf("a broadcast without a workspace was delivered: %s", got)
	}
}

// liveFeed is a minimal WebSocket client, so the test needs no extra module.
type liveFeed struct {
	conn   net.Conn
	reader *bufio.Reader
}

// dialLiveFeed opens /ws the way a signed-in browser does.
func dialLiveFeed(t *testing.T, addr, sessionID string) liveFeed {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nCookie: %s=%s\r\n\r\n",
		addr, SessionCookieName, sessionID)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %d", resp.StatusCode)
	}
	return liveFeed{conn, reader}
}

// next reads one unfragmented text frame (a server never masks its frames),
// or returns "" if nothing arrives in time.
func (f liveFeed) next(wait time.Duration) string {
	_ = f.conn.SetReadDeadline(time.Now().Add(wait))
	head := make([]byte, 2)
	if _, err := io.ReadFull(f.reader, head); err != nil {
		return ""
	}
	size := int(head[1] & 0x7f)
	if size == 126 {
		ext := make([]byte, 2)
		io.ReadFull(f.reader, ext)
		size = int(binary.BigEndian.Uint16(ext))
	}
	payload := make([]byte, size)
	io.ReadFull(f.reader, payload)
	return string(payload)
}

// hungUp reports whether the server closed the connection (as opposed to it
// merely staying quiet).
func (f liveFeed) hungUp(wait time.Duration) bool {
	_ = f.conn.SetReadDeadline(time.Now().Add(wait))
	for {
		if _, err := f.reader.ReadByte(); err != nil {
			var timeout net.Error
			return !(errors.As(err, &timeout) && timeout.Timeout())
		}
	}
}

// A real connection takes its workspace, account and session from the request
// that opened it.
func TestLiveConnectionsJoinTheirSessionsWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	useFakeSockets(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	sessionA, sessionB := sessionFor(t, a.user, a.workspace.ID), sessionFor(t, b.user, b.workspace.ID)

	app := authApp()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go app.Listener(listener)
	t.Cleanup(func() { _ = app.Shutdown() })

	alpha := dialLiveFeed(t, listener.Addr().String(), sessionA)
	beta := dialLiveFeed(t, listener.Addr().String(), sessionB)
	var registered []*webSocketClient
	for deadline := time.Now().Add(time.Second); len(registered) < 2 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		clientsMu.RLock()
		registered = registered[:0]
		for _, client := range clients {
			registered = append(registered, client)
		}
		clientsMu.RUnlock()
	}
	if len(registered) != 2 {
		t.Fatalf("%d sockets registered, want 2", len(registered))
	}
	for _, client := range registered {
		want := map[string]guardHousehold{sessionA: a, sessionB: b}[client.sessionID]
		if want.user == nil || client.userID != want.user.ID || client.workspaceID != want.workspace.ID || client.expiresAt == 0 {
			t.Fatalf("socket registered as workspace %d, user %d, session %q, expiry %d",
				client.workspaceID, client.userID, client.sessionID, client.expiresAt)
		}
	}

	broadcast(a.workspace.ID, "item_created", map[string]string{"name": "alpha milk"})
	if got := alpha.next(time.Second); !strings.Contains(got, "alpha milk") {
		t.Fatalf("alpha's own socket got %q", got)
	}
	if got := beta.next(100 * time.Millisecond); got != "" {
		t.Fatalf("beta's socket received alpha's update: %s", got)
	}

	// Revoking access hangs up the real connection, not just the queue: the
	// server must not wait for the browser to leave.
	closeWebSockets(func(client *webSocketClient) bool { return client.sessionID == sessionA })
	if !alpha.hungUp(time.Second) {
		t.Fatal("the server did not hang up alpha's connection")
	}
	if beta.hungUp(100 * time.Millisecond) {
		t.Fatal("beta's connection was closed too")
	}
}

// The REST API broadcasts into the workspace its token is pinned to, and the
// browser handlers into the session's workspace.
func TestRequestsBroadcastIntoTheirOwnWorkspace(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	useFakeSockets(t)
	a, b := newGuardHousehold(t, "alpha"), newGuardHousehold(t, "beta")
	later := time.Now().Add(time.Hour).Unix()
	alpha := connectFakeSocket(a.workspace.ID, a.user.ID, "a", later)
	beta := connectFakeSocket(b.workspace.ID, b.user.ID, "b", later)

	app := authApp()
	if r := authDo(t, app, sessionFor(t, a.user, a.workspace.ID), "POST", "/sections/"+id(a.section.ID)+"/check-all", nil, nil); r.status != http.StatusOK {
		t.Fatalf("check-all: %d %s", r.status, r.body)
	}
	if got := alpha.next(time.Second); !strings.Contains(got, "section_items_checked") {
		t.Fatalf("alpha missed its own update, got %q", got)
	}
	if got := beta.next(50 * time.Millisecond); got != "" {
		t.Fatalf("beta received alpha's update: %s", got)
	}

	t.Setenv("API_WORKSPACE_ID", id(b.workspace.ID))
	api := fiber.New()
	api.Post("/", UseAPIWorkspace, func(c *fiber.Ctx) error {
		BroadcastUpdate(c, "item_created", map[string]string{"name": "from the API"})
		return c.SendStatus(http.StatusNoContent)
	})
	resp, _ := api.Test(httptest.NewRequest("POST", "/", nil))
	resp.Body.Close()
	if got := beta.next(time.Second); !strings.Contains(got, "from the API") {
		t.Fatalf("the API workspace missed the update, got %q", got)
	}
	if got := alpha.next(50 * time.Millisecond); got != "" {
		t.Fatalf("alpha received the API workspace's update: %s", got)
	}
}

// Whoever loses access loses the live feed at once: their socket is closed and
// the reconnect is authorized from scratch.
func TestLosingAccessClosesLiveConnections(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	useFakeSockets(t)
	later := time.Now().Add(time.Hour).Unix()
	owner := newGuardHousehold(t, "owner")
	other := newGuardHousehold(t, "other")
	member, _ := db.CreateUser("member", "Member", "correct horse battery", false)
	db.AddMember(owner.workspace.ID, member.ID, db.RoleMember)
	db.AddMember(other.workspace.ID, member.ID, db.RoleMember)
	admin, _ := db.CreateUser("boss", "Boss", "correct horse battery", true)
	db.AddMember(db.DefaultWorkspaceID, admin.ID, db.RoleOwner)
	app := authApp()
	ownerSession := sessionFor(t, owner.user, owner.workspace.ID)
	adminSession := sessionFor(t, admin, db.DefaultWorkspaceID)

	memberHere := connectFakeSocket(owner.workspace.ID, member.ID, "m1", later)
	memberElsewhere := connectFakeSocket(other.workspace.ID, member.ID, "m2", later)
	ownerHere := connectFakeSocket(owner.workspace.ID, owner.user.ID, ownerSession, later)

	authDo(t, app, ownerSession, "POST", "/workspaces/"+id(owner.workspace.ID)+"/members/"+id(member.ID)+"/remove", nil, nil)
	if !memberHere.closed() {
		t.Error("removing a member should close their socket in that workspace")
	}
	if memberElsewhere.closed() || ownerHere.closed() {
		t.Error("removing a member closed sockets it should not touch")
	}

	authDo(t, app, adminSession, "POST", "/admin/users/"+id(member.ID)+"/delete", nil, nil)
	if !memberElsewhere.closed() {
		t.Error("deleting an account should close all of its sockets")
	}

	otherOwner := connectFakeSocket(other.workspace.ID, other.user.ID, "o1", later)
	authDo(t, app, sessionFor(t, other.user, other.workspace.ID), "POST", "/workspaces/"+id(other.workspace.ID)+"/delete", nil, nil)
	if !otherOwner.closed() {
		t.Error("deleting a workspace should close its sockets")
	}

	ownerSecondDevice := connectFakeSocket(owner.workspace.ID, owner.user.ID, "phone", later)
	authDo(t, app, ownerSession, "POST", "/logout", nil, nil)
	if !ownerHere.closed() {
		t.Error("signing out should close that session's socket")
	}
	if ownerSecondDevice.closed() {
		t.Error("signing out on one device closed another device's socket")
	}
}

// A socket is registered after its upgrade was authorized. If access was lost
// in between, the check after registering catches it.
func TestSocketsAreCheckedAgainAfterRegistering(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	session := sessionFor(t, a.user, a.workspace.ID)
	allowed := &webSocketClient{workspaceID: a.workspace.ID, userID: a.user.ID, sessionID: session}
	if !socketStillAllowed(allowed) {
		t.Fatal("a member's live session should be allowed")
	}
	if socketStillAllowed(&webSocketClient{workspaceID: a.workspace.ID, userID: a.user.ID, sessionID: "gone"}) {
		t.Error("a deleted session should not be allowed")
	}
	stranger := newGuardHousehold(t, "beta")
	if socketStillAllowed(&webSocketClient{workspaceID: a.workspace.ID, userID: stranger.user.ID, sessionID: sessionFor(t, stranger.user, a.workspace.ID)}) {
		t.Error("someone who is not a member should not be allowed")
	}

	t.Setenv("MULTI_USER", "false")
	if !socketStillAllowed(&webSocketClient{workspaceID: db.DefaultWorkspaceID}) {
		t.Error("without accounts (and with DISABLE_AUTH) sockets carry no session and stay allowed")
	}
}

func TestAnotherSiteCannotOpenTheLiveFeed(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	initTestDatabase(t)
	a := newGuardHousehold(t, "alpha")
	app := authApp()
	session := sessionFor(t, a.user, a.workspace.ID)
	for _, site := range []string{"cross-site", "same-site"} {
		if r := authDo(t, app, session, "GET", "/ws", nil, map[string]string{"Sec-Fetch-Site": site}); r.status != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %s answered %d, want 403", site, r.status)
		}
	}
	// Not a real upgrade, so the upgrade middleware answers 426: the request
	// got past the site check.
	if r := authDo(t, app, session, "GET", "/ws", nil, map[string]string{"Sec-Fetch-Site": "same-origin"}); r.status != http.StatusUpgradeRequired {
		t.Errorf("a same-origin socket answered %d, want 426", r.status)
	}
}

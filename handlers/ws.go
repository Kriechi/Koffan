package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"shopping-list/db"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

const (
	webSocketWriteTimeout = 5 * time.Second
	webSocketReadTimeout  = 90 * time.Second
	webSocketQueueSize    = 64
)

// WebSocket client connections
var (
	clients   = make(map[*websocket.Conn]*webSocketClient)
	clientsMu sync.RWMutex
)

// webSocketClient serializes every write to a connection. The websocket
// implementation supports one concurrent writer only; broadcasts and pong
// responses can otherwise overlap and panic.
type webSocketClient struct {
	conn      webSocketWriter
	writeMu   sync.Mutex
	outgoing  chan []byte
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
	closeErr  error

	// Who the socket belongs to, fixed when it connects. Broadcasts only reach
	// the sockets of the workspace they concern.
	workspaceID int64
	userID      int64
	sessionID   string
	expiresAt   int64
}

type webSocketWriter interface {
	WriteJSON(interface{}) error
	WriteMessage(int, []byte) error
	Close() error
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
}

func (client *webSocketClient) writeJSON(value interface{}) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := client.conn.SetWriteDeadline(time.Now().Add(webSocketWriteTimeout)); err != nil {
		return err
	}
	return client.conn.WriteJSON(value)
}

func (client *webSocketClient) writeMessage(messageType int, data []byte) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := client.conn.SetWriteDeadline(time.Now().Add(webSocketWriteTimeout)); err != nil {
		return err
	}
	return client.conn.WriteMessage(messageType, data)
}

func newWebSocketClient(conn webSocketWriter) *webSocketClient {
	client := &webSocketClient{
		conn:     conn,
		outgoing: make(chan []byte, webSocketQueueSize),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go client.writeLoop()
	return client
}

// enqueue never waits for network I/O. A suspended phone must not delay item
// saves or updates for other shoppers. Reconnection refreshes missed state.
func (client *webSocketClient) enqueue(data []byte) error {
	select {
	case <-client.done:
		return errors.New("WebSocket client closed")
	default:
	}
	select {
	case <-client.done:
		return errors.New("WebSocket client closed")
	case client.outgoing <- data:
		return nil
	default:
		_ = client.close()
		return errors.New("WebSocket client queue full")
	}
}

func (client *webSocketClient) writeLoop() {
	defer close(client.stopped)
	for {
		select {
		case <-client.done:
			return
		case data := <-client.outgoing:
			if err := client.writeMessage(websocket.TextMessage, data); err != nil {
				log.Printf("Failed to send WebSocket message to client: %v", err)
				_ = client.close()
				return
			}
		}
	}
}

func (client *webSocketClient) close() error {
	client.closeOnce.Do(func() {
		close(client.done)
		// Close is safe concurrently with writes and must not wait for the
		// write mutex. Behind fasthttp it leaves the network connection open
		// until the handler returns (a blocked write still ends at its
		// deadline), so the read loop is woken too; it sees done and returns,
		// which hangs up.
		client.closeErr = client.conn.Close()
		_ = client.conn.SetReadDeadline(time.Now())
	})
	return client.closeErr
}

// WebSocketMessage represents a message sent to clients
type WebSocketMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// WebSocketHandler handles WebSocket connections
func WebSocketHandler(c *websocket.Conn) {
	client := newWebSocketClient(c)
	client.workspaceID = workspaceFromLocals(c.Locals(localWorkspace))
	if user, ok := c.Locals(localUser).(*db.User); ok {
		client.userID = user.ID
	}
	if session, ok := c.Locals(localSession).(*db.Session); ok {
		client.sessionID = session.ID
		client.expiresAt = session.ExpiresAt
	}
	c.SetReadLimit(4096)
	_ = c.SetReadDeadline(time.Now().Add(webSocketReadTimeout))

	// Register client
	clientsMu.Lock()
	clients[c] = client
	clientCount := len(clients)
	clientsMu.Unlock()

	log.Printf("WebSocket client connected. Total clients: %d", clientCount)

	defer func() {
		// Unregister client
		clientsMu.Lock()
		delete(clients, c)
		clientCount := len(clients)
		clientsMu.Unlock()
		_ = client.close()
		// Fiber pools connections after this handler returns. Wait until the
		// writer has stopped using this connection before allowing reuse.
		<-client.stopped
		log.Printf("WebSocket client disconnected. Total clients: %d", clientCount)
	}()

	// Access may have been revoked (sign-out, removal from the workspace) after
	// the upgrade was authorized but before the socket was registered, when
	// closeWebSockets could not see it yet. Checking once more after registering
	// closes that gap.
	if !socketStillAllowed(client) {
		return
	}

	// Keep connection alive and handle incoming messages
	for {
		messageType, msg, err := c.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		_ = c.SetReadDeadline(time.Now().Add(webSocketReadTimeout))
		// Checked after pushing the deadline back, so a close() that ran in
		// between is never missed, even while the peer keeps sending.
		select {
		case <-client.done:
			return
		default:
		}

		// Handle ping/pong
		if messageType == websocket.TextMessage {
			var message map[string]string
			if err := json.Unmarshal(msg, &message); err == nil {
				if message["type"] == "ping" {
					if err := client.writeJSON(map[string]string{"type": "pong"}); err != nil {
						log.Printf("Failed to send WebSocket pong: %v", err)
						break
					}
				}
			}
		}
	}
}

// socketStillAllowed reports whether the session behind a socket still exists
// and, with accounts, still belongs to a member of the socket's workspace.
func socketStillAllowed(client *webSocketClient) bool {
	if client.sessionID != "" {
		if _, err := db.GetSession(client.sessionID); err != nil {
			return false
		}
	}
	if MultiUser() && db.MemberRole(client.workspaceID, client.userID) == "" {
		return false
	}
	return true
}

// closeWebSockets drops the sockets that match, e.g. after a sign-out or when
// someone loses access to a workspace. Their pages reconnect and are
// authorized again from scratch.
func closeWebSockets(match func(client *webSocketClient) bool) {
	clientsMu.RLock()
	var matched []*webSocketClient
	for _, client := range clients {
		if match(client) {
			matched = append(matched, client)
		}
	}
	clientsMu.RUnlock()
	for _, client := range matched {
		// Closing wakes the read loop, which unregisters the socket.
		_ = client.close()
	}
}

// BroadcastUpdate sends an update to the WebSocket clients of the request's
// workspace.
func BroadcastUpdate(c *fiber.Ctx, eventType string, data interface{}) {
	broadcast(WorkspaceID(c), eventType, data)
}

func broadcast(workspaceID int64, eventType string, data interface{}) {
	if workspaceID <= 0 {
		return
	}
	message := WebSocketMessage{
		Type: eventType,
		Data: data,
	}

	messageBytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("Failed to marshal WebSocket message: %v", err)
		return
	}

	// Copy the clients while holding the map lock, then release it before any
	// queue operations. Each client writes independently, so stale connections
	// cannot block HTTP handlers or updates to healthy connections.
	now := time.Now().Unix()
	clientsMu.RLock()
	clientSnapshot := make([]*webSocketClient, 0, len(clients))
	for _, client := range clients {
		if client.workspaceID != workspaceID {
			continue
		}
		// A socket outlives the page load that opened it; once its session has
		// expired it is dropped instead of being fed further updates.
		if client.expiresAt != 0 && client.expiresAt < now {
			_ = client.close()
			continue
		}
		clientSnapshot = append(clientSnapshot, client)
	}
	clientsMu.RUnlock()

	clientCount := len(clientSnapshot)
	log.Printf("Broadcasting %s to %d clients", eventType, clientCount)

	successCount := 0
	for _, client := range clientSnapshot {
		err := client.enqueue(messageBytes)
		if err != nil {
			log.Printf("Failed to send WebSocket message to client: %v", err)
			// Closing the connection wakes the read loop, which unregisters it.
		} else {
			successCount++
		}
	}

	log.Printf("Broadcast %s completed: %d/%d clients queued", eventType, successCount, clientCount)
}

// WebSocketUpgrade middleware to upgrade HTTP to WebSocket
func WebSocketUpgrade(c *websocket.Conn) error {
	return nil
}

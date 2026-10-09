package handlers

import (
	"errors"
	"shopping-list/db"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/websocket/v2"
)

type stalledWebSocketWriter struct {
	started  chan struct{}
	closed   chan struct{}
	start    sync.Once
	close    sync.Once
	deadline time.Time
}

func (writer *stalledWebSocketWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	return nil
}
func (writer *stalledWebSocketWriter) SetReadDeadline(time.Time) error { return nil }
func (writer *stalledWebSocketWriter) WriteJSON(interface{}) error     { return nil }
func (writer *stalledWebSocketWriter) WriteMessage(int, []byte) error {
	writer.start.Do(func() { close(writer.started) })
	<-writer.closed
	return errors.New("connection closed")
}
func (writer *stalledWebSocketWriter) Close() error {
	writer.close.Do(func() { close(writer.closed) })
	return nil
}

type recordingWebSocketWriter struct {
	messages chan []byte
}

func (writer *recordingWebSocketWriter) SetWriteDeadline(time.Time) error { return nil }
func (writer *recordingWebSocketWriter) SetReadDeadline(time.Time) error  { return nil }
func (writer *recordingWebSocketWriter) WriteJSON(interface{}) error      { return nil }
func (writer *recordingWebSocketWriter) Close() error                     { return nil }
func (writer *recordingWebSocketWriter) WriteMessage(_ int, data []byte) error {
	writer.messages <- data
	return nil
}

func waitForWebSocketSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func TestStalledWebSocketDoesNotBlockBroadcastOrHealthyClient(t *testing.T) {
	stalledWriter := &stalledWebSocketWriter{started: make(chan struct{}), closed: make(chan struct{})}
	stalled := newWebSocketClient(stalledWriter)
	stalled.workspaceID = db.DefaultWorkspaceID
	healthyWriter := &recordingWebSocketWriter{messages: make(chan []byte, 2)}
	healthy := newWebSocketClient(healthyWriter)
	healthy.workspaceID = db.DefaultWorkspaceID
	clientsMu.Lock()
	originalClients := clients
	clients = map[*websocket.Conn]*webSocketClient{nil: stalled, new(websocket.Conn): healthy}
	clientsMu.Unlock()
	t.Cleanup(func() {
		_ = stalled.close()
		_ = healthy.close()
		<-stalled.stopped
		<-healthy.stopped
		clientsMu.Lock()
		clients = originalClients
		clientsMu.Unlock()
	})

	if err := stalled.enqueue([]byte("old update")); err != nil {
		t.Fatal(err)
	}
	waitForWebSocketSignal(t, stalledWriter.started, "stalled write did not start")
	if timeout := time.Until(stalledWriter.deadline); timeout <= 0 || timeout > webSocketWriteTimeout {
		t.Fatalf("write deadline is not bounded: %v", timeout)
	}
	returned := make(chan struct{})
	go func() {
		broadcast(db.DefaultWorkspaceID, "item_toggled", map[string]int{"id": 42})
		close(returned)
	}()
	waitForWebSocketSignal(t, returned, "broadcast blocked behind suspended client")
	select {
	case data := <-healthyWriter.messages:
		if string(data) != `{"type":"item_toggled","data":{"id":42}}` {
			t.Fatalf("unexpected healthy client message: %s", data)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy client did not receive update")
	}
}

func TestWebSocketQueueOverflowClosesBlockedWriter(t *testing.T) {
	writer := &stalledWebSocketWriter{started: make(chan struct{}), closed: make(chan struct{})}
	client := newWebSocketClient(writer)
	t.Cleanup(func() { _ = client.close() })
	if err := client.enqueue([]byte("blocked")); err != nil {
		t.Fatal(err)
	}
	waitForWebSocketSignal(t, writer.started, "write did not start")
	for i := 0; i < webSocketQueueSize; i++ {
		if err := client.enqueue([]byte("queued")); err != nil {
			t.Fatalf("queue filled prematurely: %v", err)
		}
	}
	returned := make(chan struct{})
	go func() {
		if err := client.enqueue([]byte("overflow")); err == nil {
			t.Error("overflow should close the client and return an error")
		}
		close(returned)
	}()
	waitForWebSocketSignal(t, returned, "queue overflow waited for blocked network write")
	waitForWebSocketSignal(t, client.stopped, "closed client writer leaked")
	if err := client.enqueue([]byte("after close")); err == nil {
		t.Fatal("closed client accepted another message")
	}
}

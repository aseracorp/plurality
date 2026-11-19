package ai

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/azukaar/plurality/src/utils"
	"time"
)

// Regression test for the device-shell outage of 2026-11-19: the SSE writer
// goroutine hit a nil-pointer panic on a ResponseWriter whose HTTP handler had
// already returned, recovered it, and exited WITHOUT closing Done. The client
// stayed registered on the ActiveRequest forever — Broadcast kept "succeeding"
// into a queue with no reader, the app never saw stream EOF, never reconnected,
// and every device tool call stalled 90s until the server was restarted.
//
// The fix is two-fold, and this test pins both halves:
//  1. the writer goroutine selects on the request context, so a transport that
//     dies underneath it is noticed at the source (the handler unblocks on the
//     same ctx), and
//  2. every writer exit path closes Done, so a client that stops writing can
//     never linger as a live subscriber.

func TestSSEClientWriterExitsOnRequestContext(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	client := NewSSEClient(w, reqCtx)
	if client == nil {
		t.Fatal("NewSSEClient returned nil")
	}
	client.startWriter()

	// Healthy: events drain.
	if !client.Send(SSEEvent{Type: "text", Content: "hello"}) {
		t.Fatal("Send should succeed while the client is live")
	}
	time.Sleep(20 * time.Millisecond)

	cancel() // the transport died underneath the stream

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-client.Done:
			return // writer noticed, closed Done
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("writer goroutine did not close Done after the request context was cancelled — client would linger as a live subscriber (the wedge)")
}

func TestSSEClientSendFailsAfterWriterExit(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	client := NewSSEClient(w, reqCtx)
	client.startWriter()
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-client.Done:
			if client.Send(SSEEvent{Type: "tool_use"}) {
				t.Fatal("Send must fail once Done is closed — Broadcast must drop this client")
			}
			return
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("Done never closed after request-context cancellation")
}

// Broadcast must drop (not keep) a client whose writer is gone, so the
// ActiveRequest's client count reaches zero and the stream ends cleanly.
func TestBroadcastDropsDeadWriterClient(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	client := NewSSEClient(w, reqCtx)
	client.startWriter()
	cancel()

	// Wait for the writer to close Done.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-client.Done:
			goto ready
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("Done never closed")
ready:
	ar := NewActiveRequest("c1", "u1", utils.Model{}, utils.ModelSelected{})
	ar.AddClient(client)
	ar.Broadcast(SSEEvent{Type: "tool_use"})
	if n := ar.ClientCount(); n != 0 {
		t.Fatalf("dead client was not dropped by Broadcast: %d client(s) remain", n)
	}
}

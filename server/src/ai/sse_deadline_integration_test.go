package ai

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression test for the wedged-LLM-loop bug: an SSE client that stops
// reading must never block the LLM loop goroutine. With the writer-goroutine
// design, Send() only enqueues into a bounded channel and returns — it never
// touches the socket, so it cannot block on a stalled client (the pre-fix
// code called Flush() synchronously inside the LLM loop and froze the
// conversation for minutes).
//
// The client sends a real HTTP GET (so the handler runs) but then never
// reads the body, simulating a background tab. The handler streams many
// events via Send(); the bounded queue fills; Send() returns false and the
// handler completes quickly. Assert the whole flow finishes fast.
func TestSSEClientSendBoundedOnStalledClient(t *testing.T) {
	old := sseWriteTimeout
	sseWriteTimeout = 200 * time.Millisecond
	defer func() { sseWriteTimeout = old }()

	handlerDone := make(chan struct{}, 1)
	var worstSend time.Duration

	h := func(w http.ResponseWriter, r *http.Request) {
		client := NewSSEClient(w)
		if client == nil {
			t.Errorf("NewSSEClient returned nil")
			handlerDone <- struct{}{}
			return
		}
		var worst time.Duration
		var n int
		for i := 0; i < 100000; i++ {
			st := time.Now()
			ok := client.Send(SSEEvent{Type: "text", Content: strings.Repeat("x", 512), ConversationID: "c1"})
			if el := time.Since(st); el > worst {
				worst = el
			}
			if !ok {
				t.Logf("Send returned false at iter %d (queue full drop path)", i)
				break
			}
			n++
		}
		worstSend = worst
		t.Logf("DONE iterations=%d worstSend=%v", n, worst)
		handlerDone <- struct{}{}
	}

	srv := httptest.NewServer(http.HandlerFunc(h))
	defer srv.Close()

	// Raw TCP dial + hand-written HTTP GET. The handler runs, then we stop
	// reading — exactly how a background tab behaves.
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(addr)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n\r\n", host)

	// Watchdog: handler must finish fast even with a stalled client.
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not complete: Send() blocked on a stalled client")
	}

	if worstSend > 500*time.Millisecond {
		t.Fatalf("worst Send took %v; Send must never block the LLM loop", worstSend)
	}
}

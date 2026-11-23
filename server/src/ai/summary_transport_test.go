package ai

import (
	"net/http"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// The summariser's transport must NOT inherit the shared 90s
// ResponseHeaderTimeout.
//
// The summary request is Stream:false, so its response headers arrive only
// when the WHOLE completion has been generated. Transport.ResponseHeaderTimeout
// therefore caps the ANSWER at that many seconds of generation, not the time to
// first byte. The shared transport's 90s shot every durable-fold summary on the
// deployed build:
//
//	[Eco] machine-only FALLBACK fold (LLM summariser unavailable: summary
//	request failed: Post ".../v1/chat/completions": net/http: timeout
//	awaiting response headers)
//
// five folds running, at exactly 90s, with "LLM fold summary" logged ZERO
// times against 30 machine-only fallbacks.
//
// The error string is the proof of ownership. Go emits "net/http: timeout
// awaiting response headers" ONLY from Transport.ResponseHeaderTimeout
// (net/http/transport.go, errTimeout). A Client.Timeout produces
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)". The deployed log has the former, so the binding constraint was
// the inherited transport bound — NOT summaryHTTPTimeout, which is 10min and
// was never reached.
func TestSummaryTransportHeaderTimeoutMatchesTheRoundTripBudget(t *testing.T) {
	tr, ok := summaryHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("summary transport is %T, want *http.Transport", summaryHTTPClient.Transport)
	}

	// The defect: sharing utils.HTTPClient.Transport verbatim.
	if tr == utils.HTTPClient.Transport {
		t.Fatalf("summary client shares the SHARED transport verbatim — it inherits the 90s " +
			"ResponseHeaderTimeout, which caps a Stream:false summary at 90s of generation")
	}

	if tr.ResponseHeaderTimeout < summaryHTTPTimeout {
		t.Fatalf("ResponseHeaderTimeout %v is below the summariser's round-trip budget %v: "+
			"for a Stream:false call, headers arrive only when the whole completion is done, "+
			"so this bound caps the answer, not the first byte",
			tr.ResponseHeaderTimeout, summaryHTTPTimeout)
	}
	if tr.ResponseHeaderTimeout <= 90*time.Second {
		t.Fatalf("ResponseHeaderTimeout %v is at/near the shared 90s — the deployed failure mode", tr.ResponseHeaderTimeout)
	}
}

// The shared transport keeps its 90s: that bound is correct for STREAMING
// calls (time to the first SSE event) and this asserts the fix did not widen
// it globally.
func TestSharedTransportKeepsItsStreamingHeaderTimeout(t *testing.T) {
	tr, ok := utils.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Skip("shared transport is not *http.Transport")
	}
	if tr.ResponseHeaderTimeout != 90*time.Second {
		t.Fatalf("shared ResponseHeaderTimeout changed to %v — streaming calls rely on 90s", tr.ResponseHeaderTimeout)
	}
	if utils.HTTPClient.Timeout != 0 {
		t.Fatalf("shared client Timeout %v must stay 0 so SSE streams are not cut off", utils.HTTPClient.Timeout)
	}
}

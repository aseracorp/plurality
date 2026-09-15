package utils

import (
	"net"
	"net/http"
	"time"
)

// HTTPClient is the shared, bounded HTTP client used for ALL outbound HTTP
// calls (LiteLLM, embeddings, images, audio, maps, news, search, version
// checks). The default http.Client has NO timeout, so a stalled upstream
// (e.g. a hung LiteLLM/OpenRouter request) would block its goroutine forever.
// That used to wedge the whole server: the async embed goroutine held the
// global SQLite write lock across its embed HTTP call, and the main chat
// completion call also had no bound, so at conversation end every SQLite
// write (chat list / chat create / message push / eco compaction) could be
// blocked behind a stuck network call — UI stays alive, chats won't load.
//
// The 90s ResponseHeaderTimeout still allows long SSE chat streams; only the
// time to first response byte is bounded. Dial/TLS are bounded so no call can
// hang on connect either.
var HTTPClient = &http.Client{
	Timeout: 0, // no overall deadline: SSE streams can run for minutes
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
	},
}

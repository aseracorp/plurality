	// Keep the ENTIRE prefix (all tokens) so the cache grows dynamically
	// with our conversations — NOT a static budget. We send n_keep=-1,
	// which llama.cpp documents as "retain ALL tokens from the prompt".
	// Combined with cache_prompt=true this makes llama.cpp reuse the longest
	// common prefix with the previous request's KV cache regardless of
	// whether the conversation is 30k or 200k tokens: only the newly-added
	// tail is (re-)processed. A static n_keep (e.g. 4096 or 32768) would
	// cap how much of the prefix is preserved — useless for long sessions —
	// and the old avalon default --keep 4096 silently dropped all but the
	// first 4k tokens of the stable system prefix, forcing a full re-prefill
	// every turn. n_keep=-1 is the "grow with the conversation" mode.
	//
	// n_keep=-1 is sent for EVERY model here, not just the ones that opt
	// into modelRequiresSerialization (the per-model `serialize` flag / name
	// match). When NKeep stays nil, NO n_keep reaches the backend and it
	// falls back to its OWN static keep default — the swift/strata docker
	// preset uses --keep 16384, which is exactly the "input caching stuck at
	// 16K" symptom: the KV prefix is pinned to 16K tokens and everything past
	// it is re-prefilled on every turn. Sending n_keep=-1 unconditionally
	// restores grow-with-conversation caching for ALL backends; cloud
	// providers ignore n_keep harmlessly (the litellm proxy only forwards it
	// to llama.cpp-compatible backends via extra_body).
	nk := -1 // keep all prefix tokens (dynamic, grows with context)
	requestData.NKeep = &nk
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/caveman"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/memory"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

var initialAppPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play.\n"
var initialAppPromptForCalls = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play. This is a phone call, therefore avoid formatting, emoji, long sentences, etc... and act as you would normally act on an oral conversations.\n"
var callSystemPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. You run as a server that the user connects to through a client app, not as a local AI on the user's own device, so make sure to properly consider whether command/files needs to be ont he server or the client, if necessary clarify with the user. Help the user. This is a phone call: avoid formatting, emoji, and long sentences. Speak naturally as in oral conversation."

// baseSystemPrompt returns the default assistant system prompt. The persisted
// path is sourced from the PERSIST_VOL env var (defaults to /home/) so deploys
// that mount their persistent volume elsewhere can advertise the correct path.
func baseSystemPrompt() string {
	persistVol := os.Getenv("PERSIST_VOL")
	if persistVol == "" {
		persistVol = "/home/"
	}
	return "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. You run as a server that the user connects to through a client app, not as a local AI on the user's own device, so make sure to properly consider whether command/files needs to be ont he server or the client, if necessary clarify with the user. Help the user. Use Markdown (bold, lists, tables, code blocks with language tags, emoji) for visual structure. Match the user's tone: casual when they're casual, detailed when they need depth. Be concise by default but thorough when warranted. Be inquisitive and critical — don't just agree. If you setup anything in your system, make sure you take into account that only the " + persistVol + " folder, and your skills.md(s)/MCP.json will be persisted upon machine restart!"
}

// webhookExtSuffix returns the WEBHOOK_EXT advertisement appended to system
// prompts when the env var is set, otherwise empty.
func webhookExtSuffix() string {
	if v := os.Getenv("WEBHOOK_EXT"); v != "" {
		return "\n\nThe current exposed domain for webhooks is: " + v + " for external webhooks. Local ones (aka. non-SaaS that you start) must use localhost:8090!"
	}
	return ""
}

// portExtSuffix returns the PORT_EXT advertisement appended to system prompts
// when the env var is set, otherwise empty. PORT_EXT lists the ports that are
// exposed externally so the AI knows which ports it can bind servers to.
func portExtSuffix() string {
	if v := os.Getenv("PORT_EXT"); v != "" {
		return "\n\nThe following ports are exposed externally and available for you to bind servers to: " + v
	}
	return ""
}

// SendChatCompletion sends a chat completion request to the LiteLLM proxy,
// which handles routing to the correct provider (OpenAI, Claude, Gemini, Fireworks).
func SendChatCompletion(ctx context.Context, model utils.Model, conv utils.Conversation, payload ChatPayload) (io.ReadCloser, *chatMetrics, error) {
	systemPrompt := baseSystemPrompt()

	miniAppID := payload.MiniApp.ID
	isCall := payload.IsCall

	if isCall {
		systemPrompt = callSystemPrompt
	}

	utils.Log("MiniAppID: %s", miniAppID)

	if miniAppID != "" {
		username, _ := ctx.Value("userID").(string)
		miniApp, err := miniapps.Get(username, miniAppID)
		if err != nil {
			return nil, nil, err
		}

		if miniApp.Prompt["en"] != "" {
			if isCall {
				systemPrompt = initialAppPromptForCalls + miniApp.Prompt["en"]
			} else {
				systemPrompt = initialAppPrompt + miniApp.Prompt["en"]
			}
		}
	}

	isActionModel := Models.IsActionModel(model.Name)

	if isActionModel {
		if len(payload.AvailableSkills) > 0 {
			skillsList := strings.Join(payload.AvailableSkills, ", ")
			systemPrompt += "\n\nYou have access to the following local skills: " + skillsList +
				". When a user's request matches one of these skills, use the retrieve_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		if serverSkillNames := skills.Names(conv.UserID); len(serverSkillNames) > 0 {
			serverSkillsList := strings.Join(serverSkillNames, ", ")
			systemPrompt += "\n\nYou have access to the following server-shared skills: " + serverSkillsList +
				". When a user's request matches one of these skills, use the retrieve_server_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_server_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		// Append MCP server descriptions configured in mcp.json, in a
		// deterministic (sorted) order. Ranging the map directly yields a
		// different order on every request, which reshuffles the system
		// prompt and busts the KV prompt-cache prefix every turn.
		for _, sd := range mcp.ServerDescriptionsOrdered(conv.UserID) {
			systemPrompt += "\n\n[" + sd.ServerName + "] " + sd.Description
		}

		if mode, ok := model.Tools["filesystem_server__fs_write"]; ok && mode != "" && mode != "off" && mode != "false" {
			systemPrompt += "\n\nStartup scripts: any *.sh file you place in ~/.plurality/startup/ will be launched automatically with `nohup` on every server boot, with stdout/stderr going to <script>.sh.log next to it. Use this for long-running background processes you want to survive restarts. The ~/.plurality directory is persistent."
		}

		// Append a compact summary of disabled tools so the LLM can suggest enabling them.
		if disabledSummary := ai_tools.GetDisabledToolsSummary(conv.UserID, model, payload.ClientSideTools); disabledSummary != "" {
			systemPrompt += "\n\n" + disabledSummary
		}
	}

	if !LiteLLMReady() {
		return nil, nil, fmt.Errorf("AI proxy is not ready, please try again in a moment")
	}

	// Append the per-user important_memory snippet. It is always present so the
	// LLM can read its current value, and is overwritten via the
	// update_important_memory tool.
	if username, ok := ctx.Value("userID").(string); ok && username != "" {
		systemPrompt += "\n\nThis is your important_memory snippet, you can edit it further but keep it compact:\n```\n" + memory.Get(username) + "\n```"
	}

	// The system prompt is kept BYTE-STABLE across turns: no timestamp is
	// appended here. KV prompt caching (llama.cpp/avalon/strata) matches the
	// longest common PREFIX of the previous request's token stream — any
	// change in the first tokens (even a single byte) forces a full
	// re-prefill of the whole conversation on every turn. A dynamic
	// timestamp (especially upstream's time.Now().String(), which carries
	// nanoseconds + a monotonic clock component) is the #1 cache killer: it
	// lives near the top of the system prompt, so NO two consecutive turns
	// ever share a byte-identical prefix. The current time is instead
	// appended as a SEPARATE tiny message at the very END of the request
	// (see below), so the stable prefix (system prompt + full conversation
	// history) is reused across turns and only a handful of tail tokens are
	// (re-)processed.
	// Caveman "say less" (the voice).
	systemPrompt = applyCavemanVoice(systemPrompt, model, isCall)

	systemMsg := utils.Message{
		Role: "system",
		Content: utils.NewTextContent(systemPrompt +
			webhookExtSuffix() +
			portExtSuffix()),
	}

	// Apply eco-mode checkpoint filtering before assembling the request.
	// When eco is on: keep the ONE rolling checkpoint pair and every live row
	// after it — FROZEN, i.e. no sliding char window, so the sent history
	// starts at the same row every turn and the KV prompt-cache prefix keeps
	// growing (see filterCheckpointsForRequest). Durable compaction is what
	// bounds the view; the preemptive guard below is what converts "this view
	// no longer fits" into a DURABLE fold, and only as a last resort does the
	// loop's recovery trim in memory.
	// When eco is off: drop the checkpoint pair entirely and send the raw
	// history.
	windowTokens := Models.MaxContextTokensForPrefixed(model.Name)
	visibleMessages := filterCheckpointsForRequest(conv.Messages, conv.ModelSelected.EcoMode)
	allMessages := append([]utils.Message{systemMsg}, visibleMessages...)
	optimizedMessages, hasAttachments, hasDocAttachments := PrepareMessagesForAI(allMessages, model)

	// A non-vision action model must NEVER receive base64 image payloads:
	// SelectModel already had its chance to swap to a vision model (it does
	// so when a vision model is configured and images are present), so
	// reaching this point with a non-vision model means it genuinely cannot
	// see images. Shipping the re-inflated base64 to it is pure context
	// waste — a single screenshot is 10-50k tokens, and 15 recent
	// attachments once carried ~96k tokens on top of a ~6k tail, blowing
	// the 131k window (prompt 131478 vs 131072 → litellm 500 → dead turn).
	if isActionModel && !Models.IsVisionModel(model.Name) {
		optimizedMessages = stripImagesForNonVisionModel(optimizedMessages)
	}

	requestData := StandardChatRequest{
		Model:         model.Name,
		Messages:      nil, // filled below
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
	}

	if isActionModel {
		ait := ai_tools.GetRequests(conv.UserID, model, payload.ClientSideTools, hasAttachments, hasDocAttachments, payload.ModelSelected.ClientFolderPath != "", cavemanReadLess(model))
		if len(ait) > 0 {
			requestData.Tools = ait
		}
	}

	msgReqList, _ := convertMessagesToOpenAI(optimizedMessages, model)

	// Attach the current time to the request WITHOUT claiming a turn (see
	// appendCurrentTimeTail: a trailing "system"/"user"/"tool" message each
	// breaks a different self-hosted chat template, and the "user" form is what
	// makes the model narrate the clock back at the user).
	//
	// The clock is appended to the END of the newest message, so the system
	// prompt and the entire history stay byte-identical across turns and the
	// KV/prompt cache is reused; only a handful of tail tokens and the model's
	// reply are (re-)processed. That is what makes prompt caching work for a
	// long-lived agent loop.
	requestData.Messages = appendCurrentTimeTail(msgReqList)

	// Prompt caching for self-hosted (llama.cpp/avalon/strata) backends.
	// Prompt caching for ALL models (local AND cloud). Sending
	// cache_prompt=true asks the backend (llama.cpp/avalon, or a cloud
	// provider that supports KV caching) to reuse the longest common prefix
	// of the previous request's KV cache — which covers the byte-stable
	// system prompt (sorted MCP descriptions, stable important_memory) AND
	// the chat history that is unchanged since the previous turn. Cloud
	// providers ignore or honor cache_prompt as they see fit; the
	// header/body passthrough is harmless when unsupported.
	cp := true
	requestData.CachePrompt = &cp
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

	utils.Debug("A new chat request is being made with the following model: %s", model.Name)

	// Marshal WITHOUT HTML escaping. The default json.Marshal escapes &, <,
	// > and all non-ASCII to \uXXXX, which inflates code-heavy tool results
	// (shell commands full of &&, unicode, quotes) 3-6x between the
	// budgeted character count and the actual wire bytes — that inflation
	// made the preemptive guard false-positive on every turn and drove the
	// destructive durable-compaction loop (observed: conversation
	// e1c3b48c63eb077f0985b83a, a ~100k-char capped tail marshalling to
	// ~500KB / ~126k tokens against a 131072 window). The wire bytes are
	// what the provider counts, so measure them faithfully.
	var jsonBuf bytes.Buffer
	enc := json.NewEncoder(&jsonBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(requestData); err != nil {
		return nil, nil, err
	}
	jsonData := jsonBuf.Bytes()

	// Preemptive context-window guard. The provider's 500 body is often a
	// bare "Internal Server Error" with the real reason only in LiteLLM's
	// own logs, so isContextLengthError in the LLM loop cannot see it and
	// the compaction retry never fires (observed: a request at the window
	// edge → provider 500 with an opaque body → dead turn). Estimate the
	// token count client-side BEFORE sending; if it
	// would exceed the model's window minus the reply reservation, return
	// an error the loop's context-length path recognizes so it compacts
	// the history and retries instead of shipping an oversized request.
	// estTokens is hoisted above the guard because the guard needs it on both
	// sides of the compression-factor discount below. It stays local to this
	// function: it is a heuristic, and the turn's compression ratio is now
	// measured from the proxy's own paired counts (compression_measured.go)
	// rather than derived from it.
	estTokens := 0
	// Keep room for the model's answer: max_tokens + safety.
	const replyReserve = 4096 + 2048
	// viewTokens is the eco view's own size (folded rows skipped, per-message
	// cap applied) — the same measure the eco trigger gate uses, so the two
	// agree about what is being sent. Logged alongside the marshalled estimate
	// because the pair is the diagnostic for frozen compaction: viewTokens
	// must be FLAT turn over turn between folds, and must step DOWN at a
	// fold. jsonData above is the stricter bound (it carries the tool schemas
	// and re-inflated attachments too), so the guard keeps using it.
	viewTokens := SentViewTokens(visibleMessages)
	if windowTokens > 0 {
		// ~3 chars per token for JSON: tool results and shell/file dumps are
		// far denser than prose (~2.5-3.5 chars/token), and the marshalled
		// request now carries the true unescaped wire bytes (message content,
		// tool schemas, re-inflated images). 4 chars/token under-estimated
		// dense code and over-estimated escaped text; 3.0 sits in the middle
		// of the observed density band so the guard fires only when the
		// request genuinely risks the window.
		estTokens = len(jsonData) / 3
		// Compression-aware guard: when the model routes through a Headroom /
		// Caveman compression proxy, the provider actually sees a smaller
		// prompt than the marshalled bytes above. Without this discount the
		// guard fires far too early on compressed models and forces durable
		// compaction it does not need. We keep the estimate conservative:
		// apply the model's observed compression factor only when it is >1
		// (i.e. a proxy is provably active) and bounds the discount so the
		// guard can never under-count dangerously (see CompressionFactor).
		postCompressionTokens := estTokens
		if factor := CompressionFactor(model.Name); factor > 1 {
			postCompressionTokens = int(float64(estTokens) / factor)
			utils.Log("[Eco] compression-aware guard: est %d tokens / factor %.2f = %d post-compression vs window %d", estTokens, factor, postCompressionTokens, windowTokens)
		}
		// Keep room for the model's answer: max_tokens (4096) + safety.
		// Room for the model's answer — the SAME reservation we send as
		// max_tokens, so this guard and the backend agree on the prompt ceiling.
		if postCompressionTokens > windowTokens-replyReserve {
			utils.Log("[Eco] preemptive context guard: estimated %d tokens vs window %d (eco view %d tokens, %d live rows) — returning context-length error so the loop compacts and retries", estTokens, windowTokens, viewTokens, len(visibleMessages))
			return nil, nil, fmt.Errorf("request estimated to exceed the context window: ~%d tokens vs %d (max %d) — compacting history and retrying", estTokens, windowTokens, windowTokens-replyReserve)
		}
	}

	// Resolve the target once so we know whether this request is routed
	// through caveman (for fail-closed circuit-breaker accounting).
	chatURL := ChatCompletionURL(model)
	routedViaCaveman := strings.HasPrefix(chatURL, CavemanBaseURL)

	req, err := http.NewRequest("POST", chatURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	ApplyChatOptimizationHeaders(model.Name, req)

	// Use the shared serialized+retrying LLM path.
	// Serializing chat completions keeps a single-GPU self-hosted backend
	// (llama.cpp/strata) from prefilling two big prompts concurrently — the
	// queue then blows the SSE idle timeout and the turn hangs in
	// idleTimeoutReader until the client cancels. The caller's context is
	// honored while waiting for the slot, so a cancelled turn never stays
	// parked behind a busy queue. 402/429/5xx get the bounded backoff retry.
	//
	// Self-hosted local backends additionally hold the slot for the WHOLE
	// stream (DoLLMHTTPWithRetryHeld): the slot then covers prefill +
	// generation, so a second workflow's giant prompt is only sent after this
	// stream finishes decoding instead of two prompts queueing on the same
	// GPU and both tripping the idle timeout. The body's Close() releases
	// the held slot (ProcessStandardStream closes it via defer).
	//
	// Whether a model gets this treatment is decided by the per-model
	// `serialize` config flag (see modelRequiresSerialization), with a
	// name-matching fallback for existing deployments.
	// Per-model cache pacing params (`cache_gap_ms`, `cache_ttl_s`) are
	// registered here, next to the `serialize` flag, so a deployment can tune
	// one backend's prefix-cache scheduling without global env vars (see
	// utils/llmsched.go).
	utils.RegisterModelScheduling(model)

	if modelRequiresSerialization(model) {
		resp, err := utils.DoLLMHTTPWithRetryHeld(ctx, req)
		if err != nil {
			NoteCavemanResult(routedViaCaveman, 0)
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusOK {
			NoteCavemanResult(routedViaCaveman, resp.StatusCode)
			respBody, _ := io.ReadAll(resp.Body)
			strStatus := strconv.Itoa(resp.StatusCode)
			utils.Error("LiteLLM API request failed with status", nil, strStatus, ":", string(respBody))
			return nil, nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
		}
		NoteCavemanResult(routedViaCaveman, resp.StatusCode)
		// chatMetricsFrom reads the proxy's paired token counts off the
		// response headers now, at response start: the body is a stream the
		// caller consumes after this returns, and the headers are gone by
		// then (see compression_measured.go).
		return resp.Body, chatMetricsFrom(resp), nil
	}

	resp, err := utils.DoLLMHTTPWithRetry(ctx, req)
	if err != nil {
		NoteCavemanResult(routedViaCaveman, 0)
		return nil, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		NoteCavemanResult(routedViaCaveman, resp.StatusCode)
		respBody, _ := io.ReadAll(resp.Body)
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("LiteLLM API request failed with status", nil, strStatus, ":", string(respBody))
		return nil, nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	NoteCavemanResult(routedViaCaveman, resp.StatusCode)

	return resp.Body, chatMetricsFrom(resp), nil
}

// convertMessagesToOpenAI converts internal utils.Message slices into
// StandardMessageReq for sending to the LiteLLM proxy (OpenAI-compatible format).
// Returns the converted messages and the concatenated input text (for debugging/logging).
// cavemanStones resolves which caveman stones are active for a model, applying
// the operator kill-switches. Unknown models get all-false: an unrecognized
// model receives verbatim tool results and its own system prompt, never a
// partial optimization.
type cavemanStoneSet struct {
	readLess bool
	browse   bool
	sayLess  bool
}

func activeCavemanStones(model utils.Model) cavemanStoneSet {
	entry, ok := Models.Get(model.Name)
	if !ok || !entry.Caveman.Enabled {
		return cavemanStoneSet{}
	}
	st := entry.Caveman.Stones
	// read_less compression runs here ONLY while caveman owns content, so the
	// no-double-compression rule has a single source: optimizationOwners. A
	// headroom-enabled model keeps its hop (transport is decided by flags) and
	// headroom does not also compress this content, because caveman owns it.
	owner, _ := optimizationOwners(model.Name)
	return cavemanStoneSet{
		// CAVEMAN_GO_READLESS=0 / CAVEMAN_GO_BROWSE=0 hand input compression
		// back to the proxy (e.g. if a future build binds recovery and starts
		// compressing input, keeping the no-double-optimization rule).
		readLess: owner == ownerCaveman && !envDisabled("CAVEMAN_GO_READLESS"),
		browse:   st.Browse && !envDisabled("CAVEMAN_GO_BROWSE"),
		sayLess:  st.SayLess,
	}
}

func envDisabled(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "0" || strings.EqualFold(v, "false")
}

// cavemanReadLess keeps its name for the tool-registry call site: recovery is
// needed whenever ANY input compressor is active.
func cavemanReadLess(model utils.Model) bool {
	s := activeCavemanStones(model)
	return s.readLess || s.browse
}

// applyCavemanVoice appends the caveman voice to the system prompt when the
// say_less stone is on.
//
// It is the ONLY Go-side use of a caveman stone that touches the prompt, and
// it only ever APPENDS a fixed instruction: it does not compress, rewrite,
// shorten or re-voice any user message. Caveman's own research (Adobe
// CAVEWOMAN) is explicit that cavemanning the user's prompt makes answers
// longer and worse, and the upstream docs say it plainly — "Your prompts stay
// yours. Never rewritten. Research says that backfires". Compressing user text
// here would also break tool_call/tool_result pairing and inflate every later
// turn, which is exactly the loop caveman itself warns about.
//
// Voice() is a constant string, so the KV prompt-cache prefix stays byte-stable
// across turns. Voice is skipped on calls (isCall): spoken answers need full
// sentences, and the voice's terser output would fight the call prompt.
func applyCavemanVoice(systemPrompt string, model utils.Model, isCall bool) string {
	if isCall || !activeCavemanStones(model).sayLess {
		return systemPrompt
	}
	return systemPrompt + caveman.Voice()
}

// cavemanCompressApplies decides, per tool message, whether its stored
// projection may replace the raw body for this model.
//
// Write time is deliberately stone-blind (a message is written once and then
// read by many models across the conversation's life), so the request path —
// which knows the model — re-derives which stone owns the row from the tool
// name via the same pure function the write path used.
func cavemanCompressApplies(st cavemanStoneSet, toolName string) bool {
	if caveman.WebToolIsBrowsed(toolName) {
		return st.browse
	}
	return st.readLess
}

func convertMessagesToOpenAI(messages []utils.Message, model utils.Model) ([]StandardMessageReq, string) {
	stones := activeCavemanStones(model)
	result := make([]StandardMessageReq, 0, len(messages))
	inputText := ""

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			text := msg.TextContent()
			if text == "" {
				continue
			}
			inputText += "system " + text + " {}{}{}{}{}{}{}"
			result = append(result, StandardMessageReq{
				Role:    "system",
				Content: text,
			})

		case "user":
			parts := msg.ContentParts()
			if len(parts) == 0 {
				continue
			}

			contentParts := make([]StandardContentReq, 0, len(parts))
			for _, part := range parts {
				if part.Type == "image_url" && part.ImageURL != nil {
					contentParts = append(contentParts, StandardContentReq{
						Type: "image_url",
						ImageURL: &utils.ContentImageURL{
							URL: part.ImageURL.URL,
						},
					})
					continue
				}
				if part.Text == "" {
					continue
				}
				text := part.Text
				if part.Type != "text" && part.Filename != "" {
					text = fmt.Sprintf("[Attached file: %s]\n%s", part.Filename, text)
				}
				inputText += "user " + text + " {}{}{}{}{}{}{}"
				contentParts = append(contentParts, StandardContentReq{
					Type: "text",
					Text: text,
				})
			}

			if len(contentParts) == 0 {
				continue
			}
			result = append(result, StandardMessageReq{
				Role:    "user",
				Content: contentParts,
			})

		case "assistant":
			text := msg.TextContent()
			smr := StandardMessageReq{
				Role: "assistant",
			}
			if text != "" {
				smr.Content = text
				inputText += "assistant " + text + " {}{}{}{}{}{}{}"
			}
			if len(msg.ToolCalls) > 0 {
				cleaned := make([]utils.ToolCall, len(msg.ToolCalls))
				for i, tc := range msg.ToolCalls {
					fn := tc.Function
					args := strings.TrimSpace(fn.Arguments)
					if args == "" {
						fn.Arguments = "{}"
					} else if !json.Valid([]byte(args)) {
						preview := args
						if len(preview) > 120 {
							preview = textsafe.HeadBytes(preview, 120)
						}
						utils.Log("[convertMessagesToOpenAI] tool_call %s (%s) has invalid JSON args; replacing with {}. First 120 chars: %s",
							tc.ID, fn.Name, preview)
						fn.Arguments = "{}"
					}
					cleaned[i] = utils.ToolCall{
						ID:       tc.ID,
						Type:     tc.Type,
						Function: fn,
					}
				}
				smr.ToolCalls = cleaned
			}
			if text == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			result = append(result, smr)

		case "tool":
			parts := msg.ContentParts()
			hasImages := msg.HasImages()
			if hasImages {
				contentParts := make([]StandardContentReq, 0, len(parts))
				for _, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil {
						contentParts = append(contentParts, StandardContentReq{
							Type:     "image_url",
							ImageURL: &utils.ContentImageURL{URL: part.ImageURL.URL},
						})
					} else if part.Text != "" {
						text := part.Text
						if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
							text = "Tool result displayed to user."
						}
						inputText += "tool " + text + " {}{}{}{}{}{}{}"
						contentParts = append(contentParts, StandardContentReq{
							Type: "text",
							Text: text,
						})
					}
				}
				if len(contentParts) == 0 {
					continue
				}
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    contentParts,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			} else {
				text := msg.TextContent()
				if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
					text = "Tool result displayed to user."
				}
				// Caveman input compression: ship the stored projection
				// instead of the raw body, gated by the stone that owns this
				// payload (read_less for logs/JSON/test output, browse for web
				// pages). The full original stays in the DB (chat display,
				// audit, caveman_retrieve).
				if msg.CompressedContent != "" && cavemanCompressApplies(stones, msg.Name) {
					text = msg.CompressedContent
				}
				if text == "" {
					continue
				}
				inputText += "tool " + text + " {}{}{}{}{}{}{}"
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    text,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			}
		}
	}

	// Backfill any assistant tool_calls that don't have a matching tool result
	// before the next user/assistant boundary. Strict providers (Fireworks)
	// reject dangling tool_calls. This defense repairs conversations whose
	// history was persisted before the stream-processor fix existed (the old
	// code persisted assistant messages whose tool_calls were cut off
	// mid-stream with no result ever pushed). Providers also REUSE short
	// tool-call ids ("call_0") across turns, so only results inside THIS
	// assistant block are considered — never results from an earlier block.
	//
	// When a call in this block genuinely has no result, the ENTIRE call is
	// dropped rather than inventing an empty "tool" result: an invented
	// result makes the model believe the tool executed and returned nothing,
	// so it either loops calling the same tool (the provider keeps issuing
	// "call_0") or the empty-tool flood pushes the conversation past the
	// model context limit. Dropping is safe — the model just may re-issue a
	// genuinely useful call, which is exactly what a truncated-turn call is.
	out := make([]StandardMessageReq, 0, len(result))
	i := 0
	for i < len(result) {
		if result[i].Role == "assistant" && len(result[i].ToolCalls) > 0 {
			j := i + 1
			for j < len(result) && result[j].Role != "user" && result[j].Role != "assistant" {
				j++
			}
			seen := make(map[string]bool, len(result[i].ToolCalls))
			blockTools := make([]StandardMessageReq, 0, j-(i+1))
			for k := i + 1; k < j; k++ {
				if result[k].Role == "tool" && result[k].ToolCallID != "" {
					seen[result[k].ToolCallID] = true
				}
				blockTools = append(blockTools, result[k])
			}
			// Drop this assistant's tool_calls that have no result in the
			// block. Prune BEFORE emitting, on a copied message.
			kept := make([]utils.ToolCall, 0, len(result[i].ToolCalls))
			for _, tc := range result[i].ToolCalls {
				if !seen[tc.ID] {
					utils.Log("[convertMessagesToOpenAI] dropping dangling tool_call %s (%s) with no result in block", tc.ID, tc.Function.Name)
					continue
				}
				kept = append(kept, tc)
			}
			// Emit the (possibly pruned) assistant only when it still carries
			// something: kept calls, or text (Content == nil means the
			// assistant had no text). An assistant with neither is invalid
			// for the API and only existed to carry the dangling calls.
			if len(kept) > 0 || result[i].Content != nil {
				smr := result[i]
				smr.ToolCalls = kept
				out = append(out, smr)
				for _, bt := range blockTools {
					out = append(out, bt)
				}
			}
			i = j
			continue
		}
		out = append(out, result[i])
		i++
	}
	result = out

	return result, inputText
}

// SelectModel picks the vision model if any message contains images and the
// current text model doesn't already support vision; otherwise returns the text model.
//
// Evaluation runs on the post-strip ephemeral view (PrepareMessagesForAI) so
// that an image which has been stripped from the actual payload doesn't trigger
// a vision swap — otherwise we'd pay for the vision model on text-only turns.
func SelectModel(modelSelected utils.ModelSelected, conv utils.Conversation) utils.Model {
	if modelSelected.Vision != nil && modelSelected.Text != nil &&
		!Models.IsVisionModel(modelSelected.Text.Name) {
		prepared, _, _ := PrepareMessagesForAI(conv.Messages, *modelSelected.Text)
		for _, m := range prepared {
			if m.HasImages() {
				utils.Log("Vision model selected: %s", modelSelected.Vision.Name)
				return *modelSelected.Vision
			}
		}
	}

	if modelSelected.Text != nil {
		return *modelSelected.Text
	}
	if modelSelected.Vision != nil {
		return *modelSelected.Vision
	}
	return utils.Model{}
}

// GenerateImage sends an image generation request to the litellm proxy,
// which routes it to the upstream provider configured in litellm_config.yaml.
func GenerateImage(request ImageGenerationRequest) ([]byte, error) {
	if !LiteLLMReady() {
		return nil, fmt.Errorf("AI proxy is not ready")
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/images/generations", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Serialized + retried LLM path (see DoLLMHTTPWithRetry).
	resp, err := utils.DoLLMHTTPWithRetry(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Image generation request failed with status", nil, strStatus, ":", string(body))
		return nil, fmt.Errorf("image generation failed with status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GenerateTitleForMessage generates a short title for a conversation
// using the LiteLLM proxy. The model name is supplied by the caller — typically
// the "fast" shortcut's text model from data/config.json.
func GenerateTitleForMessage(message, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	msgReqList := []StandardMessageReq{
		{
			Role:    "system",
			Content: "You generate short titles for conversations. Respond with ONLY the title, no quotes, no formatting, no explanation. The title must be 3-4 words max.",
		},
		{
			Role:    "user",
			Content: "Generate a title for this conversation:\n\n" + message,
		},
	}

	maxTokens := 500
	requestData := StandardChatRequest{
		Model:           model,
		Messages:        msgReqList,
		MaxTokens:       &maxTokens,
		Stream:          false,
		ReasoningEffort: "low",
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", ChatCompletionURLForName(model)+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	ApplyChatOptimizationHeaders(model, req)

	// Serialized + retried LLM path (see DoLLMHTTPWithRetry).
	resp, err := utils.DoLLMHTTPWithRetry(context.Background(), req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		utils.Error("[TitleGen] API request failed", nil, strconv.Itoa(resp.StatusCode), string(body))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse title response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in title response")
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)

	return title, nil
}

// GenerateCheckpointSummary asks the configured fast model to produce a
// dense summary of a slice of conversation history, used by the eco mode
// rolling checkpoint. The input may include a previously-stored summary
// prefixed with "PRIOR CHECKPOINT:" — the system prompt instructs the
// model to fold it in rather than repeat it verbatim.
func GenerateCheckpointSummary(text, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	msgReqList := []StandardMessageReq{
		{
			Role: "system",
			Content: "You are summarising older conversation history that the live LLM will no longer see directly. " +
				"Your summary REPLACES those messages in the live context, so any detail you drop is lost to the model. " +
				"Rules:\n" +
				"1. Preserve verbatim: code blocks, file paths, identifiers, IDs, error messages, command outputs, URLs, numeric values, exact quoted user requests.\n" +
				"2. Preserve the FULL text content of any inline pasted snippet or text attachment shown to you under '[attachment: ...]' headers — copy them into a clearly-labeled section, do not paraphrase code or text the user pasted.\n" +
				"3. Preserve tool_call → TOOL_RESULT outcomes: which tool was called, the key arguments, and the substantive result (paths created, errors returned, data retrieved). Drop only obviously verbose framing.\n" +
				"4. For image / document / binary attachments noted as '[image attachment omitted]' or '[document attachment: ...]', just record that the attachment existed (filename and any context the user gave).\n" +
				"5. Preserve user goals, decisions, plans, and rejected options chronologically so the live LLM understands where the conversation stands.\n" +
				"6. The optional 'PRIOR CHECKPOINT' section at the start is a summary you produced earlier — fold its facts into your new summary in-place, do not repeat it verbatim and do not lose information from it.\n" +
				"7. Be dense and structured (sections / bullets), but do NOT be brief at the cost of dropping concrete content. Length should match the input's information density.\n" +
				"Respond with the summary text only — no preamble, no closing remarks.",
		},
		{
			Role:    "user",
			Content: text,
		},
	}

	maxTokens := 8192
	requestData := StandardChatRequest{
		Model:     model,
		Messages:  msgReqList,
		MaxTokens: &maxTokens,
		Stream:    false,
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		utils.Error("[EcoSummary] API request failed", nil, strconv.Itoa(resp.StatusCode), string(body))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse summary response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in summary response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

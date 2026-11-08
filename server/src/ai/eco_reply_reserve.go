package ai

import (
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// ecoReplyReserveForName is the OUTPUT budget reserved for the model's answer,
// by model name.
//
// It has two jobs, and the second one is the reason it exists:
//
//  1. The eco content budget is window - overhead - reserve, which makes the
//     prompt budget explicit: the request provably fits with room to answer.
//
//  2. It is sent as `max_tokens`. The main chat path used to leave MaxTokens
//     nil, so llama.cpp-style backends (strata) defaulted the output budget to
//     "everything left in the slot": measured live at prompt_tokens 80621 with
//     max_tokens 181515, and 262144 - 181515 = 80629 — eight tokens of headroom
//     over the 262144 window. With that reservation the prompt is free to
//     inflate all the way to the cap, so a few extra prompt tokens tip the
//     request into "request exceeds the context window". Capping the output
//     makes the prompt budget real instead of "whatever is left", which is what
//     lets compaction be scheduled deterministically BEFORE the window.
//
// A per-model `max_tokens` model param overrides the default so operators can
// tune reasoning-heavy models without a code change.
func ecoReplyReserveForName(name string) int {
	window := Models.MaxContextTokensForPrefixed(name)
	reserve := 4096 + 2048
	// Small-window models cannot afford a 6k answer out of a 65k context: cap
	// the reserve at a quarter of the window so the prompt keeps room.
	if q := window / 4; q > 1024 && q < reserve {
		reserve = q
	}
	if reserve < 1024 {
		reserve = 1024
	}
	return reserve
}

// ecoReplyReserve is ecoReplyReserveForName with the per-model `max_tokens`
// parameter honoured.
func ecoReplyReserve(model utils.Model) int {
	if v, ok := model.Params["max_tokens"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return ecoReplyReserveForName(model.Name)
}

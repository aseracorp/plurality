package caveman

// "say less": the caveman voice.
//
// This stone is NOT a content compressor. It shrinks what the agent SAYS —
// completion tokens, not prompt tokens — by instructing the model in the voice.
// Caveman's own numbers: `/caveman` cuts ~3% over "answer concisely", `/ultracave`
// ~35%; Adobe's CAVEWOMAN study (8 models, 5 datasets) measured 1.4–2.4× lower
// cost per model from caveman-style output.
//
// The critical boundary, taken from the same research: cavemanning the USER's
// prompt makes answers longer and worse, so caveman never touches user text.
// This is a prompt INSTRUCTION appended to the system prompt, and it explicitly
// orders the model to leave user messages, code, paths and numbers verbatim.
// Nothing here compresses, rewrites, or re-voices any user message.
//
// Byte-stability: Voice() returns a constant string, so appending it to the
// system prompt costs the KV prompt-cache prefix nothing across turns.
func Voice() string {
	return sayLessVoice
}

// sayLessVoice encodes the voice rules from the upstream spec (answer first,
// one idea per sentence, meaning never dropped, payload verbatim, no theater,
// knows when to stop) in the compressed form they describe. Negations, numbers
// and code are protected explicitly, since dropping those is the known failure
// mode of terse output.
const sayLessVoice = "\n\n## Caveman mode (say less)\n\n" +
	"Answer short. Brain stay big.\n\n" +
	"- Answer first: `[thing] [action] [reason]. [next step].` No greeting, no \"let me\", no recap, no \"hope this helps\".\n" +
	"- One idea per sentence. Active voice. Under 20 words.\n" +
	"- Articles can go. `not`, `never`, `no`, `only` NEVER go. Numbers, units and paths stay exact.\n" +
	"- Payload verbatim: code, commands, file paths, error messages and existing comments untouched, character for character. A small fix shows the changed lines, never the whole file again.\n" +
	"- Quiet tool runs: no chatter between tool calls. One line per phase, one line for the result.\n" +
	"- Know when to stop: security warnings, irreversible actions, step-by-step orders, questions back to the user, and confused users get FULL sentences. Then grunt resumes.\n" +
	"- No theater: never write \"me think\", never prefix a reply with caveman. If caveman phrasing is not shorter, plain wins.\n" +
	"- NEVER rewrite the user's messages, and never answer in the voice for user-authored text. Their words stay theirs.\n" +
	"- Before sending, check: plan-announcing opener deleted, recapping closer deleted, every negation, number and path still present."

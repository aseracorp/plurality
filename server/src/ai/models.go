package ai

// ValidModels is an array containing all valid model names extracted from the code
var ValidModels = []string{
	// OpenAI models (GPT-5 + GPT-4.1 families)
	"ChatGPT/gpt-5.2",
	"ChatGPT/gpt-5",
	"ChatGPT/gpt-5-mini",
	"ChatGPT/gpt-5-nano",
	"ChatGPT/gpt-4.1",
	"ChatGPT/gpt-4.1-mini",
	"ChatGPT/gpt-4.1-nano",

	// Claude models (4.5 versions)
	"Claude/claude-haiku-4-6",
	"Claude/claude-sonnet-4-6",
	"Claude/claude-opus-4-6",

	// Fireworks models
	"llama4-scout-instruct-basic",
	"llama4-maverick-instruct-basic",
	"deepseek-r1",
	"deepseek-r1-basic",
	"deepseek-r1-0528",
	"deepseek-v3",
	"deepseek-v3-0324",
	"deepseek-v3p2",
	"qwen3p6-plus",
	"kimi-k2p5",
	"glm-5p1",
	"minimax-m2p5",
	"qwen3-vl-30b-a3b-thinking",
	"qwen3-vl-30b-a3b-instruct",
	"qwen3p5-397b-a17b",

	// Image generation models
	"black-forest-labs/FLUX.2-dev",
	"black-forest-labs/FLUX.2-pro",

	// Gemini
	"Gemini/gemini-2.5-flash",
	"Gemini/gemini-2.5-flash-lite",
	"Gemini/gemini-2.5-pro",

	// Audio
	"whisper-v3-turbo",
	"cartesia/sonic",
}

var ValidVisionModels = []string{
	// Claude
	"Claude/claude-haiku-4-6",
	"Claude/claude-sonnet-4-6",
	"Claude/claude-opus-4-6",

	// OpenAI
	"ChatGPT/gpt-5.2",
	"ChatGPT/gpt-5",
	"ChatGPT/gpt-5-mini",
	"ChatGPT/gpt-4.1",
	"ChatGPT/gpt-4.1-mini",

	// Gemini
	"Gemini/gemini-2.5-flash",
	"Gemini/gemini-2.5-flash-lite",
	"Gemini/gemini-2.5-pro",

	// Fireworks Vision
	"llama4-scout-instruct-basic",
	"llama4-maverick-instruct-basic",
	"qwen3p6-plus",
	"kimi-k2p5",
	"qwen3p5-397b-a17b",
}

var ValidActionModels = []string{
	// OpenAI models
	"ChatGPT/gpt-5.2",
	"ChatGPT/gpt-5",
	"ChatGPT/gpt-5-mini",
	"ChatGPT/gpt-4.1",
	"ChatGPT/gpt-4.1-mini",

	// Claude models
	"Claude/claude-haiku-4-6",
	"Claude/claude-sonnet-4-6",
	"Claude/claude-opus-4-6",

	// Fireworks models
	"qwen3-vl-30b-a3b-thinking",
	"qwen3-vl-30b-a3b-instruct",
	"llama4-scout-instruct-basic",
	"llama4-maverick-instruct-basic",
	"deepseek-r1",
	"deepseek-r1-basic",
	"deepseek-v3",
	"deepseek-v3-0324",
	"deepseek-v3p2",
	"qwen3p5-397b-a17b",
	"qwen3p6-plus",
	"kimi-k2p5",
	"glm-5p1",
	"minimax-m2p5",

	// Gemini
	"Gemini/gemini-2.5-pro",
	"Gemini/gemini-2.5-flash",
	"Gemini/gemini-2.5-flash-lite",
}

func CheckActionModel(modelName string) bool {
	for _, validModel := range ValidActionModels {
		if validModel == modelName {
			return true
		}
	}
	return false
}

// CheckModel reports whether the given model name is one of the configured ValidModels.
func CheckModel(modelName string) bool {
	for _, validModel := range ValidModels {
		if validModel == modelName {
			return true
		}
	}
	return false
}

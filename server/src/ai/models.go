package ai

// ValidModels is an array containing all valid model names extracted from the code
var ValidModels = []string{
	// OpenAI models
	"ChatGPT/gpt-4.5",
	"ChatGPT/gpt-3.5-turbo",
	"ChatGPT/gpt-3.5-turbo-mini",
	"ChatGPT/gpt-4o-mini",
	"ChatGPT/gpt-4o",
	"ChatGPT/o3-mini",

	// Claude models
	"Claude/claude-3-haiku",
	"Claude/claude-3-7-sonnet",
	
	// Firefly models
	"llama4-scout-instruct-basic",
	"llama4-maverick-instruct-basic",
	"llama-v3p2-3b",
	"llama-v3p1-8b-instruct",
	"llama-v3p1-70b-instruct",
	"llama-v3p3-70b-instruct",
	"llama-v3p1-405b-instruct",
	"deepseek-r1",
	"deepseek-r1-basic",
	"deepseek-v3",
	"deepseek-v3-0324",
	"qwen2p5-72b-instruct",
	
	// Image generation models
	"black-forest-labs/FLUX.1-schnell",
	"black-forest-labs/FLUX.1-dev",

	// Gemini
	"Gemini/gemini-1.5-pro",
	"Gemini/gemini-1.5-flash-latest",
	"Gemini/gemini-2.0-flash",
	"Gemini/gemini-2.5-pro-exp-03-25",

	// Audio
	"whisper-v3-turbo",
	"cartesia/sonic",
}

var ValidFreeModels = []string{
	"llama4-scout-instruct-basic",
	"llama-v3p1-8b-instruct",
	"llama-v3p2-3b",
	"llama-v3p1-70b-instruct",
	"black-forest-labs/FLUX.1-schnell",
	"Gemini/gemini-2.0-flash",
	"whisper-v3-turbo",
	"cartesia/sonic",
}

var ValidVisionModels = []string{
	// "llama4-scout-instruct-basic",
	// "llama4-maverick-instruct-basic",
	"Claude/claude-3-haiku",
	"Claude/claude-3-7-sonnet",
	"ChatGPT/gpt-4o-mini",
	"ChatGPT/gpt-4o",
	
	"Gemini/gemini-1.5-flash-latest",
	"Gemini/gemini-2.0-flash",
	"Gemini/gemini-2.5-pro-exp-03-25",
}
	

var ValidActionModels = []string{
	// OpenAI models
	"ChatGPT/gpt-4.5",
	"ChatGPT/gpt-4o",
	
	// Claude models
	"Claude/claude-3-7-sonnet",
	
	// Firefly models
	"llama-v3p1-70b-instruct",
	"llama-v3p3-70b-instruct",
	"llama-v3p1-405b-instruct",
	"deepseek-r1",
	"deepseek-r1-basic",
	"deepseek-v3",
	"deepseek-v3-0324",
	"qwen2p5-72b-instruct",
	"llama4-scout-instruct-basic",
	"llama4-maverick-instruct-basic",

	// Gemini
	"Gemini/gemini-1.5-pro",
	"Gemini/gemini-2.5-pro-exp-03-25",
}

func CheckActionModel(modelName string) bool {
	for _, validModel := range ValidActionModels {
		if validModel == modelName {
			return true
		}
	}
	return false
}

// CheckModel checks if a given model name is in the ValidModels list
func CheckModel(modelName string, planName string) bool {
	if planName == "Free" {
		for _, validModel := range ValidFreeModels {
			if validModel == modelName {
				return true
			}
		}
		return false
	}

	for _, validModel := range ValidModels {
		if validModel == modelName {
			return true
		}
	}
	return false
}

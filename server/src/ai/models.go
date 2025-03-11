package ai

import (
	"strings"
)

// ValidModels is an array containing all valid model names extracted from the code
var ValidModels = []string{
	// OpenAI models
	"ChatGPT/gpt-4.5",
	"ChatGPT/gpt-4o-mini",
	"ChatGPT/gpt-4o",
	
	// Claude models
	"Claude/claude-3-haiku",
	"Claude/claude-3-7-sonnet",
	
	// Together models
	"meta-llama/Llama-3.2-11B-Vision-Instruct-Turbo",
	"meta-llama/Llama-3.2-8B-Instruct-Turbo",
	"meta-llama/Llama-3.2-3B-Instruct-Turbo",
	"meta-llama/Llama-3.3-70B-Instruct-Turbo",
	"meta-llama/Llama-3.2-90B-Vision-Instruct-Turbo",
	"meta-llama/Meta-Llama-3.1-405B-Instruct-Turbo",
	"deepseek-ai/DeepSeek-R1-Distill-Llama-70B-free",
	"deepseek-ai/DeepSeek-V3",
	"Qwen/Qwen2-VL-72B-Instruct",
	
	// Image generation models
	"black-forest-labs/FLUX.1-schnell",
	"black-forest-labs/FLUX.1-dev",
}
var ValidFreeModels = []string{
	"meta-llama/Llama-3.2-11B-Vision-Instruct-Turbo",
	"meta-llama/Llama-3.2-8B-Instruct-Turbo",
	"meta-llama/Llama-3.2-3B-Instruct-Turbo",
	"black-forest-labs/FLUX.1-schnell",
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

// GetModelsByProvider returns all models for a specific provider
func GetModelsByProvider(provider int) []string {
	var models []string
	
	prefix := ""
	switch provider {
	case OPENAI:
		prefix = "ChatGPT/"
	case CLAUDE:
		prefix = "Claude/"
	case TOGETHER:
		prefixes := []string{
			"meta-llama/",
			"deepseek-ai/",
			"Qwen/",
		}
		
		for _, validModel := range ValidModels {
			for _, p := range prefixes {
				if strings.HasPrefix(validModel, p) {
					models = append(models, validModel)
					break
				}
			}
		}
		return models
	}
	
	if prefix != "" {
		for _, validModel := range ValidModels {
			if strings.HasPrefix(validModel, prefix) {
				models = append(models, validModel)
			}
		}
	}
	
	return models
}
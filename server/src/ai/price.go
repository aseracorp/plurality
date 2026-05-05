package ai

import (
	"github.com/azukaar/plurality/src/utils"
)

// types
const (
	TEXT_INPUT   = 0
	TEXT_OUTPUT  = 1
	IMAGE_VISION = 2
	IMAGE_GEN    = 3
	TOOL_USE     = 4
	TRANSCRIBE   = 5
	TTS          = 6
	TITLE        = 7
)

// providers
const (
	NONE     = -1
	OPENAI   = 0
	CLAUDE   = 1
	TOGETHER = 2
	GOOGLE   = 3
)

// GetPriceFromTokenUsage converts actual token counts to credit amounts
// using per-model pricing. Token counts come from LiteLLM's include_usage response.
// Credits are 1:1 with dollars per million tokens (1M credits = $1).
func GetPriceFromTokenUsage(reqType int, provider int, model utils.Model, usage float64) float64 {
	price := float64(usage)

	if reqType == TEXT_INPUT {
		if provider == OPENAI {
			// GPT-5 family + GPT-4.1
			if model.Name == "ChatGPT/gpt-5.2" || model.Name == "ChatGPT/gpt-5" || model.Name == "ChatGPT/gpt-4.1" {
				// cost 2.00$ per 1M tokens
				price = usage * 2.00
			} else if model.Name == "ChatGPT/gpt-5-mini" || model.Name == "ChatGPT/gpt-4.1-mini" {
				// cost 0.40$ per 1M tokens
				price = usage * 0.40
			} else if model.Name == "ChatGPT/gpt-5-nano" || model.Name == "ChatGPT/gpt-4.1-nano" {
				// cost 0.10$ per 1M tokens
				price = usage * 0.10
			}
		} else if provider == CLAUDE {
			// Claude 4.5 family
			if model.Name == "Claude/claude-haiku-4-6" {
				// cost 0.80$ per 1M tokens
				price = usage * 0.80
			} else if model.Name == "Claude/claude-sonnet-4-6" {
				// cost 3.00$ per 1M tokens
				price = usage * 3.00
			} else if model.Name == "Claude/claude-opus-4-6" {
				// cost 15.00$ per 1M tokens
				price = usage * 15.00
			}
		} else if provider == GOOGLE {
			// Gemini 2.5 family
			if model.Name == "Gemini/gemini-2.5-pro" {
				// cost 1.25$ per 1M tokens
				price = usage * 1.25
			} else if model.Name == "Gemini/gemini-2.5-flash" {
				// cost 0.15$ per 1M tokens
				price = usage * 0.15
			} else if model.Name == "Gemini/gemini-2.5-flash-lite" {
				// cost 0.075$ per 1M tokens
				price = usage * 0.075
			}
		}
	} else if reqType == TEXT_OUTPUT {
		if provider == OPENAI {
			// GPT-5 family + GPT-4.1
			if model.Name == "ChatGPT/gpt-5.2" || model.Name == "ChatGPT/gpt-5" || model.Name == "ChatGPT/gpt-4.1" {
				// cost 8.00$ per 1M tokens
				price = usage * 8.00
			} else if model.Name == "ChatGPT/gpt-5-mini" || model.Name == "ChatGPT/gpt-4.1-mini" {
				// cost 1.60$ per 1M tokens
				price = usage * 1.60
			} else if model.Name == "ChatGPT/gpt-5-nano" || model.Name == "ChatGPT/gpt-4.1-nano" {
				// cost 0.40$ per 1M tokens
				price = usage * 0.40
			}
		} else if provider == CLAUDE {
			// Claude 4.5 family
			if model.Name == "Claude/claude-haiku-4-6" {
				// cost 4.00$ per 1M tokens
				price = usage * 4.00
			} else if model.Name == "Claude/claude-sonnet-4-6" {
				// cost 15.00$ per 1M tokens
				price = usage * 15.00
			} else if model.Name == "Claude/claude-opus-4-6" {
				// cost 75.00$ per 1M tokens
				price = usage * 75.00
			}
		} else if provider == GOOGLE {
			// Gemini 2.5 family
			if model.Name == "Gemini/gemini-2.5-pro" {
				// cost 10.00$ per 1M tokens
				price = usage * 10.00
			} else if model.Name == "Gemini/gemini-2.5-flash" {
				// cost 0.60$ per 1M tokens
				price = usage * 0.60
			} else if model.Name == "Gemini/gemini-2.5-flash-lite" {
				// cost 0.30$ per 1M tokens
				price = usage * 0.30
			}
		}
	}

	if reqType == IMAGE_VISION {
		return 5000
	}

	if provider == TOGETHER && (reqType == TEXT_INPUT || reqType == TEXT_OUTPUT || reqType == IMAGE_VISION) {
		// Llama 4 models
		if model.Name == "llama4-scout-instruct-basic" {
			if reqType == TEXT_INPUT {
				price = usage * 0.15
			} else if reqType == TEXT_OUTPUT {
				price = usage * 0.60
			}
		} else if model.Name == "llama4-maverick-instruct-basic" {
			if reqType == TEXT_INPUT {
				price = usage * 0.22
			} else if reqType == TEXT_OUTPUT {
				price = usage * 0.88
			}
		} else if model.Name == "deepseek-r1" || model.Name == "deepseek-r1-basic" {
			if reqType == TEXT_INPUT {
				price = usage * 3.00
			} else if reqType == TEXT_OUTPUT {
				price = usage * 8.00
			}
		} else if model.Name == "deepseek-r1-0528" {
			if reqType == TEXT_INPUT {
				price = usage * 1.35
			} else if reqType == TEXT_OUTPUT {
				price = usage * 5.40
			}
		} else if model.Name == "deepseek-v3" || model.Name == "deepseek-v3-0324" {
			price = usage * 0.90
		} else if model.Name == "deepseek-v3p2" {
			if reqType == TEXT_INPUT {
				price = usage * 0.56
			} else if reqType == TEXT_OUTPUT {
				price = usage * 1.68
			}
		} else if model.Name == "qwen3p6-plus" {
			if reqType == TEXT_INPUT {
				price = usage * 0.50
			} else if reqType == TEXT_OUTPUT {
				price = usage * 3.00
			}
		} else if model.Name == "qwen3-vl-30b-a3b-thinking"  || model.Name == "qwen3-vl-30b-a3b-instruct" {
			if reqType == TEXT_INPUT {
				price = usage * 0.15
			} else if reqType == TEXT_OUTPUT {
				price = usage * 0.6
			}
		} else if model.Name == "qwen3p5-397b-a17b" {
			if reqType == TEXT_INPUT {
				price = usage * 0.22
			} else if reqType == TEXT_OUTPUT {
				price = usage * 0.88
			}
		} else if model.Name == "kimi-k2p5" {
			if reqType == TEXT_INPUT {
				price = usage * 0.60
			} else if reqType == TEXT_OUTPUT {
				price = usage * 2.50
			}
		} else if model.Name == "glm-5p1" {
			if reqType == TEXT_INPUT {
				price = usage * 1.40
			} else if reqType == TEXT_OUTPUT {
				price = usage * 4.40
			}
		} else if model.Name == "minimax-m2p5" {
			if reqType == TEXT_INPUT {
				price = usage * 0.30
			} else if reqType == TEXT_OUTPUT {
				price = usage * 1.20
			}
		}
	}

	if (reqType == TEXT_INPUT || reqType == TEXT_OUTPUT) && price == usage && usage > 0 {
		utils.Error("[Price] No pricing rule for model — using fallback 1:1 rate. Credit count may be inaccurate.", nil, model.Name, provider, reqType)
	}

	return price
}

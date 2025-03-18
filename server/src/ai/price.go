package ai 

import (
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
	"github.com/pkoukk/tiktoken-go"
)

// types 
const (
	TEXT_INPUT = 0
	TEXT_OUTPUT = 1
	IMAGE_VISION = 2
	IMAGE_GEN = 3
	TOOL_USE = 4
)

// providers
const (
	NONE = -1
	OPENAI = 0
	CLAUDE = 1
	TOGETHER = 2
)

func GetPriceFromTokenUsage(reqType int, provider int, model utils.Model, usage float64) float64 {
	price := float64(usage)

	if(reqType == TEXT_INPUT) {
		if(provider == OPENAI) {
			if model.Name == "ChatGPT/gpt-4.5" {
				// cost 75$ per 1M tokens
				price = usage * 75.00
			}
			if model.Name == "ChatGPT/gpt-4o-mini" {
				// cost 0.150 per 1M tokens
				price = usage * 0.150
			} else if model.Name == "ChatGPT/gpt-4o" {
				// cost 2.5$ per 1M tokens
				price = usage * 2.50
			}
		} else if(provider == CLAUDE) {
			if model.Name == "Claude/claude-3-haiku" {
				// cost 0.80$ per 1M tokens
				price = usage * 0.80
			} else if model.Name == "Claude/claude-3-7-sonnet" {
				// cost 3$ per 1M tokens
				price = usage * 3.00
			}
		}
	} else if(reqType == TEXT_OUTPUT) {
		if(provider == OPENAI) {
			if model.Name == "ChatGPT/gpt-4.5" {
				// cost 150$ per 1M tokens
				price = usage * 150.00
			}
			if model.Name == "ChatGPT/gpt-4o-mini" {
				// cost 0.6 per 1M tokens
				price = usage * 0.60
			} else if model.Name == "ChatGPT/gpt-4o" {
				// cost 10.00$ per 1M tokens
				price = usage * 10.00
			}
		} else if(provider == CLAUDE) {
			if model.Name == "Claude/claude-3-haiku" {
				// cost 4$ per 1M tokens
				price = usage * 4.0
			} else if model.Name == "Claude/claude-3-7-sonnet" {
				// cost 15 $ per 1M tokens
				price = usage * 15.00
			}
		}
	}

	if(reqType == IMAGE_VISION) {
		return 5000
	}

	if(provider == TOGETHER && (reqType == TEXT_INPUT || reqType == TEXT_OUTPUT || reqType == IMAGE_VISION)) {
		if model.Name == "llama-v3p2-11b-vision-instruct" {
			// cost 0.18 per 1M tokens
			price = usage * 0.18
		} else if model.Name == "llama-v3p1-8b-instruct" {
			// cost 0.18 per 1M tokens
			price = usage * 0.18
		} else if model.Name == "llama-v3p1-70b-instruct" {
			// cost 0.88 per 1M tokens
			price = usage * 0.88
		} else if model.Name == "llama-v3p3-70b-instruct" {
			// cost 0.88 per 1M tokens
			price = usage * 0.88
		} else if model.Name == "llama-v3p2-90b-vision-instruct" {
			// cost 1.20 per 1M tokens
			price = usage * 1.20
		} else if model.Name == "llama-v3p1-405b-instruct" {
			// cost 3.50 per 1M tokens
			price = usage * 3.50
		} else if model.Name == "deepseek-r1" {
			// cost 2.00 per 1M tokens (free tier)
			price = usage * 2.00
		} else if model.Name == "deepseek-v3" {
			// cost 1.25 per 1M tokens
			price = usage * 1.25
		} else if model.Name == "qwen2p5-72b-instruct" {
			// cost 1.20 per 1M tokens
			price = usage * 1.20
		}
	}

	return price
}

func GetPrice(reqType int, provider int, model utils.Model, payload string) (error, float64) {
	err, token := GetTokenNumber(model, payload)
	if err != nil {
		return err, 0
	}

	return nil, GetPriceFromTokenUsage(reqType, provider, model, token)
}

func GetTokenNumber(model utils.Model, payload string) (error, float64) {
	// encoding := "cl100k_base"
	var tke *tiktoken.Tiktoken
	var err error

	mn := model.Name
	if strings.HasPrefix(mn, "ChatGPT/") {
		mn = strings.TrimPrefix(mn, "ChatGPT/")
		utils.Debug("mn: ", mn)
		tke, err = tiktoken.EncodingForModel(mn)
	} else {
		tke, err = tiktoken.EncodingForModel("gpt-4o")
	}


	
	if err != nil {
		err = fmt.Errorf("getEncoding: %v", err)
		return err, 0
	}

	// encode
	token := tke.Encode(payload, nil, nil)
	
	return nil, float64(len(token))
}

func GetImageGenPrice(
	model string,
	pixels int,
	steps int,
) float64 {
	basePrice := float64(pixels) / 1000000.00 // 0.786432

	if model == "black-forest-labs/FLUX.1-schnell" {
		// 370 image per $, 4 default steps
		model_steps := 1.0 / 4.0 * float64(steps)
		return basePrice * 2702.70 * model_steps
	} else if model == "black-forest-labs/FLUX.1-dev" {
		// 40 image per $, 28 default steps
		model_steps := 1.0 / 28.0 * float64(steps)
		return basePrice * 25000.0 * model_steps
	} 

	return basePrice * 2702.70 * (1.0 / 4.0 * float64(steps))
}
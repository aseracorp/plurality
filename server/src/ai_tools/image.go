package ai_tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

const imagePathMaxBytes = 10 * 1024 * 1024 // 10 MB

// LiteLLMBaseURL is the URL of the local litellm proxy. Set by main from
// ai.LiteLLMBaseURL during bootstrap to avoid an import cycle with the ai
// package.
var LiteLLMBaseURL = "http://127.0.0.1:4000"

var ImageGenTool = utils.AITool{
	Name:              "Image Generation",
	Description:       "Generate an image from a text description using AI image generation",
	ToolID:            "generate_image",
	PickerLabel:       "Image Generation",
	PickerDescription: "Generate images from text descriptions",
	PickerDefault:     "on",
	PickerOrder:       50,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "generate_image",
			Description: "Generate or edit an image from a text description. Write a detailed prompt covering style, composition, and subject. To edit an existing image, pass its attachment ID in the 'attachment' parameter.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"prompt": {
						Type:        "string",
						Description: "Detailed image generation prompt",
					},
					"attachment": {
						Type:        "string",
						Description: "Optional attachment ID from the conversation (e.g. 'att_0') to edit. This is an ID, NOT a file path.",
					},
					"size": {
						Type:        "string",
						Description: "Optional output size as 'WIDTHxHEIGHT' (e.g. '1024x1024' square, '1024x1792' portrait, '1792x1024' landscape). Defaults to 1024x768. Ignored when editing an existing image, where the source aspect ratio is preserved.",
					},
				},
				Required: []string{"prompt"},
			},
		},
	},
	LoadingString: "Generating image: \"{{prompt}}\"",
	IconURL:       "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&",
}

// computeOutputDims preserves the source aspect ratio while keeping total
// pixels around 1024x768, rounded to multiples of 16 and clamped to [256, 2048].
func computeOutputDims(ratio float64) (int, int) {
	targetPixels := 1024.0 * 768.0
	h := int(math.Sqrt(targetPixels / ratio))
	w := int(float64(h) * ratio)
	w = (w + 8) / 16 * 16
	h = (h + 8) / 16 * 16
	if w < 256 {
		w = 256
	}
	if w > 2048 {
		w = 2048
	}
	if h < 256 {
		h = 256
	}
	if h > 2048 {
		h = 2048
	}
	return w, h
}

// loadImageFromPath reads a server-side image file and returns it as a data
// URI plus its aspect ratio (width / height). Reuses resolveServerPath from
// filesystem_server.go for '~'/relative path handling.
func loadImageFromPath(p string) (string, float64, error) {
	resolved, errMsg := resolveServerPath(p)
	if errMsg != "" {
		return "", 0, fmt.Errorf("%s", errMsg)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, err
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("path is a directory: %s", resolved)
	}
	if info.Size() > imagePathMaxBytes {
		return "", 0, fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), imagePathMaxBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", 0, err
	}
	mimeType := sniffImageMime(data)
	if mimeType == "" {
		return "", 0, fmt.Errorf("file does not appear to be a supported image (PNG/JPEG/WebP/GIF)")
	}
	aspect := 4.0 / 3.0
	if cfg, _, decErr := image.DecodeConfig(bytes.NewReader(data)); decErr == nil && cfg.Width > 0 && cfg.Height > 0 {
		aspect = float64(cfg.Width) / float64(cfg.Height)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), aspect, nil
}

func sniffImageMime(b []byte) string {
	if len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF {
		return "image/jpeg"
	}
	if len(b) >= 4 && string(b[:4]) == "\x89PNG" {
		return "image/png"
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a") {
		return "image/gif"
	}
	return ""
}

// decodeDataURI parses a "data:<mime>;base64,<payload>" URI (as produced by
// ResolveAttachmentImage / loadImageFromPath) into raw bytes plus the mime type.
func decodeDataURI(uri string) ([]byte, string, error) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, "", fmt.Errorf("not a data URI")
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("malformed data URI")
	}
	meta := uri[len("data:"):comma] // e.g. "image/png;base64"
	mimeType := "image/png"
	if semi := strings.IndexByte(meta, ';'); semi >= 0 {
		if meta[:semi] != "" {
			mimeType = meta[:semi]
		}
	} else if meta != "" {
		mimeType = meta
	}
	data, err := base64.StdEncoding.DecodeString(uri[comma+1:])
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

// extForImageMime returns a file extension for a multipart filename so the
// upstream provider can sniff the format.
func extForImageMime(m string) string {
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
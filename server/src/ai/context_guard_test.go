package ai

import (
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// TestStripImagesForNonVisionModel verifies that image parts are replaced
// with a lightweight placeholder (never shipped as base64 to a model that
// cannot see them), while text parts and role/turn boundaries survive.
func TestStripImagesForNonVisionModel(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "here is a screenshot"},
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: "data:image/png;base64,AAAA...."}},
		})},
		{Role: "user", Content: utils.NewTextContent("plain text stays")},
	}
	out := stripImagesForNonVisionModel(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	parts := out[0].ContentParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts after strip, got %d", len(parts))
	}
	for _, p := range parts {
		if p.Type == "image_url" {
			t.Fatalf("image part survived strip: %+v", p)
		}
	}
	if out[1].TextContent() != "plain text stays" {
		t.Fatalf("text message was altered: %q", out[1].TextContent())
	}
}

// TestEstimateImageTokens verifies the image base64 payload is counted in
// the eco overhead so the tail budget reserves room for it.
func TestEstimateImageTokens(t *testing.T) {
	// A ~4KB base64 payload ≈ ~1k tokens at ~4 chars/token.
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = 'A'
	}
	u := "data:image/png;base64," + string(payload)
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "t"},
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: u}},
		})},
	}
	tokens := estimateImageTokens(msgs)
	if tokens < 900 || tokens > 1100 {
		t.Fatalf("estimateImageTokens = %d, want ~1024 for a 4096-byte base64 payload", tokens)
	}
}
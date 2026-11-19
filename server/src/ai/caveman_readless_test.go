package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// seedCavemanModel registers a model entry with the caveman stones set as
// requested, and removes it when the test ends. Mirrors the direct-map seeding
// used by model_registry_parse_test.go.
func seedCavemanModel(t *testing.T, name string, enabled, readLess bool) {
	t.Helper()
	Models.byName[name] = ModelEntry{
		Name:    name,
		Mode:    "chat",
		Caveman: CavemanSettings{Enabled: enabled, Stones: CavemanStones{ReadLess: readLess}},
	}
	t.Cleanup(func() { delete(Models.byName, name) })
}

func toolMessages() []utils.Message {
	original := strings.Repeat("2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n", 200)
	projection := "[caveman: compressed tool result, 12400 chars → 140. Full original: call caveman_retrieve with tool_call_id=\"call_1\".]\n" +
		"2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n  ^ repeated x200"
	return []utils.Message{
		{Role: "user", Content: utils.NewTextContent("check the build")},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: "call_1", Type: "function", Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Name: "system_tools__shell_exec", Content: utils.NewTextContent(original), CompressedContent: projection},
	}
}

// contentKey renders a request message deterministically so two renderings can
// be compared byte-for-byte (Content is string OR []StandardContentReq).
func contentKey(t *testing.T, m StandardMessageReq) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The gate: with read_less on, the request ships the projection.
func TestCavemanReadLessShipsProjectionWhenEnabled(t *testing.T) {
	seedCavemanModel(t, "cav-on", true, true)

	out, _ := convertMessagesToOpenAI(toolMessages(), utils.Model{Name: "cav-on"})
	tool := findToolMessage(t, out)

	if !strings.Contains(tool, "caveman_retrieve") {
		t.Fatal("expected the projection banner in the request")
	}
	if n := strings.Count(tool, "100% done"); n > 2 {
		t.Fatalf("raw repetitions present (count=%d); projection not applied", n)
	}
}

// The gate: with read_less off (or caveman disabled), the model sees raw body.
func TestCavemanReadLessOffShipsRawBody(t *testing.T) {
	seedCavemanModel(t, "cav-off", true, false)
	seedCavemanModel(t, "cav-disabled", false, true)

	for _, name := range []string{"cav-off", "cav-disabled"} {
		out, _ := convertMessagesToOpenAI(toolMessages(), utils.Model{Name: name})
		tool := findToolMessage(t, out)
		if strings.Contains(tool, "caveman_retrieve") {
			t.Fatalf("%s: projection shipped while read_less is off", name)
		}
		if n := strings.Count(tool, "100% done"); n != 200 {
			t.Fatalf("%s: expected raw body (200 lines), got count=%d", name, n)
		}
	}
}

// Unknown models must never get a partial optimization.
func TestCavemanUnknownModelIsVerbatim(t *testing.T) {
	out, _ := convertMessagesToOpenAI(toolMessages(), utils.Model{Name: "not-registered-model"})
	if strings.Contains(findToolMessage(t, out), "caveman_retrieve") {
		t.Fatal("unknown model received a projection")
	}
}

// Byte-stability across turns — the property that keeps the KV prompt-cache
// prefix reusable: rendering stored messages at turn N and at turn N+1 (after
// new messages are appended) must agree byte-for-byte on the shared prefix.
func TestReadLessRenderingIsStableAcrossTurns(t *testing.T) {
	seedCavemanModel(t, "cav-stable", true, true)
	msgs := toolMessages()
	model := utils.Model{Name: "cav-stable"}

	first, _ := convertMessagesToOpenAI(msgs, model)

	extended := append(append([]utils.Message{}, msgs...),
		utils.Message{Role: "assistant", Content: utils.NewTextContent("build is fine")},
		utils.Message{Role: "user", Content: utils.NewTextContent("and now?")},
	)
	second, _ := convertMessagesToOpenAI(extended, model)

	if len(second) < len(first) {
		t.Fatalf("second rendering shorter: %d vs %d", len(second), len(first))
	}
	for i := range first {
		if a, b := contentKey(t, first[i]), contentKey(t, second[i]); a != b {
			t.Fatalf("message %d changed bytes between turns — KV cache prefix would churn", i)
		}
	}
}

func findToolMessage(t *testing.T, msgs []StandardMessageReq) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "tool" {
			if s, ok := m.Content.(string); ok {
				return s
			}
			t.Fatal("tool content is not a plain string")
		}
	}
	t.Fatal("no tool message in output")
	return ""
}

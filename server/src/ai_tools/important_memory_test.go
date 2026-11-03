package ai_tools

import "testing"

func TestSanitizeImportantMemory_AcceptsDurableFacts(t *testing.T) {
	cases := []string{
		"Martin Raschle, email martin.raschle@example.ch",
		"Homelab: pve01 (Proxmox 8.3, 125 GiB RAM) + pve02. ZFS rpool, ARC capped 12.8 GiB.",
		"Prefers German and English, concise answers, markdown tables.",
		"User runs Plurality (Simplify container) on samOS, Cosmos host kalavallerie.ch.",
		"Name: Sam\nEmail: sam@example.org\nStanding rule: always confirm destructive actions.",
	}
	for _, c := range cases {
		got, reason := sanitizeImportantMemory(c)
		if got != c || reason != "" {
			t.Errorf("durable fact rejected: %q -> reason=%q", c, reason)
		}
	}
}

func TestSanitizeImportantMemory_RejectsTaskScratch(t *testing.T) {
	cases := []string{
		"DONE: step 1\nNEXT: deploy the fix",
		"- [ ] setup zram\n- [x] apply oom config",
		"CUTOVER: switch database to postgres",
		"14:32 UTC: started compaction",
		"current time: 14:32",
		"today is Nov 3",
	}
	for _, c := range cases {
		_, reason := sanitizeImportantMemory(c)
		if reason == "" {
			t.Errorf("task scratch NOT rejected: %q", c)
		}
	}
}

func TestSanitizeImportantMemory_RejectsEmptyAndOversized(t *testing.T) {
	if _, reason := sanitizeImportantMemory("   \n\t "); reason == "" {
		t.Error("blank content should be rejected")
	}
	long := make([]rune, ImportantMemoryMaxChars+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, reason := sanitizeImportantMemory(string(long)); reason == "" {
		t.Error("oversized content should be rejected")
	}
	// Exactly at the cap is fine.
	ok := make([]rune, ImportantMemoryMaxChars)
	for i := range ok {
		ok[i] = 'x'
	}
	if got, reason := sanitizeImportantMemory(string(ok)); reason != "" || len([]rune(got)) != ImportantMemoryMaxChars {
		t.Errorf("content at cap should be accepted: reason=%q", reason)
	}
}

func TestSanitizeImportantMemory_AllowsBorderlineWords(t *testing.T) {
	cases := []string{
		"User reports the fix in progress; likes notifications about failures.",
		"update: the docs describe important_memory as global.",
		"Birthday November 3, prefers mail.",
	}
	for _, c := range cases {
		got, reason := sanitizeImportantMemory(c)
		if got == "" || reason != "" {
			t.Errorf("borderline durable fact wrongly rejected: %q -> %q", c, reason)
		}
	}
}

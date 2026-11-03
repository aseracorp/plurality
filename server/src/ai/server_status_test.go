package ai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcMeminfoReadsRealSemantics(t *testing.T) {
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("/proc/meminfo not available on this platform")
	}
	m, err := procMeminfo()
	if err != nil {
		t.Fatalf("procMeminfo error: %v", err)
	}
	if m.Total == 0 {
		t.Error("MemTotal should be non-zero")
	}
	if m.Free == 0 {
		t.Error("MemFree should be non-zero on a real host")
	}
	if m.Available == 0 {
		t.Error("MemAvailable should be non-zero")
	}
	// Core invariant: Available >= Free (Available = Free + reclaimable cache).
	if m.Available < m.Free {
		t.Errorf("MemAvailable (%d) must be >= MemFree (%d)", m.Available, m.Free)
	}
	if m.Available > m.Total {
		t.Errorf("MemAvailable (%d) cannot exceed MemTotal (%d)", m.Available, m.Total)
	}
	// Used = Total - Available.
	if m.Used != m.Total-m.Available {
		t.Errorf("Used (%d) should equal Total-Available (%d)", m.Used, m.Total-m.Available)
	}
	// UsedExclCache <= Used.
	if m.UsedExclCache > m.Used {
		t.Errorf("UsedExclCache (%d) cannot exceed Used (%d)", m.UsedExclCache, m.Used)
	}
}

func TestProcMeminfoParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	content := `MemTotal:       1048576 kB
MemFree:         262144 kB
MemAvailable:    524288 kB
Buffers:          65536 kB
Cached:          131072 kB
SwapTotal:       1048576 kB
SwapFree:        524288 kB
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := procMeminfoPath(p)
	if err != nil {
		t.Fatal(err)
	}
	// Values are specified in kB; parsed output must be bytes.
	if m.Total != 1048576*1024 {
		t.Errorf("Total: got %d want %d", m.Total, 1048576*1024)
	}
	if m.Free != 262144*1024 {
		t.Errorf("Free: got %d want %d", m.Free, 262144*1024)
	}
	if m.Available != 524288*1024 {
		t.Errorf("Available: got %d want %d", m.Available, 524288*1024)
	}
	if m.Cached != 131072*1024 {
		t.Errorf("Cached: got %d want %d", m.Cached, 131072*1024)
	}
	if m.SwapTotal != 1048576*1024 || m.SwapFree != 524288*1024 {
		t.Errorf("Swap: got %d/%d", m.SwapTotal, m.SwapFree)
	}
	// Used = Total - Available
	if m.Used != 1048576*1024-524288*1024 {
		t.Errorf("Used: got %d", m.Used)
	}
	// UsedExclCache = Used - (Buffers + Cached)
	expectExcl := uint64(1048576-524288-65536-131072) * 1024
	if m.UsedExclCache != expectExcl {
		t.Errorf("UsedExclCache: got %d want %d", m.UsedExclCache, expectExcl)
	}
}

package ai

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ServerStatus is the JSON payload for GET /server/status. It reports REAL
// resource semantics (the values the kernel actually means), which prevents
// the classic "my box is almost out of RAM" confusion:
//
//   - Available is MemAvailable, NOT MemFree. Free only counts zeroed pages;
//     the kernel can still reclaim page cache (Cached) and zram-swap out
//     pages, so Available is the number that answers "how much more can I
//     allocate right now?".
//   - Used reported here is Total − Available, which includes reclaimable
//     cache. On a Linux host that is NOT "this process's footprint".
//
// Everything is read from /proc/meminfo at request time; unit is bytes.
type ServerStatus struct {
	OK          bool        `json:"ok"`
	Hostname    string      `json:"hostname"`
	GoVersion   string      `json:"goVersion"`
	UptimeSecs  int64       `json:"uptimeSecs"`
	ProcessID   int         `json:"processId"`
	MemoryBytes MemoryBytes `json:"memoryBytes"`
}

type MemoryBytes struct {
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Available uint64 `json:"available"`
	Buffers   uint64 `json:"buffers"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swapTotal"`
	SwapFree  uint64 `json:"swapFree"`
	// Used = Total − Available (includes reclaimable cache + kernel heuristics).
	Used uint64 `json:"used"`
	// UsedExclCache = Used − (Buffers + Cached), a better approximation of
	// what applications actually hold.
	UsedExclCache uint64 `json:"usedExclCache"`
}

func procMeminfo() (MemoryBytes, error) {
	return procMeminfoPath("/proc/meminfo")
}

// procMeminfoPath parses a meminfo-style file (exposed so tests can feed a
// synthetic file). Unit: bytes.
func procMeminfoPath(path string) (MemoryBytes, error) {
	var m MemoryBytes
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	parse := func(key string) uint64 {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, key+":") {
				fields := strings.Fields(line[len(key)+1:])
				if len(fields) > 0 {
					v, _ := strconv.ParseUint(fields[0], 10, 64)
					return v * 1024 // kB → bytes
				}
			}
		}
		return 0
	}
	m.Total = parse("MemTotal")
	m.Free = parse("MemFree")
	m.Available = parse("MemAvailable")
	m.Buffers = parse("Buffers")
	m.Cached = parse("Cached")
	m.SwapTotal = parse("SwapTotal")
	m.SwapFree = parse("SwapFree")
	if m.Available > 0 {
		m.Used = m.Total - m.Available
	}
	if m.Used > m.Buffers+m.Cached {
		m.UsedExclCache = m.Used - m.Buffers - m.Cached
	}
	return m, nil
}

func procUptimeSecs() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(fields[0], 64)
	return int64(f)
}

// HandleServerStatus is an authenticated diagnostic endpoint. Unlike the
// unauthenticated /health liveness probe, this reports real resource
// semantics so operators can tell "out of memory" from "everything is fine"
// without parsing /proc/meminfo by hand (MemFree ≠ MemAvailable).
func HandleServerStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mem, err := procMeminfo()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"ok": "false", "error": err.Error()})
		return
	}
	hostname, _ := os.Hostname()
	json.NewEncoder(w).Encode(ServerStatus{
		OK:          true,
		Hostname:    hostname,
		GoVersion:   runtime.Version(),
		ProcessID:   os.Getpid(),
		MemoryBytes: mem,
	})
}

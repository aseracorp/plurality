package ai_tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	bgStdoutLimit = 1024 * 1024 // 1 MiB
	bgStderrLimit = 256 * 1024  // 256 KiB
	bgRetention   = time.Hour   // keep completed tasks readable for 1h
	bgGCInterval  = 15 * time.Minute
)

// capBuffer is a goroutine-safe byte buffer with a fixed maximum size.
// When more bytes are written than `limit`, the oldest bytes are dropped so
// the buffer always retains the most recent `limit` bytes. The exec.Cmd
// internal copier writes into it directly, which is why Write is mutex-locked.
type capBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		excess := len(b.buf) - b.limit
		// Slide the kept window down so the underlying array doesn't grow
		// unbounded across many drops.
		copy(b.buf, b.buf[excess:])
		b.buf = b.buf[:b.limit]
	}
	return len(p), nil
}

func (b *capBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n >= len(b.buf) {
		return string(b.buf)
	}
	return string(b.buf[len(b.buf)-n:])
}

func (b *capBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// bgProcess tracks one detached shell command spawned by the AI.
type bgProcess struct {
	mu        sync.Mutex
	taskID    string
	command   string
	pwd       string
	cmd       *exec.Cmd
	stdout    *capBuffer
	stderr    *capBuffer
	startedAt time.Time
	endedAt   time.Time // zero while running
	state     string    // "running" | "exited" | "killed" | "error"
	exitCode  int       // -1 while running
	errMsg    string
	done      chan struct{}
}

func (bp *bgProcess) snapshot() bgProcessSnapshot {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	pid := 0
	if bp.cmd != nil && bp.cmd.Process != nil {
		pid = bp.cmd.Process.Pid
	}
	end := bp.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	return bgProcessSnapshot{
		TaskID:    bp.taskID,
		Command:   bp.command,
		Pwd:       bp.pwd,
		PID:       pid,
		State:     bp.state,
		ExitCode:  bp.exitCode,
		ErrMsg:    bp.errMsg,
		StartedAt: bp.startedAt,
		EndedAt:   bp.endedAt,
		Duration:  end.Sub(bp.startedAt),
	}
}

type bgProcessSnapshot struct {
	TaskID    string
	Command   string
	Pwd       string
	PID       int
	State     string
	ExitCode  int
	ErrMsg    string
	StartedAt time.Time
	EndedAt   time.Time
	Duration  time.Duration
}

var (
	bgRegistry sync.Map // task_id (string) -> *bgProcess
	bgGCOnce   sync.Once
)

func newTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fall back to timestamp-derived id; collisions still highly unlikely
		// at the granularity of a single conversation.
		return fmt.Sprintf("t%x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// registerBackground spawns `command` detached from the per-turn context and
// records it in the registry. The wait goroutine updates terminal state and
// closes done once the process exits (whether normally, via error, or via
// Kill).
func registerBackground(command, pwd string) (*bgProcess, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if pwd != "" {
		cmd.Dir = pwd
	}
	stdout := &capBuffer{limit: bgStdoutLimit}
	stderr := &capBuffer{limit: bgStderrLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	bp := &bgProcess{
		taskID:    newTaskID(),
		command:   command,
		pwd:       pwd,
		cmd:       cmd,
		stdout:    stdout,
		stderr:    stderr,
		startedAt: time.Now(),
		state:     "running",
		exitCode:  -1,
		done:      make(chan struct{}),
	}
	bgRegistry.Store(bp.taskID, bp)

	go func() {
		err := cmd.Wait()
		bp.mu.Lock()
		bp.endedAt = time.Now()
		// State priority: a Kill() call wins regardless of how Wait reports
		// it, so a user-initiated kill always shows as "killed".
		if bp.state != "killed" {
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					bp.exitCode = exitErr.ExitCode()
					bp.state = "exited"
				} else {
					bp.state = "error"
					bp.errMsg = err.Error()
				}
			} else {
				if cmd.ProcessState != nil {
					bp.exitCode = cmd.ProcessState.ExitCode()
				} else {
					bp.exitCode = 0
				}
				bp.state = "exited"
			}
		} else if cmd.ProcessState != nil {
			bp.exitCode = cmd.ProcessState.ExitCode()
		}
		close(bp.done)
		bp.mu.Unlock()
	}()

	startBackgroundGCOnce()
	return bp, nil
}

func getBackground(taskID string) (*bgProcess, bool) {
	v, ok := bgRegistry.Load(taskID)
	if !ok {
		return nil, false
	}
	return v.(*bgProcess), true
}

func listBackground() []bgProcessSnapshot {
	var out []bgProcessSnapshot
	bgRegistry.Range(func(_, v interface{}) bool {
		bp := v.(*bgProcess)
		out = append(out, bp.snapshot())
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// killBackground sets state to "killed" and signals the process. If the task
// is already finished it's a no-op. Returns the final snapshot after waiting
// briefly for the wait goroutine to settle exit code; if the goroutine doesn't
// settle within the timeout the current snapshot is returned anyway.
func killBackground(taskID string, settleTimeout time.Duration) (bgProcessSnapshot, bool) {
	bp, ok := getBackground(taskID)
	if !ok {
		return bgProcessSnapshot{}, false
	}
	bp.mu.Lock()
	alreadyDone := bp.state != "running"
	if !alreadyDone {
		bp.state = "killed"
	}
	proc := bp.cmd.Process
	done := bp.done
	bp.mu.Unlock()

	if !alreadyDone && proc != nil {
		_ = proc.Kill()
	}

	if !alreadyDone {
		select {
		case <-done:
		case <-time.After(settleTimeout):
		}
	}
	return bp.snapshot(), true
}

func startBackgroundGCOnce() {
	bgGCOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(bgGCInterval)
			defer ticker.Stop()
			for range ticker.C {
				cutoff := time.Now().Add(-bgRetention)
				bgRegistry.Range(func(k, v interface{}) bool {
					bp := v.(*bgProcess)
					bp.mu.Lock()
					completed := !bp.endedAt.IsZero() && bp.endedAt.Before(cutoff)
					bp.mu.Unlock()
					if completed {
						bgRegistry.Delete(k)
					}
					return true
				})
			}
		}()
	})
}

package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"github.com/google/uuid"
)

const requestTimeout = 30 * time.Second

const maxLogLines = 1000

// ProcessManager runs one MCP server subprocess and speaks newline-delimited
// JSON-RPC 2.0 over its stdio. Ported from client/lib/api/process.dart.
type ProcessManager struct {
	name    string
	command string
	args    []string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu       sync.Mutex
	pending  map[string]chan rpcResult
	running  bool
	exited   chan struct{}
	logLines []string // circular buffer of recent stderr lines
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// NewProcessManager builds a manager but does NOT spawn the process.
func NewProcessManager(name, command string, args []string) *ProcessManager {
	return &ProcessManager{
		name:    name,
		command: command,
		args:    args,
		pending: make(map[string]chan rpcResult),
		exited:  make(chan struct{}),
	}
}

// Start spawns the subprocess and starts the reader goroutines.
func (p *ProcessManager) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("process %s already running", p.name)
	}

	cmd := exec.Command(p.command, p.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting process: %w", err)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.stdout = stdout
	p.stderr = stderr
	p.running = true

	go p.readStdout()
	go p.readStderr()
	go p.watchExit()

	return nil
}

// IsRunning reports whether the subprocess is still running.
func (p *ProcessManager) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Stop kills the subprocess and fails any pending requests.
func (p *ProcessManager) Stop() {
	p.mu.Lock()
	if !p.running || p.cmd == nil || p.cmd.Process == nil {
		p.mu.Unlock()
		return
	}
	p.cmd.Process.Kill()
	p.mu.Unlock()

	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
	}
}

// SendRequest issues a JSON-RPC call and waits up to requestTimeout.
func (p *ProcessManager) SendRequest(method string, params interface{}) (json.RawMessage, error) {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return nil, fmt.Errorf("process %s is not running", p.name)
	}
	id := uuid.NewString()
	ch := make(chan rpcResult, 1)
	p.pending[id] = ch
	stdin := p.stdin
	p.mu.Unlock()

	if params == nil {
		params = map[string]interface{}{}
	}
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		p.removePending(id)
		return nil, err
	}

	if _, err := stdin.Write(append(body, '\n')); err != nil {
		p.removePending(id)
		return nil, fmt.Errorf("writing to stdin: %w", err)
	}

	select {
	case res := <-ch:
		return res.result, res.err
	case <-time.After(requestTimeout):
		p.removePending(id)
		return nil, fmt.Errorf("request timed out: %s", method)
	}
}

func (p *ProcessManager) removePending(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *ProcessManager) readStdout() {
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Not valid JSON-RPC, log it as output
			p.appendLog("[stdout] " + string(line))
			continue
		}
		if resp.ID == "" {
			// Notification or malformed response, log it
			p.appendLog("[stdout] " + string(line))
			continue
		}
		p.mu.Lock()
		ch, ok := p.pending[resp.ID]
		if ok {
			delete(p.pending, resp.ID)
		}
		p.mu.Unlock()
		if !ok {
			// Response for unknown request, log it
			p.appendLog("[stdout] " + string(line))
			continue
		}
		if resp.Error != nil {
			ch <- rpcResult{err: fmt.Errorf("[mcp:%s] %d: %s", p.name, resp.Error.Code, resp.Error.Message)}
		} else {
			ch <- rpcResult{result: resp.Result}
		}
	}
}

func (p *ProcessManager) readStderr() {
	scanner := bufio.NewScanner(p.stderr)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		utils.Log("[mcp:%s stderr] %s", p.name, line)
		p.appendLog("[stderr] " + line)
	}
}

// appendLog adds a line to the circular log buffer.
func (p *ProcessManager) appendLog(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.logLines) >= maxLogLines {
		p.logLines = p.logLines[1:]
	}
	p.logLines = append(p.logLines, line)
}

// GetLogs returns a copy of the recent log lines.
func (p *ProcessManager) GetLogs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.logLines))
	copy(out, p.logLines)
	return out
}

func (p *ProcessManager) watchExit() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.running = false
	pending := p.pending
	p.pending = make(map[string]chan rpcResult)
	p.mu.Unlock()

	if err != nil {
		utils.Error("[mcp:"+p.name+"] process exited", err)
	} else {
		utils.Log("[mcp:%s] process exited cleanly", p.name)
	}
	for _, ch := range pending {
		ch <- rpcResult{err: fmt.Errorf("process %s terminated", p.name)}
	}
	close(p.exited)
}

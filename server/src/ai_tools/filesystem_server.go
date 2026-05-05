package ai_tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

const (
	fsServerMaxReadBytes  = 200 * 1024 // 200 KB cap on file reads
	fsServerMaxListEntries = 500
	fsServerMaxFindResults = 500
)

// FsServerReadTool exposes find/list/read/read_segment/stat over the server filesystem.
var FsServerReadTool = utils.AITool{
	Name:        "Read Files (Server)",
	Description: "List, find, read files on the server",
	ToolID:      "fs_read",
	BundleName:  "filesystem_server",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "fs_read",
			Description: "Read filesystem on the server. Set 'op' to one of: list (directory entries), find (recursive name pattern match), read (whole file as text), read_segment (line range of a text file), stat (file metadata).",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"op": {
						Type:        "string",
						Description: "Operation: list | find | read | read_segment | stat",
						Enum:        []string{"list", "find", "read", "read_segment", "stat"},
					},
					"path": {
						Type:        "string",
						Description: "Path on the server filesystem. Absolute paths used as-is. '~' or '~/foo' expand to the home directory. Other relative paths are resolved relative to the user home of the server process. For 'find', this is the root to search under.",
					},
					"pattern": {
						Type:        "string",
						Description: "For 'find': glob pattern matched against entry names (e.g. '*.go', 'README*').",
					},
					"recursive": {
						Type:        "string",
						Description: "For 'list': 'true' to recurse, 'false' for shallow listing (default: false).",
					},
					"start_line": {
						Type:        "integer",
						Description: "For 'read_segment': 1-based starting line (inclusive).",
					},
					"end_line": {
						Type:        "integer",
						Description: "For 'read_segment': 1-based ending line (inclusive). 0 or omitted means to end of file.",
					},
				},
				Required: []string{"op", "path"},
			},
		},
	},
	LoadingString: "Reading {{path}}",
	Exec:          execFsServerRead,
}

// FsServerWriteTool exposes create/edit/copy/move/delete/mkdir over the server filesystem.
var FsServerWriteTool = utils.AITool{
	Name:        "Write Files (Server)",
	Description: "Edit, copy, move, delete files on the server",
	ToolID:      "fs_write",
	BundleName:  "filesystem_server",
	Cost:        0,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "fs_write",
			Description: "Modify the server filesystem. Set 'op' to one of: create (write a new file, fails if exists), edit (search-and-replace inside an existing file), copy, move, delete, mkdir.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"op": {
						Type:        "string",
						Description: "Operation: create | edit | copy | move | delete | mkdir",
						Enum:        []string{"create", "edit", "copy", "move", "delete", "mkdir"},
					},
					"path": {
						Type:        "string",
						Description: "Target path on the server filesystem. Absolute paths used as-is. '~' or '~/foo' expand to the home directory. Other relative paths are resolved relative to the user home of the server process.",
					},
					"dest_path": {
						Type:        "string",
						Description: "For 'copy' and 'move': destination path. Same resolution rules as 'path'.",
					},
					"content": {
						Type:        "string",
						Description: "For 'create': the file's text content.",
					},
					"old_text": {
						Type:        "string",
						Description: "For 'edit': literal substring to find. Must occur exactly once in the file.",
					},
					"new_text": {
						Type:        "string",
						Description: "For 'edit': replacement text.",
					},
				},
				Required: []string{"op", "path"},
			},
		},
	},
	LoadingString: "Writing {{path}}",
	Exec:          execFsServerWrite,
}

func execFsServerRead(ctx context.Context, input string, conv utils.Conversation) utils.MessageContent {
	parsed := utils.ParseJson(input)
	op, _ := parsed["op"].(string)
	rawPath, _ := parsed["path"].(string)

	if op == "" {
		return utils.NewTextContent("Error: 'op' is required")
	}
	if rawPath == "" {
		return utils.NewTextContent("Error: 'path' is required")
	}
	path, perr := resolveServerPath(rawPath)
	if perr != "" {
		return utils.NewTextContent(perr)
	}

	switch op {
	case "list":
		recursive := strings.EqualFold(asString(parsed["recursive"]), "true")
		return utils.NewTextContent(fsList(path, recursive))
	case "find":
		pattern, _ := parsed["pattern"].(string)
		if pattern == "" {
			return utils.NewTextContent("Error: 'pattern' is required for find")
		}
		return utils.NewTextContent(fsFind(path, pattern))
	case "read":
		return utils.NewTextContent(fsRead(path))
	case "read_segment":
		start := asInt(parsed["start_line"])
		end := asInt(parsed["end_line"])
		return utils.NewTextContent(fsReadSegment(path, start, end))
	case "stat":
		return utils.NewTextContent(fsStat(path))
	default:
		return utils.NewTextContent(fmt.Sprintf("Error: unknown op %q", op))
	}
}

func execFsServerWrite(ctx context.Context, input string, conv utils.Conversation) utils.MessageContent {
	parsed := utils.ParseJson(input)
	op, _ := parsed["op"].(string)
	rawPath, _ := parsed["path"].(string)

	if op == "" {
		return utils.NewTextContent("Error: 'op' is required")
	}
	if rawPath == "" {
		return utils.NewTextContent("Error: 'path' is required")
	}
	path, perr := resolveServerPath(rawPath)
	if perr != "" {
		return utils.NewTextContent(perr)
	}

	resolveDest := func() (string, string) {
		raw, _ := parsed["dest_path"].(string)
		if raw == "" {
			return "", fmt.Sprintf("Error: 'dest_path' is required for %s", op)
		}
		d, e := resolveServerPath(raw)
		return d, e
	}

	switch op {
	case "create":
		content, _ := parsed["content"].(string)
		return utils.NewTextContent(fsCreate(path, content))
	case "edit":
		oldText, _ := parsed["old_text"].(string)
		newText, _ := parsed["new_text"].(string)
		if oldText == "" {
			return utils.NewTextContent("Error: 'old_text' is required for edit")
		}
		return utils.NewTextContent(fsEdit(path, oldText, newText))
	case "copy":
		dest, derr := resolveDest()
		if derr != "" {
			return utils.NewTextContent(derr)
		}
		return utils.NewTextContent(fsCopy(path, dest))
	case "move":
		dest, derr := resolveDest()
		if derr != "" {
			return utils.NewTextContent(derr)
		}
		return utils.NewTextContent(fsMove(path, dest))
	case "delete":
		return utils.NewTextContent(fsDelete(path))
	case "mkdir":
		return utils.NewTextContent(fsMkdir(path))
	default:
		return utils.NewTextContent(fmt.Sprintf("Error: unknown op %q", op))
	}
}

// resolveServerPath expands '~' / '~/foo' to the home directory, and resolves
// other non-absolute paths relative to the home directory of the user running
// the server process (rather than the server binary's CWD, which is rarely
// where the user expects to act). Absolute paths pass through untouched.
//
// Returns (resolvedPath, "") on success, or ("", errMessage) on failure.
func resolveServerPath(p string) (string, string) {
	if p == "" {
		return "", "Error: 'path' is required"
	}
	if filepath.IsAbs(p) {
		return p, ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Sprintf("Error: cannot resolve '~' / relative path — user home unavailable (%v)", err)
	}
	if p == "~" {
		return home, ""
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:]), ""
	}
	return filepath.Join(home, p), ""
}

// --- Pure-stdlib helpers shared between server and (would-be) other consumers ---

func fsList(root string, recursive bool) string {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("Error: %q is not a directory", root)
	}

	var entries []string
	count := 0
	truncated := false
	if recursive {
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				entries = append(entries, fmt.Sprintf("? %s (error: %s)", p, err))
				return nil
			}
			if p == root {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			marker := "f"
			if d.IsDir() {
				marker = "d"
			}
			entries = append(entries, fmt.Sprintf("%s %s", marker, rel))
			count++
			if count >= fsServerMaxListEntries {
				truncated = true
				return io.EOF
			}
			return nil
		})
		if walkErr != nil && walkErr != io.EOF {
			return fmt.Sprintf("Error: %s", walkErr)
		}
	} else {
		dirEntries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Sprintf("Error: %s", err)
		}
		for _, e := range dirEntries {
			marker := "f"
			if e.IsDir() {
				marker = "d"
			}
			entries = append(entries, fmt.Sprintf("%s %s", marker, e.Name()))
			count++
			if count >= fsServerMaxListEntries {
				truncated = true
				break
			}
		}
	}

	sort.Strings(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "Listing of %s (%d entries%s):\n", root, len(entries),
		map[bool]string{true: ", truncated", false: ""}[truncated])
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return b.String()
}

func fsFind(root, pattern string) string {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("Error: %q is not a directory", root)
	}

	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		matched, mErr := filepath.Match(pattern, name)
		if mErr != nil {
			return mErr
		}
		if matched {
			rel, _ := filepath.Rel(root, p)
			matches = append(matches, rel)
			if len(matches) >= fsServerMaxFindResults {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != io.EOF {
		return fmt.Sprintf("Error: %s", walkErr)
	}

	sort.Strings(matches)
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d match(es) for pattern %q under %s%s:\n",
		len(matches), pattern, root,
		map[bool]string{true: " (truncated)", false: ""}[truncated])
	for _, m := range matches {
		b.WriteString(m)
		b.WriteByte('\n')
	}
	return b.String()
}

func fsRead(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	defer f.Close()

	buf := make([]byte, fsServerMaxReadBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Sprintf("Error: %s", err)
	}

	truncated := n > fsServerMaxReadBytes
	if truncated {
		n = fsServerMaxReadBytes
	}
	out := string(buf[:n])
	if truncated {
		out += fmt.Sprintf("\n\n[Content truncated — file exceeds %d bytes]", fsServerMaxReadBytes)
	}
	return out
}

func fsReadSegment(path string, start, end int) string {
	if start <= 0 {
		start = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var b strings.Builder
	line := 0
	emitted := 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if end > 0 && line > end {
			break
		}
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
		emitted++
		if b.Len() > fsServerMaxReadBytes {
			b.WriteString(fmt.Sprintf("\n[Truncated — segment exceeds %d bytes]\n", fsServerMaxReadBytes))
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("Error reading: %s", err)
	}
	return fmt.Sprintf("Lines %d..%d of %s (%d emitted):\n%s", start, line, path, emitted, b.String())
}

func fsStat(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	out := map[string]interface{}{
		"path":     path,
		"name":     info.Name(),
		"kind":     kind,
		"size":     info.Size(),
		"mode":     info.Mode().String(),
		"modified": info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data)
}

func fsCreate(path, content string) string {
	if _, err := os.Stat(path); err == nil {
		return fmt.Sprintf("Error: %q already exists. Use op=edit to modify.", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Sprintf("Error creating parent directory: %s", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("Error writing file: %s", err)
	}
	return fmt.Sprintf("Created %s (%d bytes)", path, len(content))
}

func fsEdit(path, oldText, newText string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	body := string(data)
	count := strings.Count(body, oldText)
	if count == 0 {
		return "Error: 'old_text' was not found in the file."
	}
	if count > 1 {
		return fmt.Sprintf("Error: 'old_text' occurs %d times. Provide more surrounding context so it matches exactly once.", count)
	}
	body = strings.Replace(body, oldText, newText, 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Sprintf("Error writing file: %s", err)
	}
	return fmt.Sprintf("Edited %s (%d bytes after change)", path, len(body))
}

func fsCopy(src, dst string) string {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) string {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Sprintf("Error opening source: %s", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Sprintf("Error creating destination directory: %s", err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Sprintf("Error creating destination: %s", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Sprintf("Error copying: %s", err)
	}
	return fmt.Sprintf("Copied %s → %s", src, dst)
}

func copyDir(src, dst string) string {
	walkErr := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if walkErr != nil {
		return fmt.Sprintf("Error copying directory: %s", walkErr)
	}
	return fmt.Sprintf("Copied directory %s → %s", src, dst)
}

func fsMove(src, dst string) string {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Sprintf("Error creating destination directory: %s", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Moved %s → %s", src, dst)
}

func fsDelete(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Sprintf("Error: %s", err)
		}
		if len(entries) > 0 {
			return fmt.Sprintf("Error: directory %q is not empty (%d entries). Refusing to delete.", path, len(entries))
		}
		if err := os.Remove(path); err != nil {
			return fmt.Sprintf("Error: %s", err)
		}
		return fmt.Sprintf("Deleted directory %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Deleted %s", path)
}

func fsMkdir(path string) string {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Created directory %s", path)
}

// --- arg coercion helpers ---

func asString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

func asInt(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

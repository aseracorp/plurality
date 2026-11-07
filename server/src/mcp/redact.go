package mcp

import (
	"regexp"
	"strings"
)

// RedactedPlaceholder replaces every credential that leaves the mcp package
// toward a model, a log line, or the debug-log buffer.
const RedactedPlaceholder = "***REDACTED***"

// secretKeySegments are the underscore/dash-separated words that mark an
// env-var name (or JSON key) as carrying a credential. Matching on whole
// segments — not substrings — keeps benign names like PATH intact while
// catching GITHUB_PERSONAL_ACCESS_TOKEN and OPENAI_API_KEY.
var secretKeySegments = map[string]bool{
	"token": true, "secret": true, "password": true, "passwd": true,
	"pass": true, "pat": true, "key": true, "apikey": true,
	"authorization": true, "auth": true, "credential": true,
	"credentials": true, "private": true, "access": true,
	"bearer": true, "session": true, "cookie": true,
}

// looksLikeSecretKey reports whether an env-var name / JSON key is likely to
// hold a credential.
func looksLikeSecretKey(name string) bool {
	lower := strings.ToLower(name)
	// Fast path for the most common exact names.
	switch lower {
	case "pat", "gh_token", "github_token", "password", "passwd":
		return true
	}
	for _, seg := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	}) {
		if secretKeySegments[seg] {
			return true
		}
	}
	return false
}

// --- pattern-based scrubbing (for arbitrary log / command text) ---

// urlUserinfoRe matches "scheme://user:password@" — the form git uses when a
// PAT is embedded in a clone URL.
var urlUserinfoRe = regexp.MustCompile(`(:\/\/[A-Za-z0-9._%+\-]+:)([^@\s/"']+)(@)`)

// tokenValueRe matches well-known opaque token shapes (GitHub PATs, fine
// grained PATs, Slack, AWS-style keys).
var tokenValueRe = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}`)

// bearerRe matches "Authorization: Bearer <token>" style headers.
var bearerRe = regexp.MustCompile(`(?i)((?:bearer|basic|token)\s+)([A-Za-z0-9._~+/\-]{16,}={0,2})`)

// envAssignRe matches "NAME=value" in a command line, shell snippet, or log
// line. Value is scrubbed only when NAME looks like a credential.
var envAssignRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9_]{1,})=([^\s"',;]{6,})`)

// jsonSecretRe matches "key": "value" pairs whose KEY is a recognised
// credential name. The key list is deliberately narrow and closed: an
// open-ended heuristic over JSON keys would redact "model", "api_base_url"
// and "command" too, which would break the config-editing workflow this tool
// exists for. Anything not listed here is passed through unchanged.
var jsonSecretRe = regexp.MustCompile(`(?i)("(?:[a-z0-9_]*(?:token|secret|pass|pat|key|credential|authorization|bearer|session|cookie)[a-z0-9_]*)"\s*:\s*")([^"\n]*?)(")`)

// isEnvReference reports whether a value is a "${VAR}" indirection rather
// than a literal secret, in which case it is safe to leave visible.
func isEnvReference(v string) bool {
	return strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}")
}

// RestoreRedactedSecrets walks a newly submitted config (next) and replaces
// any value that equals RedactedPlaceholder with the real value from the
// config currently on disk (prev), matched by path.
//
// Without this, the read-modify-write cycle the manage_mcp tool is designed
// around (get -> edit -> set) would persist the placeholder over the user's
// real credentials and brick the server. Redaction protects what the model
// sees; this keeps what the config file keeps.
//
// Returns the number of secrets restored.
func RestoreRedactedSecrets(next, prev interface{}) int {
	restored := 0
	restoreInto(next, prev, &restored)
	return restored
}

// redactedValuePrefix splits a redacted string into the literal text that
// precedes the placeholder. Redaction emits two shapes:
//
//	"***REDACTED***"                      (JSON values)          → prefix ""
//	"GITHUB_PERSONAL_ACCESS_TOKEN=***"    (env assignments/argv) → prefix "GITHUB_...="
//
// The prefix must be part of the match: restoring an argv entry only makes
// sense when the previous value at that same index was the same assignment.
// Accepts interface{} so callers can pass raw JSON values without asserting.
func redactedValuePrefix(v interface{}) (prefix string, ok bool) {
	s, isStr := v.(string)
	if !isStr {
		return "", false
	}
	i := strings.Index(s, RedactedPlaceholder)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(RedactedPlaceholder):]
	if i == 0 {
		// Exactly the placeholder, nothing after it.
		return "", rest == ""
	}
	// "NAME=" + placeholder.
	if rest == "" && strings.Contains(s[:i], "=") {
		return s[:i], true
	}
	return "", false
}

func restoreInto(next, prev interface{}, count *int) {
	switch n := next.(type) {
	case map[string]interface{}:
		p, ok := prev.(map[string]interface{})
		if !ok {
			return
		}
		for k, v := range n {
			old, exists := p[k]
			if !exists {
				continue
			}
			if prefix, redacted := redactedValuePrefix(v); redacted {
				if oldStr, wasStr := old.(string); wasStr && isRestorable(oldStr, prefix) {
					n[k] = oldStr
					*count++
					continue
				}
			}
			restoreInto(v, old, count)
		}
	case []interface{}:
		p, ok := prev.([]interface{})
		if !ok {
			return
		}
		for i, v := range n {
			if i >= len(p) {
				break
			}
			if prefix, redacted := redactedValuePrefix(v); redacted {
				if oldStr, wasStr := p[i].(string); wasStr && isRestorable(oldStr, prefix) {
					n[i] = oldStr
					*count++
					continue
				}
			}
			restoreInto(v, p[i], count)
		}
	}
}

// isRestorable reports whether prev can supply the real value for a
// placeholder carrying the given prefix.
func isRestorable(prev, prefix string) bool {
	if !strings.HasPrefix(prev, prefix) {
		return false
	}
	if strings.Contains(prev, RedactedPlaceholder) {
		return false // the on-disk value is itself a placeholder
	}
	return len(prev) > len(prefix) // there is an actual secret after the prefix
}

// RedactSecrets scrubs credentials from arbitrary text (process stderr,
// command lines, tool output). It is intentionally lossy: when in doubt it
// masks.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = urlUserinfoRe.ReplaceAllString(s, "${1}"+RedactedPlaceholder+"${3}")
	s = tokenValueRe.ReplaceAllString(s, RedactedPlaceholder)
	s = bearerRe.ReplaceAllString(s, "${1}"+RedactedPlaceholder)
	s = envAssignRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := envAssignRe.FindStringSubmatch(m)
		if parts == nil {
			return m
		}
		name, val := parts[1], parts[2]
		if isEnvReference(val) || !looksLikeSecretKey(name) {
			return m
		}
		return name + "=" + RedactedPlaceholder
	})
	s = jsonSecretRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := jsonSecretRe.FindStringSubmatch(m)
		if parts == nil {
			return m
		}
		prefix, val, suffix := parts[1], parts[2], parts[3]
		key := strings.Trim(prefix, `": `)
		if isEnvReference(val) || !looksLikeSecretKey(key) {
			return m
		}
		return prefix + RedactedPlaceholder + suffix
	})
	return s
}

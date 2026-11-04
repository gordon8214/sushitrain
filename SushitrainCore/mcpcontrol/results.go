// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpcontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/mcpapi"
)

var credentialPattern = regexp.MustCompile(`(?i)((?:password|passwd|api[_-]?key|authorization|credential|secret|signature|access[_-]?token)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s,&}]+)`)
var userInfoPattern = regexp.MustCompile(`(?i)(https?|socks5)://[^/\s"'<>]*@`)
var authorizationPattern = regexp.MustCompile(`(?i)((?:proxy-)?authorization["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\r\n,}]+)`)

var bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s,"'&}]+`)
var pairingPattern = regexp.MustCompile(`synctrain\+pair://[^\s"'<>]+`)

func scrub(s string, args map[string]any) string {
	s = userInfoPattern.ReplaceAllString(s, "${1}://[redacted]@")
	s = authorizationPattern.ReplaceAllString(s, "${1}[redacted]")
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = credentialPattern.ReplaceAllString(s, "${1}[redacted]")
	s = pairingPattern.ReplaceAllString(s, "[pairing invitation redacted]")
	var visit func(map[string]any)
	visit = func(values map[string]any) {
		for key, value := range values {
			if sensitive(key) || key == "link" {
				if secret, ok := value.(string); ok && secret != "" {
					s = strings.ReplaceAll(s, secret, "[redacted]")
				}
			}
			if nested, ok := value.(map[string]any); ok {
				visit(nested)
			}
		}
	}
	visit(args)
	return s
}
func sensitive(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "password") && !strings.HasPrefix(k, "has") || strings.Contains(k, "apikey") || strings.Contains(k, "api_key") || strings.Contains(k, "credential") || k == "secret" || k == "authorization" || k == "proxy-authorization" || k == "proxyauthorization" || strings.Contains(k, "privatekey") || k == "accesstoken" || k == "refreshtoken"
}
func ScrubError(err error, args map[string]any) error {
	var typed *mcpapi.Error
	if errors.As(err, &typed) {
		copy := *typed
		copy.Message = scrub(copy.Message, args)
		return &copy
	}
	message := scrub(err.Error(), args)
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}
func Sanitize(value any, args map[string]any) any {
	return sanitize(value, args, false)
}

// An explicitly requested file payload is data, not an operational log. Keep its
// bytes intact while still sanitizing operational metadata in the same result.
func sanitize(value any, args map[string]any, fileContent bool) any {
	b, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var v any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return nil
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			secret := x["IsSecret"] == true || x["isSecret"] == true
			for k, v := range x {
				if fileContent && k == "content" {
					continue
				}
				if secret && strings.EqualFold(k, "value") {
					x[k] = ""
				} else if sensitive(k) {
					x[k] = "[redacted]"
				} else if strings.EqualFold(k, "url") || strings.EqualFold(k, "nativePath") {
					if s, ok := v.(string); ok && (strings.Contains(s, "signature=") || strings.Contains(s, "token=") || strings.Contains(s, "@")) {
						x[k] = "[authenticated URL redacted]"
					} else {
						x[k] = walk(v)
					}
				} else {
					x[k] = walk(v)
				}
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case string:
			return scrub(x, args)
		default:
			return v
		}
	}
	return walk(v)
}
func validatePaths(call mcpapi.Call) error {
	if call.Name == "synctrain_folder_add" {
		return nil
	}
	for _, key := range []string{"path"} {
		if p, ok := call.Arguments[key].(string); ok {
			if strings.Contains(p, "\\") || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
				return mcpapi.Failure("invalid_path", "Use a contained forward-slash relative path.")
			}
			for _, part := range strings.Split(p, "/") {
				if part == ".." {
					return mcpapi.Failure("invalid_path", "Parent path traversal is not allowed.")
				}
			}
			if path.Clean(p) == ".." {
				return mcpapi.Failure("invalid_path", "Path escapes the folder.")
			}
		}
	}
	return nil
}
func pageKey(call mcpapi.Call) string {
	args := map[string]any{}
	for k, v := range call.Arguments {
		if k != "cursor" && k != "limit" {
			args[k] = v
		}
	}
	b, _ := json.Marshal(args)
	return fmt.Sprintf("%s:%x", call.Name, sha256.Sum256(b))
}
func pageLimit(call mcpapi.Call) int {
	if n, ok := call.Arguments["limit"].(float64); ok {
		return int(n)
	}
	return 100
}
func (c *Control) paginate(call mcpapi.Call, data any) mcpapi.Result {
	list, wrap, headerSize, ok := collection(call.Name, data)
	if !ok {
		b, _ := json.Marshal(data)
		if len(b) > mcpapi.MaxResult {
			return failed(mcpapi.Failure("response_too_large", "Result exceeds 1 MiB; narrow the query or read a smaller content range."))
		}
		return mcpapi.Result{Data: data}
	}
	if len(list) > 100000 {
		return failed(mcpapi.Failure("response_too_large", "Too many results; narrow the query."))
	}
	encoded, _ := json.Marshal(data)
	if len(encoded) > 8<<20 {
		return failed(mcpapi.Failure("response_too_large", "Listing exceeds the 8 MiB snapshot budget; narrow the query."))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := len(encoded)
	for _, p := range c.pages {
		cached += p.Size
	}
	if cached > 32<<20 {
		return failed(mcpapi.Failure("busy", "Paged snapshots reached their 32 MiB budget; finish a listing or wait for expiration."))
	}
	if len(c.pages) >= 64 {
		return failed(mcpapi.Failure("busy", "Too many paged results; finish an existing listing or wait five minutes."))
	}
	id := uuid.NewString()
	p := page{Size: len(encoded), Data: list, Wrap: wrap, HeaderSize: headerSize, Key: pageKey(call), Expires: time.Now().Add(5 * time.Minute)}
	return c.pageResult(call, id, p, 0)
}
func (c *Control) next(call mcpapi.Call, cursor string) mcpapi.Result {
	id, position, ok := strings.Cut(cursor, ":")
	offset, err := strconv.Atoi(position)
	c.mu.Lock()
	defer c.mu.Unlock()
	p, found := c.pages[id]
	if !ok || err != nil || offset < 0 || !found || p.Key != pageKey(call) || time.Now().After(p.Expires) || offset > len(p.Data) {
		return failed(mcpapi.Failure("cursor_expired", "Cursor expired or belongs to another query; restart the listing."))
	}
	return c.pageResult(call, id, p, offset)
}
func (c *Control) pageResult(call mcpapi.Call, id string, p page, offset int) mcpapi.Result {
	end := offset
	size := p.HeaderSize
	budget := mcpapi.MaxResult - 4096
	if call.Name == "synctrain_logs" {
		budget = mcpapi.MaxContent - 4096
	}
	for end < len(p.Data) && end-offset < pageLimit(call) {
		b, _ := json.Marshal(p.Data[end])
		if size+len(b) > budget {
			break
		}
		size += len(b) + 1
		end++
	}
	if end == offset && end < len(p.Data) {
		return failed(mcpapi.Failure("response_too_large", "A single result exceeds 1 MiB; narrow the query."))
	}
	result := mcpapi.Result{Data: p.Data[offset:end]}
	if p.Wrap != nil {
		result.Data = p.Wrap(p.Data[offset:end])
	}
	if end < len(p.Data) {
		c.pages[id] = p
		result.NextCursor = fmt.Sprintf("%s:%d", id, end)
	} else {
		delete(c.pages, id)
	}
	return result
}

func boundedLogs(data any) any {
	list, ok := data.([]any)
	if !ok {
		return data
	}
	for _, value := range list {
		if line, ok := value.(map[string]any); ok {
			for key, value := range line {
				if strings.EqualFold(key, "when") {
					if stamp, ok := value.(json.Number); ok {
						line[key] = stamp.String()
					}
				}
				if text, ok := value.(string); ok && len(text) > 4096 {
					text = text[:4096]
					for !utf8.ValidString(text) {
						text = text[:len(text)-1]
					}
					line[key] = text
					line["truncated"] = true
				}
			}
		}
	}
	return list
}

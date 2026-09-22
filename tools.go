package agentwire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClassifyTool maps a vendor tool name to a coarse kind, so a consumer can
// render or auto-approve without a per-vendor name table.
func ClassifyTool(name string) ToolKind {
	return classifyToolName(name)
}

func classifyToolName(name string) ToolKind {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ToolOther
	}
	if strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, "mcp_") {
		return ToolMCP
	}
	for _, token := range splitToolTokens(n) {
		switch token {
		case "mcp", "mcptoolcall":
			return ToolMCP
		case "read", "view", "cat", "open", "fetch", "readfile", "readtextfile", "notebookread":
			return ToolRead
		case "edit", "write", "multiedit", "patch", "applypatch", "apply", "create", "replace",
			"notebookedit", "filechange", "change", "writefile", "writetextfile", "save":
			return ToolEdit
		case "bash", "shell", "exec", "execute", "command", "terminal", "cmd", "run", "commandexecution":
			return ToolExec
		case "grep", "glob", "search", "find", "list", "ls", "websearch", "web":
			return ToolSearch
		}
	}
	return ToolOther
}

// splitToolTokens splits a tool name on every non-alphanumeric character.
func splitToolTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// toolPaths extracts the files a tool call touches, where the vendor input
// names them. Only edit-shaped inputs are inspected: a read path is not a
// change a consumer wants to preview.
func toolPaths(kind ToolKind, input string) []string {
	if kind != ToolEdit || strings.TrimSpace(input) == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, key := range []string{"file_path", "filePath", "path", "notebook_path", "target_file", "file"} {
		s, _ := m[key].(string)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// classifyPermissionKind derives the tool kind of a permission request from
// the tool name and, when the name is generic, from the input.
func classifyPermissionKind(name, input string) ToolKind {
	kind := classifyToolName(name)
	if kind != ToolOther {
		return kind
	}
	if strings.TrimSpace(input) == "" {
		return ToolOther
	}
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return ToolOther
	}
	for key := range m {
		switch strings.ToLower(key) {
		case "file_path", "filepath", "path", "notebook_path", "new_string", "old_string":
			return ToolEdit
		case "command", "cmd":
			return ToolExec
		}
	}
	return ToolOther
}

// loadAttachment resolves one attachment to its bytes and media type.
func loadAttachment(a Attachment) ([]byte, string, error) {
	data := a.Data
	if len(data) == 0 && a.Path != "" {
		b, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, "", fmt.Errorf("agentwire: attachment %s: %w", a.Path, err)
		}
		data = b
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("agentwire: attachment with no data")
	}
	media := a.MIME
	if media == "" {
		media = mimeByExtension(a.Path)
	}
	if media == "" {
		media = "application/octet-stream"
	}
	return data, media, nil
}

// mimeByExtension maps the image and text types a prompt can carry.
func mimeByExtension(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".txt", ".md", ".log":
		return "text/plain"
	case ".json":
		return "application/json"
	}
	return ""
}

// imageAttachment reports whether an attachment is an image.
func imageAttachment(mime string) bool { return strings.HasPrefix(mime, "image/") }

// sortedAttachments returns the images first, so a vendor that only accepts an
// image before text gets a valid prompt.
func sortedAttachments(items []Attachment) []Attachment {
	if len(items) < 2 {
		return items
	}
	out := make([]Attachment, 0, len(items))
	for _, a := range items {
		mime := a.MIME
		if mime == "" {
			mime = mimeByExtension(a.Path)
		}
		if imageAttachment(mime) {
			out = append(out, a)
		}
	}
	for _, a := range items {
		mime := a.MIME
		if mime == "" {
			mime = mimeByExtension(a.Path)
		}
		if !imageAttachment(mime) {
			out = append(out, a)
		}
	}
	return out
}

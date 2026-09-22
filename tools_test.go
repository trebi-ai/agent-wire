package agentwire

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// TestToolClassify checks the vendor tool-name mapping.
func TestToolClassify(t *testing.T) {
	tests := []struct {
		name string
		want ToolKind
	}{
		{"Read", ToolRead},
		{"read_file", ToolRead},
		{"Bash", ToolExec},
		{"commandExecution", ToolExec},
		{"Edit", ToolEdit},
		{"fileChange", ToolEdit},
		{"MultiEdit", ToolEdit},
		{"str_replace", ToolEdit},
		{"Grep", ToolSearch},
		{"Glob", ToolSearch},
		{"webSearch", ToolSearch},
		{"mcp__sling__echo", ToolMCP},
		{"mcpToolCall", ToolMCP},
		{"notebook_edit", ToolEdit},
		{"", ToolOther},
		{"FrobnicateEverything", ToolOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyTool(tt.name); got != tt.want {
				t.Fatalf("ClassifyTool(%q) = %q, want %q", tt.name, got, tt.want)
			}
			if got := classifyToolName(tt.name); got != tt.want {
				t.Fatalf("classifyToolName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestToolPaths checks the edit-input path extraction.
func TestToolPaths(t *testing.T) {
	tests := []struct {
		name  string
		kind  ToolKind
		input string
		want  []string
	}{
		{"all keys", ToolEdit, `{"file_path":"/a","path":"/b","notebook_path":"/c"}`, []string{"/a", "/b", "/c"}},
		{"dedup", ToolEdit, `{"file_path":"/x","path":"/x"}`, []string{"/x"}},
		{"no paths in input", ToolEdit, `{"content":"x"}`, nil},
		{"read kind is ignored", ToolRead, `{"file_path":"/a"}`, nil},
		{"exec kind is ignored", ToolExec, `{"path":"/a"}`, nil},
		{"malformed json", ToolEdit, `{"file_path":`, nil},
		{"empty input", ToolEdit, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolPaths(tt.kind, tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("toolPaths(%q, %q) = %v, want %v", tt.kind, tt.input, got, tt.want)
			}
		})
	}
}

// TestToolPermissionKind checks the input-key fallback for an unknown name.
func TestToolPermissionKind(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  ToolKind
	}{
		{"known name wins", "Read", `{"command":"ls"}`, ToolRead},
		{"unknown name with a path", "someVendorTool", `{"path":"/a"}`, ToolEdit},
		{"unknown name with file_path", "someVendorTool", `{"file_path":"/a"}`, ToolEdit},
		{"unknown name with old_string", "someVendorTool", `{"old_string":"a"}`, ToolEdit},
		{"unknown name with a command", "someVendorTool", `{"command":"ls"}`, ToolExec},
		{"unknown name with a cmd", "someVendorTool", `{"cmd":"ls"}`, ToolExec},
		{"unknown name with unrelated keys", "someVendorTool", `{"text":"hi"}`, ToolOther},
		{"unknown name with malformed input", "someVendorTool", `{`, ToolOther},
		{"unknown name with empty input", "someVendorTool", "", ToolOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPermissionKind(tt.tool, tt.input); got != tt.want {
				t.Fatalf("classifyPermissionKind(%q, %q) = %q, want %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

// injTestAttachmentFile writes bytes to a temp file with the given name.
func injTestAttachmentFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestAttachmentLoad checks the byte and media-type resolution.
func TestAttachmentLoad(t *testing.T) {
	png := injTestAttachmentFile(t, "pic.png", []byte("png-bytes"))
	jpg := injTestAttachmentFile(t, "pic.jpg", []byte("jpg-bytes"))
	jsonPath := injTestAttachmentFile(t, "data.json", []byte(`{"a":1}`))
	bin := injTestAttachmentFile(t, "blob", []byte("raw"))

	tests := []struct {
		name       string
		attachment Attachment
		wantData   string
		wantMIME   string
	}{
		{"data", Attachment{Data: []byte("inline")}, "inline", "application/octet-stream"},
		{"path png", Attachment{Path: png}, "png-bytes", "image/png"},
		{"path jpg", Attachment{Path: jpg}, "jpg-bytes", "image/jpeg"},
		{"path json", Attachment{Path: jsonPath}, `{"a":1}`, "application/json"},
		{"unknown extension", Attachment{Path: bin}, "raw", "application/octet-stream"},
		{"explicit mime wins", Attachment{Path: png, MIME: "text/csv"}, "png-bytes", "text/csv"},
		{"data wins over path", Attachment{Path: png, Data: []byte("data-wins")}, "data-wins", "image/png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, mime, err := loadAttachment(tt.attachment)
			if err != nil {
				t.Fatalf("loadAttachment: %v", err)
			}
			if string(data) != tt.wantData {
				t.Fatalf("data = %q, want %q", data, tt.wantData)
			}
			if mime != tt.wantMIME {
				t.Fatalf("mime = %q, want %q", mime, tt.wantMIME)
			}
		})
	}

	if _, _, err := loadAttachment(Attachment{}); err == nil {
		t.Fatal("empty attachment returned no error")
	}
}

// TestAttachmentSort checks that images come first and each group keeps its
// order.
func TestAttachmentSort(t *testing.T) {
	items := []Attachment{
		{Path: "a.json"},
		{Path: "b.png"},
		{Path: "c.txt"},
		{Path: "d.jpg"},
		{Path: "e.bin", MIME: "image/png"},
	}
	got := sortedAttachments(items)
	want := []string{"b.png", "d.jpg", "e.bin", "a.json", "c.txt"}
	if paths := injTestAttachmentPaths(got); !slices.Equal(paths, want) {
		t.Fatalf("sorted order = %v, want %v", paths, want)
	}

	single := []Attachment{{Path: "only.png"}}
	if got := sortedAttachments(single); len(got) != 1 || got[0].Path != "only.png" {
		t.Fatalf("single attachment = %v", got)
	}
}

// injTestAttachmentPaths lists the paths of attachments in order.
func injTestAttachmentPaths(items []Attachment) []string {
	out := make([]string, 0, len(items))
	for _, a := range items {
		out = append(out, a.Path)
	}
	return out
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/tools"
)

// fileService is the Connect implementation of lyon.v1.FileService —
// read-only workspace preview. Every path goes through tools.Confine, the
// same containment as the agent's read_file (symlink-expanded, judged on
// the real location, fail-closed), so the web surface can never reach
// outside the workspace the engine was launched with. v1 is read-only by
// construction; write/upload RPCs are TODO(v2) and will ride the tool
// approval gate.
type fileService struct {
	s *Server
}

var _ lyonv1connect.FileServiceHandler = (*fileService)(nil)

const (
	fileListCap     = 512       // directory entries before truncated=true
	fileTextPreview = 512 << 10 // 512KB text preview cap (mirrors the session adapter's ResultCap)
	fileBinaryCap   = 2 << 20   // 2MB binary preview cap
	fileSniffBytes  = 4 << 10   // head window for text/binary classification of unknown extensions
)

func (f *fileService) ListDirectory(_ context.Context, req *connect.Request[lyonv1.ListDirectoryRequest]) (*connect.Response[lyonv1.ListDirectoryResponse], error) {
	msg := req.Msg
	// "" means the workspace root (proto contract); Confine itself treats
	// an empty path as an error, so map it here where the meaning is known.
	p := msg.Path
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	abs, err := tools.Confine(f.s.eng.Config().Workspace, p)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	truncated := false
	if len(des) > fileListCap {
		des, truncated = des[:fileListCap], true
	}
	out := make([]*lyonv1.DirEntry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue // vanished mid-list: not an error, just not there
		}
		out = append(out, &lyonv1.DirEntry{
			Name:        de.Name(),
			IsDir:       de.IsDir(),
			SizeBytes:   info.Size(),
			MtimeUnixMs: info.ModTime().UnixMilli(),
			Kind:        fileKind(abs, de),
		})
	}
	// os.ReadDir returns entries sorted by filename — stable, cheap, and
	// what the client's own sort builds on.
	return connect.NewResponse(&lyonv1.ListDirectoryResponse{
		Path:      msg.Path,
		Entries:   out,
		Truncated: truncated,
	}), nil
}

func (f *fileService) ReadTextFile(_ context.Context, req *connect.Request[lyonv1.ReadTextFileRequest]) (*connect.Response[lyonv1.ReadTextFileResponse], error) {
	msg := req.Msg
	if msg.OffsetLines < 0 || msg.LimitLines < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("offset_lines and limit_lines must be >= 0 (line numbers are 1-based)"))
	}
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	if !utf8.Valid(data) {
		if strings.IndexByte(string(data), 0) >= 0 {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s looks like a binary file; use ReadBinaryFile", msg.Path))
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s is not valid UTF-8 text", msg.Path))
	}
	sizeBytes := int64(len(data))
	truncated := false
	if len(data) > fileTextPreview {
		data, truncated = data[:fileTextPreview], true
		// The cut may land mid-rune; trim back to a valid boundary.
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	text := string(data)
	totalLines := len(tools.SplitFileLines(text))
	offset, limit := int(msg.OffsetLines), int(msg.LimitLines)
	if offset > 0 {
		lines := tools.SplitFileLines(text)
		start := offset - 1
		if start >= len(lines) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("offset_lines %d is past the end (%d lines in the preview)", offset, totalLines))
		}
		end := len(lines)
		if limit > 0 && start+limit < end {
			end = start + limit
		}
		text = strings.Join(lines[start:end], "\n")
	} else if limit > 0 {
		lines := tools.SplitFileLines(text)
		if limit < len(lines) {
			text = strings.Join(lines[:limit], "\n")
		}
	}
	return connect.NewResponse(&lyonv1.ReadTextFileResponse{
		Path:       msg.Path,
		Content:    text,
		Truncated:  truncated,
		TotalLines: int32(totalLines),
		SizeBytes:  sizeBytes,
		Kind:       lyonv1.FileKind_FILE_KIND_TEXT,
	}), nil
}

func (f *fileService) ReadBinaryFile(_ context.Context, req *connect.Request[lyonv1.ReadBinaryFileRequest]) (*connect.Response[lyonv1.ReadBinaryFileResponse], error) {
	msg := req.Msg
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	sizeBytes := int64(len(data))
	truncated := false
	if len(data) > fileBinaryCap {
		data, truncated = data[:fileBinaryCap], true
	}
	return connect.NewResponse(&lyonv1.ReadBinaryFileResponse{
		Path:        msg.Path,
		ContentType: binaryContentType(msg.Path, data),
		Data:        data,
		Truncated:   truncated,
		SizeBytes:   sizeBytes,
	}), nil
}

// fileOSErr maps filesystem failures onto Connect codes: missing is
// NotFound, unreadable is PermissionDenied, anything else is Internal.
func fileOSErr(err error, path string) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, os.ErrNotExist):
		code = connect.CodeNotFound
	case errors.Is(err, os.ErrPermission):
		code = connect.CodePermissionDenied
	}
	return connect.NewError(code, fmt.Errorf("%s: %w", path, err))
}

// fileKind classifies a directory entry — the server-side mirror of the
// web client's renderer pick (plan file.proto: decided once, fail-closed,
// on the server). Known extensions classify without touching the file;
// anything else gets a small head sniff: valid UTF-8 text vs not. SVG is
// deliberately absent from the image set (served as OTHER — client-side
// SVG rendering is an XSS surface).
func fileKind(dir string, de os.DirEntry) lyonv1.FileKind {
	if de.IsDir() {
		return lyonv1.FileKind_FILE_KIND_DIR
	}
	name := de.Name()
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return lyonv1.FileKind_FILE_KIND_MARKDOWN
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return lyonv1.FileKind_FILE_KIND_IMAGE
	case ".pdf":
		return lyonv1.FileKind_FILE_KIND_PDF
	case ".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".rs", ".java", ".c", ".h",
		".cpp", ".hpp", ".cc", ".cs", ".rb", ".php", ".swift", ".kt", ".scala",
		".lua", ".r", ".m", ".sh", ".bash", ".zsh", ".bat", ".ps1", ".sql",
		".json", ".jsonc", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf",
		".xml", ".html", ".htm", ".css", ".scss", ".proto", ".graphql", ".vue",
		".svelte":
		return lyonv1.FileKind_FILE_KIND_CODE
	}
	// Unknown extension: one bounded head read decides text vs binary.
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return lyonv1.FileKind_FILE_KIND_OTHER
	}
	defer f.Close()
	head := make([]byte, fileSniffBytes)
	n, _ := f.Read(head)
	if utf8.Valid(head[:n]) {
		return lyonv1.FileKind_FILE_KIND_TEXT
	}
	return lyonv1.FileKind_FILE_KIND_OTHER
}

// binaryContentType maps the preview-allowlisted extensions to their MIME
// types (same set the proto documents) and falls back to content sniffing
// for everything else — the bytes decide, not the filename alone.
func binaryContentType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}

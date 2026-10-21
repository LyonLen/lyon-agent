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
// workspace preview plus user-initiated write ops. Every path goes through
// tools.Confine, the same containment as the agent's read_file
// (symlink-expanded, judged on the real location, fail-closed), so the web
// surface can never reach outside the workspace the engine was launched
// with. Writes are panel gestures, not agent tool calls — no ExecEvent
// exists for them, so they confirm client-side (file.proto) instead of
// entering the tool approval gate; the server fail-closes on containment,
// conflicts, the workspace root, and the size cap.
type fileService struct {
	s *Server
}

var _ lyonv1connect.FileServiceHandler = (*fileService)(nil)

const (
	fileListCap     = 512       // directory entries before truncated=true
	fileTextPreview = 512 << 10 // 512KB text preview cap (mirrors the session adapter's ResultCap)
	fileBinaryCap   = 32 << 20  // 32MB binary preview/upload cap — office decks need headroom (a sibling product caps at 30MB)
	fileSniffBytes  = 4 << 10   // head window for text/binary classification of unknown extensions
	fileWriteCap    = 2 << 20   // 2MB text write cap (WriteFile is utf-8 full replacement)

	// UploadFile is the binary path, so it is bounded by fileBinaryCap and
	// not by fileWriteCap: a pasted screenshot is already ~3.5MB at the
	// model's own ceiling (tools.maxImageBytes), and a 2MB upload cap
	// rejected it two layers before the layer that has an opinion. WriteFile
	// stays at 2MB — it carries a utf-8 full replacement, not pixels.
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

// --- write ops (v2) ------------------------------------------------------

func (f *fileService) WriteFile(_ context.Context, req *connect.Request[lyonv1.WriteFileRequest]) (*connect.Response[lyonv1.WriteFileResponse], error) {
	msg := req.Msg
	if len(msg.Content) > fileWriteCap {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("content is %d bytes; the panel write cap is %d", len(msg.Content), fileWriteCap))
	}
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if fi, err := os.Stat(abs); err == nil {
		if fi.IsDir() {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s is a directory", msg.Path))
		}
		if msg.CreateOnly {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s already exists", msg.Path))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fileOSErr(err, msg.Path)
	}
	// O_EXCL when the op is a pure create — the stat above is only the
	// friendly message; the flag is the actual race-free fail-close.
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if msg.CreateOnly {
		flag |= os.O_EXCL
	}
	fh, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		if msg.CreateOnly && errors.Is(err, os.ErrExist) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", msg.Path))
		}
		return nil, fileOSErr(err, msg.Path)
	}
	if _, err := fh.Write([]byte(msg.Content)); err != nil {
		fh.Close()
		return nil, fileOSErr(err, msg.Path)
	}
	if err := fh.Close(); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.WriteFileResponse{SizeBytes: int64(len(msg.Content))}), nil
}

func (f *fileService) Mkdir(_ context.Context, req *connect.Request[lyonv1.MkdirRequest]) (*connect.Response[lyonv1.MkdirResponse], error) {
	msg := req.Msg
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if msg.CreateParents {
		// mkdir -p: idempotent when the path is already the wanted dir;
		// a file squatting on the path still fails below.
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fileOSErr(err, msg.Path)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s exists and is not a directory", msg.Path))
		}
		return connect.NewResponse(&lyonv1.MkdirResponse{}), nil
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", msg.Path))
		}
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.MkdirResponse{}), nil
}

func (f *fileService) Rename(_ context.Context, req *connect.Request[lyonv1.RenameRequest]) (*connect.Response[lyonv1.RenameResponse], error) {
	msg := req.Msg
	// Single path segment only — new_name with a separator would silently
	// become a move; the client has a dedicated Move for that.
	n := msg.NewName
	if n == "" || n == "." || n == ".." || filepath.Base(n) != n ||
		strings.ContainsAny(n, `/\`) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("new_name must be a single name (no path separators)"))
	}
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	dst := filepath.Join(filepath.Dir(abs), n)
	if dst2, err := tools.Confine(f.s.eng.Config().Workspace, dst); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	} else {
		dst = dst2
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", n))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fileOSErr(err, n)
	}
	if err := os.Rename(abs, dst); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.RenameResponse{}), nil
}

func (f *fileService) Move(_ context.Context, req *connect.Request[lyonv1.MoveRequest]) (*connect.Response[lyonv1.MoveResponse], error) {
	msg := req.Msg
	src, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	dirP := msg.ToDir
	if strings.TrimSpace(dirP) == "" {
		dirP = "."
	}
	dstDir, err := tools.Confine(f.s.eng.Config().Workspace, dirP)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	fi, err := os.Stat(dstDir)
	if err != nil {
		return nil, fileOSErr(err, msg.ToDir)
	}
	if !fi.IsDir() {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("%s is not a directory", msg.ToDir))
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if dst == src {
		return connect.NewResponse(&lyonv1.MoveResponse{}), nil // no-op
	}
	// Moving a directory into its own subtree is EINVAL on every OS —
	// catch it before the rename for a readable error.
	if fi, err := os.Stat(src); err != nil {
		return nil, fileOSErr(err, msg.Path)
	} else if fi.IsDir() {
		rel, rerr := filepath.Rel(src, dst)
		if rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("cannot move %s into its own subtree", msg.Path))
		}
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("%s already exists in %s", filepath.Base(src), msg.ToDir))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fileOSErr(err, filepath.Base(src))
	}
	if err := os.Rename(src, dst); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.MoveResponse{}), nil
}

func (f *fileService) Delete(_ context.Context, req *connect.Request[lyonv1.DeleteRequest]) (*connect.Response[lyonv1.DeleteResponse], error) {
	msg := req.Msg
	ws := f.s.eng.Config().Workspace
	abs, err := tools.Confine(ws, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// The workspace root itself is never deletable, no matter the flags —
	// Confine(".") is the root, and a recursive rm there would take the
	// whole session workspace with it.
	if root, err := tools.Confine(ws, "."); err == nil && abs == root {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("refusing to delete the workspace root"))
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	if fi.IsDir() {
		if msg.Recursive {
			if err := os.RemoveAll(abs); err != nil {
				return nil, fileOSErr(err, msg.Path)
			}
			return connect.NewResponse(&lyonv1.DeleteResponse{}), nil
		}
		des, err := os.ReadDir(abs)
		if err != nil {
			return nil, fileOSErr(err, msg.Path)
		}
		if len(des) > 0 {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s is not empty (pass recursive)", msg.Path))
		}
	}
	if err := os.Remove(abs); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.DeleteResponse{}), nil
}

func (f *fileService) UploadFile(_ context.Context, req *connect.Request[lyonv1.UploadFileRequest]) (*connect.Response[lyonv1.UploadFileResponse], error) {
	msg := req.Msg
	if len(msg.Data) > fileBinaryCap {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("upload is %d bytes; the cap is %d", len(msg.Data), fileBinaryCap))
	}
	abs, err := tools.Confine(f.s.eng.Config().Workspace, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if fi, err := os.Stat(abs); err == nil {
		if fi.IsDir() {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s is a directory", msg.Path))
		}
		if !msg.Overwrite {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("%s already exists (pass overwrite)", msg.Path))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fileOSErr(err, msg.Path)
	}
	// Parent must already exist — uploads don't implicitly mkdir -p.
	if _, err := os.Stat(filepath.Dir(abs)); err != nil {
		return nil, fileOSErr(err, filepath.Dir(msg.Path))
	}
	if err := os.WriteFile(abs, msg.Data, 0o644); err != nil {
		return nil, fileOSErr(err, msg.Path)
	}
	return connect.NewResponse(&lyonv1.UploadFileResponse{SizeBytes: int64(len(msg.Data))}), nil
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
	case ".pptx":
		return lyonv1.FileKind_FILE_KIND_PPTX
	case ".docx":
		return lyonv1.FileKind_FILE_KIND_DOCX
	case ".xlsx", ".xls", ".csv":
		return lyonv1.FileKind_FILE_KIND_XLSX
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
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".csv":
		return "text/csv"
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}

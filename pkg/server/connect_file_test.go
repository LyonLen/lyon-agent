package server

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"

	"lyon-agent/pkg/agent"
)

// newFileFixture serves a FileService rooted at a fresh temp workspace
// seeded with one of each classification.
func newFileFixture(t *testing.T) (lyonv1connect.FileServiceClient, string) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", rel, err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	write("a.txt", "l1\nl2\nl3\n")
	write("note.md", "# title\n")
	write("img.png", "\x89PNG\r\n\x1a\nfakepng")
	// Invalid UTF-8 (PNG magic) — what the text RPC must reject. NUL-only
	// files are valid UTF-8 by spec and pass through, exactly like the
	// agent's read_file.
	write("data.bin", "\x89PNG\x00\x01\x02")
	write("big.txt", strings.Repeat("x", fileTextPreview+100))

	eng := agent.NewEngine(agent.Config{Workspace: root}, nil)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewFileServiceClient(ts.Client(), ts.URL), root
}

func TestFileServiceListDirectory(t *testing.T) {
	client, _ := newFileFixture(t)
	ctx := context.Background()

	res, err := client.ListDirectory(ctx, connect.NewRequest(&lyonv1.ListDirectoryRequest{}))
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	kinds := map[string]lyonv1.FileKind{}
	for _, e := range res.Msg.Entries {
		kinds[e.Name] = e.Kind
	}
	want := map[string]lyonv1.FileKind{
		"sub":      lyonv1.FileKind_FILE_KIND_DIR,
		"a.txt":    lyonv1.FileKind_FILE_KIND_TEXT, // sniffed: unknown ext, valid UTF-8
		"note.md":  lyonv1.FileKind_FILE_KIND_MARKDOWN,
		"img.png":  lyonv1.FileKind_FILE_KIND_IMAGE,
		"data.bin": lyonv1.FileKind_FILE_KIND_OTHER, // binary, unknown ext
		"big.txt":  lyonv1.FileKind_FILE_KIND_TEXT,
	}
	if len(kinds) != len(want) {
		t.Fatalf("entries = %v, want keys %v", kinds, want)
	}
	for name, w := range want {
		if got := kinds[name]; got != w {
			t.Errorf("kind(%s) = %v, want %v", name, got, w)
		}
	}
	// Filename order (os.ReadDir's contract) with sizes sane.
	if res.Msg.Entries[0].Name != "a.txt" || res.Msg.Entries[0].SizeBytes != int64(len("l1\nl2\nl3\n")) {
		t.Fatalf("first entry = %+v", res.Msg.Entries[0])
	}

	// Subdirectory listing.
	sub, err := client.ListDirectory(ctx, connect.NewRequest(&lyonv1.ListDirectoryRequest{Path: "sub"}))
	if err != nil || len(sub.Msg.Entries) != 0 {
		t.Fatalf("ListDirectory(sub) = %+v err=%v", sub.Msg.Entries, err)
	}

	// Escape and missing: InvalidArgument / NotFound.
	for _, bad := range []string{"..", "../..", filepath.Join(os.TempDir())} {
		_, err := client.ListDirectory(ctx, connect.NewRequest(&lyonv1.ListDirectoryRequest{Path: bad}))
		if got := connectCode(err); got != connect.CodeInvalidArgument {
			t.Errorf("ListDirectory(%q) code = %v, want InvalidArgument (err=%v)", bad, got, err)
		}
	}
	_, err = client.ListDirectory(ctx, connect.NewRequest(&lyonv1.ListDirectoryRequest{Path: "nope"}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Errorf("ListDirectory(missing) code = %v, want NotFound (err=%v)", got, err)
	}
}

func TestFileServiceReadTextFile(t *testing.T) {
	client, _ := newFileFixture(t)
	ctx := context.Background()

	res, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{Path: "a.txt"}))
	if err != nil {
		t.Fatalf("ReadTextFile: %v", err)
	}
	if res.Msg.Content != "l1\nl2\nl3\n" || res.Msg.TotalLines != 3 || res.Msg.Truncated {
		t.Fatalf("full read = %+v", res.Msg)
	}

	// offset 1-based + limit, identical semantics to tools.ReadFile.
	slice, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{
		Path: "a.txt", OffsetLines: 2, LimitLines: 1,
	}))
	if err != nil || slice.Msg.Content != "l2" {
		t.Fatalf("offset=2 limit=1 = %q err=%v, want %q", slice.Msg.Content, err, "l2")
	}
	if _, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{
		Path: "a.txt", OffsetLines: 99,
	})); connectCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("offset past end code = %v, want InvalidArgument", connectCode(err))
	}

	// Preview cap: 600KB file truncates at 512KB and stays valid UTF-8.
	big, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{Path: "big.txt"}))
	if err != nil || !big.Msg.Truncated || int64(len(big.Msg.Content)) != fileTextPreview || big.Msg.SizeBytes <= fileTextPreview {
		t.Fatalf("big read: truncated=%v len=%d size=%d err=%v", big.Msg.Truncated, len(big.Msg.Content), big.Msg.SizeBytes, err)
	}

	// Binary via the text RPC → FailedPrecondition pointing at ReadBinaryFile.
	_, err = client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{Path: "data.bin"}))
	if got := connectCode(err); got != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "ReadBinaryFile") {
		t.Fatalf("binary via text code = %v err=%v, want FailedPrecondition hint", got, err)
	}

	// Missing file → NotFound; escape → InvalidArgument.
	if _, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{Path: "nope.txt"})); connectCode(err) != connect.CodeNotFound {
		t.Fatalf("missing code = %v, want NotFound", connectCode(err))
	}
	if _, err := client.ReadTextFile(ctx, connect.NewRequest(&lyonv1.ReadTextFileRequest{Path: "../secret"})); connectCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("escape code = %v, want InvalidArgument", connectCode(err))
	}
}

// TestFileServiceWrites walks the whole v2 write surface: create vs
// overwrite, conflict codes, containment, the workspace-root guard, and
// the upload cap.
func TestFileServiceWrites(t *testing.T) {
	client, root := newFileFixture(t)
	ctx := context.Background()

	// --- WriteFile: create-only then overwrite.
	if _, err := client.WriteFile(ctx, connect.NewRequest(&lyonv1.WriteFileRequest{
		Path: "new.txt", Content: "hello", CreateOnly: true,
	})); err != nil {
		t.Fatalf("WriteFile create: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "new.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("on disk = %q err=%v", got, err)
	}
	if _, err := client.WriteFile(ctx, connect.NewRequest(&lyonv1.WriteFileRequest{
		Path: "new.txt", Content: "x", CreateOnly: true,
	})); connectCode(err) != connect.CodeAlreadyExists {
		t.Fatalf("create over existing code = %v, want AlreadyExists (err=%v)", connectCode(err), err)
	}
	if _, err := client.WriteFile(ctx, connect.NewRequest(&lyonv1.WriteFileRequest{
		Path: "new.txt", Content: "replaced",
	})); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(got) != "replaced" {
		t.Fatalf("after overwrite = %q", got)
	}
	// Over a directory, and out of the workspace.
	if _, err := client.WriteFile(ctx, connect.NewRequest(&lyonv1.WriteFileRequest{Path: "sub"})); connectCode(err) != connect.CodeFailedPrecondition {
		t.Fatalf("write-on-dir code = %v, want FailedPrecondition", connectCode(err))
	}
	if _, err := client.WriteFile(ctx, connect.NewRequest(&lyonv1.WriteFileRequest{Path: "../evil.txt"})); connectCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("escape write code = %v, want InvalidArgument", connectCode(err))
	}

	// --- Mkdir: plain create, conflict, mkdir -p idempotence.
	if _, err := client.Mkdir(ctx, connect.NewRequest(&lyonv1.MkdirRequest{Path: "fresh"})); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := client.Mkdir(ctx, connect.NewRequest(&lyonv1.MkdirRequest{Path: "fresh"})); connectCode(err) != connect.CodeAlreadyExists {
		t.Fatalf("re-mkdir code = %v, want AlreadyExists", connectCode(err))
	}
	if _, err := client.Mkdir(ctx, connect.NewRequest(&lyonv1.MkdirRequest{Path: "a/b/c", CreateParents: true})); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatalf("a/b/c missing: %v", err)
	}

	// --- Rename: happy path, separator rejection, conflict, missing source.
	if _, err := client.Rename(ctx, connect.NewRequest(&lyonv1.RenameRequest{
		Path: "new.txt", NewName: "renamed.txt",
	})); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "renamed.txt")); string(got) != "replaced" {
		t.Fatalf("renamed content = %q", got)
	}
	for _, bad := range []string{"sub/x.txt", `sub\x.txt`, ".."} {
		if _, err := client.Rename(ctx, connect.NewRequest(&lyonv1.RenameRequest{
			Path: "renamed.txt", NewName: bad,
		})); connectCode(err) != connect.CodeInvalidArgument {
			t.Errorf("Rename(new_name=%q) code = %v, want InvalidArgument", bad, connectCode(err))
		}
	}
	if _, err := client.Rename(ctx, connect.NewRequest(&lyonv1.RenameRequest{
		Path: "renamed.txt", NewName: "a.txt",
	})); connectCode(err) != connect.CodeAlreadyExists {
		t.Fatalf("rename onto a.txt code = %v, want AlreadyExists", connectCode(err))
	}
	if _, err := client.Rename(ctx, connect.NewRequest(&lyonv1.RenameRequest{
		Path: "ghost.txt", NewName: "x.txt",
	})); connectCode(err) != connect.CodeNotFound {
		t.Fatalf("rename missing code = %v, want NotFound", connectCode(err))
	}

	// --- Move: file into a dir, conflicts, dir into its own subtree.
	os.Mkdir(filepath.Join(root, "fresh", "inner"), 0o755)
	if _, err := client.Move(ctx, connect.NewRequest(&lyonv1.MoveRequest{
		Path: "renamed.txt", ToDir: "fresh",
	})); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh", "renamed.txt")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if _, err := client.Move(ctx, connect.NewRequest(&lyonv1.MoveRequest{
		Path: "a", ToDir: "fresh",
	})); err != nil {
		t.Fatalf("Move dir: %v", err)
	}
	// a → fresh/a; moving fresh into fresh/a is subtree + already-exists.
	if _, err := client.Move(ctx, connect.NewRequest(&lyonv1.MoveRequest{
		Path: "fresh", ToDir: "fresh",
	})); connectCode(err) != connect.CodeInvalidArgument && connectCode(err) != connect.CodeAlreadyExists {
		t.Fatalf("move-into-self code = %v, want InvalidArgument/AlreadyExists (err=%v)", connectCode(err), err)
	}
	if _, err := client.Move(ctx, connect.NewRequest(&lyonv1.MoveRequest{
		Path: "note.md", ToDir: "nowhere",
	})); connectCode(err) != connect.CodeNotFound {
		t.Fatalf("move to missing dir code = %v, want NotFound", connectCode(err))
	}

	// --- Delete: empty dir bare, non-empty requires recursive, root guard.
	// (a moved to fresh/a above — seed a fresh empty dir instead.)
	if _, err := client.Mkdir(ctx, connect.NewRequest(&lyonv1.MkdirRequest{Path: "emptydir"})); err != nil {
		t.Fatalf("Mkdir emptydir: %v", err)
	}
	if _, err := client.Delete(ctx, connect.NewRequest(&lyonv1.DeleteRequest{Path: "emptydir"})); err != nil {
		t.Fatalf("Delete empty dir: %v", err)
	}
	if _, err := client.Delete(ctx, connect.NewRequest(&lyonv1.DeleteRequest{Path: "fresh"})); connectCode(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Delete non-empty code = %v, want FailedPrecondition", connectCode(err))
	}
	if _, err := client.Delete(ctx, connect.NewRequest(&lyonv1.DeleteRequest{Path: "fresh", Recursive: true})); err != nil {
		t.Fatalf("Delete recursive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh still there: %v", err)
	}
	if _, err := client.Delete(ctx, connect.NewRequest(&lyonv1.DeleteRequest{Path: ".", Recursive: true})); connectCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("Delete root code = %v, want InvalidArgument", connectCode(err))
	}
	if _, err := client.Delete(ctx, connect.NewRequest(&lyonv1.DeleteRequest{Path: "ghost.txt"})); connectCode(err) != connect.CodeNotFound {
		t.Fatalf("Delete missing code = %v, want NotFound", connectCode(err))
	}

	// --- UploadFile: create, conflict, overwrite, over-cap.
	data := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "up.png", Data: data,
	})); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "up.png", Data: data,
	})); connectCode(err) != connect.CodeAlreadyExists {
		t.Fatalf("re-upload code = %v, want AlreadyExists", connectCode(err))
	}
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "up.png", Data: data, Overwrite: true,
	})); err != nil {
		t.Fatalf("overwrite upload: %v", err)
	}
	// Upload is bounded by the binary cap, not the 2MB text-write cap: a
	// pasted screenshot at the model's own 3.5MB ceiling must land.
	shot := make([]byte, 3_500_000)
	copy(shot, data)
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "shot.png", Data: shot,
	})); err != nil {
		t.Fatalf("3.5MB upload (pasted screenshot): %v", err)
	}
	big := make([]byte, fileBinaryCap+1)
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "big.bin", Data: big,
	})); connectCode(err) != connect.CodeResourceExhausted {
		t.Fatalf("over-cap upload code = %v, want ResourceExhausted", connectCode(err))
	}
	// Parent must pre-exist.
	if _, err := client.UploadFile(ctx, connect.NewRequest(&lyonv1.UploadFileRequest{
		Path: "missing-dir/f.bin", Data: data,
	})); connectCode(err) != connect.CodeNotFound {
		t.Fatalf("upload to missing parent code = %v, want NotFound", connectCode(err))
	}
}

func TestFileServiceReadBinaryFile(t *testing.T) {
	client, root := newFileFixture(t)
	ctx := context.Background()

	res, err := client.ReadBinaryFile(ctx, connect.NewRequest(&lyonv1.ReadBinaryFileRequest{Path: "img.png"}))
	if err != nil {
		t.Fatalf("ReadBinaryFile: %v", err)
	}
	if !bytes.HasPrefix(res.Msg.Data, []byte("\x89PNG")) || res.Msg.ContentType != "image/png" || res.Msg.Truncated {
		t.Fatalf("png read = type %q truncated %v len %d", res.Msg.ContentType, res.Msg.Truncated, len(res.Msg.Data))
	}

	// Unknown extension falls back to content sniffing (PNG magic wins).
	if err := os.WriteFile(filepath.Join(root, "magic.dat"), []byte("\x89PNG\r\n\x1a\nxx"), 0o644); err != nil {
		t.Fatalf("seed magic.dat: %v", err)
	}
	dat, err := client.ReadBinaryFile(ctx, connect.NewRequest(&lyonv1.ReadBinaryFileRequest{Path: "magic.dat"}))
	if err != nil || dat.Msg.ContentType != "image/png" {
		t.Fatalf("sniffed type = %q err=%v, want image/png", dat.Msg.ContentType, err)
	}

	// Over the binary cap: truncated with the full original size reported.
	if err := os.WriteFile(filepath.Join(root, "huge.bin"), bytes.Repeat([]byte{0xAB}, fileBinaryCap+64), 0o644); err != nil {
		t.Fatalf("seed huge.bin: %v", err)
	}
	huge, err := client.ReadBinaryFile(ctx, connect.NewRequest(&lyonv1.ReadBinaryFileRequest{Path: "huge.bin"}))
	if err != nil || !huge.Msg.Truncated || len(huge.Msg.Data) != fileBinaryCap || huge.Msg.SizeBytes != fileBinaryCap+64 {
		t.Fatalf("huge: truncated=%v len=%d size=%d err=%v", huge.Msg.Truncated, len(huge.Msg.Data), huge.Msg.SizeBytes, err)
	}
}

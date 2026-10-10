package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG drops a tiny PNG into dir and returns the path relative to dir.
func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return name
}

func TestImageMediaTypeSniff(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0}, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"gif87a", append([]byte("GIF87a"), 0, 0), "image/gif"},
		{"gif89a", append([]byte("GIF89a"), 0, 0), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"},
		{"text", []byte("hello world"), ""},
		{"empty", nil, ""},
		{"truncated png", []byte{0x89, 'P'}, ""},
	}
	for _, tc := range cases {
		if got := ImageMediaType(tc.data); got != tc.want {
			t.Errorf("%s: ImageMediaType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestViewImageReturnsImageBlock(t *testing.T) {
	dir := tempDir(t)
	writePNG(t, dir, "shot.png", 8, 6)

	v := &ViewImage{Root: dir}
	res, err := v.Execute(context.Background(), json.RawMessage(`{"path":"shot.png"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("blocks = %d, want 2 (caption + image)", len(res))
	}
	if res[0].OfText == nil || !strings.Contains(res[0].OfText.Text, "8x6") {
		t.Fatalf("caption block = %+v, want dimensions", res[0])
	}
	if res[1].OfImage == nil {
		t.Fatalf("second block = %+v, want image", res[1])
	}
	src := res[1].OfImage.Source.OfBase64
	if src == nil {
		t.Fatal("image block has no base64 source")
	}
	if string(src.MediaType) != "image/png" {
		t.Fatalf("media type = %q, want image/png", src.MediaType)
	}
	raw, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if ImageMediaType(raw) != "image/png" {
		t.Fatal("payload does not decode back to a PNG")
	}
}

func TestViewImageRejectsNonImage(t *testing.T) {
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "fake.png"), []byte("just text"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := &ViewImage{Root: dir}
	_, err := v.Execute(context.Background(), json.RawMessage(`{"path":"fake.png"}`))
	if err == nil || !strings.Contains(err.Error(), "not a supported image") {
		t.Fatalf("err = %v, want 'not a supported image'", err)
	}
}

func TestViewImageConfineRejectsEscape(t *testing.T) {
	dir := tempDir(t)
	v := &ViewImage{Root: dir}
	if _, err := v.Execute(context.Background(), json.RawMessage(`{"path":"../outside.png"}`)); err == nil {
		t.Fatal("path escape should be rejected")
	}
}

func TestPromptBlocksNoAttachmentIsSingleText(t *testing.T) {
	const p = "hello  world\n@  mail@example.com"
	blocks := PromptBlocks(tempDir(t), p)
	if len(blocks) != 1 || blocks[0].OfText == nil {
		t.Fatalf("blocks = %+v, want one text block", blocks)
	}
	if blocks[0].OfText.Text != p {
		t.Fatalf("text = %q, want prompt verbatim", blocks[0].OfText.Text)
	}
}

func TestPromptBlocksAttachesImage(t *testing.T) {
	dir := tempDir(t)
	writePNG(t, dir, "shot.png", 8, 6)

	blocks := PromptBlocks(dir, "看看 @shot.png 和 @missing.png")
	want := 4 // text, caption, image, trailing text (missing file stays text)
	if len(blocks) != want {
		t.Fatalf("blocks = %d, want %d: %+v", len(blocks), want, blocks)
	}
	if blocks[0].OfText == nil || blocks[0].OfText.Text != "看看 " {
		t.Fatalf("lead text = %+v", blocks[0])
	}
	if blocks[1].OfText == nil || !strings.Contains(blocks[1].OfText.Text, "shot.png") {
		t.Fatalf("caption = %+v", blocks[1])
	}
	if blocks[2].OfImage == nil {
		t.Fatalf("third block = %+v, want image", blocks[2])
	}
	if blocks[3].OfText == nil || blocks[3].OfText.Text != " 和 @missing.png" {
		t.Fatalf("tail text = %+v", blocks[3])
	}
}

func TestPromptBlocksQuotedAndPunctuatedTokens(t *testing.T) {
	dir := tempDir(t)
	writePNG(t, dir, "my shot.png", 4, 4)
	writePNG(t, dir, "a.png", 4, 4)

	blocks := PromptBlocks(dir, `img @"my shot.png" then @a.png,`)
	if len(blocks) != 6 {
		t.Fatalf("blocks = %d, want 6 (text,cap,img,text,cap,img): %+v", len(blocks), blocks)
	}
	if blocks[2].OfImage == nil || blocks[5].OfImage == nil {
		t.Fatalf("expected two image blocks: %+v", blocks)
	}
}

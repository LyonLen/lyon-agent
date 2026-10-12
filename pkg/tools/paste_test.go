package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noisyPNG builds a random-colored PNG: real entropy, so it encodes far
// larger than a flat image of the same size — what the byte cap has to
// deal with when a screenshot lands.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(w*7919 + h*104729)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestPasteImageStoresAttachment(t *testing.T) {
	dir := tempDir(t)
	data := noisyPNG(t, 24, 16)

	rel, mt, w, h, err := PasteImage(dir, data)
	if err != nil {
		t.Fatalf("PasteImage: %v", err)
	}
	if !strings.HasPrefix(rel, ".lyon/paste/") || !strings.HasSuffix(rel, ".png") {
		t.Fatalf("rel = %q, want .lyon/paste/*.png", rel)
	}
	if mt != "image/png" || w != 24 || h != 16 {
		t.Fatalf("mt/w/h = %q %dx%d, want image/png 24x16", mt, w, h)
	}
	if stored, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("stored file: %v", err)
	} else if !bytes.Equal(stored, data) {
		t.Fatal("stored bytes differ from the pasted ones — a small image must not be re-encoded")
	}

	// The whole point: the token resolves through the normal @path
	// attachment path into an image block.
	blocks := PromptBlocks(dir, "look @"+rel)
	if len(blocks) != 3 {
		t.Fatalf("PromptBlocks = %d blocks, want text + note + image", len(blocks))
	}
}

func TestPasteImageRejectsNonImage(t *testing.T) {
	dir := tempDir(t)
	if _, _, _, _, err := PasteImage(dir, []byte("just some text")); err == nil {
		t.Fatal("PasteImage accepted non-image data")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(pasteDirRel))); !os.IsNotExist(err) {
		t.Fatalf("paste dir created for rejected data: %v", err)
	}
}

func TestFitImageShrinksToLimits(t *testing.T) {
	data := noisyPNG(t, 128, 96)

	t.Run("dim", func(t *testing.T) {
		out, mt, w, h, err := fitImage(data, maxImageBytes, 32)
		if err != nil {
			t.Fatalf("fitImage: %v", err)
		}
		if w > 32 || h > 32 {
			t.Fatalf("dims %dx%d, want both ≤ 32", w, h)
		}
		if mt != "image/png" {
			t.Fatalf("mt = %q, want image/png", mt)
		}
		if got := ImageMediaType(out); got != "image/png" {
			t.Fatalf("stored media type %q", got)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		const cap = 900
		out, _, w, h, err := fitImage(data, cap, maxImageDim)
		if err != nil {
			t.Fatalf("fitImage: %v", err)
		}
		if len(out) > cap {
			t.Fatalf("%d bytes > cap %d", len(out), cap)
		}
		if w == 0 || h == 0 {
			t.Fatalf("dimensions lost: %dx%d", w, h)
		}
	})

	t.Run("untouched", func(t *testing.T) {
		out, _, _, _, err := fitImage(data, maxImageBytes, maxImageDim)
		if err != nil {
			t.Fatalf("fitImage: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatal("an image that already fits must be returned verbatim")
		}
	})

	t.Run("webp too big", func(t *testing.T) {
		webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBPVP8 ")...)
		webp = append(webp, bytes.Repeat([]byte{1}, 64)...)
		if _, _, _, _, err := fitImage(webp, 16, maxImageDim); err == nil {
			t.Fatal("oversized undecodable image accepted")
		}
	})
}

func TestPasteImageRotatesOldFiles(t *testing.T) {
	dir := tempDir(t)
	paste := filepath.Join(dir, filepath.FromSlash(pasteDirRel))
	if err := os.MkdirAll(paste, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < pasteKeep+8; i++ {
		name := fmt.Sprintf("old-%03d.png", i)
		if err := os.WriteFile(filepath.Join(paste, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if _, _, _, _, err := PasteImage(dir, noisyPNG(t, 8, 8)); err != nil {
		t.Fatalf("PasteImage: %v", err)
	}
	entries, err := os.ReadDir(paste)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != pasteKeep {
		t.Fatalf("paste dir holds %d files, want the rotation cap %d", len(entries), pasteKeep)
	}
}

func TestPasteFileInWorkspaceStaysInPlace(t *testing.T) {
	dir := tempDir(t)
	writePNG(t, dir, "shot.png", 6, 4)

	rel, _, w, h, err := PasteFile(dir, filepath.Join(dir, "shot.png"))
	if err != nil {
		t.Fatalf("PasteFile: %v", err)
	}
	if rel != "shot.png" {
		t.Fatalf("rel = %q, want shot.png (no copy inside the workspace)", rel)
	}
	if w != 6 || h != 4 {
		t.Fatalf("dims = %dx%d, want 6x4", w, h)
	}
}

func TestPasteFileOutsideWorkspaceIsCopied(t *testing.T) {
	dir := tempDir(t)
	outside := t.TempDir()
	writePNG(t, outside, "elsewhere.png", 5, 5)

	rel, _, _, _, err := PasteFile(dir, filepath.Join(outside, "elsewhere.png"))
	if err != nil {
		t.Fatalf("PasteFile: %v", err)
	}
	if !strings.HasPrefix(rel, ".lyon/paste/") {
		t.Fatalf("rel = %q, want a copy under .lyon/paste/", rel)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("stored copy: %v", err)
	}
}

func TestPasteFileRejectsNonImage(t *testing.T) {
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, _, _, err := PasteFile(dir, filepath.Join(dir, "notes.txt")); err == nil {
		t.Fatal("PasteFile accepted a text file")
	}
}

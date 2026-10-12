//go:build windows

package main

// Unit tests for the clipboard decoder — the part of alt+v that can't be
// exercised against a real clipboard without one, so the DIB shapes are
// built here by hand and pushed through decodeDIB.

import (
	"encoding/binary"
	"image"
	imgcolor "image/color"
	"testing"
)

// dibFrom renders img as a synthetic CF_DIB block: a 40-byte
// BITMAPINFOHEADER followed by padded rows. bitCount 24 writes BGRX bytes
// with biCompression = BI_RGB (bottom-up unless topDown), bitCount 32
// writes BI_BITFIELDS with explicit masks right after the header.
func dibFrom(img *image.RGBA, bitCount int, topDown bool) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	bpp := bitCount / 8
	stride := (w*bpp + 3) / 4 * 4
	pad := 0
	compress := uint32(0)
	if bitCount == 32 {
		compress = 3 // BI_BITFIELDS: 3 masks × 4 bytes follow the header
		pad = 12
	}
	b := make([]byte, 40+pad+stride*h)
	binary.LittleEndian.PutUint32(b[0:], 40)
	binary.LittleEndian.PutUint32(b[4:], uint32(w))
	if topDown {
		binary.LittleEndian.PutUint32(b[8:], uint32(int32(-h)))
	} else {
		binary.LittleEndian.PutUint32(b[8:], uint32(h))
	}
	binary.LittleEndian.PutUint16(b[12:], 1)
	binary.LittleEndian.PutUint16(b[14:], uint16(bitCount))
	binary.LittleEndian.PutUint32(b[16:], compress)
	if compress == 3 { // R, G, B masks: 0x00FF0000 / 0x0000FF00 / 0x000000FF
		binary.LittleEndian.PutUint32(b[40:], 0x00FF0000)
		binary.LittleEndian.PutUint32(b[44:], 0x0000FF00)
		binary.LittleEndian.PutUint32(b[48:], 0x000000FF)
	}
	for y := 0; y < h; y++ {
		src := y
		if !topDown {
			src = h - 1 - y // file row 0 is the image's bottom row
		}
		for x := 0; x < w; x++ {
			c := img.RGBAAt(img.Bounds().Min.X+x, img.Bounds().Min.Y+src)
			o := 40 + pad + y*stride + x*bpp
			b[o], b[o+1] = c.B, c.G
			if bpp >= 3 {
				b[o+2] = c.R
			}
			if bpp == 4 {
				b[o+3] = 0
			}
		}
	}
	return b
}

// patternImage is 5×4 of unmistakable colours so a row-flip or a channel
// swap shows up immediately.
func patternImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 5, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 5; x++ {
			img.SetRGBA(x, y, colorOf(x, y))
		}
	}
	return img
}

func colorOf(x, y int) imgcolor.RGBA {
	return imgcolor.RGBA{uint8(20 + x*30), uint8(40 + y*50), uint8(60 + x*y*7), 255}
}

func requirePixels(t *testing.T, got image.Image, want *image.RGBA, what string) {
	t.Helper()
	if got.Bounds().Dx() != want.Bounds().Dx() || got.Bounds().Dy() != want.Bounds().Dy() {
		t.Fatalf("%s: size %v, want %v", what, got.Bounds(), want.Bounds())
	}
	for y := 0; y < want.Bounds().Dy(); y++ {
		for x := 0; x < want.Bounds().Dx(); x++ {
			gr, gg, gb, ga := got.At(x, y).RGBA()
			w := want.RGBAAt(x, y)
			if uint8(gr>>8) != w.R || uint8(gg>>8) != w.G ||
				uint8(gb>>8) != w.B || uint8(ga>>8) != w.A {
				t.Fatalf("%s: pixel (%d,%d) = %d,%d,%d,%d want %d,%d,%d,%d",
					what, x, y, gr>>8, gg>>8, gb>>8, ga>>8, w.R, w.G, w.B, w.A)
			}
		}
	}
}

// The default clipboard shape: bottom-up, 24bpp, BI_RGB.
func TestDecodeDIB24BottomUp(t *testing.T) {
	want := patternImage()
	got, err := decodeDIB(dibFrom(want, 24, false))
	if err != nil {
		t.Fatalf("decodeDIB: %v", err)
	}
	requirePixels(t, got, want, "24bpp bottom-up")
}

// A top-down 32bpp BI_BITFIELDS block: masks carried after the header,
// first file row is the image's first row.
func TestDecodeDIB32TopDownBitfields(t *testing.T) {
	want := patternImage()
	got, err := decodeDIB(dibFrom(want, 32, true))
	if err != nil {
		t.Fatalf("decodeDIB: %v", err)
	}
	requirePixels(t, got, want, "32bpp top-down bitfields")
}

// Truncated pixel data must be reported, not decoded into garbage.
func TestDecodeDIBTruncated(t *testing.T) {
	b := dibFrom(patternImage(), 24, false)
	if _, err := decodeDIB(b[:len(b)-7]); err == nil {
		t.Fatal("truncated DIB accepted")
	}
	if _, err := decodeDIB([]byte{1, 2, 3}); err == nil {
		t.Fatal("header shorter than a BITMAPINFOHEADER accepted")
	}
}

// Compressed variants are refused with a message instead of guessed at.
func TestDecodeDIBRejectsCompression(t *testing.T) {
	b := dibFrom(patternImage(), 24, false)
	binary.LittleEndian.PutUint32(b[16:], 1) // BI_RLE8
	if _, err := decodeDIB(b); err == nil {
		t.Fatal("RLE-compressed DIB accepted")
	}
}

// maskValue normalises a channel: the top of a 5-bit field must be 255.
func TestMaskValueScaling(t *testing.T) {
	if got := maskValue(0x1F, 0x7C00); got != 0 { // wrong mask for this field
		t.Fatalf("field outside mask = %d, want 0", got)
	}
	if got := maskValue(0x7C00, 0x7C00); got != 255 {
		t.Fatalf("max of 5-bit field = %d, want 255", got)
	}
	if got := maskValue(0x03E0, 0x03E0); got != 255 {
		t.Fatalf("green at max = %d, want 255", got)
	}
	if got := maskValue(0x0400, 0x7C00); got == 0 || got == 255 {
		t.Fatalf("low step of the field = %d, want something between", got)
	}
}

// dropFileList builds a CF_HDROP payload: DROPFILES header (fWide at
// byte 16) then a double-null-terminated path list.
func dropFileList(wide bool, paths ...string) []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:], 20) // pFiles
	if wide {
		binary.LittleEndian.PutUint32(b[16:], 1)
		for _, p := range paths {
			for _, u := range append(utf16Encode(p), 0) {
				b = binary.LittleEndian.AppendUint16(b, u)
			}
		}
		return binary.LittleEndian.AppendUint16(b, 0)
	}
	for _, p := range paths {
		b = append(b, p...)
		b = append(b, 0)
	}
	return append(b, 0)
}

func utf16Encode(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		out = append(out, uint16(r))
	}
	return out
}

// Explorer's copy: paths, not pixels — alt+v attaches the file itself.
func TestDropPathsWideAndANSI(t *testing.T) {
	want := []string{`C:\shots\one.png`, `C:\Users\me\two.jpg`}
	got := dropPaths(dropFileList(true, want...))
	if len(got) != len(want) {
		t.Fatalf("wide: got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wide[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	got = dropPaths(dropFileList(false, want...))
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ansi: got %q, want %q", got, want)
	}
	if dropPaths([]byte{1, 2, 3}) != nil {
		t.Fatal("payload shorter than a DROPFILES header parsed")
	}
}

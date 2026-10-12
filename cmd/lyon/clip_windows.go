//go:build windows

package main

// Windows clipboard, read straight through the Win32 API: alt+v has to
// work while another program may be holding the clipboard, and shelling
// out to PowerShell costs ~0.5s of process startup on every paste (the
// toast path in notify_windows.go pays that deliberately — a notification
// is fire-and-forget; a paste the user is watching is not).
//
// The clipboard is only ever read here: data is copied out while the
// clipboard is open, then every handle is released, so nothing the OS owns
// outlives the call.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	imgcolor "image/color"
	"image/png"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"lyon-agent/pkg/tools"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard           = user32.NewProc("OpenClipboard")
	procCloseClipboard          = user32.NewProc("CloseClipboard")
	procGetClipboardData        = user32.NewProc("GetClipboardData")
	procRegisterClipboardFormat = user32.NewProc("RegisterClipboardFormatW")
	procGlobalLock              = kernel32.NewProc("GlobalLock")
	procGlobalUnlock            = kernel32.NewProc("GlobalUnlock")
	procGlobalSize              = kernel32.NewProc("GlobalSize")
)

// Standard clipboard formats. "PNG" is not one of them — apps that have a
// PNG payload ready register the format by name — so it is resolved once
// through RegisterClipboardFormatW and reused.
const (
	cfDIB        = 8
	cfDIBV5      = 17
	cfHDROP      = 15
	pngFmtName   = "PNG"
	clipboardCap = 1 << 30 // 1 GiB sanity ceiling before a size is trusted
)

var pngFormatID = sync.OnceValue(func() uintptr {
	name, err := windows.UTF16PtrFromString(pngFmtName)
	if err != nil {
		return 0
	}
	id, _, _ := procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(name)))
	return id
})

// clipPastePlatform reads the clipboard: image bytes when a picture sits
// on it (screenshot, "copy image"), otherwise the paths of files copied in
// Explorer, otherwise an error explaining what is missing.
func clipPastePlatform() (data []byte, paths []string, err error) {
	if err := openClipboard(); err != nil {
		return nil, nil, err
	}
	defer procCloseClipboard.Call()

	// Pictures first: copying a file leaves CF_HDROP *and* no pixels, while
	// a screenshot leaves pixels and no paths, so the order only matters
	// for the rare app that puts both — there, the pixels are what paste
	// means.
	if b := clipboardBytes(pngFormatID()); len(b) > 0 && tools.ImageMediaType(b) == "image/png" {
		return b, nil, nil
	}
	var dibErr error
	for _, fmtID := range []uintptr{cfDIB, cfDIBV5} {
		b := clipboardBytes(fmtID)
		if len(b) == 0 {
			continue
		}
		if enc, err := dibPNG(b); err == nil {
			return enc, nil, nil
		} else {
			dibErr = err
		}
	}
	if b := clipboardBytes(cfHDROP); len(b) > 0 {
		if paths := dropPaths(b); len(paths) > 0 {
			return nil, paths, nil
		}
	}
	if dibErr != nil {
		return nil, nil, dibErr
	}
	return nil, nil, errors.New("clipboard holds no image — copy a screenshot or an image file first")
}

// openClipboard takes the clipboard, retrying briefly: a clipboard viewer,
// RDP or a sync tool holding it for a moment is normal, and failing the
// paste on the first collision would be a flake the user can't explain.
func openClipboard() error {
	for i := 0; i < 50; i++ {
		if ok, _, _ := procOpenClipboard.Call(0); ok != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("clipboard is busy — another program is holding it")
}

// clipboardBytes copies one clipboard payload out under GlobalLock. The
// memory belongs to the clipboard and is freed by the next owner, so the
// bytes are copied before the handle is released.
func clipboardBytes(format uintptr) []byte {
	if format == 0 {
		return nil
	}
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return nil
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer procGlobalUnlock.Call(h)
	n, _, _ := procGlobalSize.Call(h)
	if n == 0 || n > clipboardCap {
		return nil
	}
	// GlobalLock hands the address back as a uintptr (the Win32
	// convention) — load it as a pointer word instead of converting the
	// number, which is what keeps `go vet` quiet about a value that is
	// valid only while the lock above is held.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	return append([]byte(nil), unsafe.Slice((*byte)(ptr), int(n))...)
}

// dibPNG converts a CF_DIB block into PNG bytes — the media type the API
// accepts — via the header-aware decoder below.
func dibPNG(b []byte) ([]byte, error) {
	img, err := decodeDIB(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode pasted bitmap: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeDIB turns a Windows DIB (BITMAPINFOHEADER, optional colour table
// and masks, then padded rows) into an image. What actually lands on the
// clipboard — 1/4/8-bit palettes, 16/24/32-bit BI_RGB and BI_BITFIELDS,
// top-down or bottom-up — decodes exactly; RLE and compressed-in-place
// variants are rejected with a message rather than guessed at.
func decodeDIB(b []byte) (image.Image, error) {
	if len(b) < 40 {
		return nil, fmt.Errorf("bitmap header is %d bytes, need 40", len(b))
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }

	biSize := int(u32(0))
	width, height := int(int32(u32(4))), int(int32(u32(8)))
	bitCount, compress, clrUsed := int(u16(14)), u32(16), int(u32(32))
	switch {
	case biSize < 40 || biSize > len(b):
		return nil, fmt.Errorf("bitmap header size %d is out of range", biSize)
	case biSize != 40 && biSize < 52:
		return nil, fmt.Errorf("bitmap header size %d is neither BITMAPINFOHEADER nor V2+", biSize)
	case width <= 0 || height == 0 || width > 1<<15 || height > 1<<15 || height < -(1<<15):
		return nil, fmt.Errorf("bitmap dimensions %dx%d are out of range", width, height)
	case bitCount != 1 && bitCount != 4 && bitCount != 8 &&
		bitCount != 16 && bitCount != 24 && bitCount != 32:
		return nil, fmt.Errorf("bitmap depth %d bpp is not supported", bitCount)
	}
	topDown := height < 0
	if topDown {
		height = -height
	}

	// Colour masks: BITMAPV2+ embeds them in the header, a plain 40-byte
	// header carries them right after itself when BI_BITFIELDS is set, and
	// BI_RGB implies them from the depth.
	var rMask, gMask, bMask, aMask uint32
	off := biSize
	if biSize >= 52 {
		rMask, gMask, bMask = u32(40), u32(44), u32(48)
		if biSize >= 56 {
			aMask = u32(52)
		}
	}
	switch compress {
	case 0: // BI_RGB
		if rMask == 0 && gMask == 0 && bMask == 0 {
			switch bitCount {
			case 16:
				rMask, gMask, bMask = 0x7C00, 0x03E0, 0x001F // default 5-5-5
			case 24, 32:
				rMask, gMask, bMask = 0x00FF0000, 0x0000FF00, 0x000000FF // BGRX bytes
			}
		}
	case 3, 6: // BI_BITFIELDS, BI_ALPHABITFIELDS
		if biSize == 40 {
			n := 12
			if compress == 6 {
				n = 16
			}
			if off+n > len(b) {
				return nil, errors.New("bitmap colour masks are missing")
			}
			rMask, gMask, bMask = u32(off), u32(off+4), u32(off+8)
			if n == 16 {
				aMask = u32(off + 12)
			}
			off += n
		}
		if rMask == 0 && gMask == 0 && bMask == 0 {
			return nil, errors.New("bitmap declares no colour masks")
		}
	default:
		return nil, fmt.Errorf("bitmap compression %d is not supported (need uncompressed or bitfields)", compress)
	}

	// Colour table: only meaningful up to 8 bpp, and only when it is
	// actually present (biClrUsed = 0 means "the full 2^bpp table").
	pal := b[off:off] // empty by default; resliced below
	if bitCount <= 8 {
		n := clrUsed
		if n == 0 {
			n = 1 << bitCount
		}
		if off+n*4 > len(b) {
			return nil, errors.New("bitmap colour table is truncated")
		}
		pal = b[off : off+n*4]
		off += n * 4
	}

	stride := (width*bitCount + 31) / 32 * 4
	if off+stride*height > len(b) {
		return nil, fmt.Errorf("bitmap pixel data is truncated: %d bytes for %dx%d %dbpp",
			len(b), width, height, bitCount)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		row := y
		if !topDown {
			row = height - 1 - y // BI_RGB rows start at the bottom
		}
		line := b[off+row*stride:]
		for x := 0; x < width; x++ {
			var cr, cg, cb, ca uint8
			switch bitCount {
			case 32:
				v := binary.LittleEndian.Uint32(line[x*4:])
				cr, cg, cb = maskValue(v, rMask), maskValue(v, gMask), maskValue(v, bMask)
				if aMask != 0 {
					ca = maskValue(v, aMask)
				} else {
					ca = 255
				}
			case 24:
				v := uint32(line[x*3]) | uint32(line[x*3+1])<<8 | uint32(line[x*3+2])<<16
				cr, cg, cb, ca = maskValue(v, rMask), maskValue(v, gMask), maskValue(v, bMask), 255
			case 16:
				v := uint32(binary.LittleEndian.Uint16(line[x*2:]))
				cr, cg, cb, ca = maskValue(v, rMask), maskValue(v, gMask), maskValue(v, bMask), 255
			default: // 1, 4, 8 — a palette index
				var idx int
				switch bitCount {
				case 8:
					idx = int(line[x])
				case 4:
					idx = int(line[x/2] >> 4)
					if x%2 == 1 {
						idx = int(line[x/2] & 0x0F)
					}
				case 1:
					idx = int(line[x/8]>>(7-x%8)) & 1
				}
				if idx*4+3 >= len(pal) {
					return nil, fmt.Errorf("bitmap colour table index %d is out of range", idx)
				}
				cb, cg, cr = pal[idx*4], pal[idx*4+1], pal[idx*4+2]
				// The reserved byte is 0 in a BI_RGB table; treat that as
				// opaque rather than fully transparent.
				ca = pal[idx*4+3]
				if ca == 0 {
					ca = 255
				}
			}
			// image.RGBA stores alpha-premultiplied channels.
			img.SetRGBA(x, y, imgcolor.RGBA{
				R: uint8(uint32(cr) * uint32(ca) / 255),
				G: uint8(uint32(cg) * uint32(ca) / 255),
				B: uint8(uint32(cb) * uint32(ca) / 255),
				A: ca,
			})
		}
	}
	return img, nil
}

// maskValue extracts one channel through its bitfield and normalises it to
// 0–255 (5-bit 0x1F must map to 255, not to 31).
func maskValue(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := uint(0)
	for mask&1 == 0 {
		mask >>= 1
		shift++
	}
	bits := uint(0)
	for ; mask&1 != 0; mask >>= 1 {
		bits++
	}
	if bits == 0 || bits > 32 {
		return 0
	}
	max := uint32(1)<<bits - 1
	field := (v >> shift) & max
	return uint8((field*255 + max/2) / max)
}

// dropPaths parses a CF_HDROP payload: a DROPFILES header (offset,
// point, flags — fWide at byte 16 says UTF-16) followed by a
// double-null-terminated path list.
func dropPaths(b []byte) []string {
	if len(b) < 20 {
		return nil
	}
	pFiles := int(binary.LittleEndian.Uint32(b[0:4]))
	if pFiles <= 0 || pFiles >= len(b) {
		return nil
	}
	if binary.LittleEndian.Uint32(b[16:20]) == 0 { // ANSI
		var out []string
		for _, s := range strings.Split(string(b[pFiles:]), "\x00") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	units := make([]uint16, 0, (len(b)-pFiles)/2)
	for i := pFiles; i+1 < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:]))
	}
	var out []string
	for start := 0; start < len(units); {
		end := start
		for end < len(units) && units[end] != 0 {
			end++
		}
		if end == start { // an empty entry closes the list
			break
		}
		out = append(out, syscall.UTF16ToString(units[start:end]))
		start = end + 1
	}
	return out
}

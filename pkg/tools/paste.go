package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Clipboard paste — where an alt+v screenshot lands. The model only ever
// sees workspace paths (PromptBlocks → loadImage → confine), so a pasted
// picture is written into the workspace and handed back as an @path token:
// the same attachment syntax a file already sitting there would use.
//
// Two things this file guarantees:
//
//   - Limits are enforced here, not at submit time. The API rejects images
//     over ~3.5 MB or 8000 px a side; a 4K screenshot pasted at full size
//     would only blow up one keystroke later, so oversized data is
//     re-encoded smaller first.
//   - The directory stays bounded. Pasted files stay on disk so history
//     replay still resolves them, but an agent that pastes all day must
//     not grow the workspace without end.
const (
	// pasteDirRel is the paste directory: under .lyon (git-ignored, the
	// same home as project memory) but still inside the workspace root,
	// which is what @path resolution requires.
	pasteDirRel = ".lyon/paste"
	// pasteKeep is how many pasted files survive; older ones are removed
	// as new ones arrive.
	pasteKeep = 32
)

// PasteImage stores clipboard image data under <root>/.lyon/paste and
// returns the workspace-relative path (slash-separated, ready to drop into
// an @path token), the media type it was stored as, and the dimensions the
// model will see. Data the API would refuse is re-encoded smaller first;
// non-images are rejected before anything is written.
func PasteImage(root string, data []byte) (rel, mt string, w, h int, err error) {
	if strings.TrimSpace(root) == "" {
		root = "." // confine() treats an empty root as the working directory
	}
	stored, mt, w, h, err := fitImage(data, maxImageBytes, maxImageDim)
	if err != nil {
		return "", "", 0, 0, err
	}
	name := fmt.Sprintf("paste-%s%s", time.Now().Format("20060102-150405"), extFor(mt))
	dir := filepath.Join(root, filepath.FromSlash(pasteDirRel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", 0, 0, fmt.Errorf("create %s: %w", pasteDirRel, err)
	}
	path, err := uniquePath(dir, name)
	if err != nil {
		return "", "", 0, 0, err
	}
	if err := os.WriteFile(path, stored, 0o644); err != nil {
		return "", "", 0, 0, fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	rotatePaste(dir)
	rel, err = filepath.Rel(root, path)
	if err != nil {
		return "", "", 0, 0, err
	}
	return filepath.ToSlash(rel), mt, w, h, nil
}

// PasteFile attaches an image file picked up from the clipboard (a picture
// copied in a file manager, so it arrives as a path rather than as pixels).
// A file already inside the workspace is referenced in place — nothing to
// copy — while anything outside is read and stored like pasted bytes: the
// model may only be handed workspace paths.
func PasteFile(root, p string) (rel, mt string, w, h int, err error) {
	if strings.TrimSpace(p) == "" {
		return "", "", 0, 0, fmt.Errorf("empty path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("resolve workspace: %w", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("resolve %s: %w", p, err)
	}
	if rel, err := filepath.Rel(rootAbs, filepath.Clean(abs)); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		mt, _, w, h, err = loadImage(root, abs)
		if err != nil {
			return "", "", 0, 0, err
		}
		return filepath.ToSlash(rel), mt, w, h, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("read %s: %w", p, err)
	}
	return PasteImage(root, data)
}

// fitImage returns data trimmed to maxBytes/maxDim plus its media type and
// dimensions. Images that already fit come back untouched — re-encoding a
// small paste would be a lossy round trip for nothing. Everything else is
// decoded and box-shrunk, trying PNG first at each size (screenshots are
// text: lossless beats a smaller file) and JPEG next (photos and noisy 4K
// shots buy far more bytes back per pixel than another halving does), and
// only then spending more resolution.
func fitImage(data []byte, maxBytes, maxDim int) (out []byte, mt string, w, h int, err error) {
	mt = ImageMediaType(data)
	if mt == "" {
		return nil, "", 0, 0, fmt.Errorf("clipboard image is not png, jpeg, gif or webp")
	}
	w, h, ok := imageDimensions(data)
	switch {
	case ok && w <= maxDim && h <= maxDim && len(data) <= maxBytes:
		return data, mt, w, h, nil // already fits: hand it through verbatim
	case !ok:
		// Sniffable but not decodable here (webp has no stdlib decoder):
		// the size limit can still be checked, the pixel limit cannot.
		if len(data) <= maxBytes {
			return data, mt, 0, 0, nil
		}
		return nil, "", 0, 0, fmt.Errorf("image is %d bytes; at most %d bytes of image data fit in one request", len(data), maxBytes)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decode pasted image: %w", err)
	}
	// Smallest whole-pixel factor that brings both sides under maxDim,
	// then double it only while the encoded bytes still overflow.
	factor := 1
	for (w+factor-1)/factor > maxDim || (h+factor-1)/factor > maxDim {
		factor++
	}
	for {
		shrunk := boxShrink(src, factor)
		nw, nh := shrunk.Bounds().Dx(), shrunk.Bounds().Dy()
		var buf bytes.Buffer
		if err := png.Encode(&buf, shrunk); err != nil {
			return nil, "", 0, 0, fmt.Errorf("re-encode pasted image: %w", err)
		}
		if buf.Len() <= maxBytes {
			return buf.Bytes(), "image/png", nw, nh, nil
		}
		// Lossless lost this round. JPEG buys a lot of bytes back — try it
		// before spending more resolution: for photos and noisy 4K
		// screenshots it usually fits at full size, while the next box
		// shrink halves the pixels the model gets to read.
		var jb bytes.Buffer
		if err := jpeg.Encode(&jb, flattenWhite(shrunk), &jpeg.Options{Quality: 85}); err == nil && jb.Len() <= maxBytes {
			return jb.Bytes(), "image/jpeg", nw, nh, nil
		}
		if nw <= 16 || nh <= 16 || factor >= 1<<12 {
			return nil, "", 0, 0, fmt.Errorf("image is %d bytes; even re-encoded it exceeds the %d byte limit", len(data), maxBytes)
		}
		factor *= 2
	}
}

// boxShrink averages factor×factor source blocks into one destination
// pixel — a plain box filter: no dependency, deterministic, good enough
// for screenshots (the alternative is pulling in a resampling library for
// a single call site).
func boxShrink(src image.Image, factor int) *image.RGBA {
	if factor < 1 {
		factor = 1
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, (sw+factor-1)/factor, (sh+factor-1)/factor))
	for dy := 0; dy < dst.Bounds().Dy(); dy++ {
		for dx := 0; dx < dst.Bounds().Dx(); dx++ {
			var r, g, b, a, n uint64
			for sy := dy * factor; sy < min(dy*factor+factor, sh); sy++ {
				for sx := dx * factor; sx < min(dx*factor+factor, sw); sx++ {
					pr, pg, pb, pa := src.At(sb.Min.X+sx, sb.Min.Y+sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					b += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8),
				B: uint8(b / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}

// flattenWhite composites over white before a JPEG encode: JPEG has no
// alpha channel, and handing it premultiplied pixels would paint every
// transparent region black.
func flattenWhite(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			dst.Set(x, y, color.NRGBA{
				R: uint8((int(c.R)*int(c.A) + 255*(255-int(c.A))) / 255),
				G: uint8((int(c.G)*int(c.A) + 255*(255-int(c.A))) / 255),
				B: uint8((int(c.B)*int(c.A) + 255*(255-int(c.A))) / 255),
				A: 255,
			})
		}
	}
	return dst
}

// uniquePath picks a name inside dir that no existing file answers to.
// Two pastes inside the same second are normal (an agent script can do it
// deliberately), so the timestamp suffix gets a counter instead of
// silently overwriting the earlier attachment.
func uniquePath(dir, name string) (string, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	ext := filepath.Ext(name)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		path := filepath.Join(dir, candidate)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		}
	}
	return "", fmt.Errorf("paste directory %s holds too many files from the same second", pasteDirRel)
}

// rotatePaste drops the oldest pasted files once pasteKeep is exceeded.
// Attachments must outlive the prompt they were sent with (history replay
// re-resolves @path), so only the surplus goes.
func rotatePaste(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= pasteKeep {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	files := make([]file, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{name: e.Name(), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].name > files[j].name
	})
	for _, f := range files[min(pasteKeep, len(files)):] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

// extFor maps a stored media type to its file extension.
func extFor(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

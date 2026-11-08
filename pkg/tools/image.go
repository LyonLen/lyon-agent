package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // register GIF decoder for dimension checks
	_ "image/jpeg" // register JPEG decoder for dimension checks
	"image/png"    // named: encoder for magnified output; registers the decoder too
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// maxImageBytes caps raw image size. The API rejects base64 payloads
	// over 5 MB and base64 inflates by 4/3, so ~3.75 MB raw is the ceiling;
	// 3.5 MB leaves headroom for the surrounding message.
	maxImageBytes = 3_500_000
	// maxImageDim is the API's per-side pixel limit.
	maxImageDim = 8000
)

// ImageMediaType sniffs a supported image type from magic bytes and returns
// its media type ("image/png", "image/jpeg", "image/gif", "image/webp"), or
// "" when data is not one of those images.
func ImageMediaType(data []byte) string {
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	switch {
	case len(data) >= len(pngMagic) && bytes.Equal(data[:len(pngMagic)], pngMagic):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// imageDimensions decodes width/height from an image header. The stdlib
// contract is (format, config, err), but builds in the wild have been
// observed returning config first — so the Config is picked out by type
// rather than by position, and a decode failure (unknown format, e.g. webp)
// yields ok=false so callers skip the dimension check.
func imageDimensions(data []byte) (w, h int, ok bool) {
	var v0, v1 any
	v0, v1, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	for _, v := range []any{v0, v1} {
		if c, isCfg := v.(image.Config); isCfg {
			return c.Width, c.Height, true
		}
	}
	return 0, 0, false
}

// loadImage reads an image file under root, applies the API's size and
// dimension limits, and returns the sniffed media type, raw bytes, and
// decoded dimensions (0×0 when the format has no stdlib decoder, e.g. webp).
func loadImage(root, p string) (mt string, data []byte, w, h int, err error) {
	abs, err := Confine(root, p)
	if err != nil {
		return "", nil, 0, 0, err
	}
	data, err = os.ReadFile(abs)
	if err != nil {
		return "", nil, 0, 0, fmt.Errorf("read %s: %w", p, err)
	}
	mt = ImageMediaType(data)
	if mt == "" {
		return "", nil, 0, 0, fmt.Errorf("%s is not a supported image (need png, jpeg, gif or webp)", p)
	}
	if len(data) > maxImageBytes {
		return "", nil, 0, 0, fmt.Errorf("%s is %d bytes; at most %d bytes of image data fit in one request", p, len(data), maxImageBytes)
	}
	if dw, dh, ok := imageDimensions(data); ok {
		if dw > maxImageDim || dh > maxImageDim {
			return "", nil, 0, 0, fmt.Errorf("%s is %dx%d; the API rejects images larger than %d px on a side", p, dw, dh, maxImageDim)
		}
		w, h = dw, dh
	}
	return mt, data, w, h, nil
}

// imageNote is the caption block paired with an image so the model knows
// which file and dimensions it is looking at.
func imageNote(path, mt string, w, h int) string {
	if w > 0 && h > 0 {
		return fmt.Sprintf("[image: %s (%s, %dx%d)]", path, mt, w, h)
	}
	return fmt.Sprintf("[image: %s (%s)]", path, mt)
}

// ViewImage loads an image from the workspace and returns it as a model-
// visible image block. This is how the model sees pictures: read_file only
// handles UTF-8 text. Parallel-safe (read-only).
type ViewImage struct {
	Root string
}

func (v *ViewImage) Name() string { return "view_image" }

func (v *ViewImage) Description() string {
	return "Load an image (png, jpeg, gif, webp) from the workspace and return it as a viewable image block — use this to actually see screenshots, diagrams, photos or charts. " +
		"To read small text or dense detail, pass crop [x,y,w,h] and/or scale 2-4 to get just that region magnified in one call — that is the way to inspect fine detail. " +
		"Do not slice images open with shell scripts (PIL) to 'see' them: pixel probes are a fallback for measuring coordinates, not for reading. " +
		"read_file only reads UTF-8 text and refuses binary files. Paths are relative to the workspace root; absolute paths and .. traversal are rejected."
}

func (v *ViewImage) ParallelSafe() bool { return true }

type viewImageInput struct {
	Path  string `json:"path" jsonschema:"required,description=Image file path relative to the workspace root"`
	Crop  []int  `json:"crop,omitempty" jsonschema:"description=Optional region instead of the whole image: [x, y, width, height] in pixels, origin top-left; edges clamp to the image"`
	Scale int    `json:"scale,omitempty" jsonschema:"description=Optional magnifier 2-4 (integer); the returned image — whole or cropped — is enlarged that factor so small text stays readable. Omit or 1 for none"`
}

func (v *ViewImage) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[viewImageInput]()
}

func (v *ViewImage) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in viewImageInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	mt, data, w, h, err := loadImage(WorkspaceRoot(ctx, v.Root), in.Path)
	if err != nil {
		return nil, err
	}
	caption := imageNote(in.Path, mt, w, h)
	if len(in.Crop) > 0 || in.Scale > 1 {
		var m magnify
		if data, m, err = magnifyImage(data, in.Crop, in.Scale); err != nil {
			return nil, fmt.Errorf("%s: %w", in.Path, err)
		}
		mt = "image/png"
		caption = magnifyNote(in.Path, m)
	}
	res := ImageResult(mt, base64.StdEncoding.EncodeToString(data))
	return append(TextResult(caption), res...), nil
}

// magnify is what magnifyImage did, for the caption: the region returned
// (x, y, w, h in ORIGINAL pixels; the full image when no crop was asked),
// the output size after scaling (ow, oh), and the integer scale factor.
type magnify struct {
	x, y, w, h int
	ow, oh     int
	scale      int
	cropped    bool
}

// magnifyImage decodes an image, applies the crop rectangle (clamped to
// the image; out-of-origin is an error, over-long edges just clip) and an
// integer upscale (nearest-neighbor, so screenshot text stays crisp), and
// re-encodes as PNG. This exists so "read the small text" is ONE view_image
// call instead of a PIL-slice-save-view loop through the shell.
//
// Only formats the stdlib decodes (png, jpeg, gif — the blank imports in
// this file) can be magnified; webp reaches view_image for whole-image
// viewing only, and the error says so. The result is re-checked against
// the API's size and per-side dimension limits.
func magnifyImage(data []byte, crop []int, scale int) ([]byte, magnify, error) {
	var m magnify
	if len(crop) > 0 && len(crop) != 4 {
		return nil, m, fmt.Errorf("crop must be [x, y, width, height] (%d values), not %d", 4, len(crop))
	}
	if scale < 0 || scale > 4 {
		return nil, m, fmt.Errorf("scale %d out of range: use 1-4 (or omit)", scale)
	}
	if scale == 0 {
		scale = 1
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, m, fmt.Errorf("cannot crop/scale this format (png, jpeg and gif only; view webp whole or convert it first): %w", err)
	}
	b := src.Bounds()
	x, y, w, h := b.Min.X, b.Min.Y, b.Dx(), b.Dy()
	if len(crop) == 4 {
		cx, cy, cw, ch := crop[0], crop[1], crop[2], crop[3]
		if cw <= 0 || ch <= 0 {
			return nil, m, fmt.Errorf("crop size %dx%d must be positive", cw, ch)
		}
		if cx < 0 || cy < 0 {
			return nil, m, fmt.Errorf("crop origin %d,%d must not be negative", cx, cy)
		}
		if cx >= b.Dx() || cy >= b.Dy() {
			return nil, m, fmt.Errorf("crop origin %d,%d outside the %dx%d image", cx, cy, b.Dx(), b.Dy())
		}
		x, y = b.Min.X+cx, b.Min.Y+cy
		if cx+cw > b.Dx() {
			cw = b.Dx() - cx
		}
		if cy+ch > b.Dy() {
			ch = b.Dy() - cy
		}
		w, h = cw, ch
		m.cropped = true
	}
	m.x, m.y, m.w, m.h = x-b.Min.X, y-b.Min.Y, w, h
	m.scale, m.ow, m.oh = scale, w*scale, h*scale
	if m.ow > maxImageDim || m.oh > maxImageDim {
		return nil, m, fmt.Errorf("scaled result %dx%d exceeds the %d px API limit — crop a smaller region or lower the scale", m.ow, m.oh, maxImageDim)
	}

	var out image.Image
	if scale == 1 {
		// Plain crop: one draw, no per-pixel loop.
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), src, image.Pt(x, y), draw.Src)
		out = dst
	} else {
		// Nearest-neighbor upscale: integer replication keeps text edges
		// sharp where a bilinear filter would smear them.
		dst := image.NewRGBA(image.Rect(0, 0, m.ow, m.oh))
		for oy := 0; oy < m.oh; oy++ {
			sy := y + oy/scale
			for ox := 0; ox < m.ow; ox++ {
				dst.Set(ox, oy, src.At(x+ox/scale, sy))
			}
		}
		out = dst
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, m, fmt.Errorf("re-encode: %w", err)
	}
	if buf.Len() > maxImageBytes {
		return nil, m, fmt.Errorf("scaled result is %d bytes; at most %d fit in one request — crop a smaller region", buf.Len(), maxImageBytes)
	}
	return buf.Bytes(), m, nil
}

// magnifyNote is the caption block paired with the returned pixels: which
// region of WHICH file came back and at what size, so the model never has
// to guess whether it is looking at a crop or another picture.
func magnifyNote(path string, m magnify) string {
	switch {
	case m.cropped && m.scale > 1:
		return fmt.Sprintf("[image: %s (crop %d,%d %dx%d → %dx%d, scaled %dx)]", path, m.x, m.y, m.w, m.h, m.ow, m.oh, m.scale)
	case m.cropped:
		return fmt.Sprintf("[image: %s (crop %d,%d %dx%d)]", path, m.x, m.y, m.w, m.h)
	case m.scale > 1:
		return fmt.Sprintf("[image: %s (scaled %dx → %dx%d)]", path, m.scale, m.ow, m.oh)
	default:
		return imageNote(path, "image/png", m.ow, m.oh)
	}
}

// PromptBlocks splits a user prompt into content blocks, turning every
// "@path" token that resolves to a supported image inside root into an
// image block — the image-attachment syntax. Prompts without attachments
// come back as exactly one text block carrying the prompt verbatim, so the
// prompt cache and existing tests see no difference.
//
// An @token that names an image but fails to load (missing file, wrong
// format, over the size limit) stays in the plain text where the model can
// read the reason: the token is kept verbatim and followed by the load
// error in brackets. Silent degradation is what made "the model never saw
// my image" unguessable — the model sees the token either way, so it must
// also see why no picture came with it.
func PromptBlocks(root, prompt string) []anthropic.BetaContentBlockParamUnion {
	if !strings.Contains(prompt, "@") {
		return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)}
	}
	var (
		blocks []anthropic.BetaContentBlockParamUnion
		buf    strings.Builder // text not yet emitted as a block
		start  int             // index of the first byte not yet in buf
	)
	flush := func() {
		if buf.Len() > 0 {
			blocks = append(blocks, anthropic.NewBetaTextBlock(buf.String()))
			buf.Reset()
		}
	}
	i := 0
	for i < len(prompt) {
		// "@" only opens an attachment at a token start: "a@b.com" stays text.
		if prompt[i] != '@' || (i > 0 && !isPromptSpace(prompt[i-1])) {
			i++
			continue
		}
		tok, next := scanAtToken(prompt, i)
		if path, ok := attachmentPath(tok); ok {
			mt, data, w, h, err := loadImage(root, path)
			if err == nil {
				buf.WriteString(prompt[start:i])
				flush()
				blocks = append(blocks,
					anthropic.NewBetaTextBlock(imageNote(path, mt, w, h)),
					anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
						MediaType: anthropic.BetaBase64ImageSourceMediaType(mt),
						Data:      base64.StdEncoding.EncodeToString(data),
					}),
				)
				start = next
			} else {
				buf.WriteString(prompt[start:next])
				buf.WriteString(errorNote(prompt[i:next], err))
				start = next
			}
		}
		i = next
	}
	buf.WriteString(prompt[start:])
	flush()
	if len(blocks) == 0 {
		return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)}
	}
	return blocks
}

// errorNote renders one failed attachment as "[token: reason]" so the token
// itself stays visible in the prompt text next to the explanation.
func errorNote(token string, err error) string {
	msg := err.Error()
	// Drop the leading "<path>: " loadImage repeats — the token right here
	// already names the file, and the reason is the part worth reading.
	if i := strings.Index(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return fmt.Sprintf(" [%s: %s]", token, msg)
}

// scanAtToken returns the raw attachment token starting at the '@' at
// position i (without the '@') and the index just past the token. A quoted
// token — @"my shot.png" — may contain spaces; an unquoted one ends at the
// first space.
func scanAtToken(prompt string, i int) (string, int) {
	j := i + 1
	if j < len(prompt) && (prompt[j] == '"' || prompt[j] == '\'') {
		q := prompt[j]
		j++
		start := j
		for j < len(prompt) && prompt[j] != q {
			j++
		}
		end := j
		if j < len(prompt) {
			j++ // consume the closing quote
		}
		return prompt[start:end], j
	}
	start := j
	for j < len(prompt) && !isPromptSpace(prompt[j]) {
		j++
	}
	return prompt[start:j], j
}

// attachmentPath maps a raw @token to a workspace path when it looks like
// an image reference: empty tokens are rejected and one round of trailing
// punctuation (",", ".", ")", …) is stripped so "see @a.png," still works.
func attachmentPath(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if sniffName(tok) != "" {
		return tok, true
	}
	trimmed := strings.TrimRight(tok, ".,;:!?)]}'\"")
	if trimmed != tok && sniffName(trimmed) != "" {
		return trimmed, true
	}
	return "", false
}

// sniffName reports whether a filename looks like a supported image by
// extension. Final validation happens in loadImage via magic bytes.
func sniffName(p string) string {
	lower := strings.ToLower(p)
	switch {
	case strings.HasSuffix(lower, ".png"),
		strings.HasSuffix(lower, ".jpg"),
		strings.HasSuffix(lower, ".jpeg"),
		strings.HasSuffix(lower, ".gif"),
		strings.HasSuffix(lower, ".webp"):
		return "image"
	}
	return ""
}

func isPromptSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

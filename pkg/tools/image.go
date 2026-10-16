package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder for dimension checks
	_ "image/jpeg" // register JPEG decoder for dimension checks
	_ "image/png"  // register PNG decoder for dimension checks
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
		"read_file only reads UTF-8 text and refuses binary files. Paths are relative to the workspace root; absolute paths and .. traversal are rejected."
}

func (v *ViewImage) ParallelSafe() bool { return true }

type viewImageInput struct {
	Path string `json:"path" jsonschema:"required,description=Image file path relative to the workspace root"`
}

func (v *ViewImage) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[viewImageInput]()
}

func (v *ViewImage) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in viewImageInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	mt, data, w, h, err := loadImage(v.Root, in.Path)
	if err != nil {
		return nil, err
	}
	res := ImageResult(mt, base64.StdEncoding.EncodeToString(data))
	return append(TextResult(imageNote(in.Path, mt, w, h)), res...), nil
}

// PromptBlocks splits a user prompt into content blocks, turning every
// "@path" token that resolves to a supported image inside root into an
// image block — the image-attachment syntax. Prompts without attachments
// come back as exactly one text block carrying the prompt verbatim, so the
// prompt cache and existing tests see no difference.
func PromptBlocks(root, prompt string) []anthropic.BetaContentBlockParamUnion {
	if !strings.Contains(prompt, "@") {
		return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)}
	}
	var (
		blocks []anthropic.BetaContentBlockParamUnion
		last   int // start of the plain-text span not yet flushed
	)
	flush := func(end int) {
		if end > last {
			blocks = append(blocks, anthropic.NewBetaTextBlock(prompt[last:end]))
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
				flush(i)
				blocks = append(blocks,
					anthropic.NewBetaTextBlock(imageNote(path, mt, w, h)),
					anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
						MediaType: anthropic.BetaBase64ImageSourceMediaType(mt),
						Data:      base64.StdEncoding.EncodeToString(data),
					}),
				)
				last = next
			}
		}
		i = next
	}
	flush(len(prompt))
	if len(blocks) == 0 {
		return []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(prompt)}
	}
	return blocks
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

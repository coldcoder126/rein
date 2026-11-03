// Package imageproc normalizes an image once before it enters the conversation history: it detects the format, converts it
// to a format models accept, and limits its dimensions and size. The framework never calls it; the caller decides which images need it.
//
// An oversized image makes the server reject every later request in the session, not just the failing turn,
// and modifying an image already in history breaks the prompt cache, so process images when they first enter history and never change them afterwards.
package imageproc

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/coldcoder126/rein/chat"
)

var (
	// ErrUnsupportedFormat means the data cannot be recognized or decoded as an image.
	ErrUnsupportedFormat = errors.New("imageproc: unsupported image format")
	// ErrTooLarge means the image cannot be compressed within the limits.
	ErrTooLarge = errors.New("imageproc: image cannot be compressed within the limits")
)

// Inline image formats accepted by all providers; other formats (such as BMP) are converted to PNG.
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEGIF  = "image/gif"
	MIMEWebP = "image/webp"
	mimeBMP  = "image/bmp"
)

// DetectMIME detects the image format from its magic bytes, ignoring the extension and any declared MIME; it returns "" when unrecognized.
func DetectMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return MIMEJPEG
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return MIMEPNG
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return MIMEGIF
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return MIMEWebP
	case bytes.HasPrefix(data, []byte("BM")) && isBMP(data):
		return mimeBMP
	}
	return ""
}

// isBMP checks that the BMP file header and DIB header agree, so plain text starting with "BM" is not mistaken for BMP.
func isBMP(data []byte) bool {
	if len(data) < 26 {
		return false
	}
	fileSize := leUint32(data[2:])
	pixelOffset := leUint32(data[10:])
	dibSize := leUint32(data[14:])
	switch dibSize {
	case 12, 40, 52, 56, 64, 108, 124:
	default:
		return false
	}
	if pixelOffset < 14+dibSize || (fileSize != 0 && (fileSize < 26 || pixelOffset >= fileSize)) {
		return false
	}
	planesAt := 26
	if dibSize == 12 {
		planesAt = 22
	}
	if len(data) < planesAt+2 {
		return false
	}
	return uint16(data[planesAt])|uint16(data[planesAt+1])<<8 == 1
}

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Result is a normalized image.
type Result struct {
	Data     []byte
	MIMEType string
	Width    int
	Height   int
	// Notes are remarks the model should know, for example that coordinates must be rescaled after the image was shrunk.
	Notes []string
}

// Block returns the image block to put in a chat.Message.
func (r Result) Block() chat.ContentBlock {
	return chat.ContentBlock{Type: chat.ContentTypeMedia, Data: r.Data, MIMEType: r.MIMEType}
}

// Blocks returns the image block, followed by a text block when there are notes.
func (r Result) Blocks() []chat.ContentBlock {
	blocks := []chat.ContentBlock{r.Block()}
	for _, note := range r.Notes {
		blocks = append(blocks, chat.Text(note))
	}
	return blocks
}

// Fit processes an image for the model's image specs and returns content blocks ready for a message:
// the image block, plus a note when the image was converted or shrunk.
func Fit(data []byte, model chat.ModelInfo) ([]chat.ContentBlock, error) {
	result, err := Process(data, model.ImageLimits)
	if err != nil {
		return nil, err
	}
	return result.Blocks(), nil
}

// Process normalizes an image within limits. Zero fields of limits use chat.DefaultImageLimits.
//
// PNG, JPEG, GIF and WebP images within the dimension and size limits are returned unchanged;
// other formats become PNG; oversized images are scaled down proportionally and encoded as the smaller of PNG and JPEG, lowering quality and size when needed.
func Process(data []byte, limits chat.ImageLimits) (Result, error) {
	limits = limits.WithDefaults()
	mime := DetectMIME(data)
	if mime == "" {
		return Result{}, ErrUnsupportedFormat
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	orientation := 1
	if mime == MIMEJPEG {
		orientation = jpegOrientation(data)
	}
	width, height := cfg.Width, cfg.Height
	if orientation >= 5 {
		width, height = height, width
	}

	inlineFormat := mime != mimeBMP
	if inlineFormat && width <= limits.MaxWidth && height <= limits.MaxHeight &&
		base64.StdEncoding.EncodedLen(len(data)) <= limits.MaxBytes {
		return Result{Data: data, MIMEType: mime, Width: width, Height: height}, nil
	}

	src, err := decode(data, mime)
	if err != nil {
		return Result{}, err
	}
	fitW, fitH := limits.MaxWidth, limits.MaxHeight
	if orientation >= 5 {
		fitW, fitH = fitH, fitW
	}
	src = fit(src, fitW, fitH)
	src = applyOrientation(src, orientation)

	out, err := encodeWithinLimit(src, limits)
	if err != nil {
		return Result{}, err
	}
	out.Notes = notes(mime, out, width, height)
	return out, nil
}

func notes(origMIME string, out Result, origW, origH int) []string {
	var notes []string
	if !isInline(origMIME) {
		notes = append(notes, fmt.Sprintf("[Image converted from %s to %s.]", origMIME, out.MIMEType))
	}
	if out.Width != origW || out.Height != origH {
		scale := float64(origW) / float64(out.Width)
		notes = append(notes, fmt.Sprintf(
			"[Image displayed at %dx%d; original size %dx%d. Multiply coordinates by %.2f to map back to the original.]",
			out.Width, out.Height, origW, origH, scale))
	}
	return notes
}

func isInline(mime string) bool {
	return mime == MIMEPNG || mime == MIMEJPEG || mime == MIMEGIF || mime == MIMEWebP
}

func decode(data []byte, mime string) (image.Image, error) {
	if mime == MIMEGIF {
		// Animated images keep only their first frame and are no longer animated after scaling.
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil || len(g.Image) == 0 {
			return nil, fmt.Errorf("%w: gif", ErrUnsupportedFormat)
		}
		first := image.NewNRGBA(g.Image[0].Bounds())
		draw.Draw(first, first.Bounds(), g.Image[0], g.Image[0].Bounds().Min, draw.Src)
		return first, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	return img, nil
}

// fit scales an image down proportionally to fit within maxW×maxH, never up.
func fit(src image.Image, maxW, maxH int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW && h <= maxH {
		return src
	}
	scale := math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	return resize(src, max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale))))
}

func resize(src image.Image, w, h int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

// encodeWithinLimit tries, in order: the smaller of PNG and JPEG at the current size; lower JPEG quality; smaller sizes, until the result fits MaxBytes.
func encodeWithinLimit(img image.Image, limits chat.ImageLimits) (Result, error) {
	qualities := []int{limits.JPEGQuality, 70, 55, 40}
	for {
		b := img.Bounds()
		if best, ok := bestEncoding(img, qualities, limits.MaxBytes); ok {
			best.Width, best.Height = b.Dx(), b.Dy()
			return best, nil
		}
		if b.Dx() <= 1 && b.Dy() <= 1 {
			return Result{}, ErrTooLarge
		}
		img = resize(img, max(1, b.Dx()*3/4), max(1, b.Dy()*3/4))
	}
}

func bestEncoding(img image.Image, qualities []int, maxBytes int) (Result, bool) {
	var best Result
	found := false
	consider := func(data []byte, mime string) {
		if base64.StdEncoding.EncodedLen(len(data)) > maxBytes {
			return
		}
		if !found || len(data) < len(best.Data) {
			best = Result{Data: data, MIMEType: mime}
			found = true
		}
	}
	if data, err := encodePNG(img); err == nil {
		consider(data, MIMEPNG)
	}
	flat := flatten(img)
	for _, q := range qualities {
		if data, err := encodeJPEG(flat, q); err == nil {
			consider(data, MIMEJPEG)
		}
		if found {
			// If PNG or the best-quality JPEG already fits, use it; only when both exceed the limit does JPEG quality go down.
			break
		}
	}
	return best, found
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flatten composites transparency onto a white background, since JPEG has no alpha channel.
func flatten(img image.Image) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)
	return dst
}

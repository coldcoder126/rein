package imageproc

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"golang.org/x/image/bmp"

	"github.com/coldcoder126/rein/chat"
)

func solid(w, h int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// noisy returns a random noise image that is hard to compress.
func noisy(w, h int) *image.NRGBA {
	rng := rand.New(rand.NewSource(1))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	return img
}

func encodePNGBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEGBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestDetectMIME(t *testing.T) {
	img := solid(4, 4, color.White)
	var gifBuf, bmpBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := bmp.Encode(&bmpBuf, img); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", encodePNGBytes(t, img), MIMEPNG},
		{"jpeg", encodeJPEGBytes(t, img), MIMEJPEG},
		{"gif", gifBuf.Bytes(), MIMEGIF},
		{"bmp", bmpBuf.Bytes(), mimeBMP},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 8)...), MIMEWebP},
		{"text starting with BM", []byte("BM is not an image, just some text content here."), ""},
		{"empty", nil, ""},
		{"text", []byte("hello"), ""},
	}
	for _, tt := range tests {
		if got := DetectMIME(tt.data); got != tt.want {
			t.Errorf("%s: got=%q want=%q", tt.name, got, tt.want)
		}
	}
}

func TestProcessPassesThroughWithinLimits(t *testing.T) {
	data := encodePNGBytes(t, solid(100, 50, color.RGBA{R: 255, A: 255}))
	res, err := Process(data, chat.ImageLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Data, data) || res.MIMEType != MIMEPNG || res.Width != 100 || res.Height != 50 || len(res.Notes) != 0 {
		t.Fatalf("an image within limits should be returned unchanged: %+v", res)
	}
}

func TestProcessDownscalesAndNotesOriginalSize(t *testing.T) {
	data := encodePNGBytes(t, solid(1200, 600, color.RGBA{B: 255, A: 255}))
	res, err := Process(data, chat.ImageLimits{MaxWidth: 300, MaxHeight: 300})
	if err != nil {
		t.Fatal(err)
	}
	w, h := dims(t, res.Data)
	if w != 300 || h != 150 || res.Width != 300 || res.Height != 150 {
		t.Fatalf("should scale down proportionally to 300x150: got %dx%d (%+v)", w, h, res)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "300x150") || !strings.Contains(res.Notes[0], "1200x600") {
		t.Fatalf("a shrunk image should note its original size: %v", res.Notes)
	}
	blocks := res.Blocks()
	if len(blocks) != 2 || blocks[0].Type != chat.ContentTypeMedia || blocks[1].Type != chat.ContentTypeText {
		t.Fatalf("Blocks should return the image block and the note: %+v", blocks)
	}
}

func TestProcessDoesNotUpscale(t *testing.T) {
	data := encodePNGBytes(t, solid(40, 20, color.White))
	res, err := Process(data, chat.ImageLimits{MaxWidth: 4000, MaxHeight: 4000})
	if err != nil || res.Width != 40 || res.Height != 20 {
		t.Fatalf("should not scale up: %+v err=%v", res, err)
	}
}

func TestProcessConvertsBMPToPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, solid(64, 64, color.RGBA{G: 255, A: 255})); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), chat.ImageLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.MIMEType != MIMEPNG || DetectMIME(res.Data) != MIMEPNG {
		t.Fatalf("BMP should be converted to PNG: %+v", res)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "converted from image/bmp") {
		t.Fatalf("conversion should add a note: %v", res.Notes)
	}
}

func TestProcessShrinksUntilUnderByteLimit(t *testing.T) {
	data := encodePNGBytes(t, noisy(400, 400))
	limits := chat.ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 60_000}
	if base64.StdEncoding.EncodedLen(len(data)) <= limits.MaxBytes {
		t.Fatal("test precondition failed: the noise image should exceed the size limit")
	}
	res, err := Process(data, limits)
	if err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodedLen(len(res.Data)) > limits.MaxBytes {
		t.Fatalf("result still exceeds the limit: %d", len(res.Data))
	}
	if res.Width >= 400 || len(res.Notes) == 0 {
		t.Fatalf("an oversized image should be shrunk with a note: %+v", res)
	}
}

func TestProcessReportsTooLarge(t *testing.T) {
	data := encodePNGBytes(t, noisy(64, 64))
	if _, err := Process(data, chat.ImageLimits{MaxBytes: 8}); err != ErrTooLarge {
		t.Fatalf("should return ErrTooLarge when it cannot compress: %v", err)
	}
}

func TestProcessRejectsNonImage(t *testing.T) {
	if _, err := Process([]byte("not an image"), chat.ImageLimits{}); err != ErrUnsupportedFormat {
		t.Fatalf("non-images should return ErrUnsupportedFormat: %v", err)
	}
}

func TestProcessGIFResizeKeepsFirstFrameOnly(t *testing.T) {
	pal := []color.Color{color.White, color.Black}
	frame := image.NewPaletted(image.Rect(0, 0, 400, 400), pal)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), chat.ImageLimits{MaxWidth: 100, MaxHeight: 100})
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, res.Data); w != 100 || h != 100 || (res.MIMEType != MIMEPNG && res.MIMEType != MIMEJPEG) {
		t.Fatalf("a scaled GIF should become a static PNG/JPEG: %dx%d %s", w, h, res.MIMEType)
	}
}

func TestProcessFlattensTransparencyWhenChoosingJPEG(t *testing.T) {
	// Large semi-transparent noise: PNG exceeds the limit so JPEG is used, and transparent areas should become white, not black.
	img := noisy(300, 300)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0
	}
	data := encodePNGBytes(t, img)
	res, err := Process(data, chat.ImageLimits{MaxBytes: 40_000})
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/2).RGBA()
	if r>>8 < 200 || g>>8 < 200 || b>>8 < 200 {
		t.Fatalf("fully transparent pixels should become near white: %d %d %d", r>>8, g>>8, b>>8)
	}
}

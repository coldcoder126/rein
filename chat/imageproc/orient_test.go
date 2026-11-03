package imageproc

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

// withOrientation inserts an EXIF APP1 segment holding only the orientation tag right after the JPEG SOI.
func withOrientation(t *testing.T, jpegData []byte, orientation uint16) []byte {
	t.Helper()
	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(42))
	binary.Write(&tiff, binary.LittleEndian, uint32(8))
	binary.Write(&tiff, binary.LittleEndian, uint16(1))
	binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	binary.Write(&tiff, binary.LittleEndian, uint16(3))
	binary.Write(&tiff, binary.LittleEndian, uint32(1))
	binary.Write(&tiff, binary.LittleEndian, orientation)
	binary.Write(&tiff, binary.LittleEndian, uint16(0))
	binary.Write(&tiff, binary.LittleEndian, uint32(0))

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var seg bytes.Buffer
	seg.Write([]byte{0xff, 0xe1})
	binary.Write(&seg, binary.BigEndian, uint16(len(payload)+2))
	seg.Write(payload)

	out := append([]byte{}, jpegData[:2]...)
	out = append(out, seg.Bytes()...)
	return append(out, jpegData[2:]...)
}

func twoPixelJPEG(t *testing.T) []byte {
	t.Helper()
	// A 40x20 image, red on the left and green on the right.
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			if x < 20 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{G: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestJPEGOrientationParsing(t *testing.T) {
	base := twoPixelJPEG(t)
	if got := jpegOrientation(base); got != 1 {
		t.Fatalf("no EXIF should be 1: %d", got)
	}
	for _, o := range []uint16{1, 3, 6, 8} {
		if got := jpegOrientation(withOrientation(t, base, o)); got != int(o) {
			t.Fatalf("wrong orientation: got=%d want=%d", got, o)
		}
	}
}

func TestProcessAppliesExifOrientationWhenReencoding(t *testing.T) {
	data := withOrientation(t, twoPixelJPEG(t), 6) // needs a 90° clockwise rotation: width and height swap
	res, err := Process(data, chat.ImageLimits{MaxWidth: 30, MaxHeight: 30})
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() >= b.Dy() {
		t.Fatalf("should become portrait after rotation: %dx%d", b.Dx(), b.Dy())
	}
	// 90° clockwise: the former left side (red) is on top and the right side (green) at the bottom.
	top, bottom := img.At(b.Dx()/2, 1), img.At(b.Dx()/2, b.Dy()-2)
	tr, tg, _, _ := top.RGBA()
	br, bg, _, _ := bottom.RGBA()
	if !(tr>>8 > 200 && tg>>8 < 80) || !(bg>>8 > 200 && br>>8 < 80) {
		t.Fatalf("orientation 6 should put red on top and green at the bottom: top=%v bottom=%v", top, bottom)
	}
}

func TestApplyOrientationAllCases(t *testing.T) {
	// A 2x1 image [A][B]; check the result of all 8 orientations.
	a, b := color.NRGBA{R: 255, A: 255}, color.NRGBA{G: 255, A: 255}
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, a)
	src.Set(1, 0, b)
	tests := []struct {
		o        int
		w, h     int
		expected []color.NRGBA // row-major
	}{
		{1, 2, 1, []color.NRGBA{a, b}},
		{2, 2, 1, []color.NRGBA{b, a}},
		{3, 2, 1, []color.NRGBA{b, a}},
		{4, 2, 1, []color.NRGBA{a, b}},
		{5, 1, 2, []color.NRGBA{a, b}},
		{6, 1, 2, []color.NRGBA{a, b}},
		{7, 1, 2, []color.NRGBA{b, a}},
		{8, 1, 2, []color.NRGBA{b, a}},
	}
	for _, tt := range tests {
		got := applyOrientation(src, tt.o)
		gb := got.Bounds()
		if gb.Dx() != tt.w || gb.Dy() != tt.h {
			t.Fatalf("orientation %d has the wrong size: %dx%d", tt.o, gb.Dx(), gb.Dy())
		}
		i := 0
		for y := 0; y < tt.h; y++ {
			for x := 0; x < tt.w; x++ {
				if c := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA); c != tt.expected[i] {
					t.Fatalf("orientation %d pixel (%d,%d): got=%v want=%v", tt.o, x, y, c, tt.expected[i])
				}
				i++
			}
		}
	}
}

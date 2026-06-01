package main

import (
	"bytes"
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

func TestWriteDataURLRejectsFullyTransparentCanvas(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "006.png")

	status, err := writeDataURL(dest, testPNGDataURL(t, color.RGBA{}), false)
	if err == nil {
		t.Fatalf("writeDataURL returned status %q, want fully transparent error", status)
	}
	if !strings.Contains(err.Error(), "fully transparent") {
		t.Fatalf("writeDataURL error = %q, want fully transparent", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("transparent canvas wrote file, stat err = %v", statErr)
	}
}

func TestWriteDataURLAcceptsOpaqueCanvas(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "006.png")

	status, err := writeDataURL(dest, testPNGDataURL(t, color.RGBA{R: 255, G: 255, B: 255, A: 255}), false)
	if err != nil {
		t.Fatalf("writeDataURL returned error: %v", err)
	}
	if status != "captured" {
		t.Fatalf("writeDataURL status = %q, want captured", status)
	}
	if !validCanvasPNGFile(dest) {
		t.Fatal("captured opaque PNG was not considered valid")
	}
}

func TestExistingPageFileIgnoresTransparentPNG(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "006.png")
	if err := os.WriteFile(blank, testPNG(t, color.RGBA{}), 0o644); err != nil {
		t.Fatal(err)
	}

	if name, ok := existingPageFile(dir, 6); ok {
		t.Fatalf("existingPageFile returned %q for transparent PNG", name)
	}

	opaque := filepath.Join(dir, "006.png")
	if err := os.WriteFile(opaque, testPNG(t, color.RGBA{A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	if name, ok := existingPageFile(dir, 6); !ok || name != "006.png" {
		t.Fatalf("existingPageFile = %q, %v; want 006.png, true", name, ok)
	}
}

func TestCompleteMarkerValidRejectsTransparentCanvasPNG(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.webp"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	canvas := filepath.Join(dir, "006.png")
	if err := os.WriteFile(canvas, testPNG(t, color.RGBA{}), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCompletionMarker(t, dir, completionManifest{
		ChapterID: "9380672",
		PageCount: 2,
		Files: []pageFile{
			{Page: 1, File: "001.webp", Kind: "image"},
			{Page: 6, File: "006.png", Kind: "canvas"},
		},
	})

	if completeMarkerValid(dir) {
		t.Fatal("completeMarkerValid accepted a transparent canvas PNG")
	}

	if err := os.WriteFile(canvas, testPNG(t, color.RGBA{A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	if !completeMarkerValid(dir) {
		t.Fatal("completeMarkerValid rejected an opaque canvas PNG")
	}
}

func testPNGDataURL(t *testing.T, c color.RGBA) string {
	t.Helper()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG(t, c))
}

func testPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, c)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTestCompletionMarker(t *testing.T, dir string, manifest completionManifest) {
	t.Helper()

	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, completeMarker), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

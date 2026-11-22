package whatsapp

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"rsc.io/qr"
)

func TestRenderQRDrawsEveryModule(t *testing.T) {
	const text = "2@abcdefghijklmnopqrstuvwxyz0123456789,keykeykey,identity,adv"
	var buf bytes.Buffer
	if err := renderQR(&buf, text); err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		t.Fatal(err)
	}

	rows := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	width := code.Size + 2*qrQuietZone
	if want := (width + 1) / 2; len(rows) != want {
		t.Fatalf("want %d rows, got %d", want, len(rows))
	}
	for i, row := range rows {
		inner := strings.TrimSuffix(strings.TrimPrefix(row, "\x1b[30;47m"), "\x1b[0m")
		cells := []rune(inner)
		if len(cells) != width {
			t.Fatalf("row %d: want %d cells, got %d", i, width, len(cells))
		}
		for x, cell := range cells {
			mx, top, bottom := x-qrQuietZone, 2*i-qrQuietZone, 2*i-qrQuietZone+1
			wantTop := mx >= 0 && top >= 0 && mx < code.Size && top < code.Size && code.Black(mx, top)
			wantBottom := mx >= 0 && bottom >= 0 && mx < code.Size && bottom < code.Size && code.Black(mx, bottom)
			want := map[[2]bool]rune{{true, true}: '█', {true, false}: '▀', {false, true}: '▄', {false, false}: ' '}[[2]bool{wantTop, wantBottom}]
			if cell != want {
				t.Fatalf("row %d cell %d: want %q, got %q", i, x, want, cell)
			}
		}
	}
}

const qrGolden = "testdata/qr.golden"

// testdata/qr.golden was checked once by decoding it with a QR reader; it catches any change to the output.
func TestRenderQRMatchesTheDecodedGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := renderQR(&buf, "2@kaya-golden,key,identity,adv"); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(qrGolden, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(qrGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("QR output changed; decode it with a QR reader before running with UPDATE_GOLDEN=1")
	}
}

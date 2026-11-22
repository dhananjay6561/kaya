package whatsapp

import (
	"bytes"
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

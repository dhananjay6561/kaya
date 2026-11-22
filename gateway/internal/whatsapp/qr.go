package whatsapp

import (
	"fmt"
	"io"
	"strings"

	"rsc.io/qr"
)

const qrQuietZone = 2

// renderQR writes text as a QR code made of half blocks, two modules per character row. It sets the
// colours explicitly (black on white), so the code scans on dark and light terminals alike.
func renderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return fmt.Errorf("encode qr code: %w", err)
	}
	black := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	var b strings.Builder
	for y := -qrQuietZone; y < code.Size+qrQuietZone; y += 2 {
		b.WriteString("\x1b[30;47m")
		for x := -qrQuietZone; x < code.Size+qrQuietZone; x++ {
			switch top, bottom := black(x, y), black(x, y+1); {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

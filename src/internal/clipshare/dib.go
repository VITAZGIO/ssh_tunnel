package clipshare

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
)

// Картинка в буфере Windows — DIB (BITMAPINFOHEADER и точки). Между
// устройствами ходит PNG: его понимают все.

var errDIB = errors.New("картинка в буфере в неизвестном формате")

// dibToPNG — 24- и 32-битные DIB (BI_RGB, BI_BITFIELDS со стандартными
// масками). Остальное в буфере почти не встречается.
func dibToPNG(b []byte) ([]byte, error) {
	if len(b) < 40 {
		return nil, errDIB
	}
	hs := int(binary.LittleEndian.Uint32(b[0:]))
	w := int(int32(binary.LittleEndian.Uint32(b[4:])))
	h := int(int32(binary.LittleEndian.Uint32(b[8:])))
	bpp := int(binary.LittleEndian.Uint16(b[14:]))
	comp := binary.LittleEndian.Uint32(b[16:])
	if hs < 40 || w <= 0 || h == 0 || w > 30000 || (bpp != 24 && bpp != 32) || (comp != 0 && comp != 3) {
		return nil, errDIB
	}
	off := hs
	if comp == 3 && hs == 40 {
		off += 12 // маски цветов после заголовка
	}
	if clr := int(binary.LittleEndian.Uint32(b[32:])); clr > 0 && hs == 40 {
		off += clr * 4
	}
	topDown := h < 0
	if topDown {
		h = -h
	}
	stride := (w*bpp/8 + 3) &^ 3
	if off+stride*h > len(b) {
		return nil, errDIB
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	anyAlpha := false
	if bpp == 32 {
		for i := off + 3; i < off+stride*h; i += 4 {
			if b[i] != 0 {
				anyAlpha = true
				break
			}
		}
	}
	px := bpp / 8
	for y := 0; y < h; y++ {
		row := y
		if !topDown {
			row = h - 1 - y
		}
		p := b[off+row*stride:]
		for x := 0; x < w; x++ {
			q := p[x*px:]
			a := uint8(255)
			if bpp == 32 && anyAlpha {
				a = q[3]
			}
			img.SetNRGBA(x, y, color.NRGBA{R: q[2], G: q[1], B: q[0], A: a})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pngToDIB — для программ, которые не знают формата «PNG» в буфере.
func pngToDIB(p []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	r := img.Bounds()
	w, h := r.Dx(), r.Dy()
	out := make([]byte, 40+w*h*4)
	binary.LittleEndian.PutUint32(out[0:], 40)
	binary.LittleEndian.PutUint32(out[4:], uint32(w))
	binary.LittleEndian.PutUint32(out[8:], uint32(h)) // снизу вверх
	binary.LittleEndian.PutUint16(out[12:], 1)
	binary.LittleEndian.PutUint16(out[14:], 32)
	binary.LittleEndian.PutUint32(out[20:], uint32(w*h*4))
	for y := 0; y < h; y++ {
		row := out[40+(h-1-y)*w*4:]
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(r.Min.X+x, r.Min.Y+y)).(color.NRGBA)
			row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = c.B, c.G, c.R, c.A
		}
	}
	return out, nil
}

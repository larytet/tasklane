package blob

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
)

const MaxPixels = 50_000_000 // decoding a larger image is refused (image bombs)

// Thumbnail decodes an image (png, jpeg, gif) and returns a PNG no larger than max on its long side,
// using area averaging. The header is checked first so a huge image is never decoded.
func Thumbnail(r io.Reader, max int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unreadable image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return nil, fmt.Errorf("image dimensions %dx%d are not supported", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unreadable image: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if w > max || h > max {
		if w >= h {
			nw, nh = max, h*max/w
		} else {
			nw, nh = w*max/h, max
		}
		if nh < 1 {
			nh = 1
		}
		if nw < 1 {
			nw = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw == w && nh == h {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	} else {
		for y := 0; y < nh; y++ {
			y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
			if y1 <= y0 {
				y1 = y0 + 1
			}
			for x := 0; x < nw; x++ {
				x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
				if x1 <= x0 {
					x1 = x0 + 1
				}
				var rs, gs, bs, as, n uint64
				for yy := y0; yy < y1; yy++ {
					for xx := x0; xx < x1; xx++ {
						r, g, bl, a := src.At(xx, yy).RGBA()
						rs, gs, bs, as, n = rs+uint64(r), gs+uint64(g), bs+uint64(bl), as+uint64(a), n+1
					}
				}
				i := dst.PixOffset(x, y)
				dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(rs/n>>8), uint8(gs/n>>8), uint8(bs/n>>8), uint8(as/n>>8)
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

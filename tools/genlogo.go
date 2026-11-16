//go:build ignore

// Pre-size the title-bar asset so starting the app never decodes the 1024px logo.
package main

import (
	"image"
	"image/png"
	"os"
)

func main() {
	f, err := os.Open("resources/icon.png")
	if err != nil {
		panic(err)
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		panic(err)
	}
	const px = 64
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, px, px))
	for y := range px {
		for x := range px {
			var r, g, bl, a, n uint64
			for sy := b.Min.Y + y*b.Dy()/px; sy < b.Min.Y+(y+1)*b.Dy()/px; sy++ {
				for sx := b.Min.X + x*b.Dx()/px; sx < b.Min.X+(x+1)*b.Dx()/px; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			if a == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r*255/a), uint8(g*255/a), uint8(bl*255/a), uint8(a/n>>8)
		}
	}
	out, err := os.Create("resources/titlebar.png")
	if err != nil {
		panic(err)
	}
	if err := png.Encode(out, dst); err != nil {
		panic(err)
	}
	if err := out.Close(); err != nil {
		panic(err)
	}
}

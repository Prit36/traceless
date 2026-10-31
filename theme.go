package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"

	"github.com/egoist/mygo/ui"
)

// The look of Windows 11 (Fluent): its neutral colors, light and dark, on
// the accent color the user chose, with the system's fonts and icons.

// palette holds the colors the theme has no place for.
type palette struct {
	window  ui.Color // under the navigation pane, where Mica is not shown
	stroke  ui.Color // the edge of the content layer and of cards
	footer  ui.Color // a dialog's button area
	subtle  ui.Color // the fill of a hovered navigation item
	chosen  ui.Color // the fill of the chosen navigation item and table row
	smoke   ui.Color // behind a dialog
	info    ui.Color // the fills of info bars
	warning ui.Color
	errBg   ui.Color
	success ui.Color
	tag     ui.Color // the fill of a small tag
}

func fluent(c *ui.Context) (*ui.Theme, palette) {
	t := *c.Theme()
	var p palette
	if t.Dark {
		p = palette{
			window: ui.Hex("#202020"), stroke: ui.Hex("#1c1c1c"), footer: ui.Hex("#202020"),
			subtle: ui.RGBA(255, 255, 255, 0.06), chosen: ui.RGBA(255, 255, 255, 0.08), smoke: ui.RGBA(0, 0, 0, 0.5),
			info: ui.Hex("#2d2d2d"), warning: ui.Hex("#433519"), errBg: ui.Hex("#442726"), success: ui.Hex("#393d1b"),
			tag: ui.RGBA(255, 255, 255, 0.08),
		}
		t.Background = ui.Hex("#272727")
		t.Surface, t.SurfaceHover, t.SurfacePressed = ui.Hex("#2d2d2d"), ui.Hex("#323232"), ui.Hex("#292929")
		t.Border = ui.Hex("#3a3a3a")
		t.Text, t.TextMuted = ui.Hex("#ffffff"), ui.Hex("#c5c5c5")
		t.Danger, t.Warning, t.Success = ui.Hex("#ff99a4"), ui.Hex("#fce100"), ui.Hex("#6ccb5f")
		t.Scrollbar = ui.RGBA(255, 255, 255, 0.4)
		t.Inverse, t.InverseText = ui.Hex("#2c2c2c"), ui.Hex("#ffffff")
	} else {
		p = palette{
			window: ui.Hex("#f3f3f3"), stroke: ui.Hex("#e5e5e5"), footer: ui.Hex("#f3f3f3"),
			subtle: ui.RGBA(0, 0, 0, 0.04), chosen: ui.RGBA(0, 0, 0, 0.06), smoke: ui.RGBA(0, 0, 0, 0.3),
			info: ui.Hex("#f6f6f6"), warning: ui.Hex("#fff4ce"), errBg: ui.Hex("#fde7e9"), success: ui.Hex("#dff6dd"),
			tag: ui.RGBA(0, 0, 0, 0.06),
		}
		t.Background = ui.Hex("#fbfbfb")
		t.Surface, t.SurfaceHover, t.SurfacePressed = ui.Hex("#fefefe"), ui.Hex("#f6f6f6"), ui.Hex("#f0f0f0")
		t.Border = ui.Hex("#e0e0e0")
		t.Text, t.TextMuted = ui.Hex("#1b1b1b"), ui.Hex("#5f5f5f")
		t.Danger, t.Warning, t.Success = ui.Hex("#c42b1c"), ui.Hex("#9d5d00"), ui.Hex("#0f7b0f")
		t.Scrollbar = ui.RGBA(0, 0, 0, 0.45)
		t.Inverse, t.InverseText = ui.Hex("#f9f9f9"), ui.Hex("#1b1b1b")
	}
	if c.Preferences().Accent.A == 0 { // Windows' default blue
		if t.Dark {
			t.Accent, t.AccentHover, t.AccentPressed, t.AccentText = ui.Hex("#60cdff"), ui.Hex("#5ab9e6"), ui.Hex("#52a6ce"), ui.Hex("#000000")
		} else {
			t.Accent, t.AccentHover, t.AccentPressed, t.AccentText = ui.Hex("#005fb8"), ui.Hex("#1a6fc0"), ui.Hex("#327ec7"), ui.Hex("#ffffff")
		}
	}
	t.Selection = t.Accent.Alpha(0.35)
	t.Focus = t.Text
	t.Radius = 4
	t.Font = "Segoe UI Variable Text, Segoe UI"
	return &t, p
}

// rowsTheme is the theme of a table whose chosen row is tinted, as in
// Explorer, rather than filled with the accent.
func rowsTheme(t *ui.Theme, p palette) *ui.Theme {
	r := *t
	r.Accent, r.AccentText = p.chosen, t.Text
	return &r
}

// Glyphs of Segoe Fluent Icons, Windows 11's icon font, or of Segoe MDL2
// Assets, Windows 10's, which has the same code points.
const (
	iconFont = "Segoe Fluent Icons, Segoe MDL2 Assets"

	glyphAllApps   = ''
	glyphRecent    = ''
	glyphDrive     = ''
	glyphWarning   = ''
	glyphShield    = ''
	glyphDelete    = ''
	glyphBroom     = ''
	glyphOpen      = ''
	glyphRefresh   = ''
	glyphInfo      = ''
	glyphCompleted = ''
	glyphError     = ''
	glyphFolder    = ''
	glyphFile      = ''
	glyphLink      = ''
	glyphKey       = ''
)

func glyph(c *ui.Context, g rune) ui.Element {
	return ui.Text(c, string(g)).Font(iconFont).FontSize(16).SingleLine().Shrink(0).Role(ui.RoleNone)
}

//go:embed resources/icon.png
var iconPNG []byte

// logo is the app's icon at px pixels, scaled down from the 1024 of
// icon.png with a box filter, which keeps it sharp where the GPU would not.
func logo(px int) *ui.Bitmap {
	src, err := png.Decode(bytes.NewReader(iconPNG))
	if err != nil || px <= 0 {
		return nil
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, px, px))
	for y := range px {
		y0, y1 := b.Min.Y+y*b.Dy()/px, b.Min.Y+(y+1)*b.Dy()/px
		for x := range px {
			x0, x1 := b.Min.X+x*b.Dx()/px, b.Min.X+(x+1)*b.Dx()/px
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA() // premultiplied
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			if a == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r * 255 / a)
			dst.Pix[i+1] = uint8(g * 255 / a)
			dst.Pix[i+2] = uint8(bl * 255 / a)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return ui.NewBitmap(dst)
}

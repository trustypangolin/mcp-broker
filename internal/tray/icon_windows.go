package tray

import (
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// iconState picks the tray icon's look.
type iconState int

const (
	iconRunning iconState = iota
	iconPending           // approvals waiting: orange badge
	iconStopped           // broker not running: grey
)

// rgba is a straight-alpha colour.
type rgba struct{ r, g, b, a float64 }

var (
	blue   = rgba{0x2f, 0x6f, 0xde, 1}
	grey   = rgba{0x7a, 0x80, 0x8a, 1}
	white  = rgba{0xff, 0xff, 0xff, 1}
	orange = rgba{0xf0, 0x8c, 0x1a, 1}
)

// drawIcon renders the broker mark (a hub joined to three nodes, as in the dashboard's
// icon.svg) at size×size, 4×4 supersampled. Coordinates are in a 32×32 design space.
// It returns top-down BGRA rows.
func drawIcon(size int, st iconState) []byte {
	bg := blue
	if st == iconStopped {
		bg = grey
	}
	const ss = 4
	scale := 32.0 / float64(size)
	out := make([]byte, size*size*4)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var acc rgba
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					c := sample(x, y, bg, st)
					acc.r += c.r * c.a
					acc.g += c.g * c.a
					acc.b += c.b * c.a
					acc.a += c.a
				}
			}
			n := float64(ss * ss)
			i := (py*size + px) * 4
			if acc.a > 0 {
				out[i+0] = byte(acc.b / acc.a)
				out[i+1] = byte(acc.g / acc.a)
				out[i+2] = byte(acc.r / acc.a)
			}
			out[i+3] = byte(math.Round(acc.a / n * 255))
		}
	}
	return out
}

func sample(x, y float64, bg rgba, st iconState) rgba {
	if st == iconPending && math.Hypot(x-25, y-25) <= 7.5 {
		if math.Hypot(x-25, y-25) <= 5.8 {
			return orange
		}
		return white
	}
	if !inRoundRect(x, y, 1, 1, 31, 31, 7) {
		return rgba{}
	}
	hub, nodes := [2]float64{16, 16}, [][2]float64{{8, 9}, {24, 9}, {16, 25}}
	if math.Hypot(x-hub[0], y-hub[1]) <= 3.6 {
		return white
	}
	for _, n := range nodes {
		if math.Hypot(x-n[0], y-n[1]) <= 2.6 || segDist(x, y, hub, n) <= 1.2 {
			return white
		}
	}
	return bg
}

func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return math.Hypot(x-cx, y-cy) <= r
}

func segDist(x, y float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := ((x-a[0])*dx + (y-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(a[0]+t*dx), y-(a[1]+t*dy))
}

// makeIcon builds an HICON from drawIcon's pixels.
func makeIcon(size int, st iconState) (windows.Handle, error) {
	px := drawIcon(size, st)
	hdr := bitmapInfoHeader{Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32}
	hdr.Size = uint32(unsafe.Sizeof(hdr))
	var bits unsafe.Pointer
	color, _, err := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&hdr)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if color == 0 {
		return 0, err
	}
	defer pDeleteObject.Call(color)
	copy(unsafe.Slice((*byte)(bits), len(px)), px)
	// The mask is ignored for 32-bit icons with alpha, but must exist.
	mask, _, err := pCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	if mask == 0 {
		return 0, err
	}
	defer pDeleteObject.Call(mask)
	ii := iconInfo{IsIcon: 1, Mask: windows.Handle(mask), Color: windows.Handle(color)}
	h, _, err := pCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if h == 0 {
		return 0, err
	}
	return windows.Handle(h), nil
}

func smallIconSize() int {
	n, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	if n == 0 {
		return 16
	}
	return int(n)
}

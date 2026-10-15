package watchdog

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
	"sync"
)

// trayIconColors tints the brand mark's retry arc by logical icon state:
// green while recovery is armed, amber while waiting or paused, red on a
// state that needs the user. Colors follow the old shared-icon mapping
// (32516 info → green, 32515 warning → amber, 32513 error → red).
var trayIconColors = map[string]color.NRGBA{
	"running": {R: 0x2e, G: 0xa0, B: 0x43, A: 0xff},
	"active":  {R: 0x2e, G: 0xa0, B: 0x43, A: 0xff},
	"waiting": {R: 0xd2, G: 0x99, B: 0x22, A: 0xff},
	"paused":  {R: 0xd2, G: 0x99, B: 0x22, A: 0xff},
	"stopped": {R: 0xf8, G: 0x51, B: 0x49, A: 0xff},
}

// The mark is the app icon's brand shape (desktop/tools/gen-icons.mjs): a
// dark rounded tile with a retry arc whose arrowhead points clockwise. The
// geometry below is a direct port; keep the two files in sync.
var trayIconTile = color.NRGBA{R: 0x14, G: 0x16, B: 0x1b, A: 0xff}

const (
	trayTileCorner  = 0.2 // rounded-tile corner radius (unit square)
	trayArcRadius   = 0.26
	trayArcStroke   = 0.13
	trayGapStart    = 290.0 // degrees; gap sits upper-right
	trayGapEnd      = 345.0
	trayHeadLen     = 0.11
	trayHeadHalf    = 0.1
	traySupersample = 4
)

// trayInRoundRect reports whether the unit-square point lies inside the
// rounded tile.
func trayInRoundRect(x, y float64) bool {
	r := trayTileCorner
	if x < 0 || x > 1 || y < 0 || y > 1 {
		return false
	}
	cx, cy := r, r
	if x >= 0.5 {
		cx = 1 - r
	}
	if y >= 0.5 {
		cy = 1 - r
	}
	if x >= r && x <= 1-r {
		return true
	}
	if y >= r && y <= 1-r {
		return true
	}
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

// trayOnArc reports whether the unit-square point lies on the arc stroke;
// angles are degrees clockwise from +x, and the gap spans
// (trayGapStart, trayGapEnd).
func trayOnArc(x, y float64) bool {
	dx, dy := x-0.5, y-0.5
	if math.Abs(math.Hypot(dx, dy)-trayArcRadius) > trayArcStroke/2 {
		return false
	}
	a := math.Atan2(dy, dx) * 180 / math.Pi
	if a < 0 {
		a += 360
	}
	return a >= trayGapEnd || a <= trayGapStart
}

// trayHeadTriangle is the arrowhead: apex extends from the arc end (at
// trayGapStart) along the clockwise tangent into the gap.
var trayHeadTriangle = func() [3][2]float64 {
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	e := [2]float64{0.5 + trayArcRadius*math.Cos(rad(trayGapStart)), 0.5 + trayArcRadius*math.Sin(rad(trayGapStart))}
	tan := [2]float64{math.Cos(rad(trayGapStart + 90)), math.Sin(rad(trayGapStart + 90))}
	nrm := [2]float64{-tan[1], tan[0]}
	return [3][2]float64{
		{e[0] + tan[0]*trayHeadLen, e[1] + tan[1]*trayHeadLen},   // apex
		{e[0] + nrm[0]*trayHeadHalf, e[1] + nrm[1]*trayHeadHalf}, // base 1
		{e[0] - nrm[0]*trayHeadHalf, e[1] - nrm[1]*trayHeadHalf}, // base 2
	}
}()

// trayInTriangle reports whether the unit-square point lies inside the
// arrowhead triangle (barycentric sign test).
func trayInTriangle(x, y float64) bool {
	a, b, c := trayHeadTriangle[0], trayHeadTriangle[1], trayHeadTriangle[2]
	s := func(p, q [2]float64) float64 {
		return (x-q[0])*(p[1]-q[1]) - (p[0]-q[0])*(y-q[1])
	}
	d1, d2, d3 := s(a, b), s(b, c), s(c, a)
	return !((d1 < 0 || d2 < 0 || d3 < 0) && (d1 > 0 || d2 > 0 || d3 > 0))
}

var trayIconCache sync.Map // state -> []byte, encoded for the current OS

// trayIconImage returns the encoded tray icon for a state. Windows'
// LoadImage requires .ico container bytes; the unix and darwin systray
// backends decode PNG.
func trayIconImage(state string) []byte {
	if cached, ok := trayIconCache.Load(state); ok {
		return cached.([]byte)
	}
	pngBytes := trayIconPNG(state)
	encoded := pngBytes
	if runtime.GOOS == "windows" {
		encoded = wrapIconPNG(pngBytes)
	}
	actual, _ := trayIconCache.LoadOrStore(state, encoded)
	return actual.([]byte)
}

// trayIconPNG renders the brand mark as a 32x32 PNG: dark rounded tile with
// the retry arc tinted by state. Windows scales it to the notification-area
// size; it is also the icon payload beeep accepts. Supersampled 4x for
// anti-aliasing, mirroring gen-icons.mjs.
func trayIconPNG(state string) []byte {
	mark, ok := trayIconColors[state]
	if !ok {
		mark = trayIconColors["running"]
	}
	const size = 32
	const ss = traySupersample
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var tileHits, markHits int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(x) + (float64(sx)+0.5)/ss) / size
					v := (float64(y) + (float64(sy)+0.5)/ss) / size
					if trayInRoundRect(u, v) {
						tileHits++
					}
					if trayOnArc(u, v) || trayInTriangle(u, v) {
						markHits++
					}
				}
			}
			// mark over tile over transparent
			ta := float64(tileHits) / (ss * ss)
			ma := float64(markHits) / (ss * ss)
			a := ma + ta*(1-ma)
			if a <= 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round((float64(mark.R)*ma + float64(trayIconTile.R)*ta*(1-ma)) / a)),
				G: uint8(math.Round((float64(mark.G)*ma + float64(trayIconTile.G)*ta*(1-ma)) / a)),
				B: uint8(math.Round((float64(mark.B)*ma + float64(trayIconTile.B)*ta*(1-ma)) / a)),
				A: uint8(math.Round(a * 255)),
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// wrapIconPNG packages PNG bytes as a single-image .ico file. PNG-compressed
// icon images are valid since Windows Vista, including through LoadImage.
func wrapIconPNG(payload []byte) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))  // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // image count
	buf.WriteByte(32)                                       // width px (0 means 256)
	buf.WriteByte(32)                                       // height px
	buf.WriteByte(0)                                        // palette colors
	buf.WriteByte(0)                                        // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // color planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bits per pixel
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(payload)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22)) // header+entry size
	buf.Write(payload)
	return buf.Bytes()
}

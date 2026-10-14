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

// trayIconColors maps the logical icon state onto a 16px-legible status dot:
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

// trayIconPNG renders the state dot as a 32x32 PNG. Windows scales it to the
// notification-area size; it is also the icon payload beeep accepts.
func trayIconPNG(state string) []byte {
	fill, ok := trayIconColors[state]
	if !ok {
		fill = trayIconColors["running"]
	}
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	const center = 16.0
	const radius = 13.5
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			distance := math.Hypot(float64(x)+0.5-center, float64(y)+0.5-center)
			coverage := math.Max(0, math.Min(1, radius+0.5-distance))
			if coverage <= 0 {
				continue
			}
			pixel := fill
			pixel.A = uint8(math.Round(float64(fill.A) * coverage))
			img.SetNRGBA(x, y, pixel)
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

package main

// Status screen for routers with a small SPI TFT (MF800 pocket MiFi: GC9107 128x128, see deploy/mf800-lcd).
// It runs inside router-webui and reuses its samplers (rates, clients, modem cache, thermal guard), so the
// screen adds no extra process and no extra modem polling. Off unless the device tree has the panel.
//
// Board quirks handled here:
//   - Something re-muxing the LCD's SPI pins as plain GPIOs makes every SPI transfer time out (-110). This was
//     blamed on the modem firmware, but on 2026-11-26 it turned out to be the stock image's lcd_ctl/lcd_splash
//     (they remux the pins through /dev/mem; pwrkey-lcd.service ran lcd_ctl on every key press), which
//     deploy/mf800-lcd/install.sh now disables or wraps. As a safety net the pins are still checked every second
//     (debugfs) and put back with pinctrl's pinmux-select, then the panel is re-initialised.
//   - The panel receives RGB565 with the two bytes swapped (spi_qup 16-bit words), so pixels are
//     pre-swapped before the kernel's XRGB8888 -> RGB565 conversion.
//
// Five dashboards (clock + status, mobile network, addresses + power, system, Wi-Fi join QR code) with Font Awesome icons and
// anti-aliased Noto Sans text. A short press of the power key (or the other button) switches dashboards; a
// press while the screen is off only wakes it. It sleeps after -lcd-sleep. Holding the power key 3 s (with an
// on-screen countdown) powers the device off.

import (
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	flagLCD      = flag.String("lcd", "auto", "status screen: auto (on when the device tree has the MF800 panel), off")
	flagLCDSleep = flag.Duration("lcd-sleep", 15*time.Second, "turn the screen off after this long without a button press (0 = never)")
	flagLCDSwap  = flag.Bool("lcd-swap", true, "pre-swap RGB565 bytes (the MF800 panel receives them swapped)")
)

const (
	lcdDTNode    = "/proc/device-tree/soc@0/spi@78b9000/display@0/compatible"
	lcdCompat    = "fy,mf800-lcd"
	lcdSPIDev    = "78b9000.spi"
	lcdSPIDriver = "/sys/bus/platform/drivers/spi_qup"
	lcdBacklight = "/sys/class/leds/mf800:lcd/brightness"
	lcdModemRP   = "4080000.remoteproc"
)

func lcdEnabled() bool {
	if *flagLCD == "off" {
		return false
	}
	b, err := os.ReadFile(lcdDTNode)
	return err == nil && strings.Contains(string(b), lcdCompat)
}

// ---------------------------------------------------------------- hardware

// lcdPinsOK reports whether the SPI5 clock/data pins are still muxed to the SPI controller (function 1).
// unknown=true when debugfs can't be read.
func lcdPinsOK() (ok, unknown bool) {
	b, err := os.ReadFile("/sys/kernel/debug/gpio")
	if err != nil {
		return true, true
	}
	return lcdPinsOKText(string(b))
}

func lcdPinsOKText(txt string) (ok, unknown bool) {
	seen := 0
	for _, l := range strings.Split(txt, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 || (f[0] != "gpio16" && f[0] != "gpio19") {
			continue
		}
		seen++
		if !strings.Contains(l, " func1 ") {
			return false, false
		}
	}
	return true, seen < 2
}

func modemRunning() bool {
	ds, _ := filepath.Glob("/sys/class/remoteproc/remoteproc*")
	for _, d := range ds {
		if readTrim(d+"/name") == lcdModemRP {
			return readTrim(d+"/state") == "running"
		}
	}
	return false
}

func writeSys(path, val string) error { return os.WriteFile(path, []byte(val), 0o644) }

// lcdAttach (re)binds the SPI controller so its pinmux is applied again, then loads the panel driver.
func lcdAttach() error {
	run(10*time.Second, "modprobe", "-r", "panel-mipi-dbi")
	if _, err := os.Stat(filepath.Join(lcdSPIDriver, lcdSPIDev)); err == nil {
		if err := writeSys(lcdSPIDriver+"/unbind", lcdSPIDev); err != nil {
			return fmt.Errorf("unbind spi: %w", err)
		}
	}
	if err := writeSys(lcdSPIDriver+"/bind", lcdSPIDev); err != nil {
		return fmt.Errorf("bind spi: %w", err)
	}
	// runtime suspend/resume of the QUP also breaks transfers on this kernel (udev rule does the same at boot)
	writeSys("/sys/bus/platform/devices/"+lcdSPIDev+"/power/control", "on")
	if out, err := run(15*time.Second, "modprobe", "panel-mipi-dbi"); err != nil {
		return fmt.Errorf("modprobe panel-mipi-dbi: %v %s", err, out)
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat("/dev/fb0"); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// keep the kernel console off the panel
	vts, _ := filepath.Glob("/sys/class/vtconsole/vtcon*")
	for _, v := range vts {
		if strings.Contains(readTrim(v+"/name"), "frame buffer") {
			writeSys(v+"/bind", "0")
		}
	}
	return nil
}

const lcdPinmuxSelect = "/sys/kernel/debug/pinctrl/1000000.pinctrl/pinmux-select"

// lcdRepair puts the SPI pins back on the QUP (after something re-muxed them as GPIOs; see the top of this file)
// and re-runs the panel init via an fbdev blank/unblank, in case the reset line was touched too. Only the TLMM function bits change: microseconds, no driver reload.
func lcdRepair() error {
	for _, g := range []string{"gpio16", "gpio17", "gpio19"} {
		if err := writeSys(lcdPinmuxSelect, g+" blsp_spi5"); err != nil {
			return fmt.Errorf("pinmux-select %s: %w", g, err)
		}
	}
	writeSys("/sys/bus/platform/devices/"+lcdSPIDev+"/power/control", "on")
	writeSys("/sys/class/graphics/fb0/blank", "4") // powerdown: pipe disable
	time.Sleep(50 * time.Millisecond)
	return writeSys("/sys/class/graphics/fb0/blank", "0") // pipe enable: hardware reset + init sequence
}

type lcdFB struct {
	f      *os.File
	w, h   int
	stride int
	last   []byte
}

func openFB() (*lcdFB, error) {
	f, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fb := &lcdFB{f: f}
	if w, h, ok := strings.Cut(readTrim("/sys/class/graphics/fb0/virtual_size"), ","); ok {
		fb.w, _ = strconv.Atoi(w)
		fb.h, _ = strconv.Atoi(h)
	}
	fb.stride, _ = strconv.Atoi(readTrim("/sys/class/graphics/fb0/stride"))
	if bpp := readTrim("/sys/class/graphics/fb0/bits_per_pixel"); bpp != "32" || fb.w == 0 || fb.h == 0 || fb.stride < fb.w*4 {
		f.Close()
		return nil, fmt.Errorf("unsupported framebuffer %dx%d stride %d bpp %s", fb.w, fb.h, fb.stride, bpp)
	}
	return fb, nil
}

// lcdPixel converts the wanted colour to the XRGB8888 value that reaches the panel as that colour.
func lcdPixel(c color.RGBA, swap bool) [4]byte {
	if !swap {
		return [4]byte{c.B, c.G, c.R, 0}
	}
	v := uint16(c.R>>3)<<11 | uint16(c.G>>2)<<5 | uint16(c.B>>3) // what the panel should get
	s := v<<8 | v>>8                                             // what the kernel must produce
	return [4]byte{byte(s&31) << 3, byte((s>>5)&63) << 2, byte(s>>11) << 3, 0}
}

// show writes the image if it changed (each full frame is ~33 KB of SPI traffic).
func (fb *lcdFB) show(img *image.RGBA, swap bool) error {
	buf := make([]byte, fb.stride*fb.h)
	for y := 0; y < fb.h && y < img.Rect.Dy(); y++ {
		for x := 0; x < fb.w && x < img.Rect.Dx(); x++ {
			p := lcdPixel(img.RGBAAt(x, y), swap)
			copy(buf[y*fb.stride+x*4:], p[:])
		}
	}
	if fb.last != nil && string(buf) == string(fb.last) {
		return nil
	}
	if _, err := fb.f.WriteAt(buf, 0); err != nil {
		return err
	}
	fb.last = buf
	return nil
}

func (fb *lcdFB) close() {
	if fb != nil && fb.f != nil {
		fb.f.Close()
	}
}

func lcdBacklightOn(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	writeSys(lcdBacklight, v)
}

// ---------------------------------------------------------------- buttons

type lcdKey struct {
	power bool // the PMIC power key (as opposed to the GPIO button)
	down  bool // pressed (true) or released (false); autorepeat is dropped
}

// lcdButtons reports presses and releases of the power key and the GPIO buttons.
func lcdButtons(ch chan<- lcdKey) {
	var mu sync.Mutex
	devs := map[string]bool{}
	for {
		ents, _ := filepath.Glob("/sys/class/input/event*")
		for _, e := range ents {
			name := readTrim(e + "/device/name")
			dev := "/dev/input/" + filepath.Base(e)
			mu.Lock()
			skip := devs[dev] || (name != "pm8941_pwrkey" && name != "GPIO Buttons")
			devs[dev] = true
			mu.Unlock()
			if skip {
				continue
			}
			power := name == "pm8941_pwrkey"
			go func() {
				defer func() { mu.Lock(); devs[dev] = false; mu.Unlock() }()
				f, err := os.Open(dev)
				if err != nil {
					log.Printf("lcd: %s: %v", dev, err)
					return
				}
				defer f.Close()
				ev := make([]byte, 24) // struct input_event on arm64: timeval(16) type(2) code(2) value(4)
				for {
					if _, err := ioReadFull(f, ev); err != nil {
						return
					}
					typ := binary.LittleEndian.Uint16(ev[16:])
					val := int32(binary.LittleEndian.Uint32(ev[20:]))
					if typ == 1 && (val == 0 || val == 1) { // EV_KEY release / press
						ch <- lcdKey{power: power, down: val == 1}
					}
				}
			}()
		}
		time.Sleep(30 * time.Second)
	}
}

func ioReadFull(f *os.File, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := f.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- data

type lcdData struct {
	Operator, Tech, Uplink, State, Registration string
	Present                                     bool
	Signal                                      int     // 0-100, -1 unknown
	RSRP, RSRQ, SNR                             float64 // LTE signal; NaN unknown
	BattPct, BattMV                             int     // BattPct -1: no battery
	Charging, OnUSB                             bool
	TempC                                       float64 // hottest sensor
	Throttled, Cooldown                         bool
	DownBps, UpBps                              float64
	LANIP, WANIP                                string
	Clients                                     int // -1: not collected
	CPUPct                                      float64
	MemUsed, MemTotal, DiskUsed, DiskTotal      uint64
	CPUMHz, CPUMaxMHz, CPUThermalMHz            int
	Wifi                                        lcdWifiState
	AQI                                         lcdAQIState
	Cell                                        lcdCellState
	Now                                         time.Time
}

// lcdCollect gathers what the screens show for page: the client count (runs iw) and the Wi-Fi QR settings
// (runs nmcli, cached) only for the pages that show them.
func lcdCollect(page int) lcdData {
	withClients := page == 2
	d := lcdData{Signal: -1, BattPct: -1, Clients: -1, Now: time.Now(), RSRP: math.NaN(), RSRQ: math.NaN(), SNR: math.NaN()}
	m := modemSummary()
	if p, _ := m["present"].(bool); p {
		d.Present = true
		d.Operator, _ = m["operator"].(string)
		d.Tech, _ = m["tech"].(string)
		d.State, _ = m["state"].(string)
		d.Registration, _ = m["registration"].(string)
		if s, err := strconv.Atoi(fmt.Sprint(m["signal"])); err == nil {
			d.Signal = s
		}
		for _, x := range []struct {
			k string
			p *float64
		}{{"rsrp", &d.RSRP}, {"rsrq", &d.RSRQ}, {"snr", &d.SNR}} {
			if v, err := strconv.ParseFloat(str(m[x.k]), 64); err == nil {
				*x.p = v
			}
		}
	}
	wantModem()                // signal bars / operator on every page: keep the modem details fresh while the screen is on
	if page == lcdSignalPage { // placement: the modem measures every 2 s while this screen is on
		wantFastSignal()
	}
	if page == 1 { // the mobile network page shows LTE rates
		wantLTERates()
	}
	d.Uplink = readWANState()["active"]
	if d.Uplink != "" && d.Uplink != "none" {
		r := currentRate(d.Uplink)
		d.DownBps, d.UpBps = r.RX, r.TX
		d.WANIP = ifaceIPv4(d.Uplink)
	}
	d.LANIP = ifaceIPv4(lanIf)
	if b := batteryInfo(); b != nil {
		if v, ok := b["percent"].(int); ok {
			d.BattPct = v
		}
		d.BattMV, _ = b["voltage_mv"].(int)
		d.Charging, _ = b["charging"].(bool)
		d.OnUSB, _ = b["external_power"].(bool)
	}
	d.TempC = maxThermalC()
	tg := thermalSnapshot()
	if l, ok := tg["level"].(int); ok && l > 0 {
		d.Throttled = true
	}
	d.Cooldown, _ = tg["cooldown"].(bool)
	sampMu.Lock()
	d.CPUPct = cpuPct
	sampMu.Unlock()
	if mi := memInfo(); mi["MemTotal"] > 0 {
		d.MemTotal, d.MemUsed = mi["MemTotal"], mi["MemTotal"]-mi["MemAvailable"]
	}
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		d.DiskTotal = st.Blocks * uint64(st.Bsize)
		d.DiskUsed = d.DiskTotal - st.Bavail*uint64(st.Bsize)
	}
	if f := cpuFreq(); f != nil {
		d.CPUMHz, _ = f["cur_mhz"].(int)
		d.CPUMaxMHz, _ = f["hw_max_mhz"].(int)
		d.CPUThermalMHz, _ = f["thermal_mhz"].(int)
	}
	if page == 4 {
		d.Wifi = lcdWifiQR()
	}
	if page == lcdAQIPage { // fetched only while this screen shows
		d.AQI = lcdAQI()
	}
	if page == lcdCellPage { // mmcli cell info and the tower lookup only while this screen shows
		d.Cell = cellTower()
	}
	if withClients {
		d.Clients = 0
		for _, c := range listClients() {
			if c.Wifi {
				d.Clients++
			}
		}
	}
	return d
}

func ifaceIPv4(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// ---------------------------------------------------------------- drawing

// Light theme to match the white case, on cream rather than white (easier on the eyes). Every text/background pair
// meets WCAG AA (4.5:1) after RGB565 rounding; status colours on cream reach >= 4.2:1.
var (
	colBG       = color.RGBA{246, 238, 218, 255} // cream
	colTile     = color.RGBA{235, 224, 198, 255}
	colLine     = color.RGBA{212, 198, 168, 255} // tracks, unlit bars
	colFG       = color.RGBA{40, 34, 28, 255}
	colDim      = color.RGBA{105, 92, 75, 255}
	colWhite    = color.RGBA{255, 250, 240, 255} // warm white for text on cards
	colWhiteDim = color.RGBA{225, 222, 255, 255}
	colBlue     = color.RGBA{25, 95, 200, 255}
	colIndigo   = color.RGBA{70, 60, 200, 255}
	colPurple   = color.RGBA{120, 60, 200, 255}
	colTeal     = color.RGBA{0, 118, 110, 255}
	colOrange   = color.RGBA{190, 78, 5, 255}
	colLime     = color.RGBA{100, 150, 20, 255}
	colGreen    = color.RGBA{20, 125, 60, 255}
	colGood     = colGreen
	colWarn     = color.RGBA{160, 98, 0, 255}
	colBad      = color.RGBA{195, 35, 45, 255}
)

const (
	lcdPages      = 8
	lcdAQIPage    = 5
	lcdCellPage   = 6
	lcdSignalPage = 7
)

// Anti-aliased glyphs and icons (lcdassets.go, generated by deploy/mf800-lcd/mkassets.py).
type sglyph struct {
	adv, x, w int
	a         []byte // w x font height, row-major alpha
}

type smoothFont struct {
	H, Ascent int
	g         [95]sglyph
}

type alphaIcon struct {
	W, H int
	A    []byte
}

func mustSmoothFont(h, asc int, b64 string) *smoothFont {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	f := &smoothFont{H: h, Ascent: asc}
	for i := range f.g {
		adv, x, w := int(b[0]), int(int8(b[1])), int(b[2])
		b = b[3:]
		f.g[i] = sglyph{adv: adv, x: x, w: w, a: b[:w*h]}
		b = b[w*h:]
	}
	return f
}

func mustAlphaIcon(w, h int, b64 string) alphaIcon {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != w*h {
		panic(fmt.Sprintf("icon %dx%d: %v (%d bytes)", w, h, err, len(b)))
	}
	return alphaIcon{W: w, H: h, A: b}
}

func (f *smoothFont) width(s string) int {
	n := 0
	for _, r := range s {
		if r >= 32 && r <= 126 {
			n += f.g[r-32].adv
		}
	}
	return n
}

type canvas struct{ *image.RGBA }

func newCanvas(w, h int) canvas {
	c := canvas{image.NewRGBA(image.Rect(0, 0, w, h))}
	c.fill(0, 0, w, h, colBG)
	return c
}

func (c canvas) fill(x, y, w, h int, col color.RGBA) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			c.SetRGBA(i, j, col)
		}
	}
}

// blend mixes col into the pixel at (x, y) with coverage a (0-255).
func (c canvas) blend(x, y int, col color.RGBA, a byte) {
	if a == 0 || !(image.Point{x, y}.In(c.Rect)) {
		return
	}
	if a == 255 {
		c.SetRGBA(x, y, col)
		return
	}
	d := c.RGBAAt(x, y)
	mix := func(s, t uint8) uint8 { return uint8((int(s)*int(a) + int(t)*(255-int(a))) / 255) }
	c.SetRGBA(x, y, color.RGBA{mix(col.R, d.R), mix(col.G, d.G), mix(col.B, d.B), 255})
}

// roundRect: filled rectangle with 3 px rounded corners.
func (c canvas) roundRect(x, y, w, h int, col color.RGBA) {
	c.fill(x+3, y, w-6, h, col)
	c.fill(x, y+3, w, h-6, col)
	for _, o := range [][2]int{{1, 1}, {2, 0}, {0, 2}, {1, 2}, {2, 1}, {2, 2}} {
		for _, p := range [][2]int{{x + o[0], y + o[1]}, {x + w - 1 - o[0], y + o[1]}, {x + o[0], y + h - 1 - o[1]}, {x + w - 1 - o[0], y + h - 1 - o[1]}} {
			c.SetRGBA(p[0], p[1], col)
		}
	}
}

// pill: thin bar with 1 px rounded ends.
func (c canvas) pill(x, y, w, h int, col color.RGBA) {
	c.fill(x+1, y, w-2, h, col)
	c.fill(x, y+1, 1, h-2, col)
	c.fill(x+w-1, y+1, 1, h-2, col)
}

// text draws s with its baseline at y, starting at x; returns the end x.
func (c canvas) text(f *smoothFont, x, y int, s string, col color.RGBA) int {
	top := y - f.Ascent
	for _, r := range s {
		if r < 32 || r > 126 {
			r = '?'
		}
		g := f.g[r-32]
		for j := 0; j < f.H; j++ {
			for i := 0; i < g.w; i++ {
				c.blend(x+g.x+i, top+j, col, g.a[j*g.w+i])
			}
		}
		x += g.adv
	}
	return x
}

func (c canvas) textC(f *smoothFont, cx, y int, s string, col color.RGBA) {
	c.text(f, cx-f.width(s)/2, y, s, col)
}

func (c canvas) textR(f *smoothFont, x, y int, s string, col color.RGBA) {
	c.text(f, x-f.width(s), y, s, col)
}

// fitText picks the first font in which s fits into w pixels (the last one otherwise).
func fitText(s string, w int, fonts ...*smoothFont) *smoothFont {
	for _, f := range fonts {
		if f.width(s) <= w {
			return f
		}
	}
	return fonts[len(fonts)-1]
}

// icon draws a Font Awesome icon ("name/size") with its top-left at (x, y); returns its width.
func (c canvas) icon(name string, x, y int, col color.RGBA) int {
	ic, ok := lcdIcons[name]
	if !ok {
		return 0
	}
	for j := 0; j < ic.H; j++ {
		for i := 0; i < ic.W; i++ {
			c.blend(x+i, y+j, col, ic.A[j*ic.W+i])
		}
	}
	return ic.W
}

// tempText draws "69°" (or "69°C") with the baseline at y; the degree ring isn't in the ASCII font tables.
func (c canvas) tempText(f *smoothFont, x, y int, t float64, col color.RGBA, unit bool) int {
	x = c.text(f, x, y, fmt.Sprintf("%.0f", t), col)
	r := max(2, f.Ascent/8) // ring radius, top-aligned with the digits' cap height
	cy := y - f.Ascent*7/10 + r
	cx := x + r + 1
	for j := -r - 1; j <= r+1; j++ {
		for i := -r - 1; i <= r+1; i++ {
			d := math.Hypot(float64(i), float64(j))
			if cov := 1 - math.Abs(d-float64(r))/1.1; cov > 0 {
				c.blend(cx+i, cy+j, col, byte(255*math.Min(cov, 1)))
			}
		}
	}
	x = cx + r + 2
	if unit {
		x = c.text(f, x, y, "C", col)
	}
	return x
}

func shortTech(t string) string {
	t = strings.ToLower(t)
	switch {
	case strings.Contains(t, "lte"):
		return "4G"
	case strings.Contains(t, "umts"), strings.Contains(t, "hspa"):
		return "3G"
	case strings.Contains(t, "gsm"), strings.Contains(t, "edge"), strings.Contains(t, "gprs"):
		return "2G"
	}
	return ""
}

// netStatus: short network / internet state and its colour.
func netStatus(d lcdData) (string, color.RGBA) {
	tech := firstNonEmpty(shortTech(d.Tech), "LTE")
	switch {
	case d.Uplink == "usb0":
		return "Online (USB)", colGood
	case d.Uplink == btIf:
		return "Online (phone)", colWarn
	case !d.Present:
		return "No modem", colBad
	case d.State == "failed" || d.State == "locked":
		return "SIM error", colBad
	case d.Cooldown:
		return "Off (too hot)", colBad
	case d.Registration == "home" || d.Registration == "roaming":
		if !strings.HasPrefix(d.Uplink, "wwan") {
			return tech + " no data", colWarn // registered, but the uplink check fails
		}
		if d.Registration == "roaming" {
			return tech + " roaming", colGood
		}
		return tech + " online", colGood
	case d.Registration == "searching":
		return "Searching...", colWarn
	case d.Registration == "denied":
		return "Denied", colBad
	}
	return "No service", colBad
}

func tempColor(d lcdData) color.RGBA {
	switch {
	case d.Cooldown || d.TempC >= 85:
		return colBad
	case d.Throttled || d.TempC >= 75:
		return colWarn
	}
	return colTeal
}

func battColor(d lcdData) color.RGBA {
	switch {
	case d.BattPct < 0:
		return colDim
	case d.BattPct <= 15:
		return colBad
	case d.BattPct <= 35:
		return colWarn
	}
	return colGood
}

func fmtRate(bps float64) string {
	b := bps * 8
	switch {
	case b >= 1e6:
		return fmt.Sprintf("%.1fM", b/1e6)
	case b >= 1e3:
		return fmt.Sprintf("%.0fk", b/1e3)
	}
	return fmt.Sprintf("%.0f", b)
}

// signalBars: n rising bars in a w x h box.
func (c canvas) signalBars(x, y, w, h int, signal int, col color.RGBA) {
	const n = 5
	gap := max(1, w/(n*4))
	bw := (w - gap*(n-1)) / n
	lit := 0
	if signal >= 0 {
		lit = (signal + 19) / 20
	}
	for i := 0; i < n; i++ {
		bh := h * (i + 1) / n
		bc := colLine
		if i < lit {
			bc = col
		}
		c.fill(x+i*(bw+gap), y+h-bh, bw, bh, bc)
	}
}

// battery: outline w x h with a terminal nub, filled by charge; bolt when charging.
func (c canvas) battery(x, y, w, h int, d lcdData, bolt string) {
	c.roundRect(x, y, w, h, colFG)
	c.roundRect(x+1, y+1, w-2, h-2, colBG)
	c.fill(x+w, y+h/3, 2, h-2*(h/3), colFG)
	if d.BattPct >= 0 {
		c.fill(x+2, y+2, max(1, (w-4)*d.BattPct/100), h-4, battColor(d))
	}
	if d.Charging {
		ic := lcdIcons[bolt]
		c.icon(bolt, x+(w-ic.W)/2, y+(h-ic.H)/2, colFG)
	}
}

func (c canvas) pageDots(page, w, h int) {
	x0 := w/2 - (lcdPages*8)/2
	for i := 0; i < lcdPages; i++ {
		col := colLine
		if i == page {
			col = colBlue
		}
		c.pill(x0+i*8+1, h-4, 6, 3, col)
	}
}

// card: a solid coloured tile with an icon and text in white.
func (c canvas) card(x, y, w, h int, col color.RGBA) { c.roundRect(x, y, w, h, col) }

// badge: icon in white on a small coloured rounded square.
func (c canvas) badge(x, y, size int, icon string, col color.RGBA) {
	c.roundRect(x, y, size, size, col)
	ic := lcdIcons[icon]
	c.icon(icon, x+(size-ic.W)/2, y+(size-ic.H)/2, colWhite)
}

// dot: small filled circle.
func (c canvas) dot(cx, cy, r int, col color.RGBA) {
	for j := -r - 1; j <= r+1; j++ {
		for i := -r - 1; i <= r+1; i++ {
			if cov := float64(r) + 0.5 - math.Hypot(float64(i), float64(j)); cov > 0 {
				c.blend(cx+i, cy+j, col, byte(255*math.Min(cov, 1)))
			}
		}
	}
}

// topBar: tower + signal bars + technology on the left, battery and percent on the right.
func (c canvas) topBar(d lcdData, w int) {
	_, sc := netStatus(d)
	x := 2 + c.icon("tower-cell/12", 2, 2, sc)
	c.signalBars(x+3, 3, 15, 10, d.Signal, colIndigo)
	if t := shortTech(d.Tech); t != "" {
		c.text(fontSmall, x+21, 13, t, colDim)
	}
	if d.BattPct >= 0 {
		c.battery(w-24, 3, 20, 10, d, "bolt/10")
		c.textR(fontSmall, w-28, 13, strconv.Itoa(d.BattPct)+"%", colFG)
	}
}

// lcdRender draws dashboard page 0 (clock), 1 (mobile network), 2 (addresses and power), 3 (system), 4 (Wi-Fi QR),
// 5 (New Delhi air quality), 6 (cell tower) or 7 (LTE signal).
func lcdRender(d lcdData, page, w, h int) *image.RGBA {
	c := newCanvas(w, h)
	now := d.Now
	if now.IsZero() {
		now = time.Now()
	}
	switch page {
	case 0: // clock + status tiles
		c.topBar(d, w)
		c.textC(fontClock, w/2, 44, now.Format("15:04"), colFG)
		c.textC(fontSmall, w/2, 59, now.Format("Mon, 02 Jan"), colDim)
		// tiles: internet status | temperature, filled with their state colour
		st, sc := netStatus(d)
		c.card(2, 68, 61, 52, sc)
		c.icon("globe/18", 4+(57-18)/2, 76, colWhite)
		word := "Online"
		switch {
		case sc == colWarn:
			word = "No data"
		case sc == colBad:
			word = "Offline"
		}
		if strings.HasPrefix(st, "Searching") {
			word = "Search"
		}
		c.textC(fontSmall, 2+61/2, 110, word, colWhite)
		c.card(65, 68, 61, 52, tempColor(d))
		x := 65 + (61-13-4-fontBig.width(fmt.Sprintf("%.0f", d.TempC))-6)/2
		x += c.icon("temperature-half/20", x, 84, colWhite) + 4
		c.tempText(fontBig, x, 102, d.TempC, colWhite, false)
	case 1: // mobile network
		st, sc := netStatus(d)
		c.card(2, 2, w-4, 40, colIndigo)
		c.icon("tower-cell/26", 6, 9, colWhite)
		op := firstNonEmpty(d.Operator, "No operator")
		f := fitText(op, w-44, fontBig, fontMed, fontSmall)
		c.text(f, 40, 21, op, colWhite)
		c.dot(43, 32, 3, sc)
		c.text(fontSmall, 49, 36, st, colWhiteDim)
		// signal meter: each bar its own colour, red to green
		c.rainbowBars(8, 48, 76, 32, d.Signal)
		if d.Signal >= 0 {
			c.text(fontBig, 90, 76, strconv.Itoa(d.Signal), colFG)
			c.text(fontSmall, 90+fontBig.width(strconv.Itoa(d.Signal))+1, 76, "%", colDim)
		}
		if t := shortTech(d.Tech); t != "" {
			c.roundRect(90, 46, 32, 15, colPurple)
			c.textC(fontSmall, 106, 57, t, colWhite)
		}
		// live rates
		c.card(2, 88, 61, 32, colGreen)
		c.icon("arrow-down/14", 7, 97, colWhite)
		c.text(fontMed, 21, 109, fmtRate(d.DownBps), colWhite)
		c.card(65, 88, 61, 32, colBlue)
		c.icon("arrow-up/14", 70, 97, colWhite)
		c.text(fontMed, 84, 109, fmtRate(d.UpBps), colWhite)
	case 2: // addresses + power
		row := func(y int, icon, val string, col color.RGBA) {
			c.card(2, y, w-4, 22, col)
			c.icon(icon, 7, y+4, colWhite)
			f := fitText(val, w-34, fontMed, fontSmall)
			c.text(f, 28, y+16, val, colWhite)
		}
		row(3, "house/14", firstNonEmpty(d.LANIP, "-"), colBlue)
		row(28, "globe/14", firstNonEmpty(d.WANIP, "no uplink"), colPurple)
		cl := "-"
		if d.Clients >= 0 {
			cl = strconv.Itoa(d.Clients)
		}
		c.card(2, 53, 40, 22, colTeal)
		c.icon("wifi/14", 6, 57, colWhite)
		c.text(fontMed, 26, 69, cl, colWhite)
		c.card(44, 53, 82, 22, tempColor(d))
		c.icon("plug/14", 49, 57, colWhite)
		if !d.OnUSB {
			c.fill(47, 63, 18, 2, tempColor(d)) // strike the plug when on battery
		}
		c.tempText(fontMed, 66, 69, d.TempC, colWhite, true)
		// big battery gauge
		if d.BattPct >= 0 {
			c.battery(6, 82, 86, 34, d, "bolt/16")
			c.text(fontBig, 97, 105, strconv.Itoa(d.BattPct), battColor(d))
			c.text(fontSmall, 97, 116, "%", colDim)
		} else {
			c.textC(fontMed, w/2, 104, "No battery", colDim)
		}
	case 3: // system: CPU load, memory, storage, CPU clock
		meter := func(y int, icon, val string, frac float64, col color.RGBA) {
			c.roundRect(2, y, w-4, 28, colTile)
			c.badge(5, y+4, 20, icon, col)
			c.textR(fontMed, w-6, y+15, val, colFG)
			c.pill(30, y+19, w-36, 5, colLine)
			if fw := int(float64(w-36) * math.Min(math.Max(frac, 0), 1)); fw > 0 {
				fc := col
				switch {
				case frac >= 0.9:
					fc = colBad
				case frac >= 0.7:
					fc = colWarn
				}
				c.pill(30, y+19, max(fw, 3), 5, fc)
			}
		}
		meter(2, "microchip/14", fmt.Sprintf("%.0f%%", d.CPUPct), d.CPUPct/100, colBlue)
		meter(32, "memory/14", fmt.Sprintf("%s / %s", fmtSize(d.MemUsed), fmtSize(d.MemTotal)), frac(d.MemUsed, d.MemTotal), colPurple)
		meter(62, "hard-drive/14", fmt.Sprintf("%s / %s", fmtSize(d.DiskUsed), fmtSize(d.DiskTotal)), frac(d.DiskUsed, d.DiskTotal), colTeal)
		ccol := colOrange
		if d.CPUThermalMHz > 0 {
			ccol = colWarn // the kernel's thermal throttle is holding the clock down
		}
		// the clock bar is a level, not a load: drawn directly so a full clock isn't red
		c.roundRect(2, 92, w-4, 28, colTile)
		c.badge(5, 96, 20, "gauge-high/14", ccol)
		c.textR(fontMed, w-6, 107, fmt.Sprintf("%d MHz", d.CPUMHz), colFG)
		c.pill(30, 111, w-36, 5, colLine)
		if cw := int(float64(w-36) * math.Min(frac(uint64(d.CPUMHz), uint64(d.CPUMaxMHz)), 1)); cw > 0 {
			c.pill(30, 111, max(cw, 3), 5, ccol)
		}
	case 4: // Wi-Fi join QR code
		c.icon("wifi/14", 4, 3, colBlue)
		ssid := firstNonEmpty(d.Wifi.SSID, "Wi-Fi off")
		c.text(fitText(ssid, w-26, fontMed, fontSmall), 23, 15, ssid, colFG)
		if q := d.Wifi.Code; q != nil {
			sc, quiet, side := qrLayout(q.Size, w, h)
			x0, y0 := (w-side)/2, 20+(h-26-side)/2
			c.fill(x0, y0, side, side, color.RGBA{255, 255, 255, 255})
			dark := color.RGBA{0, 0, 0, 255}
			for y := 0; y < q.Size; y++ {
				for x := 0; x < q.Size; x++ {
					if q.Black(x, y) {
						c.fill(x0+(x+quiet)*sc, y0+(y+quiet)*sc, sc, sc, dark)
					}
				}
			}
		}
	}
	if page == lcdAQIPage {
		c.aqiPage(d.AQI, w)
	}
	if page == lcdCellPage {
		c.cellPage(d.Cell, w)
	}
	if page == lcdSignalPage {
		c.signalPage(d, w)
	}
	c.pageDots(page, w, h)
	return c.RGBA
}

// rainbowBars: signal meter, bar i lit in its own colour (red, orange, amber, lime, green).
func (c canvas) rainbowBars(x, y, w, h int, signal int) {
	cols := []color.RGBA{colBad, colOrange, colWarn, colLime, colGreen}
	const n = 5
	gap := 3
	bw := (w - gap*(n-1)) / n
	lit := 0
	if signal >= 0 {
		lit = (signal + 19) / 20
	}
	for i := 0; i < n; i++ {
		bh := h * (i + 1) / n
		bc := colLine
		if i < lit {
			bc = cols[i]
		}
		c.roundRect(x+i*(bw+gap), y+h-bh, bw, bh, bc)
	}
}

// qrLayout: pixels per module, quiet-zone modules and the white card's side for a size x size QR code below the
// title row. The quiet zone is 2 modules, or 1 when that buys a bigger module (the cream around it is light too).
func qrLayout(size, w, h int) (scale, quiet, side int) {
	avail := min(w-4, h-26)
	scale, quiet = avail/(size+4), 2
	if s1 := avail / (size + 2); s1 > scale {
		scale, quiet = s1, 1
	}
	scale = max(scale, 1)
	return scale, quiet, (size + 2*quiet) * scale
}

func frac(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// fmtSize: compact sizes for the system page (317M, 3.4G).
func fmtSize(n uint64) string {
	f := float64(n)
	switch {
	case f >= 10<<30:
		return fmt.Sprintf("%.0fG", f/(1<<30))
	case f >= 1<<30:
		return fmt.Sprintf("%.1fG", f/(1<<30))
	}
	return fmt.Sprintf("%.0fM", f/(1<<20))
}

// lcdRenderPowerOff: the hold-to-power-off prompt (progress 0..1), or the final message at 1.
func lcdRenderPowerOff(progress float64, w, h int) *image.RGBA {
	c := newCanvas(w, h)
	if progress >= 1 {
		c.textC(fontBig, w/2, 70, "Bye", colOrange)
		return c.RGBA
	}
	c.textC(fontMed, w/2, 48, "Hold to power off", colFG)
	c.pill(14, 60, w-28, 12, colLine)
	if p := int(float64(w-28) * progress); p > 0 {
		c.pill(14, 60, max(p, 4), 12, colBad)
	}
	c.textC(fontSmall, w/2, 92, "release to cancel", colDim)
	return c.RGBA
}

// ---------------------------------------------------------------- main loop

const (
	lcdHoldShow     = 700 * time.Millisecond // power key held this long: show the power-off prompt
	lcdHoldPowerOff = 3 * time.Second        // held this long: shut down
)

func lcdLoop() {
	if !lcdEnabled() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("lcd: stopped after panic: %v", r)
		}
	}()
	log.Printf("lcd: MF800 panel found; waiting for the modem before attaching the SPI display")
	for i := 0; i < 90 && !modemRunning(); i++ {
		time.Sleep(time.Second)
	}
	time.Sleep(3 * time.Second) // the modem reconfigures the pins while it starts

	var fb *lcdFB
	attach := func(why string) { // full re-attach: only when the framebuffer is missing or unusable
		fb.close()
		fb = nil
		if err := lcdAttach(); err != nil {
			log.Printf("lcd: attach (%s): %v", why, err)
			return
		}
		f, err := openFB()
		if err != nil {
			log.Printf("lcd: %v", err)
			return
		}
		fb = f
		log.Printf("lcd: display attached (%s), %dx%d", why, fb.w, fb.h)
	}
	repairs := 0
	repair := func(why string) {
		if err := lcdRepair(); err != nil {
			log.Printf("lcd: repair (%s): %v", why, err)
			attach(why)
			return
		}
		if fb != nil {
			fb.last = nil // frames sent while the pins were wrong were lost
		}
		if repairs++; repairs <= 5 || repairs%100 == 0 {
			log.Printf("lcd: SPI pins restored (%s), %d so far", why, repairs)
		}
	}
	if f, err := openFB(); err == nil {
		fb = f
		repair("start") // pins + a clean panel init now that the modem is up
	} else {
		attach("start")
	}

	keys := make(chan lcdKey, 8)
	go lcdButtons(keys)
	awake, lastPress, page := true, time.Now(), 0
	var holdSince time.Time // power key held since (zero = not held)
	pressWoke := false      // this press only woke the screen: don't also switch the page
	wake := func() {
		if !awake {
			awake = true
			lcdBacklightOn(true)
		}
		lastPress = time.Now()
	}
	lcdBacklightOn(true)
	lastUSB := ""
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	n := 0
	var data lcdData
	var dataAt, drawnAt time.Time // drawnAt: last dashboard render (the clock needs at most 1/s)
	for {
		select {
		case k := <-keys:
			if k.down {
				pressWoke = !awake
			}
			wake()
			switch {
			case k.power && k.down:
				holdSince = time.Now()
			case k.power: // released: a short press switches the dashboard
				if !holdSince.IsZero() && time.Since(holdSince) < lcdHoldShow && !pressWoke {
					page = (page + 1) % lcdPages
					dataAt = time.Time{}
				}
				holdSince = time.Time{}
			case k.down && !pressWoke: // the other button: next dashboard on press
				page = (page + 1) % lcdPages
				dataAt = time.Time{}
			}
		case <-tick.C:
			n++
		}
		// safety net for anything re-muxing the SPI pins: check every second while on, every 5 s asleep
		if (awake && n%5 == 0) || n%25 == 0 {
			if ok, _ := lcdPinsOK(); !ok {
				repair("pins re-muxed")
			}
		}
		// plugging or unplugging the charger wakes the screen
		if usb := readTrim("/sys/class/power_supply/pm8916-lbc-chgr/online"); usb != lastUSB {
			if lastUSB != "" {
				wake()
			}
			lastUSB = usb
		}
		held := time.Duration(0)
		if !holdSince.IsZero() {
			held = time.Since(holdSince)
			lastPress = time.Now() // no sleeping while the key is held
		}
		sleepAfter := *flagLCDSleep
		if batterySaving.Load() && (sleepAfter == 0 || sleepAfter > 8*time.Second) {
			sleepAfter = 8 * time.Second // battery low: the backlight is a big share of the drain
		}
		if awake && sleepAfter > 0 && time.Since(lastPress) > sleepAfter {
			awake = false
			lcdBacklightOn(false)
		}
		if !awake || fb == nil {
			continue
		}
		var img *image.RGBA
		switch {
		case held >= lcdHoldPowerOff:
			fb.show(lcdRenderPowerOff(1, fb.w, fb.h), *flagLCDSwap)
			log.Printf("lcd: power key held %v: powering off", lcdHoldPowerOff)
			if out, err := run(10*time.Second, "systemctl", "--no-block", "poweroff"); err != nil {
				log.Printf("lcd: poweroff: %v %s", err, out)
			}
			holdSince = time.Time{}
			continue
		case held >= lcdHoldShow:
			img = lcdRenderPowerOff(float64(held-lcdHoldShow)/float64(lcdHoldPowerOff-lcdHoldShow), fb.w, fb.h)
		default:
			if time.Since(drawnAt) < time.Second && time.Since(lastPress) > 300*time.Millisecond {
				continue // nothing new to show yet; a button press redraws at once
			}
			drawnAt = time.Now()
			if time.Since(dataAt) >= 2*time.Second {
				data, dataAt = lcdCollect(page), time.Now()
			}
			data.Now = time.Now()
			img = lcdRender(data, page, fb.w, fb.h)
		}
		if err := fb.show(img, *flagLCDSwap); err != nil {
			log.Printf("lcd: write: %v", err)
			attach("write error")
		}
	}
}

// signalPage: large live LTE readings for finding the best spot (RSRP decides the colour).
func (c canvas) signalPage(d lcdData, w int) {
	qcol := func(kind string, v float64) color.RGBA {
		if math.IsNaN(v) {
			return colDim
		}
		return []color.RGBA{colBad, colWarn, colLime, colGreen}[signalQuality(kind, v)]
	}
	col := qcol("rsrp", d.RSRP)
	c.card(2, 2, w-4, 62, col)
	c.icon("tower-cell/12", 7, 7, colWhite)
	c.text(fontSmall, 22, 16, "RSRP dBm", colWhite)
	if !math.IsNaN(d.RSRP) {
		c.textR(fontSmall, w-7, 16, qualityName[signalQuality("rsrp", d.RSRP)], colWhite)
		v := fmt.Sprintf("%.0f", math.Abs(d.RSRP))
		const minusW = 9 // the clock font has digits only: the minus is drawn
		x := (w - minusW - 3 - fontClock.width(v)) / 2
		if d.RSRP < 0 {
			c.fill(x, 54-11, minusW, 3, colWhite)
		}
		c.text(fontClock, x+minusW+3, 54, v, colWhite)
	} else {
		c.textC(fontMed, w/2, 46, "no signal", colWhite)
	}
	tile := func(x int, label, kind string, v float64, unit string) {
		c.card(x, 68, 61, 52, qcol(kind, v))
		c.textC(fontSmall, x+61/2, 82, label, colWhite)
		s := "--"
		if !math.IsNaN(v) {
			s = fmt.Sprintf("%.0f", v)
		}
		c.textC(fontBig, x+61/2, 104, s, colWhite)
		c.textC(fontSmall, x+61/2, 116, unit, colWhiteDim)
	}
	tile(2, "SINR", "snr", d.SNR, "dB")
	tile(65, "RSRQ", "rsrq", d.RSRQ, "dB")
}

// aqiPage: New Delhi's AQI in its category colour, with the PM2.5 and PM10 24-hour averages below.
func (c canvas) aqiPage(a lcdAQIState, w int) {
	col := colDim
	name := "Loading..."
	if a.AQI > 0 {
		name, col = aqiCategory(a.AQI)
	} else if a.Err != "" {
		name = "No data"
	}
	c.card(2, 2, w-4, 74, col)
	c.icon("smog/14", 7, 6, colWhite)
	c.text(fontSmall, 26, 17, "Delhi AQI", colWhite)
	if !a.At.IsZero() {
		c.textR(fontSmall, w-7, 17, a.At.Format("15:04"), colWhiteDim)
	}
	if a.AQI > 0 {
		c.textC(fontClock, w/2, 52, strconv.Itoa(a.AQI), colWhite)
	} else {
		c.textC(fontBig, w/2, 48, "--", colWhite) // the clock font has digits only
	}
	c.textC(fitText(name, w-12, fontMed, fontSmall), w/2, 69, name, colWhite)
	tile := func(x int, label string, v float64, sub int) {
		tc := colDim
		s := "--"
		if a.AQI > 0 {
			_, tc = aqiCategory(sub)
			s = fmt.Sprintf("%.0f", v)
		}
		c.card(x, 80, 61, 40, tc)
		c.textC(fontSmall, x+61/2, 93, label, colWhiteDim)
		c.textC(fontBig, x+61/2, 114, s, colWhite)
	}
	tile(2, "PM2.5", a.PM25, a.SubPM25)
	tile(65, "PM10", a.PM10, a.SubPM10)
}

// cellPage: the serving tower (eNB-sector, band), its position from a cell database, and distance (timing advance),
// PCI and neighbour count.
func (c canvas) cellPage(s lcdCellState, w int) {
	sv := s.Serving
	c.card(2, 2, w-4, 36, colIndigo)
	c.icon("tower-cell/12", 6, 6, colWhite)
	c.text(fontSmall, 22, 16, "Cell tower", colWhite)
	if b, _ := lteBand(sv.EARFCN); s.Have && b > 0 {
		c.textR(fontSmall, w-7, 16, "B"+strconv.Itoa(b), colWhiteDim)
	}
	id := "No cell"
	if s.Have && sv.CI >= 0 {
		id = fmt.Sprintf("eNB %d-%d", sv.CI>>8, sv.CI&0xFF) // LTE ECI = 20-bit eNodeB ID + 8-bit sector
	}
	c.text(fitText(id, w-14, fontMed, fontSmall), 7, 33, id, colWhite)

	pcol := colDim
	if s.Pos.Found {
		pcol = colTeal
	}
	c.card(2, 42, w-4, 38, pcol)
	c.icon("location-dot/14", 6, 46, colWhite)
	switch {
	case s.Pos.Found:
		ns, ew := "N", "E"
		if s.Pos.Lat < 0 {
			ns = "S"
		}
		if s.Pos.Lon < 0 {
			ew = "W"
		}
		c.text(fontMed, 22, 58, fmt.Sprintf("%.4f %s", math.Abs(s.Pos.Lat), ns), colWhite)
		c.text(fontMed, 22, 75, fmt.Sprintf("%.4f %s", math.Abs(s.Pos.Lon), ew), colWhite)
	case !s.Have:
		c.text(fontMed, 22, 58, "No position", colWhite)
	case s.Looking:
		c.text(fontMed, 22, 58, "Looking up...", colWhite)
	case s.Pos.Err != "":
		c.text(fontMed, 22, 58, "Lookup failed", colWhite)
	default:
		c.text(fontMed, 22, 58, "Not in cell DB", colWhite)
	}
	if !s.Pos.Found && s.Have && sv.TAC >= 0 {
		c.text(fontSmall, 22, 74, fmt.Sprintf("TAC %d", sv.TAC), colWhiteDim)
	}

	tile := func(x int, val, label string, col color.RGBA) {
		c.card(x, 84, 40, 36, col)
		c.textC(fitText(val, 36, fontMed, fontSmall), x+20, 101, val, colWhite)
		c.textC(fontSmall, x+20, 115, label, colWhiteDim)
	}
	dist, pci, nb := "--", "--", "--"
	if s.Have {
		if m := taDistanceM(sv.TimingAdvanceU); m >= 0 {
			dist = fmtDist(m)
		}
		if sv.PCI >= 0 {
			pci = strconv.FormatInt(sv.PCI, 10)
		}
		nb = strconv.Itoa(s.Neighbours)
	}
	tile(2, dist, "away", colPurple)
	tile(44, pci, "PCI", colBlue)
	tile(86, nb, "nearby", colOrange)
}

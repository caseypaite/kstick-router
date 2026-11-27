package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLCDPixelSwap(t *testing.T) {
	for _, c := range []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {12, 200, 77, 255}} {
		p := lcdPixel(c, true)
		// kernel: XRGB8888 (B,G,R,X bytes) -> RGB565 by truncation; the wire swaps the two bytes
		k := uint16(p[2]>>3)<<11 | uint16(p[1]>>2)<<5 | uint16(p[0]>>3)
		got := k<<8 | k>>8
		want := uint16(c.R>>3)<<11 | uint16(c.G>>2)<<5 | uint16(c.B>>3)
		if got != want {
			t.Errorf("%v: panel gets %04x, want %04x", c, got, want)
		}
	}
	if p := lcdPixel(color.RGBA{1, 2, 3, 255}, false); p != [4]byte{3, 2, 1, 0} {
		t.Errorf("no swap: %v", p)
	}
}

func TestLCDPinsOKText(t *testing.T) {
	good := " gpio16  : out high func1 12mA no pull\n gpio17  : in  low  func1 12mA no pull\n gpio19  : out low  func1 12mA no pull\n"
	bad := " gpio16  : out high func0 8mA no pull\n gpio19  : out low  func0 8mA no pull\n"
	if ok, unk := lcdPinsOKText(good); !ok || unk {
		t.Errorf("good: ok=%v unknown=%v", ok, unk)
	}
	if ok, _ := lcdPinsOKText(bad); ok {
		t.Error("bad pins reported ok")
	}
	if _, unk := lcdPinsOKText(""); !unk {
		t.Error("empty text should be unknown")
	}
}

func TestLCDRender(t *testing.T) {
	now := time.Date(2026, 11, 25, 22, 45, 0, 0, time.Local)
	cases := map[string]lcdData{
		"ok": {Present: true, Operator: "IND airtel", Tech: "lte", Registration: "home", Uplink: "wwan0", Signal: 88, RSRP: -75, RSRQ: -10, SNR: 20.4, BattPct: 92, BattMV: 4217, Charging: true, OnUSB: true, TempC: 69.4, DownBps: 1.53e6 / 8, UpBps: 230e3 / 8, LANIP: "192.168.4.1", WANIP: "100.101.101.177", Clients: 3,
			CPUPct: 37, MemUsed: 158 << 20, MemTotal: 317 << 20, DiskUsed: 1400 << 20, DiskTotal: 3400 << 20, CPUMHz: 800, CPUMaxMHz: 998, Now: now},
		"hot": {Present: true, Operator: "IND airtel", Tech: "lte", Registration: "home", Uplink: "", Signal: 40, RSRP: -104, RSRQ: -16, SNR: 2, BattPct: 12, TempC: 86.2, Throttled: true,
			CPUPct: 95, MemUsed: 290 << 20, MemTotal: 317 << 20, DiskUsed: 3200 << 20, DiskTotal: 3400 << 20, CPUMHz: 400, CPUMaxMHz: 998, CPUThermalMHz: 400, Now: now},
		"none": {Signal: -1, BattPct: -1, Clients: -1, Now: now, RSRP: math.NaN(), RSRQ: math.NaN(), SNR: math.NaN()},
	}
	out := os.Getenv("LCD_PREVIEW_DIR")
	save := func(name string, img *image.RGBA) {
		if img.Rect.Dx() != 128 || img.Rect.Dy() != 128 {
			t.Fatalf("%s: size %v", name, img.Rect)
		}
		if out == "" {
			return
		}
		f, err := os.Create(filepath.Join(out, "lcd-"+name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, img)
		f.Close()
	}
	for name, d := range cases {
		if name == "ok" {
			code := wifiQRCode(wifiQRText("KStick-7F3A", "q8Hn2vLp4Rx9Tz", "wpa-psk", false))
			d.Wifi = lcdWifiState{SSID: "KStick-7F3A", Code: code}
			d.AQI = lcdAQIState{AQI: 287, PM25: 116.4, PM10: 198.2, SubPM25: 287, SubPM10: 166, At: now}
			d.Cell = lcdCellState{Have: true, Neighbours: 2, Serving: lteCell{Serving: true, OperatorID: "40410", TAC: 0x87D, CI: 0xE7F220C, PCI: 31, EARFCN: 39151, TimingAdvanceU: 20},
				Pos: cellPos{Lat: 28.61394, Lon: 77.20902, RangeM: 900, Src: "OpenCelliD", Found: true}}
		}
		if name == "hot" {
			d.AQI = lcdAQIState{AQI: 452, PM25: 318, PM10: 420, SubPM25: 452, SubPM10: 388, At: now}
			d.Cell = lcdCellState{Have: true, Serving: lteCell{Serving: true, OperatorID: "40410", TAC: 0x87D, CI: 0xE7F220C, PCI: 503, EARFCN: 1650, TimingAdvanceU: 85}}
		}
		if name == "none" {
			d.AQI = lcdAQIState{Err: "timeout"}
		}
		for p := 0; p < lcdPages; p++ {
			save(fmt.Sprintf("%s-%d", name, p), lcdRender(d, p, 128, 128))
		}
	}
	save("poweroff-hold", lcdRenderPowerOff(0.6, 128, 128))
	save("poweroff", lcdRenderPowerOff(1, 128, 128))
}

func TestNetStatus(t *testing.T) {
	for _, c := range []struct {
		d    lcdData
		want string
	}{
		{lcdData{Present: true, Tech: "lte", Registration: "home", Uplink: "wwan0"}, "4G online"},
		{lcdData{Present: true, Tech: "lte", Registration: "roaming", Uplink: "wwan0"}, "4G roaming"},
		{lcdData{Present: true, Tech: "lte", Registration: "home"}, "4G no data"},
		{lcdData{Present: true, Registration: "searching"}, "Searching..."},
		{lcdData{Present: true, State: "failed"}, "SIM error"},
		{lcdData{Present: true, Registration: "home", Cooldown: true}, "Off (too hot)"},
		{lcdData{}, "No modem"},
		{lcdData{Uplink: "usb0"}, "Online (USB)"},
	} {
		if got, _ := netStatus(c.d); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.d, got, c.want)
		}
	}
}

func TestWifiQRText(t *testing.T) {
	for _, c := range []struct {
		ssid, psk, km string
		hidden        bool
		want          string
	}{
		{"KStick-AB12", "secret123", "wpa-psk", false, "WIFI:T:WPA;S:KStick-AB12;P:secret123;;"},
		{`a;b,c:d\e"f`, `p;w:d`, "sae", true, `WIFI:T:WPA;S:a\;b\,c\:d\\e\"f;P:p\;w\:d;H:true;;`},
		{"Open", "", "", false, "WIFI:T:nopass;S:Open;;"},
	} {
		if got := wifiQRText(c.ssid, c.psk, c.km, c.hidden); got != c.want {
			t.Errorf("wifiQRText(%q) = %q, want %q", c.ssid, got, c.want)
		}
	}
}

// Even a maximum-length SSID and password (all characters needing escapes) give >= 2 px per module.
func TestWifiQRWorstCaseFits(t *testing.T) {
	for _, psk := range []string{strings.Repeat("x", 63), strings.Repeat(";", 63)} {
		c := wifiQRCode(wifiQRText(strings.Repeat("S", 32), psk, "wpa-psk", true))
		if c == nil {
			t.Fatal("no code")
		}
		if sc, _, side := qrLayout(c.Size, 128, 128); sc < 2 || side > 102 {
			t.Fatalf("QR %d modules: %d px per module, %d px", c.Size, sc, side)
		}
	}
}

package main

// Air quality for the MF800 status screen: New Delhi's Indian AQI (CPCB scale) from Open-Meteo's air-quality API
// (CAMS model data, no API key). Fetched only while the AQI screen is showing, at most every 30 minutes (~2 KB),
// so it adds no idle LTE traffic.

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	aqiCity    = "New Delhi"
	aqiURL     = "https://air-quality-api.open-meteo.com/v1/air-quality?latitude=28.6139&longitude=77.2090&current=pm2_5,pm10&hourly=pm2_5,pm10&past_days=1&forecast_days=1&timezone=Asia%2FKolkata"
	aqiRefresh = 30 * time.Minute
	aqiRetry   = 2 * time.Minute // after a failed fetch
)

type lcdAQIState struct {
	AQI        int     // Indian AQI, 0 = no data
	PM25, PM10 float64 // 24-hour averages, µg/m³
	SubPM25    int     // sub-indices
	SubPM10    int
	At         time.Time // the hour the latest reading is for
	Err        string    // last fetch failed (data, if any, is older)
}

var (
	aqiMu      sync.Mutex
	aqiCur     lcdAQIState
	aqiFetched time.Time
	aqiTried   time.Time
	aqiBusy    bool
	aqiHTTP    = &http.Client{Timeout: 20 * time.Second}
)

// lcdAQI returns the latest air quality and starts a background refresh when it is due.
func lcdAQI() lcdAQIState {
	aqiMu.Lock()
	defer aqiMu.Unlock()
	due := time.Since(aqiFetched) >= aqiRefresh && time.Since(aqiTried) >= aqiRetry
	if due && !aqiBusy {
		aqiBusy, aqiTried = true, time.Now()
		go func() {
			s, err := fetchAQI()
			aqiMu.Lock()
			defer aqiMu.Unlock()
			aqiBusy = false
			if err != nil {
				log.Printf("lcd: air quality: %v", err)
				aqiCur.Err = err.Error()
				return
			}
			aqiCur, aqiFetched = s, time.Now()
		}()
	}
	return aqiCur
}

func fetchAQI() (lcdAQIState, error) {
	req, _ := http.NewRequest("GET", aqiURL, nil)
	req.Header.Set("User-Agent", "router-webui")
	resp, err := aqiHTTP.Do(req)
	if err != nil {
		return lcdAQIState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return lcdAQIState{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return lcdAQIState{}, err
	}
	return parseAQI(b)
}

func parseAQI(b []byte) (lcdAQIState, error) {
	var r struct {
		UTCOffset int `json:"utc_offset_seconds"`
		Current   struct {
			Time string `json:"time"`
		} `json:"current"`
		Hourly struct {
			Time []string   `json:"time"`
			PM25 []*float64 `json:"pm2_5"`
			PM10 []*float64 `json:"pm10"`
		} `json:"hourly"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return lcdAQIState{}, err
	}
	loc := time.FixedZone("", r.UTCOffset)
	now, err := time.ParseInLocation("2006-01-02T15:04", r.Current.Time, loc)
	if err != nil {
		return lcdAQIState{}, fmt.Errorf("current time: %v", err)
	}
	// CPCB: 24-hour averages, valid with at least 16 hourly values
	var s25, s10 float64
	var n25, n10 int
	for i, ts := range r.Hourly.Time {
		t, err := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		if err != nil || t.After(now) || !t.After(now.Add(-24*time.Hour)) {
			continue
		}
		if i < len(r.Hourly.PM25) && r.Hourly.PM25[i] != nil {
			s25, n25 = s25+*r.Hourly.PM25[i], n25+1
		}
		if i < len(r.Hourly.PM10) && r.Hourly.PM10[i] != nil {
			s10, n10 = s10+*r.Hourly.PM10[i], n10+1
		}
	}
	if n25 < 16 || n10 < 16 {
		return lcdAQIState{}, fmt.Errorf("too few hourly values (PM2.5 %d, PM10 %d)", n25, n10)
	}
	s := lcdAQIState{PM25: s25 / float64(n25), PM10: s10 / float64(n10), At: now}
	s.SubPM25 = aqiSubIndex(s.PM25, aqiPM25)
	s.SubPM10 = aqiSubIndex(s.PM10, aqiPM10)
	s.AQI = max(s.SubPM25, s.SubPM10)
	return s, nil
}

// CPCB breakpoints (24-hour µg/m³) at AQI 50, 100, 200, 300, 400, 500; interpolated without gaps between bands.
var (
	aqiIdx  = []float64{0, 50, 100, 200, 300, 400, 500}
	aqiPM25 = []float64{0, 30, 60, 90, 120, 250, 380}
	aqiPM10 = []float64{0, 50, 100, 250, 350, 430, 510}
)

func aqiSubIndex(c float64, bp []float64) int {
	if c <= 0 {
		return 0
	}
	for i := 1; i < len(bp); i++ {
		if c <= bp[i] {
			return int(aqiIdx[i-1] + (c-bp[i-1])/(bp[i]-bp[i-1])*(aqiIdx[i]-aqiIdx[i-1]) + 0.5)
		}
	}
	return 500
}

// aqiCategory: CPCB name and a colour dark enough for white text.
func aqiCategory(aqi int) (string, color.RGBA) {
	switch {
	case aqi <= 50:
		return "Good", colGreen
	case aqi <= 100:
		return "Satisfactory", color.RGBA{80, 115, 10, 255}
	case aqi <= 200:
		return "Moderate", colWarn
	case aqi <= 300:
		return "Poor", colOrange
	case aqi <= 400:
		return "Very Poor", colBad
	}
	return "Severe", color.RGBA{120, 20, 45, 255}
}

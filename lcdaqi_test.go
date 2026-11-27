package main

import (
	"os"
	"testing"
)

func TestAQISubIndex(t *testing.T) {
	for _, c := range []struct {
		c    float64
		bp   []float64
		want int
	}{
		{0, aqiPM25, 0}, {30, aqiPM25, 50}, {45, aqiPM25, 75}, {90, aqiPM25, 200}, {185, aqiPM25, 350},
		{380, aqiPM25, 500}, {900, aqiPM25, 500},
		{100, aqiPM10, 100}, {175, aqiPM10, 150}, {430, aqiPM10, 400},
	} {
		if got := aqiSubIndex(c.c, c.bp); got != c.want {
			t.Errorf("aqiSubIndex(%v) = %d, want %d", c.c, got, c.want)
		}
	}
	for aqi, want := range map[int]string{1: "Good", 100: "Satisfactory", 101: "Moderate", 300: "Poor", 400: "Very Poor", 401: "Severe"} {
		if got, _ := aqiCategory(aqi); got != want {
			t.Errorf("aqiCategory(%d) = %q, want %q", aqi, got, want)
		}
	}
}

func TestParseAQI(t *testing.T) {
	b, err := os.ReadFile("testdata/openmeteo-aqi-delhi.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseAQI(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.AQI <= 0 || s.AQI > 500 || s.PM25 <= 0 || s.PM10 <= 0 || s.At.IsZero() {
		t.Fatalf("parsed %+v", s)
	}
	if s.AQI != max(s.SubPM25, s.SubPM10) {
		t.Errorf("AQI %d is not the larger sub-index (%d, %d)", s.AQI, s.SubPM25, s.SubPM10)
	}
	t.Logf("Delhi: AQI %d, PM2.5 %.0f, PM10 %.0f at %s", s.AQI, s.PM25, s.PM10, s.At.Format("2006-01-02 15:04"))
	if _, err := parseAQI([]byte(`{"current":{"time":"2026-11-27T07:00"},"hourly":{"time":[],"pm2_5":[],"pm10":[]}}`)); err == nil {
		t.Error("no hourly values: want an error")
	}
}

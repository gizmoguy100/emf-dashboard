package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseEventTimeUsesLondonDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}

	parsed, ok := parseEventTime("2026-06-01 12:30:00", loc)
	if !ok {
		t.Fatal("expected event time to parse")
	}
	if got := parsed.Format("15:04 MST -07:00"); got != "12:30 BST +01:00" {
		t.Fatalf("expected BST local time, got %s", got)
	}
}

func TestCountdownIncludesHoursAndMinutes(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	start := now.Add(2*time.Hour + 17*time.Minute)

	got := countdown(now, start)
	if !strings.Contains(got, "2 hours") || !strings.Contains(got, "17 mins") {
		t.Fatalf("expected hours and minutes countdown, got %q", got)
	}
}

func TestFindHAStateVitalFindsPixelBattery(t *testing.T) {
	states := []haState{
		{EntityID: "sensor.kitchen_battery_level", State: "77"},
		{
			EntityID: "sensor.pixel_8_battery_level",
			State:    "42",
			Attributes: map[string]any{
				"friendly_name":       "Pixel 8 Battery Level",
				"unit_of_measurement": "%",
			},
		},
	}

	vital, ok := findHAStateVital(states, "Phone Battery", []string{"battery_level"})
	if !ok {
		t.Fatal("expected pixel battery vital")
	}
	if vital.Value != "42%" {
		t.Fatalf("expected value with unit, got %q", vital.Value)
	}
}

func TestFindHAStateVitalFormatsStepUnits(t *testing.T) {
	states := []haState{
		{
			EntityID: "sensor.pixel_8_pro_steps_sensor",
			State:    "28348",
			Attributes: map[string]any{
				"friendly_name":       "Pixel 8 Pro Steps sensor",
				"unit_of_measurement": "steps",
			},
		},
	}

	vital, ok := findHAStateVital(states, "Steps", []string{"steps"})
	if !ok {
		t.Fatal("expected pixel steps vital")
	}
	if vital.Value != "28348 steps" {
		t.Fatalf("expected spaced steps unit, got %q", vital.Value)
	}
}

func TestHAVitalFromConfiguredMetric(t *testing.T) {
	snapshot := haSnapshot{
		States: []haState{
			{
				EntityID: "sensor.pixel_8_pro_daily_distance",
				State:    "2901.813",
				Attributes: map[string]any{
					"friendly_name":       "Pixel 8 Pro Daily distance",
					"unit_of_measurement": "m",
				},
			},
		},
	}

	vital, ok := haVitalFromMetric(snapshot, haMetricConfig{
		Label:    "Daily Distance",
		EntityID: "sensor.pixel_8_pro_daily_distance",
	})
	if !ok {
		t.Fatal("expected configured daily distance metric")
	}
	if vital.Value != "2901.813 m" {
		t.Fatalf("expected daily distance with unit, got %q", vital.Value)
	}
}

func TestHAVitalFromProgressMetric(t *testing.T) {
	snapshot := haSnapshot{
		States: []haState{
			{
				EntityID: "sensor.pixel_8_pro_battery_level",
				State:    "42",
				Attributes: map[string]any{
					"friendly_name":       "Pixel 8 Pro Battery Level",
					"unit_of_measurement": "%",
				},
			},
		},
	}

	vital, ok := haVitalFromMetric(snapshot, haMetricConfig{
		Label:    "Phone Battery",
		EntityID: "sensor.pixel_8_pro_battery_level",
		Display:  "progress",
	})
	if !ok {
		t.Fatal("expected configured battery metric")
	}
	if vital.Value != "42%" {
		t.Fatalf("expected battery percentage, got %q", vital.Value)
	}
	if !vital.HasProgress || vital.ProgressPercent != 42 {
		t.Fatalf("expected 42 percent progress, got enabled=%v value=%d", vital.HasProgress, vital.ProgressPercent)
	}
}

func TestComputeHADailyDelta(t *testing.T) {
	value, err := computeHADailyDelta([][]haState{
		{
			{State: "26802"},
			{State: "27000"},
			{State: "28375"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if value != "1573" {
		t.Fatalf("expected daily delta, got %q", value)
	}
}

func TestHAVitalFromDailyDeltaMetric(t *testing.T) {
	snapshot := haSnapshot{
		DailyDeltas: map[string]string{
			"sensor.pixel_8_pro_steps_sensor": "1573",
		},
	}

	vital, ok := haVitalFromMetric(snapshot, haMetricConfig{
		Label:    "Steps Today",
		EntityID: "sensor.pixel_8_pro_steps_sensor",
		Mode:     "daily_delta",
		Unit:     "steps",
	})
	if !ok {
		t.Fatal("expected daily delta metric")
	}
	if vital.Value != "1573 steps" {
		t.Fatalf("expected daily steps with unit, got %q", vital.Value)
	}
}

func TestHAProgressPercentClamps(t *testing.T) {
	tests := map[string]int{
		"42":   42,
		"42%":  42,
		"0":    0,
		"100":  100,
		"150":  100,
		"-5":   0,
		"41.6": 42,
	}

	for value, want := range tests {
		got, ok := haProgressPercent(value)
		if !ok {
			t.Fatalf("expected %q to parse", value)
		}
		if got != want {
			t.Fatalf("expected %q to clamp to %d, got %d", value, want, got)
		}
	}
}

func TestWeatherFromForecastFormatsCurrentConditions(t *testing.T) {
	var forecast openMeteoCurrentResponse
	forecast.CurrentUnits.Temperature = "°C"
	forecast.CurrentUnits.Precip = "mm"
	forecast.Current.Temperature = 20.6
	forecast.Current.Precip = 0

	loc := time.FixedZone("BST", 3600)
	got := weatherFromForecast(forecast, time.Date(2026, 6, 20, 19, 30, 0, 0, time.UTC), loc, "Eastnor Deer Park", "live", "")

	if got.HeaderLabel != "21°C" || got.Now != "21°C" {
		t.Fatalf("expected rounded celsius labels, got header=%q now=%q", got.HeaderLabel, got.Now)
	}
	if got.Rain != "Dry" {
		t.Fatalf("expected dry precip label, got %q", got.Rain)
	}
	if got.Night != "Light layer" {
		t.Fatalf("expected layer hint, got %q", got.Night)
	}
	if got.CachedAtLabel != "20:30" {
		t.Fatalf("expected local cached label, got %q", got.CachedAtLabel)
	}
}

func TestBarStockLineDecodesLocationDisplayString(t *testing.T) {
	var line barStockLine
	err := json.Unmarshal([]byte(`{"location_display":"Robot Arms","location":"robotarms"}`), &line)
	if err != nil {
		t.Fatal(err)
	}
	if line.LocationDisplay != "Robot Arms" {
		t.Fatalf("expected string location display, got %q", line.LocationDisplay)
	}
}

func TestBarStockLineDecodesLocationDisplayObject(t *testing.T) {
	var line barStockLine
	err := json.Unmarshal([]byte(`{"location_display":{"sort":1,"slug":"robotarms","name":"Robot Arms"},"location":"robotarms"}`), &line)
	if err != nil {
		t.Fatal(err)
	}
	if line.LocationDisplay != "Robot Arms" {
		t.Fatalf("expected object location name, got %q", line.LocationDisplay)
	}
}

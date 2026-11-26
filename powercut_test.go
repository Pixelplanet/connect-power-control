package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func afterCutOf(m *module, cfg DeviceConfig) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.plugSnapshotLocked(cfg).AfterPowerCut
}

func TestAfterPowerCutParsing(t *testing.T) {
	tasmota := map[string]string{
		`0`: afterCutOff, `1`: afterCutOn, `2`: afterCutToggle, `3`: afterCutLast,
		`4`: afterCutAlwaysOn, `5`: afterCutDelayedOn, `"3"`: afterCutLast, `"ON"`: afterCutOn,
		`6`: "", `-1`: "", `null`: "", ``: "", `{"a":1}`: "", `"x"`: "",
	}
	for raw, want := range tasmota {
		if got := tasmotaAfterPowerCut(json.RawMessage(raw)); got != want {
			t.Errorf("Tasmota %s = %q, want %q", raw, got, want)
		}
	}
	gen2 := map[string]string{"off": afterCutOff, "on": afterCutOn, "restore_last": afterCutLast, "match_input": afterCutSwitch, "": "", "other": ""}
	for raw, want := range gen2 {
		if got := shellyGen2AfterPowerCut(raw); got != want {
			t.Errorf("Shelly Gen2 %q = %q, want %q", raw, got, want)
		}
	}
	gen1 := map[string]string{"off": afterCutOff, "on": afterCutOn, "last": afterCutLast, "switch": afterCutSwitch, "": "", "other": ""}
	for raw, want := range gen1 {
		if got := shellyGen1AfterPowerCut(raw); got != want {
			t.Errorf("Shelly Gen1 %q = %q, want %q", raw, got, want)
		}
	}
}

func TestTasmotaAfterPowerCutOverMQTT(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug1", RelayIndex: 2})

	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER1":"ON"}`), now)
	if got := afterCutOf(m, cfg); got != "" {
		t.Fatalf("a plug that said nothing has %q", got)
	}
	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"PowerOnState":4}`), now)
	if got := afterCutOf(m, cfg); got != afterCutAlwaysOn {
		t.Fatalf("after PowerOnState 4 = %q, want always_on", got)
	}
	// A value that is not understood leaves what is known.
	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"PowerOnState":9}`), now)
	if got := afterCutOf(m, cfg); got != afterCutAlwaysOn {
		t.Fatalf("a bad value changed it to %q", got)
	}
	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"PowerOnState":3}`), now)
	if got := afterCutOf(m, cfg); got != afterCutLast {
		t.Fatalf("after PowerOnState 3 = %q, want last", got)
	}
}

func TestTasmotaAfterPowerCutOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Status":{"DeviceName":"Plug","FriendlyName":["Plug"],"PowerOnState":0},"StatusSTS":{"POWER":"ON"}}`))
	}))
	defer srv.Close()

	m := newModule(nil)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: srv.URL, RelayIndex: 1})
	if err := m.pollHTTPPlug(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := afterCutOf(m, cfg); got != afterCutOff {
		t.Fatalf("after PowerOnState 0 = %q, want off", got)
	}
}

func TestShellyHTTPAfterPowerCut(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	gen2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/rpc/Switch.GetStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 0, "output": true})
		case "/rpc/Switch.GetConfig":
			asked++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 0, "name": nil, "initial_state": "restore_last"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gen2.Close()

	m, clock := clockedModule(energyStart)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: gen2.URL, RelayIndex: 1})
	for range 5 {
		if err := m.pollShellyHTTPPlug(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		clock.add(5 * time.Second)
	}
	if got := afterCutOf(m, cfg); got != afterCutLast {
		t.Fatalf("Gen2 = %q, want last", got)
	}
	mu.Lock()
	if asked != 1 {
		t.Errorf("five polls in 25 seconds asked for the setting %d times, want once", asked)
	}
	mu.Unlock()

	gen1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"relays": []map[string]any{{"ison": false}}})
		case "/settings/relay/0":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": nil, "default_state": "off"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gen1.Close()
	old := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: gen1.URL, RelayIndex: 1})
	if err := m.pollShellyHTTPPlug(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if got := afterCutOf(m, old); got != afterCutOff {
		t.Fatalf("Gen1 = %q, want off", got)
	}
}

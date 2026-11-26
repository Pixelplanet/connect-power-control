package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func TestLegacyModeBackwardsCompatibility(t *testing.T) {
	legacyMQTT := normalizeDeviceConfig(DeviceConfig{
		Mode:         "mqtt",
		TasmotaTopic: "prusa-mk4",
	})
	if legacyMQTT.effectiveDriver() != DriverTasmota || legacyMQTT.plugKey() != "prusa-mk4" {
		t.Fatalf("unexpected legacy MQTT config: driver=%q key=%q", legacyMQTT.effectiveDriver(), legacyMQTT.plugKey())
	}

	legacyHTTP := normalizeDeviceConfig(DeviceConfig{
		Mode:     "http",
		HTTPHost: "192.168.1.50",
	})
	if legacyHTTP.effectiveDriver() != DriverTasmotaHTTP || legacyHTTP.plugKey() != "http:192.168.1.50" {
		t.Fatalf("unexpected legacy HTTP config: driver=%q key=%q", legacyHTTP.effectiveDriver(), legacyHTTP.plugKey())
	}
}

func TestShellyGen1MQTTDriver(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	if !m.handleDriverMQTTPacket("shellies/announce", []byte(`{"id":"shellyplug-s-123456","model":"SHPLG-S"}`), now) {
		t.Fatal("expected shellies/announce to be handled")
	}
	if !m.handleDriverMQTTPacket("shellies/shellyplug-s-123456/online", []byte("true"), now) {
		t.Fatal("expected shelly online to be handled")
	}
	if !m.handleDriverMQTTPacket("shellies/shellyplug-s-123456/relay/0", []byte("on"), now) {
		t.Fatal("expected shelly relay/0 to be handled")
	}
	if !m.handleDriverMQTTPacket("shellies/shellyplug-s-123456/relay/0/power", []byte("142.5"), now) {
		t.Fatal("expected shelly power to be handled")
	}

	m.mu.RLock()
	plug := m.plugs["shelly1:shellyplug-s-123456"]
	discovered := m.discoveredListLocked()
	m.mu.RUnlock()

	if plug == nil {
		t.Fatal("expected shelly1 plug telemetry")
	}
	if plug.Model != "SHPLG-S" || plug.relayState(1) != "ON" || plug.PowerWatts == nil || *plug.PowerWatts != 142.5 {
		t.Fatalf("unexpected shelly1 telemetry: %+v", plug)
	}
	if len(discovered) != 1 || discovered[0].Driver != DriverShellyGen1 || discovered[0].Topic != "shellyplug-s-123456" {
		t.Fatalf("unexpected discovered list: %+v", discovered)
	}

	// Overpower protection
	m.handleDriverMQTTPacket("shellies/shellyplug-s-123456/relay/0", []byte("overpower"), now)
	m.mu.RLock()
	if plug.relayState(1) != "OFF" || !strings.Contains(plug.LastError, "overpower") {
		t.Fatalf("expected overpower to set OFF + error, got state=%q err=%q", plug.relayState(1), plug.LastError)
	}
	m.mu.RUnlock()
}

func TestShellyGen2MQTTDriver(t *testing.T) {
	m := newModule(nil)
	cfg := normalizeDeviceConfig(DeviceConfig{
		Driver:       DriverShellyGen2,
		TasmotaTopic: "shellyplusplugs-abc123",
		RelayIndex:   1,
	})
	m.mu.Lock()
	m.configs["dev-1"] = cfg
	m.mu.Unlock()

	now := time.Now()
	if !m.handleDriverMQTTPacket("shellyplusplugs-abc123/online", []byte("true"), now) {
		t.Fatal("expected shelly gen2 online to be handled")
	}
	if !m.handleDriverMQTTPacket("shellyplusplugs-abc123/status/switch:0", []byte(`{"id":0,"output":true,"apower":210.4,"voltage":230.2,"current":0.92}`), now) {
		t.Fatal("expected shelly gen2 status/switch:0 to be handled")
	}

	m.mu.RLock()
	plug := m.plugs[cfg.plugKey()]
	m.mu.RUnlock()
	if plug == nil || plug.relayState(1) != "ON" || plug.PowerWatts == nil || *plug.PowerWatts != 210.4 || plug.Voltage == nil || *plug.Voltage != 230.2 {
		t.Fatalf("unexpected shelly gen2 telemetry: %+v", plug)
	}
}

func TestZigbee2MQTTDriver(t *testing.T) {
	m := newModule(nil)
	cfg := normalizeDeviceConfig(DeviceConfig{
		Driver:       DriverZigbee2MQTT,
		TasmotaTopic: "printer_plug",
		RelayIndex:   2,
	})
	if cfg.TasmotaTopic != "zigbee2mqtt/printer_plug" {
		t.Fatalf("expected zigbee2mqtt/ prefix, got %q", cfg.TasmotaTopic)
	}
	m.mu.Lock()
	m.configs["dev-z2m"] = cfg
	m.mu.Unlock()

	now := time.Now()
	m.handleDriverMQTTPacket("zigbee2mqtt/printer_plug/availability", []byte(`{"state":"online"}`), now)
	m.handleDriverMQTTPacket("zigbee2mqtt/printer_plug", []byte(`{"state_l2":"ON","power":85.5,"voltage":231,"current":0.38}`), now)

	m.mu.RLock()
	plug := m.plugs[cfg.plugKey()]
	m.mu.RUnlock()
	if plug == nil || plug.Online == nil || !*plug.Online || plug.relayState(2) != "ON" || plug.PowerWatts == nil || *plug.PowerWatts != 85.5 {
		t.Fatalf("unexpected z2m telemetry: %+v", plug)
	}
}

func TestESPHomeAndOpenBekenDrivers(t *testing.T) {
	m := newModule(nil)
	espCfg := normalizeDeviceConfig(DeviceConfig{
		Driver:       DriverESPHome,
		TasmotaTopic: "prusa-plug/switch/relay",
	})
	obkCfg := normalizeDeviceConfig(DeviceConfig{
		Driver:       DriverOpenBeken,
		TasmotaTopic: "obk_printer",
		RelayIndex:   1,
	})
	m.mu.Lock()
	m.configs["dev-esp"] = espCfg
	m.configs["dev-obk"] = obkCfg
	m.mu.Unlock()

	now := time.Now()
	m.handleDriverMQTTPacket("prusa-plug/status", []byte("online"), now)
	m.handleDriverMQTTPacket("prusa-plug/switch/relay/state", []byte("ON"), now)

	m.handleDriverMQTTPacket("obk_printer/connected", []byte("online"), now)
	m.handleDriverMQTTPacket("obk_printer/0/get", []byte("1"), now)
	m.handleDriverMQTTPacket("obk_printer/power/get", []byte("64.2"), now)

	m.mu.RLock()
	espPlug := m.plugs[espCfg.plugKey()]
	obkPlug := m.plugs[obkCfg.plugKey()]
	m.mu.RUnlock()

	if espPlug == nil || espPlug.Online == nil || !*espPlug.Online || espPlug.relayState(1) != "ON" {
		t.Fatalf("unexpected esphome telemetry: %+v", espPlug)
	}
	if obkPlug == nil || obkPlug.Online == nil || !*obkPlug.Online || obkPlug.relayState(1) != "ON" || obkPlug.PowerWatts == nil || *obkPlug.PowerWatts != 64.2 {
		t.Fatalf("unexpected openbeken telemetry: %+v", obkPlug)
	}
}

func TestGenericMQTTDriver(t *testing.T) {
	m := newModule(nil)
	cfg := normalizeDeviceConfig(DeviceConfig{
		Driver:                DriverGenericMQTT,
		MQTTCommandTopic:      "home/printer/set",
		MQTTStateTopic:        "home/printer/state",
		MQTTAvailabilityTopic: "home/printer/avail",
		MQTTPayloadOn:         "TRUE",
		MQTTPayloadOff:        "FALSE",
		MQTTStateJSONKey:      "relay",
		MQTTPowerTopic:        "home/printer/telemetry",
		MQTTPowerJSONKey:      "watts",
	})
	m.mu.Lock()
	m.configs["dev-gen"] = cfg
	m.mu.Unlock()

	now := time.Now()
	m.handleDriverMQTTPacket("home/printer/avail", []byte("online"), now)
	m.handleDriverMQTTPacket("home/printer/state", []byte(`{"relay":"TRUE"}`), now)
	m.handleDriverMQTTPacket("home/printer/telemetry", []byte(`{"watts":123.4}`), now)

	m.mu.RLock()
	plug := m.plugs[cfg.plugKey()]
	m.mu.RUnlock()
	if plug == nil || plug.Online == nil || !*plug.Online || plug.relayState(1) != "ON" || plug.PowerWatts == nil || *plug.PowerWatts != 123.4 {
		t.Fatalf("unexpected generic mqtt telemetry: %+v", plug)
	}
}

// "nan" and "inf" parse as numbers, and are what some firmware sends for a
// sensor with no value. They are no reading.
func TestAReadingThatIsNotANumberIsNotTaken(t *testing.T) {
	for _, text := range []string{"nan", "NaN", "inf", "-Inf", "+Infinity", "1e999", "", "12 W", "unavailable"} {
		if v, ok := parseReading(text); ok {
			t.Errorf("parseReading(%q) = %v, want no reading", text, v)
		}
		if v, ok := extractFloat(text); ok {
			t.Errorf("extractFloat(%q) = %v, want no reading", text, v)
		}
	}
	if v, ok := parseReading(" 64.2 "); !ok || v != 64.2 {
		t.Errorf("parseReading of 64.2 = %v, %v", v, ok)
	}
	if v, ok := parseReading("-0.5"); !ok || v != -0.5 {
		t.Errorf("parseReading of -0.5 = %v, %v", v, ok)
	}

	m := newModule(nil)
	generic := normalizeDeviceConfig(DeviceConfig{
		Driver:           DriverGenericMQTT,
		MQTTCommandTopic: "home/printer/set",
		MQTTStateTopic:   "home/printer/state",
		MQTTPowerTopic:   "home/printer/power",
	})
	beken := normalizeDeviceConfig(DeviceConfig{Driver: DriverOpenBeken, TasmotaTopic: "obk_printer", RelayIndex: 1})
	m.mu.Lock()
	m.configs["dev-gen"] = generic
	m.configs["dev-obk"] = beken
	m.mu.Unlock()

	now := time.Now()
	for _, topic := range []string{"home/printer/power", "obk_printer/power/get", "obk_printer/voltage/get", "obk_printer/current/get"} {
		m.handleDriverMQTTPacket(topic, []byte("42.5"), now)
		for _, payload := range []string{"nan", "NaN", "inf", "-inf"} {
			m.handleDriverMQTTPacket(topic, []byte(payload), now)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	gen, obk := m.plugs[generic.plugKey()], m.plugs[beken.plugKey()]
	if gen == nil || gen.PowerWatts == nil || *gen.PowerWatts != 42.5 {
		t.Fatalf("the generic plug holds %+v, want the last number", gen)
	}
	if obk == nil || obk.PowerWatts == nil || *obk.PowerWatts != 42.5 || obk.Voltage == nil || *obk.Voltage != 42.5 || obk.CurrentAmps == nil || *obk.CurrentAmps != 42.5 {
		t.Fatalf("the OpenBeken plug holds %+v, want the last numbers", obk)
	}
	if _, err := json.Marshal([]*PlugTelemetry{gen, obk}); err != nil {
		t.Fatalf("the plugs cannot be answered as JSON: %v", err)
	}
}

func TestShellyHTTPGen2AndGen1Driver(t *testing.T) {
	// 1. Shelly Gen2+ RPC
	gen2On := false
	gen2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc/Switch.GetStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 0, "output": gen2On, "apower": 95.5, "voltage": 229.8, "current": 0.42,
			})
		case "/rpc/Switch.Set":
			wasOn := gen2On
			gen2On = r.URL.Query().Get("on") == "true"
			_ = json.NewEncoder(w).Encode(map[string]any{"was_on": wasOn})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gen2Srv.Close()

	m := newModule(nil)
	cfg2 := normalizeDeviceConfig(DeviceConfig{
		Driver:     DriverShellyHTTP,
		HTTPHost:   gen2Srv.URL,
		RelayIndex: 1,
	})
	if err := m.pollShellyHTTPPlug(context.Background(), cfg2); err != nil {
		t.Fatalf("poll gen2: %v", err)
	}
	if err := m.switchShellyHTTPPlugVerified(context.Background(), cfg2, true); err != nil {
		t.Fatalf("switch gen2 on: %v", err)
	}
	m.mu.RLock()
	p2 := m.plugs[cfg2.plugKey()]
	m.mu.RUnlock()
	if p2 == nil || p2.relayState(1) != "ON" || p2.PowerWatts == nil || *p2.PowerWatts != 95.5 {
		t.Fatalf("unexpected gen2 http telemetry: %+v", p2)
	}

	// 2. Shelly Gen1 REST fallback
	gen1On := false
	gen1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"relays": []map[string]any{{"ison": gen1On, "overpower": false}},
				"meters": []map[string]any{{"power": 48.0}},
			})
		case "/relay/0":
			gen1On = r.URL.Query().Get("turn") == "on"
			_ = json.NewEncoder(w).Encode(map[string]any{"ison": gen1On})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gen1Srv.Close()

	cfg1 := normalizeDeviceConfig(DeviceConfig{
		Driver:     DriverShellyHTTP,
		HTTPHost:   gen1Srv.URL,
		RelayIndex: 1,
	})
	if err := m.pollShellyHTTPPlug(context.Background(), cfg1); err != nil {
		t.Fatalf("poll gen1: %v", err)
	}
	if err := m.switchShellyHTTPPlugVerified(context.Background(), cfg1, true); err != nil {
		t.Fatalf("switch gen1 on: %v", err)
	}
	m.mu.RLock()
	p1 := m.plugs[cfg1.plugKey()]
	m.mu.RUnlock()
	if p1 == nil || p1.relayState(1) != "ON" || p1.PowerWatts == nil || *p1.PowerWatts != 48.0 {
		t.Fatalf("unexpected gen1 http telemetry: %+v", p1)
	}
}

func TestHomeAssistantDriver(t *testing.T) {
	swState := "off"
	haSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-ha-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/":
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "API running."})
		case "/api/states":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"entity_id": "switch.prusa_xl", "state": swState, "attributes": map[string]any{"friendly_name": "Prusa XL Plug"}},
				{"entity_id": "sensor.prusa_xl_power", "state": "118.5", "attributes": map[string]any{"friendly_name": "Prusa XL Power", "device_class": "power", "unit_of_measurement": "W"}},
				{"entity_id": "light.kitchen", "state": "on", "attributes": map[string]any{"friendly_name": "Kitchen"}},
			})
		case "/api/states/switch.prusa_xl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entity_id": "switch.prusa_xl", "state": swState, "attributes": map[string]any{"friendly_name": "Prusa XL Plug"},
			})
		case "/api/states/sensor.prusa_xl_power":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entity_id": "sensor.prusa_xl_power", "state": "118.5",
			})
		case "/api/services/homeassistant/turn_on":
			swState = "on"
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/services/homeassistant/turn_off":
			swState = "off"
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer haSrv.Close()

	m := newModule(nil)
	s := normalizeSettings(Settings{
		HAURL:   haSrv.URL,
		HAToken: "test-ha-token",
	})
	m.held.Store(&s)

	ok, _, entities, errMsg := m.testHomeAssistant(context.Background(), nil)
	if !ok || errMsg != "" || len(entities) != 2 {
		t.Fatalf("unexpected testHomeAssistant result: ok=%v err=%q entities=%+v", ok, errMsg, entities)
	}

	cfg := normalizeDeviceConfig(DeviceConfig{
		Driver:          DriverHomeAssistant,
		HAEntityID:      "switch.prusa_xl",
		HAPowerEntityID: "sensor.prusa_xl_power",
	})
	if err := m.pollHomeAssistantPlug(context.Background(), cfg); err != nil {
		t.Fatalf("poll HA: %v", err)
	}
	if err := m.switchHomeAssistantPlugVerified(context.Background(), cfg, true); err != nil {
		t.Fatalf("switch HA on: %v", err)
	}

	m.mu.RLock()
	plug := m.plugs[cfg.plugKey()]
	m.mu.RUnlock()
	if plug == nil || plug.relayState(1) != "ON" || plug.FriendlyName != "Prusa XL Plug" || plug.PowerWatts == nil || *plug.PowerWatts != 118.5 {
		t.Fatalf("unexpected HA plug telemetry: %+v", plug)
	}
}

func TestHomeAssistantPowerSensorUnitAndLoss(t *testing.T) {
	state, unit := "0.25", "kW"
	haSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/states/switch.plug":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "on"})
		case "/api/states/sensor.plug_power":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": state, "attributes": map[string]any{"unit_of_measurement": unit},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer haSrv.Close()

	m := newModule(nil)
	s := normalizeSettings(Settings{HAURL: haSrv.URL, HAToken: "t"})
	m.held.Store(&s)
	cfg := normalizeDeviceConfig(DeviceConfig{
		Driver:          DriverHomeAssistant,
		HAEntityID:      "switch.plug",
		HAPowerEntityID: "sensor.plug_power",
	})
	watts := func() *float64 {
		t.Helper()
		if err := m.pollHomeAssistantPlug(context.Background(), cfg); err != nil {
			t.Fatalf("poll HA: %v", err)
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.plugs[cfg.plugKey()].PowerWatts
	}

	if w := watts(); w == nil || *w != 250 {
		t.Fatalf("0.25 kW must read as 250 W, got %v", w)
	}
	state, unit = "1500", "mW"
	if w := watts(); w == nil || *w != 1.5 {
		t.Fatalf("1500 mW must read as 1.5 W, got %v", w)
	}
	for _, lost := range []string{"unavailable", "unknown"} {
		state, unit = "80", "W"
		if w := watts(); w == nil || *w != 80 {
			t.Fatalf("80 W must read as 80 W, got %v", w)
		}
		state = lost
		if w := watts(); w != nil {
			t.Fatalf("a sensor that is %s must leave no reading, got %v", lost, *w)
		}
	}
	state, unit = "3", "kWh"
	if w := watts(); w != nil {
		t.Fatalf("an energy sensor is no power reading, got %v", *w)
	}
}

func TestAllMQTTDriversEndToEndOverRealBroker(t *testing.T) {
	brk, err := StartEmbeddedBroker("127.0.0.1:0", "plugs", "secret")
	if err != nil {
		t.Fatalf("StartEmbeddedBroker: %v", err)
	}
	defer func() { _ = brk.Close() }()

	brokerURL := fmt.Sprintf("tcp://127.0.0.1:%d", brk.Port())

	// Simulated smart-plug responder connected to the real TCP MQTT broker.
	simClient := mqtt.NewClient(mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetUsername("plugs").
		SetPassword("secret").
		SetClientID("plug-simulator").
		SetConnectTimeout(2 * time.Second).
		SetAutoReconnect(false))
	if tok := simClient.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("simClient connect: %v", tok.Error())
	}
	defer simClient.Disconnect(50)

	type recordedCmd struct {
		Topic   string
		Payload string
	}
	var recMu sync.Mutex
	var recorded []recordedCmd
	record := func(topic, payload string) {
		recMu.Lock()
		recorded = append(recorded, recordedCmd{Topic: topic, Payload: payload})
		recMu.Unlock()
	}

	// Register virtual firmware handlers for all 6 MQTT drivers.
	simSubs := map[string]func(payload string){
		"shellies/shellyplug-sim/relay/0/command": func(p string) {
			record("shellies/shellyplug-sim/relay/0/command", p)
			simClient.Publish("shellies/shellyplug-sim/relay/0", 0, false, p)
			if p == "on" {
				simClient.Publish("shellies/shellyplug-sim/relay/0/power", 0, false, "115.2")
			} else {
				simClient.Publish("shellies/shellyplug-sim/relay/0/power", 0, false, "0")
			}
		},
		"shellyplus-sim/command/switch:0": func(p string) {
			record("shellyplus-sim/command/switch:0", p)
			on := p == "on"
			watts := 0.0
			if on {
				watts = 188.4
			}
			simClient.Publish("shellyplus-sim/status/switch:0", 0, false,
				fmt.Sprintf(`{"id":0,"output":%t,"apower":%.1f,"voltage":230.5,"current":0.82}`, on, watts))
		},
		"zigbee2mqtt/z2m_sim/set": func(p string) {
			record("zigbee2mqtt/z2m_sim/set", p)
			var req map[string]string
			if json.Unmarshal([]byte(p), &req) == nil {
				st := req["state"]
				watts := 0.0
				if st == "ON" {
					watts = 76.0
				}
				simClient.Publish("zigbee2mqtt/z2m_sim", 0, false,
					fmt.Sprintf(`{"state":"%s","power":%.1f}`, st, watts))
			}
		},
		"esphome-sim/switch/printer/command": func(p string) {
			record("esphome-sim/switch/printer/command", p)
			simClient.Publish("esphome-sim/switch/printer/state", 0, false, p)
		},
		"obk_sim/0/set": func(p string) {
			record("obk_sim/0/set", p)
			simClient.Publish("obk_sim/0/get", 0, false, p)
			if p == "1" {
				simClient.Publish("obk_sim/power/get", 0, false, "92.3")
			} else {
				simClient.Publish("obk_sim/power/get", 0, false, "0")
			}
		},
		"custom/plug/cmd": func(p string) {
			record("custom/plug/cmd", p)
			simClient.Publish("custom/plug/stat", 0, false, fmt.Sprintf(`{"relay":"%s"}`, p))
		},
	}

	for topic, handler := range simSubs {
		h := handler
		if tok := simClient.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			h(string(msg.Payload()))
		}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
			t.Fatalf("simClient subscribe %s: %v", topic, tok.Error())
		}
	}

	// Connect the real extension module to the embedded broker.
	m := newModule(nil)
	s := normalizeSettings(Settings{
		MQTTHost:     "127.0.0.1",
		MQTTPort:     brk.Port(),
		MQTTUsername: "plugs",
		MQTTPassword: "secret",
		MQTTClientID: "ext-e2e-sim",
	})
	modClient := mqtt.NewClient(m.buildMQTTOptions(s, brokerURL, "ext-e2e-sim").SetAutoReconnect(false))
	if tok := modClient.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("modClient connect: %v", tok.Error())
	}
	defer modClient.Disconnect(50)

	cases := []struct {
		name           string
		cfg            DeviceConfig
		wantTopic      string
		wantPayloadOn  string
		wantPayloadOff string
	}{
		{
			name:           "ShellyGen1",
			cfg:            normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen1, TasmotaTopic: "shellyplug-sim", RelayIndex: 1}),
			wantTopic:      "shellies/shellyplug-sim/relay/0/command",
			wantPayloadOn:  "on",
			wantPayloadOff: "off",
		},
		{
			name:           "ShellyGen2",
			cfg:            normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen2, TasmotaTopic: "shellyplus-sim", RelayIndex: 1}),
			wantTopic:      "shellyplus-sim/command/switch:0",
			wantPayloadOn:  "on",
			wantPayloadOff: "off",
		},
		{
			name:           "Zigbee2MQTT",
			cfg:            normalizeDeviceConfig(DeviceConfig{Driver: DriverZigbee2MQTT, TasmotaTopic: "z2m_sim", RelayIndex: 1}),
			wantTopic:      "zigbee2mqtt/z2m_sim/set",
			wantPayloadOn:  `{"state":"ON"}`,
			wantPayloadOff: `{"state":"OFF"}`,
		},
		{
			name:           "ESPHome",
			cfg:            normalizeDeviceConfig(DeviceConfig{Driver: DriverESPHome, TasmotaTopic: "esphome-sim/switch/printer"}),
			wantTopic:      "esphome-sim/switch/printer/command",
			wantPayloadOn:  "ON",
			wantPayloadOff: "OFF",
		},
		{
			name:           "OpenBeken",
			cfg:            normalizeDeviceConfig(DeviceConfig{Driver: DriverOpenBeken, TasmotaTopic: "obk_sim", RelayIndex: 1}),
			wantTopic:      "obk_sim/0/set",
			wantPayloadOn:  "1",
			wantPayloadOff: "0",
		},
		{
			name: "GenericMQTT",
			cfg: normalizeDeviceConfig(DeviceConfig{
				Driver:           DriverGenericMQTT,
				MQTTCommandTopic: "custom/plug/cmd",
				MQTTStateTopic:   "custom/plug/stat",
				MQTTPayloadOn:    "ENABLE",
				MQTTPayloadOff:   "DISABLE",
				MQTTStateJSONKey: "relay",
			}),
			wantTopic:      "custom/plug/cmd",
			wantPayloadOn:  "ENABLE",
			wantPayloadOff: "DISABLE",
		},
	}

	m.mu.Lock()
	m.mqttClient = modClient
	m.mqttUp = true
	for _, tc := range cases {
		m.configs["dev-"+tc.name] = tc.cfg
	}
	m.mu.Unlock()

	m.syncDriverSubscriptions()
	time.Sleep(150 * time.Millisecond)

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.switchDevicePower(ctx, tc.cfg, true); err != nil {
				t.Fatalf("%s ON failed: %v", tc.name, err)
			}
			m.mu.RLock()
			stOn := m.plugs[tc.cfg.plugKey()].relayState(tc.cfg.relayIndex())
			m.mu.RUnlock()
			if stOn != "ON" {
				t.Fatalf("%s state after ON = %q, want ON", tc.name, stOn)
			}

			if err := m.switchDevicePower(ctx, tc.cfg, false); err != nil {
				t.Fatalf("%s OFF failed: %v", tc.name, err)
			}
			m.mu.RLock()
			stOff := m.plugs[tc.cfg.plugKey()].relayState(tc.cfg.relayIndex())
			m.mu.RUnlock()
			if stOff != "OFF" {
				t.Fatalf("%s state after OFF = %q, want OFF", tc.name, stOff)
			}

			recMu.Lock()
			var sawOn, sawOff bool
			for _, c := range recorded {
				if c.Topic == tc.wantTopic && c.Payload == tc.wantPayloadOn {
					sawOn = true
				}
				if c.Topic == tc.wantTopic && c.Payload == tc.wantPayloadOff {
					sawOff = true
				}
			}
			recMu.Unlock()
			if !sawOn || !sawOff {
				t.Fatalf("%s did not receive expected commands on %s (sawOn=%v sawOff=%v)", tc.name, tc.wantTopic, sawOn, sawOff)
			}
			t.Logf("SIMULATED %s OK: %s received %q -> ON, %q -> OFF", tc.name, tc.wantTopic, tc.wantPayloadOn, tc.wantPayloadOff)
		})
	}
}

// --- Wi-Fi signal and the plug's own protection, Shelly ---

func TestShellyGen1ProtectionAndSignalOverMQTT(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	const key = "shelly1:shelly25-aabbcc"
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen1, TasmotaTopic: "shelly25-aabbcc", RelayIndex: 2})

	for _, sub := range []string{"shellies/shelly25-aabbcc/overtemperature", "shellies/shelly25-aabbcc/info"} {
		if !slices.Contains(cfg.driverSubscriptions(), sub) {
			t.Errorf("%s is not subscribed to: %v", sub, cfg.driverSubscriptions())
		}
	}

	// Overpower is one relay's own fault.
	m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/relay/0", []byte("on"), now)
	m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/relay/1", []byte("overpower"), now)
	if got := protectionOf(m, key, 2); got != protectOverpower {
		t.Fatalf("relay 2 after overpower: %q", got)
	}
	if got := protectionOf(m, key, 1); got != "" {
		t.Fatalf("relay 1 took the fault of relay 2: %q", got)
	}
	m.mu.RLock()
	shown := m.plugSnapshotLocked(cfg)
	m.mu.RUnlock()
	if shown.Protection != protectOverpower || shown.Power != "OFF" {
		t.Fatalf("the answer for relay 2: protection %q, power %q", shown.Protection, shown.Power)
	}
	m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/relay/1", []byte("off"), now)
	if got := protectionOf(m, key, 2); got != protectOverpower {
		t.Fatal("the fault went away while the relay is still off")
	}
	m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/relay/1", []byte("on"), now)
	if got := protectionOf(m, key, 2); got != "" {
		t.Fatalf("the fault stayed with the relay on again: %q", got)
	}

	// Too hot is the whole device.
	if !m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/overtemperature", []byte("1"), now) {
		t.Fatal("overtemperature was not handled")
	}
	if protectionOf(m, key, 1) != protectOvertemp || protectionOf(m, key, 2) != protectOvertemp {
		t.Fatalf("overtemperature is not on both relays: %q, %q", protectionOf(m, key, 1), protectionOf(m, key, 2))
	}
	m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/overtemperature", []byte("0"), now)
	if got := protectionOf(m, key, 1); got != "" {
		t.Fatalf("the fault stayed after it cooled down: %q", got)
	}

	if !m.handleDriverMQTTPacket("shellies/shelly25-aabbcc/info", []byte(`{"wifi_sta":{"connected":true,"ssid":"HomeNet","ip":"192.168.1.44","rssi":-71},"relays":[{"ison":true}]}`), now) {
		t.Fatal("info was not handled")
	}
	if got := wifiOf(m, key); got == nil || *got != -71 {
		t.Fatalf("signal from info = %v, want -71", got)
	}
}

func TestShellyGen2ProtectionAndSignalOverMQTT(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	first := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen2, TasmotaTopic: "shellypro2pm-aabbcc", RelayIndex: 1})
	second := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen2, TasmotaTopic: "shellypro2pm-aabbcc", RelayIndex: 2})
	m.configs["p1"], m.configs["p2"] = first, second
	key := first.plugKey()

	if !slices.Contains(first.driverSubscriptions(), "shellypro2pm-aabbcc/status/wifi") {
		t.Errorf("status/wifi is not subscribed to: %v", first.driverSubscriptions())
	}

	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:0", []byte(`{"id":0,"output":false,"apower":0,"errors":["overpower"]}`), now)
	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:1", []byte(`{"id":1,"output":true,"apower":80}`), now)
	if protectionOf(m, key, 1) != protectOverpower || protectionOf(m, key, 2) != "" {
		t.Fatalf("relay 1 %q, relay 2 %q", protectionOf(m, key, 1), protectionOf(m, key, 2))
	}

	// The gravest of several, and each relay keeps its own.
	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:1", []byte(`{"id":1,"output":false,"errors":["undervoltage","overtemp"]}`), now)
	if protectionOf(m, key, 1) != protectOverpower || protectionOf(m, key, 2) != protectOvertemp {
		t.Fatalf("relay 1 %q, relay 2 %q", protectionOf(m, key, 1), protectionOf(m, key, 2))
	}
	// A status of relay 2 with no error does not clear relay 1.
	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:1", []byte(`{"id":1,"output":true}`), now)
	if protectionOf(m, key, 1) != protectOverpower || protectionOf(m, key, 2) != "" {
		t.Fatalf("after relay 2 was well again: relay 1 %q, relay 2 %q", protectionOf(m, key, 1), protectionOf(m, key, 2))
	}
	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:0", []byte(`{"id":0,"output":true,"errors":[]}`), now)
	if got := protectionOf(m, key, 1); got != "" {
		t.Fatalf("relay 1 kept its fault: %q", got)
	}
	// An error this page has no word for is no protection fault.
	m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/switch:0", []byte(`{"id":0,"output":true,"errors":["power_meter_failure"]}`), now)
	if got := protectionOf(m, key, 1); got != "" {
		t.Fatalf("an unknown error is a fault: %q", got)
	}

	if !m.handleDriverMQTTPacket("shellypro2pm-aabbcc/status/wifi", []byte(`{"sta_ip":"192.168.1.45","status":"got ip","ssid":"HomeNet","rssi":-54}`), now) {
		t.Fatal("status/wifi was not handled")
	}
	if got := wifiOf(m, key); got == nil || *got != -54 {
		t.Fatalf("signal from status/wifi = %v, want -54", got)
	}
}

func TestShellyHTTPGen2ProtectionAndSignal(t *testing.T) {
	var mu sync.Mutex
	faults := []string{"overvoltage"}
	wifiAsked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/rpc/Switch.GetStatus":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 0, "output": false, "apower": 0.0, "errors": faults})
		case "/rpc/Wifi.GetStatus":
			wifiAsked++
			_ = json.NewEncoder(w).Encode(map[string]any{"sta_ip": "192.168.1.46", "ssid": "HomeNet", "rssi": -62})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m, clock := clockedModule(energyStart)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: srv.URL, RelayIndex: 1})
	asked := func() int {
		mu.Lock()
		defer mu.Unlock()
		return wifiAsked
	}

	for range 5 {
		if err := m.pollShellyHTTPPlug(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		clock.add(5 * time.Second)
	}
	if got := protectionOf(m, cfg.plugKey(), 1); got != protectOvervoltage {
		t.Fatalf("protection = %q, want overvoltage", got)
	}
	if got := wifiOf(m, cfg.plugKey()); got == nil || *got != -62 {
		t.Fatalf("signal = %v, want -62", got)
	}
	if asked() != 1 {
		t.Fatalf("five polls in 25 seconds asked for the signal %d times, want once", asked())
	}

	clock.add(time.Minute)
	mu.Lock()
	faults = nil
	mu.Unlock()
	for range 3 {
		if err := m.pollShellyHTTPPlug(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if asked() != 2 {
		t.Fatalf("after a minute the signal was asked for %d times in all, want 2", asked())
	}
	if got := protectionOf(m, cfg.plugKey(), 1); got != "" {
		t.Fatalf("the fault stayed after the plug was well again: %q", got)
	}
}

func TestShellyHTTPGen1ProtectionAndSignal(t *testing.T) {
	var mu sync.Mutex
	hot, over := true, true
	requests := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests[r.URL.Path]++
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"wifi_sta":        map[string]any{"connected": true, "ssid": "HomeNet", "ip": "192.168.1.47", "rssi": -77},
			"relays":          []map[string]any{{"ison": false, "overpower": over}},
			"meters":          []map[string]any{{"power": 0.0}},
			"overtemperature": hot,
		})
	}))
	defer srv.Close()

	m := newModule(nil)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: srv.URL, RelayIndex: 1})
	if err := m.pollShellyHTTPPlug(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// The relay's own fault is told before the device's.
	if got := protectionOf(m, cfg.plugKey(), 1); got != protectOverpower {
		t.Fatalf("protection = %q, want overpower", got)
	}
	if got := protectionOf(m, cfg.plugKey(), 2); got != protectOvertemp {
		t.Fatalf("the device's fault = %q, want overtemp", got)
	}
	if got := wifiOf(m, cfg.plugKey()); got == nil || *got != -77 {
		t.Fatalf("signal = %v, want -77", got)
	}

	mu.Lock()
	hot, over = false, false
	mu.Unlock()
	if err := m.pollShellyHTTPPlug(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if protectionOf(m, cfg.plugKey(), 1) != "" || protectionOf(m, cfg.plugKey(), 2) != "" {
		t.Fatal("the faults stayed after the plug was well again")
	}

	// A first-generation Shelly is asked nothing new for the signal: it is
	// in the status it is read from anyway. Its settings are asked for once.
	mu.Lock()
	defer mu.Unlock()
	if requests["/settings/relay/0"] != 1 {
		t.Errorf("two polls asked for the settings %d times, want once", requests["/settings/relay/0"])
	}
	for path := range requests {
		if path != "/status" && path != "/rpc/Switch.GetStatus" && path != "/settings/relay/0" {
			t.Errorf("the plug was asked %s", path)
		}
	}
}

func TestASignalOutOfRangeIsNotTaken(t *testing.T) {
	plug := &PlugTelemetry{}
	for _, bad := range []float64{1, 31, 100, -121, -1000, math.NaN(), math.Inf(-1)} {
		plug.setWifi(bad)
		if plug.WifiDBm != nil {
			t.Fatalf("%v was taken as a signal: %d", bad, *plug.WifiDBm)
		}
	}
	for in, want := range map[float64]int{0: 0, -120: -120, -67.4: -67, -67.6: -68} {
		plug.setWifi(in)
		if plug.WifiDBm == nil || *plug.WifiDBm != want {
			t.Fatalf("%v gave %v, want %d", in, plug.WifiDBm, want)
		}
	}
	// A value that is not taken leaves the last one.
	plug.setWifi(-50)
	plug.setWifi(55)
	if *plug.WifiDBm != -50 {
		t.Fatalf("a bad value changed the signal to %d", *plug.WifiDBm)
	}

	// Tasmota: a quality outside 0 to 100 is no reading.
	m := newModule(nil)
	m.handleMQTTPacket("tele/plug1/STATE", []byte(`{"POWER":"ON","Wifi":{"RSSI":250}}`), time.Now())
	m.handleMQTTPacket("tele/plug1/STATE", []byte(`{"POWER":"ON","Wifi":{"Signal":20}}`), time.Now())
	m.handleMQTTPacket("tele/plug1/STATE", []byte(`{"POWER":"ON","Wifi":"none"}`), time.Now())
	if got := wifiOf(m, "plug1"); got != nil {
		t.Fatalf("a signal out of range was taken: %d", *got)
	}
}

// The name of the Wi-Fi network, its access point and the plug's address in
// it are in what the plugs send. None of them is kept, so none can be
// answered.
func TestNoSSIDReachesAnAnswer(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	tasmota := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug1"})
	gen1 := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen1, TasmotaTopic: "shelly1-aabbcc"})
	gen2 := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyGen2, TasmotaTopic: "shellyplus-aabbcc"})
	m.configs["p1"], m.configs["p2"], m.configs["p3"] = tasmota, gen1, gen2

	m.handleMQTTPacket("tele/plug1/STATE", []byte(`{"POWER":"ON","Wifi":{"AP":1,"SSId":"SecretNet","BSSId":"DE:AD:BE:EF:00:01","Channel":6,"RSSI":74,"Signal":-63}}`), now)
	m.handleDriverMQTTPacket("shellies/shelly1-aabbcc/info", []byte(`{"wifi_sta":{"connected":true,"ssid":"SecretNet","ip":"10.9.8.7","rssi":-71}}`), now)
	m.handleDriverMQTTPacket("shellyplus-aabbcc/status/wifi", []byte(`{"sta_ip":"10.9.8.6","status":"got ip","ssid":"SecretNet","rssi":-54}`), now)

	overview, err := json.Marshal(m.buildOverview(overviewOptions{visible: func(string) bool { return true }}))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	found, err := json.Marshal(m.discoveredListLocked())
	m.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	answers := string(overview) + string(found) + m.collectDiagnostics("").Files[0].Text

	for _, private := range []string{"SecretNet", "DE:AD:BE", "10.9.8.7", "10.9.8.6", "ssid", "SSId", "BSSId"} {
		if strings.Contains(answers, private) {
			t.Errorf("an answer holds %q", private)
		}
	}
	if got := strings.Count(string(overview), `"wifi_dbm"`); got != 3 {
		t.Errorf("the overview holds the signal of %d plugs, want 3: %s", got, overview)
	}
}

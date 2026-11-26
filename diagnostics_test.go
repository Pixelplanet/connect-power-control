package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/diagnosticsapi"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// hostBlanked is a key the host's own filter takes for a secret.
var hostBlanked = regexp.MustCompile(`(?i)"[A-Za-z0-9_-]*(?:password|passwd|pwd|token|secret|api[_-]?key)"\s*:`)

// diagText is the text of the one file the module adds to the bundle.
func diagText(t *testing.T, m *module) string {
	t.Helper()
	page := m.collectDiagnostics("")
	if err := page.Validate(); err != nil {
		t.Fatalf("the page is not valid: %v", err)
	}
	if len(page.Files) != 1 || page.Files[0].Name != diagFileName {
		t.Fatalf("files = %+v, want one %s", page.Files, diagFileName)
	}
	return page.Files[0].Text
}

func TestTheDiagnosticsFileNamesNoSecretAndNoAddress(t *testing.T) {
	m, _ := clockedModule(energyStart)
	s := normalizeSettings(Settings{
		MQTTHost: "mqtt.internal.example", MQTTPort: 8883, MQTTTLS: true,
		MQTTUsername: "brokeruser", MQTTPassword: "brokerpass123", MQTTClientID: "client-of-the-farm",
		HAURL: "http://homeassistant.local:8123", HAToken: "eyJ0eXAi.long.lived.token",
		IdleOffMin: 120,
	})
	m.held.Store(&s)
	m.setHostConfig(hostSettings{HTTPHost: "suggested-plug.lan"}, wirerpc.Grants{})

	own := 45
	byMQTT := seedPlug(m, DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "workshop_plug_7", AutoOffAfterPrint: true}, "ON", 61)
	byAddress := seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: "192.168.77.21", HTTPUser: "plugadmin", HTTPPassword: "plugpass456", IdleOffMin: &own}, "ON", 5)
	shelly := seedPlug(m, DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: "shellyplug-kitchen.local", HTTPPassword: "shellypass789"}, "OFF", 0)
	viaHA := seedPlug(m, DeviceConfig{Driver: DriverHomeAssistant, HAEntityID: "switch.garage_printer", HAPowerEntityID: "sensor.garage_printer_power"}, "ON", 12)
	m.configs["printer-a"], m.configs["printer-b"], m.configs["printer-c"], m.configs["printer-d"] = byMQTT, byAddress, shelly, viaHA
	// A printer with no plug is not listed.
	m.configs["printer-e"] = DeviceConfig{AutoOffDefault: true}

	offline := false
	signal := -67
	m.mu.Lock()
	m.mqttBroker = "ssl://mqtt.internal.example:8883"
	m.mqttError = "dial tcp: lookup mqtt.internal.example on 10.0.0.53:53: no such host"
	m.mqttWarning = "plugs must reach mqtt.internal.example:8883 with brokerpass123"
	m.plugs[byMQTT.plugKey()].FriendlyName = "Bench plug of Alex"
	m.plugs[byMQTT.plugKey()].Model = "NOUS A1T"
	m.plugs[byMQTT.plugKey()].MAC = "AA:BB:CC:00:11:22"
	m.plugs[byMQTT.plugKey()].WifiDBm = &signal
	m.plugs[byAddress.plugKey()].Online = &offline
	// What an error text looked like before the password was taken out of it.
	m.plugs[byAddress.plugKey()].LastError = `Get "http://192.168.77.21/cm?cmnd=Status+0&password=plugpass456&user=plugadmin": dial tcp 192.168.77.21:80: connect: connection refused`
	m.plugs[shelly.plugKey()].LastError = "dial tcp: lookup ShellyPlug-Kitchen.local: no such host"
	m.plugs[shelly.plugKey()].setProtection(1, protectOvertemp)
	m.plugs[viaHA.plugKey()].LastError = "Get \"http://homeassistant.local:8123/api/states/switch.garage_printer\": context deadline exceeded\nBearer eyJ0eXAi.long.lived.token"
	m.trackers["printer-a"] = &autoOffTracker{sawActive: true, jobID: "job-41", offFault: "mqtt publish to mqtt.internal.example failed for 192.168.77.99"}
	m.idle["printer-b"] = &idleClock{since: energyStart.Add(-time.Minute), fault: "plug at 192.168.77.21 said no to plugpass456"}
	m.mu.Unlock()

	text := diagText(t, m)

	for _, secret := range []string{
		"brokerpass123", "plugpass456", "shellypass789", "eyJ0eXAi", "long.lived",
		"mqtt.internal.example", "192.168.77.21", "192.168.77.99", "10.0.0.53", "shellyplug-kitchen", "ShellyPlug-Kitchen",
		"homeassistant.local", "suggested-plug", ":8883", ":8123", "://",
		"workshop_plug_7", "switch.garage_printer", "sensor.garage_printer_power", "garage_printer",
		"brokeruser", "plugadmin", "client-of-the-farm",
		"Bench plug", "Alex", "NOUS", "AA:BB:CC",
		"password=", "user=", "cmnd", "Bearer ey",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("the file names %q", secret)
		}
	}
	if t.Failed() {
		t.Logf("the file:\n%s", text)
	}

	for _, kept := range []string{
		"connection refused", "no such host", "context deadline exceeded",
		`"driver": "tasmota"`, `"driver": "tasmota_http"`, `"driver": "shelly_http"`, `"driver": "homeassistant"`,
		`"transport": "mqtt"`, `"transport": "http"`, `"transport": "home_assistant"`,
		`"address_kind": "ipv4"`, `"address_kind": "mdns"`, `"address_kind": "hostname"`,
		`"mode": "external"`, `"tls": true`, `"has_user": true`, `"auth_set": true`,
		`"wifi_dbm": -67`, `"protection": "overtemp"`, `"auto_off_state": "failed"`,
		`"idle_off_min": 45`, `"idle_off_min": 120`, `"idle_off_pending": true`,
		`"plugs_total": 4`, `"device_id": "printer-a"`, `"version": "` + string(release) + `"`, `"events": "on"`,
	} {
		if !strings.Contains(text, kept) {
			t.Errorf("the file does not hold %s", kept)
		}
	}

	var report diagReport
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatalf("the file does not parse: %v", err)
	}
	if len(report.Plugs) != 4 || report.Plugs[1].DeviceID != "printer-b" {
		t.Fatalf("plugs = %+v", report.Plugs)
	}
	b := report.Plugs[1]
	if b.Online == nil || *b.Online || b.Power != "ON" || !b.AuthSet || !b.HasMeter || !b.Seen || b.IdleOffError == "" {
		t.Fatalf("printer-b = %+v", b)
	}
	if !report.Broker.AuthSet || !report.HomeAssistant.AuthSet || report.Plugs[0].AuthSet {
		t.Fatalf("auth_set: broker %v, Home Assistant %v, a plug with no password %v", report.Broker.AuthSet, report.HomeAssistant.AuthSet, report.Plugs[0].AuthSet)
	}
	// The host blanks the value of a key with one of these words in its
	// name, which would leave a file that is no JSON.
	if key := hostBlanked.FindString(text); key != "" {
		t.Fatalf("the host would blank the value of %s", key)
	}
	if report.GeneratedAt != energyStart.Format(time.RFC3339) {
		t.Fatalf("generated_at = %q", report.GeneratedAt)
	}
}

func TestTheDiagnosticsPageIsValid(t *testing.T) {
	m := newModule(nil)

	text := diagText(t, m)
	if !strings.Contains(text, `"plugs": []`) || !strings.Contains(text, `"mode": "none"`) {
		t.Fatalf("an install with nothing set up answers:\n%s", text)
	}
	if !strings.HasSuffix(text, "}\n") || !json.Valid([]byte(text)) {
		t.Fatalf("the file is not JSON with a line end:\n%s", text)
	}
	if !diagnosticsapi.ValidName(diagFileName) {
		t.Fatalf("%s is no name the host takes", diagFileName)
	}

	// The host asks again with the cursor of the page before. There is one page.
	more := m.collectDiagnostics("2")
	if err := more.Validate(); err != nil || len(more.Files) != 0 || more.Cursor != "" {
		t.Fatalf("a second page answers %+v (%v)", more, err)
	}
	if first := m.collectDiagnostics(""); first.Cursor != "" {
		t.Fatalf("the first page asks for a second with the cursor %q", first.Cursor)
	}
}

func TestTheDiagnosticsFileIsBounded(t *testing.T) {
	long := strings.Repeat("the plug said no and went on about it ", 40)
	for name, fault := range map[string]string{"plugs that are well": "", "plugs with long errors": long} {
		m := newModule(nil)
		for i := range 400 {
			cfg := seedPlug(m, DeviceConfig{Driver: DriverTasmota, TasmotaTopic: fmt.Sprintf("plug_%03d", i), AutoOffAfterPrint: true}, "ON", 10)
			id := fmt.Sprintf("printer-%03d", i)
			m.configs[id] = cfg
			m.plugs[cfg.plugKey()].LastError = fault
			m.trackers[id] = &autoOffTracker{offFault: fault}
			m.idle[id] = &idleClock{fault: fault}
		}
		s := normalizeSettings(Settings{IdleOffMin: 60})
		m.held.Store(&s)

		text := diagText(t, m)
		if len(text) > diagnosticsapi.MaxPageBytes {
			t.Fatalf("%s: the file is %d bytes", name, len(text))
		}
		var report diagReport
		if err := json.Unmarshal([]byte(text), &report); err != nil {
			t.Fatalf("%s: the file does not parse: %v", name, err)
		}
		listed := len(report.Plugs)
		if report.PlugsTotal != 400 || listed == 0 || listed > maxDiagPlugs {
			t.Fatalf("%s: %d plugs of %d are listed, want at most %d of 400", name, listed, report.PlugsTotal, maxDiagPlugs)
		}
		if fault == "" && listed != maxDiagPlugs {
			t.Fatalf("%s: %d plugs are listed, want %d", name, listed, maxDiagPlugs)
		}
		if report.Plugs[0].DeviceID != "printer-000" || report.Plugs[listed-1].DeviceID != fmt.Sprintf("printer-%03d", listed-1) {
			t.Fatalf("%s: the plugs are not in order: %s to %s", name, report.Plugs[0].DeviceID, report.Plugs[listed-1].DeviceID)
		}
		if got := len(report.Plugs[0].LastError); got > maxScrubbedBytes {
			t.Fatalf("%s: an error text of %d bytes is in the file", name, got)
		}
	}
}

func TestAutoOffStateOf(t *testing.T) {
	armed := DeviceConfig{AutoOffAfterPrint: true}
	at := time.Now()

	for _, tc := range []struct {
		name string
		cfg  DeviceConfig
		tr   *autoOffTracker
		want string
	}{
		{"not armed", DeviceConfig{}, &autoOffTracker{sawActive: true}, "off"},
		{"armed, nothing seen", armed, nil, "armed"},
		{"armed, after a turn-off", armed, &autoOffTracker{fired: true}, "armed"},
		{"printing", armed, &autoOffTracker{sawActive: true}, "printing"},
		{"waiting", armed, &autoOffTracker{sawActive: true, finishedAt: &at}, "waiting"},
		{"failed", armed, &autoOffTracker{sawActive: true, finishedAt: &at, offFault: "no"}, "failed"},
		{"kept on", armed, &autoOffTracker{fired: true, kept: "cancelled"}, "kept_on"},
	} {
		if got := autoOffStateOf(tc.cfg, tc.tr); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAddressKind(t *testing.T) {
	for host, want := range map[string]string{
		"":                           "",
		"   ":                        "",
		"192.168.1.20":               "ipv4",
		"192.168.1.20:8080":          "ipv4",
		"https://192.168.1.20":       "ipv4",
		"fe80::1":                    "ipv6",
		"[fe80::1]:80":               "ipv6",
		"http://[2001:db8::7]/x":     "ipv6",
		"shellyplug.local":           "mdns",
		"Tasmota-1234.LOCAL:80":      "mdns",
		"plug.lan":                   "hostname",
		"https://ha.example.org:443": "hostname",
		"user:pw@broker.example.org": "hostname",
		"homeassistant":              "hostname",
	} {
		if got := addressKind(host); got != want {
			t.Errorf("addressKind(%q) = %q, want %q", host, got, want)
		}
	}
}

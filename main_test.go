package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func TestNormalizeTopicStripsPrefixAndPowerSuffix(t *testing.T) {
	cases := map[string]string{
		"tasmota_FA3578":             "tasmota_FA3578",
		"/tasmota_FA3578/":           "tasmota_FA3578",
		"cmnd/tasmota_FA3578/Power":  "tasmota_FA3578",
		"cmnd/tasmota_FA3578/POWER2": "tasmota_FA3578",
		"stat/Entertainment_Center/": "Entertainment_Center",
	}
	for in, want := range cases {
		if got := normalizeTopic(in); got != want {
			t.Errorf("normalizeTopic(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTasmotaDiscoveryAndMultiRelay(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	discPayload := `{
		"ip":"192.168.178.14",
		"dn":"Entertainment Center",
		"fn":["Printer 1","Printer 2",null,null],
		"hn":"tasmota-14",
		"mac":"AABBCCDDEEFF",
		"md":"NOUS A1T",
		"t":"Entertainment_Center",
		"ft":"%prefix%/%topic%/",
		"tp":["cmnd","stat","tele"],
		"rl":[1,1,0,0,0,0,0,0],
		"state":["OFF","ON","TOGGLE","HOLD"],
		"onln":"Online",
		"ofln":"Offline"
	}`

	customSubs, queryTopic := m.handleMQTTPacket("tasmota/discovery/AABBCCDDEEFF/config", []byte(discPayload), now)
	if len(customSubs) != 0 {
		t.Fatalf("expected no custom subs for default FullTopic, got %v", customSubs)
	}
	if queryTopic != "Entertainment_Center" {
		t.Fatalf("expected queryTopic Entertainment_Center, got %q", queryTopic)
	}

	m.mu.RLock()
	plug := m.plugs["Entertainment_Center"]
	m.mu.RUnlock()
	if plug == nil {
		t.Fatal("expected discovered plug to be stored")
	}
	if plug.FriendlyName != "Printer 1" {
		t.Errorf("FriendlyName = %q, want Printer 1", plug.FriendlyName)
	}
	if plug.Model != "NOUS A1T" {
		t.Errorf("Model = %q, want NOUS A1T", plug.Model)
	}
	if plug.RelayCount != 2 {
		t.Errorf("RelayCount = %d, want 2", plug.RelayCount)
	}
	if len(plug.RelayNames) != 2 || plug.RelayNames[1] != "Printer 2" {
		t.Errorf("RelayNames = %v, want [Printer 1 Printer 2]", plug.RelayNames)
	}
}

func TestTasmotaSingleAndSetOption26(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER":"ON"}`), now)
	m.mu.RLock()
	st1 := m.plugs["plug1"].relayState(1)
	m.mu.RUnlock()
	if st1 != "ON" {
		t.Fatalf("relay 1 after POWER=ON got %q, want ON", st1)
	}

	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER1":"OFF"}`), now)
	m.mu.RLock()
	st1 = m.plugs["plug1"].relayState(1)
	powerField := m.plugs["plug1"].Power
	m.mu.RUnlock()
	if st1 != "OFF" || powerField != "OFF" {
		t.Fatalf("relay 1 after SetOption26 POWER1=OFF got (%q, %q), want OFF", st1, powerField)
	}
}

func TestTasmotaMultiRelayIsolation(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	m.handleMQTTPacket("stat/strip1/RESULT", []byte(`{"POWER1":"ON"}`), now)
	m.handleMQTTPacket("stat/strip1/POWER2", []byte(`OFF`), now)
	// Ensure Status.Power bitstring never corrupts relay state.
	m.handleMQTTPacket("stat/strip1/STATUS", []byte(`{"Status":{"Power":"00"}}`), now)

	m.mu.RLock()
	p := m.plugs["strip1"]
	r1 := p.relayState(1)
	r2 := p.relayState(2)
	m.mu.RUnlock()

	if r1 != "ON" {
		t.Errorf("relay 1 = %q, want ON", r1)
	}
	if r2 != "OFF" {
		t.Errorf("relay 2 = %q, want OFF", r2)
	}
}

func TestTasmotaSetOption4AndSetOption90(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	// SetOption4 1 puts JSON on stat/<topic>/POWER1 directly.
	m.handleMQTTPacket("stat/so4plug/POWER1", []byte(`{"POWER1":"ON"}`), now)
	// SetOption90 1 sends only RESULT JSON.
	m.handleMQTTPacket("stat/so4plug/RESULT", []byte(`{"POWER1":"OFF"}`), now)
	// SetOption4 1 single-relay default sends JSON then plain text on POWER.
	m.handleMQTTPacket("stat/so4plug/POWER", []byte(`{"POWER":"ON"}`), now)

	m.mu.RLock()
	got := m.plugs["so4plug"].relayState(1)
	m.mu.RUnlock()
	if got != "ON" {
		t.Fatalf("expected ON, got %q", got)
	}
}

func TestTasmotaCustomStateTextAndEmptyPayload(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	disc := `{"t":"deplug","ft":"%prefix%/%topic%/","tp":["cmnd","stat","tele"],"rl":[1],"state":["aus","an","TOGGLE","HOLD"]}`
	m.handleMQTTPacket("tasmota/discovery/112233445566/config", []byte(disc), now)

	m.handleMQTTPacket("stat/deplug/RESULT", []byte(`{"POWER":"an"}`), now)
	m.mu.RLock()
	if got := m.plugs["deplug"].relayState(1); got != "ON" {
		m.mu.RUnlock()
		t.Fatalf("custom StateText 'an' -> %q, want ON", got)
	}
	m.mu.RUnlock()

	// Empty payload from PowerRetain 0 must be ignored and keep ON.
	m.handleMQTTPacket("stat/deplug/POWER", []byte(""), now)
	m.mu.RLock()
	if got := m.plugs["deplug"].relayState(1); got != "ON" {
		m.mu.RUnlock()
		t.Fatalf("empty payload changed state to %q, want ON", got)
	}
	m.mu.RUnlock()

	m.handleMQTTPacket("stat/deplug/POWER1", []byte("aus"), now)
	m.mu.RLock()
	if got := m.plugs["deplug"].relayState(1); got != "OFF" {
		m.mu.RUnlock()
		t.Fatalf("custom StateText 'aus' -> %q, want OFF", got)
	}
	m.mu.RUnlock()
}

func TestTasmotaCustomFullTopicAndStaleLWT(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	// Stale retained Offline on old default LWT topic arrives first.
	m.handleMQTTPacket("tele/Entertainment_Center/LWT", []byte("Offline"), now)

	// Discovery announces custom FullTopic & Prefix2=state, Prefix3=telem.
	disc := `{
		"dn":"Entertainment Center",
		"t":"Entertainment_Center",
		"ft":"devintest/%topic%/%prefix%/",
		"tp":["cmnd","state","telem"],
		"rl":[1],
		"onln":"Connected",
		"ofln":"Disconnected"
	}`
	customSubs, _ := m.handleMQTTPacket("tasmota/discovery/AABBCCDDEEFF/config", []byte(disc), now)
	if len(customSubs) != 2 || customSubs[0] != "devintest/Entertainment_Center/state/+" || customSubs[1] != "devintest/Entertainment_Center/telem/+" {
		t.Fatalf("unexpected customSubs: %v", customSubs)
	}

	// Verify command topic resolution.
	m.mu.RLock()
	plug := m.plugs["Entertainment_Center"]
	cmdTopic := plug.formatTopic(0, "POWER1")
	m.mu.RUnlock()
	if cmdTopic != "devintest/Entertainment_Center/cmnd/POWER1" {
		t.Fatalf("formatTopic = %q", cmdTopic)
	}

	// Custom LWT Online sets Online=true and asks to query state.
	_, qt := m.handleMQTTPacket("devintest/Entertainment_Center/telem/LWT", []byte("Connected"), now)
	if qt != "Entertainment_Center" {
		t.Fatalf("expected queryTopic on LWT Online transition, got %q", qt)
	}

	// Subsequent stale Offline on old tele/<topic>/LWT must be ignored.
	m.handleMQTTPacket("tele/Entertainment_Center/LWT", []byte("Offline"), now)

	// State message arrives under custom layout.
	m.handleMQTTPacket("devintest/Entertainment_Center/state/RESULT", []byte(`{"POWER":"ON"}`), now)

	m.mu.RLock()
	online := m.plugs["Entertainment_Center"].Online
	power := m.plugs["Entertainment_Center"].relayState(1)
	m.mu.RUnlock()
	if online == nil || !*online {
		t.Fatalf("expected Online=true (stale LWT ignored), got %v", online)
	}
	if power != "ON" {
		t.Fatalf("expected power=ON, got %q", power)
	}
}

func TestTasmotaCommandErrorWaiterAndEnergyStatus(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	waiter := &stateWaiter{
		topic: "plugX",
		relay: 3,
		ch:    make(chan stateEvent, 4),
	}
	m.mu.Lock()
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()
	defer m.removeWaiter(waiter)

	m.handleMQTTPacket("stat/plugX/RESULT", []byte(`{"Command":"Error","Input":"POWER3 1"}`), now)

	select {
	case evt := <-waiter.ch:
		if evt.Err == "" {
			t.Fatal("expected non-empty error event on Command: Error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for Command: Error waiter notification")
	}

	m.handleMQTTPacket("stat/plugX/STATUS10", []byte(`{"StatusSNS":{"ENERGY":{"Power":124.5,"Voltage":231,"Current":0.54}}}`), now)
	m.handleMQTTPacket("stat/plugX/STATUS11", []byte(`{"StatusSTS":{"POWER":"ON"}}`), now)

	m.mu.RLock()
	plug := m.plugs["plugX"]
	m.mu.RUnlock()
	if plug.PowerWatts == nil || *plug.PowerWatts != 124.5 {
		t.Errorf("PowerWatts = %v, want 124.5", plug.PowerWatts)
	}
	if plug.relayState(1) != "ON" {
		t.Errorf("relay 1 after STATUS11 = %q, want ON", plug.relayState(1))
	}
}

func TestEmbeddedMQTTBrokerEndToEnd(t *testing.T) {
	brk, err := StartEmbeddedBroker("127.0.0.1:0", "prusa", "secret")
	if err != nil {
		t.Fatalf("StartEmbeddedBroker: %v", err)
	}
	defer func() { _ = brk.Close() }()

	brokerURL := fmt.Sprintf("tcp://127.0.0.1:%d", brk.Port())

	// Wrong credentials must be rejected.
	badOpts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID("bad-client").
		SetUsername("prusa").
		SetPassword("wrong").
		SetConnectTimeout(2 * time.Second).
		SetAutoReconnect(false)
	badClient := mqtt.NewClient(badOpts)
	if tok := badClient.Connect(); tok.WaitTimeout(3*time.Second) && tok.Error() == nil {
		badClient.Disconnect(50)
		t.Fatal("expected authentication error for wrong password, got nil")
	}

	// Simulate a smart plug publishing retained LWT and answering cmnd/+/POWER1.
	plugOpts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID("tasmota-sim").
		SetUsername("prusa").
		SetPassword("secret").
		SetConnectTimeout(2 * time.Second).
		SetAutoReconnect(false)
	plugClient := mqtt.NewClient(plugOpts)
	if tok := plugClient.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("plugClient connect failed: %v", tok.Error())
	}
	defer plugClient.Disconnect(50)

	if tok := plugClient.Publish("tele/ec_plug/LWT", 1, true, "Online"); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("publish retained LWT failed: %v", tok.Error())
	}

	// Extension module connects using module client and switches plug via embedded broker.
	m := newModule(nil)
	s := normalizeSettings(Settings{
		MQTTBuiltin:  true,
		MQTTHost:     "127.0.0.1",
		MQTTPort:     brk.Port(),
		MQTTUsername: "prusa",
		MQTTPassword: "secret",
		MQTTClientID: "ext-test",
	})
	modOpts := m.buildMQTTOptions(s, brokerURL, "ext-test").SetAutoReconnect(false)
	modClient := mqtt.NewClient(modOpts)
	if tok := modClient.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("modClient connect failed: %v", tok.Error())
	}
	defer modClient.Disconnect(50)

	m.mu.Lock()
	m.mqttClient = modClient
	m.mqttUp = true
	m.mu.Unlock()

	lwtCh := make(chan string, 1)
	if tok := modClient.Subscribe("tele/+/LWT", 0, func(c mqtt.Client, msg mqtt.Message) {
		m.onMQTTMessage(c, msg)
		select {
		case lwtCh <- string(msg.Payload()):
		default:
		}
	}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe LWT failed: %v", tok.Error())
	}
	if tok := modClient.Subscribe("stat/+/+", 0, m.onMQTTMessage); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe stat failed: %v", tok.Error())
	}

	// Retained Online LWT must arrive immediately on Subscribe.
	select {
	case got := <-lwtCh:
		if got != "Online" {
			t.Fatalf("retained LWT = %q, want Online", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retained LWT delivery")
	}

	if got := brk.ClientCount(); got != 2 {
		t.Errorf("ClientCount() = %d, want 2", got)
	}
	if brk.AcceptedCount() < 2 {
		t.Errorf("AcceptedCount() = %d, want >= 2", brk.AcceptedCount())
	}
	if warn := m.probeBuiltinBrokerReachability(brk, "127.0.0.1"); warn != "" {
		t.Errorf("expected no warning for loopback host, got %q", warn)
	}

	if tok := plugClient.Subscribe("cmnd/ec_plug/POWER1", 1, func(_ mqtt.Client, msg mqtt.Message) {
		if string(msg.Payload()) == "0" {
			plugClient.Publish("stat/ec_plug/POWER1", 0, false, "OFF")
		}
	}); !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("plugClient subscribe cmd failed: %v", tok.Error())
	}

	if err := m.switchPlugVerified(context.Background(), "ec_plug", 1, false); err != nil {
		t.Fatalf("switchPlugVerified through embedded broker failed: %v", err)
	}
	m.mu.RLock()
	plug := m.plugs["ec_plug"]
	m.mu.RUnlock()
	if plug == nil || plug.relayState(1) != "OFF" {
		t.Fatalf("plug relay state = %v, want OFF", plug)
	}
}

func TestDirectHTTPTasmotaPollAndSwitch(t *testing.T) {
	var mu sync.Mutex
	relayOn := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cm" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("user") != "admin" || q.Get("password") != "secretpw" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"WARNING":"Need user=<username>&password=<password>"}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch q.Get("cmnd") {
		case "Status 0":
			pwr := "OFF"
			watts := 0.0
			if relayOn {
				pwr = "ON"
				watts = 42.3
			}
			_, _ = fmt.Fprintf(w, `{
				"Status":{"DeviceName":"Entertainment Center","FriendlyName":["Entertainment Center"],"Topic":"Entertainment_Center"},
				"StatusFWR":{"Hardware":"ESP8266EX"},
				"StatusSNS":{"ENERGY":{"Power":%.1f,"Voltage":231.0,"Current":0.25}},
				"StatusSTS":{"POWER":"%s","Wifi":{"AP":1,"SSId":"HomeNet","RSSI":84,"Signal":-58}}
			}`, watts, pwr)
		case "POWER1 0":
			relayOn = false
			_, _ = w.Write([]byte(`{"POWER":"OFF"}`))
		case "POWER1 1":
			relayOn = true
			_, _ = w.Write([]byte(`{"POWER":"ON"}`))
		case "POWER2 1":
			_, _ = w.Write([]byte(`{"Command":"Error","Input":"POWER2 1"}`))
		default:
			_, _ = w.Write([]byte(`{"Command":"Unknown"}`))
		}
	}))
	defer srv.Close()

	m := newModule(nil)
	ctx := context.Background()

	if err := m.saveConfig(ctx, "printer-1", DeviceConfig{
		Mode:         "http",
		HTTPHost:     srv.URL,
		HTTPPassword: "secretpw",
		RelayIndex:   1,
	}); err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	cfg, _ := m.getConfig(ctx, "printer-1")
	if err := m.pollHTTPPlug(ctx, cfg); err != nil {
		t.Fatalf("pollHTTPPlug failed: %v", err)
	}

	// saveConfig polls the plug in the background too, so the plug is read
	// under the lock it is written under.
	m.mu.RLock()
	plug := m.plugs[cfg.plugKey()]
	found := plug != nil
	var online bool
	var relay1, name string
	var watts *float64
	if found {
		online = plug.Online != nil && *plug.Online
		relay1, name, watts = plug.relayState(1), plug.FriendlyName, plug.PowerWatts
	}
	m.mu.RUnlock()
	if !found || !online {
		t.Fatalf("expected HTTP plug online, found=%v online=%v", found, online)
	}
	if relay1 != "ON" {
		t.Errorf("relayState(1) = %q, want ON", relay1)
	}
	if name != "Entertainment Center" {
		t.Errorf("FriendlyName = %q, want Entertainment Center", name)
	}
	if watts == nil || *watts != 42.3 {
		t.Errorf("PowerWatts = %v, want 42.3", watts)
	}
	if signal := wifiOf(m, cfg.plugKey()); signal == nil || *signal != -58 {
		t.Errorf("WifiDBm = %v, want -58", signal)
	}

	if err := m.switchDevicePower(ctx, cfg, false); err != nil {
		t.Fatalf("switchDevicePower(OFF) failed: %v", err)
	}
	mu.Lock()
	stillOn := relayOn
	mu.Unlock()
	if stillOn {
		t.Fatal("expected mock plug relay to be OFF after switchDevicePower(false)")
	}

	badCfg := cfg
	badCfg.RelayIndex = 2
	if err := m.switchDevicePower(ctx, badCfg, true); err == nil {
		t.Fatal("expected error for non-existent relay 2, got nil")
	}

	// Updating config with KeepHTTPPassword=true and blank password must preserve stored secret.
	if err := m.saveConfig(ctx, "printer-1", DeviceConfig{
		Mode:              "http",
		HTTPHost:          srv.URL,
		KeepHTTPPassword:  true,
		AutoOffAfterPrint: true,
		RelayIndex:        1,
	}); err != nil {
		t.Fatalf("saveConfig keep password failed: %v", err)
	}
	updated, _ := m.getConfig(ctx, "printer-1")
	if updated.HTTPPassword != "secretpw" {
		t.Fatalf("expected preserved HTTPPassword, got %q", updated.HTTPPassword)
	}
	red := updated.redacted()
	if red.HTTPPassword != "" || !red.HasHTTPPassword {
		t.Fatalf("redacted() unexpected: %+v", red)
	}
}

// --- Wi-Fi signal and the plug's own protection ---

func wifiOf(m *module, key string) *int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if plug := m.plugs[key]; plug != nil && plug.WifiDBm != nil {
		v := *plug.WifiDBm
		return &v
	}
	return nil
}

func protectionOf(m *module, key string, relay int) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if plug := m.plugs[key]; plug != nil {
		return plug.protectionOf(relay)
	}
	return ""
}

func TestTasmotaWifiSignalFromState(t *testing.T) {
	m := newModule(nil)
	now := time.Now()

	m.handleMQTTPacket("tele/plug1/STATE", []byte(`{"POWER":"ON","Wifi":{"AP":1,"SSId":"HomeNet","BSSId":"AA:BB:CC:DD:EE:FF","Channel":6,"RSSI":74,"Signal":-63}}`), now)
	if got := wifiOf(m, "plug1"); got == nil || *got != -63 {
		t.Fatalf("signal from STATE = %v, want -63", got)
	}

	// An older build tells only a quality from 0 to 100.
	m.handleMQTTPacket("tele/plug2/STATE", []byte(`{"POWER":"ON","Wifi":{"RSSI":60}}`), now)
	if got := wifiOf(m, "plug2"); got == nil || *got != -70 {
		t.Fatalf("signal from RSSI = %v, want -70", got)
	}

	// The answer to the STATE query, and the one to Status 11.
	m.handleMQTTPacket("stat/plug3/RESULT", []byte(`{"POWER":"OFF","Wifi":{"Signal":-48}}`), now)
	if got := wifiOf(m, "plug3"); got == nil || *got != -48 {
		t.Fatalf("signal from RESULT = %v, want -48", got)
	}
	m.handleMQTTPacket("stat/plug4/STATUS11", []byte(`{"StatusSTS":{"POWER":"ON","Wifi":{"Signal":-81}}}`), now)
	if got := wifiOf(m, "plug4"); got == nil || *got != -81 {
		t.Fatalf("signal from STATUS11 = %v, want -81", got)
	}

	// A message with no Wifi object leaves the signal as it is.
	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER":"OFF"}`), now)
	if got := wifiOf(m, "plug1"); got == nil || *got != -63 {
		t.Fatalf("a message with no signal changed it to %v", got)
	}
}

func TestTasmotaMaxPowerWarningIsAProtectionFault(t *testing.T) {
	now := time.Now()
	for name, warning := range map[string]string{
		"a number":  `{"Time":"2026-11-22T10:00:00","MaxPowerReached":2600}`,
		"with unit": `{"MaxPowerReached":"2600W"}`,
		"a new try": `{"MaxPowerReachedRetry":"OFF"}`,
	} {
		m := newModule(nil)
		m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER":"ON"}`), now)
		m.handleMQTTPacket("stat/plug1/WARNING", []byte(warning), now)
		if got := protectionOf(m, "plug1", 1); got != protectOverpower {
			t.Fatalf("%s: protection = %q, want overpower", name, got)
		}

		// The relay going off does not end it. The relay coming on does.
		m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER":"OFF"}`), now)
		if got := protectionOf(m, "plug1", 1); got != protectOverpower {
			t.Fatalf("%s: the fault went away with the relay off", name)
		}
		m.handleMQTTPacket("stat/plug1/POWER", []byte("ON"), now)
		if got := protectionOf(m, "plug1", 1); got != "" {
			t.Fatalf("%s: the fault stayed with the relay on again: %q", name, got)
		}
	}

	m := newModule(nil)
	// A budget a person set is no fault, and neither is another warning.
	m.handleMQTTPacket("stat/plug1/WARNING", []byte(`{"MaxEnergyReached":"3000Wh"}`), now)
	m.handleMQTTPacket("stat/plug1/WARNING", []byte(`not json`), now)
	if got := protectionOf(m, "plug1", 1); got != "" {
		t.Fatalf("another warning is a fault: %q", got)
	}

	// The fault is the whole plug's, and it shows on the relay a printer has.
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug1", RelayIndex: 2})
	m.handleMQTTPacket("stat/plug1/WARNING", []byte(`{"MaxPowerReached":3100}`), now)
	m.mu.RLock()
	shown := m.plugSnapshotLocked(cfg)
	m.mu.RUnlock()
	if shown.Protection != protectOverpower {
		t.Fatalf("the answer for relay 2 says %q", shown.Protection)
	}
	m.handleMQTTPacket("stat/plug1/RESULT", []byte(`{"POWER2":"ON"}`), now)
	if got := protectionOf(m, "plug1", 2); got != "" {
		t.Fatalf("the fault stayed after relay 2 came on: %q", got)
	}

	// A plug with a topic layout of its own.
	disc := `{"t":"bench","ft":"farm/%topic%/%prefix%/","tp":["cmnd","state","telem"],"rl":[1]}`
	m.handleMQTTPacket("tasmota/discovery/AABBCCDDEEFF/config", []byte(disc), now)
	m.handleMQTTPacket("farm/bench/state/WARNING", []byte(`{"MaxPowerReached":2600}`), now)
	if got := protectionOf(m, "bench", 1); got != protectOverpower {
		t.Fatalf("a warning on a custom topic: protection = %q", got)
	}
	m.handleMQTTPacket("farm/bench/state/RESULT", []byte(`{"POWER":"ON"}`), now)
	if got := protectionOf(m, "bench", 1); got != "" {
		t.Fatalf("the fault stayed on a custom topic: %q", got)
	}
}

func TestAQueryAsksTasmotaForItsState(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	has := func(list []string, want string) bool {
		for _, got := range list {
			if got == want {
				return true
			}
		}
		return false
	}

	// A plug nothing is known of yet.
	m.mu.RLock()
	empty, status := m.plugQueryLocked("plug1")
	m.mu.RUnlock()
	if !has(empty, "cmnd/plug1/STATE") || !has(empty, "cmnd/plug1/POWER1") || status != "cmnd/plug1/Status" {
		t.Fatalf("an unknown plug is asked %v and %q", empty, status)
	}

	// A plug with two relays and a topic layout of its own.
	disc := `{"t":"bench","ft":"farm/%topic%/%prefix%/","tp":["cmnd","state","telem"],"rl":[1,1]}`
	m.handleMQTTPacket("tasmota/discovery/AABBCCDDEEFF/config", []byte(disc), now)
	m.mu.RLock()
	empty, status = m.plugQueryLocked("bench")
	m.mu.RUnlock()
	if len(empty) != 4 || !has(empty, "farm/bench/cmnd/PowerOnState") || !has(empty, "farm/bench/cmnd/STATE") || !has(empty, "farm/bench/cmnd/POWER2") || status != "farm/bench/cmnd/Status" {
		t.Fatalf("a plug with its own layout is asked %v and %q", empty, status)
	}

	// The 5-second tick does not ask for the state: only the meter reading,
	// and the relay while it is not known.
	m.configs["p1"] = normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "bench"})
	recorder := &publishRecorder{}
	m.mqttClient, m.mqttUp = recorder, true
	m.pollAssignedPlugsEnergy()
	for _, topic := range recorder.topics() {
		if strings.HasSuffix(topic, "/STATE") {
			t.Fatalf("the tick asked for the state: %v", recorder.topics())
		}
	}
	if !has(recorder.topics(), "farm/bench/cmnd/Status") {
		t.Fatalf("the tick did not ask for the reading: %v", recorder.topics())
	}

	recorder.reset()
	m.queryPlugState("bench")
	if !has(recorder.topics(), "farm/bench/cmnd/STATE") {
		t.Fatalf("the query did not ask for the state: %v", recorder.topics())
	}
}

// publishRecorder is an MQTT client that keeps the topics it was told to
// publish on. Only Publish is used: every other call of the interface would
// stop the test.
type publishRecorder struct {
	mqtt.Client
	mu   sync.Mutex
	sent []string
}

func (r *publishRecorder) Publish(topic string, _ byte, _ bool, _ interface{}) mqtt.Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, topic)
	return &mqtt.DummyToken{}
}

func (r *publishRecorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func (r *publishRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
}

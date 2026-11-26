package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A plug or Home Assistant that answers with a redirect is not followed: the
// address it names is declared nowhere, so no request may reach it.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(elsewhere.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	m := newModule(nil)
	tasmota := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: redirector.URL, HTTPPassword: "pw"})
	if _, err := m.tasmotaHTTPGet(t.Context(), tasmota, "Power1"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("Tasmota: err = %v, want a redirect error", err)
	}
	shelly := normalizeDeviceConfig(DeviceConfig{Driver: DriverShellyHTTP, HTTPHost: redirector.URL, HTTPPassword: "pw"})
	if _, _, err := m.shellyHTTPRequest(t.Context(), shelly, "/rpc/Switch.GetStatus?id=0"); err == nil {
		t.Fatal("Shelly: a redirect was taken as an answer")
	}
	if err := m.pollShellyHTTPPlug(t.Context(), shelly); err == nil {
		t.Fatal("Shelly poll (with the Gen1 fallback) took a redirect as an answer")
	}
	s := normalizeSettings(Settings{HAURL: redirector.URL, HAToken: "token"})
	if _, _, err := m.haHTTPRequest(t.Context(), s, http.MethodPost, "/api/services/homeassistant/turn_on", map[string]string{"entity_id": "switch.x"}); err == nil {
		t.Fatal("Home Assistant: a redirect was taken as an answer")
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want none", n)
	}
}

func TestValidHAEntity(t *testing.T) {
	cases := []struct {
		id    string
		power bool
		want  bool
	}{
		{"switch.printer", false, true},
		{"input_boolean.printer_power", false, true},
		{"light.desk_lamp", false, true},
		{"fan.enclosure", false, true},
		{"sensor.printer_power", true, true},
		{"cover.garage_door", false, false},
		{"lock.front_door", false, false},
		{"alarm_control_panel.home", false, false},
		{"script.open_gate", false, false},
		{"automation.leave", false, false},
		{"switch.a/../x", false, false},
		{"switch.", false, false},
		{"switch", false, false},
		{"Switch.printer", false, false},
		{"sensor.printer_power", false, false},
		{"switch.printer", true, false},
	}
	for _, c := range cases {
		if got := validHAEntity(c.id, c.power); got != c.want {
			t.Errorf("validHAEntity(%q, %v) = %v, want %v", c.id, c.power, got, c.want)
		}
	}
}

func TestTheConfigRefusesAnUnsupportedHAEntity(t *testing.T) {
	access := &accessFake{allow: map[string]bool{actionConfigureResource: true, actionOperate: true}}
	for _, body := range []string{
		`{"driver":"homeassistant","ha_entity_id":"cover.garage_door"}`,
		`{"ha_entity_id":"lock.front_door"}`,
		`{"driver":"homeassistant","ha_entity_id":"switch.plug","ha_power_entity_id":"switch.other"}`,
	} {
		m := guardedModule(t, "user:manager", access)
		rec := serve(m, "PUT", "/devices/p1/config", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
		if _, saved := m.configs["p1"]; saved {
			t.Fatalf("%s: the config was saved", body)
		}
	}
}

// A config saved by an older version with another entity is not acted on.
func TestAnUnsupportedHAEntityIsNeitherSwitchedNorRead(t *testing.T) {
	var asked atomic.Int32
	ha := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	t.Cleanup(ha.Close)
	m := newModule(nil)
	s := normalizeSettings(Settings{HAURL: ha.URL, HAToken: "token"})
	m.held.Store(&s)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverHomeAssistant, HAEntityID: "cover.garage_door"})

	if err := m.switchHomeAssistantPlugVerified(t.Context(), cfg, true); !errors.Is(err, errUnsupportedHAEntity) {
		t.Fatalf("switch: err = %v, want errUnsupportedHAEntity", err)
	}
	if err := m.pollHomeAssistantPlug(t.Context(), cfg); !errors.Is(err, errUnsupportedHAEntity) {
		t.Fatalf("poll: err = %v, want errUnsupportedHAEntity", err)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("Home Assistant got %d requests, want none", n)
	}
}

// connectPacket builds an MQTT 3.1.1 CONNECT with a user name and password.
func connectPacket(user, pass string) []byte {
	str := func(s string) []byte { return append([]byte{byte(len(s) >> 8), byte(len(s))}, s...) }
	body := append(str("MQTT"), 0x04, 0xC2, 0x00, 0x3C)
	body = append(body, str("t")...)
	body = append(body, str(user)...)
	body = append(body, str(pass)...)
	return append([]byte{mqttPktConnect << 4, byte(len(body))}, body...)
}

func startTestBroker(t *testing.T) *EmbeddedBroker {
	t.Helper()
	brk, err := StartEmbeddedBroker("127.0.0.1:0", "prusa", "secret")
	if err != nil {
		t.Fatalf("StartEmbeddedBroker: %v", err)
	}
	t.Cleanup(func() { _ = brk.Close() })
	return brk
}

// closedSoon reports whether the broker closed conn within a second.
func closedSoon(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := conn.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || (err != nil && !isTimeout(err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestTheBrokerRefusesALargePacketBeforeSignIn(t *testing.T) {
	brk := startTestBroker(t)
	conn, err := net.Dial("tcp", brk.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// CONNECT announcing about 80 KiB, more than a sign-in may carry.
	if _, err := conn.Write([]byte{mqttPktConnect << 4, 0xFF, 0xFF, 0x04}); err != nil {
		t.Fatal(err)
	}
	if !closedSoon(conn) {
		t.Fatal("the broker waits for an oversized CONNECT")
	}
}

func TestTheBrokerBoundsConnectionsThatHaveNotSignedIn(t *testing.T) {
	brk := startTestBroker(t)
	addr := brk.listener.Addr().String()
	var waiting []net.Conn
	t.Cleanup(func() {
		for _, c := range waiting {
			_ = c.Close()
		}
	})
	for range maxPendingConns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		waiting = append(waiting, c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for brk.AcceptedCount() < maxPendingConns && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	if !closedSoon(extra) {
		t.Fatal("a connection over the limit was kept")
	}

	// The module's own client is never locked out.
	pipe, err := brk.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	_ = pipe.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := pipe.Write(connectPacket("prusa", "secret")); err != nil {
		t.Fatal(err)
	}
	ack := make([]byte, 4)
	if _, err := io.ReadFull(bufio.NewReader(pipe), ack); err != nil || ack[3] != 0 {
		t.Fatalf("the module's own client was not let in: %v %v", ack, err)
	}
}

func TestABadSignInIsAnsweredLate(t *testing.T) {
	old := badSignInDelay
	badSignInDelay = 300 * time.Millisecond
	t.Cleanup(func() { badSignInDelay = old })

	brk := startTestBroker(t)
	conn, err := net.Dial("tcp", brk.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	if _, err := conn.Write(connectPacket("prusa", "wrong")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	ack := make([]byte, 4)
	if _, err := io.ReadFull(conn, ack); err != nil {
		t.Fatal(err)
	}
	if ack[3] != 0x04 {
		t.Fatalf("CONNACK code %#x, want 0x04", ack[3])
	}
	if elapsed := time.Since(start); elapsed < badSignInDelay {
		t.Fatalf("a bad sign-in was answered after %v, want at least %v", elapsed, badSignInDelay)
	}
}

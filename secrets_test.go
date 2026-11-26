package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// managerModule is a module of a person who may configure it, with s saved.
func managerModule(t *testing.T, s Settings) *module {
	t.Helper()
	m := guardedModule(t, "user:manager", &accessFake{allow: map[string]bool{actionConfigureResource: true}})
	saved := normalizeSettings(s)
	m.held.Store(&saved)
	return m
}

func TestKeepPasswordOnlyForTheSameBroker(t *testing.T) {
	saved := normalizeSettings(Settings{MQTTHost: "broker.lan", MQTTTLS: true, MQTTPassword: "pw"})
	ask := Settings{KeepPassword: true}

	cases := []struct {
		name string
		next Settings
		keep bool
	}{
		{"same broker", Settings{MQTTHost: "Broker.LAN", MQTTTLS: true}, true},
		{"other host", Settings{MQTTHost: "evil.example", MQTTTLS: true}, false},
		{"other port", Settings{MQTTHost: "broker.lan", MQTTTLS: true, MQTTPort: 9000}, false},
		{"TLS turned off", Settings{MQTTHost: "broker.lan"}, false},
		{"written with a scheme", Settings{MQTTHost: "mqtts://broker.lan"}, true},
		{"written with the port", Settings{MQTTHost: "broker.lan:8883", MQTTTLS: true}, true},
		{"built-in broker", Settings{MQTTBuiltin: true, MQTTHost: "192.168.1.5"}, false},
	}
	for _, c := range cases {
		next := normalizeSettings(c.next)
		err := keepPassword(ask, saved, &next)
		if c.keep && (err != nil || next.MQTTPassword != "pw") {
			t.Errorf("%s: password not kept: %v", c.name, err)
		}
		if !c.keep && (!errors.Is(err, errPasswordNeedsRetype) || next.MQTTPassword != "") {
			t.Errorf("%s: err = %v, password = %q, want a refusal and no password", c.name, err, next.MQTTPassword)
		}
	}

	// The built-in broker's password does not follow to a broker elsewhere.
	builtin := normalizeSettings(Settings{MQTTBuiltin: true, MQTTUsername: "plugs", MQTTPassword: "pw"})
	next := normalizeSettings(Settings{MQTTHost: "evil.example"})
	if err := keepPassword(ask, builtin, &next); !errors.Is(err, errPasswordNeedsRetype) {
		t.Errorf("built-in to external: err = %v, want a refusal", err)
	}

	// The built-in broker keeps its own password.
	next = normalizeSettings(Settings{MQTTBuiltin: true, MQTTUsername: "plugs"})
	if err := keepPassword(ask, builtin, &next); err != nil || next.MQTTPassword != "pw" {
		t.Errorf("built-in to built-in: err = %v, password = %q", err, next.MQTTPassword)
	}

	// A password typed now goes where the person sends it.
	typed := Settings{MQTTPassword: "new", KeepPassword: true}
	next = normalizeSettings(Settings{MQTTHost: "other.lan", MQTTPassword: "new"})
	if err := keepPassword(typed, saved, &next); err != nil || next.MQTTPassword != "new" {
		t.Errorf("typed password: err = %v, password = %q", err, next.MQTTPassword)
	}
}

func TestKeepHATokenOnlyForTheSameServer(t *testing.T) {
	saved := normalizeSettings(Settings{HAURL: "https://ha.lan:8123", HAToken: "tok"})
	ask := Settings{KeepHAToken: true}

	cases := []struct {
		url  string
		keep bool
	}{
		{"https://HA.lan:8123/", true},
		{"https://ha.lan:8123/some/path", true},
		{"http://ha.lan:8123", false},
		{"https://ha.lan:8124", false},
		{"https://evil.example:8123", false},
	}
	for _, c := range cases {
		next := normalizeSettings(Settings{HAURL: c.url})
		err := keepHAToken(ask, saved, &next)
		if c.keep && (err != nil || next.HAToken != "tok") {
			t.Errorf("%s: token not kept: %v", c.url, err)
		}
		if !c.keep && (!errors.Is(err, errTokenNeedsRetype) || next.HAToken != "") {
			t.Errorf("%s: err = %v, token = %q, want a refusal and no token", c.url, err, next.HAToken)
		}
	}
}

// The origin leaves a default port out, as the browser does, so the form
// and the process agree on what is the same server.
func TestHAOriginLeavesTheDefaultPortOut(t *testing.T) {
	cases := map[string]string{
		"https://ha.example.com":        "https://ha.example.com",
		"https://HA.example.com:443/x":  "https://ha.example.com",
		"http://ha.lan:80":              "http://ha.lan",
		"http://ha.lan:443":             "http://ha.lan:443",
		"https://ha.lan:8123":           "https://ha.lan:8123",
		"http://[fd00::1]:80":           "http://[fd00::1]",
		"http://[fd00::1]:8123":         "http://[fd00::1]:8123",
		"":                              "",
	}
	for raw, want := range cases {
		if got := haOrigin(raw); got != want {
			t.Errorf("haOrigin(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A test that keeps the saved token but names another server sends nothing
// there, and neither does a save.
func TestSavedHATokenIsNotSentToAnotherServer(t *testing.T) {
	var asked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		asked.Add(1)
	}))
	defer other.Close()

	m := managerModule(t, Settings{HAURL: "http://ha.lan:8123", HAToken: "tok"})
	body := fmt.Sprintf(`{"ha_url":%q,"keep_ha_token":true}`, other.URL)

	rec := serve(m, "POST", "/ha/test", body)
	var tested struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tested); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || tested.OK || tested.Error != errTokenNeedsRetype.Error() {
		t.Fatalf("test: status %d: %s", rec.Code, rec.Body.String())
	}

	rec = serve(m, "PUT", "/settings", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("save: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := m.settings(); got.HAURL != "http://ha.lan:8123" || got.HAToken != "tok" {
		t.Fatalf("a refused save changed the settings: %+v", got.redacted())
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the other server was asked %d times", n)
	}

	// The same server keeps its token, and a token typed now may go anywhere.
	if rec := serve(m, "PUT", "/settings", `{"ha_url":"http://ha.lan:8123/","keep_ha_token":true}`); rec.Code != http.StatusOK {
		t.Fatalf("save for the same server: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := m.settings(); got.HAToken != "tok" {
		t.Fatalf("token = %q, want the saved one kept", got.HAToken)
	}
	typed := fmt.Sprintf(`{"ha_url":%q,"ha_token":"mine"}`, other.URL)
	if rec := serve(m, "PUT", "/settings", typed); rec.Code != http.StatusOK {
		t.Fatalf("save with a typed token: status %d: %s", rec.Code, rec.Body.String())
	}
}

// A broker test that keeps the saved password but names another host opens
// no connection to it.
func TestSavedMQTTPasswordIsNotSentToAnotherBroker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var dialed atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			dialed.Add(1)
			_ = conn.Close()
		}
	}()

	m := managerModule(t, Settings{MQTTHost: "broker.lan", MQTTUsername: "plugs", MQTTPassword: "pw"})
	port := ln.Addr().(*net.TCPAddr).Port
	body := fmt.Sprintf(`{"mqtt_host":"127.0.0.1","mqtt_port":%d,"mqtt_username":"plugs","keep_password":true}`, port)

	rec := serve(m, "POST", "/broker/test", body)
	var tested struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tested); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || tested.OK || tested.Error != errPasswordNeedsRetype.Error() {
		t.Fatalf("test: status %d: %s", rec.Code, rec.Body.String())
	}

	rec = serve(m, "PUT", "/settings", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("save: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := m.settings(); got.MQTTHost != "broker.lan" {
		t.Fatalf("a refused save changed the broker to %q", got.MQTTHost)
	}

	time.Sleep(100 * time.Millisecond)
	if n := dialed.Load(); n != 0 {
		t.Fatalf("the other broker was dialed %d times", n)
	}
}

// A plug's own password is kept only while the plug's address stays.
func TestSavedPlugPasswordIsNotSentToAnotherAddress(t *testing.T) {
	m := newModule(nil)
	ctx := t.Context()
	// Nothing listens on these; the reads after a save fail and are dropped.
	first := DeviceConfig{Mode: "http", HTTPHost: "127.0.0.1:9", HTTPPassword: "secretpw", RelayIndex: 1}
	if err := m.saveConfig(ctx, "p", first); err != nil {
		t.Fatal(err)
	}

	moved := DeviceConfig{Mode: "http", HTTPHost: "127.0.0.2:9", KeepHTTPPassword: true, RelayIndex: 1}
	if err := m.saveConfig(ctx, "p", moved); !errors.Is(err, errPlugPasswordRetype) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got, _ := m.getConfig(ctx, "p"); got.HTTPHost != "127.0.0.1:9" || got.HTTPPassword != "secretpw" {
		t.Fatalf("a refused save changed the plug: host %q", got.HTTPHost)
	}

	// Without the wish to keep it, the plug moves and the password is gone.
	moved.KeepHTTPPassword = false
	if err := m.saveConfig(ctx, "p", moved); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.getConfig(ctx, "p"); got.HTTPHost != "127.0.0.2:9" || got.HTTPPassword != "" {
		t.Fatalf("host %q, password set: %v", got.HTTPHost, got.HTTPPassword != "")
	}
}

// --- The plug's password and the error text ---

// namesPassword reports what of a Tasmota request's address a text still
// holds: the password, or the part of the address that carries it.
func namesPassword(text, password string) string {
	for _, part := range []string{password, url.QueryEscape(password), "password=", "/cm?"} {
		if strings.Contains(text, part) {
			return part
		}
	}
	return ""
}

func TestAnUnreachableTasmotaPlugDoesNotNameItsPassword(t *testing.T) {
	m := newModule(nil)
	// Nothing listens on port 1.
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: "127.0.0.1:1", HTTPPassword: "hunter2 & more"})

	err := m.pollHTTPPlug(t.Context(), cfg)
	if err == nil {
		t.Fatal("a plug nothing listens for answered")
	}
	if part := namesPassword(err.Error(), cfg.HTTPPassword); part != "" {
		t.Fatalf("the error names %q: %v", part, err)
	}

	plug := plugOf(m, cfg)
	if plug == nil || plug.LastError == "" {
		t.Fatalf("the plug keeps no error: %+v", plug)
	}
	if part := namesPassword(plug.LastError, cfg.HTTPPassword); part != "" {
		t.Fatalf("the plug's last error names %q: %s", part, plug.LastError)
	}
}

func TestATimedOutTasmotaPlugDoesNotNameItsPassword(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	// The handler is let go before the server waits for it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	m := newModule(nil)
	m.httpClient = newHTTPClient(100 * time.Millisecond)
	cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: srv.URL, HTTPPassword: "hunter2pw"})

	err := m.switchHTTPPlugVerified(t.Context(), cfg, 1, false)
	if err == nil {
		t.Fatal("a plug that does not answer was switched")
	}
	if part := namesPassword(err.Error(), cfg.HTTPPassword); part != "" {
		t.Fatalf("the error names %q: %v", part, err)
	}
	if strings.Contains(err.Error(), strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatalf("the error names the address: %v", err)
	}
}

func TestWithoutURL(t *testing.T) {
	inner := errors.New("connection refused")
	wrapped := &url.Error{Op: "Get", URL: "http://plug.lan/cm?user=admin&password=pw&cmnd=Status+0", Err: inner}

	if got := withoutURL(wrapped); got != inner {
		t.Fatalf("withoutURL = %v, want the inner error", got)
	}
	if got := withoutURL(fmt.Errorf("read the plug: %w", wrapped)); got != inner {
		t.Fatalf("a wrapped error gave %v, want the inner error", got)
	}
	if got := withoutURL(inner); got != inner {
		t.Fatalf("an error with no address became %v", got)
	}
	if withoutURL(nil) != nil {
		t.Fatal("no error became one")
	}
	empty := &url.Error{Op: "Get", URL: "http://plug.lan/"}
	if got := withoutURL(empty); got != error(empty) {
		t.Fatalf("an error with no cause became %v", got)
	}
}

func TestScrubWith(t *testing.T) {
	secrets := []string{"s3cret/pw", "tok+en", "ab"}
	hosts := []string{"printer-plug.lan", "broker.example.org", "ha"}

	for _, tc := range []struct{ name, in, want string }{
		{"clean text", "plug did not confirm state change within 2.5s", "plug did not confirm state change within 2.5s"},
		{"a URL with a query", `Get "http://192.168.1.50/cm?cmnd=Status+0&password=x": connection refused`, `Get "[address]": connection refused`},
		{"IPv4 with a port", "dial tcp 192.168.1.50:80: connect: connection refused", "dial tcp [address]: connect: connection refused"},
		{"bracketed IPv6", "dial tcp [fe80::1%eth0]:1883: i/o timeout", "dial tcp [address]: i/o timeout"},
		{"a known host", "dial tcp: lookup Printer-Plug.lan: no such host", "dial tcp: lookup [address]: no such host"},
		{"a known secret", "refused the password s3cret/pw", "refused the password [secret]"},
		{"a secret as a query writes it", "bad request s3cret%2Fpw and tok%2Ben", "bad request [secret] and [secret]"},
		{"credentials in a query", "need password=x&user=y to go on", "need password=[secret]&user=[secret] to go on"},
		{"a hardware address", "device AA:BB:CC:DD:EE:FF is gone", "device [address] is gone"},
		{"more than one line", "first\r\nsecond\nthird", "first second third"},
		{"a short host the resolver names", "dial tcp: lookup p1 on 192.168.1.1:53: no such host", "dial tcp: lookup [address] on [address]: no such host"},
		{"a short host with nothing after it", "dial tcp: lookup ha: i/o timeout", "dial tcp: lookup [address]: i/o timeout"},
		{"the names of a certificate", "tls: failed to verify certificate: x509: certificate is valid for nas.home.example, *.home.example, not mqtt.lan", "tls: failed to verify certificate: x509: certificate is valid for [address]"},
		{"a certificate with one name", "x509: certificate is valid for nas.home.example, not broker.example.org", "x509: certificate is valid for [address]"},
		{"a short secret is left", "ab is a word", "ab is a word"},
	} {
		if got := scrubWith(tc.in, secrets, hosts); got != tc.want {
			t.Errorf("%s: scrubWith(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	long := scrubWith(strings.Repeat("é", 250), nil, nil)
	if len(long) > maxScrubbedBytes || len(long) < maxScrubbedBytes-1 || !utf8.ValidString(long) {
		t.Errorf("a long text was cut to %d bytes, valid: %v", len(long), utf8.ValidString(long))
	}
	if got := scrubWith(strings.Repeat("x", 500), nil, nil); len(got) != maxScrubbedBytes {
		t.Errorf("500 characters were cut to %d", len(got))
	}
}

// The filter of a module knows its own secrets and addresses.
func TestTheFilterKnowsTheSavedSecretsAndAddresses(t *testing.T) {
	m := newModule(nil)
	s := normalizeSettings(Settings{MQTTHost: "broker.lan", MQTTPassword: "brokerpw", HAURL: "http://hass.lan:8123", HAToken: "longlivedtoken"})
	m.held.Store(&s)
	m.configs["p"] = normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: "plug-7.lan", HTTPPassword: "plugpw"})

	got := m.scrubText("plug-7.lan said plugpw, broker.lan said brokerpw, hass.lan:8123 said longlivedtoken")
	if got != "[address] said [secret], [address] said [secret], [address] said [secret]" {
		t.Fatalf("scrubText = %q", got)
	}
}

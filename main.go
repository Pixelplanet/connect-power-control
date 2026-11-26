package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prusa3d/prusa-connect/sdk/accessapi"
	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/diagnosticsapi"
	"github.com/prusa3d/prusa-connect/sdk/extension"
	"github.com/prusa3d/prusa-connect/sdk/fault"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"
	"github.com/prusa3d/prusa-connect/sdk/wire"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

const (
	name    = "connect-power-control"
	logName = "connect-power-control"
	release = extension.Version("0.6.0")

	defaultMQTTPort     = 1883
	defaultMQTTTLSPort  = 8883
	defaultMQTTClientID = "prusa-connect-tasmota"
	defaultCooldownSec  = 120
	pollIntervalSec     = 5
	configsFileName     = "configs.json"
	settingsFileName    = "settings.json"
	usageFileName       = "usage.json"
	// maxPowerSamples caps the samples kept per printer: an hour at one
	// sample every two seconds, the shortest step between two samples.
	maxPowerSamples = 1800
	maxPrintHistory = 10
)

//go:embed openapi.json
var openAPI []byte

// Settings holds structured MQTT broker and default auto-off settings.
// Persisted in settings.json under the module's granted data directory.
type Settings struct {
	MQTTBuiltin        bool   `json:"mqtt_builtin"`
	MQTTHost           string `json:"mqtt_host"`
	MQTTPort           int    `json:"mqtt_port"`
	MQTTTLS            bool   `json:"mqtt_tls"`
	MQTTUsername       string `json:"mqtt_username"`
	MQTTPassword       string `json:"mqtt_password,omitempty"`
	HasPassword        bool   `json:"has_password"`
	KeepPassword       bool   `json:"keep_password,omitempty"`
	MQTTClientID       string `json:"mqtt_client_id"`
	HAURL              string `json:"ha_url,omitempty"`
	HAToken            string `json:"ha_token,omitempty"`
	HasHAToken         bool   `json:"has_ha_token"`
	KeepHAToken        bool   `json:"keep_ha_token,omitempty"`
	AutoOffCooldownSec int    `json:"auto_off_cooldown_sec"`
	ConfirmTurnOff     *bool  `json:"confirm_turn_off"`
	// IdleOffMin is after how many minutes a printer that is on and idle is
	// turned off. 0, the default, never does that.
	IdleOffMin int `json:"idle_off_min"`

	// Legacy option from 0.1.0; parsed into MQTTHost/MQTTPort/MQTTTLS when present.
	MQTTBroker string `json:"mqtt_broker,omitempty"`
}

// builtinListenName is the name module.yaml gives the built-in broker's port
// under needs.listen. The operator approves the port and may move it.
const builtinListenName = "mqtt"

// hostSettings is what the host sends on plugin.configure: the options
// module.yaml declares, as the operator wrote them on the extension's own
// Settings tab or in the server's configuration. The host decodes strictly, so
// every declared option needs a field here: a key with no field refuses the
// start.
type hostSettings struct {
	Settings

	// HTTPHost is the http_host option. A plug's address is kept per printer,
	// so this one only fills in the address field when a plug is set up.
	HTTPHost string `json:"http_host,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

func (s Settings) confirmTurnOffEnabled() bool {
	if s.ConfirmTurnOff == nil {
		return true
	}
	return *s.ConfirmTurnOff
}

func defaultSettings() Settings {
	return Settings{
		MQTTPort:           defaultMQTTPort,
		MQTTClientID:       defaultMQTTClientID,
		AutoOffCooldownSec: defaultCooldownSec,
		ConfirmTurnOff:     boolPtr(true),
	}
}

func normalizeSettings(s Settings) Settings {
	if strings.TrimSpace(s.MQTTHost) == "" && strings.TrimSpace(s.MQTTBroker) != "" {
		host, port, useTLS := parseBrokerURL(s.MQTTBroker)
		s.MQTTHost = host
		if port > 0 {
			s.MQTTPort = port
		}
		s.MQTTTLS = useTLS
	} else if strings.Contains(s.MQTTHost, "://") || strings.Contains(s.MQTTHost, ":") {
		// If the user pasted a scheme or host:port into the host box, split it cleanly.
		host, port, useTLS := parseBrokerURL(s.MQTTHost)
		if host != "" {
			s.MQTTHost = host
			if port > 0 && s.MQTTPort == 0 {
				s.MQTTPort = port
			}
			if useTLS {
				s.MQTTTLS = true
			}
		}
	}
	s.MQTTHost = strings.TrimSpace(s.MQTTHost)
	s.MQTTBroker = ""
	if s.MQTTPort <= 0 || s.MQTTPort > maxPort {
		if s.MQTTTLS && !s.MQTTBuiltin {
			s.MQTTPort = defaultMQTTTLSPort
		} else {
			s.MQTTPort = defaultMQTTPort
		}
	}
	if strings.TrimSpace(s.MQTTClientID) == "" {
		s.MQTTClientID = defaultMQTTClientID
	} else {
		s.MQTTClientID = strings.TrimSpace(s.MQTTClientID)
	}
	s.AutoOffCooldownSec = clampCooldownSec(s.AutoOffCooldownSec)
	if s.ConfirmTurnOff == nil {
		s.ConfirmTurnOff = boolPtr(true)
	}
	s.IdleOffMin = clampIdleOffMin(s.IdleOffMin)
	s.HAURL = normalizeBaseURL(s.HAURL)
	s.HAToken = strings.TrimSpace(s.HAToken)
	s.HasPassword = s.MQTTPassword != ""
	s.HasHAToken = s.HAToken != ""
	return s
}

// The idle time a printer may be given, in minutes. A shorter one would turn
// a printer off while somebody is still loading it.
const (
	minIdleOffMin = 10
	maxIdleOffMin = 10080
)

// maxCooldownSec is the longest wait after a print: one day.
const maxCooldownSec = 86400

// clampCooldownSec brings a wait after a print into what is allowed.
func clampCooldownSec(sec int) int {
	return min(max(sec, 0), maxCooldownSec)
}

// clampIdleOffMin brings an idle time into what is allowed. 0 stays 0, which
// is "never".
func clampIdleOffMin(minutes int) int {
	switch {
	case minutes <= 0:
		return 0
	case minutes < minIdleOffMin:
		return minIdleOffMin
	case minutes > maxIdleOffMin:
		return maxIdleOffMin
	}
	return minutes
}

func parseBrokerURL(raw string) (host string, port int, useTLS bool) {
	b := strings.TrimSpace(raw)
	if b == "" {
		return "", 0, false
	}
	// The scheme, when one was written, says only whether TLS is wanted.
	if scheme, rest, found := strings.Cut(b, "://"); found {
		scheme = strings.ToLower(scheme)
		useTLS = scheme == "ssl" || scheme == "tls" || scheme == "mqtts"
		b = rest
	}
	b, _, _ = strings.Cut(b, "/")
	if at := strings.LastIndex(b, "@"); at >= 0 {
		b = b[at+1:]
	}
	if h, p, err := net.SplitHostPort(b); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= maxPort {
			port = n
		}
		return h, port, useTLS
	}
	return strings.Trim(b, "[]"), 0, useTLS
}

// maxPort is the highest TCP port.
const maxPort = 65535

// portAllowed answers whether the port written in an address, if any, is a
// TCP port. An empty port means the scheme's default.
func portAllowed(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n <= maxPort
}

func (s Settings) mqttConfigured() bool {
	return s.MQTTBuiltin || strings.TrimSpace(s.MQTTHost) != ""
}

func (s Settings) haConfigured() bool {
	return normalizeBaseURL(s.HAURL) != "" && strings.TrimSpace(s.HAToken) != ""
}

// builtinCredentialsSet answers whether the built-in broker has the user name
// and password it needs to start.
func (s Settings) builtinCredentialsSet() bool {
	return strings.TrimSpace(s.MQTTUsername) != "" && s.MQTTPassword != ""
}

// errBuiltinNeedsCredentials is what the settings say while the built-in
// broker is switched on without a user name and a password.
const errBuiltinNeedsCredentials = "The built-in broker needs a user name and a password, so that only your plugs can sign in to it. Set both on the Connections tab, then set the same values in each plug."

// inProcessBroker names the built-in broker in this module's MQTT client. The
// client never dials it: its connection function opens a pipe into the broker
// in this process (EmbeddedBroker.Pipe), so no socket is opened.
const inProcessBroker = "built-in-broker"

// builtinLabel is how the settings name the built-in broker.
func builtinLabel(port int) string {
	return fmt.Sprintf("built-in broker on port %d", port)
}

// pipeInto makes an MQTT client reach brk in process.
func pipeInto(opts *mqtt.ClientOptions, brk *EmbeddedBroker) *mqtt.ClientOptions {
	return opts.SetCustomOpenConnectionFn(func(*url.URL, mqtt.ClientOptions) (net.Conn, error) {
		return brk.Pipe()
	})
}

// brokerURL is the address of the external broker the operator set, which
// this module's MQTT client dials: the mqtt_host option or the host on the
// Connections tab. It is empty for the built-in broker, which is reached in
// process.
func (s Settings) brokerURL() string {
	if s.MQTTBuiltin {
		return ""
	}
	host := strings.TrimSpace(s.MQTTHost)
	if host == "" {
		return ""
	}
	scheme := "tcp"
	if s.MQTTTLS {
		scheme = "ssl"
	}
	port := s.MQTTPort
	if port <= 0 {
		if s.MQTTTLS {
			port = defaultMQTTTLSPort
		} else {
			port = defaultMQTTPort
		}
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
}

func (s Settings) redacted() Settings {
	out := s
	out.HasPassword = s.MQTTPassword != ""
	out.MQTTPassword = ""
	out.KeepPassword = false
	out.HasHAToken = s.HAToken != ""
	out.HAToken = ""
	out.KeepHAToken = false
	out.MQTTBroker = ""
	return out
}

// DeviceConfig is persisted per printer in configs.json.
type DeviceConfig struct {
	Driver                string `json:"driver,omitempty"` // tasmota, tasmota_http, shelly_gen1, shelly_gen2, shelly_http, zigbee2mqtt, esphome, openbeken, generic_mqtt, homeassistant
	Mode                  string `json:"mode,omitempty"`   // legacy: "mqtt" or "http"
	TasmotaTopic          string `json:"tasmota_topic"`
	HTTPHost              string `json:"http_host,omitempty"`
	HTTPUser              string `json:"http_user,omitempty"`
	HTTPPassword          string `json:"http_password,omitempty"`
	HasHTTPPassword       bool   `json:"has_http_password,omitempty"`
	KeepHTTPPassword      bool   `json:"keep_http_password,omitempty"`
	MQTTCommandTopic      string `json:"mqtt_command_topic,omitempty"`
	MQTTStateTopic        string `json:"mqtt_state_topic,omitempty"`
	MQTTAvailabilityTopic string `json:"mqtt_availability_topic,omitempty"`
	MQTTPayloadOn         string `json:"mqtt_payload_on,omitempty"`
	MQTTPayloadOff        string `json:"mqtt_payload_off,omitempty"`
	MQTTStateOn           string `json:"mqtt_state_on,omitempty"`
	MQTTStateOff          string `json:"mqtt_state_off,omitempty"`
	MQTTStateJSONKey      string `json:"mqtt_state_json_key,omitempty"`
	MQTTPowerTopic        string `json:"mqtt_power_topic,omitempty"`
	MQTTPowerJSONKey      string `json:"mqtt_power_json_key,omitempty"`
	HAEntityID            string `json:"ha_entity_id,omitempty"`
	HAPowerEntityID       string `json:"ha_power_entity_id,omitempty"`
	RelayIndex            int    `json:"relay_index,omitempty"`
	// AutoOffAfterPrint says whether the plug turns off after the print that
	// runs now, or the next one. AutoOffDefault is what it goes back to once
	// that print is over: a printer that turns off after every print has both.
	AutoOffAfterPrint bool `json:"auto_off_after_print"`
	AutoOffDefault    bool `json:"auto_off_default,omitempty"`
	CooldownSec       *int `json:"cooldown_sec,omitempty"`
	// IdleOffMin is this printer's own idle time in minutes. Left out, the
	// setting for all printers counts; 0 never turns this printer off.
	IdleOffMin *int `json:"idle_off_min,omitempty"`
}

func (c DeviceConfig) relayIndex() int {
	if c.RelayIndex <= 0 {
		return 1
	}
	if c.RelayIndex > 8 {
		return 8
	}
	return c.RelayIndex
}

func (c DeviceConfig) isHTTP() bool {
	return c.usesHTTP()
}

func (c DeviceConfig) configured() bool {
	switch c.effectiveDriver() {
	case DriverTasmota:
		return normalizeTopic(c.TasmotaTopic) != ""
	case DriverTasmotaHTTP, DriverShellyHTTP:
		return normalizeHTTPHost(c.HTTPHost) != ""
	case DriverShellyGen1:
		return normalizeShellyGen1ID(c.TasmotaTopic) != ""
	case DriverShellyGen2, DriverESPHome, DriverOpenBeken:
		return normalizeMQTTPrefix(c.TasmotaTopic) != ""
	case DriverZigbee2MQTT:
		return normalizeZ2MTopic(c.TasmotaTopic) != ""
	case DriverGenericMQTT:
		return strings.TrimSpace(c.MQTTCommandTopic) != ""
	case DriverHomeAssistant:
		return strings.TrimSpace(c.HAEntityID) != ""
	default:
		return false
	}
}

func (c DeviceConfig) plugKey() string {
	switch c.effectiveDriver() {
	case DriverTasmota:
		return normalizeTopic(c.TasmotaTopic)
	case DriverTasmotaHTTP:
		if h := normalizeHTTPHost(c.HTTPHost); h != "" {
			return "http:" + strings.ToLower(h)
		}
	case DriverShellyHTTP:
		if h := normalizeHTTPHost(c.HTTPHost); h != "" {
			return "shellyhttp:" + strings.ToLower(h)
		}
	case DriverShellyGen1:
		if id := normalizeShellyGen1ID(c.TasmotaTopic); id != "" {
			return "shelly1:" + id
		}
	case DriverShellyGen2:
		if p := normalizeMQTTPrefix(c.TasmotaTopic); p != "" {
			return "shelly2:" + p
		}
	case DriverZigbee2MQTT:
		if b := normalizeZ2MTopic(c.TasmotaTopic); b != "" {
			return "z2m:" + b
		}
	case DriverESPHome:
		if p := normalizeMQTTPrefix(c.TasmotaTopic); p != "" {
			return "esphome:" + p
		}
	case DriverOpenBeken:
		if p := normalizeMQTTPrefix(c.TasmotaTopic); p != "" {
			return "openbeken:" + p
		}
	case DriverGenericMQTT:
		if cmd := strings.TrimSpace(c.MQTTCommandTopic); cmd != "" {
			return "generic:" + cmd
		}
	case DriverHomeAssistant:
		if ent := strings.TrimSpace(c.HAEntityID); ent != "" {
			return "ha:" + strings.ToLower(ent)
		}
	}
	return ""
}

func (c DeviceConfig) httpBaseURL() string {
	h := normalizeHTTPHost(c.HTTPHost)
	if h == "" {
		return ""
	}
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		return h
	}
	return "http://" + h
}

func (c DeviceConfig) redacted() DeviceConfig {
	out := c
	out.HasHTTPPassword = c.HTTPPassword != ""
	out.HTTPPassword = ""
	out.KeepHTTPPassword = false
	return out
}

func normalizeHTTPHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || strings.TrimSpace(u.Host) == "" || !portAllowed(u.Port()) {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.TrimSpace(u.Host)
	if scheme == "https" {
		return "https://" + host
	}
	return host
}

// PlugTelemetry holds live state for a smart plug.
type PlugTelemetry struct {
	Topic        string         `json:"topic,omitempty"`
	Driver       string         `json:"driver,omitempty"`
	FriendlyName string         `json:"friendly_name,omitempty"`
	Model        string         `json:"model,omitempty"`
	RelayCount   int            `json:"relay_count,omitempty"`
	RelayNames   []string       `json:"relay_names,omitempty"`
	Relays       map[int]string `json:"relays,omitempty"`
	Power        string         `json:"power"`        // "ON", "OFF", or "UNKNOWN"
	Online       *bool          `json:"online"`       // from tele/<topic>/LWT
	PowerWatts   *float64       `json:"power_watts"`  // from tele/<topic>/SENSOR ENERGY.Power
	Voltage      *float64       `json:"voltage"`      // from tele/<topic>/SENSOR ENERGY.Voltage
	CurrentAmps  *float64       `json:"current_amps"` // from tele/<topic>/SENSOR ENERGY.Current
	LastError    string         `json:"last_error,omitempty"`
	UpdatedAt    *time.Time     `json:"updated_at,omitempty"`
	// WifiDBm is the plug's Wi-Fi signal in dBm, where the plug tells it.
	WifiDBm *int `json:"wifi_dbm,omitempty"`
	// Protection says the plug turned itself off to protect itself, and why:
	// overpower, overtemp, overvoltage or undervoltage. It is filled for the
	// relay a printer's config names (plugSnapshotLocked).
	Protection string `json:"protection,omitempty"`
	// AfterPowerCut is what the plug's relay does when the power comes back:
	// off, on, last, toggle, always_on, delayed_on or switch. It is the plug's
	// own setting, read and never changed, and filled like Protection.
	AfterPowerCut string `json:"after_power_cut,omitempty"`

	MAC         string    `json:"-"`
	FullTopic   string    `json:"-"`
	Prefixes    [3]string `json:"-"`
	StateTexts  [4]string `json:"-"`
	OnlineText  string    `json:"-"`
	OfflineText string    `json:"-"`

	// meterAt is when PowerWatts was last reported. It is not exported, so it
	// stays out of every answer, and clone copies it with the struct.
	meterAt time.Time
	// protections is what tripped, by relay number. Key 0 is the whole plug.
	protections map[int]string
	// wifiAt is when a plug read by address was last asked for its signal
	// and for what it does after a power cut.
	wifiAt time.Time
	// afterCut is what a relay does after a power cut, by relay number. Key 0
	// is the whole plug.
	afterCut map[int]string
}

// Why a plug turned itself off.
const (
	protectOverpower    = "overpower"
	protectOvertemp     = "overtemp"
	protectOvervoltage  = "overvoltage"
	protectUndervoltage = "undervoltage"
)

// setProtection keeps why relay turned itself off, 0 for the whole plug. An
// empty kind says it is over.
func (p *PlugTelemetry) setProtection(relay int, kind string) {
	if kind == "" {
		delete(p.protections, relay)
		return
	}
	if p.protections == nil {
		p.protections = make(map[int]string)
	}
	p.protections[relay] = kind
}

// protectionOf is what tripped for relay: its own fault, else the plug's.
func (p *PlugTelemetry) protectionOf(relay int) string {
	if kind := p.protections[relay]; kind != "" {
		return kind
	}
	return p.protections[0]
}

// setWifi takes a signal in dBm. A value no radio reports is not taken.
func (p *PlugTelemetry) setWifi(dbm float64) {
	if math.IsNaN(dbm) || dbm < -120 || dbm > 0 {
		return
	}
	v := int(math.Round(dbm))
	p.WifiDBm = &v
}

// wifiReading is what a Tasmota plug says of its Wi-Fi: Signal in dBm, and
// on older builds only RSSI, a quality from 0 to 100. The network's name is
// not read.
type wifiReading struct {
	Signal *float64 `json:"Signal"`
	RSSI   *float64 `json:"RSSI"`
}

// setTasmotaWifi takes the Wifi object of a Tasmota state message.
func (p *PlugTelemetry) setTasmotaWifi(raw json.RawMessage) {
	var wifi wifiReading
	if len(raw) == 0 || json.Unmarshal(raw, &wifi) != nil {
		return
	}
	switch {
	case wifi.Signal != nil:
		p.setWifi(*wifi.Signal)
	case wifi.RSSI != nil && *wifi.RSSI >= 0 && *wifi.RSSI <= 100:
		p.setWifi(*wifi.RSSI/2 - 100)
	}
}

func normalizeRelayIndex(idx int) int {
	if idx <= 0 {
		return 1
	}
	if idx > 8 {
		return 8
	}
	return idx
}

func (p *PlugTelemetry) clone() PlugTelemetry {
	cp := *p
	if len(p.RelayNames) > 0 {
		cp.RelayNames = append([]string(nil), p.RelayNames...)
	}
	if len(p.Relays) > 0 {
		cp.Relays = make(map[int]string, len(p.Relays))
		for k, v := range p.Relays {
			cp.Relays[k] = v
		}
	}
	if p.Online != nil {
		v := *p.Online
		cp.Online = &v
	}
	if p.PowerWatts != nil {
		v := *p.PowerWatts
		cp.PowerWatts = &v
	}
	if p.Voltage != nil {
		v := *p.Voltage
		cp.Voltage = &v
	}
	if p.CurrentAmps != nil {
		v := *p.CurrentAmps
		cp.CurrentAmps = &v
	}
	if p.UpdatedAt != nil {
		v := *p.UpdatedAt
		cp.UpdatedAt = &v
	}
	if p.WifiDBm != nil {
		v := *p.WifiDBm
		cp.WifiDBm = &v
	}
	cp.afterCut = nil
	if len(p.afterCut) > 0 {
		cp.afterCut = make(map[int]string, len(p.afterCut))
		for k, v := range p.afterCut {
			cp.afterCut[k] = v
		}
	}
	cp.protections = nil
	if len(p.protections) > 0 {
		cp.protections = make(map[int]string, len(p.protections))
		for k, v := range p.protections {
			cp.protections[k] = v
		}
	}
	return cp
}

func (p *PlugTelemetry) relayState(idx int) string {
	idx = normalizeRelayIndex(idx)
	if p.Relays != nil {
		if st := p.Relays[idx]; st != "" {
			return st
		}
	}
	if idx == 1 && p.Power != "" {
		return p.Power
	}
	return "UNKNOWN"
}

func (p *PlugTelemetry) setRelayState(idx int, state string) {
	idx = normalizeRelayIndex(idx)
	if p.Relays == nil {
		p.Relays = make(map[int]string)
	}
	p.Relays[idx] = state
	if p.RelayCount < idx {
		p.RelayCount = idx
	}
	if idx == 1 {
		p.Power = state
	}
}

func (p *PlugTelemetry) formatTopic(prefixIdx int, leaf string) string {
	if prefixIdx < 0 || prefixIdx > 2 {
		prefixIdx = 0
	}
	defaults := [3]string{"cmnd", "stat", "tele"}
	prefix := p.Prefixes[prefixIdx]
	if prefix == "" {
		prefix = defaults[prefixIdx]
	}
	ft := p.FullTopic
	if ft == "" {
		ft = "%prefix%/%topic%/"
	}
	base := strings.ReplaceAll(ft, "%prefix%", prefix)
	base = strings.ReplaceAll(base, "%topic%", p.Topic)
	base = strings.Trim(base, "/")
	if leaf == "" {
		if base == "" {
			return ""
		}
		return base + "/"
	}
	if base == "" {
		return leaf
	}
	return base + "/" + leaf
}

func (p *PlugTelemetry) hasCustomFullTopic() bool {
	if p.FullTopic != "" && p.FullTopic != "%prefix%/%topic%/" {
		return true
	}
	defaults := [3]string{"cmnd", "stat", "tele"}
	for i, d := range defaults {
		if p.Prefixes[i] != "" && p.Prefixes[i] != d {
			return true
		}
	}
	return false
}

func (p *PlugTelemetry) normalizePowerValue(raw string) (string, bool) {
	v := strings.Trim(strings.TrimSpace(raw), "\"")
	if v == "" {
		return "", false
	}
	if p.StateTexts[1] != "" && strings.EqualFold(v, p.StateTexts[1]) {
		return "ON", true
	}
	if p.StateTexts[0] != "" && strings.EqualFold(v, p.StateTexts[0]) {
		return "OFF", true
	}
	switch strings.ToUpper(v) {
	case "ON", "1", "TRUE":
		return "ON", true
	case "OFF", "0", "FALSE":
		return "OFF", true
	default:
		return "", false
	}
}

func (p *PlugTelemetry) onlineWord() string {
	if p.OnlineText != "" {
		return p.OnlineText
	}
	return "Online"
}

func (p *PlugTelemetry) offlineWord() string {
	if p.OfflineText != "" {
		return p.OfflineText
	}
	return "Offline"
}

type stateEvent struct {
	State string
	Err   string
}

type stateWaiter struct {
	topic string
	relay int
	ch    chan stateEvent
}

var errBrokerOffline = errors.New("mqtt broker not connected")

type autoOffTracker struct {
	sawActive  bool
	finishedAt *time.Time
	// fired says the plug was turned off for the print before. Nothing more
	// happens until the printer is seen printing again.
	fired bool
	// jobID is the print that was seen running, when the printer named it.
	jobID string
	// kept says why the printer stayed on after its last print: cancelled or
	// failed. It is empty once a new print is seen.
	kept string
	// offFault is why the plug could not be turned off when the wait was
	// over. The turn-off is tried again until it works.
	offFault string
	// restored says the tracker was read from autooff.json and the printer
	// was not seen since. The first look at the printer drops a print it has
	// left behind.
	restored bool
}

// skippedPrint follows the one print a person switched auto-off away for, on
// a printer that turns off by default.
type skippedPrint struct {
	sawPrint bool
	ended    bool
	jobID    string
}

type axisSample struct {
	X           float64
	Y           float64
	Z           float64
	SampledAt   time.Time
	LastMovedAt time.Time
}

// PowerSample is one timestamped power draw point for the usage graph.
type PowerSample struct {
	Timestamp int64   `json:"ts"`
	Watts     float64 `json:"watts"`
}

// PowerSession tracks energy consumed while the printer plug is turned ON.
type PowerSession struct {
	Active      bool       `json:"active"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	DurationSec int        `json:"duration_sec"`
	EnergyWh    float64    `json:"energy_wh"`
	AvgWatts    float64    `json:"avg_watts"`
	PeakWatts   float64    `json:"peak_watts"`
}

// PrintPowerRecord tracks energy consumed for a single print job / file.
type PrintPowerRecord struct {
	JobID       string     `json:"job_id,omitempty"`
	FileName    string     `json:"file_name"`
	Active      bool       `json:"active"`
	Outcome     string     `json:"outcome,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	DurationSec int        `json:"duration_sec"`
	EnergyWh    float64    `json:"energy_wh"`
	AvgWatts    float64    `json:"avg_watts"`
	PeakWatts   float64    `json:"peak_watts"`
	// MeteredSec is how long the plug was on with a reading during the print.
	// EndEstimated says the print ended while nothing was sampled, so its end
	// is the last sample before that time.
	MeteredSec   float64 `json:"metered_sec,omitempty"`
	EndEstimated bool    `json:"end_estimated,omitempty"`
	// AwaySince is when the printer went away during the print. It is set
	// only on a running print, and gone once the printer is back on it or the
	// print is closed.
	AwaySince *time.Time `json:"away_since,omitempty"`
}

// DeviceUsage holds persisted and live power usage metrics for one printer.
type DeviceUsage struct {
	History      []PowerSample      `json:"history,omitempty"`
	Session      *PowerSession      `json:"session,omitempty"`
	CurrentPrint *PrintPowerRecord  `json:"current_print,omitempty"`
	PrintHistory []PrintPowerRecord `json:"print_history,omitempty"`
	LastSampleAt time.Time          `json:"last_sample_at,omitempty"`
}

type module struct {
	devices *deviceapi.Client
	// access is the host's access.check, asked before a route changes
	// anything (access.go).
	access accessChecker
	// hints keeps the answers that only decide what a page offers.
	hints hintCache
	// principal reads the person a request is for. It is nil outside tests,
	// which read it with routesapi.PrincipalOf.
	principal func(*http.Request) string

	held atomic.Pointer[Settings]

	mu      sync.RWMutex
	dataDir string
	// What the host sent on plugin.configure. A pinned value is one the
	// operator wrote in the host's own settings; it wins over settings.json.
	pinnedMQTTHost    string
	pinnedHAURL       string
	suggestedHTTPHost string
	// builtinPort is the port granted for the built-in broker, 0 when none is.
	builtinPort    int
	mqttClient     mqtt.Client
	mqttUp         bool
	mqttBroker     string
	mqttError      string
	mqttWarning    string
	embeddedBroker *EmbeddedBroker
	httpClient     *http.Client
	plugs          map[string]*PlugTelemetry
	configs        map[string]DeviceConfig
	trackers       map[string]*autoOffTracker
	skips          map[string]*skippedPrint
	axes           map[string]*axisSample
	usage          map[string]*DeviceUsage
	lastUsageSaved time.Time
	waiters        []*stateWaiter
	// autoOffSaved is autooff.json as it was last written or read.
	autoOffSaved []byte
	// autoOffSaveMu makes one save of autooff.json follow another, so the
	// state taken last is the one written last.
	autoOffSaveMu sync.Mutex
	// configsSaveMu and usageSaveMu do the same for configs.json and
	// usage.json. Each is taken before mu, never while mu is held.
	configsSaveMu sync.Mutex
	usageSaveMu   sync.Mutex
	// idle is since when each printer is on with nothing to do, for the
	// printers an idle time counts for.
	idle map[string]*idleClock
	// lost follows a plug that went away during a print (events.go).
	lost map[string]*lostWatch

	// events tells the host's event stream. It is nil where there is no
	// session, as in tests. eventsOff says the host does not serve the call,
	// eventOff names the endpoints the operator did not approve, and
	// eventLogged the ones a dropped event was logged for (events.go).
	events      eventEmitter
	eventsOff   atomic.Bool
	eventOff    sync.Map
	eventLogged sync.Map

	// conn is the session with the host. It is nil in tests. hostJobs is the
	// newest print of each printer as the host's event stream told it, and
	// jobCursor where that stream is read on from (jobfeed.go). Both are
	// under mu.
	conn      *wirerpc.Conn
	hostJobs  map[string]hostJob
	jobCursor string
	// jobRetry is the wait before the stream is asked for again. It is zero
	// outside tests, which shorten it.
	jobRetry time.Duration

	// energy is the energy record kept in files (energy.go), and energyHeld
	// its settings.
	energy     energyStore
	energyHeld atomic.Pointer[EnergySettings]
	// prices keeps the price of sessions and prints the printer page was
	// answered with (energy_cost.go).
	prices priceMemo
	// now is the clock. It is nil outside tests, which read time.Now.
	now func() time.Time
}

// clock is the time the recorder and the energy routes go by.
func (m *module) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func newModule(conn *wirerpc.Conn) *module {
	m := &module{
		devices:    deviceapi.NewClient(conn),
		access:     accessapi.NewClient(conn),
		httpClient: newHTTPClient(5 * time.Second),
		plugs:      make(map[string]*PlugTelemetry),
		configs:    make(map[string]DeviceConfig),
		trackers:   make(map[string]*autoOffTracker),
		skips:      make(map[string]*skippedPrint),
		axes:       make(map[string]*axisSample),
		usage:      make(map[string]*DeviceUsage),
		conn:       conn,
		hostJobs:   make(map[string]hostJob),
		idle:       make(map[string]*idleClock),
		lost:       make(map[string]*lostWatch),
	}
	// A nil session inside the interface would not be nil.
	if conn != nil {
		m.events = busapi.NewClient(conn)
	}
	def := defaultSettings()
	m.held.Store(&def)
	m.energy.pending = make(map[string]map[string]*[slotsPerDay]slotAcc)
	m.energy.days = make(map[string]*cachedDay)
	energyDef := defaultEnergySettings()
	m.energyHeld.Store(&energyDef)
	m.prices.held = make(map[priceMemoKey]priceMemoEntry)
	return m
}

func (m *module) settings() Settings {
	if s := m.held.Load(); s != nil {
		return *s
	}
	return defaultSettings()
}

// setHostConfig keeps what plugin.configure carried beside the settings: the
// values the operator pinned in the host's own settings, and the port granted
// for the built-in broker.
func (m *module) setHostConfig(sent hostSettings, grants wirerpc.Grants) {
	port := 0
	if grant, ok := grants.Listen[builtinListenName]; ok && grant.Port > 0 {
		port = grant.Port
	}
	m.mu.Lock()
	m.pinnedMQTTHost = strings.TrimSpace(sent.MQTTHost)
	m.pinnedHAURL = normalizeBaseURL(sent.HAURL)
	m.suggestedHTTPHost = normalizeHTTPHost(sent.HTTPHost)
	m.builtinPort = port
	m.mu.Unlock()
}

// startSettings is what the module runs on after plugin.configure: what
// settings.json holds, or the defaults on an install that has saved nothing
// yet, with the values the operator pinned written over either.
func (m *module) startSettings(sent hostSettings) Settings {
	effective := defaultSettings()
	if stored, ok := m.loadSettingsFromDisk(); ok {
		effective = stored
	} else if legacy := normalizeSettings(sent.Settings); legacy.MQTTHost != "" || legacy.MQTTBuiltin || legacy.HAURL != "" {
		// Migrate legacy host-passed settings into settings.json once.
		effective = legacy
		_ = m.writeSettingsToDisk(effective)
	}

	return withoutMovedSecrets(normalizeSettings(effective), m.applyHostPins(effective))
}

// withoutMovedSecrets is next without a saved password or token whose address
// a pin in the host's settings changed. A secret is used only with the
// address it was saved for. Only the running settings lose it, so removing
// the pin brings it back.
func withoutMovedSecrets(saved, next Settings) Settings {
	if next.MQTTPassword != "" && !sameBroker(saved, next) {
		next.MQTTPassword = ""
		next.HasPassword = false
	}
	if next.HAToken != "" && haOrigin(saved.HAURL) != haOrigin(next.HAURL) {
		next.HAToken = ""
		next.HasHAToken = false
	}
	return next
}

// applyHostPins writes the values the operator pinned in the host's settings
// over s, so the two places a host can be written never disagree.
func (m *module) applyHostPins(s Settings) Settings {
	m.mu.RLock()
	host, haURL := m.pinnedMQTTHost, m.pinnedHAURL
	m.mu.RUnlock()
	if host != "" {
		s.MQTTHost = host
	}
	if haURL != "" {
		s.HAURL = haURL
	}
	return normalizeSettings(s)
}

func (m *module) grantedBuiltinPort() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.builtinPort
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", logName, err)
		os.Exit(1)
	}
}

// moduleInfo is what the extension says of itself on plugin.info.
func moduleInfo() wirerpc.Info {
	return wirerpc.Info{
		Name:     name,
		Version:  release,
		Contract: wirerpc.Contract,
		Serves:   []wirerpc.Method{wirerpc.MethodConfigure, diagnosticsapi.MethodCollect},
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	line, err := wire.Handshake{
		Protocol: wire.ProtocolMajor,
		MinMinor: wire.MinMinor,
		MaxMinor: wire.MaxMinor,
		Address:  wire.StdioAddress,
	}.Line()
	if err != nil {
		return fmt.Errorf("render handshake: %w", err)
	}
	if _, err = os.Stdout.WriteString(line); err != nil {
		return fmt.Errorf("write handshake: %w", err)
	}

	var mod *module
	conn, err := wirerpc.NewConn(stdio{}, wirerpc.Options{
		Role: wirerpc.RoleChild,
		Methods: wirerpc.Methods{
			wirerpc.MethodInfo: func(context.Context, wirerpc.Call) (any, error) {
				return routesapi.Describe(moduleInfo(), openAPI)
			},
			// A file for the diagnostics bundle an administrator downloads. A
			// host that does not know the call never makes it.
			diagnosticsapi.MethodCollect: diagnosticsapi.Serve(func(_ context.Context, cursor string) (diagnosticsapi.Page, error) {
				return mod.collectDiagnostics(cursor), nil
			}),
			wirerpc.MethodConfigure: wirerpc.ConfigureGranted(func(_ context.Context, sent hostSettings, grants wirerpc.Grants) error {
				mod.configureData(grants.Data)
				mod.setHostConfig(sent, grants)

				effective := mod.startSettings(sent)

				mod.held.Store(&effective)
				mod.reconnectMQTT(effective)
				return nil
			}),
			wirerpc.MethodShutdown: func(context.Context, wirerpc.Call) (any, error) {
				mod.saveOnExit()
				mod.disconnectMQTT()
				return nil, nil
			},
			routesapi.MethodServe: routesapi.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mod.routes().ServeHTTP(w, r)
			})),
		},
	})
	if err != nil {
		return fmt.Errorf("build wirerpc session: %w", err)
	}

	mod = newModule(conn)

	// Start background auto-off loop once the pipe is serving.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		mod.autoOffLoop(ctx)
	}()

	// Print start, end and outcome as the host tells them. A host that does
	// not serve the stream ends this at once, and the poll is what remains.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		mod.followJobs(ctx, func(ctx context.Context, params busapi.SubscribeParams, onWindow func(busapi.Window) error) error {
			return busapi.Subscribe(ctx, conn, params, onWindow)
		})
	}()

	fmt.Fprintf(os.Stderr, "%s %s serving on stdio (pid %d)\n", logName, release, os.Getpid())
	if err := conn.Serve(ctx); err != nil {
		mod.saveOnExit()
		mod.disconnectMQTT()
		return fmt.Errorf("serve: %w", err)
	}
	mod.saveOnExit()
	mod.disconnectMQTT()
	return nil
}

// --- Persistence (in grants.Data) ---

// setDataDir keeps the granted data directory and says whether it changed.
func (m *module) setDataDir(dir string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.dataDir != dir
	m.dataDir = dir
	return changed
}

// configureData takes the data directory plugin.configure granted. The plug
// assignments are read every time. Usage and energy are read only when the
// directory changed, so a second plugin.configure keeps what memory holds and
// has not been saved yet.
func (m *module) configureData(dir string) {
	if dir == "" {
		return
	}
	m.mu.RLock()
	fresh := m.dataDir != dir
	m.mu.RUnlock()
	if fresh {
		// Before the directory is taken into use, so no write of this process
		// is under way in it.
		removeStaleTemps(dir)
	}
	changed := m.setDataDir(dir)
	m.loadAllConfigs()
	if changed {
		m.loadAllUsage()
		m.loadAutoOff()
		m.loadEnergy()
	}
}

// removeStaleTemps deletes what a write that was cut off left in the data
// directory. writeFileAtomic writes beside its target under a name of its
// own, and a process that is stopped before the rename leaves that file
// behind. Those of settings.json and configs.json hold the saved secrets.
// Only the temporary names of the five files kept in this directory are
// touched, also the fixed name earlier versions wrote to.
func removeStaleTemps(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		for _, target := range []string{settingsFileName, configsFileName, usageFileName, energySettingsFile, autoOffFileName} {
			if strings.HasPrefix(name, target+".") {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					fmt.Fprintf(os.Stderr, "%s: delete %s: %v\n", logName, name, err)
				}
				break
			}
		}
	}
}

// saveOnExit writes what memory still holds when the extension stops. The
// energy record goes first, like on every tick, so a print is in its own file
// before usage.json stops naming it as running.
func (m *module) saveOnExit() {
	if err := m.saveEnergy(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: save energy: %v\n", logName, err)
	}
	if err := m.saveAllUsage(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: save usage: %v\n", logName, err)
	}
	m.saveAutoOffIfChanged()
}

func (m *module) loadSettingsFromDisk() (Settings, bool) {
	m.mu.RLock()
	dir := m.dataDir
	m.mu.RUnlock()
	if dir == "" {
		return Settings{}, false
	}

	raw, err := os.ReadFile(filepath.Join(dir, settingsFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s: read settings: %v\n", logName, err)
		}
		return Settings{}, false
	}

	var loaded Settings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		fmt.Fprintf(os.Stderr, "%s: parse settings: %v\n", logName, err)
		return Settings{}, false
	}
	return normalizeSettings(loaded), true
}

func (m *module) writeSettingsToDisk(s Settings) error {
	m.mu.RLock()
	dir := m.dataDir
	m.mu.RUnlock()
	if dir == "" {
		return nil
	}

	toWrite := s
	toWrite.HasPassword = false
	toWrite.KeepPassword = false
	toWrite.HasHAToken = false
	toWrite.KeepHAToken = false
	toWrite.MQTTBroker = ""

	return writeJSONAtomic(filepath.Join(dir, settingsFileName), toWrite)
}

func writeJSONAtomic(target string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(target, raw)
}

// writeFileAtomic writes raw beside target under a name of its own, syncs it,
// and renames it over target, so two writers never share a temporary file.
func writeFileAtomic(target string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (m *module) loadAllConfigs() {
	m.mu.RLock()
	dir := m.dataDir
	m.mu.RUnlock()
	if dir == "" {
		return
	}

	raw, err := os.ReadFile(filepath.Join(dir, configsFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s: read configs: %v\n", logName, err)
		}
		return
	}

	var loaded map[string]DeviceConfig
	if err := json.Unmarshal(raw, &loaded); err != nil {
		fmt.Fprintf(os.Stderr, "%s: parse configs: %v\n", logName, err)
		return
	}

	m.mu.Lock()
	for id, cfg := range loaded {
		m.configs[id] = normalizeDeviceConfig(cfg)
	}
	m.mu.Unlock()

	m.syncDriverSubscriptions()
	for _, cfg := range loaded {
		ncfg := normalizeDeviceConfig(cfg)
		if ncfg.configured() {
			go m.refreshConfigState(context.Background(), ncfg)
		}
	}
}

func (m *module) refreshConfigState(ctx context.Context, cfg DeviceConfig) {
	switch cfg.effectiveDriver() {
	case DriverTasmota:
		if cfg.TasmotaTopic != "" {
			m.queryPlugState(cfg.TasmotaTopic)
		}
	case DriverTasmotaHTTP:
		if cfg.HTTPHost != "" {
			_ = m.pollHTTPPlug(ctx, cfg)
		}
	case DriverShellyHTTP:
		if cfg.HTTPHost != "" {
			_ = m.pollShellyHTTPPlug(ctx, cfg)
		}
	case DriverHomeAssistant:
		if cfg.HAEntityID != "" {
			_ = m.pollHomeAssistantPlug(ctx, cfg)
		}
	case DriverShellyGen1, DriverShellyGen2, DriverZigbee2MQTT, DriverESPHome, DriverOpenBeken, DriverGenericMQTT:
		m.syncDriverSubscriptions()
		m.queryDriverState(cfg)
	}
}

func cloneDeviceUsage(u *DeviceUsage) *DeviceUsage {
	if u == nil {
		return nil
	}
	cp := &DeviceUsage{
		LastSampleAt: u.LastSampleAt,
	}
	if len(u.History) > 0 {
		cp.History = append([]PowerSample(nil), u.History...)
	}
	if u.Session != nil {
		s := *u.Session
		if u.Session.EndedAt != nil {
			t := *u.Session.EndedAt
			s.EndedAt = &t
		}
		cp.Session = &s
	}
	if u.CurrentPrint != nil {
		p := *u.CurrentPrint
		if u.CurrentPrint.EndedAt != nil {
			t := *u.CurrentPrint.EndedAt
			p.EndedAt = &t
		}
		if u.CurrentPrint.AwaySince != nil {
			t := *u.CurrentPrint.AwaySince
			p.AwaySince = &t
		}
		cp.CurrentPrint = &p
	}
	if len(u.PrintHistory) > 0 {
		cp.PrintHistory = make([]PrintPowerRecord, len(u.PrintHistory))
		for i, item := range u.PrintHistory {
			cp.PrintHistory[i] = item
			if item.EndedAt != nil {
				t := *item.EndedAt
				cp.PrintHistory[i].EndedAt = &t
			}
		}
	}
	return cp
}

func (m *module) loadAllUsage() {
	m.mu.RLock()
	dir := m.dataDir
	m.mu.RUnlock()
	if dir == "" {
		return
	}

	raw, err := os.ReadFile(filepath.Join(dir, usageFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s: read usage: %v\n", logName, err)
		}
		return
	}

	var loaded map[string]*DeviceUsage
	if err := json.Unmarshal(raw, &loaded); err != nil {
		fmt.Fprintf(os.Stderr, "%s: parse usage: %v\n", logName, err)
		return
	}

	m.mu.Lock()
	for id, u := range loaded {
		if u == nil {
			continue
		}
		u.History = trimHistory(u.History, time.Now(), powerHistoryKeep)
		if len(u.PrintHistory) > maxPrintHistory {
			u.PrintHistory = u.PrintHistory[:maxPrintHistory]
		}
		m.usage[id] = u
	}
	m.lastUsageSaved = time.Now()
	m.mu.Unlock()
}

func (m *module) saveAllUsage() error {
	m.usageSaveMu.Lock()
	defer m.usageSaveMu.Unlock()
	m.mu.Lock()
	dir := m.dataDir
	if dir == "" {
		m.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]*DeviceUsage, len(m.usage))
	for k, u := range m.usage {
		if u != nil {
			snapshot[k] = cloneDeviceUsage(u)
		}
	}
	m.lastUsageSaved = time.Now()
	m.mu.Unlock()

	return writeJSONAtomic(filepath.Join(dir, usageFileName), snapshot)
}

func (m *module) shouldPeriodicSaveUsage(interval time.Duration) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastUsageSaved.IsZero() || time.Since(m.lastUsageSaved) >= interval
}

func (m *module) getConfig(_ context.Context, deviceID string) (DeviceConfig, error) {
	m.mu.RLock()
	cfg := m.configs[deviceID]
	m.mu.RUnlock()
	return cfg, nil
}

func normalizeDeviceConfig(cfg DeviceConfig) DeviceConfig {
	d := cfg.effectiveDriver()

	clearHTTP := func() {
		cfg.HTTPHost = ""
		cfg.HTTPUser = ""
		cfg.HTTPPassword = ""
	}
	clearGeneric := func() {
		cfg.MQTTCommandTopic = ""
		cfg.MQTTStateTopic = ""
		cfg.MQTTAvailabilityTopic = ""
		cfg.MQTTPayloadOn = ""
		cfg.MQTTPayloadOff = ""
		cfg.MQTTStateOn = ""
		cfg.MQTTStateOff = ""
		cfg.MQTTStateJSONKey = ""
		cfg.MQTTPowerTopic = ""
		cfg.MQTTPowerJSONKey = ""
	}
	clearHA := func() {
		cfg.HAEntityID = ""
		cfg.HAPowerEntityID = ""
	}

	switch d {
	case DriverTasmotaHTTP, DriverShellyHTTP:
		cfg.HTTPHost = normalizeHTTPHost(cfg.HTTPHost)
		cfg.HTTPUser = strings.TrimSpace(cfg.HTTPUser)
		cfg.TasmotaTopic = ""
		clearGeneric()
		clearHA()
		if cfg.HTTPHost == "" {
			cfg.Driver = ""
			cfg.Mode = ""
			clearHTTP()
		} else {
			cfg.Driver = d
			cfg.Mode = "http"
		}
	case DriverHomeAssistant:
		cfg.HAEntityID = strings.TrimSpace(cfg.HAEntityID)
		cfg.HAPowerEntityID = strings.TrimSpace(cfg.HAPowerEntityID)
		cfg.TasmotaTopic = ""
		clearHTTP()
		clearGeneric()
		if cfg.HAEntityID == "" {
			cfg.Driver = ""
			cfg.Mode = ""
			clearHA()
		} else {
			cfg.Driver = d
			cfg.Mode = "ha"
		}
	case DriverGenericMQTT:
		cfg.MQTTCommandTopic = strings.TrimSpace(cfg.MQTTCommandTopic)
		cfg.MQTTStateTopic = strings.TrimSpace(cfg.MQTTStateTopic)
		cfg.MQTTAvailabilityTopic = strings.TrimSpace(cfg.MQTTAvailabilityTopic)
		cfg.MQTTPayloadOn = strings.TrimSpace(cfg.MQTTPayloadOn)
		cfg.MQTTPayloadOff = strings.TrimSpace(cfg.MQTTPayloadOff)
		cfg.MQTTStateOn = strings.TrimSpace(cfg.MQTTStateOn)
		cfg.MQTTStateOff = strings.TrimSpace(cfg.MQTTStateOff)
		cfg.MQTTStateJSONKey = strings.TrimSpace(cfg.MQTTStateJSONKey)
		cfg.MQTTPowerTopic = strings.TrimSpace(cfg.MQTTPowerTopic)
		cfg.MQTTPowerJSONKey = strings.TrimSpace(cfg.MQTTPowerJSONKey)
		cfg.TasmotaTopic = ""
		clearHTTP()
		clearHA()
		if cfg.MQTTCommandTopic == "" {
			cfg.Driver = ""
			cfg.Mode = ""
			clearGeneric()
		} else {
			cfg.Driver = d
			cfg.Mode = "mqtt"
		}
	case DriverShellyGen1:
		cfg.TasmotaTopic = normalizeShellyGen1ID(cfg.TasmotaTopic)
		clearHTTP()
		clearGeneric()
		clearHA()
		if cfg.TasmotaTopic == "" {
			cfg.Driver = ""
			cfg.Mode = ""
		} else {
			cfg.Driver = d
			cfg.Mode = "mqtt"
		}
	case DriverZigbee2MQTT:
		cfg.TasmotaTopic = normalizeZ2MTopic(cfg.TasmotaTopic)
		clearHTTP()
		clearGeneric()
		clearHA()
		if cfg.TasmotaTopic == "" {
			cfg.Driver = ""
			cfg.Mode = ""
		} else {
			cfg.Driver = d
			cfg.Mode = "mqtt"
		}
	case DriverShellyGen2, DriverESPHome, DriverOpenBeken:
		cfg.TasmotaTopic = normalizeMQTTPrefix(cfg.TasmotaTopic)
		clearHTTP()
		clearGeneric()
		clearHA()
		if cfg.TasmotaTopic == "" {
			cfg.Driver = ""
			cfg.Mode = ""
		} else {
			cfg.Driver = d
			cfg.Mode = "mqtt"
		}
	default:
		cfg.TasmotaTopic = normalizeTopic(cfg.TasmotaTopic)
		clearHTTP()
		clearGeneric()
		clearHA()
		if cfg.TasmotaTopic != "" {
			cfg.Driver = DriverTasmota
			cfg.Mode = "mqtt"
		} else {
			cfg.Driver = ""
			cfg.Mode = ""
		}
	}

	cfg.HasHTTPPassword = cfg.HTTPPassword != ""
	cfg.KeepHTTPPassword = false
	if cfg.CooldownSec != nil {
		wait := clampCooldownSec(*cfg.CooldownSec)
		cfg.CooldownSec = &wait
	}
	if cfg.IdleOffMin != nil {
		own := clampIdleOffMin(*cfg.IdleOffMin)
		cfg.IdleOffMin = &own
	}
	if !cfg.configured() {
		cfg.RelayIndex = 0
	} else if cfg.Driver == DriverHomeAssistant || cfg.Driver == DriverGenericMQTT {
		cfg.RelayIndex = 1
	} else {
		cfg.RelayIndex = cfg.relayIndex()
	}
	return cfg
}

func (m *module) saveConfig(_ context.Context, deviceID string, cfg DeviceConfig) error {
	keepHTTPPw := cfg.KeepHTTPPassword
	cfg = normalizeDeviceConfig(cfg)

	m.configsSaveMu.Lock()
	defer m.configsSaveMu.Unlock()
	m.mu.Lock()
	if existing, ok := m.configs[deviceID]; ok && cfg.usesHTTP() && cfg.HTTPHost != "" && cfg.HTTPPassword == "" && keepHTTPPw && existing.HTTPPassword != "" {
		// A saved password is kept only for the plug it was saved for.
		if !strings.EqualFold(existing.HTTPHost, cfg.HTTPHost) {
			m.mu.Unlock()
			return errPlugPasswordRetype
		}
		cfg.HTTPPassword = existing.HTTPPassword
		cfg.HasHTTPPassword = cfg.HTTPPassword != ""
	}
	return m.storeConfigLocked(deviceID, cfg)
}

// storeConfigLocked puts cfg in place for deviceID, drops what no longer
// fits it and writes configs.json. The caller holds configsSaveMu and mu;
// mu is released here.
func (m *module) storeConfigLocked(deviceID string, cfg DeviceConfig) error {
	m.configs[deviceID] = cfg
	if !cfg.AutoOffAfterPrint {
		delete(m.trackers, deviceID)
	}
	if cfg.AutoOffAfterPrint || !cfg.AutoOffDefault {
		delete(m.skips, deviceID)
	}
	if !cfg.configured() {
		delete(m.idle, deviceID)
		delete(m.lost, deviceID)
	}
	dir := m.dataDir
	snapshot := make(map[string]DeviceConfig, len(m.configs))
	for k, v := range m.configs {
		snapshot[k] = v
	}
	m.mu.Unlock()

	if dir != "" {
		if err := writeJSONAtomic(filepath.Join(dir, configsFileName), snapshot); err != nil {
			return err
		}
	}

	if cfg.configured() {
		go m.refreshConfigState(context.Background(), cfg)
	}
	return nil
}

func normalizeTopic(t string) string {
	t = strings.TrimSpace(t)
	t = strings.Trim(t, "/")
	t = strings.TrimPrefix(t, "cmnd/")
	t = strings.TrimPrefix(t, "stat/")
	t = strings.TrimPrefix(t, "tele/")
	if idx := strings.LastIndex(t, "/"); idx >= 0 {
		if parsePowerLeafIndex(t[idx+1:]) >= 0 {
			t = t[:idx]
		}
	}
	return strings.Trim(t, "/")
}

// --- MQTT ---

func (m *module) disconnectMQTT() {
	m.mu.Lock()
	c := m.mqttClient
	m.mqttClient = nil
	m.mqttUp = false
	brk := m.embeddedBroker
	m.embeddedBroker = nil
	m.mu.Unlock()
	if c != nil && c.IsConnected() {
		c.Disconnect(250)
	}
	if brk != nil {
		_ = brk.Close()
	}
}

func (m *module) buildMQTTOptions(s Settings, brokerURL, clientID string) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(clientID).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(true).
		SetConnectRetry(false)

	if s.MQTTUsername != "" {
		opts.SetUsername(s.MQTTUsername)
		opts.SetPassword(s.MQTTPassword)
	}
	return opts
}

// probeBuiltinBrokerReachability checks, while the built-in broker runs, that
// the address the operator gave plugs for this server reaches it. That address
// is the broker host on the Connections tab, or the mqtt_host option, which
// module.yaml declares under egress. It opens one TCP connection to that host
// on the granted broker port and closes it again, sending nothing.
func (m *module) probeBuiltinBrokerReachability(brk *EmbeddedBroker, rawHost string) string {
	if brk == nil {
		return ""
	}
	host, _, _ := parseBrokerURL(strings.TrimSpace(rawHost))
	if host == "" || host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost") {
		return ""
	}
	// module.yaml declares the MQTT ports for this address. A broker granted
	// another port is not checked.
	if p := brk.Port(); p != defaultMQTTPort && p != defaultMQTTTLSPort {
		return ""
	}
	before := brk.AcceptedCount()
	target := net.JoinHostPort(host, strconv.Itoa(brk.Port()))
	conn, err := net.DialTimeout("tcp", target, 1500*time.Millisecond)
	if err != nil {
		return fmt.Sprintf("Built-in broker is listening internally on :%d, but %s is not reachable (%v). If Prusa Connect runs in a Docker container (bridge network), publish TCP port %d (-p %d:%d) on the container so smart plugs on your LAN can reach it — or connect the plug directly by its IP address.", brk.Port(), target, err, brk.Port(), brk.Port(), brk.Port())
	}
	time.Sleep(60 * time.Millisecond)
	_ = conn.Close()
	if brk.AcceptedCount() == before {
		return fmt.Sprintf("Another MQTT broker is already running on your network at %s! Smart plugs connecting to %s will reach that existing broker instead of this built-in broker. Disable the built-in broker and use your existing broker instead.", target, target)
	}
	return ""
}

func (m *module) reconnectMQTT(s Settings) {
	m.disconnectMQTT()
	port := m.grantedBuiltinPort()
	brokerURL := s.brokerURL()
	label := brokerURL
	if s.MQTTBuiltin && port > 0 {
		label = builtinLabel(port)
	}
	m.mu.Lock()
	m.mqttBroker = label
	m.mqttError = ""
	m.mqttWarning = ""
	m.mu.Unlock()

	if s.MQTTBuiltin {
		if port <= 0 {
			// The module listens only on a port the operator granted.
			errMsg := "The built-in broker is switched on, but its port is not approved. Approve it on the Ports tab of this extension, then save the connection again."
			m.mu.Lock()
			m.mqttError = errMsg
			m.mu.Unlock()
			fmt.Fprintf(os.Stderr, "%s: %s\n", logName, errMsg)
			return
		}
		if !s.builtinCredentialsSet() {
			m.mu.Lock()
			m.mqttError = errBuiltinNeedsCredentials
			m.mu.Unlock()
			fmt.Fprintf(os.Stderr, "%s: %s\n", logName, errBuiltinNeedsCredentials)
			return
		}
		listenAddr := fmt.Sprintf(":%d", port)
		brk, err := StartEmbeddedBroker(listenAddr, s.MQTTUsername, s.MQTTPassword)
		if err != nil {
			errMsg := fmt.Sprintf("Port %d is already in use on this host (%v). Another MQTT broker appears to be running — disable the built-in broker and connect to your existing broker instead.", port, err)
			m.mu.Lock()
			m.mqttError = errMsg
			m.mu.Unlock()
			fmt.Fprintf(os.Stderr, "%s: %s\n", logName, errMsg)
			return
		}
		m.mu.Lock()
		m.embeddedBroker = brk
		m.mu.Unlock()
		fmt.Fprintf(os.Stderr, "%s: started built-in MQTT broker on %s\n", logName, listenAddr)
		if warn := m.probeBuiltinBrokerReachability(brk, s.MQTTHost); warn != "" {
			m.mu.Lock()
			m.mqttWarning = warn
			m.mu.Unlock()
			fmt.Fprintf(os.Stderr, "%s: warning: %s\n", logName, warn)
		}
	}

	var opts *mqtt.ClientOptions
	if s.MQTTBuiltin {
		m.mu.RLock()
		brk := m.embeddedBroker
		m.mu.RUnlock()
		opts = pipeInto(m.buildMQTTOptions(s, inProcessBroker, s.MQTTClientID), brk)
	} else {
		if brokerURL == "" {
			return
		}
		opts = m.buildMQTTOptions(s, brokerURL, s.MQTTClientID)
	}

	fmt.Fprintf(os.Stderr, "%s: connecting to MQTT broker %s (client_id=%s)\n", logName, label, s.MQTTClientID)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		m.mu.Lock()
		m.mqttUp = true
		m.mqttError = ""
		var customSubs []string
		for _, p := range m.plugs {
			if p.hasCustomFullTopic() {
				if subStat := p.formatTopic(1, "+"); subStat != "" {
					customSubs = append(customSubs, subStat)
				}
				if subTele := p.formatTopic(2, "+"); subTele != "" {
					customSubs = append(customSubs, subTele)
				}
			}
		}
		m.mu.Unlock()

		c.Subscribe("tasmota/discovery/+/config", 0, m.onMQTTMessage)
		c.Subscribe("stat/+/+", 0, m.onMQTTMessage)
		c.Subscribe("tele/+/LWT", 0, m.onMQTTMessage)
		c.Subscribe("tele/+/STATE", 0, m.onMQTTMessage)
		c.Subscribe("tele/+/SENSOR", 0, m.onMQTTMessage)
		for _, sub := range customSubs {
			c.Subscribe(sub, 0, m.onMQTTMessage)
		}

		go func() {
			m.syncDriverSubscriptions()
			m.queryAllKnownPlugs()
		}()
		fmt.Fprintf(os.Stderr, "%s: connected to MQTT broker %s\n", logName, label)
	})

	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		m.mu.Lock()
		m.mqttUp = false
		if err != nil {
			m.mqttError = err.Error()
		}
		m.mu.Unlock()
		fmt.Fprintf(os.Stderr, "%s: MQTT connection lost: %v\n", logName, err)
	})

	opts.SetReconnectingHandler(func(_ mqtt.Client, _ *mqtt.ClientOptions) {
		fmt.Fprintf(os.Stderr, "%s: retrying MQTT connection to %s\n", logName, label)
	})

	client := mqtt.NewClient(opts)
	m.mu.Lock()
	m.mqttClient = client
	m.mu.Unlock()

	go func() {
		for {
			m.mu.RLock()
			current := m.mqttClient
			m.mu.RUnlock()
			if current != client {
				return
			}
			tok := client.Connect()
			if tok.WaitTimeout(8 * time.Second) {
				if err := tok.Error(); err == nil {
					return
				} else {
					m.mu.Lock()
					m.mqttError = err.Error()
					m.mu.Unlock()
					fmt.Fprintf(os.Stderr, "%s: MQTT connect failed (retrying in 10s): %v\n", logName, err)
				}
			} else {
				m.mu.Lock()
				m.mqttError = "connection timed out"
				m.mu.Unlock()
			}
			time.Sleep(10 * time.Second)
		}
	}()
}

func (m *module) testBroker(override *Settings) (bool, string, string, string) {
	saved := m.settings()
	candidate := saved
	if override != nil {
		candidate = m.applyHostPins(*override)
		if err := keepPassword(*override, saved, &candidate); err != nil {
			return false, candidate.brokerURL(), err.Error(), ""
		}
	}

	if candidate.MQTTBuiltin {
		return m.testBuiltinBroker(candidate)
	}

	brokerURL := candidate.brokerURL()
	if brokerURL == "" {
		return false, "", "Broker host is required", ""
	}
	if err := tryConnect(m.buildMQTTOptions(candidate, brokerURL, testClientID(candidate))); err != nil {
		return false, brokerURL, err.Error(), ""
	}
	return true, brokerURL, "", ""
}

// testBuiltinBroker tests the built-in broker without listening anywhere but
// on the port the operator granted. While the broker runs it is asked in
// process; while it does not, it is started on that port for the test.
func (m *module) testBuiltinBroker(candidate Settings) (bool, string, string, string) {
	port := m.grantedBuiltinPort()
	if port <= 0 {
		return false, "", "The built-in broker's port is not approved. Approve it on the Ports tab of this extension.", ""
	}
	label := builtinLabel(port)
	if !candidate.builtinCredentialsSet() {
		return false, label, errBuiltinNeedsCredentials, ""
	}

	m.mu.RLock()
	brk := m.embeddedBroker
	m.mu.RUnlock()

	warnMsg := ""
	if brk == nil || brk.Port() != port {
		tmp, err := StartEmbeddedBroker(fmt.Sprintf(":%d", port), candidate.MQTTUsername, candidate.MQTTPassword)
		if err != nil {
			return false, label, fmt.Sprintf("Port %d is already in use on this host (%v). Another MQTT broker appears to be running.", port, err), ""
		}
		defer func() { _ = tmp.Close() }()
		brk = tmp
	} else if !brk.signsIn(candidate.MQTTUsername, candidate.MQTTPassword) {
		// The broker that runs keeps the saved user name and password until
		// the new ones are saved, so the test signs in with those.
		warnMsg = "The built-in broker runs with the saved user name and password. Save to use the new ones, and set them in each plug too."
		candidate.MQTTUsername, candidate.MQTTPassword = brk.username, brk.password
	}

	opts := pipeInto(m.buildMQTTOptions(candidate, inProcessBroker, testClientID(candidate)), brk)
	if err := tryConnect(opts); err != nil {
		return false, label, err.Error(), ""
	}
	if reach := m.probeBuiltinBrokerReachability(brk, candidate.MQTTHost); reach != "" {
		warnMsg = strings.TrimSpace(warnMsg + " " + reach)
	}
	return true, label, "", warnMsg
}

// testClientID is the client id of a test connection, apart from the module's
// own so the test does not push that client off the broker.
func testClientID(s Settings) string {
	return fmt.Sprintf("%s-test-%d", s.MQTTClientID, time.Now().UnixNano()%10000)
}

// tryConnect connects once with opts and disconnects again.
func tryConnect(opts *mqtt.ClientOptions) error {
	opts.SetConnectTimeout(5 * time.Second).SetAutoReconnect(false).SetConnectRetry(false)
	tc := mqtt.NewClient(opts)
	tok := tc.Connect()
	if !tok.WaitTimeout(6 * time.Second) {
		return errors.New("connection timed out after 6s")
	}
	if err := tok.Error(); err != nil {
		return err
	}
	tc.Disconnect(100)
	return nil
}

func (m *module) discoverPlugs() {
	m.mu.RLock()
	c := m.mqttClient
	up := m.mqttUp
	m.mu.RUnlock()
	if c == nil || !up {
		return
	}
	c.Subscribe("tasmota/discovery/+/config", 0, m.onMQTTMessage)
	m.queryAllKnownPlugs()
}

func isDriverPrefixedKey(topic string) bool {
	for _, p := range []string{"http:", "shellyhttp:", "shelly1:", "shelly2:", "z2m:", "esphome:", "openbeken:", "generic:", "ha:"} {
		if strings.HasPrefix(topic, p) {
			return true
		}
	}
	return false
}

func (m *module) discoveredListLocked() []PlugTelemetry {
	out := make([]PlugTelemetry, 0, len(m.plugs))
	for topic, p := range m.plugs {
		// Show auto-discovered Tasmota plugs and Shelly Gen1 plugs in the discovery list.
		if isDriverPrefixedKey(topic) && !strings.HasPrefix(topic, "shelly1:") {
			continue
		}
		cp := p.clone()
		if strings.HasPrefix(topic, "shelly1:") {
			cp.Topic = strings.TrimPrefix(topic, "shelly1:")
			if cp.Driver == "" {
				cp.Driver = DriverShellyGen1
			}
		} else {
			cp.Topic = topic
			if cp.Driver == "" {
				cp.Driver = DriverTasmota
			}
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Driver != out[j].Driver {
			return out[i].Driver < out[j].Driver
		}
		return out[i].Topic < out[j].Topic
	})
	return out
}

// parsePowerLeafIndex returns 1 for "POWER" or "POWER1", 2..8 for "POWER2".."POWER8",
// 0 for "POWER0", or -1 if leaf is not a POWER leaf.
func parsePowerLeafIndex(leaf string) int {
	upper := strings.ToUpper(strings.TrimSpace(leaf))
	if upper == "POWER" {
		return 1
	}
	if !strings.HasPrefix(upper, "POWER") || len(upper) != 6 {
		return -1
	}
	ch := upper[5]
	if ch < '0' || ch > '8' {
		return -1
	}
	return int(ch - '0')
}

func (m *module) notifyWaitersLocked(topic string, relay int, evt stateEvent) {
	for _, w := range m.waiters {
		if w.topic == topic && (relay == 0 || w.relay == relay) {
			select {
			case w.ch <- evt:
			default:
			}
		}
	}
}

func (m *module) removeWaiter(target *stateWaiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, w := range m.waiters {
		if w == target {
			m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
			break
		}
	}
}

func (m *module) resolveIncomingTopicLocked(msgTopic string) (plug *PlugTelemetry, kind string, leaf string, ok bool) {
	for _, p := range m.plugs {
		if !p.hasCustomFullTopic() {
			continue
		}
		if statPrefix := p.formatTopic(1, ""); statPrefix != "" && strings.HasPrefix(msgTopic, statPrefix) {
			rem := strings.TrimPrefix(msgTopic, statPrefix)
			if rem != "" && !strings.Contains(rem, "/") {
				return p, "stat", rem, true
			}
		}
		if telePrefix := p.formatTopic(2, ""); telePrefix != "" && strings.HasPrefix(msgTopic, telePrefix) {
			rem := strings.TrimPrefix(msgTopic, telePrefix)
			if rem != "" && !strings.Contains(rem, "/") {
				return p, "tele", rem, true
			}
		}
	}

	parts := strings.Split(msgTopic, "/")
	if len(parts) < 3 {
		return nil, "", "", false
	}

	var topic string
	switch {
	case parts[0] == "stat" || parts[0] == "tele":
		kind = parts[0]
		topic = strings.Join(parts[1:len(parts)-1], "/")
		leaf = parts[len(parts)-1]
	case parts[len(parts)-2] == "stat" || parts[len(parts)-2] == "tele":
		kind = parts[len(parts)-2]
		topic = strings.Join(parts[:len(parts)-2], "/")
		leaf = parts[len(parts)-1]
	default:
		return nil, "", "", false
	}

	p, exists := m.plugs[topic]
	if !exists {
		p = &PlugTelemetry{Topic: topic, Power: "UNKNOWN"}
		m.plugs[topic] = p
	}
	return p, kind, leaf, true
}

func (m *module) onMQTTMessage(c mqtt.Client, msg mqtt.Message) {
	now := time.Now()
	if m.handleDriverMQTTPacket(msg.Topic(), msg.Payload(), now) {
		return
	}
	customSubs, queryTopic := m.handleMQTTPacket(msg.Topic(), msg.Payload(), now)
	if c != nil && len(customSubs) > 0 {
		for _, sub := range customSubs {
			c.Subscribe(sub, 0, m.onMQTTMessage)
		}
	}
	if queryTopic != "" {
		go m.queryPlugState(queryTopic)
	}
}

func (m *module) handleMQTTPacket(msgTopic string, payloadBytes []byte, now time.Time) (customSubs []string, queryTopic string) {
	parts := strings.Split(msgTopic, "/")
	if len(parts) == 4 && parts[0] == "tasmota" && parts[1] == "discovery" && parts[3] == "config" {
		var disc struct {
			DN    string    `json:"dn"`
			FN    []*string `json:"fn"`
			HN    string    `json:"hn"`
			MD    string    `json:"md"`
			MAC   string    `json:"mac"`
			T     string    `json:"t"`
			FT    string    `json:"ft"`
			TP    []string  `json:"tp"`
			State []string  `json:"state"`
			Onln  string    `json:"onln"`
			Ofln  string    `json:"ofln"`
			RL    []int     `json:"rl"`
		}
		if json.Unmarshal(payloadBytes, &disc) != nil || strings.TrimSpace(disc.T) == "" {
			return nil, ""
		}

		m.mu.Lock()
		plug, ok := m.plugs[disc.T]
		if !ok {
			plug = &PlugTelemetry{Topic: disc.T, Power: "UNKNOWN"}
			m.plugs[disc.T] = plug
		}

		if len(disc.FN) > 0 && disc.FN[0] != nil && strings.TrimSpace(*disc.FN[0]) != "" {
			plug.FriendlyName = strings.TrimSpace(*disc.FN[0])
		} else if strings.TrimSpace(disc.DN) != "" {
			plug.FriendlyName = strings.TrimSpace(disc.DN)
		} else if strings.TrimSpace(disc.HN) != "" && plug.FriendlyName == "" {
			plug.FriendlyName = strings.TrimSpace(disc.HN)
		}

		if disc.MD != "" {
			plug.Model = disc.MD
		}
		if disc.MAC != "" {
			plug.MAC = disc.MAC
		}
		prevLWTTopic := plug.formatTopic(2, "LWT")
		if disc.FT != "" {
			plug.FullTopic = disc.FT
		}
		if len(disc.TP) == 3 {
			for i := 0; i < 3; i++ {
				if disc.TP[i] != "" {
					plug.Prefixes[i] = disc.TP[i]
				}
			}
		}
		if plug.formatTopic(2, "LWT") != prevLWTTopic {
			plug.Online = nil
		}
		for i := 0; i < len(disc.State) && i < 4; i++ {
			if disc.State[i] != "" {
				plug.StateTexts[i] = disc.State[i]
			}
		}
		if disc.Onln != "" {
			plug.OnlineText = disc.Onln
		}
		if disc.Ofln != "" {
			plug.OfflineText = disc.Ofln
		}

		relayCount := 0
		for i, kind := range disc.RL {
			if kind > 0 && i < 8 {
				relayCount = i + 1
			}
		}
		if relayCount < 1 && plug.RelayCount < 1 {
			relayCount = 1
		}
		if relayCount > 0 {
			plug.RelayCount = relayCount
			names := make([]string, relayCount)
			for i := 0; i < relayCount && i < len(disc.FN); i++ {
				if disc.FN[i] != nil {
					names[i] = strings.TrimSpace(*disc.FN[i])
				}
			}
			plug.RelayNames = names
		}

		plug.UpdatedAt = &now
		if plug.hasCustomFullTopic() {
			if s1 := plug.formatTopic(1, "+"); s1 != "" {
				customSubs = append(customSubs, s1)
			}
			if s2 := plug.formatTopic(2, "+"); s2 != "" {
				customSubs = append(customSubs, s2)
			}
		}
		queryTopic = plug.Topic
		m.mu.Unlock()
		return customSubs, queryTopic
	}

	payload := strings.TrimSpace(string(payloadBytes))
	if payload == "" {
		// Ignore empty payloads (e.g. PowerRetain 0 clearing retained state).
		return nil, ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	plug, kind, leaf, ok := m.resolveIncomingTopicLocked(msgTopic)
	if !ok {
		return nil, ""
	}

	switch {
	case kind == "tele" && leaf == "LWT":
		// Ignore stale LWT messages arriving on an old topic layout.
		if plug.FullTopic != "" && msgTopic != plug.formatTopic(2, "LWT") {
			return nil, ""
		}
		if strings.EqualFold(payload, plug.onlineWord()) {
			wasOnline := plug.Online != nil && *plug.Online
			online := true
			plug.Online = &online
			plug.UpdatedAt = &now
			if !wasOnline {
				queryTopic = plug.Topic
			}
		} else if strings.EqualFold(payload, plug.offlineWord()) {
			online := false
			plug.Online = &online
			plug.UpdatedAt = &now
		}

	case leaf == "SENSOR" || strings.EqualFold(leaf, "STATUS8") || strings.EqualFold(leaf, "STATUS10"):
		if !strings.HasPrefix(payload, "{") {
			return nil, ""
		}
		var sns struct {
			ENERGY    *energyPayload `json:"ENERGY"`
			StatusSNS *struct {
				ENERGY *energyPayload `json:"ENERGY"`
			} `json:"StatusSNS"`
		}
		if json.Unmarshal(payloadBytes, &sns) == nil {
			if sns.ENERGY != nil {
				applyEnergyReading(plug, sns.ENERGY, now)
			} else if sns.StatusSNS != nil && sns.StatusSNS.ENERGY != nil {
				applyEnergyReading(plug, sns.StatusSNS.ENERGY, now)
			}
		}

	case kind == "stat" && strings.EqualFold(leaf, "WARNING"):
		// MaxPower: the plug measured more than its limit and turned its
		// relays off. It says so once, and again each time it tries anew.
		var warning map[string]json.RawMessage
		if json.Unmarshal(payloadBytes, &warning) != nil {
			return nil, ""
		}
		_, reached := warning["MaxPowerReached"]
		_, retry := warning["MaxPowerReachedRetry"]
		if reached || retry {
			plug.setProtection(0, protectOverpower)
			plug.UpdatedAt = &now
		}

	case strings.EqualFold(leaf, "RESULT") || leaf == "STATE" || strings.EqualFold(leaf, "STATUS11") || parsePowerLeafIndex(leaf) >= 0:
		if strings.HasPrefix(payload, "{") {
			var rawMap map[string]json.RawMessage
			if json.Unmarshal(payloadBytes, &rawMap) != nil {
				return nil, ""
			}
			if stsRaw, hasSTS := rawMap["StatusSTS"]; hasSTS {
				var stsMap map[string]json.RawMessage
				if json.Unmarshal(stsRaw, &stsMap) == nil {
					rawMap = stsMap
				}
			}
			plug.setTasmotaWifi(rawMap["Wifi"])
			if raw, has := rawMap["PowerOnState"]; has {
				plug.setAfterPowerCut(0, tasmotaAfterPowerCut(raw))
			}
			if cmdRaw, hasCmd := rawMap["Command"]; hasCmd {
				var cmdStr string
				if json.Unmarshal(cmdRaw, &cmdStr) == nil && strings.EqualFold(cmdStr, "Error") {
					plug.LastError = "Tasmota returned Command: Error"
					errRelay := 0
					if inRaw, hasIn := rawMap["Input"]; hasIn {
						var inStr string
						if json.Unmarshal(inRaw, &inStr) == nil {
							fields := strings.Fields(inStr)
							if len(fields) > 0 {
								if idx := parsePowerLeafIndex(fields[0]); idx > 0 {
									errRelay = idx
								}
							}
						}
					}
					m.notifyWaitersLocked(plug.Topic, errRelay, stateEvent{
						Err: "Tasmota rejected command (Command: Error — check relay index)",
					})
					return nil, ""
				}
			}

			applyKey := func(relayIdx int, key string) {
				rawVal, exists := rawMap[key]
				if !exists {
					return
				}
				var valStr string
				if json.Unmarshal(rawVal, &valStr) != nil {
					return
				}
				if norm, valid := plug.normalizePowerValue(valStr); valid {
					plug.setRelayState(relayIdx, norm)
					plug.LastError = ""
					plug.UpdatedAt = &now
					// A relay that is on again says the protection is over.
					if norm == "ON" {
						plug.setProtection(0, "")
					}
					m.notifyWaitersLocked(plug.Topic, relayIdx, stateEvent{State: norm})
				}
			}

			applyKey(1, "POWER")
			for idx := 1; idx <= 8; idx++ {
				applyKey(idx, fmt.Sprintf("POWER%d", idx))
			}
		} else if relayIdx := parsePowerLeafIndex(leaf); relayIdx > 0 {
			if norm, valid := plug.normalizePowerValue(payload); valid {
				plug.setRelayState(relayIdx, norm)
				plug.LastError = ""
				plug.UpdatedAt = &now
				if norm == "ON" {
					plug.setProtection(0, "")
				}
				m.notifyWaitersLocked(plug.Topic, relayIdx, stateEvent{State: norm})
			}
		}
	}

	return nil, queryTopic
}

type energyPayload struct {
	Power   *float64 `json:"Power"`
	Voltage *float64 `json:"Voltage"`
	Current *float64 `json:"Current"`
}

func applyEnergyReading(plug *PlugTelemetry, energy *energyPayload, now time.Time) {
	if energy == nil {
		return
	}
	plug.PowerWatts = energy.Power
	if energy.Power != nil {
		plug.meterAt = now
	}
	plug.Voltage = energy.Voltage
	plug.CurrentAmps = energy.Current
	plug.UpdatedAt = &now
}

// plugQueryLocked is what a Tasmota plug is asked when it is to say how it
// is: the state of each relay and its own state, which carries the Wi-Fi
// signal, and what it does after a power cut (PowerOnState, which an empty
// payload reads and does not change), each with an empty payload, and the
// topic of the meter reading.
// The caller holds m.mu.
func (m *module) plugQueryLocked(topic string) (cmdTopics []string, statusTopic string) {
	plug := m.plugs[topic]
	maxRelay := 1
	if plug != nil && plug.RelayCount > maxRelay {
		maxRelay = plug.RelayCount
	}
	for _, cfg := range m.configs {
		if cfg.TasmotaTopic == topic && cfg.relayIndex() > maxRelay {
			maxRelay = cfg.relayIndex()
		}
	}
	if maxRelay > 8 {
		maxRelay = 8
	}
	cmdTopics = make([]string, 0, maxRelay+1)
	for r := 1; r <= maxRelay; r++ {
		leaf := fmt.Sprintf("POWER%d", r)
		if plug != nil {
			cmdTopics = append(cmdTopics, plug.formatTopic(0, leaf))
		} else {
			cmdTopics = append(cmdTopics, "cmnd/"+topic+"/"+leaf)
		}
	}
	statusTopic = "cmnd/" + topic + "/Status"
	stateTopic := "cmnd/" + topic + "/STATE"
	afterCutTopic := "cmnd/" + topic + "/PowerOnState"
	if plug != nil {
		statusTopic = plug.formatTopic(0, "Status")
		stateTopic = plug.formatTopic(0, "STATE")
		afterCutTopic = plug.formatTopic(0, "PowerOnState")
	}
	return append(cmdTopics, stateTopic, afterCutTopic), statusTopic
}

// queryPlugState asks a Tasmota plug how it is. It runs when the broker
// connects, when a plug is found or comes back, and after a switch or a
// change of its set-up, not on every tick.
func (m *module) queryPlugState(topic string) {
	if topic == "" {
		return
	}
	m.mu.RLock()
	c := m.mqttClient
	up := m.mqttUp
	if c == nil || !up {
		m.mu.RUnlock()
		return
	}
	cmdTopics, statusTopic := m.plugQueryLocked(topic)
	m.mu.RUnlock()

	for _, ct := range cmdTopics {
		c.Publish(ct, 1, false, "")
	}
	c.Publish(statusTopic, 1, false, "8")
}

func (m *module) queryAllKnownPlugs() {
	m.mu.RLock()
	seen := make(map[string]struct{})
	var topics []string
	var driverConfigs []DeviceConfig
	for topic := range m.plugs {
		if topic != "" && !isDriverPrefixedKey(topic) {
			seen[topic] = struct{}{}
			topics = append(topics, topic)
		}
	}
	for _, cfg := range m.configs {
		d := cfg.effectiveDriver()
		if d == DriverTasmota && cfg.TasmotaTopic != "" {
			if _, ok := seen[cfg.TasmotaTopic]; !ok {
				seen[cfg.TasmotaTopic] = struct{}{}
				topics = append(topics, cfg.TasmotaTopic)
			}
		} else if d == DriverShellyGen2 || d == DriverZigbee2MQTT {
			driverConfigs = append(driverConfigs, cfg)
		}
	}
	m.mu.RUnlock()

	for _, t := range topics {
		m.queryPlugState(t)
	}
	for _, cfg := range driverConfigs {
		m.queryDriverState(cfg)
	}
}

func (m *module) pollAssignedPlugsEnergy() {
	m.mu.RLock()
	c := m.mqttClient
	up := m.mqttUp
	var statusPubs []string
	var unknownPubs []string
	var pollConfigs []DeviceConfig
	var queryDriverConfigs []DeviceConfig
	seenStatus := make(map[string]struct{})
	seenUnknown := make(map[string]struct{})
	seenPoll := make(map[string]struct{})
	seenQuery := make(map[string]struct{})
	for _, cfg := range m.configs {
		if !cfg.configured() {
			continue
		}
		d := cfg.effectiveDriver()
		switch d {
		case DriverTasmotaHTTP, DriverShellyHTTP, DriverHomeAssistant:
			key := cfg.plugKey()
			if _, dup := seenPoll[key]; !dup {
				seenPoll[key] = struct{}{}
				pollConfigs = append(pollConfigs, cfg)
			}
			continue
		case DriverShellyGen2, DriverZigbee2MQTT:
			if c != nil && up {
				key := cfg.plugKey()
				if _, dup := seenQuery[key]; !dup {
					seenQuery[key] = struct{}{}
					queryDriverConfigs = append(queryDriverConfigs, cfg)
				}
			}
			continue
		case DriverTasmota:
			// handled below
		default:
			// Shelly Gen1, ESPHome, OpenBeken, Generic MQTT push telemetry on MQTT
			continue
		}
		if c == nil || !up {
			continue
		}
		t := cfg.TasmotaTopic
		plug := m.plugs[t]
		if _, dup := seenStatus[t]; !dup {
			seenStatus[t] = struct{}{}
			if plug != nil {
				statusPubs = append(statusPubs, plug.formatTopic(0, "Status"))
			} else {
				statusPubs = append(statusPubs, "cmnd/"+t+"/Status")
			}
		}
		relayIdx := cfg.relayIndex()
		key := fmt.Sprintf("%s#%d", t, relayIdx)
		if _, dup := seenUnknown[key]; !dup && (plug == nil || plug.relayState(relayIdx) == "UNKNOWN") {
			seenUnknown[key] = struct{}{}
			leaf := fmt.Sprintf("POWER%d", relayIdx)
			if plug != nil {
				unknownPubs = append(unknownPubs, plug.formatTopic(0, leaf))
			} else {
				unknownPubs = append(unknownPubs, "cmnd/"+t+"/"+leaf)
			}
		}
	}
	m.mu.RUnlock()

	for _, pub := range unknownPubs {
		c.Publish(pub, 1, false, "")
	}
	for _, pub := range statusPubs {
		c.Publish(pub, 1, false, "8")
	}
	for _, cfg := range queryDriverConfigs {
		m.queryDriverState(cfg)
	}
	if len(pollConfigs) > 0 {
		var wg sync.WaitGroup
		for _, cfg := range pollConfigs {
			wg.Add(1)
			go func(pc DeviceConfig) {
				defer wg.Done()
				switch pc.effectiveDriver() {
				case DriverTasmotaHTTP:
					_ = m.pollHTTPPlug(context.Background(), pc)
				case DriverShellyHTTP:
					_ = m.pollShellyHTTPPlug(context.Background(), pc)
				case DriverHomeAssistant:
					_ = m.pollHomeAssistantPlug(context.Background(), pc)
				}
			}(cfg)
		}
		wg.Wait()
	}
}

func (m *module) switchPlugVerified(ctx context.Context, topic string, relayIndex int, wantOn bool) error {
	relayIndex = normalizeRelayIndex(relayIndex)
	wantState := "OFF"
	payload := "0"
	if wantOn {
		wantState = "ON"
		payload = "1"
	}

	m.mu.Lock()
	c := m.mqttClient
	up := m.mqttUp
	if c == nil || !up {
		m.mu.Unlock()
		return errBrokerOffline
	}
	plug := m.plugs[topic]
	if plug != nil && plug.Online != nil && !*plug.Online {
		m.mu.Unlock()
		return errors.New("smart plug is offline (LWT Offline)")
	}
	cmdLeaf := fmt.Sprintf("POWER%d", relayIndex)
	cmdTopic := "cmnd/" + topic + "/" + cmdLeaf
	if plug != nil {
		cmdTopic = plug.formatTopic(0, cmdLeaf)
	}
	waiter := &stateWaiter{
		topic: topic,
		relay: relayIndex,
		ch:    make(chan stateEvent, 8),
	}
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()
	defer m.removeWaiter(waiter)

	tok := c.Publish(cmdTopic, 1, false, payload)
	if !tok.WaitTimeout(3 * time.Second) {
		return errors.New("MQTT publish timed out")
	}
	if err := tok.Error(); err != nil {
		return err
	}

	overallTimer := time.NewTimer(2500 * time.Millisecond)
	defer overallTimer.Stop()

	var mismatchTimer *time.Timer
	var mismatchC <-chan time.Time
	defer func() {
		if mismatchTimer != nil {
			mismatchTimer.Stop()
		}
	}()
	lastMismatch := ""

	for {
		select {
		case evt := <-waiter.ch:
			if evt.Err != "" {
				return errors.New(evt.Err)
			}
			if evt.State == wantState {
				return nil
			}
			if evt.State != "" && evt.State != wantState {
				lastMismatch = evt.State
				if mismatchTimer == nil {
					// Allow a brief grace window in case an in-flight tele/STATE arrived right before the command reply.
					mismatchTimer = time.NewTimer(450 * time.Millisecond)
					mismatchC = mismatchTimer.C
				}
			}
		case <-mismatchC:
			return fmt.Errorf("plug refused to switch (reported %s; check PowerLock or PowerOnState)", lastMismatch)
		case <-overallTimer.C:
			if lastMismatch != "" {
				return fmt.Errorf("plug refused to switch (reported %s; check PowerLock or PowerOnState)", lastMismatch)
			}
			return errors.New("plug did not confirm state change within 2.5s")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *module) getHTTPClient() *http.Client {
	if m.httpClient != nil {
		return m.httpClient
	}
	return newHTTPClient(5 * time.Second)
}

// newHTTPClient returns the client for plugs and Home Assistant. It follows no
// redirect: a redirect may name a host that no egress entry declares, so the
// 3xx answer is handed back and the caller treats it as an error.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// redirectError is the error for a 3xx answer, which is never followed.
func redirectError(who string, status int) error {
	return fmt.Errorf("%s answered with a redirect (HTTP %d); enter the final address instead", who, status)
}

func isRedirect(status int) bool { return status >= 300 && status < 400 }

func (m *module) tasmotaHTTPGet(ctx context.Context, cfg DeviceConfig, cmnd string) ([]byte, error) {
	base := cfg.httpBaseURL()
	if base == "" {
		return nil, errors.New("HTTP plug host or IP is required")
	}
	q := url.Values{}
	if cfg.HTTPPassword != "" {
		user := strings.TrimSpace(cfg.HTTPUser)
		if user == "" {
			user = "admin"
		}
		q.Set("user", user)
		q.Set("password", cfg.HTTPPassword)
	}
	q.Set("cmnd", cmnd)

	reqCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		reqCtx, cancel = context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
	}

	// The query carries the plug's password. It is set after the request is
	// built, and an error of the request is told without its address, so the
	// password is in no error text.
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/cm", nil)
	if err != nil {
		return nil, withoutURL(err)
	}
	req.URL.RawQuery = q.Encode()
	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	defer resp.Body.Close()
	if isRedirect(resp.StatusCode) {
		return nil, redirectError("plug", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("HTTP authentication failed (check Web Password)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("plug returned HTTP %d", resp.StatusCode)
	}

	var warnCheck struct {
		Warning string `json:"WARNING"`
	}
	if json.Unmarshal(body, &warnCheck) == nil && warnCheck.Warning != "" {
		return nil, fmt.Errorf("plug auth error: %s", warnCheck.Warning)
	}
	return body, nil
}

func (m *module) pollHTTPPlug(ctx context.Context, cfg DeviceConfig) error {
	key := cfg.plugKey()
	if key == "" {
		return errors.New("HTTP plug host is required")
	}

	raw, err := m.tasmotaHTTPGet(ctx, cfg, "Status 0")
	if err != nil {
		m.mu.Lock()
		plug := m.plugs[key]
		if plug == nil {
			plug = &PlugTelemetry{Topic: key, FriendlyName: cfg.HTTPHost, Power: "UNKNOWN"}
			m.plugs[key] = plug
		}
		offline := false
		plug.Online = &offline
		plug.LastError = err.Error()
		m.mu.Unlock()
		return err
	}

	var parsed struct {
		Status struct {
			DeviceName   string          `json:"DeviceName"`
			FriendlyName []string        `json:"FriendlyName"`
			PowerOnState json.RawMessage `json:"PowerOnState"`
		} `json:"Status"`
		StatusFWR struct {
			Hardware string `json:"Hardware"`
		} `json:"StatusFWR"`
		StatusSNS struct {
			ENERGY *energyPayload `json:"ENERGY"`
		} `json:"StatusSNS"`
		StatusSTS map[string]json.RawMessage `json:"StatusSTS"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("invalid Status 0 JSON from plug: %w", err)
	}

	now := time.Now()
	online := true
	m.mu.Lock()
	plug := m.plugs[key]
	if plug == nil {
		plug = &PlugTelemetry{Topic: key, Power: "UNKNOWN"}
		m.plugs[key] = plug
	}
	plug.Topic = key
	plug.Online = &online
	plug.LastError = ""
	plug.UpdatedAt = &now

	if parsed.StatusFWR.Hardware != "" && plug.Model == "" {
		plug.Model = parsed.StatusFWR.Hardware
	}
	if len(parsed.Status.FriendlyName) > 0 {
		plug.RelayNames = make([]string, len(parsed.Status.FriendlyName))
		copy(plug.RelayNames, parsed.Status.FriendlyName)
		if parsed.Status.FriendlyName[0] != "" {
			plug.FriendlyName = parsed.Status.FriendlyName[0]
		}
	}
	if plug.FriendlyName == "" && parsed.Status.DeviceName != "" {
		plug.FriendlyName = parsed.Status.DeviceName
	}
	if plug.FriendlyName == "" {
		plug.FriendlyName = cfg.HTTPHost
	}

	maxRelay := plug.RelayCount
	for k, v := range parsed.StatusSTS {
		idx := parsePowerLeafIndex(k)
		if idx <= 0 {
			continue
		}
		var stateVal string
		if json.Unmarshal(v, &stateVal) != nil {
			continue
		}
		if norm, ok := plug.normalizePowerValue(stateVal); ok {
			plug.setRelayState(idx, norm)
			if idx > maxRelay {
				maxRelay = idx
			}
		}
	}
	if len(parsed.Status.FriendlyName) > maxRelay {
		maxRelay = len(parsed.Status.FriendlyName)
	}
	if maxRelay > 0 {
		plug.RelayCount = maxRelay
	}
	if parsed.StatusSNS.ENERGY != nil {
		applyEnergyReading(plug, parsed.StatusSNS.ENERGY, now)
	}
	plug.setTasmotaWifi(parsed.StatusSTS["Wifi"])
	plug.setAfterPowerCut(0, tasmotaAfterPowerCut(parsed.Status.PowerOnState))
	m.mu.Unlock()
	return nil
}

func (m *module) switchHTTPPlugVerified(ctx context.Context, cfg DeviceConfig, relayIndex int, wantOn bool) error {
	key := cfg.plugKey()
	if key == "" {
		return errors.New("HTTP plug host is required")
	}
	relayIndex = normalizeRelayIndex(relayIndex)
	wantState := "OFF"
	cmdVal := "0"
	if wantOn {
		wantState = "ON"
		cmdVal = "1"
	}

	raw, err := m.tasmotaHTTPGet(ctx, cfg, fmt.Sprintf("POWER%d %s", relayIndex, cmdVal))
	if err != nil {
		return err
	}

	var respMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &respMap); err != nil {
		return fmt.Errorf("invalid HTTP response from plug: %w", err)
	}
	if cmdRaw, ok := respMap["Command"]; ok {
		var cmdStatus string
		if json.Unmarshal(cmdRaw, &cmdStatus) == nil &&
			(strings.EqualFold(cmdStatus, "Error") || strings.EqualFold(cmdStatus, "Unknown")) {
			return fmt.Errorf("plug rejected command POWER%d (check Relay index)", relayIndex)
		}
	}

	now := time.Now()
	online := true
	m.mu.Lock()
	plug := m.plugs[key]
	if plug == nil {
		plug = &PlugTelemetry{Topic: key, FriendlyName: cfg.HTTPHost, Power: "UNKNOWN"}
		m.plugs[key] = plug
	}

	matchedState := ""
	for k, v := range respMap {
		idx := parsePowerLeafIndex(k)
		if idx != relayIndex {
			continue
		}
		var stateVal string
		if json.Unmarshal(v, &stateVal) != nil {
			continue
		}
		if norm, ok := plug.normalizePowerValue(stateVal); ok {
			plug.setRelayState(idx, norm)
			matchedState = norm
		}
	}
	if matchedState != "" {
		plug.Online = &online
		plug.LastError = ""
		plug.UpdatedAt = &now
	}
	m.mu.Unlock()

	if matchedState == wantState {
		return nil
	}
	if matchedState != "" {
		return fmt.Errorf("plug refused to switch (reported %s; check PowerLock or PowerOnState)", matchedState)
	}
	return errors.New("plug did not confirm state change in HTTP response")
}

func (m *module) switchDevicePower(ctx context.Context, cfg DeviceConfig, wantOn bool) error {
	switch cfg.effectiveDriver() {
	case DriverTasmotaHTTP:
		return m.switchHTTPPlugVerified(ctx, cfg, cfg.relayIndex(), wantOn)
	case DriverShellyHTTP:
		return m.switchShellyHTTPPlugVerified(ctx, cfg, wantOn)
	case DriverHomeAssistant:
		return m.switchHomeAssistantPlugVerified(ctx, cfg, wantOn)
	case DriverTasmota:
		return m.switchPlugVerified(ctx, cfg.TasmotaTopic, cfg.relayIndex(), wantOn)
	default:
		return m.switchMQTTDriverVerified(ctx, cfg, wantOn)
	}
}

// --- Printer Activity & Safety Interlock ---

type printerSafety struct {
	DeviceName     string
	Connected      bool
	PrinterState   string
	ActiveJob      bool
	Moving         bool
	CanTurnOff     bool
	LockReason     string
	JobID          string
	JobFile        string
	LastJobID      string
	LastJobOutcome string
	// JobStarted is when the server first saw the running print, and
	// LastJobEnded when it saw the print before stop. Both are from the
	// server's own job record, and empty where it has none.
	JobStarted   time.Time
	LastJobEnded *time.Time
	// JobOver says the printer's state still names the print the server has
	// just closed. The print record ends with the server's record, and no
	// second record is begun for the moment the state lags behind.
	JobOver bool
}

// closedJobLag is how long a printer's state may go on naming a print the
// server has closed. After that the printer is taken at its word again.
const closedJobLag = 2 * time.Minute

// closedJobStillNamed reports whether the print the printer's state names,
// by the number the printer gave it, is the one the server closed a moment
// ago, with no print open.
func closedJobStillNamed(jobs deviceapi.Jobs, stateJobID string, now time.Time) bool {
	if jobs.Current != nil || jobs.Last == nil || jobs.Last.Ended == nil || stateJobID == "" {
		return false
	}
	if strings.TrimSpace(jobs.Last.DeviceJobID) != stateJobID {
		return false
	}
	since := now.Sub(*jobs.Last.Ended)
	return since >= 0 && since <= closedJobLag
}

func readStateString(state map[string]json.RawMessage, key string) string {
	raw, ok := state[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func readStateToken(state map[string]json.RawMessage, key string) string {
	raw, ok := state[key]
	if !ok {
		return ""
	}
	if s := readStateString(state, key); s != "" {
		return s
	}
	trimmed := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	return trimmed
}

func cleanJobFileName(displayName, filePath, deviceJobID string) string {
	if d := strings.TrimSpace(displayName); d != "" {
		return d
	}
	if p := strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/"); p != "" {
		base := path.Base(p)
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	if id := strings.TrimSpace(deviceJobID); id != "" {
		return "Print #" + id
	}
	return ""
}

func readAxisFloat(state map[string]json.RawMessage, path string) (float64, bool) {
	raw, ok := state[path]
	if !ok {
		return 0, false
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	return v, true
}

func (m *module) updateAxisMotion(deviceID string, state map[string]json.RawMessage) bool {
	x, hasX := readAxisFloat(state, "/axis/x")
	y, hasY := readAxisFloat(state, "/axis/y")
	z, hasZ := readAxisFloat(state, "/axis/z")
	if !hasX && !hasY && !hasZ {
		return false
	}

	now := time.Now()
	const moveThreshold = 0.05
	const settleWindow = 8 * time.Second

	m.mu.Lock()
	defer m.mu.Unlock()

	prev, ok := m.axes[deviceID]
	if !ok {
		m.axes[deviceID] = &axisSample{
			X:         x,
			Y:         y,
			Z:         z,
			SampledAt: now,
		}
		return false
	}

	if !prev.SampledAt.IsZero() && now.Sub(prev.SampledAt) < 30*time.Second {
		if (hasX && math.Abs(x-prev.X) > moveThreshold) ||
			(hasY && math.Abs(y-prev.Y) > moveThreshold) ||
			(hasZ && math.Abs(z-prev.Z) > moveThreshold) {
			prev.LastMovedAt = now
		}
	}

	if hasX {
		prev.X = x
	}
	if hasY {
		prev.Y = y
	}
	if hasZ {
		prev.Z = z
	}
	prev.SampledAt = now

	return !prev.LastMovedAt.IsZero() && now.Sub(prev.LastMovedAt) < settleWindow
}

// stateUnreachable is the state a refused turn-off names for a printer that
// is not connected.
const stateUnreachable = "UNREACHABLE"

// offRefusal says whether a request to turn a printer off is refused, and
// with what reason and state. idleOnly is a request that is to turn off idle
// printers only: a printer Connect cannot reach may be printing, so it is
// refused too.
func offRefusal(safety printerSafety, idleOnly bool) (reason, state string, refused bool) {
	if !safety.CanTurnOff {
		return safety.LockReason, safety.PrinterState, true
	}
	if idleOnly && !safety.Connected {
		return "Connect cannot reach the printer", stateUnreachable, true
	}
	return "", "", false
}

func (m *module) checkSafety(ctx context.Context, principal, deviceID string) (printerSafety, error) {
	dev, err := m.devices.Get(ctx, deviceapi.GetParams{Principal: principal, ID: deviceID})
	if err != nil {
		return printerSafety{}, err
	}

	devName := strings.TrimSpace(dev.Name)
	if devName == "" {
		devName = deviceID
	}

	var stateStr string
	if raw, ok := dev.State["/state"]; ok {
		_ = json.Unmarshal(raw, &stateStr)
	}
	stateUpper := strings.ToUpper(strings.TrimSpace(stateStr))

	jobs, err := m.devices.Jobs(ctx, deviceapi.JobsParams{Principal: principal, ID: deviceID})
	activeJob := err == nil && jobs.Current != nil && jobs.Current.Ended == nil

	var jobID, jobFile, lastJobID, lastJobOutcome string
	var jobStarted time.Time
	var lastJobEnded *time.Time
	var jobOver bool
	if err == nil {
		if jobs.Current != nil && jobs.Current.Ended == nil {
			jobStarted = jobs.Current.Started
			jobID = strings.TrimSpace(jobs.Current.ID)
			if jobID == "" {
				jobID = strings.TrimSpace(jobs.Current.DeviceJobID)
			}
			jobFile = cleanJobFileName(jobs.Current.DisplayName, jobs.Current.Path, jobs.Current.DeviceJobID)
		}
		if jobs.Last != nil {
			lastJobID = strings.TrimSpace(jobs.Last.ID)
			lastJobOutcome = strings.TrimSpace(jobs.Last.Outcome)
			if jobs.Last.Ended != nil {
				ended := *jobs.Last.Ended
				lastJobEnded = &ended
			}
		}
	}
	if jobFile == "" {
		stateDisplay := readStateString(dev.State, "/job/display")
		statePath := readStateString(dev.State, "/job/path")
		stateJobID := readStateToken(dev.State, "/job/id")
		if err == nil && closedJobStillNamed(jobs, stateJobID, m.clock()) {
			jobOver = true
			jobID = lastJobID
		} else if jobID == "" && stateJobID != "" {
			jobID = stateJobID
		}
		jobFile = cleanJobFileName(stateDisplay, statePath, stateJobID)
	}

	moving := dev.Connected && m.updateAxisMotion(deviceID, dev.State)

	s := printerSafety{
		DeviceName:     devName,
		Connected:      dev.Connected,
		PrinterState:   stateUpper,
		ActiveJob:      activeJob,
		Moving:         moving,
		CanTurnOff:     true,
		JobID:          jobID,
		JobFile:        jobFile,
		LastJobID:      lastJobID,
		LastJobOutcome: lastJobOutcome,
		JobStarted:     jobStarted,
		LastJobEnded:   lastJobEnded,
		JobOver:        jobOver,
	}

	if !dev.Connected {
		return s, nil
	}

	switch stateUpper {
	case "PRINTING", "PAUSED", "BUSY", "ATTENTION":
		s.CanTurnOff = false
		s.LockReason = fmt.Sprintf("Printer is currently %s", strings.ToLower(stateUpper))
		return s, nil
	}

	if activeJob {
		s.CanTurnOff = false
		s.LockReason = "Printer has an active print job"
		return s, nil
	}

	if moving {
		s.CanTurnOff = false
		s.LockReason = "Printer axes are still moving"
		return s, nil
	}

	return s, nil
}

// accumulateEnergy adds wh, the energy of this tick as energyStepLocked
// decided it, and brings the duration, the peak and the average up to now.
func accumulateEnergy(now, startedAt time.Time, watts, wh float64, durationSec *int, energyWh, avgWatts, peakWatts *float64) {
	*energyWh += wh
	*durationSec = int(now.Sub(startedAt).Seconds())
	if watts > *peakWatts {
		*peakWatts = watts
	}
	if *durationSec > 0 {
		*avgWatts = *energyWh / (float64(*durationSec) / 3600.0)
	}
}

func (m *module) updateDeviceUsage(deviceID string, cfg DeviceConfig, safety printerSafety) (dirtyDisk bool, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p := m.plugs[cfg.plugKey()]
	if p == nil {
		return false, false
	}

	relayPower := p.relayState(cfg.relayIndex())
	plugOn := relayPower == "ON"
	plugOff := relayPower == "OFF"
	if !plugOn && !plugOff {
		return false, false
	}

	watts := 0.0
	if w, ok := meterWatts(p); ok && plugOn {
		watts = w
	}

	now := m.clock()
	u := m.usage[deviceID]
	if u == nil {
		u = &DeviceUsage{}
		m.usage[deviceID] = u
	}

	// prevSampleAt is the sample before this tick. A print that is found
	// ended after a long time with no sample ended somewhere in that time.
	prevSampleAt := u.LastSampleAt
	sampleDue := u.LastSampleAt.IsZero() || now.Sub(u.LastSampleAt) >= 2*time.Second
	var dt time.Duration
	// step is what this tick adds. The session, the print and the 15-minute
	// slot all take it, so they never disagree. It is zero when no sample is
	// due.
	var step energyStep
	if sampleDue {
		if !u.LastSampleAt.IsZero() && now.After(u.LastSampleAt) && now.Sub(u.LastSampleAt) <= 2*time.Minute {
			dt = now.Sub(u.LastSampleAt)
		}
		step = m.energyStepLocked(cfg, p, now, dt, plugOn)
		// The slots take the step at the end, once it is known whether it
		// went into a print.
		defer func() { m.recordEnergyLocked(deviceID, now.Add(-dt), now, step) }()
		u.LastSampleAt = now
		u.History = append(u.History, PowerSample{
			Timestamp: now.Unix(),
			Watts:     math.Round(watts*10) / 10,
		})
		u.History = trimHistory(u.History, now, powerHistoryKeep)
	}

	if plugOn {
		if u.Session == nil || !u.Session.Active {
			u.Session = &PowerSession{
				Active:    true,
				StartedAt: now,
				EnergyWh:  step.wh,
				PeakWatts: watts,
			}
			dirtyDisk = true
		} else {
			accumulateEnergy(now, u.Session.StartedAt, watts, step.wh,
				&u.Session.DurationSec, &u.Session.EnergyWh, &u.Session.AvgWatts, &u.Session.PeakWatts)
		}
	} else if plugOff && u.Session != nil && u.Session.Active {
		u.Session.Active = false
		ended := now
		u.Session.EndedAt = &ended
		u.Session.DurationSec = int(now.Sub(u.Session.StartedAt).Seconds())
		if u.Session.DurationSec > 0 {
			u.Session.AvgWatts = u.Session.EnergyWh / (float64(u.Session.DurationSec) / 3600.0)
		}
		dirtyDisk = true
	}

	rec := u.CurrentPrint
	open := func() bool { return u.CurrentPrint != nil && u.CurrentPrint.Active }
	host := m.hostJobs[deviceID]

	printingNow := safety.Connected && !safety.JobOver && (safety.ActiveJob || safety.PrinterState == "PRINTING" || safety.PrinterState == "PAUSED")
	finishingMoves := open() && safety.Connected && safety.Moving
	printActive := printingNow || finishingMoves

	// finalizeCurrentPrint closes the running print. outcome is empty when it
	// is to be read from the server's job record and the printer's state.
	// endAt is zero for a print that ended now, and else the moment the print
	// was last known to run.
	finalizeCurrentPrint := func(outcome string, endAt time.Time) {
		if !open() {
			return
		}
		cur := u.CurrentPrint
		_, knownEnd, known := jobFacts(host, cur.JobID, safety)
		cur.Active = false
		cur.AwaySince = nil
		ended := now
		switch {
		case !endAt.IsZero():
			// The print was lost sight of at endAt. The server's record of
			// its end counts when it has one that is not earlier.
			ended = endAt
			cur.EndEstimated = true
			if knownEnd != nil && outcome != "interrupted" && !knownEnd.Before(endAt) {
				ended = *knownEnd
				cur.EndEstimated = false
			}
		case !prevSampleAt.IsZero() && now.Sub(prevSampleAt) > 2*time.Minute:
			// Nothing was sampled for a while, for one because the extension
			// was not running. The print ended in that time, and the last
			// sample is the latest moment it is known to have run.
			ended = prevSampleAt
			cur.EndEstimated = true
		case knownEnd != nil:
			ended = *knownEnd
		}
		if ended.After(now) {
			ended = now
		}
		if ended.Before(cur.StartedAt) {
			ended = cur.StartedAt
		}
		cur.EndedAt = &ended
		cur.DurationSec = int(ended.Sub(cur.StartedAt).Seconds())
		cur.MeteredSec = math.Round(cur.MeteredSec)
		if cur.DurationSec > 0 {
			cur.AvgWatts = cur.EnergyWh / (float64(cur.DurationSec) / 3600.0)
		}
		if outcome == "" {
			outcome = known
		}
		if outcome == "" {
			if safety.LastJobID != "" && safety.LastJobID == cur.JobID && safety.LastJobOutcome != "" {
				outcome = safety.LastJobOutcome
			} else if safety.PrinterState == "STOPPED" {
				outcome = "cancelled"
			} else {
				outcome = "finished"
			}
		}
		cur.Outcome = outcome
		doneCopy := *cur
		u.PrintHistory = append([]PrintPowerRecord{doneCopy}, u.PrintHistory...)
		if len(u.PrintHistory) > maxPrintHistory {
			u.PrintHistory = u.PrintHistory[:maxPrintHistory]
		}
		m.queuePrintLocked(deviceID, doneCopy)
		dirtyDisk = true
	}

	// A printer that drops off the network says nothing about its print. The
	// record stays open: for printAwayGrace it goes on counting, after that
	// it waits, and it is closed only when the print is known to be over.
	hold := false
	switch {
	case !open():
	case !safety.Connected:
		if plugOff {
			// No power, no print.
			at := now
			if rec.AwaySince != nil {
				at = *rec.AwaySince
			}
			finalizeCurrentPrint("interrupted", at)
			break
		}
		if rec.AwaySince == nil {
			at := now
			rec.AwaySince = &at
			dirtyDisk = true
		}
		switch away := now.Sub(*rec.AwaySince); {
		case away > printAwayMax:
			finalizeCurrentPrint("interrupted", *rec.AwaySince)
		case away <= printAwayGrace:
			printActive = true
		default:
			hold = true
		}
	case rec.AwaySince != nil:
		switch {
		case sameJob(rec, safety) && stateRunning(safety):
			rec.AwaySince = nil
			dirtyDisk = true
			printActive = true
		case safety.ActiveJob && sameJob(rec, safety) && now.Sub(*rec.AwaySince) <= printAwayMax:
			// The server still holds the print open and the printer has not
			// said yet what it does.
			hold = true
		default:
			finalizeCurrentPrint(outcomeAfterAbsence(rec, safety, host), *rec.AwaySince)
		}
	}

	switch {
	case hold:
	case printActive:
		if open() && safety.Connected && safety.JobID != "" && u.CurrentPrint.JobID != "" && u.CurrentPrint.JobID != safety.JobID {
			finalizeCurrentPrint("", time.Time{})
		}
		// This step's energy goes into a print record, so the print share of
		// the slots equals the print records.
		step.printing = true
		if !open() {
			fileName := safety.JobFile
			if fileName == "" {
				fileName = "Active Print"
			}
			// The server's record of the start counts when it lies in this
			// step. An older one is a print that ran before it was seen here.
			startedAt := now
			if at, _, _ := jobFacts(host, safety.JobID, safety); !at.IsZero() && at.After(prevSampleAt) && !at.After(now) && now.Sub(at) <= 2*time.Minute {
				startedAt = at
			}
			u.CurrentPrint = &PrintPowerRecord{
				JobID:     safety.JobID,
				FileName:  fileName,
				Active:    true,
				StartedAt: startedAt,
				EnergyWh:  step.wh,
				PeakWatts: watts,
			}
			if step.metered {
				u.CurrentPrint.MeteredSec = dt.Seconds()
			}
			dirtyDisk = true
		} else {
			if safety.JobID != "" && u.CurrentPrint.JobID == "" {
				u.CurrentPrint.JobID = safety.JobID
			}
			if safety.JobFile != "" && (u.CurrentPrint.FileName == "" || u.CurrentPrint.FileName == "Active Print") {
				u.CurrentPrint.FileName = safety.JobFile
				dirtyDisk = true
			}
			accumulateEnergy(now, u.CurrentPrint.StartedAt, watts, step.wh,
				&u.CurrentPrint.DurationSec, &u.CurrentPrint.EnergyWh, &u.CurrentPrint.AvgWatts, &u.CurrentPrint.PeakWatts)
			if step.metered {
				u.CurrentPrint.MeteredSec += dt.Seconds()
			}
		}
	case open():
		finalizeCurrentPrint("", time.Time{})
	}

	active = (u.Session != nil && u.Session.Active) || open()
	return dirtyDisk, active
}

// printAwayGrace is how long a print goes on counting energy while its
// printer is away. printAwayMax is how long its record is kept open.
const (
	printAwayGrace = 15 * time.Minute
	printAwayMax   = 24 * time.Hour
)

// sameJob reports whether the printer may be on the print of rec: it is,
// unless both name their print and the names differ.
func sameJob(rec *PrintPowerRecord, s printerSafety) bool {
	return rec.JobID == "" || s.JobID == "" || rec.JobID == s.JobID
}

// stateRunning reports a printer that says it is on a print.
func stateRunning(s printerSafety) bool {
	return s.PrinterState == "PRINTING" || s.PrinterState == "PAUSED" || s.PrinterState == "ATTENTION"
}

// knownOutcome is how a print ended in the words of the print table, or
// empty for a word that says nothing.
func knownOutcome(raw string) string {
	switch word := strings.ToLower(strings.TrimSpace(raw)); word {
	case "finished", "cancelled", "failed":
		return word
	}
	return ""
}

// jobFacts is what the server's job record says about the print jobID: when
// it started, when it ended and how. The event stream is read first
// (jobfeed.go), then the row the poll carries. Every answer is empty where
// neither names that print.
func jobFacts(host hostJob, jobID string, s printerSafety) (started time.Time, ended *time.Time, outcome string) {
	if jobID == "" {
		return time.Time{}, nil, ""
	}
	if host.JobID == jobID {
		started, ended, outcome = host.Started, host.Ended, knownOutcome(host.Outcome)
	}
	if started.IsZero() && s.JobID == jobID {
		started = s.JobStarted
	}
	if s.LastJobID == jobID {
		if ended == nil {
			ended = s.LastJobEnded
		}
		if outcome == "" {
			outcome = knownOutcome(s.LastJobOutcome)
		}
	}
	return started, ended, outcome
}

// outcomeAfterAbsence is how the print of rec ended, read when its printer
// is back and no longer on it. A print nothing says to be over in its own
// way was cut.
func outcomeAfterAbsence(rec *PrintPowerRecord, s printerSafety, host hostJob) string {
	if _, _, outcome := jobFacts(host, rec.JobID, s); outcome != "" {
		return outcome
	}
	if rec.JobID != "" && s.LastJobID == rec.JobID {
		return "interrupted"
	}
	switch s.PrinterState {
	case "FINISHED":
		return "finished"
	case "STOPPED":
		return "cancelled"
	case "ERROR":
		return "failed"
	}
	return "interrupted"
}

// --- An armed auto-off kept over a restart ---

// autoOffFileName holds what the auto-off logic follows of each printer, so
// that a restart of the server or the extension does not lose a print that
// was seen running or a wait that began.
const autoOffFileName = "autooff.json"

type savedTracker struct {
	SawActive  bool       `json:"saw_active"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Fired      bool       `json:"fired"`
	JobID      string     `json:"job_id,omitempty"`
	Kept       string     `json:"kept,omitempty"`
}

type savedSkip struct {
	SawPrint bool   `json:"saw_print"`
	Ended    bool   `json:"ended"`
	JobID    string `json:"job_id,omitempty"`
}

// savedIdle is since when a printer is on with nothing to do.
type savedIdle struct {
	Since time.Time `json:"since"`
}

type savedPrinter struct {
	Tracker *savedTracker `json:"tracker,omitempty"`
	Skip    *savedSkip    `json:"skip,omitempty"`
	Idle    *savedIdle    `json:"idle,omitempty"`
}

type savedAutoOff struct {
	V        int                      `json:"v"`
	Printers map[string]*savedPrinter `json:"printers"`
}

// autoOffSnapshotLocked is what autooff.json is to hold. A printer nothing
// is followed of is left out. Why a turn-off failed is not kept: the next
// try says it again.
func (m *module) autoOffSnapshotLocked() savedAutoOff {
	out := savedAutoOff{V: 1, Printers: make(map[string]*savedPrinter)}
	of := func(id string) *savedPrinter {
		p := out.Printers[id]
		if p == nil {
			p = &savedPrinter{}
			out.Printers[id] = p
		}
		return p
	}
	for id, tr := range m.trackers {
		if tr == nil || (!tr.sawActive && tr.finishedAt == nil && !tr.fired && tr.jobID == "" && tr.kept == "") {
			continue
		}
		saved := &savedTracker{SawActive: tr.sawActive, Fired: tr.fired, JobID: tr.jobID, Kept: tr.kept}
		if tr.finishedAt != nil {
			at := tr.finishedAt.UTC()
			saved.FinishedAt = &at
		}
		of(id).Tracker = saved
	}
	for id, skip := range m.skips {
		if skip == nil || *skip == (skippedPrint{}) {
			continue
		}
		of(id).Skip = &savedSkip{SawPrint: skip.sawPrint, Ended: skip.ended, JobID: skip.jobID}
	}
	for id, clock := range m.idle {
		if clock == nil || clock.since.IsZero() {
			continue
		}
		of(id).Idle = &savedIdle{Since: clock.since.UTC()}
	}
	return out
}

// saveAutoOffIfChanged writes autooff.json when what it is to hold differs
// from what it holds.
func (m *module) saveAutoOffIfChanged() {
	m.autoOffSaveMu.Lock()
	defer m.autoOffSaveMu.Unlock()
	m.mu.RLock()
	dir := m.dataDir
	raw, err := json.Marshal(m.autoOffSnapshotLocked())
	same := err == nil && string(raw) == string(m.autoOffSaved)
	m.mu.RUnlock()
	if dir == "" || err != nil || same {
		return
	}
	if err = writeFileAtomic(filepath.Join(dir, autoOffFileName), raw); err != nil {
		fmt.Fprintf(os.Stderr, "%s: save auto-off state: %v\n", logName, err)
		return
	}
	m.mu.Lock()
	m.autoOffSaved = raw
	m.mu.Unlock()
}

// loadAutoOff reads autooff.json. Only what still fits a printer's config is
// taken: a print is followed for a printer that is armed, and a skipped print
// for one that turns off by default and is switched off for now.
func (m *module) loadAutoOff() {
	m.mu.RLock()
	dir := m.dataDir
	m.mu.RUnlock()
	if dir == "" {
		return
	}

	empty, _ := json.Marshal(savedAutoOff{V: 1, Printers: map[string]*savedPrinter{}})
	raw, err := os.ReadFile(filepath.Join(dir, autoOffFileName))
	var loaded savedAutoOff
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = empty
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: read auto-off state: %v\n", logName, err)
		raw = nil
	default:
		if err = json.Unmarshal(raw, &loaded); err != nil || loaded.V > 1 {
			fmt.Fprintf(os.Stderr, "%s: %s could not be used and is ignored\n", logName, autoOffFileName)
			loaded, raw = savedAutoOff{}, nil
		}
	}

	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	// A file that could not be used is written anew on the first change.
	m.autoOffSaved = raw
	for id, saved := range loaded.Printers {
		cfg, ok := m.configs[id]
		if !ok || saved == nil {
			continue
		}
		if tr := saved.Tracker; tr != nil && cfg.AutoOffAfterPrint {
			restored := &autoOffTracker{sawActive: tr.SawActive, fired: tr.Fired, jobID: tr.JobID, kept: tr.Kept, restored: true}
			// A wait runs on only when the extension was away for a moment,
			// as the idle time does. After a longer stop the printer may
			// have been turned on again, and the wait starts anew.
			fresh := false
			if u := m.usage[id]; u != nil && !u.LastSampleAt.IsZero() {
				gap := now.Sub(u.LastSampleAt)
				fresh = gap >= 0 && gap <= idleGapGrace
			}
			if fresh && tr.FinishedAt != nil && tr.FinishedAt.UTC().Year() >= minSaneYear {
				at := *tr.FinishedAt
				restored.finishedAt = &at
			}
			m.trackers[id] = restored
		}
		if skip := saved.Skip; skip != nil && !cfg.AutoOffAfterPrint && cfg.AutoOffDefault {
			m.skips[id] = &skippedPrint{sawPrint: skip.SawPrint, ended: skip.Ended, jobID: skip.JobID}
		}
		// The idle time runs on only when the extension was away for a
		// moment. When it last looked is the last sample of that printer.
		if idle := saved.Idle; idle != nil && idle.Since.UTC().Year() >= minSaneYear {
			clock := &idleClock{since: idle.Since}
			if u := m.usage[id]; u != nil {
				clock.seen = u.LastSampleAt
			}
			m.idle[id] = clock
		}
	}
}

// --- A printer that is on and idle ---

const (
	// idleAwayGrace is how long a printer may be away before its idle time
	// starts again. Nobody knows what it did while it was away.
	idleAwayGrace = 2 * time.Minute
	// idleGapGrace is how long the extension may not have looked. After a
	// longer stop, or a jump of the clock, the idle time starts again.
	idleGapGrace = 10 * time.Minute
)

// idleClock follows one printer an idle time counts for.
type idleClock struct {
	// since is from when the printer is on, connected and free to be turned
	// off. It is zero while it is not.
	since time.Time
	// seen is the last tick, and away the first tick the printer was away.
	seen, away time.Time
	// fault is why the plug could not be turned off when the time was over.
	fault string
}

// idleLimit is the idle time that counts for a printer: its own, or the one
// for all printers. 0 never turns it off.
func idleLimit(cfg DeviceConfig, s Settings) time.Duration {
	minutes := s.IdleOffMin
	if cfg.IdleOffMin != nil {
		minutes = *cfg.IdleOffMin
	}
	return time.Duration(clampIdleOffMin(minutes)) * time.Minute
}

// idleTick follows a printer for one tick and reports whether its plug is to
// be turned off now. The time runs only while the printer is connected, its
// plug is not known to be off and the interlock would let it be turned off:
// printing, a paused print, a busy printer and moving axes all start it again.
func (m *module) idleTick(deviceID string, cfg DeviceConfig, safety printerSafety, limit time.Duration, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 {
		delete(m.idle, deviceID)
		return false
	}
	clock := m.idle[deviceID]
	if clock == nil {
		clock = &idleClock{}
		m.idle[deviceID] = clock
	}

	// Time nobody watched does not count as idle time.
	if !clock.since.IsZero() && (now.Sub(clock.seen) > idleGapGrace || now.Before(clock.since)) {
		clock.since = now
	}
	clock.seen = now

	if !safety.Connected {
		if clock.away.IsZero() {
			clock.away = now
		}
		if now.Sub(clock.away) > idleAwayGrace {
			clock.since = time.Time{}
		}
		return false
	}
	clock.away = time.Time{}

	plug := m.plugs[cfg.plugKey()]
	state := "UNKNOWN"
	if plug != nil {
		state = plug.relayState(cfg.relayIndex())
	}
	if state == "OFF" || !safety.CanTurnOff {
		clock.since = time.Time{}
		clock.fault = ""
		return false
	}
	// While the turn-off after a print waits, that wait counts alone. The
	// idle time starts when it is over or was called off.
	if tr := m.trackers[deviceID]; cfg.AutoOffAfterPrint && tr != nil && tr.finishedAt != nil {
		clock.since = time.Time{}
		clock.fault = ""
		return false
	}
	if clock.since.IsZero() {
		clock.since = now
	}

	return state == "ON" && !m.plugStaleLocked(cfg, plug) && now.Sub(clock.since) >= limit
}

// keepOn starts a printer's idle time again. It reports whether one ran.
func (m *module) keepOn(deviceID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	clock := m.idle[deviceID]
	if clock == nil || clock.since.IsZero() {
		return false
	}
	clock.since = now
	clock.fault = ""
	return true
}

// idleStillDue reports whether a printer's idle time is still over. It is
// asked just before the plug is turned off.
func (m *module) idleStillDue(deviceID string, limit time.Duration, now time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	clock := m.idle[deviceID]
	return clock != nil && !clock.since.IsZero() && now.Sub(clock.since) >= limit
}

// noteIdleFault keeps why the plug could not be turned off after the idle
// time. It reports whether this is the first failure of this turn-off.
func (m *module) noteIdleFault(deviceID string, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	clock := m.idle[deviceID]
	if clock == nil {
		return false
	}
	first := clock.fault == ""
	clock.fault = err.Error()
	return first
}

// idleState is what a page shows of a printer's idle time: the time that
// counts in minutes, how many seconds are left while it runs, and why the
// turn-off failed. The caller holds m.mu.
func (m *module) idleStateLocked(deviceID string, cfg DeviceConfig, s Settings, now time.Time) (limitMin int, remaining *int, fault string) {
	limit := idleLimit(cfg, s)
	limitMin = int(limit / time.Minute)
	clock := m.idle[deviceID]
	if limit <= 0 || clock == nil {
		return limitMin, nil, ""
	}
	fault = clock.fault
	plug := m.plugs[cfg.plugKey()]
	if clock.since.IsZero() || plug == nil || plug.relayState(cfg.relayIndex()) != "ON" {
		return limitMin, nil, fault
	}
	left := max(0, int(math.Ceil((limit - now.Sub(clock.since)).Seconds())))
	return limitMin, &left, fault
}

// idleStep turns a printer off whose idle time is over. recheck reads the
// printer once more just before, like the turn-off after a print does.
func (m *module) idleStep(ctx context.Context, deviceID string, cfg DeviceConfig, safety printerSafety, recheck func() (printerSafety, error)) {
	limit := idleLimit(cfg, m.settings())
	if !m.idleTick(deviceID, cfg, safety, limit, m.clock()) {
		return
	}
	again, err := recheck()
	if err != nil || !again.Connected || !again.CanTurnOff {
		return
	}
	// "Keep on" may have been pressed while the printer was read again.
	if !m.idleStillDue(deviceID, limit, m.clock()) {
		return
	}

	if err := m.switchDevicePower(ctx, cfg, false); err != nil {
		first := m.noteIdleFault(deviceID, err)
		fmt.Fprintf(os.Stderr, "%s: idle-off failed for printer %q [%s]: %v\n", logName, safety.DeviceName, deviceID, err)
		if first {
			m.emitEvent(ctx, endpointAutoOffFailed, deviceID, autoOffEvent{
				DeviceID: deviceID, DeviceName: safety.DeviceName, Reason: reasonIdle, Error: m.scrubText(err.Error()),
			})
		}
		return
	}

	fmt.Fprintf(os.Stderr, "%s: idle-off triggered for printer %q [%s] (plug %s relay %d)\n", logName, safety.DeviceName, deviceID, cfg.plugKey(), cfg.relayIndex())
	m.mu.Lock()
	delete(m.idle, deviceID)
	m.mu.Unlock()
	m.emitEvent(ctx, endpointAutoOffDone, deviceID, autoOffEvent{
		DeviceID: deviceID, DeviceName: safety.DeviceName, Reason: reasonIdle,
	})
}

// --- Background Auto-Off & Usage Polling Loop ---

func (m *module) autoOffLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(pollIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.stepAutoOff(ctx)
		}
	}
}

// outcomeGrace is how long a print with no recorded outcome is given before
// it counts as finished.
const outcomeGrace = 15 * time.Second

// printEnd is how a print ended, as far as it is known.
type printEnd int

const (
	// endUnknown is a print the printer and the server say nothing about yet.
	endUnknown printEnd = iota
	endFinished
	// endNotFinished is a print that was cancelled or that failed.
	endNotFinished
)

// printEnd says how the print jobID ended, and for one that did not finish
// whether it was cancelled or failed. The printer's own state is read first.
// The server's record of the last print is read only when it is that print,
// or when the printer never named its print.
func (s printerSafety) printEnd(jobID string) (printEnd, string) {
	switch s.PrinterState {
	case "FINISHED":
		return endFinished, ""
	case "STOPPED":
		return endNotFinished, "cancelled"
	case "ERROR":
		return endNotFinished, "failed"
	}

	if jobID != "" && s.LastJobID != jobID {
		return endUnknown, ""
	}

	switch strings.ToLower(s.LastJobOutcome) {
	case "finished":
		return endFinished, ""
	case "cancelled", "failed":
		return endNotFinished, strings.ToLower(s.LastJobOutcome)
	}

	return endUnknown, ""
}

// printing reports whether the printer runs a print. Heating or moving by hand
// keeps a printer busy and is not a print.
func (s printerSafety) printing() bool {
	return s.ActiveJob || s.PrinterState == "PRINTING" || s.PrinterState == "PAUSED"
}

// autoOffDue reports whether a printer armed to turn off after its print has
// finished that print and waited out cooldown. It turns off only after a print
// it saw, or one the printer still reports as finished.
func (m *module) autoOffDue(deviceID string, safety printerSafety, cooldown time.Duration, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	tr, ok := m.trackers[deviceID]
	if !ok {
		tr = &autoOffTracker{}
		m.trackers[deviceID] = tr
	}

	// A printer that is away says nothing about its print, so a lost
	// connection never cuts the power mid-print. A print already seen to be
	// over stays over: the wait that began then runs on. After a restart
	// nothing is known of the printer until it was seen once: it may have
	// started its next print while nobody watched.
	// The server's record of the print is still read: a print it records as
	// cancelled or failed keeps the power on, and one with no outcome yet
	// gets the same outcomeGrace as a printer that is there.
	if !safety.Connected {
		if tr.restored || tr.finishedAt == nil {
			return false
		}
		how, reason := safety.printEnd(tr.jobID)
		if how == endNotFinished {
			*tr = autoOffTracker{fired: true, kept: reason}
			return false
		}
		return now.Sub(*tr.finishedAt) >= autoOffWait(how, cooldown)
	}

	// The first look after a restart. A print the printer and the server no
	// longer name was left behind while nobody watched: it is dropped, and
	// the printer waits for its next print.
	if tr.restored {
		tr.restored = false
		if tr.jobID != "" && safety.JobID != tr.jobID && safety.LastJobID != tr.jobID {
			*tr = autoOffTracker{fired: true}
		}
	}

	if !safety.CanTurnOff {
		if safety.printing() {
			tr.sawActive = true
			tr.kept = ""
			tr.offFault = ""
			if safety.JobID != "" {
				tr.jobID = safety.JobID
			}
		}
		tr.finishedAt = nil
		return false
	}

	if !tr.sawActive && (tr.fired || safety.PrinterState != "FINISHED") {
		return false
	}

	// Only a print that finished turns the printer off. After a cancelled or
	// a failed print it stays on, and waits for the next print.
	how, reason := safety.printEnd(tr.jobID)
	if how == endNotFinished {
		*tr = autoOffTracker{fired: true, kept: reason}
		return false
	}

	if tr.finishedAt == nil {
		at := now
		tr.finishedAt = &at
	}

	return now.Sub(*tr.finishedAt) >= autoOffWait(how, cooldown)
}

// autoOffWait is how long after a print ended the plug is turned off. The
// server records how a print ended a moment after the printer stops. Until it
// has, the wait is at least outcomeGrace, so a short wait does not turn off a
// printer whose print turns out to be cancelled.
func autoOffWait(how printEnd, cooldown time.Duration) time.Duration {
	if how == endUnknown && cooldown < outcomeGrace {
		return outcomeGrace
	}
	return cooldown
}

// noteAutoOffFault keeps why the plug could not be turned off after a print,
// for the printer's page to show while the turn-off is tried again.
// It reports whether this is the first failure of this turn-off.
func (m *module) noteAutoOffFault(deviceID string, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	tr, ok := m.trackers[deviceID]
	if !ok {
		return false
	}
	first := tr.offFault == ""
	tr.offFault = err.Error()
	return first
}

// plugIsOff reports whether the printer's plug is known to be off. A plug
// nothing was heard from is not known to be off.
func (m *module) plugIsOff(cfg DeviceConfig) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	plug, ok := m.plugs[cfg.plugKey()]

	return ok && plug.relayState(cfg.relayIndex()) == "OFF"
}

// forgetPrint drops what was seen of a printer's print. The printer then
// turns off only after a print it is seen running from here on.
func (m *module) forgetPrint(deviceID string) {
	m.mu.Lock()
	m.trackers[deviceID] = &autoOffTracker{fired: true}
	m.mu.Unlock()
}

// afterAutoOff is the config once the plug was turned off after a print. The
// choice for one print is used up, so the printer goes back to its default. A
// printer that turns off by default stays armed, and waits to see its next
// print before it acts again.
func (m *module) afterAutoOff(deviceID string, cfg DeviceConfig) DeviceConfig {
	cfg.AutoOffAfterPrint = cfg.AutoOffDefault

	m.mu.Lock()
	if cfg.AutoOffDefault {
		m.trackers[deviceID] = &autoOffTracker{fired: true}
	} else {
		delete(m.trackers, deviceID)
	}
	m.mu.Unlock()

	return cfg
}

// skippedPrintIsOver follows a printer that turns off by default while a
// person has switched that off for one print. It answers true once, when the
// print after the skipped one starts.
func (m *module) skippedPrintIsOver(deviceID string, safety printerSafety) bool {
	if !safety.Connected {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	skip, ok := m.skips[deviceID]
	if !ok {
		skip = &skippedPrint{}
		m.skips[deviceID] = skip
	}

	switch {
	case safety.printing() && skip.ended:
		delete(m.skips, deviceID)
		return true
	case safety.printing():
		skip.sawPrint = true
		if safety.JobID != "" {
			skip.jobID = safety.JobID
		}
	case skip.sawPrint:
		// A skipped print that did not finish is not used up. The choice
		// holds for the print that is started in its place.
		if how, _ := safety.printEnd(skip.jobID); how == endNotFinished {
			*skip = skippedPrint{}
		} else {
			skip.ended = true
		}
	}

	return false
}

// stepPrinter is what one tick does for one printer, once its state is read:
// the usage record, the word that its plug went away during a print, the
// turn-off after a print, and the turn-off of a printer left on and idle.
// recheck reads the printer once more, just before a plug is turned off.
func (m *module) stepPrinter(ctx context.Context, deviceID string, cfg DeviceConfig, safety printerSafety, recheck func() (printerSafety, error)) (dirty, active bool) {
	dirty, active = m.updateDeviceUsage(deviceID, cfg, safety)

	if cause, due := m.plugLostDue(deviceID, cfg, safety, m.clock()); due {
		m.emitEvent(ctx, endpointPlugLost, deviceID, plugLostEvent{
			DeviceID: deviceID, DeviceName: safety.DeviceName, JobID: safety.JobID,
			Driver: cfg.effectiveDriver(), Cause: cause,
		})
	}

	if !m.afterPrintStep(ctx, deviceID, cfg, safety, recheck) {
		m.idleStep(ctx, deviceID, cfg, safety, recheck)
	}
	return dirty, active
}

// updateConfig applies change to the config saved for deviceID now, not to
// the copy a tick began with, and saves it. A person may have saved the
// config while the tick waited for a plug. Nothing is saved when the printer
// is gone or its plug or relay is no longer the one in was.
func (m *module) updateConfig(deviceID string, was DeviceConfig, change func(*DeviceConfig)) {
	m.configsSaveMu.Lock()
	defer m.configsSaveMu.Unlock()
	m.mu.Lock()
	cur, ok := m.configs[deviceID]
	if !ok || cur.plugKey() != was.plugKey() || cur.relayIndex() != was.relayIndex() {
		m.mu.Unlock()
		return
	}
	change(&cur)
	if err := m.storeConfigLocked(deviceID, cur); err != nil {
		fmt.Fprintf(os.Stderr, "%s: save config: %v\n", logName, err)
	}
}

// afterPrintStep turns a printer off that is armed, has finished its print
// and has waited. It reports whether it turned the plug off.
func (m *module) afterPrintStep(ctx context.Context, deviceID string, cfg DeviceConfig, safety printerSafety, recheck func() (printerSafety, error)) bool {
	if !cfg.AutoOffAfterPrint {
		// Switched off for one print on a printer that turns off by
		// default: the default comes back when the print after it starts.
		if cfg.AutoOffDefault && m.skippedPrintIsOver(deviceID, safety) {
			m.updateConfig(deviceID, cfg, func(cur *DeviceConfig) {
				if cur.AutoOffDefault {
					cur.AutoOffAfterPrint = true
				}
			})
		}
		return false
	}

	cooldownSec := m.settings().AutoOffCooldownSec
	if cfg.CooldownSec != nil {
		cooldownSec = *cfg.CooldownSec
	}

	// A plug that is already off has nothing left to turn off. Whatever
	// print was followed is over, and the next one starts from nothing.
	if m.plugIsOff(cfg) {
		m.forgetPrint(deviceID)
		return false
	}

	if !m.autoOffDue(deviceID, safety, time.Duration(cooldownSec)*time.Second, m.clock()) {
		return false
	}

	again, err := recheck()
	if err != nil || !again.CanTurnOff {
		return false
	}

	m.mu.RLock()
	jobID := ""
	if tr := m.trackers[deviceID]; tr != nil {
		jobID = tr.jobID
	}
	m.mu.RUnlock()

	if err := m.switchDevicePower(ctx, cfg, false); err != nil {
		first := m.noteAutoOffFault(deviceID, err)
		fmt.Fprintf(os.Stderr, "%s: auto-off failed for printer %q [%s]: %v\n", logName, safety.DeviceName, deviceID, err)
		// One event for a turn-off that fails, not one for every try.
		if first {
			m.emitEvent(ctx, endpointAutoOffFailed, deviceID, autoOffEvent{
				DeviceID: deviceID, DeviceName: safety.DeviceName, JobID: jobID,
				Reason: reasonPrintFinished, Error: m.scrubText(err.Error()),
			})
		}
		return false
	}

	fmt.Fprintf(os.Stderr, "%s: auto-off triggered for printer %q [%s] (plug %s relay %d)\n", logName, safety.DeviceName, deviceID, cfg.plugKey(), cfg.relayIndex())
	m.afterAutoOff(deviceID, cfg)
	m.updateConfig(deviceID, cfg, func(cur *DeviceConfig) {
		cur.AutoOffAfterPrint = cur.AutoOffDefault
	})
	m.emitEvent(ctx, endpointAutoOffDone, deviceID, autoOffEvent{
		DeviceID: deviceID, DeviceName: safety.DeviceName, JobID: jobID, Reason: reasonPrintFinished,
	})
	return true
}

func (m *module) stepAutoOff(ctx context.Context) {
	// What this step changed of the prints it follows is on disk before the
	// next one, so a restart goes on from there.
	defer m.saveAutoOffIfChanged()

	m.pollAssignedPlugsEnergy()

	m.mu.RLock()
	assigned := make(map[string]DeviceConfig)
	for id, cfg := range m.configs {
		if cfg.configured() {
			assigned[id] = cfg
		}
	}
	m.mu.RUnlock()

	if len(assigned) == 0 {
		m.flushEnergyIfDue()
		return
	}

	var anyDirty bool
	var hasActiveUsage bool

	for deviceID, cfg := range assigned {
		safety, err := m.checkSafety(ctx, "", deviceID)
		if err != nil {
			continue
		}

		dirty, active := m.stepPrinter(ctx, deviceID, cfg, safety, func() (printerSafety, error) {
			return m.checkSafety(ctx, "", deviceID)
		})
		anyDirty = anyDirty || dirty
		hasActiveUsage = hasActiveUsage || active
	}

	// The energy record is written before usage.json. After a crash between
	// the two, usage.json still names the print as running; the restart ends
	// it again and the print store keeps the copy it already has.
	m.flushEnergyIfDue()
	if anyDirty || (hasActiveUsage && m.shouldPeriodicSaveUsage(60*time.Second)) {
		if err := m.saveAllUsage(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: save usage: %v\n", logName, err)
		}
	}
}

// --- HTTP Routes ---

type settingsResponse struct {
	Settings           Settings `json:"settings"`
	MQTTConfigured     bool     `json:"mqtt_configured"`
	MQTTConnected      bool     `json:"mqtt_connected"`
	MQTTBroker         string   `json:"mqtt_broker,omitempty"`
	MQTTError          string   `json:"mqtt_error,omitempty"`
	MQTTWarning        string   `json:"mqtt_warning,omitempty"`
	MQTTBuiltinRunning bool     `json:"mqtt_builtin_running"`
	MQTTBuiltinPort    int      `json:"mqtt_builtin_port,omitempty"`
	MQTTBuiltinClients int      `json:"mqtt_builtin_clients"`
	// MQTTBuiltinGranted says whether the operator approved the built-in
	// broker's port. MQTTBuiltinPort is that port, running or not.
	MQTTBuiltinGranted bool `json:"mqtt_builtin_granted"`
	HAConfigured       bool `json:"ha_configured"`
	// A pinned value was written in the host's own settings for this
	// extension, and is read-only here.
	MQTTHostPinned bool `json:"mqtt_host_pinned"`
	HAURLPinned    bool `json:"ha_url_pinned"`
	// SuggestedHTTPHost is the http_host option, offered as the address when a
	// plug is set up by IP address.
	SuggestedHTTPHost string `json:"suggested_http_host,omitempty"`
}

func (m *module) buildSettingsResponse() settingsResponse {
	s := m.settings()
	m.mu.RLock()
	up := m.mqttUp
	broker := m.mqttBroker
	errMsg := m.mqttError
	warnMsg := m.mqttWarning
	brk := m.embeddedBroker
	grantedPort := m.builtinPort
	hostPinned := m.pinnedMQTTHost != ""
	haPinned := m.pinnedHAURL != ""
	suggestedHTTP := m.suggestedHTTPHost
	m.mu.RUnlock()

	builtinRunning := brk != nil
	builtinPort := grantedPort
	builtinClients := 0
	if brk != nil {
		builtinPort = brk.Port()
		builtinClients = brk.ClientCount()
	}

	return settingsResponse{
		Settings:           s.redacted(),
		MQTTConfigured:     s.mqttConfigured(),
		MQTTConnected:      up,
		MQTTBroker:         broker,
		MQTTError:          errMsg,
		MQTTWarning:        warnMsg,
		MQTTBuiltinRunning: builtinRunning,
		MQTTBuiltinPort:    builtinPort,
		MQTTBuiltinClients: builtinClients,
		MQTTBuiltinGranted: grantedPort > 0,
		HAConfigured:       s.haConfigured(),
		MQTTHostPinned:     hostPinned,
		HAURLPinned:        haPinned,
		SuggestedHTTPHost:  suggestedHTTP,
	}
}

// powerHistoryKeep is how far back the power samples of a printer reach: the
// longest span the dashboard card draws, the same as the core's temperature
// card. statusHistoryWindow is what the printer's own page draws.
const (
	powerHistoryKeep    = 60 * time.Minute
	statusHistoryWindow = 30 * time.Minute
)

// trimHistory drops the samples older than keep before now, and the oldest
// beyond maxPowerSamples.
func trimHistory(history []PowerSample, now time.Time, keep time.Duration) []PowerSample {
	since := now.Add(-keep).Unix()
	first := 0
	for first < len(history) && history[first].Timestamp < since {
		first++
	}
	history = history[first:]
	if len(history) > maxPowerSamples {
		history = history[len(history)-maxPowerSamples:]
	}
	return history
}

// overviewEntry is one printer's plug in the fleet overview: what a list of
// every printer shows. The graph samples come only when asked for, and the
// print history never.
type overviewEntry struct {
	DeviceID          string            `json:"device_id"`
	Config            DeviceConfig      `json:"config"`
	Plug              *PlugTelemetry    `json:"plug,omitempty"`
	SessionUsage      *PowerSession     `json:"session_usage,omitempty"`
	CurrentPrintUsage *PrintPowerRecord `json:"current_print_usage,omitempty"`
	// PowerHistory is the samples of the last minutes ?history= names, at
	// most an hour. It is an empty list, never left out, when asked for.
	PowerHistory *[]PowerSample `json:"power_history,omitempty"`
	// CanOperate says whether the caller may switch this printer's plug. It
	// only decides what the page offers: the routes ask again.
	CanOperate bool `json:"can_operate"`
	// IdleOffRemainingSec is how long until the plug is turned off because
	// the printer is idle. It is left out while no idle time runs.
	IdleOffRemainingSec *int `json:"idle_off_remaining_sec,omitempty"`
}

type overviewResponse struct {
	Devices            []overviewEntry `json:"devices"`
	MQTTConfigured     bool            `json:"mqtt_configured"`
	MQTTConnected      bool            `json:"mqtt_connected"`
	MQTTError          string          `json:"mqtt_error,omitempty"`
	HAConfigured       bool            `json:"ha_configured"`
	DefaultCooldownSec int             `json:"default_cooldown_sec"`
	ConfirmTurnOff     bool            `json:"confirm_turn_off"`
	PollIntervalSec    int             `json:"poll_interval_sec"`
	// CanConfigure says whether the caller may change plugs and
	// connections. It only decides what the page offers.
	CanConfigure bool `json:"can_configure"`
}

// historyAsked reads ?history=, the minutes of samples a graph draws, clamped
// to powerHistoryKeep. Anything else, or nothing, asks for none.
func historyAsked(raw string) time.Duration {
	minutes, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || minutes <= 0 {
		return 0
	}
	asked := time.Duration(minutes) * time.Minute
	return min(asked, powerHistoryKeep)
}

// overviewOptions is what a caller of buildOverview asks for beyond the plugs.
type overviewOptions struct {
	// visible answers whether the caller may see a printer.
	visible func(deviceID string) bool
	// history adds each printer's samples of the last history, none when 0.
	history time.Duration
	// canOperate answers whether the caller may switch a printer's plug;
	// nil answers no.
	canOperate func(deviceID string) bool
}

// plugSnapshotLocked is the plug a printer's config points at, with the state
// of the relay that config names. The caller holds m.mu.
func (m *module) plugSnapshotLocked(cfg DeviceConfig) *PlugTelemetry {
	key := cfg.plugKey()
	if key == "" {
		return nil
	}
	if p, ok := m.plugs[key]; ok {
		cp := p.clone()
		relayIdx := cfg.relayIndex()
		cp.Power = cp.relayState(relayIdx)
		cp.Protection = cp.protectionOf(relayIdx)
		cp.AfterPowerCut = cp.afterPowerCutOf(relayIdx)
		if relayIdx > 1 && len(cp.RelayNames) >= relayIdx && cp.RelayNames[relayIdx-1] != "" {
			cp.FriendlyName = cp.RelayNames[relayIdx-1]
		}
		return &cp
	}
	d := cfg.effectiveDriver()
	friendly := cfg.TasmotaTopic
	switch d {
	case DriverTasmotaHTTP, DriverShellyHTTP:
		friendly = cfg.HTTPHost
	case DriverHomeAssistant:
		friendly = cfg.HAEntityID
	case DriverGenericMQTT:
		friendly = cfg.MQTTCommandTopic
	}
	return &PlugTelemetry{Topic: key, Driver: d, FriendlyName: friendly, Power: "UNKNOWN"}
}

// buildOverview answers every printer that has a plug or an auto-off setting,
// kept to the printers visible answers true for. It reads memory only, so one
// request serves a whole list of printers.
func (m *module) buildOverview(opts overviewOptions) overviewResponse {
	s := m.settings()
	now := time.Now()
	m.mu.RLock()
	entries := make([]overviewEntry, 0, len(m.configs))
	for deviceID, cfg := range m.configs {
		if !opts.visible(deviceID) {
			continue
		}
		entry := overviewEntry{DeviceID: deviceID, Config: cfg.redacted()}
		if cfg.configured() {
			entry.Plug = m.plugSnapshotLocked(cfg)
			_, entry.IdleOffRemainingSec, _ = m.idleStateLocked(deviceID, cfg, s, m.clock())
			if held := m.usage[deviceID]; held != nil {
				// Only the parts the overview answers are copied, so a poll
				// does not copy an hour of samples it does not send.
				usage := cloneDeviceUsage(&DeviceUsage{Session: held.Session, CurrentPrint: held.CurrentPrint})
				entry.SessionUsage = usage.Session
				entry.CurrentPrintUsage = usage.CurrentPrint
				if opts.history > 0 {
					samples := append([]PowerSample{}, trimHistory(held.History, now, opts.history)...)
					entry.PowerHistory = &samples
				}
			}
			if opts.history > 0 && entry.PowerHistory == nil {
				entry.PowerHistory = &[]PowerSample{}
			}
		}
		entries = append(entries, entry)
	}
	up := m.mqttUp
	errMsg := m.mqttError
	m.mu.RUnlock()

	// Asked outside the lock: each answer may be a question to the host.
	if opts.canOperate != nil {
		for i := range entries {
			entries[i].CanOperate = entries[i].Config.configured() && opts.canOperate(entries[i].DeviceID)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].DeviceID < entries[j].DeviceID })

	return overviewResponse{
		Devices:            entries,
		MQTTConfigured:     s.mqttConfigured(),
		MQTTConnected:      up,
		MQTTError:          errMsg,
		HAConfigured:       s.haConfigured(),
		DefaultCooldownSec: s.AutoOffCooldownSec,
		ConfirmTurnOff:     s.confirmTurnOffEnabled(),
		PollIntervalSec:    pollIntervalSec,
	}
}

// visibleDevices reads the printers principal may see, so a list this module
// answers never names a printer the caller could not open.
func (m *module) visibleDevices(ctx context.Context, principal string) (map[string]bool, error) {
	names, err := m.visiblePrinters(ctx, principal)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	for id := range names {
		seen[id] = true
	}
	return seen, nil
}

// visiblePrinters reads the printers principal may see with their names, the
// id where a printer has none. An empty principal is the module itself, which
// is every printer Connect lists.
func (m *module) visiblePrinters(ctx context.Context, principal string) (map[string]string, error) {
	seen := make(map[string]string)
	for offset := 0; ; {
		page, err := m.devices.List(ctx, deviceapi.ListParams{
			Principal: principal,
			Limit:     deviceapi.MaxPageLimit,
			Offset:    offset,
		})
		if err != nil {
			return nil, err
		}
		for _, dev := range page.Items {
			seen[dev.ID] = dev.Name
			if dev.Name == "" {
				seen[dev.ID] = dev.ID
			}
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			return seen, nil
		}
	}
}

type statusResponse struct {
	DeviceID            string         `json:"device_id"`
	MQTTConfigured      bool           `json:"mqtt_configured"`
	MQTTConnected       bool           `json:"mqtt_connected"`
	MQTTBroker          string         `json:"mqtt_broker,omitempty"`
	MQTTError           string         `json:"mqtt_error,omitempty"`
	HAConfigured        bool           `json:"ha_configured"`
	Config              DeviceConfig   `json:"config"`
	Plug                *PlugTelemetry `json:"plug,omitempty"`
	CanTurnOff          bool           `json:"can_turn_off"`
	LockReason          string         `json:"lock_reason,omitempty"`
	PrinterConnected    bool           `json:"printer_connected"`
	PrinterState        string         `json:"printer_state,omitempty"`
	PrinterMoving       bool           `json:"printer_moving"`
	AutoOffPending      bool           `json:"auto_off_pending"`
	AutoOffRemainingSec int            `json:"auto_off_remaining_sec"`
	// AutoOffError is why the plug could not be turned off after a print.
	// AutoOffKept is why the printer stayed on: cancelled or failed.
	AutoOffError       string         `json:"auto_off_error,omitempty"`
	AutoOffKept        string         `json:"auto_off_kept,omitempty"`
	DefaultCooldownSec int            `json:"default_cooldown_sec"`
	ConfirmTurnOff     bool           `json:"confirm_turn_off"`
	PollIntervalSec    int            `json:"poll_interval_sec"`
	PowerHistory       []PowerSample  `json:"power_history,omitempty"`
	SessionUsage       *pricedSession `json:"session_usage,omitempty"`
	CurrentPrintUsage  *pricedPrint   `json:"current_print_usage,omitempty"`
	PrintHistory       []pricedPrint  `json:"print_history,omitempty"`
	// Currency and PriceSet go with the cost of the session and the prints.
	Currency string `json:"currency"`
	PriceSet bool   `json:"price_set"`
	// IdleOffMin is the idle time that counts for this printer, 0 for none.
	// IdleOffPending says the time runs, and IdleOffError why the plug could
	// not be turned off when it was over.
	IdleOffMin          int    `json:"idle_off_min"`
	IdleOffPending      bool   `json:"idle_off_pending"`
	IdleOffRemainingSec int    `json:"idle_off_remaining_sec"`
	IdleOffError        string `json:"idle_off_error,omitempty"`
}

func (m *module) buildStatus(ctx context.Context, principal, deviceID string) (statusResponse, error) {
	cfg, err := m.getConfig(ctx, deviceID)
	if err != nil {
		return statusResponse{}, err
	}
	safety, err := m.checkSafety(ctx, principal, deviceID)
	if err != nil {
		return statusResponse{}, err
	}

	if cfg.configured() {
		if dirty, _ := m.updateDeviceUsage(deviceID, cfg, safety); dirty {
			m.flushEnergyIfDue()
			_ = m.saveAllUsage()
		}
	}

	s := m.settings()
	cooldownSec := s.AutoOffCooldownSec
	if cfg.CooldownSec != nil {
		cooldownSec = *cfg.CooldownSec
	}

	m.mu.RLock()
	mqttUp := m.mqttUp
	mqttBroker := m.mqttBroker
	mqttErr := m.mqttError
	plugCopy := m.plugSnapshotLocked(cfg)
	var pending bool
	var remaining int
	var offFault, kept string
	if cfg.AutoOffAfterPrint {
		if tr, ok := m.trackers[deviceID]; ok {
			offFault, kept = tr.offFault, tr.kept
			if tr.finishedAt != nil && safety.CanTurnOff {
				pending = true
				rem := int((time.Duration(cooldownSec)*time.Second - time.Since(*tr.finishedAt)).Seconds())
				if rem > 0 {
					remaining = rem
				}
			} else if tr.sawActive && safety.Moving {
				pending = true
			}
		}
	}
	usageCopy := cloneDeviceUsage(m.usage[deviceID])
	idleMin, idleLeft, idleFault := m.idleStateLocked(deviceID, cfg, s, m.clock())
	m.mu.RUnlock()

	// The countdown shows only while the printer could be turned off.
	idlePending := idleLeft != nil && safety.Connected && safety.CanTurnOff
	idleRemaining := 0
	if idlePending {
		idleRemaining = *idleLeft
	}

	var historyCopy []PowerSample
	if usageCopy != nil {
		historyCopy = trimHistory(usageCopy.History, time.Now(), statusHistoryWindow)
	}
	priced := m.priceUsage(deviceID, usageCopy, m.clock())

	return statusResponse{
		DeviceID:            deviceID,
		MQTTConfigured:      s.mqttConfigured(),
		MQTTConnected:       mqttUp,
		MQTTBroker:          mqttBroker,
		MQTTError:           mqttErr,
		HAConfigured:        s.haConfigured(),
		Config:              cfg.redacted(),
		Plug:                plugCopy,
		CanTurnOff:          safety.CanTurnOff,
		LockReason:          safety.LockReason,
		PrinterConnected:    safety.Connected,
		PrinterState:        safety.PrinterState,
		PrinterMoving:       safety.Moving,
		AutoOffPending:      pending,
		AutoOffRemainingSec: remaining,
		AutoOffError:        offFault,
		AutoOffKept:         kept,
		DefaultCooldownSec:  s.AutoOffCooldownSec,
		ConfirmTurnOff:      s.confirmTurnOffEnabled(),
		PollIntervalSec:     pollIntervalSec,
		PowerHistory:        historyCopy,
		SessionUsage:        priced.session,
		CurrentPrintUsage:   priced.current,
		PrintHistory:        priced.history,
		Currency:            priced.currency,
		PriceSet:            priced.priceSet,
		IdleOffMin:          idleMin,
		IdleOffPending:      idlePending,
		IdleOffRemainingSec: idleRemaining,
		IdleOffError:        idleFault,
	}, nil
}

func (m *module) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, m.buildSettingsResponse())
	})

	mux.HandleFunc("PUT /settings", m.guard(mayConfigure, func(w http.ResponseWriter, r *http.Request) {
		var body Settings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		current := m.settings()
		next := m.applyHostPins(body)
		// A saved secret is kept only for the address it was saved for.
		if err := errors.Join(keepPassword(body, current, &next), keepHAToken(body, current, &next)); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		// A save that turns the built-in broker on or changes its user name
		// or password needs both. An install from before 0.5.0 that runs the
		// built-in broker without them can still save its other settings;
		// the broker stays off and says why until both are set.
		unchanged := current.MQTTBuiltin && current.MQTTUsername == next.MQTTUsername && current.MQTTPassword == next.MQTTPassword
		if next.MQTTBuiltin && !next.builtinCredentialsSet() && !unchanged {
			refuse(w, r, InvalidInput.New(errBuiltinNeedsCredentials))
			return
		}
		if err := m.writeSettingsToDisk(next); err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		m.held.Store(&next)
		m.reconnectMQTT(next)
		// Allow brief time for initial connect so the returned status reflects it if immediate.
		time.Sleep(300 * time.Millisecond)
		answer(w, http.StatusOK, m.buildSettingsResponse())
	}))

	mux.HandleFunc("POST /broker/test", m.guard(mayConfigure, func(w http.ResponseWriter, r *http.Request) {
		var override *Settings
		if r.Body != nil {
			raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
			if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
				var body Settings
				if err := json.Unmarshal(raw, &body); err != nil {
					refuse(w, r, InvalidInput.New(err.Error()))
					return
				}
				override = &body
			}
		}
		ok, broker, errMsg, warnMsg := m.testBroker(override)
		answer(w, http.StatusOK, map[string]any{
			"ok":      ok,
			"broker":  broker,
			"error":   errMsg,
			"warning": warnMsg,
		})
	}))

	mux.HandleFunc("POST /ha/test", m.guard(mayConfigure, func(w http.ResponseWriter, r *http.Request) {
		var override *Settings
		if r.Body != nil {
			raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
			if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
				var body Settings
				if err := json.Unmarshal(raw, &body); err != nil {
					refuse(w, r, InvalidInput.New(err.Error()))
					return
				}
				override = &body
			}
		}
		ok, haURL, entities, errMsg := m.testHomeAssistant(r.Context(), override)
		answer(w, http.StatusOK, map[string]any{
			"ok":       ok,
			"url":      haURL,
			"entities": entities,
			"error":    errMsg,
		})
	}))

	mux.HandleFunc("POST /discover", m.guard(mayConfigure, func(w http.ResponseWriter, _ *http.Request) {
		m.discoverPlugs()
		m.syncDriverSubscriptions()
		time.Sleep(400 * time.Millisecond)
		m.mu.RLock()
		discovered := m.discoveredListLocked()
		m.mu.RUnlock()
		answer(w, http.StatusOK, map[string]any{
			"plugs": discovered,
		})
	}))

	mux.HandleFunc("GET /assignments", m.guard(mayConfigure, func(w http.ResponseWriter, r *http.Request) {
		s := m.settings()
		m.mu.RLock()
		configsCopy := make(map[string]DeviceConfig, len(m.configs))
		for k, v := range m.configs {
			configsCopy[k] = v.redacted()
		}
		discovered := m.discoveredListLocked()
		up := m.mqttUp
		broker := m.mqttBroker
		errMsg := m.mqttError
		m.mu.RUnlock()
		var haEntities []HAEntity
		if s.haConfigured() {
			haEntities, _ = m.fetchHAEntities(r.Context(), s)
		}
		answer(w, http.StatusOK, map[string]any{
			"assignments":          configsCopy,
			"discovered_plugs":     discovered,
			"mqtt_configured":      s.mqttConfigured(),
			"mqtt_connected":       up,
			"mqtt_broker":          broker,
			"mqtt_error":           errMsg,
			"ha_configured":        s.haConfigured(),
			"ha_entities":          haEntities,
			"default_cooldown_sec": s.AutoOffCooldownSec,
		})
	}))

	mux.HandleFunc("GET /overview", func(w http.ResponseWriter, r *http.Request) {
		seen, err := m.visibleDevices(r.Context(), m.principalOf(r))
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		person := m.principalOf(r)
		resp := m.buildOverview(overviewOptions{
			visible:    func(deviceID string) bool { return seen[deviceID] },
			history:    historyAsked(r.URL.Query().Get("history")),
			canOperate: func(deviceID string) bool { return m.mayHint(r.Context(), person, mayOperate, deviceID) },
		})
		resp.CanConfigure = m.mayHint(r.Context(), person, mayConfigure, "")
		answer(w, http.StatusOK, resp)
	})

	m.energyRoutes(mux)

	mux.HandleFunc("GET /devices/{device_id}", func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("device_id")
		principal := m.principalOf(r)
		resp, err := m.buildStatus(r.Context(), principal, deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, resp)
	})

	mux.HandleFunc("PUT /devices/{device_id}/config", m.guard(mayConfigureDevice, func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("device_id")
		principal := m.principalOf(r)
		var body DeviceConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		mode := strings.ToLower(strings.TrimSpace(body.Mode))
		if mode != "" && mode != "mqtt" && mode != "http" && mode != "ha" {
			refuse(w, r, InvalidInput.New("mode must be mqtt, http, or ha"))
			return
		}
		if !isValidDriverInput(body.Driver) {
			refuse(w, r, InvalidInput.New("unsupported plug driver"))
			return
		}
		if body.usesHTTP() && strings.TrimSpace(body.HTTPHost) != "" && normalizeHTTPHost(body.HTTPHost) == "" {
			refuse(w, r, InvalidInput.New("invalid HTTP plug IP, hostname, or URL"))
			return
		}
		if body.effectiveDriver() == DriverHomeAssistant {
			ent := strings.TrimSpace(body.HAEntityID)
			pEnt := strings.TrimSpace(body.HAPowerEntityID)
			if (ent != "" && !validHAEntity(ent, false)) || (pEnt != "" && !validHAEntity(pEnt, true)) {
				refuse(w, r, InvalidInput.New(errUnsupportedHAEntity.Error()))
				return
			}
		}
		normalizedCfg := normalizeDeviceConfig(body)
		candidateKey := normalizedCfg.plugKey()
		relayIdx := normalizedCfg.relayIndex()
		if candidateKey != "" {
			m.mu.RLock()
			var conflictID string
			for otherID, otherCfg := range m.configs {
				if otherID != deviceID && otherCfg.configured() && otherCfg.plugKey() == candidateKey && otherCfg.relayIndex() == relayIdx {
					conflictID = otherID
					break
				}
			}
			m.mu.RUnlock()
			if conflictID != "" {
				refuse(w, r, TopicInUse.New("plug relay is already assigned to another printer").
					With(fault.StringParam("topic", candidateKey), fault.StringParam("device", conflictID)))
				return
			}
		}
		if err := m.saveConfig(r.Context(), deviceID, body); err != nil {
			if errors.Is(err, errPlugPasswordRetype) {
				refuse(w, r, InvalidInput.New(err.Error()))
				return
			}
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		if candidateKey != "" {
			savedCfg, _ := m.getConfig(r.Context(), deviceID)
			switch savedCfg.effectiveDriver() {
			case DriverTasmotaHTTP:
				_ = m.pollHTTPPlug(r.Context(), savedCfg)
			case DriverShellyHTTP:
				_ = m.pollShellyHTTPPlug(r.Context(), savedCfg)
			case DriverHomeAssistant:
				_ = m.pollHomeAssistantPlug(r.Context(), savedCfg)
			}
		}
		resp, err := m.buildStatus(r.Context(), principal, deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("POST /devices/{device_id}/power", m.guard(mayOperate, func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("device_id")
		principal := m.principalOf(r)
		var body struct {
			Action string `json:"action"` // "on", "off", "toggle"
			// IdleOnly refuses to turn off a printer nothing is known of.
			IdleOnly bool `json:"idle_only"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		action := strings.ToLower(strings.TrimSpace(body.Action))
		if action != "on" && action != "off" && action != "toggle" {
			refuse(w, r, InvalidInput.New("action must be on, off, or toggle"))
			return
		}

		cfg, err := m.getConfig(r.Context(), deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		if !cfg.configured() {
			refuse(w, r, NotConfigured.New("no smart plug is set up for this printer"))
			return
		}

		if action == "toggle" {
			m.mu.RLock()
			p := m.plugs[cfg.plugKey()]
			cur := ""
			if p != nil {
				cur = p.relayState(cfg.relayIndex())
			}
			m.mu.RUnlock()
			if cur == "ON" {
				action = "off"
			} else {
				action = "on"
			}
		}

		if action == "off" {
			safety, err := m.checkSafety(r.Context(), principal, deviceID)
			if err != nil {
				refuse(w, r, InternalError.New(err.Error()))
				return
			}
			if reason, state, refused := offRefusal(safety, body.IdleOnly); refused {
				refuse(w, r, PrinterBusy.New(reason).With(fault.StringParam("state", state)))
				return
			}
		}

		if err := m.switchDevicePower(r.Context(), cfg, action == "on"); err != nil {
			if errors.Is(err, errBrokerOffline) {
				refuse(w, r, BrokerOffline.New(err.Error()))
			} else {
				refuse(w, r, PlugRefused.New(err.Error()))
			}
			return
		}

		go func(c DeviceConfig) {
			time.Sleep(400 * time.Millisecond)
			m.refreshConfigState(context.Background(), c)
		}(cfg)

		resp, err := m.buildStatus(r.Context(), principal, deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("POST /devices/{device_id}/auto-off", m.guard(mayOperate, func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("device_id")
		principal := m.principalOf(r)
		var body struct {
			Enabled bool `json:"enabled"`
			// Default, when sent, sets what the printer does after every
			// print, and the print that runs now follows it.
			Default *bool `json:"default"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}

		cfg, err := m.getConfig(r.Context(), deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		if !cfg.configured() {
			refuse(w, r, NotConfigured.New("no smart plug is set up for this printer"))
			return
		}
		cfg.AutoOffAfterPrint = body.Enabled
		if body.Default != nil {
			cfg.AutoOffDefault = *body.Default
			cfg.AutoOffAfterPrint = *body.Default
		}
		if err := m.saveConfig(r.Context(), deviceID, cfg); err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}

		resp, err := m.buildStatus(r.Context(), principal, deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("POST /devices/{device_id}/idle-off", m.guard(mayOperate, func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("device_id")
		var body struct {
			Action string `json:"action"` // "keep"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		if strings.ToLower(strings.TrimSpace(body.Action)) != "keep" {
			refuse(w, r, InvalidInput.New("action must be keep"))
			return
		}
		cfg, err := m.getConfig(r.Context(), deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		if !cfg.configured() {
			refuse(w, r, NotConfigured.New("no smart plug is set up for this printer"))
			return
		}
		// The whole idle time starts again.
		m.keepOn(deviceID, m.clock())
		m.saveAutoOffIfChanged()

		resp, err := m.buildStatus(r.Context(), m.principalOf(r), deviceID)
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, r, RouteNotFound.New("unknown path").With(fault.StringParam("path", r.URL.Path)))
	})

	return mux
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }

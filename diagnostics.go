package main

import (
	"encoding/json"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/diagnosticsapi"
)

// The server's diagnostics bundle is what an administrator downloads and
// hands to somebody else. The extension adds one file to it: which driver
// each plug uses and whether it answers, its last error, and the state of the
// broker and of auto-off.
//
// The file is built from memory only. Nothing is asked of a printer, a plug
// or a file. Left out on purpose: host names, IP addresses, URLs, MQTT
// topics, the client id, user names, entity ids, the names of printers and
// plugs, the plug's model, passwords and tokens. Error texts go through the
// filter of secrets.go.
//
// The host filters the file once more, line by line, and blanks the value of
// every key whose name holds "password", "token" or "secret". So whether a
// password or a token is set is told under the name auth_set.

const (
	diagFileName = "power-control.json"
	// maxDiagPlugs is the most plugs the file lists. Fewer are listed where
	// that many do not fit the page the host takes.
	maxDiagPlugs = 250
)

type diagReport struct {
	Version       string      `json:"version"`
	GeneratedAt   string      `json:"generated_at"`
	Broker        diagBroker  `json:"broker"`
	HomeAssistant diagHA      `json:"home_assistant"`
	AutoOff       diagAutoOff `json:"auto_off"`
	// Events is "off" where the server does not take the extension's events.
	Events     string     `json:"events"`
	PlugsTotal int        `json:"plugs_total"`
	Plugs      []diagPlug `json:"plugs"`
}

type diagBroker struct {
	// Mode is builtin, external or none.
	Mode           string `json:"mode"`
	Configured     bool   `json:"configured"`
	Connected      bool   `json:"connected"`
	TLS            bool   `json:"tls"`
	HostPinned     bool   `json:"host_pinned"`
	HasUser        bool   `json:"has_user"`
	AuthSet        bool   `json:"auth_set"`
	AddressKind    string `json:"address_kind,omitempty"`
	BuiltinGranted bool   `json:"builtin_granted"`
	BuiltinRunning bool   `json:"builtin_running"`
	BuiltinClients int    `json:"builtin_clients"`
	Error          string `json:"error,omitempty"`
	Warning        string `json:"warning,omitempty"`
}

type diagHA struct {
	Configured  bool   `json:"configured"`
	URLPinned   bool   `json:"url_pinned"`
	AuthSet     bool   `json:"auth_set"`
	AddressKind string `json:"address_kind,omitempty"`
}

type diagAutoOff struct {
	DefaultCooldownSec int  `json:"default_cooldown_sec"`
	ConfirmTurnOff     bool `json:"confirm_turn_off"`
	IdleOffMin         int  `json:"idle_off_min"`
}

type diagPlug struct {
	DeviceID string `json:"device_id"`
	Driver   string `json:"driver"`
	// Transport is mqtt, http or home_assistant.
	Transport   string `json:"transport"`
	AddressKind string `json:"address_kind,omitempty"`
	Relay       int    `json:"relay"`
	AuthSet     bool   `json:"auth_set"`
	// Seen says the plug was heard from since the extension started. Online
	// is null for a plug that never said whether it is there.
	Seen        bool   `json:"seen"`
	Online      *bool  `json:"online"`
	Power       string `json:"power"`
	HasMeter    bool   `json:"has_meter"`
	LastSeenSec *int   `json:"last_seen_sec"`
	LastError   string `json:"last_error,omitempty"`
	WifiDBm     *int   `json:"wifi_dbm,omitempty"`
	Protection  string `json:"protection,omitempty"`

	AutoOffArmed   bool `json:"auto_off_armed"`
	AutoOffDefault bool `json:"auto_off_default"`
	CooldownSec    int  `json:"cooldown_sec"`
	// AutoOffState is off, armed, printing, waiting, failed or kept_on.
	AutoOffState   string `json:"auto_off_state"`
	AutoOffKept    string `json:"auto_off_kept,omitempty"`
	AutoOffError   string `json:"auto_off_error,omitempty"`
	IdleOffMin     int    `json:"idle_off_min"`
	IdleOffPending bool   `json:"idle_off_pending"`
	IdleOffError   string `json:"idle_off_error,omitempty"`
}

// autoOffStateOf says in one word where the turn-off after a print stands.
func autoOffStateOf(cfg DeviceConfig, tr *autoOffTracker) string {
	switch {
	case !cfg.AutoOffAfterPrint:
		return "off"
	case tr == nil:
		return "armed"
	case tr.offFault != "":
		return "failed"
	case tr.finishedAt != nil:
		return "waiting"
	case tr.sawActive:
		return "printing"
	case tr.kept != "":
		return "kept_on"
	}
	return "armed"
}

// addressKind says what kind of address host is, without naming it: ipv4,
// ipv6, mdns for a name ending in .local, hostname for any other name, and
// nothing for no address.
func addressKind(host string) string {
	h := strings.TrimSpace(host)
	if _, rest, found := strings.Cut(h, "://"); found {
		h = rest
	}
	h, _, _ = strings.Cut(h, "/")
	if at := strings.LastIndex(h, "@"); at >= 0 {
		h = h[at+1:]
	}
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	h = strings.Trim(h, "[]")
	if h == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if addr.Is4() || addr.Is4In6() {
			return "ipv4"
		}
		return "ipv6"
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(h, ".")), ".local") {
		return "mdns"
	}
	return "hostname"
}

// transportOf is how the extension reaches a plug of this config.
func transportOf(cfg DeviceConfig) string {
	switch {
	case cfg.usesHTTP():
		return "http"
	case cfg.usesHomeAssistant():
		return "home_assistant"
	}
	return "mqtt"
}

// buildDiagnostics is the content of the file. It copies what it needs under
// one read lock and filters the error texts after it.
func (m *module) buildDiagnostics(now time.Time) diagReport {
	s := m.settings()
	filter := m.scrubber()

	report := diagReport{
		Version:     string(release),
		GeneratedAt: now.UTC().Format(time.RFC3339),
		HomeAssistant: diagHA{
			Configured:  s.haConfigured(),
			AuthSet:     s.HAToken != "",
			AddressKind: addressKind(s.HAURL),
		},
		AutoOff: diagAutoOff{
			DefaultCooldownSec: s.AutoOffCooldownSec,
			ConfirmTurnOff:     s.confirmTurnOffEnabled(),
			IdleOffMin:         s.IdleOffMin,
		},
		Events: "on",
		Plugs:  []diagPlug{},
	}
	if m.eventsOff.Load() {
		report.Events = "off"
	}

	broker := diagBroker{
		Mode:        "none",
		Configured:  s.mqttConfigured(),
		TLS:         s.MQTTTLS,
		HasUser:     strings.TrimSpace(s.MQTTUsername) != "",
		AuthSet:     s.MQTTPassword != "",
	}
	switch {
	case s.MQTTBuiltin:
		broker.Mode = "builtin"
	case broker.Configured:
		broker.Mode = "external"
		broker.AddressKind = addressKind(s.MQTTHost)
	}

	m.mu.RLock()
	broker.Connected = m.mqttUp
	broker.HostPinned = m.pinnedMQTTHost != ""
	broker.BuiltinGranted = m.builtinPort > 0
	broker.Error, broker.Warning = m.mqttError, m.mqttWarning
	embedded := m.embeddedBroker
	report.HomeAssistant.URLPinned = m.pinnedHAURL != ""

	ids := make([]string, 0, len(m.configs))
	for id, cfg := range m.configs {
		if cfg.configured() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	report.PlugsTotal = len(ids)
	if len(ids) > maxDiagPlugs {
		ids = ids[:maxDiagPlugs]
	}
	for _, id := range ids {
		cfg := m.configs[id]
		row := diagPlug{
			DeviceID:       id,
			Driver:         cfg.effectiveDriver(),
			Transport:      transportOf(cfg),
			Relay:          cfg.relayIndex(),
			AuthSet:        cfg.HTTPPassword != "",
			Power:          "UNKNOWN",
			AutoOffArmed:   cfg.AutoOffAfterPrint,
			AutoOffDefault: cfg.AutoOffDefault,
			CooldownSec:    s.AutoOffCooldownSec,
			AutoOffState:   autoOffStateOf(cfg, m.trackers[id]),
		}
		if cfg.usesHTTP() {
			row.AddressKind = addressKind(cfg.HTTPHost)
		}
		if cfg.CooldownSec != nil {
			row.CooldownSec = *cfg.CooldownSec
		}
		if plug := m.plugs[cfg.plugKey()]; plug != nil {
			row.Seen = true
			if plug.Online != nil {
				online := *plug.Online
				row.Online = &online
			}
			row.Power = plug.relayState(cfg.relayIndex())
			row.HasMeter = plug.PowerWatts != nil
			if plug.UpdatedAt != nil {
				ago := max(0, int(now.Sub(*plug.UpdatedAt).Seconds()))
				row.LastSeenSec = &ago
			}
			row.LastError = plug.LastError
			if plug.WifiDBm != nil {
				dbm := *plug.WifiDBm
				row.WifiDBm = &dbm
			}
			row.Protection = plug.protectionOf(cfg.relayIndex())
		}
		if tr := m.trackers[id]; tr != nil && cfg.AutoOffAfterPrint {
			row.AutoOffKept, row.AutoOffError = tr.kept, tr.offFault
		}
		var left *int
		row.IdleOffMin, left, row.IdleOffError = m.idleStateLocked(id, cfg, s, now)
		row.IdleOffPending = left != nil
		report.Plugs = append(report.Plugs, row)
	}
	m.mu.RUnlock()

	if embedded != nil {
		broker.BuiltinRunning = true
		broker.BuiltinClients = embedded.ClientCount()
	}
	broker.Error = filter.text(broker.Error)
	broker.Warning = filter.text(broker.Warning)
	report.Broker = broker
	for i := range report.Plugs {
		row := &report.Plugs[i]
		row.LastError = filter.text(row.LastError)
		row.AutoOffError = filter.text(row.AutoOffError)
		row.IdleOffError = filter.text(row.IdleOffError)
	}
	return report
}

// collectDiagnostics answers the host's diagnostics.collect: one page with
// one file. It never fails: a file that could not be built says so in a file.
func (m *module) collectDiagnostics(cursor string) diagnosticsapi.Page {
	if cursor != "" {
		return diagnosticsapi.Page{Files: []diagnosticsapi.File{}}
	}
	report := m.buildDiagnostics(m.clock())
	text := ""
	for {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			break
		}
		text = string(raw) + "\n"
		// Long error texts on many plugs: fewer plugs are listed, and
		// plugs_total still says how many there are.
		if len(text) <= diagnosticsapi.MaxPageBytes || len(report.Plugs) == 0 {
			break
		}
		report.Plugs = report.Plugs[:len(report.Plugs)/2]
	}
	if text == "" || len(text) > diagnosticsapi.MaxPageBytes {
		raw, _ := json.MarshalIndent(map[string]string{"version": string(release), "error": "too large"}, "", "  ")
		text = string(raw) + "\n"
	}
	return diagnosticsapi.Page{Files: []diagnosticsapi.File{{Name: diagFileName, Text: text}}}
}

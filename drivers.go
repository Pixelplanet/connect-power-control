package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DriverTasmota       = "tasmota"
	DriverTasmotaHTTP   = "tasmota_http"
	DriverShellyGen1    = "shelly_gen1"
	DriverShellyGen2    = "shelly_gen2"
	DriverShellyHTTP    = "shelly_http"
	DriverZigbee2MQTT   = "zigbee2mqtt"
	DriverESPHome       = "esphome"
	DriverOpenBeken     = "openbeken"
	DriverGenericMQTT   = "generic_mqtt"
	DriverHomeAssistant = "homeassistant"
)

// HAEntity represents a Home Assistant switch or power sensor returned by /ha/test or /assignments.
type HAEntity struct {
	EntityID     string `json:"entity_id"`
	FriendlyName string `json:"friendly_name,omitempty"`
	State        string `json:"state,omitempty"`
	Domain       string `json:"domain,omitempty"`
	DeviceClass  string `json:"device_class,omitempty"`
	Unit         string `json:"unit,omitempty"`
}

func normalizeDriverName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "mqtt", DriverTasmota:
		return DriverTasmota
	case "http", DriverTasmotaHTTP:
		return DriverTasmotaHTTP
	case DriverShellyGen1:
		return DriverShellyGen1
	case DriverShellyGen2:
		return DriverShellyGen2
	case DriverShellyHTTP:
		return DriverShellyHTTP
	case DriverZigbee2MQTT:
		return DriverZigbee2MQTT
	case DriverESPHome:
		return DriverESPHome
	case DriverOpenBeken:
		return DriverOpenBeken
	case DriverGenericMQTT:
		return DriverGenericMQTT
	case DriverHomeAssistant:
		return DriverHomeAssistant
	default:
		return ""
	}
}

func isValidDriverInput(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	return normalizeDriverName(raw) != ""
}

func normalizeShellyGen1ID(raw string) string {
	s := strings.Trim(strings.TrimSpace(raw), "/")
	s = strings.TrimPrefix(s, "shellies/")
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return strings.Trim(s, "/")
}

func normalizeZ2MTopic(raw string) string {
	s := strings.Trim(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, "/set")
	s = strings.TrimSuffix(s, "/get")
	s = strings.TrimSuffix(s, "/availability")
	if !strings.Contains(s, "/") {
		return "zigbee2mqtt/" + s
	}
	return s
}

func normalizeMQTTPrefix(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "/")
}

func normalizeBaseURL(raw string) string {
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
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func (c DeviceConfig) effectiveDriver() string {
	if d := strings.ToLower(strings.TrimSpace(c.Driver)); d != "" {
		if norm := normalizeDriverName(d); norm != "" {
			return norm
		}
	}
	if strings.EqualFold(strings.TrimSpace(c.Mode), "http") {
		return DriverTasmotaHTTP
	}
	if strings.TrimSpace(c.HAEntityID) != "" {
		return DriverHomeAssistant
	}
	if strings.TrimSpace(c.MQTTCommandTopic) != "" {
		return DriverGenericMQTT
	}
	if normalizeHTTPHost(c.HTTPHost) != "" && normalizeTopic(c.TasmotaTopic) == "" {
		return DriverTasmotaHTTP
	}
	if normalizeTopic(c.TasmotaTopic) != "" {
		return DriverTasmota
	}
	return ""
}

func (c DeviceConfig) usesMQTT() bool {
	switch c.effectiveDriver() {
	case DriverTasmota, DriverShellyGen1, DriverShellyGen2, DriverZigbee2MQTT, DriverESPHome, DriverOpenBeken, DriverGenericMQTT:
		return true
	default:
		return false
	}
}

func (c DeviceConfig) usesHTTP() bool {
	switch c.effectiveDriver() {
	case DriverTasmotaHTTP, DriverShellyHTTP:
		return true
	default:
		return false
	}
}

func (c DeviceConfig) usesHomeAssistant() bool {
	return c.effectiveDriver() == DriverHomeAssistant
}

func (c DeviceConfig) driverSubscriptions() []string {
	d := c.effectiveDriver()
	relay := c.relayIndex()
	zeroIdx := relay - 1
	switch d {
	case DriverShellyGen1:
		id := normalizeShellyGen1ID(c.TasmotaTopic)
		if id == "" {
			return nil
		}
		return []string{
			fmt.Sprintf("shellies/%s/online", id),
			fmt.Sprintf("shellies/%s/relay/%d", id, zeroIdx),
			fmt.Sprintf("shellies/%s/relay/%d/power", id, zeroIdx),
			fmt.Sprintf("shellies/%s/overtemperature", id),
			fmt.Sprintf("shellies/%s/info", id),
		}
	case DriverShellyGen2:
		prefix := normalizeMQTTPrefix(c.TasmotaTopic)
		if prefix == "" {
			return nil
		}
		return []string{
			fmt.Sprintf("%s/online", prefix),
			fmt.Sprintf("%s/status/switch:%d", prefix, zeroIdx),
			fmt.Sprintf("%s/status/wifi", prefix),
		}
	case DriverZigbee2MQTT:
		base := normalizeZ2MTopic(c.TasmotaTopic)
		if base == "" {
			return nil
		}
		return []string{
			base,
			base + "/availability",
		}
	case DriverESPHome:
		prefix := normalizeMQTTPrefix(c.TasmotaTopic)
		if prefix == "" {
			return nil
		}
		subs := []string{prefix + "/state"}
		if idx := strings.Index(prefix, "/"); idx > 0 {
			subs = append(subs, prefix[:idx]+"/status")
		}
		return subs
	case DriverOpenBeken:
		name := normalizeMQTTPrefix(c.TasmotaTopic)
		if name == "" {
			return nil
		}
		return []string{
			fmt.Sprintf("%s/%d/get", name, zeroIdx),
			fmt.Sprintf("%s/%d/get", name, relay),
			name + "/connected",
			name + "/power/get",
			name + "/voltage/get",
			name + "/current/get",
		}
	case DriverGenericMQTT:
		var subs []string
		if t := strings.TrimSpace(c.MQTTStateTopic); t != "" {
			subs = append(subs, t)
		}
		if t := strings.TrimSpace(c.MQTTAvailabilityTopic); t != "" {
			subs = append(subs, t)
		}
		if t := strings.TrimSpace(c.MQTTPowerTopic); t != "" {
			subs = append(subs, t)
		}
		return subs
	default:
		return nil
	}
}

func (m *module) syncDriverSubscriptions() {
	m.mu.RLock()
	c := m.mqttClient
	up := m.mqttUp
	if c == nil || !up {
		m.mu.RUnlock()
		return
	}
	seen := make(map[string]struct{})
	var subs []string
	addSub := func(t string) {
		if t == "" {
			return
		}
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			subs = append(subs, t)
		}
	}
	// Global lightweight discovery for Shelly Gen1
	addSub("shellies/announce")
	addSub("shellies/+/online")
	addSub("shellies/+/relay/+")
	addSub("shellies/+/relay/+/power")
	for _, cfg := range m.configs {
		for _, sub := range cfg.driverSubscriptions() {
			addSub(sub)
		}
	}
	m.mu.RUnlock()

	for _, sub := range subs {
		c.Subscribe(sub, 0, m.onMQTTMessage)
	}
}

func (m *module) queryDriverState(cfg DeviceConfig) {
	m.mu.RLock()
	c := m.mqttClient
	up := m.mqttUp
	m.mu.RUnlock()
	if c == nil || !up {
		return
	}
	switch cfg.effectiveDriver() {
	case DriverShellyGen2:
		prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
		if prefix != "" {
			c.Publish(prefix+"/command", 1, false, "status_update")
		}
	case DriverZigbee2MQTT:
		base := normalizeZ2MTopic(cfg.TasmotaTopic)
		if base != "" {
			c.Publish(base+"/get", 1, false, `{"state":""}`)
		}
	}
}

// handleDriverMQTTPacket processes incoming MQTT messages for Shelly Gen1/Gen2+,
// Zigbee2MQTT, ESPHome, OpenBeken, and Generic MQTT drivers.
// Returns true if the packet was handled by a non-Tasmota driver.
func (m *module) handleDriverMQTTPacket(msgTopic string, payloadBytes []byte, now time.Time) bool {
	payload := strings.TrimSpace(string(payloadBytes))
	handled := false

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Shelly Gen1 global discovery & telemetry: shellies/<id>/...
	if msgTopic == "shellies/announce" && strings.HasPrefix(payload, "{") {
		var ann struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		}
		if json.Unmarshal(payloadBytes, &ann) == nil && ann.ID != "" {
			key := "shelly1:" + ann.ID
			plug := m.ensureDriverPlugLocked(key, DriverShellyGen1, ann.ID)
			if ann.Model != "" {
				plug.Model = ann.Model
			}
			plug.UpdatedAt = &now
			return true
		}
	}
	if strings.HasPrefix(msgTopic, "shellies/") {
		parts := strings.Split(msgTopic, "/")
		if len(parts) >= 3 {
			id := parts[1]
			key := "shelly1:" + id
			plug := m.ensureDriverPlugLocked(key, DriverShellyGen1, id)
			switch {
			case len(parts) == 3 && parts[2] == "online":
				on := strings.EqualFold(payload, "true") || payload == "1"
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			case len(parts) == 4 && parts[2] == "relay":
				if idx, err := strconv.Atoi(parts[3]); err == nil && idx >= 0 && idx < 8 {
					relayNum := idx + 1
					switch strings.ToLower(payload) {
					case "on":
						plug.setRelayState(relayNum, "ON")
						plug.LastError = ""
						plug.setProtection(relayNum, "")
						plug.UpdatedAt = &now
						m.notifyWaitersLocked(key, relayNum, stateEvent{State: "ON"})
					case "off":
						plug.setRelayState(relayNum, "OFF")
						plug.UpdatedAt = &now
						m.notifyWaitersLocked(key, relayNum, stateEvent{State: "OFF"})
					case "overpower":
						plug.setRelayState(relayNum, "OFF")
						plug.LastError = "overpower protection triggered"
						plug.setProtection(relayNum, protectOverpower)
						plug.UpdatedAt = &now
						m.notifyWaitersLocked(key, relayNum, stateEvent{State: "OFF", Err: "overpower protection triggered"})
					}
					if relayNum > plug.RelayCount {
						plug.RelayCount = relayNum
					}
					handled = true
				}
			case len(parts) == 5 && parts[2] == "relay" && parts[4] == "power":
				if w, ok := parseReading(payload); ok {
					plug.PowerWatts = &w
					plug.meterAt = now
					plug.UpdatedAt = &now
					handled = true
				}
			case len(parts) == 3 && parts[2] == "overtemperature":
				// The whole device is too hot. Its relays are off until it
				// has cooled down.
				switch payload {
				case "1":
					plug.setProtection(0, protectOvertemp)
				case "0":
					if plug.protections[0] == protectOvertemp {
						plug.setProtection(0, "")
					}
				}
				plug.UpdatedAt = &now
				handled = true
			case len(parts) == 3 && parts[2] == "info" && strings.HasPrefix(payload, "{"):
				// Published by the firmware by itself. Only the signal is
				// read; the name of the network is not.
				var info struct {
					WifiSta struct {
						RSSI *float64 `json:"rssi"`
					} `json:"wifi_sta"`
				}
				if json.Unmarshal(payloadBytes, &info) == nil && info.WifiSta.RSSI != nil {
					plug.setWifi(*info.WifiSta.RSSI)
				}
				handled = true
			}
		}
	}

	// 2. Configured non-Tasmota MQTT drivers
	for _, cfg := range m.configs {
		d := cfg.effectiveDriver()
		if d == "" || d == DriverTasmota || !cfg.usesMQTT() {
			continue
		}
		key := cfg.plugKey()
		relay := cfg.relayIndex()
		zeroIdx := relay - 1

		switch d {
		case DriverShellyGen2:
			prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
			if msgTopic == prefix+"/online" {
				plug := m.ensureDriverPlugLocked(key, d, prefix)
				on := strings.EqualFold(payload, "true") || payload == "1"
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			} else if msgTopic == prefix+"/status/wifi" && strings.HasPrefix(payload, "{") {
				var wifi struct {
					RSSI *float64 `json:"rssi"`
				}
				if json.Unmarshal(payloadBytes, &wifi) == nil && wifi.RSSI != nil {
					m.ensureDriverPlugLocked(key, d, prefix).setWifi(*wifi.RSSI)
				}
				handled = true
			} else if msgTopic == fmt.Sprintf("%s/status/switch:%d", prefix, zeroIdx) && strings.HasPrefix(payload, "{") {
				var st struct {
					Output  *bool    `json:"output"`
					APower  *float64 `json:"apower"`
					Voltage *float64 `json:"voltage"`
					Current *float64 `json:"current"`
					Errors  []string `json:"errors"`
				}
				if json.Unmarshal(payloadBytes, &st) == nil {
					plug := m.ensureDriverPlugLocked(key, d, prefix)
					online := true
					plug.Online = &online
					plug.UpdatedAt = &now
					plug.setProtection(relay, shellyProtection(st.Errors))
					if st.Output != nil {
						state := "OFF"
						if *st.Output {
							state = "ON"
						}
						plug.setRelayState(relay, state)
						m.notifyWaitersLocked(key, relay, stateEvent{State: state})
					}
					if st.APower != nil {
						plug.PowerWatts = st.APower
						plug.meterAt = now
					}
					if st.Voltage != nil {
						plug.Voltage = st.Voltage
					}
					if st.Current != nil {
						plug.CurrentAmps = st.Current
					}
					handled = true
				}
			}

		case DriverZigbee2MQTT:
			base := normalizeZ2MTopic(cfg.TasmotaTopic)
			if msgTopic == base+"/availability" {
				plug := m.ensureDriverPlugLocked(key, d, strings.TrimPrefix(base, "zigbee2mqtt/"))
				on := parseZ2MAvailability(payload)
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			} else if msgTopic == base && strings.HasPrefix(payload, "{") {
				var obj map[string]any
				if json.Unmarshal(payloadBytes, &obj) == nil {
					plug := m.ensureDriverPlugLocked(key, d, strings.TrimPrefix(base, "zigbee2mqtt/"))
					online := true
					if plug.Online == nil {
						plug.Online = &online
					}
					plug.UpdatedAt = &now
					stateKey := "state"
					if relay > 1 {
						stateKey = fmt.Sprintf("state_l%d", relay)
					}
					if rawSt, ok := obj[stateKey]; ok {
						if s, ok := rawSt.(string); ok {
							if norm, ok := normalizeGenericBoolState(s); ok {
								plug.setRelayState(relay, norm)
								m.notifyWaitersLocked(key, relay, stateEvent{State: norm})
							}
						}
					}
					if v, ok := extractFloat(obj["power"]); ok {
						plug.PowerWatts = &v
						plug.meterAt = now
					}
					if v, ok := extractFloat(obj["voltage"]); ok {
						plug.Voltage = &v
					}
					if v, ok := extractFloat(obj["current"]); ok {
						plug.CurrentAmps = &v
					}
					handled = true
				}
			}

		case DriverESPHome:
			prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
			statusTopic := ""
			if idx := strings.Index(prefix, "/"); idx > 0 {
				statusTopic = prefix[:idx] + "/status"
			}
			if statusTopic != "" && msgTopic == statusTopic {
				plug := m.ensureDriverPlugLocked(key, d, prefix)
				on := strings.EqualFold(payload, "online")
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			} else if msgTopic == prefix+"/state" {
				plug := m.ensureDriverPlugLocked(key, d, prefix)
				if norm, ok := normalizeGenericBoolState(payload); ok {
					online := true
					if plug.Online == nil {
						plug.Online = &online
					}
					plug.setRelayState(relay, norm)
					plug.UpdatedAt = &now
					m.notifyWaitersLocked(key, relay, stateEvent{State: norm})
				}
				handled = true
			}

		case DriverOpenBeken:
			name := normalizeMQTTPrefix(cfg.TasmotaTopic)
			if msgTopic == name+"/connected" {
				plug := m.ensureDriverPlugLocked(key, d, name)
				on := strings.EqualFold(payload, "online") || payload == "1" || strings.EqualFold(payload, "true")
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			} else if msgTopic == fmt.Sprintf("%s/%d/get", name, zeroIdx) || msgTopic == fmt.Sprintf("%s/%d/get", name, relay) {
				plug := m.ensureDriverPlugLocked(key, d, name)
				if norm, ok := normalizeGenericBoolState(payload); ok {
					online := true
					if plug.Online == nil {
						plug.Online = &online
					}
					plug.setRelayState(relay, norm)
					plug.UpdatedAt = &now
					m.notifyWaitersLocked(key, relay, stateEvent{State: norm})
				}
				handled = true
			} else if msgTopic == name+"/power/get" {
				if w, ok := parseReading(payload); ok {
					plug := m.ensureDriverPlugLocked(key, d, name)
					plug.PowerWatts = &w
					plug.meterAt = now
					plug.UpdatedAt = &now
					handled = true
				}
			} else if msgTopic == name+"/voltage/get" {
				if v, ok := parseReading(payload); ok {
					plug := m.ensureDriverPlugLocked(key, d, name)
					plug.Voltage = &v
					plug.UpdatedAt = &now
					handled = true
				}
			} else if msgTopic == name+"/current/get" {
				if a, ok := parseReading(payload); ok {
					plug := m.ensureDriverPlugLocked(key, d, name)
					plug.CurrentAmps = &a
					plug.UpdatedAt = &now
					handled = true
				}
			}

		case DriverGenericMQTT:
			if cfg.MQTTAvailabilityTopic != "" && msgTopic == strings.TrimSpace(cfg.MQTTAvailabilityTopic) {
				plug := m.ensureDriverPlugLocked(key, d, cfg.MQTTCommandTopic)
				on := parseZ2MAvailability(payload)
				plug.Online = &on
				plug.UpdatedAt = &now
				handled = true
			}
			if cfg.MQTTStateTopic != "" && msgTopic == strings.TrimSpace(cfg.MQTTStateTopic) {
				plug := m.ensureDriverPlugLocked(key, d, cfg.MQTTCommandTopic)
				val := payload
				if cfg.MQTTStateJSONKey != "" && strings.HasPrefix(payload, "{") {
					var obj map[string]any
					if json.Unmarshal(payloadBytes, &obj) == nil {
						if extracted, ok := obj[cfg.MQTTStateJSONKey]; ok {
							val = fmt.Sprint(extracted)
						}
					}
				}
				stateOn := cfg.effectiveStateOn()
				stateOff := cfg.effectiveStateOff()
				var norm string
				switch {
				case strings.EqualFold(val, stateOn):
					norm = "ON"
				case strings.EqualFold(val, stateOff):
					norm = "OFF"
				default:
					norm, _ = normalizeGenericBoolState(val)
				}
				if norm != "" {
					online := true
					if plug.Online == nil {
						plug.Online = &online
					}
					plug.setRelayState(relay, norm)
					plug.UpdatedAt = &now
					m.notifyWaitersLocked(key, relay, stateEvent{State: norm})
				}
				handled = true
			}
			if cfg.MQTTPowerTopic != "" && msgTopic == strings.TrimSpace(cfg.MQTTPowerTopic) {
				plug := m.ensureDriverPlugLocked(key, d, cfg.MQTTCommandTopic)
				if cfg.MQTTPowerJSONKey != "" && strings.HasPrefix(payload, "{") {
					var obj map[string]any
					if json.Unmarshal(payloadBytes, &obj) == nil {
						if w, ok := extractFloat(obj[cfg.MQTTPowerJSONKey]); ok {
							plug.PowerWatts = &w
							plug.meterAt = now
							plug.UpdatedAt = &now
						}
					}
				} else if w, ok := parseReading(payload); ok {
					plug.PowerWatts = &w
					plug.meterAt = now
					plug.UpdatedAt = &now
				}
				handled = true
			}
		}
	}
	return handled
}

// shellyProtection is the protection a Shelly Gen2+ switch names in the
// errors of its status, the gravest first, or nothing.
func shellyProtection(faults []string) string {
	for _, kind := range []string{protectOvertemp, protectOverpower, protectOvervoltage, protectUndervoltage} {
		for _, fault := range faults {
			if strings.EqualFold(strings.TrimSpace(fault), kind) {
				return kind
			}
		}
	}
	return ""
}

// shellyWifiEvery is how often a Shelly read by address is asked for its
// Wi-Fi signal.
const shellyWifiEvery = time.Minute

func (m *module) ensureDriverPlugLocked(key, driver, friendly string) *PlugTelemetry {
	plug, ok := m.plugs[key]
	if !ok {
		plug = &PlugTelemetry{
			Topic:        key,
			Driver:       driver,
			FriendlyName: friendly,
			Power:        "UNKNOWN",
		}
		m.plugs[key] = plug
	}
	if plug.Driver == "" {
		plug.Driver = driver
	}
	if plug.FriendlyName == "" && friendly != "" {
		plug.FriendlyName = friendly
	}
	return plug
}

func (c DeviceConfig) effectivePayloadOn() string {
	if s := strings.TrimSpace(c.MQTTPayloadOn); s != "" {
		return s
	}
	return "ON"
}

func (c DeviceConfig) effectivePayloadOff() string {
	if s := strings.TrimSpace(c.MQTTPayloadOff); s != "" {
		return s
	}
	return "OFF"
}

func (c DeviceConfig) effectiveStateOn() string {
	if s := strings.TrimSpace(c.MQTTStateOn); s != "" {
		return s
	}
	return c.effectivePayloadOn()
}

func (c DeviceConfig) effectiveStateOff() string {
	if s := strings.TrimSpace(c.MQTTStateOff); s != "" {
		return s
	}
	return c.effectivePayloadOff()
}

func parseZ2MAvailability(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "{") {
		var obj struct {
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			s = obj.State
		}
	}
	return strings.EqualFold(s, "online") || strings.EqualFold(s, "true") || s == "1"
}

func normalizeGenericBoolState(raw string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "ON", "1", "TRUE":
		return "ON", true
	case "OFF", "0", "FALSE":
		return "OFF", true
	default:
		return "", false
	}
}

// parseReading reads a number a plug reported as text. "nan" and "inf" parse
// as numbers, and are what some firmware sends for a sensor with no value.
// They are no reading: a sum that took one in would never be a number again,
// and JSON cannot hold them.
func parseReading(text string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func extractFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case string:
		return parseReading(n)
	default:
		return 0, false
	}
}

// switchMQTTDriverVerified publishes a command for any non-Tasmota MQTT driver
// and waits up to 2.5s for a matching state confirmation.
func (m *module) switchMQTTDriverVerified(ctx context.Context, cfg DeviceConfig, wantOn bool) error {
	d := cfg.effectiveDriver()
	key := cfg.plugKey()
	relay := cfg.relayIndex()
	zeroIdx := relay - 1
	wantState := "OFF"
	if wantOn {
		wantState = "ON"
	}

	var cmdTopic, cmdPayload string
	switch d {
	case DriverShellyGen1:
		id := normalizeShellyGen1ID(cfg.TasmotaTopic)
		cmdTopic = fmt.Sprintf("shellies/%s/relay/%d/command", id, zeroIdx)
		if wantOn {
			cmdPayload = "on"
		} else {
			cmdPayload = "off"
		}
	case DriverShellyGen2:
		prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
		cmdTopic = fmt.Sprintf("%s/command/switch:%d", prefix, zeroIdx)
		if wantOn {
			cmdPayload = "on"
		} else {
			cmdPayload = "off"
		}
	case DriverZigbee2MQTT:
		base := normalizeZ2MTopic(cfg.TasmotaTopic)
		cmdTopic = base + "/set"
		stateKey := "state"
		if relay > 1 {
			stateKey = fmt.Sprintf("state_l%d", relay)
		}
		cmdPayload = fmt.Sprintf(`{"%s":"%s"}`, stateKey, wantState)
	case DriverESPHome:
		prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
		cmdTopic = prefix + "/command"
		cmdPayload = wantState
	case DriverOpenBeken:
		name := normalizeMQTTPrefix(cfg.TasmotaTopic)
		cmdTopic = fmt.Sprintf("%s/%d/set", name, zeroIdx)
		if wantOn {
			cmdPayload = "1"
		} else {
			cmdPayload = "0"
		}
	case DriverGenericMQTT:
		cmdTopic = strings.TrimSpace(cfg.MQTTCommandTopic)
		if wantOn {
			cmdPayload = cfg.effectivePayloadOn()
		} else {
			cmdPayload = cfg.effectivePayloadOff()
		}
	default:
		return fmt.Errorf("unsupported MQTT driver %q", d)
	}

	m.mu.Lock()
	c := m.mqttClient
	up := m.mqttUp
	if c == nil || !up {
		m.mu.Unlock()
		return errBrokerOffline
	}
	plug := m.plugs[key]
	if plug != nil && plug.Online != nil && !*plug.Online {
		m.mu.Unlock()
		return errors.New("smart plug is offline")
	}
	waiter := &stateWaiter{
		topic: key,
		relay: relay,
		ch:    make(chan stateEvent, 8),
	}
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()
	defer m.removeWaiter(waiter)

	tok := c.Publish(cmdTopic, 1, false, cmdPayload)
	if !tok.WaitTimeout(3 * time.Second) {
		return errors.New("MQTT publish timed out")
	}
	if err := tok.Error(); err != nil {
		return err
	}

	// Shelly Gen2+ status_ntf may be off by default; request immediate status_update.
	if d == DriverShellyGen2 {
		prefix := normalizeMQTTPrefix(cfg.TasmotaTopic)
		c.Publish(prefix+"/command", 0, false, "status_update")
	}

	// Generic MQTT without a state topic cannot confirm state; succeed after publish.
	if d == DriverGenericMQTT && strings.TrimSpace(cfg.MQTTStateTopic) == "" {
		m.mu.Lock()
		p := m.ensureDriverPlugLocked(key, d, cmdTopic)
		p.setRelayState(relay, wantState)
		now := time.Now()
		p.UpdatedAt = &now
		m.mu.Unlock()
		return nil
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
					mismatchTimer = time.NewTimer(450 * time.Millisecond)
					mismatchC = mismatchTimer.C
				}
			}
		case <-mismatchC:
			return fmt.Errorf("plug refused to switch (reported %s)", lastMismatch)
		case <-overallTimer.C:
			if lastMismatch != "" {
				return fmt.Errorf("plug refused to switch (reported %s)", lastMismatch)
			}
			return errors.New("plug did not confirm state change within 2.5s")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- Shelly Direct HTTP (Gen2+ RPC with automatic Gen1 REST fallback) ---

func (m *module) shellyHTTPRequest(ctx context.Context, cfg DeviceConfig, pathAndQuery string) (int, []byte, error) {
	base := cfg.httpBaseURL()
	if base == "" {
		return 0, nil, errors.New("Shelly HTTP host or IP is required")
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		reqCtx, cancel = context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+pathAndQuery, nil)
	if err != nil {
		return 0, nil, err
	}
	if cfg.HTTPPassword != "" {
		user := strings.TrimSpace(cfg.HTTPUser)
		if user == "" {
			user = "admin"
		}
		req.SetBasicAuth(user, cfg.HTTPPassword)
	}
	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if isRedirect(resp.StatusCode) {
		return resp.StatusCode, nil, redirectError("Shelly plug", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, body, errors.New("Shelly HTTP authentication failed (check credentials)")
	}
	return resp.StatusCode, body, nil
}

func (m *module) pollShellyHTTPPlug(ctx context.Context, cfg DeviceConfig) error {
	key := cfg.plugKey()
	relay := cfg.relayIndex()
	zeroIdx := relay - 1

	// Try Shelly Gen2+ RPC first: /rpc/Switch.GetStatus?id=<i>
	status, raw, err := m.shellyHTTPRequest(ctx, cfg, fmt.Sprintf("/rpc/Switch.GetStatus?id=%d", zeroIdx))
	if err == nil && status >= 200 && status < 300 {
		var gen2 struct {
			Output  *bool    `json:"output"`
			APower  *float64 `json:"apower"`
			Voltage *float64 `json:"voltage"`
			Current *float64 `json:"current"`
			Errors  []string `json:"errors"`
		}
		if json.Unmarshal(raw, &gen2) == nil && gen2.Output != nil {
			now := time.Now()
			online := true
			state := "OFF"
			if *gen2.Output {
				state = "ON"
			}
			m.mu.Lock()
			plug := m.ensureDriverPlugLocked(key, DriverShellyHTTP, cfg.HTTPHost)
			plug.Online = &online
			plug.LastError = ""
			plug.UpdatedAt = &now
			plug.setRelayState(relay, state)
			if gen2.APower != nil {
				plug.PowerWatts = gen2.APower
				plug.meterAt = now
			}
			if gen2.Voltage != nil {
				plug.Voltage = gen2.Voltage
			}
			if gen2.Current != nil {
				plug.CurrentAmps = gen2.Current
			}
			plug.setProtection(relay, shellyProtection(gen2.Errors))
			asked := m.clock()
			askWifi := plug.wifiAt.IsZero() || asked.Sub(plug.wifiAt) >= shellyWifiEvery || asked.Before(plug.wifiAt)
			if askWifi {
				plug.wifiAt = asked
			}
			m.mu.Unlock()

			// The signal and what the relay does after a power cut are two
			// more questions to the same plug. They are asked once a minute,
			// and an answer that does not come changes nothing.
			if askWifi {
				var relayCfg struct {
					InitialState string `json:"initial_state"`
				}
				cfgStatus, cfgRaw, cfgErr := m.shellyHTTPRequest(ctx, cfg, fmt.Sprintf("/rpc/Switch.GetConfig?id=%d", zeroIdx))
				if cfgErr == nil && cfgStatus >= 200 && cfgStatus < 300 && json.Unmarshal(cfgRaw, &relayCfg) == nil {
					m.mu.Lock()
					if held := m.plugs[key]; held != nil {
						held.setAfterPowerCut(relay, shellyGen2AfterPowerCut(relayCfg.InitialState))
					}
					m.mu.Unlock()
				}
				var wifi struct {
					RSSI *float64 `json:"rssi"`
				}
				wifiStatus, wifiRaw, wifiErr := m.shellyHTTPRequest(ctx, cfg, "/rpc/Wifi.GetStatus")
				if wifiErr == nil && wifiStatus >= 200 && wifiStatus < 300 && json.Unmarshal(wifiRaw, &wifi) == nil && wifi.RSSI != nil {
					m.mu.Lock()
					if held := m.plugs[key]; held != nil {
						held.setWifi(*wifi.RSSI)
					}
					m.mu.Unlock()
				}
			}
			return nil
		}
	}

	// Fallback to Shelly Gen1 REST: /status
	if err == nil && (status == http.StatusNotFound || status == http.StatusBadRequest) {
		status, raw, err = m.shellyHTTPRequest(ctx, cfg, "/status")
		if err == nil && status >= 200 && status < 300 {
			var gen1 struct {
				Relays []struct {
					IsOn      bool `json:"ison"`
					Overpower bool `json:"overpower"`
				} `json:"relays"`
				Meters []struct {
					Power float64 `json:"power"`
				} `json:"meters"`
				Overtemperature bool `json:"overtemperature"`
				WifiSta         struct {
					RSSI *float64 `json:"rssi"`
				} `json:"wifi_sta"`
			}
			if json.Unmarshal(raw, &gen1) == nil && len(gen1.Relays) > zeroIdx {
				now := time.Now()
				online := true
				r := gen1.Relays[zeroIdx]
				state := "OFF"
				if r.IsOn {
					state = "ON"
				}
				m.mu.Lock()
				plug := m.ensureDriverPlugLocked(key, DriverShellyHTTP, cfg.HTTPHost)
				plug.Online = &online
				plug.UpdatedAt = &now
				plug.RelayCount = len(gen1.Relays)
				plug.setRelayState(relay, state)
				if r.Overpower {
					plug.LastError = "overpower protection triggered"
					plug.setProtection(relay, protectOverpower)
				} else {
					plug.LastError = ""
					plug.setProtection(relay, "")
				}
				if gen1.Overtemperature {
					plug.setProtection(0, protectOvertemp)
				} else if plug.protections[0] == protectOvertemp {
					plug.setProtection(0, "")
				}
				if gen1.WifiSta.RSSI != nil {
					plug.setWifi(*gen1.WifiSta.RSSI)
				}
				if len(gen1.Meters) > zeroIdx {
					w := gen1.Meters[zeroIdx].Power
					plug.PowerWatts = &w
					plug.meterAt = now
				}
				asked := m.clock()
				askSettings := plug.wifiAt.IsZero() || asked.Sub(plug.wifiAt) >= shellyWifiEvery || asked.Before(plug.wifiAt)
				if askSettings {
					plug.wifiAt = asked
				}
				m.mu.Unlock()

				// What the relay does after a power cut is in its settings, a
				// second question that is asked once a minute.
				if askSettings {
					var relaySettings struct {
						DefaultState string `json:"default_state"`
					}
					setStatus, setRaw, setErr := m.shellyHTTPRequest(ctx, cfg, fmt.Sprintf("/settings/relay/%d", zeroIdx))
					if setErr == nil && setStatus >= 200 && setStatus < 300 && json.Unmarshal(setRaw, &relaySettings) == nil {
						m.mu.Lock()
						if held := m.plugs[key]; held != nil {
							held.setAfterPowerCut(relay, shellyGen1AfterPowerCut(relaySettings.DefaultState))
						}
						m.mu.Unlock()
					}
				}
				return nil
			}
		}
	}

	if err == nil {
		err = fmt.Errorf("Shelly returned HTTP %d", status)
	}
	m.mu.Lock()
	plug := m.ensureDriverPlugLocked(key, DriverShellyHTTP, cfg.HTTPHost)
	offline := false
	plug.Online = &offline
	plug.LastError = err.Error()
	m.mu.Unlock()
	return err
}

func (m *module) switchShellyHTTPPlugVerified(ctx context.Context, cfg DeviceConfig, wantOn bool) error {
	key := cfg.plugKey()
	relay := cfg.relayIndex()
	zeroIdx := relay - 1
	wantState := "OFF"
	onBool := "false"
	turnWord := "off"
	if wantOn {
		wantState = "ON"
		onBool = "true"
		turnWord = "on"
	}

	// Try Gen2+ RPC first
	status, raw, err := m.shellyHTTPRequest(ctx, cfg, fmt.Sprintf("/rpc/Switch.Set?id=%d&on=%s", zeroIdx, onBool))
	if err == nil && status >= 200 && status < 300 {
		// Gen2+ Switch.Set returns {"was_on": ...}; verify with Switch.GetStatus
		if pollErr := m.pollShellyHTTPPlug(ctx, cfg); pollErr != nil {
			return pollErr
		}
		m.mu.RLock()
		plug := m.plugs[key]
		got := ""
		if plug != nil {
			got = plug.relayState(relay)
		}
		m.mu.RUnlock()
		if got == wantState {
			return nil
		}
		return fmt.Errorf("Shelly did not switch (reported %s)", got)
	}

	// Fallback to Gen1 REST: /relay/<i>?turn=on|off
	if err == nil && (status == http.StatusNotFound || status == http.StatusBadRequest) {
		status, raw, err = m.shellyHTTPRequest(ctx, cfg, fmt.Sprintf("/relay/%d?turn=%s", zeroIdx, turnWord))
		if err != nil {
			return err
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("Shelly Gen1 returned HTTP %d", status)
		}
		var gen1Resp struct {
			IsOn bool `json:"ison"`
		}
		if err := json.Unmarshal(raw, &gen1Resp); err != nil {
			return fmt.Errorf("invalid Shelly Gen1 JSON: %w", err)
		}
		got := "OFF"
		if gen1Resp.IsOn {
			got = "ON"
		}
		now := time.Now()
		online := true
		m.mu.Lock()
		plug := m.ensureDriverPlugLocked(key, DriverShellyHTTP, cfg.HTTPHost)
		plug.Online = &online
		plug.LastError = ""
		plug.UpdatedAt = &now
		plug.setRelayState(relay, got)
		m.mu.Unlock()
		if got == wantState {
			return nil
		}
		return fmt.Errorf("Shelly refused to switch (reported %s)", got)
	}

	if err != nil {
		return err
	}
	return fmt.Errorf("Shelly returned HTTP %d", status)
}

// --- Home Assistant REST Transport ---

func (m *module) haHTTPRequest(ctx context.Context, s Settings, method, apiPath string, payload any) (int, []byte, error) {
	base := normalizeBaseURL(s.HAURL)
	if base == "" {
		return 0, nil, errors.New("Home Assistant URL is not configured in Settings")
	}
	if strings.TrimSpace(s.HAToken) == "" {
		return 0, nil, errors.New("Home Assistant Long-Lived Access Token is not configured in Settings")
	}
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		reqCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, method, base+apiPath, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.HAToken))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.getHTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if isRedirect(resp.StatusCode) {
		return resp.StatusCode, nil, redirectError("Home Assistant", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, body, errors.New("Home Assistant authentication failed (check Long-Lived Access Token)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, body, fmt.Errorf("Home Assistant returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, body, nil
}

func (m *module) testHomeAssistant(ctx context.Context, override *Settings) (bool, string, []HAEntity, string) {
	saved := m.settings()
	candidate := saved
	if override != nil {
		candidate = m.applyHostPins(*override)
		if err := keepHAToken(*override, saved, &candidate); err != nil {
			return false, normalizeBaseURL(candidate.HAURL), nil, err.Error()
		}
	}
	base := normalizeBaseURL(candidate.HAURL)
	if base == "" {
		return false, "", nil, "Home Assistant URL is required"
	}
	if _, _, err := m.haHTTPRequest(ctx, candidate, http.MethodGet, "/api/", nil); err != nil {
		return false, base, nil, err.Error()
	}
	entities, _ := m.fetchHAEntities(ctx, candidate)
	return true, base, entities, ""
}

func (m *module) fetchHAEntities(ctx context.Context, s Settings) ([]HAEntity, error) {
	if normalizeBaseURL(s.HAURL) == "" || strings.TrimSpace(s.HAToken) == "" {
		return nil, nil
	}
	_, raw, err := m.haHTTPRequest(ctx, s, http.MethodGet, "/api/states", nil)
	if err != nil {
		return nil, err
	}
	var states []struct {
		EntityID   string `json:"entity_id"`
		State      string `json:"state"`
		Attributes struct {
			FriendlyName string `json:"friendly_name"`
			DeviceClass  string `json:"device_class"`
			Unit         string `json:"unit_of_measurement"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, err
	}
	out := make([]HAEntity, 0, 32)
	for _, st := range states {
		domain, _, _ := strings.Cut(st.EntityID, ".")
		isSwitch := domain == "switch" || domain == "input_boolean"
		isPowerSensor := domain == "sensor" && (strings.EqualFold(st.Attributes.DeviceClass, "power") || strings.EqualFold(st.Attributes.Unit, "W"))
		if !isSwitch && !isPowerSensor {
			continue
		}
		out = append(out, HAEntity{
			EntityID:     st.EntityID,
			FriendlyName: st.Attributes.FriendlyName,
			State:        st.State,
			Domain:       domain,
			DeviceClass:  st.Attributes.DeviceClass,
			Unit:         st.Attributes.Unit,
		})
	}
	return out, nil
}

// errUnsupportedHAEntity is the refusal for an entity the extension does not
// switch or read, such as a cover, lock, alarm, script or automation.
var errUnsupportedHAEntity = errors.New("unsupported Home Assistant entity (use a switch, input_boolean, light or fan, and a sensor for power)")

// validHAEntity reports whether id is an entity the extension may act on: a
// switch, input_boolean, light or fan, or with power a sensor. The ids the
// entity picker offers are of these domains.
func validHAEntity(id string, power bool) bool {
	domain, object, ok := strings.Cut(id, ".")
	if !ok || object == "" {
		return false
	}
	for _, c := range object {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	if power {
		return domain == "sensor"
	}
	switch domain {
	case "switch", "input_boolean", "light", "fan":
		return true
	}
	return false
}

func (m *module) pollHomeAssistantPlug(ctx context.Context, cfg DeviceConfig) error {
	entityID := strings.TrimSpace(cfg.HAEntityID)
	if entityID == "" {
		return errors.New("Home Assistant entity_id is required")
	}
	key := cfg.plugKey()
	s := m.settings()

	var raw []byte
	var err error
	if validHAEntity(entityID, false) {
		_, raw, err = m.haHTTPRequest(ctx, s, http.MethodGet, "/api/states/"+url.PathEscape(entityID), nil)
	} else {
		err = errUnsupportedHAEntity
	}
	if err != nil {
		m.mu.Lock()
		plug := m.ensureDriverPlugLocked(key, DriverHomeAssistant, entityID)
		offline := false
		plug.Online = &offline
		plug.LastError = err.Error()
		m.mu.Unlock()
		return err
	}

	var st struct {
		State      string `json:"state"`
		Attributes struct {
			FriendlyName string `json:"friendly_name"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("invalid Home Assistant state JSON: %w", err)
	}

	// The sensor is asked on every poll, so a poll without a reading (the
	// sensor is unavailable, or its request failed) leaves no reading. The
	// last one must not stand: energy would go on counting from it.
	var wattsPtr *float64
	pEnt := strings.TrimSpace(cfg.HAPowerEntityID)
	if pEnt != "" && validHAEntity(pEnt, true) {
		if _, pRaw, pErr := m.haHTTPRequest(ctx, s, http.MethodGet, "/api/states/"+url.PathEscape(pEnt), nil); pErr == nil {
			var pSt struct {
				State      string `json:"state"`
				Attributes struct {
					Unit string `json:"unit_of_measurement"`
				} `json:"attributes"`
			}
			if json.Unmarshal(pRaw, &pSt) == nil {
				if w, ok := haPowerWatts(pSt.State, pSt.Attributes.Unit); ok {
					wattsPtr = &w
				}
			}
		}
	}

	now := time.Now()
	online := !strings.EqualFold(st.State, "unavailable") && !strings.EqualFold(st.State, "unknown")
	m.mu.Lock()
	plug := m.ensureDriverPlugLocked(key, DriverHomeAssistant, entityID)
	plug.Online = &online
	plug.UpdatedAt = &now
	if st.Attributes.FriendlyName != "" {
		plug.FriendlyName = st.Attributes.FriendlyName
	}
	if online {
		plug.LastError = ""
		if norm, ok := normalizeGenericBoolState(st.State); ok {
			plug.setRelayState(1, norm)
		}
	} else {
		plug.LastError = fmt.Sprintf("Home Assistant entity is %s", st.State)
	}
	if wattsPtr != nil {
		plug.PowerWatts = wattsPtr
		plug.meterAt = now
	} else if pEnt != "" {
		plug.PowerWatts = nil
	}
	m.mu.Unlock()
	return nil
}

// haPowerWatts reads a Home Assistant power sensor's state in watts. The
// sensor says its own unit; one without a unit is taken as watts. A state
// that is no number ("unavailable", "unknown"), and a unit that is no power
// unit, give no reading.
func haPowerWatts(state, unit string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(state), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	switch strings.TrimSpace(unit) {
	case "", "W":
		return v, true
	case "kW":
		return v * 1000, true
	case "MW":
		return v * 1e6, true
	case "mW":
		return v / 1000, true
	default:
		return 0, false
	}
}

func (m *module) switchHomeAssistantPlugVerified(ctx context.Context, cfg DeviceConfig, wantOn bool) error {
	entityID := strings.TrimSpace(cfg.HAEntityID)
	if entityID == "" {
		return errors.New("Home Assistant entity_id is required")
	}
	if !validHAEntity(entityID, false) {
		return errUnsupportedHAEntity
	}
	s := m.settings()
	service := "turn_off"
	wantState := "OFF"
	if wantOn {
		service = "turn_on"
		wantState = "ON"
	}
	_, _, err := m.haHTTPRequest(ctx, s, http.MethodPost, "/api/services/homeassistant/"+service, map[string]string{
		"entity_id": entityID,
	})
	if err != nil {
		return err
	}
	return m.confirmHomeAssistantState(ctx, cfg, wantState)
}

// Many Home Assistant integrations change an entity's state a moment after
// the service call returned, so the state is read again for a while before
// a switch counts as refused, as the MQTT drivers wait for their answer.
var (
	haConfirmWait = 2500 * time.Millisecond
	haConfirmStep = 250 * time.Millisecond
)

// confirmHomeAssistantState reads the entity until it reports wantState. It
// gives up at once when the entity is unavailable or unknown, and otherwise
// after haConfirmWait.
func (m *module) confirmHomeAssistantState(ctx context.Context, cfg DeviceConfig, wantState string) error {
	deadline := time.Now().Add(haConfirmWait)
	key := cfg.plugKey()
	var lastErr error
	for {
		lastErr = m.pollHomeAssistantPlug(ctx, cfg)
		got, lost, reason := "", false, ""
		m.mu.RLock()
		if plug := m.plugs[key]; plug != nil {
			got = plug.relayState(1)
			if lastErr == nil && plug.Online != nil && !*plug.Online {
				lost, reason = true, plug.LastError
			}
		}
		m.mu.RUnlock()
		if lastErr == nil && got == wantState {
			return nil
		}
		if lost {
			return fmt.Errorf("Home Assistant entity did not confirm %s (%s)", wantState, reason)
		}
		if !time.Now().Add(haConfirmStep).Before(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("Home Assistant entity did not confirm %s within %s (reported %s)", wantState, haConfirmWait, got)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(haConfirmStep):
		}
	}
}

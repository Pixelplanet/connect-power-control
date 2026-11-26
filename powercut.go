package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// What a plug does with its relay when the power comes back after a cut. It
// is a setting of the plug. This extension reads it and never changes it.
const (
	afterCutOff       = "off"        // stays off
	afterCutOn        = "on"         // turns on
	afterCutLast      = "last"       // returns to the state it had
	afterCutToggle    = "toggle"     // the opposite of the state it had
	afterCutAlwaysOn  = "always_on"  // on, and refuses to be switched (Tasmota PowerOnState 4)
	afterCutDelayedOn = "delayed_on" // on after the plug's pulse time (Tasmota PowerOnState 5)
	afterCutSwitch    = "switch"     // follows the switch wired to the plug (Shelly)
)

// tasmotaAfterPowerCut reads a Tasmota PowerOnState, a number from 0 to 5
// that some builds send as text. Anything else gives "".
func tasmotaAfterPowerCut(raw json.RawMessage) string {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.Atoi(text)
	if err != nil {
		switch strings.ToUpper(text) {
		case "OFF":
			return afterCutOff
		case "ON":
			return afterCutOn
		case "TOGGLE":
			return afterCutToggle
		case "SAVED":
			return afterCutLast
		}
		return ""
	}
	switch n {
	case 0:
		return afterCutOff
	case 1:
		return afterCutOn
	case 2:
		return afterCutToggle
	case 3:
		return afterCutLast
	case 4:
		return afterCutAlwaysOn
	case 5:
		return afterCutDelayedOn
	}
	return ""
}

// shellyGen2AfterPowerCut reads initial_state of Switch.GetConfig.
func shellyGen2AfterPowerCut(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "off":
		return afterCutOff
	case "on":
		return afterCutOn
	case "restore_last":
		return afterCutLast
	case "match_input":
		return afterCutSwitch
	}
	return ""
}

// shellyGen1AfterPowerCut reads default_state of /settings/relay/<n>.
func shellyGen1AfterPowerCut(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "off":
		return afterCutOff
	case "on":
		return afterCutOn
	case "last":
		return afterCutLast
	case "switch":
		return afterCutSwitch
	}
	return ""
}

// setAfterPowerCut keeps what relay does after a power cut, 0 for the whole
// plug. A mode that was not understood changes nothing.
func (p *PlugTelemetry) setAfterPowerCut(relay int, mode string) {
	if mode == "" {
		return
	}
	if p.afterCut == nil {
		p.afterCut = make(map[int]string)
	}
	p.afterCut[relay] = mode
}

// afterPowerCutOf is the setting for relay: its own, else the plug's.
func (p *PlugTelemetry) afterPowerCutOf(relay int) string {
	if mode := p.afterCut[relay]; mode != "" {
		return mode
	}
	return p.afterCut[0]
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// toldEvent is one event a module told its emitter.
type toldEvent struct {
	endpoint, key string
	data          any
}

func (e toldEvent) auto() autoOffEvent {
	got, _ := e.data.(autoOffEvent)
	return got
}

func (e toldEvent) lost() plugLostEvent {
	got, _ := e.data.(plugLostEvent)
	return got
}

// emitFake is the host's bus.emit. It keeps what it was told, and answers
// the error of answers[endpoint] in place of taking the event.
type emitFake struct {
	mu      sync.Mutex
	calls   int
	events  []toldEvent
	answers map[string]error
}

func (f *emitFake) Emit(_ context.Context, endpoint, key string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.answers[endpoint]; err != nil {
		return err
	}
	f.events = append(f.events, toldEvent{endpoint: endpoint, key: key, data: data})
	return nil
}

func (f *emitFake) told() []toldEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]toldEvent(nil), f.events...)
}

func (f *emitFake) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// logged runs fn and answers what it wrote to the log.
func logged(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = old }()

	fn()

	_ = write.Close()
	buf := make([]byte, 64<<10)
	n, _ := read.Read(buf)
	_ = read.Close()
	return string(buf[:n])
}

// A host that answers "no such method", as the one of bus.emit does on an
// older core and where the operator approved no event.
func noSuchMethod() error {
	return fmt.Errorf("busapi: bus.emit: %w", &wirerpc.Error{Code: wirerpc.CodeMethodNotFound, Message: "this side serves no method bus.emit"})
}

func TestAutoOffFailedIsToldOnce(t *testing.T) {
	none := 0
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &none}, Settings{})
	plug.set(true)
	running := printerPrinting
	running.JobID = "job-9"

	step(m, clock, running, 5*time.Second)
	for range 5 {
		step(m, clock, printerFinished, 5*time.Second)
	}

	told := events.told()
	if len(told) != 1 || told[0].endpoint != endpointAutoOffFailed {
		t.Fatalf("five failed tries told %+v, want one auto_off_failed", told)
	}
	failed := told[0].auto()
	if failed.DeviceID != "p1" || failed.JobID != "job-9" || failed.Reason != reasonPrintFinished || failed.Error == "" || failed.DeviceName == "" {
		t.Fatalf("payload = %+v", failed)
	}

	// The plug answers again: the turn-off works and is told.
	plug.set(false)
	step(m, clock, printerFinished, 5*time.Second)
	told = events.told()
	if len(told) != 2 || told[1].endpoint != endpointAutoOffDone || told[1].auto().JobID != "job-9" || told[1].auto().Error != "" {
		t.Fatalf("after the turn-off worked: %+v", told)
	}

}

// A turn-off that fails is one piece of news until the printer prints again.
func TestAFailedTurnOffIsNewsAgainAfterTheNextPrint(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	failed := fmt.Errorf("plug did not answer")

	if m.noteAutoOffFault("p", failed) {
		t.Fatal("a printer nothing is followed of had news")
	}
	m.autoOffDue("p", printerPrinting, 0, now)
	m.autoOffDue("p", printerFinished, 0, now)
	if !m.noteAutoOffFault("p", failed) || m.noteAutoOffFault("p", failed) {
		t.Fatal("the first failure is news and the second is not")
	}
	m.autoOffDue("p", printerPrinting, 0, now.Add(time.Hour))
	m.autoOffDue("p", printerFinished, 0, now.Add(2*time.Hour))
	if !m.noteAutoOffFault("p", failed) {
		t.Fatal("a failure after the next print was no news")
	}
}

func TestEventsStopWhereTheHostServesNoEmit(t *testing.T) {
	m := newModule(nil)
	events := &emitFake{answers: map[string]error{endpointAutoOffDone: noSuchMethod(), endpointPlugLost: noSuchMethod()}}
	m.events = events

	log := logged(t, func() {
		m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
		m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
		m.emitEvent(context.Background(), endpointAutoOffFailed, "p1", autoOffEvent{DeviceID: "p1"})
		m.emitEvent(context.Background(), endpointPlugLost, "p1", plugLostEvent{DeviceID: "p1"})
	})

	if log != "" {
		t.Fatalf("a host that serves no bus.emit was logged: %q", log)
	}
	if events.asked() != 1 || len(events.told()) != 0 {
		t.Fatalf("the host was asked %d times and took %d events, want one question and nothing after it", events.asked(), len(events.told()))
	}
	if !m.eventsOff.Load() {
		t.Fatal("the events were not switched off")
	}
	if got := m.buildDiagnostics(time.Now()).Events; got != "off" {
		t.Fatalf("the diagnostics say events are %q", got)
	}
}

// With no event approved the whole tick runs as before: the plug is turned
// off, nothing is logged about events, and no fault is kept.
func TestATurnOffWorksWhereTheHostServesNoEmit(t *testing.T) {
	none := 0
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &none}, Settings{})
	events.answers = map[string]error{endpointAutoOffDone: noSuchMethod()}

	log := logged(t, func() {
		step(m, clock, printerPrinting, 5*time.Second)
		step(m, clock, printerFinished, 5*time.Second)
	})

	if plug.turnedOff() != 1 {
		t.Fatal("the plug was not turned off")
	}
	if strings.Contains(log, "event") || strings.Contains(log, "bus.emit") {
		t.Fatalf("the log names the events: %q", log)
	}
	if p := plugOf(m, m.configs["p1"]); p.LastError != "" {
		t.Fatalf("the plug keeps an error: %q", p.LastError)
	}
}

func TestAnUnapprovedEndpointIsLeftAlone(t *testing.T) {
	m := newModule(nil)
	notApproved := fmt.Errorf("bus.emit: %w", busapi.ErrNoEndpoint)
	events := &emitFake{answers: map[string]error{endpointPlugLost: notApproved}}
	m.events = events

	log := logged(t, func() {
		for range 3 {
			m.emitEvent(context.Background(), endpointPlugLost, "p1", plugLostEvent{DeviceID: "p1"})
		}
		m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
	})

	if log != "" {
		t.Fatalf("an endpoint the operator did not approve was logged: %q", log)
	}
	if events.asked() != 2 {
		t.Fatalf("the host was asked %d times, want once for each endpoint", events.asked())
	}
	if told := events.told(); len(told) != 1 || told[0].endpoint != endpointAutoOffDone {
		t.Fatalf("the approved endpoint was not told: %+v", told)
	}
	if m.eventsOff.Load() {
		t.Fatal("one endpoint that is not approved switched every event off")
	}
}

func TestATransientEmitErrorIsDropped(t *testing.T) {
	m := newModule(nil)
	events := &emitFake{answers: map[string]error{endpointAutoOffDone: fmt.Errorf("bus.emit: %w", busapi.ErrNotReady)}}
	m.events = events

	log := logged(t, func() {
		for range 3 {
			m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
		}
	})
	if strings.Count(log, "\n") != 1 || !strings.Contains(log, endpointAutoOffDone) {
		t.Fatalf("three dropped events logged %q, want one line", log)
	}

	// The next one is tried again, and goes through.
	events.mu.Lock()
	events.answers = nil
	events.mu.Unlock()
	m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
	if events.asked() != 4 || len(events.told()) != 1 {
		t.Fatalf("asked %d times, told %d", events.asked(), len(events.told()))
	}
	if m.eventsOff.Load() {
		t.Fatal("an error of the moment switched the events off")
	}
}

func TestNoEmitterIsHarmless(t *testing.T) {
	m := newModule(nil)
	if m.events != nil {
		t.Fatal("a module with no session has an emitter")
	}
	log := logged(t, func() {
		m.emitEvent(context.Background(), endpointAutoOffDone, "p1", autoOffEvent{DeviceID: "p1"})
		m.emitEvent(context.Background(), endpointPlugLost, "p1", plugLostEvent{DeviceID: "p1"})
	})
	if log != "" {
		t.Fatalf("a module with no session logged %q", log)
	}
}

// --- A plug that goes away during a print ---

// lostModule is a module with the printer p1 on a Tasmota plug over MQTT,
// with its broker up.
func lostModule() (*module, *testClock, DeviceConfig) {
	m, clock := clockedModule(energyStart)
	s := normalizeSettings(Settings{MQTTHost: "broker.lan"})
	m.held.Store(&s)
	m.mqttUp = true
	cfg := seedPlug(m, DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug"}, "ON", 60)
	return m, clock, cfg
}

// lostAfter moves the clock and looks once.
func lostAfter(m *module, clock *testClock, cfg DeviceConfig, safety printerSafety, d time.Duration) (string, bool) {
	clock.add(d)
	return m.plugLostDue("p1", cfg, safety, clock.now())
}

func TestPlugLostIsToldOncePerPrint(t *testing.T) {
	m, clock, cfg := lostModule()
	running := printerPrinting
	running.JobID = "job-1"

	if _, due := lostAfter(m, clock, cfg, running, 5*time.Second); due {
		t.Fatal("a plug that is there was told as lost")
	}
	setRelay(m, cfg, "ON", false)
	if _, due := lostAfter(m, clock, cfg, running, 5*time.Second); due {
		t.Fatal("the plug was told as lost at once")
	}
	if _, due := lostAfter(m, clock, cfg, running, 50*time.Second); due {
		t.Fatal("the plug was told as lost before a minute had passed")
	}
	cause, due := lostAfter(m, clock, cfg, running, 10*time.Second)
	if !due || cause != causePlugOffline {
		t.Fatalf("after a minute: due %v, cause %q", due, cause)
	}
	for range 20 {
		if _, due = lostAfter(m, clock, cfg, running, time.Minute); due {
			t.Fatal("the same print was told twice")
		}
	}

	// It came back and went again in the same print: still told once.
	setRelay(m, cfg, "ON", true)
	lostAfter(m, clock, cfg, running, 5*time.Second)
	setRelay(m, cfg, "ON", false)
	if _, due = lostAfter(m, clock, cfg, running, 5*time.Minute); due {
		t.Fatal("the same print was told twice after the plug came back")
	}

	// The next print is told again.
	next := running
	next.JobID = "job-2"
	lostAfter(m, clock, cfg, next, 5*time.Second)
	if _, due = lostAfter(m, clock, cfg, next, time.Minute); !due {
		t.Fatal("the next print was not told")
	}
}

// A plug that is away for a moment is not told.
func TestAPlugThatComesBackInTimeIsNotTold(t *testing.T) {
	m, clock, cfg := lostModule()
	for range 10 {
		setRelay(m, cfg, "ON", false)
		if _, due := lostAfter(m, clock, cfg, printerPrinting, 50*time.Second); due {
			t.Fatal("a plug away for under a minute was told")
		}
		setRelay(m, cfg, "ON", true)
		if _, due := lostAfter(m, clock, cfg, printerPrinting, 5*time.Second); due {
			t.Fatal("a plug that is back was told")
		}
	}
}

func TestPlugLostNeedsAPrint(t *testing.T) {
	m, clock, cfg := lostModule()
	setRelay(m, cfg, "ON", false)

	for _, safety := range []printerSafety{printerIdle, printerFinished, printerHeating} {
		lostAfter(m, clock, cfg, safety, 5*time.Second)
		if _, due := lostAfter(m, clock, cfg, safety, time.Hour); due {
			t.Fatalf("a plug of a printer that is %s was told as lost", safety.PrinterState)
		}
	}
	if len(m.lost) != 0 {
		t.Fatalf("a printer that does not print is followed: %+v", m.lost)
	}
}

func TestPlugLostHoldsWhileThePrinterIsAway(t *testing.T) {
	m, clock, cfg := lostModule()
	lostAfter(m, clock, cfg, printerPrinting, 5*time.Second)

	// A power cut takes the plug and the printer with it.
	setRelay(m, cfg, "ON", false)
	away := printerSafety{PrinterState: "PRINTING", CanTurnOff: true}
	if _, due := lostAfter(m, clock, cfg, away, 5*time.Second); due {
		t.Fatal("told at once")
	}
	if cause, due := lostAfter(m, clock, cfg, away, time.Minute); !due || cause != causePlugOffline {
		t.Fatalf("a plug lost with its printer was not told: due %v, cause %q", due, cause)
	}

	// A printer that was never seen printing is not told while it is away.
	m, clock, cfg = lostModule()
	setRelay(m, cfg, "ON", false)
	lostAfter(m, clock, cfg, away, 5*time.Second)
	if _, due := lostAfter(m, clock, cfg, away, time.Hour); due {
		t.Fatal("a printer that was never seen printing was told")
	}
}

func TestABrokerThatIsDownCountsForMQTTPlugsOnly(t *testing.T) {
	m, clock, cfg := lostModule()
	lostAfter(m, clock, cfg, printerPrinting, 5*time.Second)
	m.mu.Lock()
	m.mqttUp = false
	m.mu.Unlock()
	lostAfter(m, clock, cfg, printerPrinting, 5*time.Second)
	if cause, due := lostAfter(m, clock, cfg, printerPrinting, time.Minute); !due || cause != causeBrokerOffline {
		t.Fatalf("a broker that is down: due %v, cause %q", due, cause)
	}

	// A plug read by address has nothing to do with the broker.
	byAddress := seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: "plug-2.lan"}, "ON", 60)
	m.plugLostDue("p2", byAddress, printerPrinting, clock.now())
	if _, due := m.plugLostDue("p2", byAddress, printerPrinting, clock.now().Add(time.Hour)); due {
		t.Fatal("a plug read by address was told as lost with the broker")
	}

	// With no broker set up, a broker that is not there is no news.
	m, clock, cfg = lostModule()
	none := normalizeSettings(Settings{})
	m.held.Store(&none)
	m.mqttUp = false
	lostAfter(m, clock, cfg, printerPrinting, 5*time.Second)
	if _, due := lostAfter(m, clock, cfg, printerPrinting, time.Hour); due {
		t.Fatal("a broker that is not set up was told as lost")
	}
}

func TestEventKeyAndPayloadCarryNoAddress(t *testing.T) {
	if got := eventKey("printer-1"); got != "printer-1" {
		t.Errorf("eventKey = %q", got)
	}
	for _, bad := range []string{"two words", "line\nbreak", "tab\t", "del\x7f", strings.Repeat("x", busapi.MaxKeyBytes+1)} {
		if got := eventKey(bad); got != "" {
			t.Errorf("eventKey(%q) = %q, want none", bad, got)
		}
	}

	// A turn-off that fails with the plug's address and password in its
	// cause: neither is in the event.
	none := 0
	m, clock, cfg, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &none}, Settings{MQTTHost: "broker.lan", MQTTPassword: "brokerpw"})
	plug.set(true)
	m.httpClient.Transport = failingTransport{fmt.Errorf(`Get "http://%s/cm?user=admin&password=%s": dial tcp %s: connection refused`, cfg.HTTPHost, cfg.HTTPPassword, cfg.HTTPHost)}
	step(m, clock, printerPrinting, 5*time.Second)
	step(m, clock, printerFinished, 5*time.Second)

	// And a plug that is lost during a print.
	setRelay(m, cfg, "ON", false)
	step(m, clock, printerPrinting, 5*time.Second)
	step(m, clock, printerPrinting, 2*time.Minute)

	told := events.told()
	if len(told) != 2 || told[0].endpoint != endpointAutoOffFailed || told[1].endpoint != endpointPlugLost {
		t.Fatalf("events = %+v, want a failed turn-off and a lost plug", told)
	}
	if lost := told[1].lost(); lost.Driver != DriverTasmotaHTTP || lost.Cause != causePlugOffline || lost.DeviceID != "p1" {
		t.Fatalf("plug.lost = %+v", lost)
	}
	host, _, _ := strings.Cut(cfg.HTTPHost, ":")
	for _, event := range told {
		raw, err := json.Marshal(event.data)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > busapi.MaxDataBytes {
			t.Errorf("%s is %d bytes", event.endpoint, len(raw))
		}
		for _, secret := range []string{cfg.HTTPHost, host, cfg.HTTPPassword, "brokerpw", "broker.lan", "password=", "http://", cfg.plugKey()} {
			if strings.Contains(string(raw), secret) || strings.Contains(event.key, secret) {
				t.Errorf("%s names %q: %s (key %q)", event.endpoint, secret, raw, event.key)
			}
		}
		if event.key != "p1" {
			t.Errorf("%s has the key %q, want the printer's id", event.endpoint, event.key)
		}
	}
	if failed := told[0].auto().Error; !strings.Contains(failed, "connection refused") {
		t.Errorf("the cause was lost with the address: %q", failed)
	}
}

func TestIdleOffTellsTheSameEvents(t *testing.T) {
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{}, Settings{IdleOffMin: 10})
	plug.set(true)

	step(m, clock, printerIdle, 0)
	for range 4 {
		step(m, clock, printerIdle, 10*time.Minute)
	}
	told := events.told()
	if len(told) != 1 || told[0].endpoint != endpointAutoOffFailed {
		t.Fatalf("four failed tries told %+v, want one auto_off_failed", told)
	}
	if failed := told[0].auto(); failed.Reason != reasonIdle || failed.JobID != "" || failed.Error == "" || failed.DeviceID != "p1" {
		t.Fatalf("payload = %+v", failed)
	}

	plug.set(false)
	step(m, clock, printerIdle, 5*time.Second)
	told = events.told()
	if len(told) != 2 || told[1].endpoint != endpointAutoOffDone || told[1].auto().Reason != reasonIdle {
		t.Fatalf("after the turn-off worked: %+v", told)
	}
}

// module.yaml names the endpoints for the operator to approve. An endpoint
// the code names and the file does not would never be told.
func TestModuleYAMLDeclaresTheEndpointsTheCodeNames(t *testing.T) {
	raw, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{}
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "events:" {
			inside = true
			continue
		}
		if !inside || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			if strings.TrimSpace(line) != "" {
				inside = false
			}
			continue
		}
		endpoint, tail, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			t.Fatalf("the events table has the line %q", line)
		}
		declared[endpoint] = strings.TrimSpace(tail)
	}

	named := []string{endpointAutoOffDone, endpointAutoOffFailed, endpointPlugLost}
	if len(declared) != len(named) {
		t.Fatalf("module.yaml declares %v, the code names %v", declared, named)
	}
	tail := regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
	for _, endpoint := range named {
		got, ok := declared[endpoint]
		if !ok {
			t.Errorf("module.yaml does not declare %s", endpoint)
			continue
		}
		if !tail.MatchString(got) {
			t.Errorf("%s has the type tail %q", endpoint, got)
		}
		if len(endpoint) > busapi.MaxEndpointBytes {
			t.Errorf("%s is longer than the host takes", endpoint)
		}
	}
	// Events need no capability and no permission of their own.
	if strings.Contains(string(raw), "core-bus") {
		t.Error("module.yaml requires a bus capability; a core without it would refuse the start")
	}
}

// failingTransport fails every request with err, as a network that is down.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

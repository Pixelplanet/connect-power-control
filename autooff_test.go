package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The moments of a printer the auto-off logic tells apart.
var (
	printerPrinting = printerSafety{Connected: true, PrinterState: "PRINTING", ActiveJob: true}
	printerFinished = printerSafety{Connected: true, PrinterState: "FINISHED", CanTurnOff: true}
	printerIdle     = printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true}
	printerHeating  = printerSafety{Connected: true, PrinterState: "BUSY"}
	printerAway     = printerSafety{PrinterState: "PRINTING", CanTurnOff: true}
)

func TestAutoOffWaitsForThePrintAndTheCooldown(t *testing.T) {
	m := newModule(nil)
	start := time.Now()
	wait := 2 * time.Minute

	if m.autoOffDue("p", printerIdle, wait, start) {
		t.Fatal("an idle printer that printed nothing was due")
	}
	if m.autoOffDue("p", printerPrinting, wait, start) {
		t.Fatal("a printing printer was due")
	}
	if m.autoOffDue("p", printerAway, wait, start.Add(time.Hour)) {
		t.Fatal("a printer that lost its connection mid-print was due")
	}
	if m.autoOffDue("p", printerFinished, wait, start.Add(time.Hour)) {
		t.Fatal("the plug was due before the wait began")
	}
	if m.autoOffDue("p", printerFinished, wait, start.Add(time.Hour+time.Minute)) {
		t.Fatal("the plug was due before the wait was over")
	}
	if !m.autoOffDue("p", printerFinished, wait, start.Add(time.Hour+wait)) {
		t.Fatal("the plug was not due after the print and the wait")
	}
}

func TestAPrinterThatTurnsOffByDefaultStaysArmed(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	cfg := DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", AutoOffAfterPrint: true, AutoOffDefault: true}

	m.autoOffDue("p", printerPrinting, 0, now)
	if !m.autoOffDue("p", printerFinished, 0, now) {
		t.Fatal("the plug was not due after the first print")
	}
	cfg = m.afterAutoOff("p", cfg)
	if !cfg.AutoOffAfterPrint {
		t.Fatal("a printer that turns off by default was disarmed after one print")
	}

	// Turned on again by hand: it still reports the old print as finished,
	// and it is heated and moved. None of that is a print.
	for _, moment := range []printerSafety{printerFinished, printerIdle, printerHeating, printerIdle} {
		if m.autoOffDue("p", moment, 0, now.Add(time.Hour)) {
			t.Fatalf("the plug was due again with no new print, in state %s", moment.PrinterState)
		}
	}

	m.autoOffDue("p", printerPrinting, 0, now.Add(2*time.Hour))
	if !m.autoOffDue("p", printerFinished, 0, now.Add(3*time.Hour)) {
		t.Fatal("the plug was not due after the second print")
	}
}

func TestAOneTimeChoiceGoesBackToTheDefault(t *testing.T) {
	m := newModule(nil)

	once := m.afterAutoOff("p", DeviceConfig{AutoOffAfterPrint: true})
	if once.AutoOffAfterPrint {
		t.Fatal("a choice for one print stayed on for the next")
	}

	// Switched off for one print on a printer that turns off by default.
	steps := []struct {
		moment printerSafety
		over   bool
	}{
		{printerIdle, false},
		{printerPrinting, false},
		{printerFinished, false},
		{printerHeating, false},
		{printerAway, false},
		{printerIdle, false},
		{printerPrinting, true},
	}
	for i, step := range steps {
		if got := m.skippedPrintIsOver("p", step.moment); got != step.over {
			t.Fatalf("step %d (%s): over = %v, want %v", i, step.moment.PrinterState, got, step.over)
		}
	}
}

// Saving a printer's plug must not lose what it does after every print.
func TestSavingAPlugKeepsTheAutoOffDefault(t *testing.T) {
	m := newModule(nil)
	cfg := DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", AutoOffAfterPrint: true, AutoOffDefault: true}
	if err := m.saveConfig(t.Context(), "p", cfg); err != nil {
		t.Fatal(err)
	}

	got, err := m.getConfig(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoOffDefault || !got.AutoOffAfterPrint {
		t.Fatalf("the default was lost on save: %+v", got)
	}
}

// A plug cut by hand ends the print that was followed. The printer turned on
// again later must not be turned off for a print that is long over.
func TestAPlugTurnedOffByHandEndsThePrintThatWasFollowed(t *testing.T) {
	now := time.Now()
	wait := 2 * time.Minute

	for name, before := range map[string][]printerSafety{
		"during the wait": {printerPrinting, printerFinished},
		"mid-print":       {printerPrinting},
	} {
		m := newModule(nil)
		for _, moment := range before {
			m.autoOffDue("p", moment, wait, now)
		}
		m.forgetPrint("p")

		for _, later := range []time.Duration{24 * time.Hour, 24*time.Hour + wait} {
			if m.autoOffDue("p", printerIdle, wait, now.Add(later)) {
				t.Fatalf("%s: the printer was turned off again %s later with no new print", name, later)
			}
		}
	}

	m := newModule(nil)
	cfg := DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug_a"}
	if m.plugIsOff(cfg) {
		t.Fatal("a plug nothing was heard from counted as off")
	}
	plug := &PlugTelemetry{Topic: "plug_a", Power: "OFF"}
	plug.setRelayState(1, "OFF")
	m.plugs[cfg.plugKey()] = plug
	if !m.plugIsOff(cfg) {
		t.Fatal("a plug that reported OFF did not count as off")
	}
}

// A print seen to be over stays over when the printer then goes away.
func TestTheWaitRunsOnWhenThePrinterGoesAwayAfterThePrint(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	wait := 2 * time.Minute
	away := printerSafety{CanTurnOff: true}

	m.autoOffDue("p", printerPrinting, wait, now)
	m.autoOffDue("p", printerFinished, wait, now)
	if m.autoOffDue("p", away, wait, now.Add(time.Minute)) {
		t.Fatal("the plug was due before the wait was over")
	}
	if !m.autoOffDue("p", away, wait, now.Add(wait)) {
		t.Fatal("a printer that went away after its print stayed on")
	}
}

// A printer turns off only after a print that finished. A cancelled or a
// failed print leaves it on and leaves the choice armed for the next print.
func TestOnlyAFinishedPrintTurnsThePrinterOff(t *testing.T) {
	now := time.Now()
	wait := 2 * time.Minute
	long := now.Add(24 * time.Hour)

	for name, tc := range map[string]struct {
		end  printerSafety
		kept string
	}{
		"cancelled, by the printer's state": {printerSafety{Connected: true, PrinterState: "STOPPED", CanTurnOff: true}, "cancelled"},
		"failed, by the printer's state":    {printerSafety{Connected: true, PrinterState: "ERROR", CanTurnOff: true}, "failed"},
		"cancelled, by the server's record": {printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "cancelled"}, "cancelled"},
		"failed, by the server's record":    {printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "failed"}, "failed"},
	} {
		m := newModule(nil)
		running := printerPrinting
		running.JobID = "job-1"
		m.autoOffDue("p", running, wait, now)
		if m.autoOffDue("p", tc.end, wait, now) || m.autoOffDue("p", tc.end, wait, long) {
			t.Fatalf("%s: the printer was turned off", name)
		}
		if got := m.trackers["p"].kept; got != tc.kept {
			t.Fatalf("%s: kept = %q, want %q", name, got, tc.kept)
		}

		// The retry is a new print, and it finishes.
		m.autoOffDue("p", running, wait, long)
		if m.trackers["p"].kept != "" {
			t.Fatalf("%s: the reason outlived the next print", name)
		}
		m.autoOffDue("p", printerFinished, wait, long)
		if !m.autoOffDue("p", printerFinished, wait, long.Add(wait)) {
			t.Fatalf("%s: the retry that finished did not turn the printer off", name)
		}
	}
}

func TestAPausedPrintNeverTurnsThePrinterOff(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	paused := printerSafety{Connected: true, PrinterState: "PAUSED", ActiveJob: true}

	m.autoOffDue("p", printerPrinting, 0, now)
	for _, later := range []time.Duration{time.Minute, time.Hour, 72 * time.Hour} {
		if m.autoOffDue("p", paused, 0, now.Add(later)) {
			t.Fatalf("a print paused for %s turned the printer off", later)
		}
	}
}

// The record of another print says nothing about this one, and a print with
// no outcome yet is given a moment before it counts as finished.
func TestAPrintWithNoOutcomeYetIsGivenAMoment(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	running := printerPrinting
	running.JobID = "job-2"
	stale := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "finished"}

	m.autoOffDue("p", running, 0, now)
	if m.autoOffDue("p", stale, 0, now) {
		t.Fatal("the printer was turned off on the record of an older print")
	}
	recorded := stale
	recorded.LastJobID, recorded.LastJobOutcome = "job-2", "cancelled"
	if m.autoOffDue("p", recorded, 0, now.Add(5*time.Second)) || m.autoOffDue("p", recorded, 0, now.Add(time.Hour)) {
		t.Fatal("a print recorded as cancelled a moment later turned the printer off")
	}

	n := newModule(nil)
	n.autoOffDue("p", running, 0, now)
	n.autoOffDue("p", stale, 0, now)
	if !n.autoOffDue("p", stale, 0, now.Add(outcomeGrace)) {
		t.Fatal("a print nothing was recorded about never turned the printer off")
	}
}

// A print skipped on a printer that turns off by default, and then cancelled,
// is not used up: the print started in its place is skipped too.
func TestASkippedPrintThatWasCancelledIsSkippedAgain(t *testing.T) {
	m := newModule(nil)
	stopped := printerSafety{Connected: true, PrinterState: "STOPPED", CanTurnOff: true}

	for i, moment := range []printerSafety{printerPrinting, stopped, printerPrinting, printerFinished} {
		if m.skippedPrintIsOver("p", moment) {
			t.Fatalf("step %d (%s): the default came back before a print finished", i, moment.PrinterState)
		}
	}
	if !m.skippedPrintIsOver("p", printerPrinting) {
		t.Fatal("the default did not come back for the print after the one that finished")
	}
}

func TestAPlugThatWillNotTurnOffIsReported(t *testing.T) {
	m := newModule(nil)
	now := time.Now()
	m.autoOffDue("p", printerPrinting, 0, now)
	m.autoOffDue("p", printerFinished, 0, now)
	m.noteAutoOffFault("p", errBrokerOffline)

	if got := m.trackers["p"].offFault; got != errBrokerOffline.Error() {
		t.Fatalf("the fault was not kept: %q", got)
	}
	if !m.autoOffDue("p", printerFinished, 0, now.Add(time.Minute)) {
		t.Fatal("the turn-off was not tried again")
	}
}

// --- Over a restart ---

// restartedModule is a module that starts on the data directory dir, with
// the printer p set up as cfg.
func restartedModule(dir string, cfg DeviceConfig) *module {
	m := newModule(nil)
	m.configs["p"] = cfg
	// The printer was sampled just before the stop.
	m.usage["p"] = &DeviceUsage{LastSampleAt: time.Now()}
	m.setDataDir(dir)
	m.loadAutoOff()
	return m
}

var (
	armedPlug   = DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", AutoOffAfterPrint: true}
	skippedPlug = DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", AutoOffDefault: true}
)

func autoOffFile(dir string) string {
	return filepath.Join(dir, autoOffFileName)
}

func TestAnArmedAutoOffSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	wait := 2 * time.Minute
	running := printerPrinting
	running.JobID = "job-1"
	// The print ended while nothing ran. The printer is idle again, and only
	// the server's record names the print.
	over := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "finished"}

	fresh := restartedModule(dir, armedPlug)
	if fresh.autoOffDue("p", over, wait, now) || fresh.autoOffDue("p", over, wait, now.Add(time.Hour)) {
		t.Fatal("a printer whose print was never seen was due")
	}

	m := restartedModule(t.TempDir(), armedPlug)
	dir = m.dataDir
	m.autoOffDue("p", running, wait, now)
	m.saveAutoOffIfChanged()

	restarted := restartedModule(dir, armedPlug)
	if restarted.autoOffDue("p", over, wait, now.Add(time.Minute)) {
		t.Fatal("the plug was due before the wait began")
	}
	if !restarted.autoOffDue("p", over, wait, now.Add(time.Minute+wait)) {
		t.Fatal("a print that ended during the restart did not turn the printer off")
	}
}

func TestTheWaitRunsOnAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	wait := 10 * time.Minute

	m := restartedModule(dir, armedPlug)
	m.autoOffDue("p", printerPrinting, wait, now)
	m.autoOffDue("p", printerFinished, wait, now) // the wait begins
	m.saveAutoOffIfChanged()

	restarted := restartedModule(dir, armedPlug)
	if restarted.autoOffDue("p", printerFinished, wait, now.Add(wait-time.Second)) {
		t.Fatal("the plug was due before the wait was over")
	}
	// Were the wait started again after the restart, it would end later.
	if !restarted.autoOffDue("p", printerFinished, wait, now.Add(wait)) {
		t.Fatal("the wait began again after the restart")
	}

	// A printer that is still away after the restart is left alone until it
	// was seen once. The wait ran on in that time.
	away := restartedModule(dir, armedPlug)
	if away.autoOffDue("p", printerSafety{CanTurnOff: true}, wait, now.Add(wait)) {
		t.Fatal("a printer nobody has seen since the restart was due")
	}
	if !away.autoOffDue("p", printerFinished, wait, now.Add(wait+5*time.Second)) {
		t.Fatal("the wait did not run on for a printer that was away")
	}
}

func TestARestoredPrintThePrinterHasLeftBehindIsDropped(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	running := printerPrinting
	running.JobID = "job-1"

	m := restartedModule(dir, armedPlug)
	m.autoOffDue("p", running, 0, now)
	m.saveAutoOffIfChanged()

	// Two prints later. Nothing names job-1 any more.
	later := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-3", LastJobOutcome: "finished"}
	restarted := restartedModule(dir, armedPlug)
	if restarted.autoOffDue("p", later, 0, now.Add(time.Hour)) || restarted.autoOffDue("p", later, 0, now.Add(2*time.Hour)) {
		t.Fatal("the printer was turned off for a print it left behind long ago")
	}
	if tr := restarted.trackers["p"]; tr.sawActive || !tr.fired || tr.jobID != "" {
		t.Fatalf("the old print is still followed: %+v", tr)
	}

	// The print that was followed is still the one running: it stays.
	still := restartedModule(dir, armedPlug)
	still.autoOffDue("p", running, 0, now.Add(time.Hour))
	done := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "finished"}
	if !still.autoOffDue("p", done, 0, now.Add(2*time.Hour)) {
		t.Fatal("the print that ran through the restart did not turn the printer off")
	}
}

func TestASkippedPrintSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	m := restartedModule(dir, skippedPlug)
	for i, moment := range []printerSafety{printerPrinting, printerFinished} {
		if m.skippedPrintIsOver("p", moment) {
			t.Fatalf("step %d: the default came back too early", i)
		}
	}
	m.saveAutoOffIfChanged()

	restarted := restartedModule(dir, skippedPlug)
	if !restarted.skippedPrintIsOver("p", printerPrinting) {
		t.Fatal("after the restart the skipped print was forgotten, so the next print was skipped too")
	}

	// A printer that is armed again in the meantime has no skipped print.
	if armed := restartedModule(dir, armedPlug); len(armed.skips) != 0 {
		t.Fatalf("a skipped print was restored for an armed printer: %+v", armed.skips)
	}
}

func TestADamagedAutoOffFileIsIgnored(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":        `{"v":1,"printers":{"p":{"tracker":`,
		"a newer version": `{"v":2,"printers":{"p":{"tracker":{"saw_active":true,"fired":false}}}}`,
		"another shape":   `{"v":1,"printers":[1,2,3]}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(autoOffFile(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		m := restartedModule(dir, armedPlug)
		if len(m.trackers) != 0 || len(m.skips) != 0 {
			t.Fatalf("%s: something was restored: %+v", name, m.trackers)
		}

		// It goes on from nothing, and the next change writes a good file.
		m.autoOffDue("p", printerPrinting, 0, time.Now())
		m.saveAutoOffIfChanged()
		if again := restartedModule(dir, armedPlug); again.trackers["p"] == nil || !again.trackers["p"].sawActive {
			t.Fatalf("%s: the file was not written anew: %+v", name, again.trackers)
		}
	}
}

func TestTheAutoOffFileIsWrittenOnlyOnAChange(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	m := restartedModule(dir, armedPlug)

	m.autoOffDue("p", printerIdle, 0, now)
	m.saveAutoOffIfChanged()
	if exists(autoOffFile(dir)) {
		t.Fatal("a file was written with nothing to keep")
	}

	m.autoOffDue("p", printerPrinting, 0, now)
	m.saveAutoOffIfChanged()
	if !exists(autoOffFile(dir)) {
		t.Fatal("a print that is followed was not written")
	}

	// Removed behind its back: with no change it is not written again.
	if err := os.Remove(autoOffFile(dir)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		m.autoOffDue("p", printerPrinting, 0, now.Add(time.Minute))
		m.saveAutoOffIfChanged()
	}
	if exists(autoOffFile(dir)) {
		t.Fatal("the file was written again though nothing changed")
	}

	m.autoOffDue("p", printerFinished, time.Hour, now.Add(2*time.Minute))
	m.saveAutoOffIfChanged()
	raw, err := os.ReadFile(autoOffFile(dir))
	if err != nil {
		t.Fatalf("the wait that began was not written: %v", err)
	}
	var saved savedAutoOff
	if err = json.Unmarshal(raw, &saved); err != nil || saved.V != 1 || saved.Printers["p"] == nil || saved.Printers["p"].Tracker == nil || saved.Printers["p"].Tracker.FinishedAt == nil {
		t.Fatalf("autooff.json = %s (%v)", raw, err)
	}

	// Why a turn-off failed is not kept.
	m.noteAutoOffFault("p", errBrokerOffline)
	m.saveAutoOffIfChanged()
	if raw, _ = os.ReadFile(autoOffFile(dir)); strings.Contains(string(raw), "broker") {
		t.Fatalf("the fault was written: %s", raw)
	}
}

func TestATrackerOfAPrinterWithAutoOffSwitchedOffIsNotRestored(t *testing.T) {
	dir := t.TempDir()
	m := restartedModule(dir, armedPlug)
	m.autoOffDue("p", printerPrinting, 0, time.Now())
	m.saveAutoOffIfChanged()

	for name, cfg := range map[string]DeviceConfig{
		"switched off":         {Driver: DriverTasmota, TasmotaTopic: "plug"},
		"switched off for one": skippedPlug,
	} {
		if restarted := restartedModule(dir, cfg); len(restarted.trackers) != 0 {
			t.Fatalf("%s: a print is followed for a printer that is not armed: %+v", name, restarted.trackers["p"])
		}
	}

	// A printer that is no longer set up is not restored either.
	gone := newModule(nil)
	gone.setDataDir(dir)
	gone.loadAutoOff()
	if len(gone.trackers) != 0 {
		t.Fatalf("a print is followed for a printer with no plug: %+v", gone.trackers)
	}
}

// --- A printer that is on and idle ---

// idleModule is a module whose clock a test moves, with the printer p1 on a
// plug that is on, and minutes as the idle time for all printers.
func idleModule(minutes int) (*module, *testClock, DeviceConfig) {
	m, clock := clockedModule(energyStart)
	s := normalizeSettings(Settings{IdleOffMin: minutes})
	m.held.Store(&s)
	cfg := seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, HTTPHost: "plug-1.lan"}, "ON", 5)
	m.configs["p1"] = cfg
	return m, clock, cfg
}

// idleAfter moves the clock and looks at p1 once, as the poll loop does.
func idleAfter(m *module, clock *testClock, cfg DeviceConfig, safety printerSafety, step time.Duration) bool {
	clock.add(step)
	return m.idleTick("p1", cfg, safety, idleLimit(cfg, m.settings()), clock.now())
}

// idleFor ticks every five seconds for d and reports whether p1 was due.
func idleFor(m *module, clock *testClock, cfg DeviceConfig, safety printerSafety, d time.Duration) (due bool) {
	for passed := time.Duration(0); passed < d; passed += 5 * time.Second {
		due = idleAfter(m, clock, cfg, safety, 5*time.Second) || due
	}
	return due
}

// idleFromNow follows p1 from this moment, free and idle. It reports whether
// p1 was due before ten minutes had passed, and whether it was due then.
func idleFromNow(m *module, clock *testClock, cfg DeviceConfig) (early, onTime bool) {
	early = idleAfter(m, clock, cfg, printerIdle, 0)
	early = idleFor(m, clock, cfg, printerIdle, 10*time.Minute-5*time.Second) || early
	return early, idleAfter(m, clock, cfg, printerIdle, 5*time.Second)
}

func setRelay(m *module, cfg DeviceConfig, state string, online bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plug := m.plugs[cfg.plugKey()]
	plug.setRelayState(cfg.relayIndex(), state)
	plug.Online = &online
}

func TestAnIdlePrinterIsTurnedOffAfterTheWait(t *testing.T) {
	m, clock, cfg := idleModule(10)

	if idleAfter(m, clock, cfg, printerIdle, 0) {
		t.Fatal("a printer was due on the first look")
	}
	if idleFor(m, clock, cfg, printerIdle, 10*time.Minute-5*time.Second) {
		t.Fatal("a printer was due before its idle time was over")
	}
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("a printer was not due after its idle time")
	}
	// It stays due until the plug is off: a turn-off that failed is tried again.
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("the turn-off would not be tried again")
	}
}

func TestTheIdleClockStartsWhenThePlugIsSeenOn(t *testing.T) {
	m, clock, cfg := idleModule(10)
	setRelay(m, cfg, "OFF", true)

	if idleFor(m, clock, cfg, printerIdle, time.Hour) {
		t.Fatal("a plug that is off was due")
	}
	if m.idle["p1"] == nil || !m.idle["p1"].since.IsZero() {
		t.Fatalf("the idle time ran for a plug that is off: %+v", m.idle["p1"])
	}

	setRelay(m, cfg, "ON", true)
	if early, onTime := idleFromNow(m, clock, cfg); early || !onTime {
		t.Fatalf("after the plug was turned on: due early %v, due after ten minutes %v", early, onTime)
	}
}

func TestPrintingPausedBusyAndMovingRestartTheIdleClock(t *testing.T) {
	for name, busy := range map[string]printerSafety{
		"printing":  printerPrinting,
		"paused":    {Connected: true, PrinterState: "PAUSED", ActiveJob: true},
		"busy":      printerHeating,
		"attention": {Connected: true, PrinterState: "ATTENTION"},
		"moving":    {Connected: true, PrinterState: "IDLE", Moving: true},
		"open job":  {Connected: true, PrinterState: "IDLE", ActiveJob: true},
	} {
		t.Run(name, func(t *testing.T) {
			m, clock, cfg := idleModule(10)
			idleFor(m, clock, cfg, printerIdle, 9*time.Minute)

			if idleFor(m, clock, cfg, busy, time.Hour) {
				t.Fatal("a printer that may not be turned off was due")
			}
			if early, onTime := idleFromNow(m, clock, cfg); early || !onTime {
				t.Fatalf("after the printer was free again: due early %v, due after ten minutes %v", early, onTime)
			}
		})
	}
}

func TestAPrinterThatIsAwayIsNeverTurnedOffForIdling(t *testing.T) {
	away := printerSafety{PrinterState: "IDLE", CanTurnOff: true}

	m, clock, cfg := idleModule(10)
	idleFor(m, clock, cfg, printerIdle, 9*time.Minute)
	if idleFor(m, clock, cfg, away, 6*time.Hour) {
		t.Fatal("a printer Connect cannot reach was due")
	}
	if early, onTime := idleFromNow(m, clock, cfg); early || !onTime {
		t.Fatalf("after a long absence: due early %v, due after ten minutes %v", early, onTime)
	}

	// A drop of a moment does not start the time again.
	m, clock, cfg = idleModule(10)
	idleFor(m, clock, cfg, printerIdle, 9*time.Minute)
	if idleFor(m, clock, cfg, away, time.Minute) {
		t.Fatal("a printer that is away was due")
	}
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("a short absence started the idle time again")
	}
}

func TestAStalePlugIsNotDue(t *testing.T) {
	m, clock, cfg := idleModule(10)
	idleFor(m, clock, cfg, printerIdle, 5*time.Minute)

	// The plug stopped answering. Its last word was "on".
	setRelay(m, cfg, "ON", false)
	if idleFor(m, clock, cfg, printerIdle, time.Hour) {
		t.Fatal("a plug that does not answer was due")
	}
	setRelay(m, cfg, "ON", true)
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("the plug was not due once it answered again")
	}

	// A plug nothing is known of is never due.
	m, clock, cfg = idleModule(10)
	setRelay(m, cfg, "UNKNOWN", true)
	if idleFor(m, clock, cfg, printerIdle, time.Hour) {
		t.Fatal("a plug in an unknown state was due")
	}
}

func TestIdleOffIsOffByDefault(t *testing.T) {
	if got := defaultSettings().IdleOffMin; got != 0 {
		t.Fatalf("the default idle time is %d minutes, want none", got)
	}
	m, clock, cfg := idleModule(0)
	def := normalizeSettings(defaultSettings())
	m.held.Store(&def)

	if idleFor(m, clock, cfg, printerIdle, 3*time.Hour) || idleAfter(m, clock, cfg, printerIdle, 7*24*time.Hour) {
		t.Fatal("a printer was due though no idle time is set")
	}
	if len(m.idle) != 0 {
		t.Fatalf("an idle time is followed though none is set: %+v", m.idle)
	}
}

func TestAPrintersOwnIdleTimeWinsAndZeroMeansNever(t *testing.T) {
	minutes := func(v int) *int { return &v }
	all := Settings{IdleOffMin: 60}

	for _, tc := range []struct {
		name string
		cfg  DeviceConfig
		s    Settings
		want time.Duration
	}{
		{"the setting for all", DeviceConfig{}, all, time.Hour},
		{"its own", DeviceConfig{IdleOffMin: minutes(30)}, all, 30 * time.Minute},
		{"its own with none for all", DeviceConfig{IdleOffMin: minutes(30)}, Settings{}, 30 * time.Minute},
		{"never for this one", DeviceConfig{IdleOffMin: minutes(0)}, all, 0},
		{"none at all", DeviceConfig{}, Settings{}, 0},
	} {
		if got := idleLimit(tc.cfg, tc.s); got != tc.want {
			t.Errorf("%s: idleLimit = %v, want %v", tc.name, got, tc.want)
		}
	}

	m, clock, cfg := idleModule(10)
	cfg.IdleOffMin = minutes(0)
	if idleFor(m, clock, cfg, printerIdle, 3*time.Hour) {
		t.Fatal("a printer set to never was due")
	}
}

func TestKeepOnStartsTheWaitAgain(t *testing.T) {
	m, clock, cfg := idleModule(10)
	if m.keepOn("p1", clock.now()) {
		t.Fatal("an idle time that does not run was started again")
	}
	idleFor(m, clock, cfg, printerIdle, 9*time.Minute)

	if !m.keepOn("p1", clock.now()) {
		t.Fatal("keep on found no idle time")
	}
	if idleFor(m, clock, cfg, printerIdle, 10*time.Minute-5*time.Second) {
		t.Fatal("the printer was due before the whole time had passed again")
	}
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("the printer was not due a whole idle time after keep on")
	}
}

// restartedIdle is a module that starts on dir at the time at, with the
// printer p1 on a plug that is on. sampled is when p1 was last sampled before
// the stop, zero for never.
func restartedIdle(dir string, at, sampled time.Time) (*module, *testClock, DeviceConfig) {
	m, clock, cfg := idleModule(10)
	clock.set(at)
	if !sampled.IsZero() {
		m.usage["p1"] = &DeviceUsage{LastSampleAt: sampled}
	}
	m.setDataDir(dir)
	m.loadAutoOff()
	return m, clock, cfg
}

func TestTheIdleClockSurvivesAShortRestart(t *testing.T) {
	dir := t.TempDir()
	m, clock, cfg := restartedIdle(dir, energyStart, time.Time{})
	idleFor(m, clock, cfg, printerIdle, 8*time.Minute)
	m.saveAutoOffIfChanged()
	stopped := clock.now()

	raw, err := os.ReadFile(autoOffFile(dir))
	if err != nil || !strings.Contains(string(raw), `"idle":{"since":"`) {
		t.Fatalf("the idle time was not written: %s (%v)", raw, err)
	}

	again, clock, cfg := restartedIdle(dir, stopped.Add(time.Minute), stopped)
	if idleAfter(again, clock, cfg, printerIdle, 0) {
		t.Fatal("the printer was due nine minutes in")
	}
	if !idleAfter(again, clock, cfg, printerIdle, 2*time.Minute) {
		t.Fatal("the idle time did not run on across a short restart")
	}
}

func TestTheIdleClockStartsAgainAfterALongStop(t *testing.T) {
	dir := t.TempDir()
	m, clock, cfg := restartedIdle(dir, energyStart, time.Time{})
	idleFor(m, clock, cfg, printerIdle, 8*time.Minute)
	m.saveAutoOffIfChanged()
	stopped := clock.now()

	for name, sampled := range map[string]time.Time{"stopped for hours": stopped, "never sampled": {}} {
		again, clock, cfg := restartedIdle(dir, stopped.Add(3*time.Hour), sampled)
		if early, onTime := idleFromNow(again, clock, cfg); early || !onTime {
			t.Fatalf("%s: due early %v, due ten minutes after the start %v", name, early, onTime)
		}
	}
}

func TestAClockJumpDoesNotTurnAPrinterOff(t *testing.T) {
	m, clock, cfg := idleModule(10)
	idleFor(m, clock, cfg, printerIdle, time.Minute)

	if idleAfter(m, clock, cfg, printerIdle, 24*time.Hour) {
		t.Fatal("a jump of the clock turned a printer off")
	}
	if idleFor(m, clock, cfg, printerIdle, 10*time.Minute-5*time.Second) {
		t.Fatal("the printer was due before ten minutes after the jump")
	}
	if !idleAfter(m, clock, cfg, printerIdle, 5*time.Second) {
		t.Fatal("the printer was not due ten minutes after the jump")
	}

	// Back in time: the idle time does not get longer than it is.
	clock.add(-48 * time.Hour)
	if early, onTime := idleFromNow(m, clock, cfg); early || !onTime {
		t.Fatalf("after the clock went back: due early %v, due after ten minutes %v", early, onTime)
	}
}

func TestTurningTheIdleRuleOnStartsTheClockNow(t *testing.T) {
	m, clock, cfg := idleModule(0)
	idleFor(m, clock, cfg, printerIdle, 5*time.Hour)

	on := normalizeSettings(Settings{IdleOffMin: 10})
	m.held.Store(&on)
	if early, onTime := idleFromNow(m, clock, cfg); early || !onTime {
		t.Fatalf("after the rule was set: due early %v, due after ten minutes %v", early, onTime)
	}
}

func TestAnIdleValueIsClamped(t *testing.T) {
	for in, want := range map[int]int{-5: 0, 0: 0, 1: 10, 9: 10, 10: 10, 90: 90, 10080: 10080, 20000: 10080} {
		if got := normalizeSettings(Settings{IdleOffMin: in}).IdleOffMin; got != want {
			t.Errorf("the setting %d became %d, want %d", in, got, want)
		}
		own := in
		cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", IdleOffMin: &own})
		if cfg.IdleOffMin == nil || *cfg.IdleOffMin != want {
			t.Errorf("a printer's own %d became %v, want %d", in, cfg.IdleOffMin, want)
		}
	}
	if cfg := normalizeDeviceConfig(DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug"}); cfg.IdleOffMin != nil {
		t.Errorf("a printer with no value of its own got %d", *cfg.IdleOffMin)
	}
}

// --- The turn-off itself, against a plug that answers by address ---

// switchedPlug is a Tasmota plug on HTTP that is on, and that can be made to
// refuse. It answers how often it was told to turn off.
type switchedPlug struct {
	mu     sync.Mutex
	on     bool
	broken bool
	offs   int
}

func (p *switchedPlug) set(broken bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken = broken
}

func (p *switchedPlug) turnedOff() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offs
}

// steppedModule is a module with the printer p1 on a plug that can really be
// switched, and with every event it tells kept in the emitter it answers.
func steppedModule(t *testing.T, cfg DeviceConfig, s Settings) (*module, *testClock, DeviceConfig, *switchedPlug, *emitFake) {
	t.Helper()
	plug := &switchedPlug{on: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plug.mu.Lock()
		defer plug.mu.Unlock()
		if plug.broken {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		state := map[bool]string{true: "ON", false: "OFF"}
		switch r.URL.Query().Get("cmnd") {
		case "POWER1 0":
			plug.on = false
			plug.offs++
			_, _ = fmt.Fprintf(w, `{"POWER":"%s"}`, state[plug.on])
		default:
			_, _ = fmt.Fprintf(w, `{"StatusSTS":{"POWER":"%s"}}`, state[plug.on])
		}
	}))
	t.Cleanup(srv.Close)

	m, clock := clockedModule(energyStart)
	s = normalizeSettings(s)
	m.held.Store(&s)
	cfg.Driver, cfg.HTTPHost, cfg.HTTPPassword = DriverTasmotaHTTP, srv.URL, "plugpw"
	cfg = seedPlug(m, cfg, "ON", 5)
	m.configs["p1"] = cfg
	events := &emitFake{}
	m.events = events
	return m, clock, cfg, plug, events
}

// step moves the clock and runs one tick for p1, as the poll loop does.
func step(m *module, clock *testClock, safety printerSafety, d time.Duration) {
	clock.add(d)
	m.mu.RLock()
	cfg := m.configs["p1"]
	m.mu.RUnlock()
	safety.DeviceName = "Mk4 in the hall"
	m.stepPrinter(context.Background(), "p1", cfg, safety, func() (printerSafety, error) { return safety, nil })
}

func TestAutoOffAfterAPrintComesBeforeIdleOff(t *testing.T) {
	none := 0
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &none}, Settings{IdleOffMin: 10})
	running := printerPrinting
	running.JobID = "job-1"

	step(m, clock, running, 5*time.Second)
	step(m, clock, printerFinished, time.Hour)

	if plug.turnedOff() != 1 {
		t.Fatalf("the plug was told to turn off %d times, want once", plug.turnedOff())
	}
	told := events.told()
	if len(told) != 1 || told[0].endpoint != endpointAutoOffDone || told[0].auto().Reason != reasonPrintFinished || told[0].auto().JobID != "job-1" {
		t.Fatalf("events = %+v, want one auto_off_done for the print", told)
	}
	// The plug is off now, so the idle rule has nothing to do either.
	step(m, clock, printerIdle, time.Hour)
	if plug.turnedOff() != 1 || len(events.told()) != 1 {
		t.Fatalf("the idle rule acted on a plug that is off: %d turn-offs, %+v", plug.turnedOff(), events.told())
	}
}

func TestACancelledPrintIsTurnedOffByTheIdleRule(t *testing.T) {
	none := 0
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &none}, Settings{IdleOffMin: 10})
	cancelled := printerSafety{Connected: true, PrinterState: "STOPPED", CanTurnOff: true}

	step(m, clock, printerPrinting, 5*time.Second)
	step(m, clock, cancelled, 5*time.Second)
	step(m, clock, cancelled, 9*time.Minute)
	if plug.turnedOff() != 0 {
		t.Fatal("a printer whose print was cancelled was turned off at once")
	}

	step(m, clock, cancelled, time.Minute)
	if plug.turnedOff() != 1 {
		t.Fatalf("the idle rule did not turn the printer off: %d turn-offs", plug.turnedOff())
	}
	told := events.told()
	if len(told) != 1 || told[0].endpoint != endpointAutoOffDone || told[0].auto().Reason != reasonIdle {
		t.Fatalf("events = %+v, want one auto_off_done for idling", told)
	}
	if m.idle["p1"] != nil {
		t.Fatalf("the idle time is still followed after the turn-off: %+v", m.idle["p1"])
	}
}

func TestAnIdleTurnOffThatFailsIsShownAndTriedAgain(t *testing.T) {
	m, clock, cfg, plug, _ := steppedModule(t, DeviceConfig{}, Settings{IdleOffMin: 10})
	plug.set(true)

	step(m, clock, printerIdle, 0)
	step(m, clock, printerIdle, 10*time.Minute)
	m.mu.RLock()
	_, left, fault := m.idleStateLocked("p1", cfg, m.settings(), clock.now())
	m.mu.RUnlock()
	if fault == "" || left == nil || *left != 0 {
		t.Fatalf("a failed turn-off is not shown: fault %q, left %v", fault, left)
	}

	plug.set(false)
	step(m, clock, printerIdle, 5*time.Second)
	if plug.turnedOff() != 1 {
		t.Fatal("the turn-off was not tried again")
	}
}

// The recheck just before the turn-off is the last word.
func TestAPrinterThatStartedToPrintIsNotTurnedOffForIdling(t *testing.T) {
	m, clock, cfg, plug, _ := steppedModule(t, DeviceConfig{}, Settings{IdleOffMin: 10})
	m.stepPrinter(context.Background(), "p1", cfg, printerIdle, func() (printerSafety, error) { return printerIdle, nil })
	clock.add(10 * time.Minute)
	m.stepPrinter(context.Background(), "p1", cfg, printerIdle, func() (printerSafety, error) { return printerPrinting, nil })

	if plug.turnedOff() != 0 {
		t.Fatal("a printer that had just started to print was turned off")
	}
}

// Two saves at once: the file ends as the state that was taken last.
func TestOverlappingSavesLeaveTheNewestAutoOffState(t *testing.T) {
	dir := t.TempDir()
	m := restartedModule(dir, armedPlug)
	start := time.Now()

	for round := range 40 {
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.mu.Lock()
				m.idle["p"] = &idleClock{since: start.Add(time.Duration(round*4+i) * time.Second)}
				m.mu.Unlock()
				m.saveAutoOffIfChanged()
			}()
		}
		wg.Wait()

		m.mu.RLock()
		want, err := json.Marshal(m.autoOffSnapshotLocked())
		m.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(autoOffFile(dir))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("round %d: the file holds %s, the state is %s", round, got, want)
		}
	}
}

func TestAWaitAfterAPrintIsBounded(t *testing.T) {
	huge, below := 10_000_000_000, -5
	if got := normalizeDeviceConfig(DeviceConfig{TasmotaTopic: "plug", CooldownSec: &huge}); *got.CooldownSec != maxCooldownSec {
		t.Errorf("a printer's wait of %d s was saved as %d", huge, *got.CooldownSec)
	}
	if got := normalizeDeviceConfig(DeviceConfig{TasmotaTopic: "plug", CooldownSec: &below}); *got.CooldownSec != 0 {
		t.Errorf("a printer's wait of %d s was saved as %d", below, *got.CooldownSec)
	}
	if got := normalizeDeviceConfig(DeviceConfig{TasmotaTopic: "plug"}); got.CooldownSec != nil {
		t.Errorf("a printer with no wait of its own got %d", *got.CooldownSec)
	}
	if got := normalizeSettings(Settings{AutoOffCooldownSec: huge}); got.AutoOffCooldownSec != maxCooldownSec {
		t.Errorf("the wait for all printers of %d s was saved as %d", huge, got.AutoOffCooldownSec)
	}
	if got := normalizeSettings(Settings{AutoOffCooldownSec: 300}); got.AutoOffCooldownSec != 300 {
		t.Errorf("a wait of 300 s was saved as %d", got.AutoOffCooldownSec)
	}
}

// After a long stop the wait after a print starts again from the first look.
// The printer may have been turned on by hand in the meantime.
func TestTheWaitStartsAgainAfterALongStop(t *testing.T) {
	dir := t.TempDir()
	finished := time.Now().Add(-10 * time.Hour)
	wait := time.Hour
	running := printerPrinting
	running.JobID = "job-1"
	over := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "finished"}

	m := restartedModule(dir, armedPlug)
	m.autoOffDue("p", running, wait, finished.Add(-time.Minute))
	m.autoOffDue("p", over, wait, finished) // the wait begins
	m.saveAutoOffIfChanged()

	for name, sampled := range map[string]time.Time{"sampled long ago": finished, "never sampled": {}} {
		restarted := newModule(nil)
		restarted.configs["p"] = armedPlug
		if !sampled.IsZero() {
			restarted.usage["p"] = &DeviceUsage{LastSampleAt: sampled}
		}
		restarted.setDataDir(dir)
		restarted.loadAutoOff()

		now := time.Now()
		if restarted.autoOffDue("p", over, wait, now) {
			t.Fatalf("%s: the printer was turned off at once after a long stop", name)
		}
		if restarted.autoOffDue("p", over, wait, now.Add(wait-time.Second)) {
			t.Fatalf("%s: the plug was due before a new wait was over", name)
		}
		if !restarted.autoOffDue("p", over, wait, now.Add(wait)) {
			t.Fatalf("%s: the plug was not due after a new wait", name)
		}
	}
}

// A tick that took a copy of the config before a person saved it changes
// only whether the printer is armed, and nothing for a plug that changed.
func TestTheTickKeepsAConfigSavedMeanwhile(t *testing.T) {
	m := newModule(nil)
	m.setDataDir(t.TempDir())
	idle := 30
	stale := DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug", AutoOffAfterPrint: true}
	if err := m.saveConfig(t.Context(), "p", stale); err != nil {
		t.Fatal(err)
	}
	saved := stale
	saved.IdleOffMin = &idle
	saved.AutoOffDefault = true
	if err := m.saveConfig(t.Context(), "p", saved); err != nil {
		t.Fatal(err)
	}

	m.updateConfig("p", stale, func(cur *DeviceConfig) { cur.AutoOffAfterPrint = cur.AutoOffDefault })
	got, _ := m.getConfig(t.Context(), "p")
	if got.IdleOffMin == nil || *got.IdleOffMin != idle || !got.AutoOffDefault || !got.AutoOffAfterPrint {
		t.Fatalf("the config saved meanwhile was lost: %+v", got)
	}
	var onDisk map[string]DeviceConfig
	raw, err := os.ReadFile(filepath.Join(m.dataDir, configsFileName))
	if err != nil || json.Unmarshal(raw, &onDisk) != nil || onDisk["p"].IdleOffMin == nil {
		t.Fatalf("configs.json lost the config saved meanwhile: %s %v", raw, err)
	}

	moved := saved
	moved.TasmotaTopic = "other"
	moved.AutoOffAfterPrint = false
	if err := m.saveConfig(t.Context(), "p", moved); err != nil {
		t.Fatal(err)
	}
	m.updateConfig("p", stale, func(cur *DeviceConfig) { cur.AutoOffAfterPrint = true })
	if got, _ := m.getConfig(t.Context(), "p"); got.AutoOffAfterPrint {
		t.Fatal("a tick for the old plug armed the new one")
	}

	delete(m.configs, "p")
	m.updateConfig("p", moved, func(cur *DeviceConfig) { cur.AutoOffAfterPrint = true })
	if _, ok := m.configs["p"]; ok {
		t.Fatal("a tick put back a printer that was removed")
	}
}

// Saves from more than one goroutine leave the newest config on disk.
func TestConcurrentConfigSavesLeaveTheNewestOnDisk(t *testing.T) {
	m := newModule(nil)
	m.setDataDir(t.TempDir())
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_ = m.saveConfig(t.Context(), fmt.Sprintf("p%d", i), DeviceConfig{Driver: DriverTasmota, TasmotaTopic: fmt.Sprintf("plug%d", i)})
		})
	}
	wg.Wait()
	var onDisk map[string]DeviceConfig
	raw, err := os.ReadFile(filepath.Join(m.dataDir, configsFileName))
	if err != nil || json.Unmarshal(raw, &onDisk) != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 20 {
		t.Fatalf("configs.json holds %d printers, want 20", len(onDisk))
	}
}

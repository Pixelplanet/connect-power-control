package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is the clock of a module under test. A test moves it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// energyStart is a Saturday morning, seven minutes into the slot 10:00 UTC,
// which is slot 40 of its day.
var energyStart = time.Date(2026, 11, 21, 10, 7, 0, 0, time.UTC)

const energyDay = "2026-11-21"

var (
	idlePrinter     = printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true}
	printingPrinter = printerSafety{Connected: true, PrinterState: "PRINTING", ActiveJob: true, JobID: "7", JobFile: "benchy.gcode"}
)

// clockedModule is a module whose clock a test moves, with no data directory.
func clockedModule(start time.Time) (*module, *testClock) {
	m := newModule(nil)
	clock := &testClock{t: start}
	m.now = clock.now
	return m, clock
}

// energyModule is a module with a data directory and the printer p1 on an
// HTTP plug that is on and draws 120 W, which is 2 Wh a minute. Its days are
// UTC days, whatever zone the tests run in.
func energyModule(t *testing.T, start time.Time) (*module, *testClock, DeviceConfig) {
	t.Helper()
	m, clock := clockedModule(start)
	m.setDataDir(t.TempDir())
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "UTC" })
	return m, clock, seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, Mode: "http", HTTPHost: "plug-1.lan"}, "ON", 120)
}

// seedPlug puts a plug that answers into memory and answers its config.
// watts below zero is a plug with no meter.
func seedPlug(m *module, cfg DeviceConfig, power string, watts float64) DeviceConfig {
	cfg = normalizeDeviceConfig(cfg)
	online := true
	plug := &PlugTelemetry{Topic: cfg.plugKey(), Power: power, Online: &online}
	if watts >= 0 {
		plug.PowerWatts = &watts
	}
	m.mu.Lock()
	m.plugs[cfg.plugKey()] = plug
	m.mu.Unlock()
	return cfg
}

func plugOf(m *module, cfg DeviceConfig) *PlugTelemetry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.plugs[cfg.plugKey()]
}

// tick moves the clock and samples p1 once, like the poll loop does.
func tick(m *module, clock *testClock, cfg DeviceConfig, safety printerSafety, step time.Duration) {
	clock.add(step)
	m.updateDeviceUsage("p1", cfg, safety)
}

func pendingSlot(m *module, day, id string, slot int) slotAcc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if slots := m.energy.pending[day][id]; slots != nil {
		return slots[slot]
	}
	return slotAcc{}
}

func pendingSum(m *module, id string) (sum slotAcc) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, devices := range m.energy.pending {
		if slots := devices[id]; slots != nil {
			for _, a := range slots {
				sum.add(a)
			}
		}
	}
	return sum
}

func pendingPrints(m *module) []pendingPrint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]pendingPrint(nil), m.energy.pendingPrints...)
}

func usageOf(m *module, id string) *DeviceUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneDeviceUsage(m.usage[id])
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func nearSlot(a, b slotAcc) bool {
	return near(a.Wh, b.Wh) && near(a.Obs, b.Obs) && near(a.On, b.On) && near(a.Met, b.Met) &&
		near(a.Kn, b.Kn) && near(a.Prt, b.Prt)
}

// recordSlots puts measured energy into the record, spread evenly from at
// over d.
func recordSlots(m *module, id string, at time.Time, d time.Duration, wh float64) {
	m.mu.Lock()
	m.recordEnergyLocked(id, at, at.Add(d), energyStep{wh: wh, observed: true, on: true, metered: true})
	m.mu.Unlock()
}

func queuePrint(m *module, id string, rec PrintPowerRecord) {
	m.mu.Lock()
	m.queuePrintLocked(id, rec)
	m.mu.Unlock()
}

func printRecord(file string, started time.Time, d time.Duration, wh float64) PrintPowerRecord {
	ended := started.Add(d)
	return PrintPowerRecord{
		FileName: file, Outcome: "finished", StartedAt: started, EndedAt: &ended,
		DurationSec: int(d.Seconds()), EnergyWh: wh, MeteredSec: d.Seconds(), PeakWatts: 150,
	}
}

// changeEnergy changes the energy settings in memory.
func changeEnergy(m *module, change func(*EnergySettings)) {
	s := m.energySettings()
	s.Tariffs = append([]Tariff{}, s.Tariffs...)
	change(&s)
	m.energyHeld.Store(&s)
}

func mustSave(t *testing.T, m *module) {
	t.Helper()
	if err := m.saveEnergy(); err != nil {
		t.Fatalf("save energy: %v", err)
	}
}

func readDay(t *testing.T, m *module, day string) dayFile {
	t.Helper()
	raw, err := os.ReadFile(dayPath(m.energyDir(), day))
	if err != nil {
		t.Fatalf("read the day file: %v", err)
	}
	var f dayFile
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the day file does not parse: %v: %s", err, raw)
	}
	return f
}

func readPrints(t *testing.T, m *module, month string) printsFile {
	t.Helper()
	raw, err := os.ReadFile(printsPath(m.energyDir(), month))
	if err != nil {
		t.Fatalf("read the prints file: %v", err)
	}
	var f printsFile
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the prints file does not parse: %v", err)
	}
	return f
}

// reportOf reads a report for a caller who may see the named printers.
func reportOf(t *testing.T, m *module, span, at, device string, visible ...string) energyReport {
	t.Helper()
	q := energyQuery{span: span, at: at, device: device, visible: map[string]string{}, now: m.clock()}
	for _, id := range visible {
		q.visible[id] = "Printer " + id
	}
	rep, err := m.buildEnergyReport(q)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return rep
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- The recorder ---

func TestEnergyGoesToTheSlotOfTheSample(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0) // the first sample has no time before it
	if got := pendingSum(m, "p1"); got != (slotAcc{}) {
		t.Fatalf("the first sample recorded %+v", got)
	}
	tick(m, clock, cfg, idlePrinter, time.Minute)

	want := slotAcc{Wh: 2, Obs: 60, On: 60, Met: 60, Kn: 60}
	if got := pendingSlot(m, energyDay, "p1", 40); !nearSlot(got, want) {
		t.Fatalf("slot 40 = %+v, want %+v", got, want)
	}
	if got := pendingSum(m, "p1"); !nearSlot(got, want) {
		t.Fatalf("all slots = %+v, want only slot 40 with %+v", got, want)
	}
}

func TestAStepIsSplitAcrossSlotsAndMidnight(t *testing.T) {
	m, clock, cfg := energyModule(t, time.Date(2026, 11, 21, 10, 14, 30, 0, time.UTC))
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute) // 10:14:30 to 10:15:30
	half := slotAcc{Wh: 1, Obs: 30, On: 30, Met: 30, Kn: 30}
	if a, b := pendingSlot(m, energyDay, "p1", 40), pendingSlot(m, energyDay, "p1", 41); !nearSlot(a, half) || !nearSlot(b, half) {
		t.Fatalf("slots 40 and 41 = %+v and %+v, want %+v each", a, b, half)
	}

	clock.set(time.Date(2026, 11, 21, 23, 59, 0, 0, time.UTC))
	m.updateDeviceUsage("p1", cfg, idlePrinter)     // more than two minutes later: adds nothing
	tick(m, clock, cfg, idlePrinter, 2*time.Minute) // 23:59 to 00:01
	minute := slotAcc{Wh: 2, Obs: 60, On: 60, Met: 60, Kn: 60}
	if a, b := pendingSlot(m, energyDay, "p1", 95), pendingSlot(m, "2026-11-22", "p1", 0); !nearSlot(a, minute) || !nearSlot(b, minute) {
		t.Fatalf("the last slot of the day and the first of the next = %+v and %+v, want %+v each", a, b, minute)
	}
	if got := pendingSum(m, "p1"); !near(got.Wh, 6) || !near(got.Obs, 180) {
		t.Fatalf("all slots = %+v, want 6 Wh over 180 s", got)
	}
}

func TestSessionPrintAndSlotGetTheSameEnergy(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	plugOf(m, cfg).Power = "OFF"
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, 5*time.Second)

	// The plug goes on and the print starts on one tick: that tick opens the
	// session and the print with the energy the slot gets.
	plugOf(m, cfg).Power = "ON"
	tick(m, clock, cfg, printingPrinter, 5*time.Second)
	u := usageOf(m, "p1")
	first := 120 * 5.0 / 3600
	if u.Session == nil || u.CurrentPrint == nil || !near(u.Session.EnergyWh, first) || !near(u.CurrentPrint.EnergyWh, first) {
		t.Fatalf("the opening tick: session %+v, print %+v, want %v Wh in both", u.Session, u.CurrentPrint, first)
	}
	for range 10 {
		tick(m, clock, cfg, printingPrinter, 5*time.Second)
	}

	u = usageOf(m, "p1")
	sum := pendingSum(m, "p1")
	if !near(sum.Wh, u.Session.EnergyWh) || !near(sum.Wh, u.CurrentPrint.EnergyWh) || !near(sum.Wh, 120*55.0/3600) {
		t.Fatalf("slot %v Wh, session %v Wh, print %v Wh: they differ", sum.Wh, u.Session.EnergyWh, u.CurrentPrint.EnergyWh)
	}
	if !near(sum.Obs, 60) || !near(sum.On, 55) || !near(sum.Met, 55) || !near(u.CurrentPrint.MeteredSec, 55) {
		t.Fatalf("slot %+v, print metered %v s; want 60 s observed and 55 s on and metered", sum, u.CurrentPrint.MeteredSec)
	}
}

func TestAPlugThatIsOffRecordsAZero(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	plugOf(m, cfg).Power = "OFF" // the last reading of 120 W is still there
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)

	if got, want := pendingSlot(m, energyDay, "p1", 40), (slotAcc{Obs: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("slot = %+v, want %+v: observed, not on, no energy", got, want)
	}
	mustSave(t, m)
	rows := readDay(t, m, energyDay).Devices["p1"]
	if len(rows) != 1 || rows[0] != (slotRow{40, 0, 60, 0, 0, 0, 60, 0}) {
		t.Fatalf("day file rows = %v, want one row that proves the zero", rows)
	}
}

func TestAPlugWithNoMeterRecordsOnTime(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	plugOf(m, cfg).PowerWatts = nil
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)

	if got, want := pendingSlot(m, energyDay, "p1", 40), (slotAcc{Obs: 60, On: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("slot = %+v, want %+v: on, not metered", got, want)
	}
}

func TestAnOfflinePlugAddsNothing(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	offline := false
	plugOf(m, cfg).Online = &offline // it keeps ON and 120 W, as a dead plug does
	tick(m, clock, cfg, idlePrinter, 0)
	for range 12 {
		tick(m, clock, cfg, idlePrinter, 5*time.Second)
	}
	if got := pendingSum(m, "p1"); got != (slotAcc{}) {
		t.Fatalf("an offline plug recorded %+v", got)
	}
	if u := usageOf(m, "p1"); u.Session == nil || u.Session.EnergyWh != 0 {
		t.Fatalf("an offline plug added to the session: %+v", u.Session)
	}

	online := true
	plugOf(m, cfg).Online = &online
	tick(m, clock, cfg, idlePrinter, 5*time.Second)
	if got := pendingSum(m, "p1"); !near(got.Obs, 5) || !near(got.Wh, 120*5.0/3600) {
		t.Fatalf("back online it recorded %+v, want 5 s", got)
	}
}

func TestABrokerThatIsDownAddsNothing(t *testing.T) {
	m, clock := clockedModule(energyStart)
	m.setDataDir(t.TempDir())
	cfg := seedPlug(m, DeviceConfig{Driver: DriverGenericMQTT, MQTTCommandTopic: "plug/cmd"}, "ON", 120)

	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got := pendingSum(m, "p1"); got != (slotAcc{}) {
		t.Fatalf("with the broker down it recorded %+v", got)
	}

	m.mu.Lock()
	m.mqttUp = true
	m.mu.Unlock()
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got := pendingSum(m, "p1"); !near(got.Wh, 2) {
		t.Fatalf("with the broker up it recorded %+v, want 2 Wh", got)
	}
}

func tasmotaPlug(t *testing.T, watts float64) (*module, *testClock, DeviceConfig) {
	t.Helper()
	m, clock := clockedModule(energyStart)
	m.setDataDir(t.TempDir())
	m.mqttUp = true
	return m, clock, seedPlug(m, DeviceConfig{Driver: DriverTasmota, TasmotaTopic: "plug_1"}, "ON", watts)
}

func TestAnOldTasmotaReadingIsNotCounted(t *testing.T) {
	m, clock, cfg := tasmotaPlug(t, 120)
	plugOf(m, cfg).meterAt = energyStart.Add(-5 * time.Minute)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got, want := pendingSum(m, "p1"), (slotAcc{Obs: 60, On: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("with a reading of five minutes ago: %+v, want %+v", got, want)
	}

	plugOf(m, cfg).meterAt = clock.now()
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got, want := pendingSum(m, "p1"), (slotAcc{Wh: 2, Obs: 120, On: 120, Met: 60, Kn: 120}); !nearSlot(got, want) {
		t.Fatalf("with a fresh reading: %+v, want %+v", got, want)
	}
}

// A Tasmota plug with no meter never reports a reading, and its last message
// can be old. It is still there.
func TestATasmotaPlugWithNoMeterStaysObserved(t *testing.T) {
	m, clock, cfg := tasmotaPlug(t, -1)
	old := energyStart.Add(-time.Hour)
	plugOf(m, cfg).UpdatedAt = &old
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got, want := pendingSum(m, "p1"), (slotAcc{Obs: 60, On: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
}

// A plug that reports only when its load changes says nothing for hours
// under a steady load. Its reading still counts.
func TestAPushPlugUnderASteadyLoadKeepsCounting(t *testing.T) {
	m, clock := clockedModule(energyStart)
	m.setDataDir(t.TempDir())
	m.mqttUp = true
	cfg := seedPlug(m, DeviceConfig{Driver: DriverGenericMQTT, MQTTCommandTopic: "plug/cmd"}, "ON", 120)
	old := energyStart.Add(-3 * time.Hour)
	plugOf(m, cfg).UpdatedAt = &old
	plugOf(m, cfg).meterAt = old

	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got, want := pendingSum(m, "p1"), (slotAcc{Wh: 2, Obs: 60, On: 60, Met: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
}

// onePrint runs a print of three ticks and one tick after it.
func onePrint(m *module, clock *testClock, cfg DeviceConfig, job string) {
	safety := printingPrinter
	safety.JobID = job
	for range 3 {
		tick(m, clock, cfg, safety, 5*time.Second)
	}
	tick(m, clock, cfg, idlePrinter, 5*time.Second)
}

func TestNothingIsRecordedWithoutADataDirectory(t *testing.T) {
	m, clock := clockedModule(energyStart)
	cfg := seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, Mode: "http", HTTPHost: "plug-1.lan"}, "ON", 120)
	tick(m, clock, cfg, idlePrinter, 0)
	onePrint(m, clock, cfg, "1")

	if got := pendingSum(m, "p1"); got != (slotAcc{}) || len(pendingPrints(m)) != 0 {
		t.Fatalf("recorded %+v and %d prints with no data directory", got, len(pendingPrints(m)))
	}
	if u := usageOf(m, "p1"); u.Session == nil || u.Session.EnergyWh <= 0 || len(u.PrintHistory) != 1 {
		t.Fatalf("the session and the print history must still work: %+v", u)
	}
	if err := m.saveEnergy(); err != nil {
		t.Fatalf("save with no data directory: %v", err)
	}
	if rep := reportOf(t, m, "day", "", "", "p1"); rep.Recording || rep.Earliest != "" {
		t.Fatalf("report says recording=%v earliest=%q", rep.Recording, rep.Earliest)
	}
}

func TestNothingIsRecordedWithAWrongClock(t *testing.T) {
	m, clock, cfg := energyModule(t, time.Date(2021, 3, 1, 12, 0, 0, 0, time.UTC))
	tick(m, clock, cfg, idlePrinter, 0)
	onePrint(m, clock, cfg, "1")
	if got := pendingSum(m, "p1"); got != (slotAcc{}) || len(pendingPrints(m)) != 0 {
		t.Fatalf("recorded %+v and %d prints under a clock in 2021", got, len(pendingPrints(m)))
	}

	// A wrong clock deletes nothing either.
	recordSlots(m, "p1", energyStart, time.Minute, 2)
	clock.set(energyStart)
	mustSave(t, m)
	clock.set(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	m.pruneEnergy(clock.now())
	if !exists(dayPath(m.energyDir(), energyDay)) {
		t.Fatal("a prune under a wrong clock deleted a day file")
	}
}

// "nan" is what some firmware reports for a sensor with no value, and it
// parses as a number. One such reading in a sum leaves no number in it, and
// JSON cannot hold it, so the day file of every printer could not be written
// any more.
func TestAReadingThatIsNotANumberIsNotCounted(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), 1e300, -5} {
		m, clock, cfg := energyModule(t, energyStart)
		read := func(watts float64) {
			m.mu.Lock()
			m.plugs[cfg.plugKey()].PowerWatts = &watts
			m.mu.Unlock()
		}
		tick(m, clock, cfg, printingPrinter, 0)
		tick(m, clock, cfg, printingPrinter, time.Minute) // 2 Wh
		read(bad)
		tick(m, clock, cfg, printingPrinter, time.Minute)
		read(120)
		tick(m, clock, cfg, printingPrinter, time.Minute) // 2 Wh

		u := usageOf(m, "p1")
		if !near(u.Session.EnergyWh, 4) || !near(u.CurrentPrint.EnergyWh, 4) || u.Session.PeakWatts != 120 || u.CurrentPrint.PeakWatts != 120 {
			t.Fatalf("after a reading of %v: session %+v, print %+v; want 4 Wh and a peak of 120 W in both", bad, u.Session, u.CurrentPrint)
		}
		if got, want := pendingSlot(m, energyDay, "p1", 40), (slotAcc{Wh: 4, Obs: 180, On: 180, Met: 120, Kn: 180, Prt: 4}); !nearSlot(got, want) {
			t.Fatalf("after a reading of %v the slot holds %+v, want %+v", bad, got, want)
		}
		mustSave(t, m)
		if err := m.saveAllUsage(); err != nil {
			t.Fatalf("after a reading of %v usage.json cannot be written: %v", bad, err)
		}
		rep := reportOf(t, m, "day", "", "", "p1")
		if _, err := json.Marshal(rep); err != nil || rep.Total.EnergyWh != 4 || rep.Total.MeteredSec != 120 || rep.Total.OnSec != 180 {
			t.Fatalf("after a reading of %v the report is %+v: %v", bad, rep.Total, err)
		}
	}

	// Whatever reaches the record, the file of the day can be written.
	m, _, _ := energyModule(t, energyStart)
	recordSlots(m, "p1", energyStart, time.Minute, math.NaN())
	recordSlots(m, "p2", energyStart, time.Minute, 2)
	mustSave(t, m)
	if rows := readDay(t, m, energyDay).Devices; rows["p1"][0][1] != 0 || rows["p2"][0][1] != 2 {
		t.Fatalf("the day file holds %v", rows)
	}
	// A figure in a file that the cache cannot hold stays a number.
	writeEnergyFile(t, dayPath(m.energyDir(), "2026-11-19"), `{"v":1,"day":"2026-11-19","devices":{"p1":[[40,1e300,900,900,900,0]]}}`)
	if _, err := json.Marshal(reportOf(t, m, "day", "2026-11-19", "", "p1")); err != nil {
		t.Fatalf("a report of a file with a huge figure: %v", err)
	}
}

// --- Files ---

func TestEnergySurvivesARestart(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	for range 4 {
		onePrint(m, clock, cfg, "1")
		tick(m, clock, cfg, idlePrinter, 100*time.Second)
	}
	mustSave(t, m)
	before := reportOf(t, m, "day", "", "", "p1")
	if before.Total.EnergyWh <= 0 || before.PrintsTotal != 4 || before.Earliest != energyDay {
		t.Fatalf("nothing to compare: %+v", before)
	}
	if got := pendingSum(m, "p1"); got != (slotAcc{}) || len(pendingPrints(m)) != 0 {
		t.Fatalf("after a save memory still holds %+v and %d prints", got, len(pendingPrints(m)))
	}

	restarted, _ := clockedModule(clock.now())
	restarted.configureData(m.dataDir)
	changeEnergy(restarted, func(s *EnergySettings) { s.TimeZone = "UTC" })
	if after := reportOf(t, restarted, "day", "", "", "p1"); !reflect.DeepEqual(before, after) {
		t.Fatalf("the report changed over a restart:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestAFailedWriteKeepsThePendingPart(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	// The day file and the prints file cannot be written: a directory is in
	// the way of each.
	dayBlock, printsBlock := dayPath(m.energyDir(), energyDay), printsPath(m.energyDir(), "2026-11")
	for _, block := range []string{dayBlock, printsBlock} {
		if err := os.MkdirAll(block, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	queuePrint(m, "p1", printRecord("a.gcode", energyStart, time.Minute, 2))

	if err := m.saveEnergy(); err == nil {
		t.Fatal("a save onto a directory reported no error")
	}
	if got := pendingSum(m, "p1"); !near(got.Wh, 2) || len(pendingPrints(m)) != 1 {
		t.Fatalf("after the failed save memory holds %+v and %d prints, want 2 Wh and 1 print", got, len(pendingPrints(m)))
	}
	// The report still shows what could not be written.
	if rep := reportOf(t, m, "day", "", "", "p1"); !near(rep.Total.EnergyWh, 2) || rep.PrintsTotal != 1 {
		t.Fatalf("report after the failed save: %+v", rep.Total)
	}

	// What is recorded meanwhile joins the part that waits.
	tick(m, clock, cfg, idlePrinter, time.Minute)
	for _, block := range []string{dayBlock, printsBlock} {
		if err := os.Remove(block); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, m)
	if rows := readDay(t, m, energyDay).Devices["p1"]; len(rows) != 1 || rows[0] != (slotRow{40, 4, 120, 120, 120, 0, 120, 0}) {
		t.Fatalf("day file rows = %v, want 4 Wh over 120 s", rows)
	}
	if got := readPrints(t, m, "2026-11").Prints["p1"]; len(got) != 1 {
		t.Fatalf("prints file holds %d prints, want 1", len(got))
	}
	if got := pendingSum(m, "p1"); got != (slotAcc{}) || len(pendingPrints(m)) != 0 {
		t.Fatalf("after the save memory still holds %+v", got)
	}
}

// The host may send plugin.configure again. What is not saved yet stays.
func TestConfigureTwiceKeepsUnsavedEnergy(t *testing.T) {
	m, clock := clockedModule(energyStart)
	dir := t.TempDir()
	m.configureData(dir)
	cfg := seedPlug(m, DeviceConfig{Driver: DriverTasmotaHTTP, Mode: "http", HTTPHost: "plug-1.lan"}, "ON", 120)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, printingPrinter, time.Minute)
	queuePrint(m, "p1", printRecord("a.gcode", energyStart, time.Minute, 2))

	m.configureData(dir)
	if got := pendingSum(m, "p1"); !near(got.Wh, 2) || len(pendingPrints(m)) != 1 {
		t.Fatalf("a second configure left %+v and %d prints, want 2 Wh and 1 print", got, len(pendingPrints(m)))
	}
	if u := usageOf(m, "p1"); u == nil || u.CurrentPrint == nil || !near(u.Session.EnergyWh, 2) {
		t.Fatalf("a second configure replaced the usage in memory: %+v", u)
	}

	// Another directory is another record.
	if !m.setDataDir(t.TempDir()) || m.setDataDir(m.dataDir) {
		t.Fatal("setDataDir does not say whether the directory changed")
	}
}

func TestTwoSavesAtOnceLeaveAValidFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.json")
	var wg sync.WaitGroup
	for writer := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]string{}
			for i := range 2000 {
				body[fmt.Sprintf("key-%d", i)] = strings.Repeat(fmt.Sprint(writer), 40)
			}
			for range 5 {
				if err := writeJSONAtomic(target, body); err != nil {
					t.Errorf("writer %d: %v", writer, err)
				}
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err = json.Unmarshal(raw, &got); err != nil || len(got) != 2000 {
		t.Fatalf("the file is not one writer's whole body: %v, %d keys", err, len(got))
	}
	for key, value := range got {
		if value != got["key-0"] {
			t.Fatalf("%s holds %q, another writer's value than key-0", key, value)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temporary files were left: %v", left)
	}

	// The same for the energy record, while the recorder runs.
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if err := m.saveEnergy(); err != nil {
						t.Errorf("save: %v", err)
					}
					_ = m.saveAllUsage()
				}
			}
		}()
	}
	for range 200 {
		tick(m, clock, cfg, idlePrinter, 3*time.Second) // 0.1 Wh each
	}
	close(stop)
	wg.Wait()
	mustSave(t, m)

	sum := 0.0
	for _, row := range readDay(t, m, energyDay).Devices["p1"] {
		sum += row[1]
	}
	// Every save rounds the file to a thousandth of a Wh.
	if math.Abs(sum-20) > 0.05 {
		t.Fatalf("the day file holds %v Wh, want 20", sum)
	}
	left, _ = filepath.Glob(filepath.Join(m.energyDir(), energyDaysDir, "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temporary files were left: %v", left)
	}
}

// A write that was cut off leaves its temporary file. Those of the files in
// the data directory itself hold the saved secrets, and are removed at the
// start.
func TestLeftoverTemporaryFilesAreRemovedAtTheStart(t *testing.T) {
	dir := t.TempDir()
	files := map[string]bool{ // name -> stays
		"usage.json.123456.tmp":    false,
		"configs.json.9.tmp":       false,
		"settings.json.4821.tmp":   false,
		"energy.json.77.tmp":       false,
		"autooff.json.5.tmp":       false,
		"autooff.json":             true,
		"usage.json.tmp":           false, // the name earlier versions wrote to
		"usage.json":               true,
		"configs.json":             true,
		"settings.json":            true,
		"energy.json.bad":          true,
		"usage.json.bak":           true,
		"notes.tmp":                true,
		"other.json.1.tmp":         true,
		"myusage.json.1.tmp":       true,
		"settings.json.tmp.backup": true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "usage.json.dir.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	m, _ := clockedModule(energyStart)
	m.configureData(dir)
	for name, stays := range files {
		if exists(filepath.Join(dir, name)) != stays {
			t.Errorf("%s: there %v, want %v", name, !stays, stays)
		}
	}
	if !exists(filepath.Join(dir, "usage.json.dir.tmp")) {
		t.Error("a directory was removed")
	}

	// A second configure of the same directory touches nothing: a write of
	// this process may be under way.
	running := filepath.Join(dir, "usage.json.555.tmp")
	if err := os.WriteFile(running, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.configureData(dir)
	if !exists(running) {
		t.Error("a second configure removed a temporary file")
	}
}

func TestADamagedDayFileIsSetAside(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	path := dayPath(m.energyDir(), energyDay)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	damaged := []byte(`{"v":1,"day":"2026-11-21","devices":{"p1":[[40,`)
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	mustSave(t, m)

	if kept, err := os.ReadFile(path + ".bad"); err != nil || !bytes.Equal(kept, damaged) {
		t.Fatalf("the damaged file was not kept as it was: %v: %s", err, kept)
	}
	if rows := readDay(t, m, energyDay).Devices["p1"]; len(rows) != 1 || rows[0][1] != 2 {
		t.Fatalf("the new day file holds %v, want the 2 Wh recorded since", rows)
	}

	// A second damaged file of the same day does not replace the first.
	if err := os.WriteFile(path, []byte("again"), 0o600); err != nil {
		t.Fatal(err)
	}
	tick(m, clock, cfg, idlePrinter, time.Minute)
	mustSave(t, m)
	aside, _ := filepath.Glob(path + ".bad*")
	if kept, _ := os.ReadFile(path + ".bad"); len(aside) != 2 || !bytes.Equal(kept, damaged) {
		t.Fatalf("set aside: %v, want the first copy and a second one", aside)
	}
}

func TestANewerFileVersionIsLeftAlone(t *testing.T) {
	for name, files := range map[string][2]string{
		"the same shape": {
			`{"v":2,"day":"2026-11-21","devices":{"p1":[[40,5,900,900,900,0]]},"counters":{"p1":1}}`,
			`{"v":2,"month":"2026-11","prints":{"p1":[]},"index":[1]}`,
		},
		// A later version may give the rows another shape. Such a file does
		// not parse as version 1, and is still not a damaged file.
		"another shape": {
			`{"v":2,"day":"2026-11-21","devices":{"p1":[{"slot":40,"wh":5}]}}`,
			`{"v":3,"month":"2026-11","prints":[{"printer":"p1","file":"a.gcode"}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, clock, cfg := energyModule(t, energyStart)
			paths := [2]string{dayPath(m.energyDir(), energyDay), printsPath(m.energyDir(), "2026-11")}
			for i, path := range paths {
				writeEnergyFile(t, path, files[i])
			}
			tick(m, clock, cfg, idlePrinter, 0)
			tick(m, clock, cfg, idlePrinter, time.Minute)
			queuePrint(m, "p1", printRecord("b.gcode", energyStart, time.Minute, 2))
			mustSave(t, m)

			for i, path := range paths {
				if kept, err := os.ReadFile(path); err != nil || string(kept) != files[i] {
					t.Fatalf("the newer file %s was changed: %v: %s", filepath.Base(path), err, kept)
				}
				if aside, _ := filepath.Glob(path + ".bad*"); len(aside) != 0 {
					t.Fatalf("the newer file %s was set aside", filepath.Base(path))
				}
			}
			if rep := reportOf(t, m, "day", "", "", "p1"); rep.Total.EnergyWh != 0 || rep.PrintsTotal != 0 {
				t.Fatalf("the newer files were read as version 1: %+v, %d prints", rep.Total, rep.PrintsTotal)
			}
		})
	}
}

// A day file that is put into the directory while the extension runs, from
// a backup for one, is read, also when that day was asked for before.
func TestADayFilePutThereLaterIsRead(t *testing.T) {
	m, _, _ := energyModule(t, energyStart)
	if rep := reportOf(t, m, "day", "2026-11-19", "", "p1"); rep.Total.ObservedSec != 0 {
		t.Fatalf("an empty day holds %+v", rep.Total)
	}
	path := dayPath(m.energyDir(), "2026-11-19")
	writeEnergyFile(t, path, `{"v":1,"day":"2026-11-19","devices":{"p1":[[40,5,900,900,900,0]]}}`)
	if rep := reportOf(t, m, "day", "2026-11-19", "", "p1"); rep.Total.EnergyWh != 5 || rep.Earliest != "2026-11-19" {
		t.Fatalf("the new file is not read: %+v, earliest %q", rep.Total, rep.Earliest)
	}
	writeEnergyFile(t, path, `{"v":1,"day":"2026-11-19","devices":{"p1":[[40,5,900,900,900,0],[41,7,900,900,900,0]]}}`)
	if rep := reportOf(t, m, "day", "2026-11-19", "", "p1"); rep.Total.EnergyWh != 12 {
		t.Fatalf("the changed file is not read: %+v", rep.Total)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rep := reportOf(t, m, "day", "2026-11-19", "", "p1"); rep.Total.ObservedSec != 0 {
		t.Fatalf("a deleted file is still shown: %+v", rep.Total)
	}
}

// --- Prints ---

func TestAFinishedPrintIsKeptBeyondTheLastTen(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	for i := range 12 {
		onePrint(m, clock, cfg, fmt.Sprint(i))
	}
	if got := len(usageOf(m, "p1").PrintHistory); got != maxPrintHistory {
		t.Fatalf("the printer page keeps %d prints, want %d", got, maxPrintHistory)
	}
	if !m.energyFlushDue(clock.now()) {
		t.Fatal("a finished print does not ask for a flush")
	}
	mustSave(t, m)
	if got := len(readPrints(t, m, "2026-11").Prints["p1"]); got != 12 {
		t.Fatalf("the print store holds %d prints, want 12", got)
	}
	rep := reportOf(t, m, "month", "", "", "p1")
	if rep.PrintsTotal != 12 || len(rep.Prints) != 12 || rep.Prints[0].JobID != "11" {
		t.Fatalf("the report holds %d prints, newest job %q; want 12 and job 11", rep.PrintsTotal, rep.Prints[0].JobID)
	}
	if p := rep.Prints[0]; p.FileName != "benchy.gcode" || p.Outcome != "finished" || p.DurationSec != 15 || p.MeteredSec != 15 || p.EnergyWh != 0.5 {
		t.Fatalf("print = %+v", p)
	}
}

func TestAPrintFinalizedTwiceIsStoredOnce(t *testing.T) {
	m, _, _ := energyModule(t, energyStart)
	started := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	queuePrint(m, "p1", printRecord("a.gcode", started, time.Hour, 50))
	mustSave(t, m)
	// A crash after the print store was written leaves the print running in
	// usage.json. The restart ends it again, later.
	queuePrint(m, "p1", printRecord("a.gcode", started, 2*time.Hour, 60))
	// The same start on another printer is another print.
	queuePrint(m, "p2", printRecord("a.gcode", started, time.Hour, 50))
	mustSave(t, m)
	f := readPrints(t, m, "2026-11")
	if got := f.Prints["p1"]; len(got) != 1 || got[0].EnergyWh != 50 {
		t.Fatalf("p1 holds %+v, want the first copy only", got)
	}
	if len(f.Prints["p2"]) != 1 {
		t.Fatalf("p2 holds %d prints, want 1", len(f.Prints["p2"]))
	}

	// Across a month edge the two copies would go to two files.
	started = time.Date(2026, 10, 31, 23, 50, 0, 0, time.UTC)
	queuePrint(m, "p1", printRecord("b.gcode", started, 8*time.Minute, 5))
	mustSave(t, m)
	queuePrint(m, "p1", printRecord("b.gcode", started, 20*time.Minute, 9))
	mustSave(t, m)
	if got := readPrints(t, m, "2026-10").Prints["p1"]; len(got) != 1 || got[0].EnergyWh != 5 {
		t.Fatalf("October holds %+v, want the first copy", got)
	}
	if got := readPrints(t, m, "2026-11").Prints["p1"]; len(got) != 1 {
		t.Fatalf("November holds %d prints of p1, want only the one of the 20th", len(got))
	}
	// Two copies still in the queue are one print in a report.
	queuePrint(m, "p3", printRecord("c.gcode", started.Add(24*time.Hour), time.Minute, 1))
	queuePrint(m, "p3", printRecord("c.gcode", started.Add(24*time.Hour), time.Hour, 2))
	if rep := reportOf(t, m, "month", "", "", "p3"); rep.PrintsTotal != 1 {
		t.Fatalf("the report holds %d prints of p3, want 1", rep.PrintsTotal)
	}
}

func TestAPrintThatEndedDuringAGapEndsAtTheLastSample(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, printingPrinter, 0)
	for range 3 {
		tick(m, clock, cfg, printingPrinter, 5*time.Second)
	}
	last := clock.now()

	// Nothing is sampled for ten minutes, as over a restart. Then the
	// printer is found idle.
	tick(m, clock, cfg, idlePrinter, 10*time.Minute)
	history := usageOf(m, "p1").PrintHistory
	if len(history) != 1 {
		t.Fatalf("print history holds %d prints, want 1", len(history))
	}
	if p := history[0]; p.EndedAt == nil || !p.EndedAt.Equal(last) || !p.EndEstimated || p.DurationSec != 15 {
		t.Fatalf("print ended %v (estimated %v) after %d s; want the last sample %v, estimated, 15 s", p.EndedAt, p.EndEstimated, p.DurationSec, last)
	}
	if queued := pendingPrints(m); len(queued) != 1 || !queued[0].Record.EndedAt.Equal(last) || !queued[0].Record.EndEstimated {
		t.Fatalf("queued %+v", queued)
	}

	// A print that ends between two samples ends now.
	tick(m, clock, cfg, printingPrinter, 5*time.Second)
	tick(m, clock, cfg, idlePrinter, 5*time.Second)
	if p := usageOf(m, "p1").PrintHistory[0]; !p.EndedAt.Equal(clock.now()) || p.EndEstimated {
		t.Fatalf("print ended %v (estimated %v), want now %v and not estimated", p.EndedAt, p.EndEstimated, clock.now())
	}
}

func TestAFullMonthCountsDroppedPrints(t *testing.T) {
	m, _, _ := energyModule(t, energyStart)
	started := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxPrintsPerMonth + 3 {
		queuePrint(m, "p1", printRecord("a.gcode", started.Add(time.Duration(i)*time.Minute), 30*time.Second, 1))
	}
	queuePrint(m, "p2", printRecord("a.gcode", started, time.Minute, 1))
	mustSave(t, m)

	f := readPrints(t, m, "2026-11")
	if len(f.Prints["p1"]) != maxPrintsPerMonth || f.Dropped["p1"] != 3 {
		t.Fatalf("p1: %d prints kept and %d dropped, want %d and 3", len(f.Prints["p1"]), f.Dropped["p1"], maxPrintsPerMonth)
	}
	if len(f.Prints["p2"]) != 1 || f.Dropped["p2"] != 0 {
		t.Fatalf("p2: %d prints kept and %d dropped, want 1 and 0", len(f.Prints["p2"]), f.Dropped["p2"])
	}
	if rep := reportOf(t, m, "month", "", "", "p1", "p2"); rep.PrintsDropped != 3 || rep.PrintsTotal != maxPrintsPerMonth+1 {
		t.Fatalf("report: %d prints, %d dropped; want %d and 3", rep.PrintsTotal, rep.PrintsDropped, maxPrintsPerMonth+1)
	}
	if rep := reportOf(t, m, "month", "", "", "p2"); rep.PrintsDropped != 0 {
		t.Fatalf("a caller who sees only p2 is told of %d dropped prints of p1", rep.PrintsDropped)
	}
}

// --- The keep time ---

func writeEnergyFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cachedDays(m *module) map[string]bool {
	m.energy.io.Lock()
	defer m.energy.io.Unlock()
	out := make(map[string]bool, len(m.energy.days))
	for day := range m.energy.days {
		out[day] = true
	}
	return out
}

func TestOldFilesAreDeletedAfterTheKeepTime(t *testing.T) {
	m, clock, _ := energyModule(t, energyStart)
	dir := m.energyDir()
	days := filepath.Join(dir, energyDaysDir)
	prints := filepath.Join(dir, energyPrintsDir)
	for _, day := range []string{"2026-09-01", "2026-10-21", "2026-10-22", "2026-11-19", "2026-11-20"} {
		writeEnergyFile(t, dayPath(dir, day), `{"v":1,"day":"`+day+`","devices":{"p1":[[40,5,900,900,900,0]]}}`)
	}
	for _, month := range []string{"2026-08", "2026-09", "2026-10", "2026-11"} {
		writeEnergyFile(t, printsPath(dir, month), `{"v":1,"month":"`+month+`","prints":{}}`)
	}
	writeEnergyFile(t, filepath.Join(days, "2026-09-02.json.bad"), "x")
	writeEnergyFile(t, filepath.Join(days, "notes.txt"), "x")
	// The old day is read while it is kept, so it is in the cache, and must
	// leave it too.
	if before := reportOf(t, m, "day", "2026-09-01", "", "p1"); before.Total.EnergyWh != 5 || !cachedDays(m)["2026-09-01"] {
		t.Fatalf("a day inside the keep time is not read: %+v", before.Total)
	}
	changeEnergy(m, func(s *EnergySettings) { s.KeepDays = 30 }) // kept from 22 October 10:07
	if before := reportOf(t, m, "day", "2026-09-01", "", "p1"); before.Total.EnergyWh != 0 {
		t.Fatalf("a day before the keep time is shown: %+v", before.Total)
	}

	m.pruneEnergy(clock.now())
	if cachedDays(m)["2026-09-01"] {
		t.Error("a deleted day is still in the cache")
	}

	for name, want := range map[string]bool{
		filepath.Join(days, "2026-09-01.json"):     false,
		filepath.Join(days, "2026-10-21.json"):     false,
		filepath.Join(days, "2026-10-22.json"):     true,
		filepath.Join(days, "2026-11-20.json"):     true,
		filepath.Join(days, "2026-09-02.json.bad"): false,
		filepath.Join(days, "notes.txt"):           true,
		filepath.Join(prints, "2026-08.json"):      false,
		filepath.Join(prints, "2026-09.json"):      false,
		filepath.Join(prints, "2026-10.json"):      true,
		filepath.Join(prints, "2026-11.json"):      true,
	} {
		if exists(name) != want {
			t.Errorf("%s: exists = %v, want %v", filepath.Base(name), exists(name), want)
		}
	}
	if rep := reportOf(t, m, "month", "", "", "p1"); rep.Earliest != "2026-10-22" {
		t.Fatalf("earliest = %q, want 2026-10-22", rep.Earliest)
	}
}

// The day the keep time cuts through stays on disk, and a report shows the
// part of it that is kept.
func TestTheBoundaryDayStays(t *testing.T) {
	m, clock, _ := energyModule(t, energyStart)
	changeEnergy(m, func(s *EnergySettings) { s.KeepDays = 30; s.TimeZone = "UTC" })
	recordSlots(m, "p1", time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC), 15*time.Minute, 100)
	recordSlots(m, "p1", time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), 15*time.Minute, 7)
	mustSave(t, m) // the first save of a day prunes
	m.pruneEnergy(clock.now())

	if rows := readDay(t, m, "2026-10-22").Devices["p1"]; len(rows) != 2 {
		t.Fatalf("the boundary day holds %d rows, want both", len(rows))
	}
	rep := reportOf(t, m, "month", "2026-10-22", "", "p1")
	if rep.Total.EnergyWh != 7 || rep.Earliest != "2026-10-22" {
		t.Fatalf("total %v Wh, earliest %q; want the 7 Wh after the keep time and 2026-10-22", rep.Total.EnergyWh, rep.Earliest)
	}
}

// A clock that jumped ahead by more than the keep time makes every file look
// too old. The record must outlive that, and a wrong date on the host is
// often corrected within the day.
func TestAClockThatJumpedAheadDeletesNothing(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	queuePrint(m, "p1", printRecord("a.gcode", energyStart, time.Minute, 2))
	mustSave(t, m)
	day, prints := dayPath(m.energyDir(), energyDay), printsPath(m.energyDir(), "2026-11")

	recordADay := func() {
		tick(m, clock, cfg, idlePrinter, 0)
		tick(m, clock, cfg, idlePrinter, time.Minute)
		mustSave(t, m) // the first save of a day prunes
	}
	clock.set(energyStart.AddDate(3, 0, 0))
	recordADay()
	if !exists(day) || !exists(prints) {
		t.Fatalf("after the clock jumped three years: day file %v, prints file %v, want both kept", exists(day), exists(prints))
	}
	// The same when the extension starts under that clock.
	restarted, _ := clockedModule(clock.now())
	restarted.configureData(m.dataDir)
	if !exists(day) || !exists(prints) {
		t.Fatalf("after a start under that clock: day file %v, prints file %v, want both kept", exists(day), exists(prints))
	}
	if got := restarted.energySettingsAnswer().RecordingSince; got != energyDay {
		t.Fatalf("recording_since = %q, want %s", got, energyDay)
	}

	// A clock that stays there writes its own days. On the third the old
	// files count as old.
	clock.add(dayLength)
	recordADay()
	if !exists(day) || !exists(prints) {
		t.Fatal("the old files went on the second day under the new clock")
	}
	clock.add(dayLength)
	recordADay()
	if exists(day) || exists(prints) {
		t.Fatalf("on the third day: day file %v, prints file %v, want both deleted", exists(day), exists(prints))
	}
}

// A settings file that is there but cannot be used leaves the default keep
// time, which nobody chose. Nothing is deleted under it.
func TestADamagedSettingsFileDeletesNothing(t *testing.T) {
	old := energyStart.Add(-800 * dayLength)
	seed := func(t *testing.T, settings string) (m *module, clock *testClock, oldDay, oldPrints string) {
		t.Helper()
		m, clock = energyManager(t)
		dir := t.TempDir()
		if settings != "" {
			writeEnergyFile(t, filepath.Join(dir, energySettingsFile), settings)
		}
		record := filepath.Join(dir, energyDirName)
		oldDay, oldPrints = dayPath(record, old.Format(dayLayout)), printsPath(record, old.Format(monthLayout))
		writeEnergyFile(t, oldPrints, `{"v":1,"month":"`+old.Format(monthLayout)+`","prints":{}}`)
		// Three days inside the keep time, so that the clock is not in doubt.
		for _, at := range []time.Time{old, energyStart.Add(-3 * dayLength), energyStart.Add(-2 * dayLength), energyStart.Add(-dayLength)} {
			day := at.Format(dayLayout)
			writeEnergyFile(t, dayPath(record, day), `{"v":1,"day":"`+day+`","devices":{"p1":[[40,5,900,900,900,0]]}}`)
		}
		m.configureData(dir)
		return m, clock, oldDay, oldPrints
	}

	for name, settings := range map[string]string{
		"cut short":       `{"v":1,"keep_days":3650,"tariffs":[`,
		"a newer version": `{"v":2,"keep":{"days":3650}}`,
	} {
		t.Run(name, func(t *testing.T) {
			m, clock, oldDay, oldPrints := seed(t, settings)
			kept := func(when string) {
				t.Helper()
				if !exists(oldDay) || !exists(oldPrints) {
					t.Fatalf("%s: day file %v, prints file %v, want both kept", when, exists(oldDay), exists(oldPrints))
				}
			}
			kept("after the start")
			if got := energySettingsOf(t, m); got.KeepDays != defaultKeepDays || !got.PrunePaused {
				t.Fatalf("keep_days %d, prune_paused %v; want the default and true", got.KeepDays, got.PrunePaused)
			}
			if !exists(filepath.Join(m.dataDir, energySettingsFile+".bad")) {
				t.Fatal("the settings file was not set aside")
			}

			recordSlots(m, "p1", clock.now(), time.Minute, 2)
			mustSave(t, m) // the first save of a day prunes
			kept("after a flush")

			// Another setting is saved, but no keep time is chosen by it.
			mustPutEnergy(t, m, `{"currency":"CZK"}`)
			clock.add(dayLength)
			recordSlots(m, "p1", clock.now(), time.Minute, 2)
			mustSave(t, m)
			kept("after another setting was saved")
			if !energySettingsOf(t, m).PrunePaused {
				t.Fatal("saving the currency ended the pause")
			}

			// A keep time that deletes needs the flag, also when it is the
			// number memory holds.
			if code := putEnergy(t, m, `{"keep_days":365}`); code != http.StatusBadRequest {
				t.Fatalf("keep_days 365 without shorten: status %d, want 400", code)
			}
			kept("after the refused keep time")
			mustPutEnergy(t, m, `{"keep_days":3650}`)
			kept("after 3650 days were saved")
			if got := energySettingsOf(t, m); got.KeepDays != 3650 || got.PrunePaused || got.Currency != "CZK" {
				t.Fatalf("after the keep time was saved: %+v", got)
			}

			// From here on the keep time is one a person chose.
			mustPutEnergy(t, m, `{"keep_days":365,"shorten":true}`)
			if exists(oldDay) || exists(oldPrints) {
				t.Fatalf("after 365 days with shorten: day file %v, prints file %v, want both deleted", exists(oldDay), exists(oldPrints))
			}
		})
	}

	// The same number in a paused state is saved, and deletes with the flag.
	m, _, oldDay, _ := seed(t, "{")
	mustPutEnergy(t, m, `{"keep_days":365,"shorten":true}`)
	if got := energySettingsOf(t, m); got.PrunePaused || exists(oldDay) || !exists(filepath.Join(m.dataDir, energySettingsFile)) {
		t.Fatalf("365 days with shorten: paused %v, old day there %v", got.PrunePaused, exists(oldDay))
	}

	// With no settings file the default is the keep time, as before.
	m, _, oldDay, oldPrints := seed(t, "")
	if got := energySettingsOf(t, m); got.PrunePaused || exists(oldDay) || exists(oldPrints) {
		t.Fatalf("with no settings file: paused %v, old day there %v, old prints there %v", got.PrunePaused, exists(oldDay), exists(oldPrints))
	}
}

// --- Settings ---

func energySettingsOf(t *testing.T, m *module) energySettingsResponse {
	t.Helper()
	rec := serve(m, "GET", "/energy/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /energy/settings: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp energySettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// putEnergy saves a change as a manager and answers the status.
func putEnergy(t *testing.T, m *module, body string) int {
	t.Helper()
	rec := serve(m, "PUT", "/energy/settings", body)
	if rec.Code != http.StatusOK && !strings.HasSuffix(problemCode(t, rec), "INVALID_INPUT") {
		t.Fatalf("PUT /energy/settings %s: status %d: %s", body, rec.Code, rec.Body.String())
	}
	return rec.Code
}

func mustPutEnergy(t *testing.T, m *module, body string) {
	t.Helper()
	if code := putEnergy(t, m, body); code != http.StatusOK {
		t.Fatalf("PUT /energy/settings %s: status %d", body, code)
	}
}

// energyManager is a module of a person who may configure it, with a clock.
func energyManager(t *testing.T) (*module, *testClock) {
	t.Helper()
	m := managerModule(t, Settings{})
	clock := &testClock{t: energyStart}
	m.now = clock.now
	return m, clock
}

func TestEnergySettingsDefaults(t *testing.T) {
	m, _ := energyManager(t)
	rec := serve(m, "GET", "/energy/settings", "")
	for _, want := range []string{`"keep_days":365`, `"time_zone":""`, `"zone_set":false`, `"currency":"EUR"`, `"price":null`, `"price_since":null`, `"tariffs":[]`, `"recording":true`, `"recording_since":""`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the answer lacks %s: %s", want, rec.Body.String())
		}
	}
	// Reading asks the host nothing, and a viewer may read.
	if asked := m.access.(*accessFake).asked(); len(asked) != 0 {
		t.Fatalf("GET /energy/settings asked %+v", asked)
	}

	none := newModule(nil)
	if got := none.energySettingsAnswer(); got.Recording || got.KeepDays != defaultKeepDays {
		t.Fatalf("with no data directory: %+v", got)
	}
}

func TestEnergySettingsSurviveARestart(t *testing.T) {
	m, clock := energyManager(t)
	mustPutEnergy(t, m, `{"keep_days":90,"time_zone":" Europe/Prague ","currency":" CZK ","price":{"price":5.123456,"night":true,"night_price":3.2,"night_start":22,"night_end":6}}`)
	saved := m.energySettings()
	want := EnergySettings{V: 1, KeepDays: 90, TimeZone: "Europe/Prague", Currency: "CZK", Tariffs: []Tariff{
		{Price: 5.12346, Night: true, NightPrice: 3.2, NightStart: 22, NightEnd: 6},
	}}
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("saved %+v, want %+v", saved, want)
	}
	resp := energySettingsOf(t, m)
	if resp.Price == nil || resp.Price.NightPrice != 3.2 || resp.PriceSince != nil || !resp.ZoneSet || resp.UTCOffsetSec != 3600 {
		t.Fatalf("answer = %+v", resp)
	}

	restarted, _ := clockedModule(clock.now())
	restarted.configureData(m.dataDir)
	if got := restarted.energySettings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after a restart %+v, want %+v", got, want)
	}

	// A file that does not parse is set aside, and the defaults count.
	path := filepath.Join(m.dataDir, energySettingsFile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged, _ := clockedModule(clock.now())
	damaged.configureData(m.dataDir)
	if got := damaged.energySettings(); !reflect.DeepEqual(got, defaultEnergySettings()) || !exists(path+".bad") || exists(path) {
		t.Fatalf("after a damaged file: %+v, set aside %v", got, exists(path+".bad"))
	}
}

func TestEnergySettingsFromTheFileAreMadeUsable(t *testing.T) {
	got := normalizeEnergySettings(EnergySettings{
		KeepDays: 2, TimeZone: "Mars/Base", Currency: "123456789",
		Tariffs: []Tariff{
			{From: 900, Price: 2},
			{From: 300, Price: -1},
			{From: 600, Price: 1, NightPrice: 9, NightStart: 3},
			{From: 1200, Price: 1, Night: true, NightStart: 4, NightEnd: 4},
		},
	})
	want := EnergySettings{V: 1, KeepDays: minKeepDays, Currency: defaultCurrency, Tariffs: []Tariff{{From: 0, Price: 1}, {From: 900, Price: 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized %+v, want %+v", got, want)
	}
	if got = normalizeEnergySettings(EnergySettings{KeepDays: 99999}); got.KeepDays != maxKeepDays || got.Tariffs == nil {
		t.Fatalf("normalized %+v", got)
	}
	if got = normalizeEnergySettings(EnergySettings{}); got.KeepDays != defaultKeepDays {
		t.Fatalf("keep_days 0 became %d, want %d", got.KeepDays, defaultKeepDays)
	}
}

func TestAnEmptyChangeKeepsEverySetting(t *testing.T) {
	m, _ := energyManager(t)
	mustPutEnergy(t, m, `{"keep_days":90,"time_zone":"Europe/Prague","currency":"CZK","price":{"price":5.5}}`)
	path := filepath.Join(m.dataDir, energySettingsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := energySettingsOf(t, m)

	rec := serve(m, "PUT", "/energy/settings", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var answered energySettingsResponse
	if err = json.Unmarshal(rec.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if got := energySettingsOf(t, m); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(answered, want) {
		t.Fatalf("an empty change changed the settings:\n%+v\n%+v", got, want)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("an empty change rewrote the file:\n%s\n%s", before, after)
	}
	// A field alone keeps the others.
	mustPutEnergy(t, m, `{"currency":"EUR"}`)
	if got := m.energySettings(); got.Currency != "EUR" || got.KeepDays != 90 || got.TimeZone != "Europe/Prague" || len(got.Tariffs) != 1 {
		t.Fatalf("one field changed the others: %+v", got)
	}
	// An empty currency and an empty zone are values.
	mustPutEnergy(t, m, `{"currency":"","time_zone":""}`)
	if got := m.energySettings(); got.Currency != "" || got.TimeZone != "" {
		t.Fatalf("empty values were not saved: %+v", got)
	}
}

func TestPutSettingsDoesNotTouchEnergySettings(t *testing.T) {
	m, _ := energyManager(t)
	mustPutEnergy(t, m, `{"keep_days":90,"currency":"CZK","price":{"price":5.5}}`)
	want := m.energySettings()

	rec := serve(m, "PUT", "/settings", `{"keep_days":9,"currency":"X","time_zone":"UTC","tariffs":[],"clear_price":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /settings: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := m.energySettings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT /settings changed the energy settings: %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(m.dataDir, settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "keep_days") || strings.Contains(string(raw), "tariffs") {
		t.Fatalf("settings.json took energy settings: %s", raw)
	}
}

func TestEnergySettingsAreChecked(t *testing.T) {
	for name, body := range map[string]string{
		"keep 3":                  `{"keep_days":3}`,
		"keep 4000":               `{"keep_days":4000}`,
		"an unknown zone":         `{"time_zone":"Mars/Base"}`,
		"the zone Local":          `{"time_zone":"Local"}`,
		"a currency of 9":         `{"currency":"123456789"}`,
		"a control character":     `{"currency":"E\u0007R"}`,
		"price -1":                `{"price":{"price":-1}}`,
		"price 1001":              `{"price":{"price":1001}}`,
		"night price 1001":        `{"price":{"price":1,"night":true,"night_price":1001,"night_start":22,"night_end":6}}`,
		"night hours equal":       `{"price":{"price":1,"night":true,"night_price":1,"night_start":5,"night_end":5}}`,
		"hour 24":                 `{"price":{"price":1,"night":true,"night_price":1,"night_start":24,"night_end":5}}`,
		"price_from x":            `{"price":{"price":1},"price_from":"x"}`,
		"price with clear_price":  `{"price":{"price":1},"clear_price":true}`,
		"a valid field beside it": `{"currency":"CZK","keep_days":3}`,
		"not JSON":                `{"keep_days":`,
		"a body over 16 KiB":      `{"currency":"` + strings.Repeat("x", 17<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := energyManager(t)
			if code := putEnergy(t, m, body); code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", code)
			}
			if got := m.energySettings(); !reflect.DeepEqual(got, defaultEnergySettings()) {
				t.Fatalf("a refused change was kept: %+v", got)
			}
			if exists(filepath.Join(m.dataDir, energySettingsFile)) {
				t.Fatal("a refused change was saved")
			}
		})
	}
	// The limits themselves are accepted.
	m, _ := energyManager(t)
	mustPutEnergy(t, m, `{"keep_days":7,"currency":"12345678","price":{"price":1000,"night":true,"night_price":0,"night_start":23,"night_end":0}}`)
	mustPutEnergy(t, m, `{"keep_days":3650,"time_zone":"UTC","price_from":"now"}`)
}

func TestAShorterKeepTimeNeedsTheFlag(t *testing.T) {
	m, _ := energyManager(t)
	old := energyStart.Add(-100 * dayLength)
	recordSlots(m, "p1", old, time.Minute, 2)
	mustSave(t, m)
	oldDay := dayPath(m.energyDir(), old.Format(dayLayout))

	// 200 days delete nothing, so the flag is not needed.
	mustPutEnergy(t, m, `{"keep_days":200}`)
	if code := putEnergy(t, m, `{"keep_days":30}`); code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 without shorten", code)
	}
	if got := m.energySettings().KeepDays; got != 200 || !exists(oldDay) {
		t.Fatalf("the refused change counted: keep %d, old day there %v", got, exists(oldDay))
	}
	mustPutEnergy(t, m, `{"keep_days":30,"shorten":true}`)
	if got := m.energySettings().KeepDays; got != 30 {
		t.Fatalf("keep_days = %d, want 30", got)
	}
	// A longer time needs no flag.
	mustPutEnergy(t, m, `{"keep_days":400}`)
}

func TestAShorterKeepTimeDeletesAtOnce(t *testing.T) {
	m, _ := energyManager(t)
	old, recent := energyStart.Add(-100*dayLength), energyStart.Add(-3*dayLength)
	recordSlots(m, "p1", old, time.Minute, 2)
	recordSlots(m, "p1", recent, time.Minute, 2)
	queuePrint(m, "p1", printRecord("a.gcode", old, time.Minute, 2))
	mustSave(t, m)
	oldDay, recentDay := dayPath(m.energyDir(), old.Format(dayLayout)), dayPath(m.energyDir(), recent.Format(dayLayout))
	oldPrints := printsPath(m.energyDir(), old.Format(monthLayout))
	if !exists(oldDay) || !exists(recentDay) || !exists(oldPrints) {
		t.Fatal("the files to delete were not written")
	}
	reportOf(t, m, "year", "", "", "p1") // the old day is now in the cache

	mustPutEnergy(t, m, `{"keep_days":30,"shorten":true}`)
	if exists(oldDay) || exists(oldPrints) || !exists(recentDay) {
		t.Fatalf("after the change: old day %v, old prints %v, recent day %v; want false, false, true", exists(oldDay), exists(oldPrints), exists(recentDay))
	}
	if got := energySettingsOf(t, m).RecordingSince; got != recent.Format(dayLayout) {
		t.Fatalf("recording_since = %q, want %s", got, recent.Format(dayLayout))
	}
	if rep := reportOf(t, m, "year", "", "", "p1"); rep.Total.EnergyWh != 2 {
		t.Fatalf("the year holds %v Wh, want the recent 2", rep.Total.EnergyWh)
	}
}

// --- Prices ---

func costOf(t *testing.T, f energyFigures) float64 {
	t.Helper()
	if f.Cost == nil {
		t.Fatalf("no cost in %+v", f)
	}
	return *f.Cost
}

func TestTheFirstPriceCoversEarlierEnergy(t *testing.T) {
	m, _ := energyManager(t)
	recordSlots(m, "p1", energyStart.Add(-3*dayLength), 15*time.Minute, 1000)
	if rep := reportOf(t, m, "month", "", "", "p1"); rep.Total.Cost != nil || rep.PriceSet {
		t.Fatalf("a cost with no price: %+v", rep.Total)
	}

	mustPutEnergy(t, m, `{"price":{"price":0.3}}`)
	if got := m.energySettings().Tariffs; len(got) != 1 || got[0].From != 0 {
		t.Fatalf("tariffs = %+v, want one from the first record", got)
	}
	rep := reportOf(t, m, "month", "", "", "p1")
	if got := costOf(t, rep.Total); got != 0.3 || !rep.PriceSet || rep.PriceChanged {
		t.Fatalf("cost %v, price_changed %v; want 0.3 and false", got, rep.PriceChanged)
	}
	if resp := energySettingsOf(t, m); resp.PriceSince != nil || resp.Price == nil || resp.Price.Price != 0.3 {
		t.Fatalf("answer = %+v", resp)
	}
}

// twoPrices is 1 kWh at 0.30, then a price of 0.50 saved at 10:07, then 1 kWh
// in the slot after the one it was saved in.
func twoPrices(t *testing.T) (*module, *testClock) {
	t.Helper()
	m, clock := energyManager(t)
	mustPutEnergy(t, m, `{"time_zone":"UTC","price":{"price":0.3}}`)
	recordSlots(m, "p1", energyStart.Add(-2*time.Hour), 15*time.Minute, 1000)
	mustPutEnergy(t, m, `{"price":{"price":0.5}}`)
	clock.add(23 * time.Minute)
	recordSlots(m, "p1", time.Date(2026, 11, 21, 10, 15, 0, 0, time.UTC), 15*time.Minute, 1000)
	return m, clock
}

func TestALaterPriceLeavesEarlierEnergyAlone(t *testing.T) {
	m, _ := twoPrices(t)
	slot := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	want := []Tariff{{From: 0, Price: 0.3}, {From: slot.Unix(), Price: 0.5}}
	if got := m.energySettings().Tariffs; !reflect.DeepEqual(got, want) {
		t.Fatalf("tariffs = %+v, want %+v", got, want)
	}
	rep := reportOf(t, m, "day", "", "", "p1")
	if got := costOf(t, rep.Total); got != 0.8 || !rep.PriceChanged {
		t.Fatalf("cost %v, price_changed %v; want 0.30 + 0.50 and true", got, rep.PriceChanged)
	}
	if a, b := costOf(t, rep.Points[8].energyFigures), costOf(t, rep.Points[10].energyFigures); a != 0.3 || b != 0.5 {
		t.Fatalf("the hours 8 and 10 cost %v and %v, want 0.3 and 0.5", a, b)
	}
	if resp := energySettingsOf(t, m); resp.PriceSince == nil || !resp.PriceSince.Equal(slot) || resp.Price.Price != 0.5 {
		t.Fatalf("answer = %+v", resp)
	}
	// The day before knows one price only.
	if rep = reportOf(t, m, "day", "2026-11-20", "", "p1"); rep.PriceChanged {
		t.Fatal("the day before the change says the price changed")
	}
}

func TestAPriceForAllHistoryReplacesTheList(t *testing.T) {
	m, _ := twoPrices(t)
	mustPutEnergy(t, m, `{"price":{"price":0.7},"price_from":"start"}`)
	if got, want := m.energySettings().Tariffs, []Tariff{{Price: 0.7}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tariffs = %+v, want %+v", got, want)
	}
	rep := reportOf(t, m, "day", "", "", "p1")
	if got := costOf(t, rep.Total); got != 1.4 || rep.PriceChanged {
		t.Fatalf("cost %v, price_changed %v; want 2 kWh at 0.70 and false", got, rep.PriceChanged)
	}
}

func TestTheSamePriceAddsNoEntry(t *testing.T) {
	m, clock := twoPrices(t)
	clock.add(time.Hour)
	mustPutEnergy(t, m, `{"price":{"price":0.5}}`)
	if got := m.energySettings().Tariffs; len(got) != 2 {
		t.Fatalf("the same price again made %d entries, want 2", len(got))
	}
	// A price changed twice within one slot is one entry.
	mustPutEnergy(t, m, `{"price":{"price":0.6}}`)
	mustPutEnergy(t, m, `{"price":{"price":0.65}}`)
	if got := m.energySettings().Tariffs; len(got) != 3 || got[2].Price != 0.65 {
		t.Fatalf("tariffs = %+v, want three with 0.65 last", got)
	}
	// Changed back within the slot, it is no change.
	mustPutEnergy(t, m, `{"price":{"price":0.5}}`)
	if got := m.energySettings().Tariffs; len(got) != 2 || got[1].Price != 0.5 {
		t.Fatalf("tariffs = %+v, want the two from before", got)
	}

	// Past 100 entries the oldest goes, and the next covers the start.
	list := []Tariff{}
	for i := range maxTariffs + 5 {
		list = applyPrice(list, priceChange{Price: float64(i)}, "now", energyStart.Add(time.Duration(i)*time.Hour))
	}
	if len(list) != maxTariffs || list[0].From != 0 || list[0].Price != 5 || list[maxTariffs-1].Price != maxTariffs+4 {
		t.Fatalf("%d entries, first %+v, last %+v", len(list), list[0], list[len(list)-1])
	}
}

func TestClearPriceRemovesCost(t *testing.T) {
	m, _ := twoPrices(t)
	mustPutEnergy(t, m, `{"clear_price":true}`)
	if got := m.energySettings().Tariffs; got == nil || len(got) != 0 {
		t.Fatalf("tariffs = %#v, want an empty list", got)
	}
	rep := reportOf(t, m, "day", "", "", "p1")
	if rep.Total.Cost != nil || rep.PriceSet || rep.Total.EnergyWh != 2000 {
		t.Fatalf("after clearing the price: %+v", rep.Total)
	}
	if resp := energySettingsOf(t, m); resp.Price != nil || resp.Tariffs == nil {
		t.Fatalf("answer = %+v", resp)
	}
}

func TestPriceZeroIsAPrice(t *testing.T) {
	m, _ := energyManager(t)
	recordSlots(m, "p1", energyStart.Add(-time.Hour), 15*time.Minute, 1000)
	mustPutEnergy(t, m, `{"price":{"price":0}}`)
	rep := reportOf(t, m, "month", "", "", "p1")
	if !rep.PriceSet || rep.Total.Cost == nil || *rep.Total.Cost != 0 {
		t.Fatalf("price 0: price_set %v, cost %v; want true and 0", rep.PriceSet, rep.Total.Cost)
	}
	if resp := energySettingsOf(t, m); resp.Price == nil || resp.Price.Price != 0 {
		t.Fatalf("answer = %+v", resp)
	}
}

// --- A printer that goes away during a print ---

// awayPrinter is a printer that dropped off the network during job 7. The
// server still holds what it last heard.
var awayPrinter = printerSafety{PrinterState: "PRINTING", ActiveJob: true, JobID: "7", CanTurnOff: true}

// startPrint samples p1 idle once and then printing for a minute, which is
// 2 Wh in the print.
func startPrint(m *module, clock *testClock, cfg DeviceConfig) {
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, printingPrinter, time.Minute)
}

// onePrintLeft fails the test unless p1 has exactly one closed print and
// none running, and answers that print.
func onePrintLeft(t *testing.T, m *module) PrintPowerRecord {
	t.Helper()
	u := usageOf(m, "p1")
	if len(u.PrintHistory) != 1 || (u.CurrentPrint != nil && u.CurrentPrint.Active) {
		t.Fatalf("history holds %d prints, running %+v; want one closed print", len(u.PrintHistory), u.CurrentPrint)
	}
	if p := u.PrintHistory[0]; p.AwaySince != nil || p.Active {
		t.Fatalf("a closed print is still marked away or running: %+v", p)
	}
	if queued := pendingPrints(m); len(queued) != 1 {
		t.Fatalf("%d prints are queued for the print store, want 1", len(queued))
	}
	return u.PrintHistory[0]
}

func TestAPrintSurvivesAShortAbsence(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	started := usageOf(m, "p1").CurrentPrint.StartedAt

	tick(m, clock, cfg, awayPrinter, time.Minute)
	wentAway := clock.now()
	for range 2 {
		tick(m, clock, cfg, awayPrinter, time.Minute)
	}
	u := usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || len(u.PrintHistory) != 0 {
		t.Fatalf("the print did not stay open while the printer was away: %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
	if u.CurrentPrint.AwaySince == nil || !u.CurrentPrint.AwaySince.Equal(wentAway) {
		t.Fatalf("away since %v, want the first tick without the printer %v", u.CurrentPrint.AwaySince, wentAway)
	}

	tick(m, clock, cfg, printingPrinter, time.Minute)
	u = usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || len(u.PrintHistory) != 0 || len(pendingPrints(m)) != 0 {
		t.Fatalf("the print was split: running %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
	if p := u.CurrentPrint; p.AwaySince != nil || !p.StartedAt.Equal(started) || !near(p.EnergyWh, 10) || !near(p.MeteredSec, 300) || p.JobID != "7" {
		t.Fatalf("print = %+v; want the same print with 10 Wh over 300 s and no absence", p)
	}
}

func TestAPrintCutWhileAwayIsInterrupted(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute)
	wentAway := clock.now()
	for range 3 {
		tick(m, clock, cfg, awayPrinter, time.Minute)
	}

	// The printer is back with nothing running, as after a power cut.
	tick(m, clock, cfg, idlePrinter, time.Minute)
	p := onePrintLeft(t, m)
	if p.Outcome != "interrupted" || p.EndedAt == nil || !p.EndedAt.Equal(wentAway) || !p.EndEstimated {
		t.Fatalf("print = %+v; want interrupted, ended when the printer went away (%v), estimated", p, wentAway)
	}
	if p.DurationSec != 60 {
		t.Fatalf("the print lasted %d s, want 60", p.DurationSec)
	}

	// The next print is a record of its own.
	next := printingPrinter
	next.JobID = "8"
	tick(m, clock, cfg, next, time.Minute)
	if u := usageOf(m, "p1"); u.CurrentPrint == nil || u.CurrentPrint.JobID != "8" || len(u.PrintHistory) != 1 {
		t.Fatalf("after the cut print: running %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
}

func TestAPrintThatFinishedWhileAwayKeepsItsOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		back printerSafety
		want string
	}{
		"the server recorded it as finished": {printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "finished"}, "finished"},
		"the server recorded it as failed":   {printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "FAILED"}, "failed"},
		"the server recorded no outcome":     {printerSafety{Connected: true, PrinterState: "FINISHED", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "unknown"}, "interrupted"},
		"the printer says finished":          {printerSafety{Connected: true, PrinterState: "FINISHED", CanTurnOff: true}, "finished"},
		"the printer says stopped":           {printerSafety{Connected: true, PrinterState: "STOPPED", CanTurnOff: true}, "cancelled"},
		"the printer says error":             {printerSafety{Connected: true, PrinterState: "ERROR", CanTurnOff: true}, "failed"},
		"nothing says how it ended":          {printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "6", LastJobOutcome: "finished"}, "interrupted"},
		"another print runs":                 {printerSafety{Connected: true, PrinterState: "PRINTING", ActiveJob: true, JobID: "8"}, "interrupted"},
	} {
		m, clock, cfg := energyModule(t, energyStart)
		startPrint(m, clock, cfg)
		tick(m, clock, cfg, awayPrinter, time.Minute)
		wentAway := clock.now()
		tick(m, clock, cfg, tc.back, time.Minute)

		u := usageOf(m, "p1")
		if len(u.PrintHistory) != 1 {
			t.Fatalf("%s: history holds %d prints, want 1", name, len(u.PrintHistory))
		}
		if p := u.PrintHistory[0]; p.Outcome != tc.want || !p.EndedAt.Equal(wentAway) || !p.EndEstimated {
			t.Fatalf("%s: print = %+v; want outcome %q, ended %v, estimated", name, p, tc.want, wentAway)
		}
		if running := u.CurrentPrint != nil && u.CurrentPrint.Active; running != (tc.back.JobID == "8") {
			t.Fatalf("%s: a print is running: %v", name, running)
		}
	}
}

// The server's record of the end counts when the print ended while the
// printer was away.
func TestAPrintThatFinishedWhileAwayEndsWhenTheServerSaysSo(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute)
	ended := clock.now().Add(20 * time.Second)
	back := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "finished", LastJobEnded: &ended}
	tick(m, clock, cfg, back, time.Minute)

	if p := onePrintLeft(t, m); p.Outcome != "finished" || !p.EndedAt.Equal(ended) || p.EndEstimated {
		t.Fatalf("print = %+v; want finished at %v, not estimated", p, ended)
	}
}

func TestAPlugTurnedOffWhileThePrinterIsAwayEndsThePrint(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute)
	wentAway := clock.now()

	plugOf(m, cfg).Power = "OFF"
	tick(m, clock, cfg, awayPrinter, time.Minute)
	if p := onePrintLeft(t, m); p.Outcome != "interrupted" || !p.EndedAt.Equal(wentAway) || !p.EndEstimated {
		t.Fatalf("print = %+v; want interrupted when the printer went away (%v)", p, wentAway)
	}

	// Printer and power gone on one tick: the print ends then.
	m, clock, cfg = energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	plugOf(m, cfg).Power = "OFF"
	tick(m, clock, cfg, awayPrinter, time.Minute)
	if p := onePrintLeft(t, m); p.Outcome != "interrupted" || !p.EndedAt.Equal(clock.now()) {
		t.Fatalf("print = %+v; want interrupted now (%v)", p, clock.now())
	}
}

func TestALongAbsenceStopsCountingButKeepsThePrint(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg) // 2 Wh

	// The first tick without the printer and the fifteen minutes after it
	// count: sixteen ticks of 2 Wh. The four after that add nothing.
	for range 20 {
		tick(m, clock, cfg, awayPrinter, time.Minute)
	}
	u := usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || len(u.PrintHistory) != 0 {
		t.Fatalf("the print did not stay open: %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
	if !near(u.CurrentPrint.EnergyWh, 34) || !near(u.CurrentPrint.MeteredSec, 17*60) {
		t.Fatalf("the print holds %v Wh over %v s, want 34 Wh over 1020 s", u.CurrentPrint.EnergyWh, u.CurrentPrint.MeteredSec)
	}

	tick(m, clock, cfg, printingPrinter, time.Minute)
	u = usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || u.CurrentPrint.AwaySince != nil || len(u.PrintHistory) != 0 {
		t.Fatalf("the print did not go on: %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
	if !near(u.CurrentPrint.EnergyWh, 36) {
		t.Fatalf("the print holds %v Wh after the printer came back, want 36", u.CurrentPrint.EnergyWh)
	}
	// The slots hold all the plug used, and as print share what the print got.
	if sum := pendingSum(m, "p1"); !near(sum.Wh, 44) || !near(sum.Prt, 36) || !near(sum.Kn, sum.Obs) {
		t.Fatalf("slots = %+v; want 44 Wh, 36 Wh of it in the print", sum)
	}
}

func TestAnAbsenceOfADayClosesThePrint(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute)
	wentAway := clock.now()

	tick(m, clock, cfg, awayPrinter, 23*time.Hour)
	if u := usageOf(m, "p1"); u.CurrentPrint == nil || !u.CurrentPrint.Active {
		t.Fatalf("the print was closed before a day was over: %+v", u.CurrentPrint)
	}
	tick(m, clock, cfg, awayPrinter, 2*time.Hour)
	if p := onePrintLeft(t, m); p.Outcome != "interrupted" || !p.EndedAt.Equal(wentAway) || !p.EndEstimated {
		t.Fatalf("print = %+v; want interrupted when the printer went away (%v)", p, wentAway)
	}
	// No record is started for a printer that is away.
	tick(m, clock, cfg, awayPrinter, time.Minute)
	if u := usageOf(m, "p1"); len(u.PrintHistory) != 1 || u.CurrentPrint.Active {
		t.Fatalf("a print was started for a printer that is away: %+v", u.CurrentPrint)
	}
}

func TestAnOpenRowAfterReturnWaitsForTheHost(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute) // 4 Wh

	// Back on the network, but it has not said yet what it does. The server
	// still holds the print open.
	undecided := printerSafety{Connected: true, PrinterState: "IDLE", ActiveJob: true, JobID: "7"}
	for range 3 {
		tick(m, clock, cfg, undecided, time.Minute)
	}
	u := usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || u.CurrentPrint.AwaySince == nil || len(u.PrintHistory) != 0 {
		t.Fatalf("the print did not wait: %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}
	if !near(u.CurrentPrint.EnergyWh, 4) {
		t.Fatalf("the wait added energy: %v Wh, want 4", u.CurrentPrint.EnergyWh)
	}

	tick(m, clock, cfg, printingPrinter, time.Minute)
	u = usageOf(m, "p1")
	if u.CurrentPrint == nil || !u.CurrentPrint.Active || u.CurrentPrint.AwaySince != nil || len(u.PrintHistory) != 0 || !near(u.CurrentPrint.EnergyWh, 6) {
		t.Fatalf("the print did not go on: %+v, history %d", u.CurrentPrint, len(u.PrintHistory))
	}

	// The same wait, and then the server closes the print.
	m, clock, cfg = energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, awayPrinter, time.Minute)
	tick(m, clock, cfg, undecided, time.Minute)
	tick(m, clock, cfg, printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "cancelled"}, time.Minute)
	if p := onePrintLeft(t, m); p.Outcome != "cancelled" {
		t.Fatalf("print = %+v; want cancelled", p)
	}
}

func TestARestartMidPrintKeepsOneRecord(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	started := usageOf(m, "p1").CurrentPrint.StartedAt
	m.saveOnExit()

	// Five minutes later the extension runs again. The printer has not
	// signed in to the server yet on the first tick.
	restarted, clock2 := clockedModule(clock.now().Add(5 * time.Minute))
	restarted.configureData(m.dataDir)
	cfg = seedPlug(restarted, cfg, "ON", 120)
	tick(restarted, clock2, cfg, awayPrinter, 0)
	if err := restarted.saveAllUsage(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.dataDir, usageFileName))
	if err != nil || !strings.Contains(string(raw), `"away_since"`) {
		t.Fatalf("usage.json does not say the printer is away (%v): %s", err, raw)
	}
	tick(restarted, clock2, cfg, printingPrinter, 5*time.Second)
	tick(restarted, clock2, cfg, printingPrinter, 5*time.Second)

	u := usageOf(restarted, "p1")
	if len(u.PrintHistory) != 0 || len(pendingPrints(restarted)) != 0 {
		t.Fatalf("the restart closed the print: history %+v", u.PrintHistory)
	}
	if p := u.CurrentPrint; p == nil || !p.Active || !p.StartedAt.Equal(started) || p.AwaySince != nil || p.JobID != "7" {
		t.Fatalf("print after the restart = %+v; want the print that began at %v", p, started)
	}
	if exists(printsPath(restarted.energyDir(), "2026-11")) {
		t.Fatal("a print was written to the print store")
	}
}

// --- The print share of a slot ---

// cachedSlotOf reads one slot the way a report does: from the day file,
// through the cache.
func cachedSlotOf(t *testing.T, m *module, day, id string, slot int) slotAcc {
	t.Helper()
	dir := m.energyDir()
	m.energy.io.Lock()
	defer m.energy.io.Unlock()
	devices, err := m.energy.dayLocked(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range devices[id] {
		if int(c.Slot) == slot {
			return c.acc()
		}
	}
	t.Fatalf("day %s holds no slot %d of %s", day, slot, id)
	return slotAcc{}
}

func TestPrintEnergyGoesToTheSlot(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	if got, want := pendingSlot(m, energyDay, "p1", 40), (slotAcc{Wh: 2, Obs: 60, On: 60, Met: 60, Kn: 60}); !nearSlot(got, want) {
		t.Fatalf("an idle minute = %+v, want %+v", got, want)
	}
	tick(m, clock, cfg, printingPrinter, time.Minute)
	if got, want := pendingSlot(m, energyDay, "p1", 40), (slotAcc{Wh: 4, Obs: 120, On: 120, Met: 120, Kn: 120, Prt: 2}); !nearSlot(got, want) {
		t.Fatalf("an idle and a printing minute = %+v, want %+v", got, want)
	}
}

func TestThePrintShareOfTheSlotsEqualsThePrintRecord(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	for range 5 {
		tick(m, clock, cfg, idlePrinter, time.Minute)
	}
	for range 25 { // over two slot edges
		tick(m, clock, cfg, printingPrinter, time.Minute)
	}
	for range 5 {
		tick(m, clock, cfg, idlePrinter, time.Minute)
	}
	p := onePrintLeft(t, m)
	sum := pendingSum(m, "p1")
	if !near(p.EnergyWh, 50) || !near(sum.Prt, p.EnergyWh) || !near(sum.Wh, 70) || !near(sum.Kn, sum.Obs) {
		t.Fatalf("the print holds %v Wh, the slots %+v; want 50 Wh in both and 70 Wh in all", p.EnergyWh, sum)
	}
}

func TestADayFileKeepsThePrintShare(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	tick(m, clock, cfg, printingPrinter, time.Minute)
	mustSave(t, m)
	if rows := readDay(t, m, energyDay).Devices["p1"]; len(rows) != 1 || rows[0] != (slotRow{40, 4, 120, 120, 120, 0, 120, 2}) {
		t.Fatalf("day file rows = %v, want the split time and 2 Wh of print", rows)
	}

	restarted, _ := clockedModule(clock.now())
	restarted.configureData(m.dataDir)
	want := slotAcc{Wh: 4, Obs: 120, On: 120, Met: 120, Kn: 120, Prt: 2}
	if got := cachedSlotOf(t, restarted, energyDay, "p1", 40); !nearSlot(got, want) || !got.splitExact() {
		t.Fatalf("after a restart the slot reads %+v, want %+v", got, want)
	}
}

// A file of a build that did not keep the print share has six values a row.
const oldDayFile = `{"v":1,"day":"2026-11-21","devices":{"p1":[[40,4,120,120,120,0]]}}`

func TestAnOldRowHasNoPrintShare(t *testing.T) {
	m, _, _ := energyModule(t, energyStart)
	writeEnergyFile(t, dayPath(m.energyDir(), energyDay), oldDayFile)

	if rows := readDay(t, m, energyDay).Devices["p1"]; len(rows) != 1 || rows[0] != (slotRow{40, 4, 120, 120, 120, 0, 0, 0}) {
		t.Fatalf("rows = %v, want the six values and no split", rows)
	}
	got := cachedSlotOf(t, m, energyDay, "p1", 40)
	if want := (slotAcc{Wh: 4, Obs: 120, On: 120, Met: 120}); !nearSlot(got, want) || got.splitExact() {
		t.Fatalf("the old row reads %+v (exact %v); want %+v, not recorded", got, got.splitExact(), want)
	}
}

// The first or last second of an older build's recording is as old as the
// rest of it.
func TestAShortOldRowHasNoPrintShare(t *testing.T) {
	m, _, _ := energyModule(t, energyStart)
	writeEnergyFile(t, dayPath(m.energyDir(), energyDay), `{"v":1,"day":"2026-11-21","devices":{"p1":[[40,0.02,1,1,1,0]]}}`)

	if got := cachedSlotOf(t, m, energyDay, "p1", 40); got.splitExact() {
		t.Fatalf("an old row of one second counts as exact: %+v", got)
	}
	if !(slotAcc{Wh: 0.02, Obs: 0.5, On: 0.5, Met: 0.5, Kn: 0.5}).splitExact() {
		t.Fatal("a short row of this build is not exact")
	}
}

func TestARowBegunByAnOlderBuildIsNotExact(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	writeEnergyFile(t, dayPath(m.energyDir(), energyDay), oldDayFile)
	tick(m, clock, cfg, printingPrinter, 0)
	tick(m, clock, cfg, printingPrinter, time.Minute) // slot 40 again
	clock.set(time.Date(2026, 11, 21, 10, 16, 0, 0, time.UTC))
	m.updateDeviceUsage("p1", cfg, printingPrinter) // more than two minutes later: adds nothing
	tick(m, clock, cfg, printingPrinter, time.Minute)
	mustSave(t, m)

	rows := readDay(t, m, energyDay).Devices["p1"]
	if len(rows) != 2 || rows[0] != (slotRow{40, 6, 180, 180, 180, 0, 60, 2}) {
		t.Fatalf("rows = %v, want slot 40 with a split for 60 of 180 s", rows)
	}
	if got := cachedSlotOf(t, m, energyDay, "p1", 40); got.splitExact() {
		t.Fatalf("a slot an older build began counts as exact: %+v", got)
	}
	if got := cachedSlotOf(t, m, energyDay, "p1", 41); !got.splitExact() || !near(got.Wh, 2) || !near(got.Prt, 2) {
		t.Fatalf("a slot of this build is not exact: %+v", got)
	}
}

func TestAnOlderBuildReadsTheNewRows(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	tick(m, clock, cfg, idlePrinter, time.Minute)
	tick(m, clock, cfg, printingPrinter, time.Minute)
	mustSave(t, m)

	raw, err := os.ReadFile(dayPath(m.energyDir(), energyDay))
	if err != nil {
		t.Fatal(err)
	}
	// The day file as the build before read it.
	var old struct {
		V       int                     `json:"v"`
		Day     string                  `json:"day"`
		Devices map[string][][6]float64 `json:"devices"`
	}
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("the older build cannot read the file: %v: %s", err, raw)
	}
	if rows := old.Devices["p1"]; old.V != 1 || old.Day != energyDay || len(rows) != 1 || rows[0] != [6]float64{40, 4, 120, 120, 120, 0} {
		t.Fatalf("the older build reads v %d, day %q, rows %v", old.V, old.Day, old.Devices["p1"])
	}
}

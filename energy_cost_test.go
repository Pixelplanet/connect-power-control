package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// nightSlots is p1 with a night price and energy on both sides of 22:00 on
// the 19th: 100 Wh at 0.40 from 21:45 and 300 Wh at 0.20 from 22:00.
func nightSlots(t *testing.T) *module {
	t.Helper()
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	slot(m, "p1", utc(11, 19, 21, 45), 100)
	slot(m, "p1", utc(11, 19, 22, 0), 300)
	return m
}

func keptPrices(m *module) map[priceMemoKey]priceMemoEntry {
	m.prices.mu.Lock()
	defer m.prices.mu.Unlock()
	out := make(map[priceMemoKey]priceMemoEntry, len(m.prices.held))
	for k, v := range m.prices.held {
		out[k] = v
	}
	return out
}

// answers runs fn and fails when it does not come back, which is what a
// wait for a lock looks like.
func answers(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for the record", what)
	}
}

func TestMeanPriceIsTheRuleOfPrintOf(t *testing.T) {
	m := nightSlots(t)
	rec := printRecord("a.gcode", utc(11, 19, 21, 45), 30*time.Minute, 400)
	queuePrint(m, "p1", rec)
	listed := reportOf(t, m, "day", "2026-11-19", "", "p1").Prints[0]

	settings := m.energySettings()
	book := priceBook{tariffs: settings.Tariffs, loc: settings.location()}
	m.energy.io.Lock()
	price, night, ok := book.meanPrice(m.energyReaderLocked(), "p1", rec.StartedAt, *rec.EndedAt)
	m.energy.io.Unlock()
	// 100 Wh at 0.40 and 300 Wh at 0.20.
	if !ok || !near(price, 0.25) || !near(night, 0.75) {
		t.Fatalf("mean price %v with a night share of %v, ok %v; want 0.25 and 0.75", price, night, ok)
	}
	if got := roundCost(rec.EnergyWh / 1000 * price); got != *listed.Cost || round3(rec.EnergyWh*night) != listed.NightWh {
		t.Fatalf("the mean price gives %v, the report lists %v with %v Wh at night", got, *listed.Cost, listed.NightWh)
	}
	// A time with no slot is weighed by its seconds.
	m.energy.io.Lock()
	price, night, ok = book.meanPrice(m.energyReaderLocked(), "p2", utc(11, 19, 21, 45), utc(11, 19, 22, 15))
	m.energy.io.Unlock()
	if !ok || !near(price, 0.3) || !near(night, 0.5) {
		t.Fatalf("with no energy on record: %v, %v, %v; want 0.30 and a half", price, night, ok)
	}
}

func TestTheStatusPricesSessionAndPrints(t *testing.T) {
	m := nightSlots(t)
	done := printRecord("a.gcode", utc(11, 19, 21, 45), 30*time.Minute, 400)
	queuePrint(m, "p1", done)
	// Today 10 Wh by day, in the quarter hour before 10:00.
	slot(m, "p1", utc(11, 21, 9, 45), 10)
	usage := &DeviceUsage{
		Session:      &PowerSession{Active: true, StartedAt: utc(11, 19, 21, 45), EnergyWh: 410},
		CurrentPrint: &PrintPowerRecord{Active: true, FileName: "b.gcode", StartedAt: utc(11, 21, 9, 45), EnergyWh: 10, MeteredSec: 900},
		PrintHistory: []PrintPowerRecord{done},
	}

	priced := m.priceUsage("p1", usage, m.clock())
	if !priced.priceSet || priced.currency != "EUR" {
		t.Fatalf("price set %v, currency %q", priced.priceSet, priced.currency)
	}
	listed := reportOf(t, m, "day", "2026-11-19", "", "p1").Prints[0]
	if got := priced.history[0]; got.Cost == nil || *got.Cost != *listed.Cost || *got.Cost != 0.1 || got.FileName != "a.gcode" {
		t.Fatalf("the finished print costs %v, the Energy tab says %v", got.Cost, *listed.Cost)
	}
	// The running print: 10 Wh at 0.40.
	if got := priced.current; got == nil || got.Cost == nil || *got.Cost != 0.004 {
		t.Fatalf("the running print = %+v", got)
	}
	// The session: 100 Wh and 10 Wh at 0.40, 300 Wh at 0.20, so 410 Wh at
	// their mean.
	if got := priced.session; got == nil || got.Cost == nil || *got.Cost != 0.104 || !got.Active {
		t.Fatalf("the session = %+v", got)
	}
	// What the page is given equals the totals of the Energy tab.
	total := costOf(t, reportOf(t, m, "week", "", "", "p1").Total)
	if total != *priced.session.Cost {
		t.Fatalf("the week costs %v, the session that covers it %v", total, *priced.session.Cost)
	}
}

func TestTheStatusHasNoCostWithoutAPrice(t *testing.T) {
	m := reportModule(t, "UTC")
	slot(m, "p1", utc(11, 21, 9, 0), 100)
	usage := &DeviceUsage{
		Session:      &PowerSession{Active: true, StartedAt: utc(11, 21, 9, 0), EnergyWh: 100},
		CurrentPrint: &PrintPowerRecord{Active: true, StartedAt: utc(11, 21, 9, 0), EnergyWh: 100, MeteredSec: 900},
		PrintHistory: []PrintPowerRecord{printRecord("a.gcode", utc(11, 20, 9, 0), time.Hour, 50)},
	}
	priced := m.priceUsage("p1", usage, m.clock())
	if priced.priceSet || priced.currency != "EUR" || priced.session.Cost != nil || priced.current.Cost != nil || priced.history[0].Cost != nil {
		t.Fatalf("with no price: %+v", priced)
	}
	if len(keptPrices(m)) != 0 {
		t.Fatal("a price was kept with no price set")
	}
	// A printer with no usage answers nothing to price.
	if priced = m.priceUsage("p9", nil, m.clock()); priced.session != nil || priced.current != nil || priced.history != nil {
		t.Fatalf("with no usage: %+v", priced)
	}
}

func TestAPrintWithNoReadingHasNoCost(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	blind := printRecord("a.gcode", utc(11, 20, 9, 0), time.Hour, 0)
	blind.MeteredSec = 0
	zero := printRecord("b.gcode", utc(11, 20, 11, 0), time.Hour, 0)
	usage := &DeviceUsage{
		Session:      &PowerSession{Active: true, StartedAt: utc(11, 21, 9, 0)},
		PrintHistory: []PrintPowerRecord{blind, zero},
	}
	priced := m.priceUsage("p1", usage, m.clock())
	if priced.history[0].Cost != nil {
		t.Fatalf("a print with no reading costs %v", *priced.history[0].Cost)
	}
	// A measured zero is a cost of zero, and so is a session that used nothing.
	if c := priced.history[1].Cost; c == nil || *c != 0 {
		t.Fatalf("a print that measured 0 Wh costs %v", c)
	}
	if c := priced.session.Cost; c == nil || *c != 0 {
		t.Fatalf("a session with no energy costs %v", c)
	}
}

func TestAFlatPriceIsPricedWithoutTheRecordLock(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	spans := []pricedSpan{
		{started: utc(11, 20, 9, 0), ended: utc(11, 20, 10, 0), energyWh: 1000},
		{started: utc(11, 21, 9, 0), running: true, energyWh: 2000},
		{started: utc(1, 1, 0, 0), running: true, energyWh: 0.001}, // from before the 31 days that are read
	}
	m.energy.io.Lock()
	defer m.energy.io.Unlock()
	answers(t, "a flat price", func() {
		costs, priceSet, _ := m.priceRecords("p1", m.clock(), spans)
		if !priceSet || *costs[0] != 0.3 || *costs[1] != 0.6 || *costs[2] != 0.0001 {
			t.Errorf("costs = %v, %v, %v", *costs[0], *costs[1], *costs[2])
		}
	})
	if len(keptPrices(m)) != 0 {
		t.Fatal("a flat price was kept")
	}

	// A second price that counts from later leaves the time before it flat.
	changeEnergy(m, func(s *EnergySettings) {
		s.Tariffs = append(s.Tariffs, Tariff{From: utc(11, 21, 0, 0).Unix(), Price: 0.5})
	})
	answers(t, "a print before the price change", func() {
		if costs, _, _ := m.priceRecords("p1", m.clock(), spans[:1]); *costs[0] != 0.3 {
			t.Errorf("cost = %v", *costs[0])
		}
	})
}

func TestARunningPrintIsReadOncePerMinute(t *testing.T) {
	m := nightSlots(t)
	clock := &testClock{t: utc(11, 19, 22, 15)}
	m.now = clock.now
	running := []pricedSpan{{started: utc(11, 19, 21, 45), running: true, energyWh: 400}}

	costs, _, _ := m.priceRecords("p1", clock.now(), running)
	if *costs[0] != 0.1 || len(keptPrices(m)) != 1 {
		t.Fatalf("cost %v with %d prices kept", *costs[0], len(keptPrices(m)))
	}
	// Inside the minute the record is not looked at, also while it is held.
	clock.add(55 * time.Second)
	m.energy.io.Lock()
	answers(t, "a price that is kept", func() {
		if again, _, _ := m.priceRecords("p1", clock.now(), running); *again[0] != 0.1 {
			t.Errorf("cost = %v", *again[0])
		}
	})
	m.energy.io.Unlock()

	// After it the record is read again: more energy at night lowers the mean.
	slot(m, "p1", utc(11, 19, 22, 15), 600)
	clock.set(utc(11, 19, 22, 30))
	running[0].energyWh = 1000
	if costs, _, _ = m.priceRecords("p1", clock.now(), running); *costs[0] != 0.22 {
		t.Fatalf("after a minute the print costs %v, want 0.22", *costs[0])
	}
	if len(keptPrices(m)) != 1 {
		t.Fatalf("%d prices kept for one running print", len(keptPrices(m)))
	}
}

func TestAFinishedPrintIsReadOnce(t *testing.T) {
	m := nightSlots(t)
	done := []pricedSpan{{started: utc(11, 19, 21, 45), ended: utc(11, 19, 22, 15), energyWh: 400}}
	if costs, _, _ := m.priceRecords("p1", m.clock(), done); *costs[0] != 0.1 {
		t.Fatalf("cost = %v", *costs[0])
	}
	m.energy.io.Lock()
	answers(t, "a finished print", func() {
		if costs, _, _ := m.priceRecords("p1", m.clock().Add(3*time.Hour), done); *costs[0] != 0.1 {
			t.Errorf("cost = %v", *costs[0])
		}
	})
	m.energy.io.Unlock()
	// Another printer's print over the same time is its own.
	if costs, _, _ := m.priceRecords("p2", m.clock(), done); *costs[0] != 0.12 || len(keptPrices(m)) != 2 {
		t.Fatalf("p2, with no slot: cost %v, %d kept", *costs[0], len(keptPrices(m)))
	}
	// The kept prices are bounded.
	m.prices.mu.Lock()
	for i := range maxPriceMemo {
		m.prices.held[priceMemoKey{device: "x", started: int64(i)}] = priceMemoEntry{}
	}
	m.prices.mu.Unlock()
	other := []pricedSpan{{started: utc(11, 19, 22, 0), ended: utc(11, 19, 23, 0), energyWh: 1}}
	m.priceRecords("p1", m.clock(), other)
	if n := len(keptPrices(m)); n != 1 {
		t.Fatalf("%d prices kept after the bound was passed, want 1", n)
	}
}

func TestAPriceChangeForgetsKeptPrices(t *testing.T) {
	m, _ := energyManager(t)
	mustPutEnergy(t, m, `{"time_zone":"UTC","price":{"price":0.4,"night":true,"night_price":0.2,"night_start":22,"night_end":6}}`)
	slot(m, "p1", utc(11, 19, 21, 45), 100)
	slot(m, "p1", utc(11, 19, 22, 0), 300)
	done := []pricedSpan{{started: utc(11, 19, 21, 45), ended: utc(11, 19, 22, 15), energyWh: 400}}
	if costs, _, _ := m.priceRecords("p1", m.clock(), done); *costs[0] != 0.1 || len(keptPrices(m)) != 1 {
		t.Fatalf("cost %v, %d kept", *costs[0], len(keptPrices(m)))
	}

	mustPutEnergy(t, m, `{"price":{"price":1,"night":true,"night_price":0.5,"night_start":22,"night_end":6},"price_from":"start"}`)
	if n := len(keptPrices(m)); n != 0 {
		t.Fatalf("%d prices kept after the price changed", n)
	}
	if costs, _, _ := m.priceRecords("p1", m.clock(), done); *costs[0] != 0.25 {
		t.Fatalf("at the new price the print costs %v, want 0.25", *costs[0])
	}
	// Reading the settings again, as a start does, forgets them too.
	m.loadEnergy()
	if n := len(keptPrices(m)); n != 0 {
		t.Fatalf("%d prices kept after the settings were read again", n)
	}
	// With the price cleared there is no cost.
	mustPutEnergy(t, m, `{"clear_price":true}`)
	if costs, priceSet, _ := m.priceRecords("p1", m.clock(), done); priceSet || costs[0] != nil {
		t.Fatalf("with the price cleared: set %v, cost %v", priceSet, costs[0])
	}
}

// A print from before the last 31 days is read over its own time, like the
// Energy tab reads it.
func TestAnOldPrintIsPricedOverItsOwnTime(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	slot(m, "p1", utc(10, 1, 21, 45), 100)
	slot(m, "p1", utc(10, 1, 22, 0), 300)
	rec := printRecord("old.gcode", utc(10, 1, 21, 45), 30*time.Minute, 400)
	queuePrint(m, "p1", rec)
	listed := reportOf(t, m, "day", "2026-10-01", "", "p1").Prints[0]

	old := []pricedSpan{{started: rec.StartedAt, ended: *rec.EndedAt, energyWh: 400}}
	costs, _, _ := m.priceRecords("p1", m.clock(), old)
	// 100 Wh at 0.40 and 300 Wh at 0.20.
	if costs[0] == nil || *costs[0] != 0.1 || *costs[0] != *listed.Cost {
		t.Fatalf("the old print costs %v, the Energy tab says %v", costs[0], *listed.Cost)
	}
}

// A price that is read while the settings change is never one of the old
// settings: the change holds the record until it has forgotten the prices.
func TestAPriceReadDuringASettingsChangeIsNotKept(t *testing.T) {
	m := nightSlots(t)
	done := []pricedSpan{{started: utc(11, 19, 21, 45), ended: utc(11, 19, 22, 15), energyWh: 400}}

	m.energy.io.Lock()
	got := make(chan *float64, 1)
	go func() {
		costs, _, _ := m.priceRecords("p1", m.clock(), done)
		got <- costs[0]
	}()
	// The read has taken the old settings and waits for the record.
	time.Sleep(100 * time.Millisecond)
	changeEnergy(m, func(s *EnergySettings) {
		s.Tariffs = []Tariff{{Price: 1, Night: true, NightPrice: 0.5, NightStart: 22, NightEnd: 6}}
	})
	m.prices.reset()
	m.energy.io.Unlock()

	if cost := <-got; cost == nil || *cost != 0.25 {
		t.Fatalf("the print was priced by the old settings: %v", cost)
	}
	if costs, _, _ := m.priceRecords("p1", m.clock(), done); *costs[0] != 0.25 {
		t.Fatalf("a price of the old settings was kept: %v", *costs[0])
	}
}

func TestTheStatusKeepsItsFieldNames(t *testing.T) {
	cost := 0.25
	ended := utc(11, 20, 10, 0)
	rec := PrintPowerRecord{
		JobID: "7", FileName: "a.gcode", Active: true, Outcome: "finished", StartedAt: utc(11, 20, 9, 0), EndedAt: &ended,
		DurationSec: 3600, EnergyWh: 100, AvgWatts: 100, PeakWatts: 150, MeteredSec: 3600, EndEstimated: true, AwaySince: &ended,
	}
	raw, err := json.Marshal(statusResponse{
		DeviceID:          "p1",
		SessionUsage:      &pricedSession{PowerSession: PowerSession{Active: true, StartedAt: utc(11, 20, 9, 0), EndedAt: &ended, DurationSec: 3600, EnergyWh: 100, AvgWatts: 100, PeakWatts: 150}, Cost: &cost},
		CurrentPrintUsage: &pricedPrint{PrintPowerRecord: rec},
		PrintHistory:      []pricedPrint{{PrintPowerRecord: rec, Cost: &cost}},
		Currency:          "EUR",
		PriceSet:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Session map[string]any   `json:"session_usage"`
		Current map[string]any   `json:"current_print_usage"`
		History []map[string]any `json:"print_history"`
		Top     map[string]any   `json:"-"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &got.Top); err != nil {
		t.Fatal(err)
	}
	session := strings.Fields("active started_at ended_at duration_sec energy_wh avg_watts peak_watts cost")
	record := strings.Fields("job_id file_name active outcome started_at ended_at duration_sec energy_wh avg_watts peak_watts metered_sec end_estimated away_since cost")
	for name, part := range map[string]struct {
		have map[string]any
		want []string
	}{
		"session_usage":       {got.Session, session},
		"current_print_usage": {got.Current, record},
		"print_history":       {got.History[0], record},
		"the status":          {got.Top, strings.Fields("device_id mqtt_configured config can_turn_off printer_connected auto_off_pending default_cooldown_sec poll_interval_sec session_usage current_print_usage print_history currency price_set")},
	} {
		for _, key := range part.want {
			if _, ok := part.have[key]; !ok {
				t.Errorf("%s lacks %q: %s", name, key, raw)
			}
		}
		if name != "the status" && len(part.have) != len(part.want) {
			t.Errorf("%s has %d fields, want %d: %v", name, len(part.have), len(part.want), part.have)
		}
	}
	if got.Session["cost"] != 0.25 || got.Current["cost"] != nil || got.Top["currency"] != "EUR" || got.Top["price_set"] != true {
		t.Errorf("cost %v, cost of the running print %v, currency %v, price set %v", got.Session["cost"], got.Current["cost"], got.Top["currency"], got.Top["price_set"])
	}
}

package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// reportModule is a module with a data directory, no plug, and the clock at
// energyStart: Saturday 21 November 2026, 10:07 UTC.
func reportModule(t *testing.T, zone string) *module {
	t.Helper()
	m, _ := clockedModule(energyStart)
	m.setDataDir(t.TempDir())
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = zone })
	return m
}

func utc(month time.Month, day, hour, minute int) time.Time {
	return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
}

// slot records wh in the quarter hour that starts at at.
func slot(m *module, id string, at time.Time, wh float64) {
	recordSlots(m, id, at, 15*time.Minute, wh)
}

func pointHours(rep energyReport) []int {
	hours := make([]int, len(rep.Points))
	for i, p := range rep.Points {
		hours[i] = p.Hour
	}
	return hours
}

// --- Zones ---

func TestDaysFollowTheSiteZoneOnAUTCServer(t *testing.T) {
	// 23:30 UTC on the 20th is 00:30 on the 21st in Prague.
	m := reportModule(t, "Europe/Prague")
	slot(m, "p1", utc(11, 20, 23, 30), 500)

	rep := reportOf(t, m, "day", "2026-11-21", "", "p1")
	if len(rep.Points) != 24 || rep.Points[0].EnergyWh != 500 || rep.Points[0].Day != "2026-11-21" || rep.Total.EnergyWh != 500 {
		t.Fatalf("the Prague day of the 21st: %d points, first %+v, total %v", len(rep.Points), rep.Points[0], rep.Total.EnergyWh)
	}
	if !rep.From.Equal(utc(11, 20, 23, 0)) || !rep.To.Equal(utc(11, 21, 23, 0)) || rep.At != "2026-11-21" {
		t.Fatalf("the day runs from %v to %v", rep.From, rep.To)
	}
	if rep.TimeZone != "Europe/Prague" || !rep.ZoneSet || rep.UTCOffsetSec != 3600 {
		t.Fatalf("zone %q set %v offset %d", rep.TimeZone, rep.ZoneSet, rep.UTCOffsetSec)
	}
	if raw, _ := json.Marshal(rep.Points[0].Start); string(raw) != `"2026-11-21T00:00:00+01:00"` {
		t.Fatalf("a point starts at %s, want the site offset", raw)
	}
	if rep = reportOf(t, m, "day", "2026-11-20", "", "p1"); rep.Total.EnergyWh != 0 || rep.Total.ObservedSec != 0 {
		t.Fatalf("the Prague day of the 20th holds %+v", rep.Total)
	}

	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "UTC" })
	if rep = reportOf(t, m, "day", "2026-11-20", "", "p1"); rep.Points[23].EnergyWh != 500 || rep.Points[23].Hour != 23 {
		t.Fatalf("the UTC day of the 20th: hour 23 = %+v", rep.Points[23])
	}

	// With no zone set, the days follow the server clock, and the answer
	// says so.
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "" })
	_, offset := energyStart.In(time.Local).Zone()
	if rep = reportOf(t, m, "day", "", "", "p1"); rep.ZoneSet || rep.TimeZone != "" || rep.UTCOffsetSec != offset {
		t.Fatalf("with no zone: set %v, zone %q, offset %d, want the server offset %d", rep.ZoneSet, rep.TimeZone, rep.UTCOffsetSec, offset)
	}
}

func TestDaylightSavingDaysHave23And25Hours(t *testing.T) {
	m := reportModule(t, "Europe/Prague")
	// 29 March: the clock goes from 02:00 to 03:00.
	slot(m, "p1", utc(3, 29, 0, 30), 10) // 01:30 CET
	slot(m, "p1", utc(3, 29, 1, 0), 20)  // 03:00 CEST
	spring := reportOf(t, m, "day", "2026-03-29", "", "p1")
	if len(spring.Points) != 23 || spring.To.Sub(spring.From) != 23*time.Hour {
		t.Fatalf("29 March has %d hours over %v, want 23", len(spring.Points), spring.To.Sub(spring.From))
	}
	if hours := pointHours(spring); hours[1] != 1 || hours[2] != 3 || hours[22] != 23 {
		t.Fatalf("the hours of 29 March: %v", hours)
	}
	if spring.Points[1].EnergyWh != 10 || spring.Points[2].EnergyWh != 20 || spring.Total.EnergyWh != 30 {
		t.Fatalf("29 March: 01:00 holds %v, 03:00 holds %v", spring.Points[1].EnergyWh, spring.Points[2].EnergyWh)
	}

	// 25 October: the clock goes from 03:00 back to 02:00, so 02:00 comes twice.
	slot(m, "p1", utc(10, 25, 0, 15), 100) // 02:15 CEST
	slot(m, "p1", utc(10, 25, 1, 15), 200) // 02:15 CET
	autumn := reportOf(t, m, "day", "2026-10-25", "", "p1")
	if len(autumn.Points) != 25 || autumn.To.Sub(autumn.From) != 25*time.Hour {
		t.Fatalf("25 October has %d hours over %v, want 25", len(autumn.Points), autumn.To.Sub(autumn.From))
	}
	if hours := pointHours(autumn); hours[2] != 2 || hours[3] != 2 || hours[4] != 3 || hours[24] != 23 {
		t.Fatalf("the hours of 25 October: %v", hours)
	}
	if autumn.Points[2].EnergyWh != 100 || autumn.Points[3].EnergyWh != 200 || autumn.Total.EnergyWh != 300 {
		t.Fatalf("25 October: the first 02:00 holds %v, the second %v", autumn.Points[2].EnergyWh, autumn.Points[3].EnergyWh)
	}
	for i := 1; i < len(autumn.Points); i++ {
		if !autumn.Points[i].Start.After(autumn.Points[i-1].Start) {
			t.Fatalf("period %d does not start after the one before", i)
		}
	}
	// Both hours are 02:00 on the wall clock. The page tells them apart by
	// the offset their start is sent with.
	first, _ := json.Marshal(autumn.Points[2].Start)
	second, _ := json.Marshal(autumn.Points[3].Start)
	if string(first) != `"2026-10-25T02:00:00+02:00"` || string(second) != `"2026-10-25T02:00:00+01:00"` {
		t.Fatalf("the two 02:00 hours are sent as %s and %s", first, second)
	}

	// The month has every day once, also the long one.
	month := reportOf(t, m, "month", "2026-10-25", "", "p1")
	if len(month.Points) != 31 || month.Points[24].EnergyWh != 300 || month.Points[24].Day != "2026-10-25" || month.Points[25].Day != "2026-10-26" {
		t.Fatalf("October has %d days, the 25th holds %v", len(month.Points), month.Points[24].EnergyWh)
	}
	if year := reportOf(t, m, "year", "", "", "p1"); len(year.Points) != 12 || year.Points[2].EnergyWh != 30 || year.Points[9].EnergyWh != 300 || year.At != "2026-01-01" {
		t.Fatalf("the year has %d months: March %v, October %v", len(year.Points), year.Points[2].EnergyWh, year.Points[9].EnergyWh)
	}
	if week := reportOf(t, m, "week", "2026-10-25", "", "p1"); len(week.Points) != 7 || week.At != "2026-10-19" || week.Points[6].EnergyWh != 300 {
		t.Fatalf("the week starts %s with %d days, Sunday holds %v", week.At, len(week.Points), week.Points[6].EnergyWh)
	}
}

func TestHalfHourZonesCutDaysExactly(t *testing.T) {
	for zone, last := range map[string]time.Time{
		"Asia/Kolkata":   utc(11, 20, 18, 15), // +05:30: 23:45
		"Asia/Kathmandu": utc(11, 20, 18, 0),  // +05:45: 23:45
	} {
		t.Run(zone, func(t *testing.T) {
			m := reportModule(t, zone)
			slot(m, "p1", last, 1)
			slot(m, "p1", last.Add(15*time.Minute), 2) // 00:00 of the 21st

			before := reportOf(t, m, "day", "2026-11-20", "", "p1")
			after := reportOf(t, m, "day", "2026-11-21", "", "p1")
			if len(before.Points) != 24 || before.Total.EnergyWh != 1 || before.Points[23].EnergyWh != 1 {
				t.Fatalf("the 20th holds %v Wh, its last hour %v; want 1 and 1", before.Total.EnergyWh, before.Points[23].EnergyWh)
			}
			if after.Total.EnergyWh != 2 || after.Points[0].EnergyWh != 2 || after.Points[0].Hour != 0 {
				t.Fatalf("the 21st holds %v Wh, its first hour %v; want 2 and 2", after.Total.EnergyWh, after.Points[0].EnergyWh)
			}
			if !after.From.Equal(last.Add(15 * time.Minute)) {
				t.Fatalf("the 21st starts at %v", after.From.UTC())
			}
		})
	}
}

// --- Cost ---

func nightPrice(start, end int) Tariff {
	return Tariff{Price: 0.4, Night: true, NightPrice: 0.2, NightStart: start, NightEnd: end}
}

func TestNightHoursWrapMidnight(t *testing.T) {
	m := reportModule(t, "Europe/Prague")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	// Prague is UTC+1: 21:45, 22:00, 05:45 and 06:00 on the wall clock.
	slot(m, "p1", utc(11, 19, 20, 45), 1000)
	slot(m, "p1", utc(11, 19, 21, 0), 1000)
	slot(m, "p1", utc(11, 20, 4, 45), 1000)
	slot(m, "p1", utc(11, 20, 5, 0), 1000)

	rep := reportOf(t, m, "week", "2026-11-20", "", "p1")
	if rep.Total.NightWh != 2000 || costOf(t, rep.Total) != 1.2 || !rep.Night {
		t.Fatalf("night %v Wh, cost %v, night flag %v; want 2000, 1.2, true", rep.Total.NightWh, *rep.Total.Cost, rep.Night)
	}
	day := reportOf(t, m, "day", "2026-11-19", "", "p1")
	if day.Points[21].NightWh != 0 || day.Points[22].NightWh != 1000 || costOf(t, day.Points[21].energyFigures) != 0.4 || costOf(t, day.Points[22].energyFigures) != 0.2 {
		t.Fatalf("21:00 = %+v, 22:00 = %+v", day.Points[21].energyFigures, day.Points[22].energyFigures)
	}

	// The night follows the wall clock through a daylight-saving change: in
	// summer Prague is UTC+2, so 22:00 is 20:00 UTC.
	slot(m, "p1", utc(7, 1, 19, 45), 1000) // 21:45
	slot(m, "p1", utc(7, 1, 20, 0), 1000)  // 22:00
	if summer := reportOf(t, m, "day", "2026-07-01", "", "p1"); summer.Total.NightWh != 1000 || costOf(t, summer.Total) != 0.6 {
		t.Fatalf("a summer day: night %v Wh, cost %v; want 1000 and 0.6", summer.Total.NightWh, *summer.Total.Cost)
	}
}

func TestNightHoursInsideOneDay(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(1, 5)} })
	slot(m, "p1", utc(11, 20, 0, 45), 1000) // day
	slot(m, "p1", utc(11, 20, 1, 0), 1000)  // night
	slot(m, "p1", utc(11, 20, 4, 45), 1000) // night
	slot(m, "p1", utc(11, 20, 5, 0), 1000)  // day
	slot(m, "p1", utc(11, 20, 23, 0), 1000) // day

	rep := reportOf(t, m, "day", "2026-11-20", "", "p1")
	if rep.Total.NightWh != 2000 || costOf(t, rep.Total) != 1.6 {
		t.Fatalf("night %v Wh, cost %v; want 2000 and 1.6", rep.Total.NightWh, *rep.Total.Cost)
	}
}

func TestCostUsesTheTariffOfEachSlot(t *testing.T) {
	m := reportModule(t, "UTC")
	change := utc(11, 20, 12, 0)
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}, {From: change.Unix(), Price: 0.5}} })
	slot(m, "p1", utc(11, 20, 11, 45), 1000)
	slot(m, "p1", change, 1000)

	rep := reportOf(t, m, "month", "", "", "p1")
	if costOf(t, rep.Total) != 0.8 || !rep.PriceChanged || rep.Night {
		t.Fatalf("cost %v, price_changed %v, night %v", *rep.Total.Cost, rep.PriceChanged, rep.Night)
	}
	if costOf(t, rep.Points[19].energyFigures) != 0.8 || costOf(t, rep.Printers[0].energyFigures) != 0.8 {
		t.Fatalf("the 20th costs %v, the printer %v; want 0.8", *rep.Points[19].Cost, *rep.Printers[0].Cost)
	}
	day := reportOf(t, m, "day", "2026-11-20", "", "p1")
	if costOf(t, day.Points[11].energyFigures) != 0.3 || costOf(t, day.Points[12].energyFigures) != 0.5 {
		t.Fatalf("11:00 costs %v and 12:00 %v, want 0.3 and 0.5", *day.Points[11].Cost, *day.Points[12].Cost)
	}
	if before := reportOf(t, m, "day", "2026-11-19", "", "p1"); before.PriceChanged {
		t.Fatal("the day before the change says the price changed")
	}
}

func TestNoPriceGivesNoCost(t *testing.T) {
	m := reportModule(t, "UTC")
	slot(m, "p1", utc(11, 20, 12, 0), 1000)
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 20, 12, 0), 10*time.Minute, 400))

	rep := reportOf(t, m, "month", "", "", "p1")
	if rep.PriceSet || rep.Total.Cost != nil || rep.Points[19].Cost != nil || rep.Printers[0].Cost != nil || rep.Prints[0].Cost != nil || rep.PrintsCost != nil {
		t.Fatalf("a cost with no price: %+v", rep)
	}
	if rep.Total.EnergyWh != 1000 || rep.PrintsWh != 400 {
		t.Fatalf("energy must still be there: %v Wh, prints %v Wh", rep.Total.EnergyWh, rep.PrintsWh)
	}
	raw, _ := json.Marshal(rep)
	for _, want := range []string{`"total":{"energy_wh":1000,"night_wh":0,"cost":null,`, `"prints_cost":null`, `"previous":null`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the answer lacks %s", want)
		}
	}
}

// One print over the night edge and a price change. Its cost is the cost of
// the slots it ran in.
func TestAPrintIsPricedByItsOwnSlots(t *testing.T) {
	m := reportModule(t, "UTC")
	later := utc(11, 19, 22, 15)
	changeEnergy(m, func(s *EnergySettings) {
		s.Tariffs = []Tariff{
			{Price: 0.4, Night: true, NightPrice: 0.2, NightStart: 22, NightEnd: 6},
			{From: later.Unix(), Price: 0.6, Night: true, NightPrice: 0.1, NightStart: 22, NightEnd: 6},
		}
	})
	slot(m, "p1", utc(11, 19, 21, 30), 100) // day, 0.40
	slot(m, "p1", utc(11, 19, 21, 45), 100) // day, 0.40
	slot(m, "p1", utc(11, 19, 22, 0), 100)  // night, 0.20
	slot(m, "p1", later, 300)               // night at the later price, 0.10
	// Another printer's energy in the same slots has no say.
	slot(m, "p2", utc(11, 19, 21, 30), 5000)
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 19, 21, 30), time.Hour, 600))

	rep := reportOf(t, m, "day", "2026-11-19", "p1", "p1", "p2")
	if rep.PrintsTotal != 1 {
		t.Fatalf("%d prints, want 1", rep.PrintsTotal)
	}
	p := rep.Prints[0]
	if p.Cost == nil || *p.Cost != 0.13 || p.NightWh != 400 {
		t.Fatalf("the print costs %v with %v Wh at night; want 0.13 and 400", p.Cost, p.NightWh)
	}
	// The same rule as the totals: the print is all the printer used.
	if costOf(t, rep.Total) != *p.Cost || rep.Total.NightWh != p.NightWh || *rep.PrintsCost != *p.Cost {
		t.Fatalf("total %v with %v Wh at night, prints %v; the print says %v and %v", *rep.Total.Cost, rep.Total.NightWh, *rep.PrintsCost, *p.Cost, p.NightWh)
	}

	// A print that covers half a slot weighs half of that slot's energy.
	queuePrint(m, "p1", printRecord("b.gcode", utc(11, 19, 21, 52).Add(30*time.Second), 15*time.Minute, 100))
	rep = reportOf(t, m, "day", "2026-11-19", "p1", "p1")
	// 50 Wh at 0.40 and 50 Wh at 0.20 weigh the same: 0.30 for 0.1 kWh.
	// It ended before the first print, so it is the second in the list.
	if p = rep.Prints[1]; p.FileName != "b.gcode" || *p.Cost != 0.03 || p.NightWh != 50 {
		t.Fatalf("the second print: %+v cost %v", p, *p.Cost)
	}
}

func TestAPrintWithNoSlotEnergyIsSplitByTime(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	// 15 minutes of day and 15 minutes of night, and no slot on record.
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 19, 21, 45), 30*time.Minute, 100))
	// A print with no length takes the price of its moment.
	queuePrint(m, "p1", printRecord("b.gcode", utc(11, 19, 23, 0), 0, 100))

	rep := reportOf(t, m, "day", "2026-11-19", "", "p1")
	if rep.PrintsTotal != 2 {
		t.Fatalf("%d prints, want 2", rep.PrintsTotal)
	}
	if p := rep.Prints[1]; *p.Cost != 0.03 || p.NightWh != 50 {
		t.Fatalf("the print costs %v with %v Wh at night; want 0.03 and 50", *p.Cost, p.NightWh)
	}
	if p := rep.Prints[0]; *p.Cost != 0.02 || p.NightWh != 100 {
		t.Fatalf("the print with no length costs %v with %v Wh at night; want 0.02 and 100", *p.Cost, p.NightWh)
	}
}

func TestAZoneChangeMovesPrintsAndTotalsTogether(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(0, 6)} })
	at := utc(11, 20, 23, 30)
	slot(m, "p1", at, 100)
	queuePrint(m, "p1", printRecord("a.gcode", at, 15*time.Minute, 100))

	day := func(at string) energyReport { return reportOf(t, m, "day", at, "", "p1") }
	if a, b := day("2026-11-20"), day("2026-11-21"); a.Total.EnergyWh != 100 || a.PrintsTotal != 1 || b.Total.EnergyWh != 0 || b.PrintsTotal != 0 {
		t.Fatalf("in UTC: the 20th %v Wh and %d prints, the 21st %v Wh and %d prints", a.Total.EnergyWh, a.PrintsTotal, b.Total.EnergyWh, b.PrintsTotal)
	}
	if a := day("2026-11-20"); a.Total.NightWh != 0 || a.Prints[0].NightWh != 0 || a.Prints[0].EndedLocal != "2026-11-20T23:45:00" {
		t.Fatalf("in UTC it is day time: %+v, %+v", a.Total, a.Prints[0])
	}

	// In Prague the same moment is 00:30 on the 21st, in the night hours.
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "Europe/Prague" })
	a, b := day("2026-11-20"), day("2026-11-21")
	if a.Total.EnergyWh != 0 || a.PrintsTotal != 0 || b.Total.EnergyWh != 100 || b.PrintsTotal != 1 {
		t.Fatalf("in Prague: the 20th %v Wh and %d prints, the 21st %v Wh and %d prints", a.Total.EnergyWh, a.PrintsTotal, b.Total.EnergyWh, b.PrintsTotal)
	}
	if b.Total.NightWh != 100 || b.Prints[0].NightWh != 100 || *b.Total.Cost != *b.Prints[0].Cost || b.Prints[0].EndedLocal != "2026-11-21T00:45:00" {
		t.Fatalf("in Prague it is night: %+v, %+v", b.Total, b.Prints[0])
	}
}

// --- Who sees what ---

// twoPrinters has energy and a print for p1 and for p2 on the 20th.
func twoPrinters(t *testing.T) *module {
	t.Helper()
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	slot(m, "p1", utc(11, 20, 12, 0), 100)
	slot(m, "p2", utc(11, 20, 12, 0), 7000)
	slot(m, "p2", utc(10, 20, 12, 0), 7000) // the month before
	queuePrint(m, "p1", printRecord("one.gcode", utc(11, 20, 12, 0), 10*time.Minute, 40))
	queuePrint(m, "p2", printRecord("two.gcode", utc(11, 20, 12, 0), 10*time.Minute, 4000))
	return m
}

func exportOf(t *testing.T, m *module, q energyQuery, table string) string {
	t.Helper()
	q.now = m.clock()
	d, err := m.collectEnergy(q)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for offset := 0; ; {
		part := d.export(table, offset)
		out.WriteString(part.CSV)
		if offset = part.NextOffset; offset == 0 {
			return out.String()
		}
	}
}

func TestTheReportKeepsToVisiblePrinters(t *testing.T) {
	m := twoPrinters(t)
	rep := reportOf(t, m, "month", "", "", "p1")

	if rep.Total.EnergyWh != 100 || costOf(t, rep.Total) != 0.03 || rep.Points[19].EnergyWh != 100 {
		t.Fatalf("total %+v, the 20th %+v: another printer is in them", rep.Total, rep.Points[19].energyFigures)
	}
	if rep.Previous != nil {
		t.Fatalf("the period before holds %+v of a printer the caller may not see", *rep.Previous)
	}
	if len(rep.Printers) != 1 || rep.Printers[0].DeviceID != "p1" || rep.PrintersTotal != 1 || rep.Printers[0].Prints != 1 || rep.Printers[0].Name != "Printer p1" {
		t.Fatalf("printers = %+v", rep.Printers)
	}
	if len(rep.Prints) != 1 || rep.Prints[0].DeviceID != "p1" || rep.PrintsTotal != 1 || rep.PrintsWh != 40 {
		t.Fatalf("prints = %+v", rep.Prints)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "p2") || strings.Contains(string(raw), "two.gcode") {
		t.Fatalf("the answer names the unseen printer: %s", raw)
	}
	q := energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}}
	for _, table := range []string{"periods", "prints"} {
		text := exportOf(t, m, q, table)
		if strings.Contains(text, "p2") || strings.Contains(text, "two.gcode") || !strings.Contains(text, "Printer p1") {
			t.Fatalf("the %s table names the unseen printer, or lacks the seen one:\n%s", table, text)
		}
		if rows := strings.Count(text, "\r\n"); rows != 2 {
			t.Fatalf("the %s table has %d lines, want the header and one row", table, rows)
		}
	}

	// A caller who sees both gets both, and the filter narrows the figures
	// but not the list of printers.
	both := reportOf(t, m, "month", "", "p1", "p1", "p2")
	if both.Device != "p1" || both.Total.EnergyWh != 100 || len(both.Prints) != 1 || len(both.Printers) != 2 || both.Printers[0].DeviceID != "p2" {
		t.Fatalf("narrowed to p1: device %q, total %v, %d prints, printers %+v", both.Device, both.Total.EnergyWh, len(both.Prints), both.Printers)
	}
	all := reportOf(t, m, "month", "", "", "p1", "p2")
	if all.Total.EnergyWh != 7100 || all.PrintsTotal != 2 || all.Previous == nil || all.Previous.EnergyWh != 7000 {
		t.Fatalf("both printers: total %v, %d prints, before %+v", all.Total.EnergyWh, all.PrintsTotal, all.Previous)
	}
}

func TestAnUnseenPrinterFilterIsReadAsNoFilter(t *testing.T) {
	m := twoPrinters(t)
	none := reportOf(t, m, "month", "", "", "p1")
	unseen := reportOf(t, m, "month", "", "p2", "p1")
	unknown := reportOf(t, m, "month", "", "no-such-printer", "p1")
	if unseen.Device != "" || !reflect.DeepEqual(unseen, none) || !reflect.DeepEqual(unknown, none) {
		t.Fatalf("a filter for an unseen printer answers something of its own:\n%+v\n%+v", unseen, none)
	}
	q := energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}}
	plain := exportOf(t, m, q, "periods")
	q.device = "p2"
	if got := exportOf(t, m, q, "periods"); got != plain {
		t.Fatalf("the CSV for an unseen printer differs:\n%s\n%s", got, plain)
	}
}

func removedOf(t *testing.T, m *module, known ...string) int {
	t.Helper()
	q := energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}, now: m.clock()}
	if known != nil {
		q.known = map[string]bool{}
		for _, id := range known {
			q.known[id] = true
		}
	}
	rep, err := m.buildEnergyReport(q)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.EnergyWh != 100 || len(rep.Printers) != 1 {
		t.Fatalf("a printer that is not visible is in the figures: %+v", rep.Total)
	}
	return rep.Removed
}

func TestRemovedPrintersAreCountedNotShown(t *testing.T) {
	m := twoPrinters(t)
	// Connect lists p1 only: p2 has a record and is gone.
	if got := removedOf(t, m, "p1"); got != 1 {
		t.Fatalf("removed = %d, want 1", got)
	}
	// A printer with a record only in another period is not counted here.
	slot(m, "p3", utc(10, 20, 12, 0), 5)
	if got := removedOf(t, m, "p1"); got != 1 {
		t.Fatalf("removed = %d, want 1: p3 has no record this month", got)
	}
	// With no list of all printers nothing counts as removed.
	if got := removedOf(t, m); got != 0 {
		t.Fatalf("removed = %d with no list of printers, want 0", got)
	}
}

func TestAnUnseenListedPrinterIsNotCounted(t *testing.T) {
	m := twoPrinters(t)
	if got := removedOf(t, m, "p1", "p2"); got != 0 {
		t.Fatalf("removed = %d, want 0: p2 is listed, the caller only may not see it", got)
	}
}

// --- Earlier and later ---

func TestEarlierAndLaterStopAtTheRecords(t *testing.T) {
	m := reportModule(t, "UTC")
	if rep := reportOf(t, m, "month", "", "", "p1"); rep.Earliest != "" || rep.PrevAt != "" || rep.NextAt != "" || !rep.Recording {
		t.Fatalf("with no record: earliest %q, earlier %q, later %q", rep.Earliest, rep.PrevAt, rep.NextAt)
	}
	slot(m, "p1", utc(11, 10, 12, 0), 1)
	mustSave(t, m)
	slot(m, "p1", utc(11, 21, 9, 0), 1) // not saved yet

	for _, c := range []struct{ span, at, wantAt, prev, next string }{
		{"month", "", "2026-11-01", "", ""},
		{"day", "", "2026-11-21", "2026-11-20", ""},
		{"day", "2026-11-15", "2026-11-15", "2026-11-14", "2026-11-16"},
		{"day", "2026-11-11", "2026-11-11", "2026-11-10", "2026-11-12"},
		{"day", "2026-11-10", "2026-11-10", "", "2026-11-11"},
		{"day", "2027-01-01", "2026-11-21", "2026-11-20", ""}, // a day in the future is today
		{"week", "", "2026-11-16", "2026-11-09", ""},
		{"week", "2026-11-12", "2026-11-09", "", "2026-11-16"},
		{"year", "", "2026-01-01", "", ""},
		{"", "", "2026-11-01", "", ""}, // no span is a month
	} {
		rep := reportOf(t, m, c.span, c.at, "", "p1")
		if rep.At != c.wantAt || rep.PrevAt != c.prev || rep.NextAt != c.next || rep.Earliest != "2026-11-10" {
			t.Errorf("%s at %q: at %q, earlier %q, later %q, earliest %q; want %q, %q, %q", c.span, c.at, rep.At, rep.PrevAt, rep.NextAt, rep.Earliest, c.wantAt, c.prev, c.next)
		}
	}
	if rep := reportOf(t, m, "", "", "", "p1"); rep.Span != "month" || rep.Total.EnergyWh != 2 {
		t.Fatalf("span %q with %v Wh, want the month with what is saved and what is not", rep.Span, rep.Total.EnergyWh)
	}
}

func TestTheReportStopsAtTheKeepTime(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.KeepDays = 7 }) // kept from the 14th, 10:07
	slot(m, "p1", utc(11, 11, 12, 0), 100)
	slot(m, "p1", utc(11, 14, 10, 0), 100) // the slot the keep time cuts through starts before it
	slot(m, "p1", utc(11, 14, 10, 15), 5)
	slot(m, "p1", utc(11, 18, 12, 0), 5)
	queuePrint(m, "p1", printRecord("old.gcode", utc(11, 11, 12, 0), time.Minute, 1))
	queuePrint(m, "p1", printRecord("new.gcode", utc(11, 18, 12, 0), time.Minute, 1))

	rep := reportOf(t, m, "month", "", "", "p1")
	if rep.Total.EnergyWh != 10 || rep.Earliest != "2026-11-14" || rep.KeepDays != 7 {
		t.Fatalf("total %v Wh, earliest %q; want 10 and 2026-11-14", rep.Total.EnergyWh, rep.Earliest)
	}
	if rep.PrintsTotal != 1 || rep.Prints[0].FileName != "new.gcode" || rep.Printers[0].Prints != 1 {
		t.Fatalf("prints = %+v", rep.Prints)
	}
	if day := reportOf(t, m, "day", "2026-11-14", "", "p1"); day.PrevAt != "" || day.Total.EnergyWh != 5 {
		t.Fatalf("the day of the keep time: earlier %q, %v Wh", day.PrevAt, day.Total.EnergyWh)
	}
}

// at may name any day back to the year 0, and any viewer may send it. A
// report reads no day before the keep time or before the first record, and
// memory keeps nothing for a day that has no file.
func TestAReportOfAFarYearReadsNothing(t *testing.T) {
	m := reportModule(t, "UTC")
	reportOf(t, m, "year", "1000-06-01", "", "p1")
	reportOf(t, m, "year", "", "", "p1")
	if cached := cachedDays(m); len(cached) != 0 {
		t.Fatalf("with no record, %d days are kept in memory", len(cached))
	}

	slot(m, "p1", utc(11, 20, 12, 0), 100)
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 20, 12, 0), 10*time.Minute, 40))
	mustSave(t, m)
	for year := 1000; year < 1020; year++ {
		at := fmt.Sprintf("%d-06-01", year)
		rep := reportOf(t, m, "year", at, "", "p1")
		if rep.At != fmt.Sprintf("%d-01-01", year) || rep.Total.ObservedSec != 0 || rep.Previous != nil || rep.PrintsTotal != 0 || rep.NextAt == "" {
			t.Fatalf("the year %d: at %q, total %+v, %d prints", year, rep.At, rep.Total, rep.PrintsTotal)
		}
		q := energyQuery{span: "year", at: at, visible: map[string]string{"p1": "Printer p1"}}
		if text := exportOf(t, m, q, "periods"); strings.Count(text, "\r\n") != 1 {
			t.Fatalf("the CSV of the year %d has rows: %q", year, text)
		}
	}
	// The years with the record read its one file, and the days around it
	// that have none.
	for _, at := range []string{"", "2025-06-01", "2026-10-20"} {
		reportOf(t, m, "year", at, "", "p1")
		reportOf(t, m, "month", at, "", "p1")
	}
	if rep := reportOf(t, m, "year", "", "", "p1"); rep.Total.EnergyWh != 100 || rep.PrintsTotal != 1 {
		t.Fatalf("this year holds %v Wh and %d prints, want 100 and 1", rep.Total.EnergyWh, rep.PrintsTotal)
	}
	if cached := cachedDays(m); len(cached) != 1 || !cached["2026-11-20"] {
		t.Fatalf("memory keeps %v, want the one day with a file", cached)
	}
}

func TestThePeriodBeforeIsNullWithoutRecords(t *testing.T) {
	m := reportModule(t, "UTC")
	slot(m, "p1", utc(11, 20, 12, 0), 100)
	if rep := reportOf(t, m, "month", "", "", "p1"); rep.Previous != nil {
		t.Fatalf("the period before = %+v, want null", *rep.Previous)
	}

	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	slot(m, "p1", utc(10, 31, 23, 45), 1000)
	rep := reportOf(t, m, "month", "", "", "p1")
	if rep.Previous == nil || rep.Previous.EnergyWh != 1000 || costOf(t, *rep.Previous) != 0.3 || rep.Previous.ObservedSec != 900 || rep.Total.EnergyWh != 100 {
		t.Fatalf("the period before = %+v, total %v", rep.Previous, rep.Total.EnergyWh)
	}
	// A plug that was off the whole period before is a record, of zero.
	m = reportModule(t, "UTC")
	m.mu.Lock()
	m.recordEnergyLocked("p1", utc(11, 20, 12, 0), utc(11, 20, 12, 15), energyStep{observed: true})
	m.mu.Unlock()
	if rep = reportOf(t, m, "day", "", "", "p1"); rep.Previous == nil || rep.Previous.EnergyWh != 0 || rep.Previous.OnSec != 0 || rep.Previous.ObservedSec != 900 {
		t.Fatalf("a day with the plug off = %+v", rep.Previous)
	}
}

func TestEnergyRoutesRefuseABadQuery(t *testing.T) {
	m := guardedModule(t, "user:viewer", &accessFake{})
	for _, path := range []string{
		"/energy?span=decade",
		"/energy?at=yesterday",
		"/energy?at=2026-13-01",
		"/energy?at=21.11.2026",
		"/energy/export?span=hour",
		"/energy/export?table=printers",
		"/energy/export?table=bogus",
		"/energy/export?offset=-1",
		"/energy/export?offset=abc",
		"/energy/export?as_of=abc",
		"/energy/export?as_of=0",
		"/energy/export?offset=2000&as_of=1000",        // long ago
		"/energy/export?offset=2000&as_of=99999999999", // not yet
	} {
		rec := serve(m, "GET", path, "")
		if rec.Code != http.StatusBadRequest || !strings.HasSuffix(problemCode(t, rec), "INVALID_INPUT") {
			t.Errorf("%s: status %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	if _, err := m.buildEnergyReport(energyQuery{span: "decade", now: energyStart}); err == nil {
		t.Error("buildEnergyReport took the span decade")
	}
	if _, err := m.buildEnergyReport(energyQuery{at: "soon", now: energyStart}); err == nil {
		t.Error("buildEnergyReport took the day soon")
	}
	// A query that is in order is read as it was written.
	req, err := http.NewRequest("GET", "/energy/export?span=week&at=2026-11-20&device=p1&table=prints&offset=4000", nil)
	if err != nil {
		t.Fatal(err)
	}
	q, table, offset, err := energyQueryOf(req)
	if err != nil || q.span != "week" || q.at != "2026-11-20" || q.device != "p1" || table != "prints" || offset != 4000 {
		t.Errorf("read %+v, table %q, offset %d: %v", q, table, offset, err)
	}
	if req, _ = http.NewRequest("GET", "/energy/export?table=files", nil); req != nil {
		if _, table, _, err = energyQueryOf(req); err != nil || table != "files" {
			t.Errorf("table=files: table %q: %v", table, err)
		}
	}
	if req, _ = http.NewRequest("GET", "/energy", nil); req != nil {
		if _, table, offset, err = energyQueryOf(req); err != nil || table != "periods" || offset != 0 {
			t.Errorf("with no query: table %q, offset %d: %v", table, offset, err)
		}
	}
}

func TestThePrintListIsCappedAndCounted(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.5}} })
	slot(m, "p1", utc(11, 20, 12, 0), 1)
	first := utc(11, 2, 0, 0)
	for i := range 250 {
		queuePrint(m, "p1", printRecord(fmt.Sprintf("part-%03d.gcode", i), first.Add(time.Duration(i)*time.Hour), 30*time.Minute, 10))
	}

	rep := reportOf(t, m, "month", "", "", "p1")
	if len(rep.Prints) != reportPrintLimit || rep.PrintsTotal != 250 || rep.PrintsWh != 2500 || *rep.PrintsCost != 1.25 || rep.Printers[0].Prints != 250 {
		t.Fatalf("%d prints shown of %d, %v Wh, cost %v", len(rep.Prints), rep.PrintsTotal, rep.PrintsWh, *rep.PrintsCost)
	}
	if rep.Prints[0].FileName != "part-249.gcode" || rep.Prints[199].FileName != "part-050.gcode" {
		t.Fatalf("the newest is %s and the last shown %s", rep.Prints[0].FileName, rep.Prints[199].FileName)
	}
	// The CSV has them all.
	q := energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}}
	if rows := strings.Count(exportOf(t, m, q, "prints"), "\r\n"); rows != 251 {
		t.Fatalf("the CSV has %d lines, want the header and 250 prints", rows)
	}
}

// --- CSV ---

func csvOf(t *testing.T, text string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not parse: %v:\n%s", err, text)
	}
	return rows
}

func TestCSVColumnsAndQuoting(t *testing.T) {
	m := reportModule(t, "Europe/Prague")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	slot(m, "p1", utc(11, 20, 12, 0), 1500)
	slot(m, "p1", utc(11, 20, 22, 0), 500) // 23:00 in Prague: night
	queuePrint(m, "p1", PrintPowerRecord{
		JobID: "42", FileName: `bracket "v2", final.gcode`, Outcome: "finished",
		StartedAt: utc(11, 20, 12, 0), EndedAt: timePtr(utc(11, 20, 12, 15)),
		DurationSec: 900, EnergyWh: 1500, MeteredSec: 900, PeakWatts: 212.34,
	})
	q := energyQuery{span: "month", visible: map[string]string{"p1": `Bay 3, "left"`}, now: m.clock()}
	d, err := m.collectEnergy(q)
	if err != nil {
		t.Fatal(err)
	}

	periods := d.export("periods", 0)
	if periods.FileName != "power-energy-month-2026-11-01.csv" || periods.NextOffset != 0 {
		t.Fatalf("export = %q next %d", periods.FileName, periods.NextOffset)
	}
	if !strings.Contains(periods.CSV, `"Bay 3, ""left"""`) || strings.Count(periods.CSV, "\r\n") != 2 || strings.Count(periods.CSV, "\n") != 2 {
		t.Fatalf("quoting or line ends are wrong:\n%q", periods.CSV)
	}
	rows := csvOf(t, periods.CSV)
	if !reflect.DeepEqual(rows[0], strings.Split("period_start,period_end,printer,printer_id,energy_kwh,night_energy_kwh,cost,currency,plug_on_hours,measured_hours,observed_hours", ",")) {
		t.Fatalf("header = %v", rows[0])
	}
	want := []string{"2026-11-20T00:00:00+01:00", "2026-11-21T00:00:00+01:00", `Bay 3, "left"`, "p1", "2.0000", "0.5000", "0.7000", "EUR", "0.50", "0.50", "0.50"}
	if !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("row = %v\nwant  %v", rows[1], want)
	}

	prints := d.export("prints", 0)
	if prints.FileName != "power-prints-month-2026-11-01.csv" {
		t.Fatalf("export = %q", prints.FileName)
	}
	rows = csvOf(t, prints.CSV)
	if !reflect.DeepEqual(rows[0], strings.Split("started,ended,printer,printer_id,file,job_id,outcome,duration_s,energy_kwh,night_energy_kwh,cost,currency,peak_w,measured_s,end_estimated", ",")) {
		t.Fatalf("header = %v", rows[0])
	}
	want = []string{"2026-11-20T13:00:00+01:00", "2026-11-20T13:15:00+01:00", `Bay 3, "left"`, "p1", `bracket "v2", final.gcode`, "42", "finished", "900", "1.5000", "0.0000", "0.6000", "EUR", "212.3", "900", "false"}
	if !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("row = %v\nwant  %v", rows[1], want)
	}

	// The day table has a row for each hour with a record.
	q.span, q.at = "day", "2026-11-20"
	if d, err = m.collectEnergy(q); err != nil {
		t.Fatal(err)
	}
	rows = csvOf(t, d.export("periods", 0).CSV)
	if len(rows) != 3 || rows[1][0] != "2026-11-20T13:00:00+01:00" || rows[1][1] != "2026-11-20T14:00:00+01:00" || rows[2][0] != "2026-11-20T23:00:00+01:00" || rows[2][5] != "0.5000" {
		t.Fatalf("day rows = %v", rows)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestCSVGuardsFormulaCells(t *testing.T) {
	m := reportModule(t, "UTC")
	names := []string{"=HYPERLINK(\"http://x\")", "+1", "-1", "@SUM(A1)", "\tx", "\rx", "benchy.gcode", ""}
	for i, name := range names {
		rec := printRecord(name, utc(11, 20, 12, i), time.Minute, 1)
		rec.JobID, rec.Outcome = "=1+1", "-failed"
		queuePrint(m, "p1", rec)
	}
	slot(m, "=p", utc(11, 20, 12, 0), 1)
	q := energyQuery{span: "month", visible: map[string]string{"p1": "=cmd|' /C calc'!A0", "=p": "@printer"}}

	rows := csvOf(t, exportOf(t, m, q, "prints"))
	if len(rows) != len(names)+1 {
		t.Fatalf("%d rows, want %d", len(rows), len(names)+1)
	}
	for i, row := range rows[1:] {
		name := names[len(names)-1-i] // newest first
		want := name
		if name != "" && name != "benchy.gcode" {
			// The CSV writer leaves a lone carriage return out of a cell.
			want = "'" + strings.ReplaceAll(name, "\r", "")
		}
		if row[4] != want {
			t.Errorf("file %q is written as %q, want %q", name, row[4], want)
		}
		if row[2] != "'=cmd|' /C calc'!A0" || row[5] != "'=1+1" || row[6] != "'-failed" {
			t.Errorf("printer, job or outcome is not guarded: %q", row)
		}
		if row[8] != "0.0010" || row[12] != "150.0" {
			t.Errorf("a number cell changed: %q", row)
		}
	}
	rows = csvOf(t, exportOf(t, m, q, "periods"))
	if len(rows) != 2 || rows[1][2] != "'@printer" || rows[1][3] != "'=p" {
		t.Fatalf("periods rows = %q", rows)
	}

	// Changing the label never changes a figure, and a label is guarded too.
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 1}}; s.Currency = "=EUR" })
	if rows = csvOf(t, exportOf(t, m, q, "periods")); rows[1][6] != "0.0010" || rows[1][7] != "'=EUR" {
		t.Fatalf("periods rows = %q", rows)
	}
}

func TestCSVLeavesUnmeasuredAndUnpricedCellsEmpty(t *testing.T) {
	m := reportModule(t, "UTC")
	at := utc(11, 20, 12, 0)
	slot(m, "measured", at, 250)
	m.mu.Lock()
	m.recordEnergyLocked("nometer", at, at.Add(15*time.Minute), energyStep{observed: true, on: true})
	m.recordEnergyLocked("off", at, at.Add(15*time.Minute), energyStep{observed: true})
	m.mu.Unlock()
	unmetered := printRecord("blind.gcode", at, 10*time.Minute, 0)
	unmetered.MeteredSec = 0
	queuePrint(m, "nometer", unmetered)
	queuePrint(m, "measured", printRecord("seen.gcode", at, 10*time.Minute, 0))
	q := energyQuery{span: "month", visible: map[string]string{"measured": "A", "nometer": "B", "off": "C"}}

	cells := func(table string) map[string][]string {
		out := map[string][]string{}
		for _, row := range csvOf(t, exportOf(t, m, q, table))[1:] {
			out[row[3]] = row
		}
		return out
	}
	// energy_kwh, night_energy_kwh, cost, currency are columns 4 to 7.
	rows := cells("periods")
	for id, want := range map[string][]string{
		"measured": {"0.2500", "", "", ""},
		"nometer":  {"", "", "", ""},
		"off":      {"0.0000", "", "", ""},
	} {
		if got := rows[id][4:8]; !reflect.DeepEqual(got, want) {
			t.Errorf("with no price, %s: %q, want %q", id, got, want)
		}
	}
	if rows["nometer"][8] != "0.25" || rows["nometer"][9] != "0.00" || rows["off"][8] != "0.00" || rows["off"][10] != "0.25" {
		t.Errorf("hours: no meter %q, off %q", rows["nometer"][8:], rows["off"][8:])
	}
	prints := cells("prints")
	if got := prints["nometer"][8:12]; !reflect.DeepEqual(got, []string{"", "", "", ""}) {
		t.Errorf("a print that was not measured: %q", got)
	}
	if got := prints["measured"][8:12]; !reflect.DeepEqual(got, []string{"0.0000", "", "", ""}) {
		t.Errorf("a print that measured zero: %q", got)
	}

	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	rows = cells("periods")
	for id, want := range map[string][]string{
		"measured": {"0.2500", "0.0000", "0.1000", "EUR"},
		"nometer":  {"", "", "", ""},
		"off":      {"0.0000", "0.0000", "0.0000", "EUR"},
	} {
		if got := rows[id][4:8]; !reflect.DeepEqual(got, want) {
			t.Errorf("with a price, %s: %q, want %q", id, got, want)
		}
	}
	// The same three facts are in the report.
	rep := reportOf(t, m, "month", "", "nometer", "measured", "nometer", "off")
	if rep.Total.ObservedSec != 900 || rep.Total.OnSec != 900 || rep.Total.MeteredSec != 0 || rep.Total.EnergyWh != 0 {
		t.Errorf("no meter: %+v", rep.Total)
	}
	if rep = reportOf(t, m, "month", "", "off", "measured", "nometer", "off"); rep.Total.ObservedSec != 900 || rep.Total.OnSec != 0 {
		t.Errorf("off: %+v", rep.Total)
	}
	if rep = reportOf(t, m, "day", "2026-11-19", "", "measured"); rep.Total.ObservedSec != 0 {
		t.Errorf("a day with no record: %+v", rep.Total)
	}
}

// manyPrints queues n prints of p1 on the 2nd and after, a minute apart.
func manyPrints(m *module, n int, file string) {
	first := utc(11, 2, 0, 0)
	for i := range n {
		queuePrint(m, "p1", printRecord(fmt.Sprintf("%s-%04d", file, i), first.Add(time.Duration(i)*time.Minute), 30*time.Second, 1))
	}
}

func TestCSVPartsJoinToTheWholeFile(t *testing.T) {
	m := reportModule(t, "UTC")
	manyPrints(m, 4500, "part")
	d, err := m.collectEnergy(energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}, now: m.clock()})
	if err != nil {
		t.Fatal(err)
	}
	header, all := d.csvRows("prints")
	whole := csvLine(header)
	for _, row := range all {
		whole += csvLine(row)
	}

	var joined strings.Builder
	var offsets []int
	for offset := 0; ; {
		part := d.export("prints", offset)
		if rows := strings.Count(part.CSV, "\r\n"); rows > exportChunkRows+1 {
			t.Fatalf("the part at %d has %d rows", offset, rows)
		}
		if (offset == 0) != strings.HasPrefix(part.CSV, "started,ended,") {
			t.Fatalf("the part at %d starts with %.20q", offset, part.CSV)
		}
		joined.WriteString(part.CSV)
		offsets = append(offsets, offset)
		if offset = part.NextOffset; offset == 0 {
			break
		}
	}
	if !reflect.DeepEqual(offsets, []int{0, 2000, 4000}) {
		t.Fatalf("the parts start at %v, want 0, 2000 and 4000", offsets)
	}
	if joined.String() != whole || len(csvOf(t, whole)) != 4501 {
		t.Fatalf("the parts do not join to the whole file: %d bytes against %d", joined.Len(), len(whole))
	}
	// Past the end there is nothing, and it is the last part.
	if part := d.export("prints", 99999); part.CSV != "" || part.NextOffset != 0 {
		t.Fatalf("past the end: %q next %d", part.CSV, part.NextOffset)
	}
}

// A download asks for its parts one after the other. What is recorded
// between two parts must not move the rows, or the joined file lists a row
// twice.
func TestCSVPartsAreReadForOneMoment(t *testing.T) {
	m, clock := clockedModule(energyStart)
	m.setDataDir(t.TempDir())
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "UTC" })
	manyPrints(m, 2500, "part")
	slot(m, "p1", utc(11, 20, 12, 0), 1)
	visible := map[string]string{"p1": "Printer p1", "p2": "Printer p2"}
	partOf := func(table string, offset int, asOf int64) (energyExport, string) {
		t.Helper()
		q := energyQuery{span: "month", visible: visible, asOf: asOf}
		if err := exportMoment(&q, clock.now()); err != nil {
			t.Fatal(err)
		}
		d, err := m.collectEnergy(q)
		if err != nil {
			t.Fatal(err)
		}
		header, rows := d.csvRows(table)
		whole := csvLine(header)
		for _, row := range rows {
			whole += csvLine(row)
		}
		return d.export(table, offset), whole
	}

	first, whole := partOf("prints", 0, 0)
	periods, _ := partOf("periods", 0, 0)
	if first.NextOffset != 2000 || first.AsOf != energyStart.Unix() || len(csvOf(t, whole)) != 2501 {
		t.Fatalf("the first part: next %d, as_of %d, %d lines in the whole file", first.NextOffset, first.AsOf, len(csvOf(t, whole)))
	}

	// Meanwhile a print ends, and another printer gets its first record.
	clock.add(20 * time.Minute)
	queuePrint(m, "p1", printRecord("late.gcode", clock.now().Add(-5*time.Minute), time.Minute, 1))
	slot(m, "p2", utc(11, 21, 10, 15), 5)

	second, _ := partOf("prints", first.NextOffset, first.AsOf)
	if second.AsOf != first.AsOf || second.NextOffset != 0 || first.CSV+second.CSV != whole {
		t.Fatalf("the parts do not join to the file of the first moment: as_of %d, next %d", second.AsOf, second.NextOffset)
	}
	if again, _ := partOf("periods", 0, first.AsOf); again.CSV != periods.CSV {
		t.Fatalf("the periods moved:\n%s\n%s", periods.CSV, again.CSV)
	}

	// A new download holds both.
	fresh, now := partOf("prints", 0, 0)
	if !strings.Contains(fresh.CSV, "late.gcode") || len(csvOf(t, now)) != 2502 {
		t.Fatalf("a new download lacks the print that ended: %d lines", len(csvOf(t, now)))
	}
	if fresh, _ = partOf("periods", 0, 0); !strings.Contains(fresh.CSV, "Printer p2") {
		t.Fatalf("a new download lacks the new printer: %s", fresh.CSV)
	}

	// as_of is the moment of a first part: not in the future, and not old.
	for _, asOf := range []int64{clock.now().Unix() + 1, clock.now().Add(-2 * time.Hour).Unix()} {
		q := energyQuery{asOf: asOf}
		if err := exportMoment(&q, clock.now()); err == nil {
			t.Errorf("as_of %d was taken at %d", asOf, clock.now().Unix())
		}
	}
}

func TestACSVPartStaysUnderTheHostBound(t *testing.T) {
	m := reportModule(t, "UTC")
	// A character like < takes six bytes in the JSON answer, and a file
	// name is cut to 512 characters.
	manyPrints(m, 1500, strings.Repeat("<", 600))
	d, err := m.collectEnergy(energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}, now: m.clock()})
	if err != nil {
		t.Fatal(err)
	}
	rows, parts := 0, 0
	for offset := 0; ; {
		part := d.export("prints", offset)
		var body bytes.Buffer
		if err = json.NewEncoder(&body).Encode(part); err != nil {
			t.Fatal(err)
		}
		if body.Len() >= 1<<20 {
			t.Fatalf("the part at %d is %d bytes in the answer, over the host's 1 MiB", offset, body.Len())
		}
		if body.Len() < 300<<10 && part.NextOffset != 0 {
			t.Fatalf("the part at %d is only %d bytes", offset, body.Len())
		}
		rows += strings.Count(part.CSV, "\r\n")
		parts++
		if offset = part.NextOffset; offset == 0 {
			break
		}
	}
	if rows != 1501 || parts < 3 {
		t.Fatalf("%d rows in %d parts, want 1501 rows in several parts", rows, parts)
	}
}

func TestAnInterruptedPrintIsNamedInTheCSV(t *testing.T) {
	m := reportModule(t, "UTC")
	rec := printRecord("benchy.gcode", utc(11, 20, 12, 0), time.Minute, 1)
	rec.Outcome, rec.EndEstimated = "interrupted", true
	queuePrint(m, "p1", rec)

	rows := csvOf(t, exportOf(t, m, energyQuery{span: "month", visible: map[string]string{"p1": "Printer"}}, "prints"))
	if len(rows) != 2 || rows[1][6] != "interrupted" || rows[1][14] != "true" {
		t.Fatalf("rows = %q; want one print with the outcome interrupted", rows)
	}
	if rep := reportOf(t, m, "month", "", "", "p1"); len(rep.Prints) != 1 || rep.Prints[0].Outcome != "interrupted" {
		t.Fatalf("report prints = %+v", rep.Prints)
	}
}

// --- Printing against idle ---

// splitSlot records wh in the quarter hour at at, of which prt went into a
// print, the way this build records it.
func splitSlot(m *module, id string, at time.Time, wh, prt float64) {
	half := 450 * time.Second
	m.mu.Lock()
	m.recordEnergyLocked(id, at, at.Add(half), energyStep{wh: prt, observed: true, on: true, metered: true, printing: true})
	m.recordEnergyLocked(id, at.Add(half), at.Add(2*half), energyStep{wh: wh - prt, observed: true, on: true, metered: true})
	m.mu.Unlock()
}

// oldSlot records wh in the quarter hour at at the way a build before the
// split did: with no print share.
func oldSlot(m *module, id string, at time.Time, wh float64) {
	slot(m, id, at, wh)
	at = at.UTC()
	m.mu.Lock()
	acc := &m.energy.pending[at.Format(dayLayout)][id][at.Hour()*4+at.Minute()/15]
	acc.Kn, acc.Prt = 0, 0
	m.mu.Unlock()
}

func addsUp(f energyFigures) bool {
	return round3(f.PrintWh+f.IdleWh+f.SplitUnknownWh) == f.EnergyWh && f.PrintWh >= 0 && f.IdleWh >= 0 && f.SplitUnknownWh >= 0
}

func TestIdleIsWhatNoPrintUsed(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	splitSlot(m, "p1", utc(11, 19, 21, 45), 100, 60) // day, 0.40: 40 Wh idle
	splitSlot(m, "p1", utc(11, 19, 22, 0), 200, 50)  // night, 0.20: 150 Wh idle
	slot(m, "p1", utc(11, 19, 23, 0), 10)            // no print at all

	total := reportOf(t, m, "day", "2026-11-19", "", "p1").Total
	if total.EnergyWh != 310 || total.PrintWh != 110 || total.IdleWh != 200 || total.SplitUnknownWh != 0 || total.SplitEstimated {
		t.Fatalf("total = %+v; want 110 Wh printing and 200 Wh idle, nothing estimated", total)
	}
	// 40 Wh at 0.40, 150 Wh and 10 Wh at 0.20.
	if total.IdleCost == nil || *total.IdleCost != 0.048 {
		t.Fatalf("idle cost = %v, want 0.048", total.IdleCost)
	}
	// The same on a point and on the printer's row.
	rep := reportOf(t, m, "day", "2026-11-19", "", "p1")
	if p := rep.Points[21]; p.PrintWh != 60 || p.IdleWh != 40 || *p.IdleCost != 0.016 {
		t.Fatalf("21:00 = %+v", p.energyFigures)
	}
	if p := rep.Printers[0]; p.PrintWh != 110 || p.IdleWh != 200 {
		t.Fatalf("the printer's row = %+v", p.energyFigures)
	}
	// A record that was saved and read again says the same.
	mustSave(t, m)
	if again := reportOf(t, m, "day", "2026-11-19", "", "p1").Total; again.PrintWh != 110 || again.IdleWh != 200 || *again.IdleCost != 0.048 {
		t.Fatalf("after a save: %+v", again)
	}
}

func TestOldRowsAreSplitByThePrintRecords(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	for _, minute := range []int{0, 15, 30} {
		oldSlot(m, "p1", utc(11, 19, 12, minute), 90)
	}
	// Ten minutes into the first slot until ten minutes into the second.
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 19, 12, 10), 15*time.Minute, 90))
	// A slot that lies inside a print is all printing.
	oldSlot(m, "p1", utc(11, 19, 15, 15), 70)
	queuePrint(m, "p1", printRecord("b.gcode", utc(11, 19, 15, 0), time.Hour, 70))

	rep := reportOf(t, m, "day", "2026-11-19", "", "p1")
	// 5 of 15 minutes of 90 Wh, 10 of 15 minutes of 90 Wh, and 70 Wh.
	if total := rep.Total; !total.SplitEstimated || total.PrintWh != 160 || total.IdleWh != 180 || total.SplitUnknownWh != 0 {
		t.Fatalf("total = %+v; want 160 Wh printing, 180 Wh idle, estimated", total)
	}
	if p := rep.Points[15]; p.PrintWh != 70 || p.IdleWh != 0 || !p.SplitEstimated {
		t.Fatalf("15:00 = %+v; want all of it printing", p.energyFigures)
	}
	if p := rep.Points[12]; p.PrintWh != 90 || p.IdleWh != 180 || *p.IdleCost != 0.054 {
		t.Fatalf("12:00 = %+v", p.energyFigures)
	}
	// A period with rows of this build only is not marked.
	slot(m, "p1", utc(11, 19, 18, 0), 5)
	if p := reportOf(t, m, "day", "2026-11-19", "", "p1").Points[18]; p.SplitEstimated || p.IdleWh != 5 {
		t.Fatalf("18:00 = %+v", p.energyFigures)
	}
}

func TestAMissingPrintShareIsNeverReadAsIdle(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	oldSlot(m, "p1", utc(11, 21, 8, 0), 100)
	splitSlot(m, "p1", utc(11, 21, 9, 0), 50, 20)

	// The summary reads no print, so it cannot work the share out.
	sum, err := m.buildEnergySummary(map[string]string{"p1": "Printer"}, m.clock())
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]energyFigures{"today": sum.Today, "this month": sum.ThisMonth, "the printer": sum.Printers[0].Today} {
		if f.SplitUnknownWh != 100 || f.PrintWh != 20 || f.IdleWh != 30 || f.SplitEstimated || *f.IdleCost != 0.009 {
			t.Errorf("%s = %+v; want 100 Wh not known, 20 printing, 30 idle", name, f)
		}
	}
	// A row an older build began and this one went on with is not exact.
	m.mu.Lock()
	m.energy.pending[energyDay]["p1"][36].Kn = 400
	m.mu.Unlock()
	if sum, _ = m.buildEnergySummary(map[string]string{"p1": "Printer"}, m.clock()); sum.Today.SplitUnknownWh != 150 || sum.Today.IdleWh != 0 {
		t.Fatalf("with a row begun by an older build: %+v", sum.Today)
	}
}

func TestAnOldRowWithDroppedPrintsIsUnknown(t *testing.T) {
	m := reportModule(t, "UTC")
	oldSlot(m, "p1", utc(11, 19, 12, 0), 100)
	oldSlot(m, "p2", utc(11, 19, 12, 0), 100)
	writeEnergyFile(t, printsPath(m.energyDir(), "2026-11"), `{"v":1,"month":"2026-11","prints":{},"dropped":{"p1":3}}`)

	rep := reportOf(t, m, "day", "2026-11-19", "", "p1", "p2")
	for _, p := range rep.Printers {
		switch p.DeviceID {
		case "p1":
			if p.SplitUnknownWh != 100 || p.IdleWh != 0 || p.SplitEstimated {
				t.Errorf("the printer with dropped prints = %+v; want all of it not known", p.energyFigures)
			}
		case "p2":
			if p.SplitUnknownWh != 0 || p.IdleWh != 100 || !p.SplitEstimated {
				t.Errorf("the printer with every print = %+v; want all of it idle, estimated", p.energyFigures)
			}
		}
	}
	if !addsUp(rep.Total) || rep.Total.SplitUnknownWh != 100 {
		t.Fatalf("total = %+v", rep.Total)
	}
}

func TestARunningPrintCountsAsPrinting(t *testing.T) {
	m := reportModule(t, "UTC")
	oldSlot(m, "p1", utc(11, 21, 9, 30), 100)
	oldSlot(m, "p1", utc(11, 21, 9, 45), 100)
	m.mu.Lock()
	m.usage["p1"] = &DeviceUsage{CurrentPrint: &PrintPowerRecord{Active: true, StartedAt: utc(11, 21, 9, 45)}}
	m.mu.Unlock()

	if total := reportOf(t, m, "day", "", "", "p1").Total; total.PrintWh != 100 || total.IdleWh != 100 || !total.SplitEstimated {
		t.Fatalf("total = %+v; want the slot of the running print as printing", total)
	}
}

func TestAPrintThatEndsAfterTheSpanCountsInIt(t *testing.T) {
	m := reportModule(t, "UTC")
	oldSlot(m, "p1", utc(11, 19, 23, 45), 100)
	queuePrint(m, "p1", printRecord("night.gcode", utc(11, 19, 23, 30), time.Hour, 400))

	rep := reportOf(t, m, "day", "2026-11-19", "", "p1")
	if rep.PrintsTotal != 0 {
		t.Fatalf("%d prints ended on the 19th, want none", rep.PrintsTotal)
	}
	if rep.Total.PrintWh != 100 || rep.Total.IdleWh != 0 || !rep.Total.SplitEstimated {
		t.Fatalf("total = %+v; want the slot as printing", rep.Total)
	}
	// Also once the print is in its month file.
	mustSave(t, m)
	if total := reportOf(t, m, "day", "2026-11-19", "", "p1").Total; total.PrintWh != 100 {
		t.Fatalf("after a save: %+v", total)
	}
}

func TestTheSplitAddsUpToTheEnergy(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	for i := range 40 {
		at := utc(11, 19, 0, 0).Add(time.Duration(i) * 15 * time.Minute)
		wh := 0.0004 + float64(i)*0.3337
		switch i % 3 {
		case 0:
			splitSlot(m, "p1", at, wh, wh*0.4449)
		case 1:
			oldSlot(m, "p1", at, wh)
		default:
			oldSlot(m, "p2", at, wh)
		}
	}
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 19, 2, 7), 3*time.Hour+11*time.Second, 1))
	writeEnergyFile(t, printsPath(m.energyDir(), "2026-11"), `{"v":1,"month":"2026-11","prints":{},"dropped":{"p2":1}}`)

	for _, span := range []string{"day", "week", "month"} {
		rep := reportOf(t, m, span, "2026-11-19", "", "p1", "p2")
		if !addsUp(rep.Total) || rep.Total.PrintWh == 0 || rep.Total.IdleWh == 0 || rep.Total.SplitUnknownWh == 0 {
			t.Errorf("%s: total = %+v", span, rep.Total)
		}
		for _, p := range rep.Points {
			if !addsUp(p.energyFigures) {
				t.Errorf("%s: the point %s = %+v", span, p.Start, p.energyFigures)
			}
		}
		for _, p := range rep.Printers {
			if !addsUp(p.energyFigures) {
				t.Errorf("%s: the printer %s = %+v", span, p.DeviceID, p.energyFigures)
			}
		}
	}
	if rep := reportOf(t, m, "day", "2026-11-20", "", "p1", "p2"); rep.Previous == nil || !addsUp(*rep.Previous) {
		t.Fatalf("the day before = %+v", rep.Previous)
	}
}

func TestNoPriceGivesNoIdleCost(t *testing.T) {
	m := reportModule(t, "UTC")
	splitSlot(m, "p1", utc(11, 19, 12, 0), 100, 40)
	rep := reportOf(t, m, "day", "2026-11-19", "", "p1")
	if rep.Total.IdleCost != nil || rep.Total.IdleWh != 60 {
		t.Fatalf("total = %+v; want 60 Wh idle and no cost", rep.Total)
	}
	raw, err := json.Marshal(rep.Total)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"idle_cost":null`, `"print_wh":40`, `"idle_wh":60`, `"split_unknown_wh":0`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the answer %s lacks %s", raw, want)
		}
	}
	if strings.Contains(string(raw), "split_estimated") {
		t.Errorf("the answer %s names split_estimated without an estimate", raw)
	}
}

func TestThePreviousSpanIsNotEstimated(t *testing.T) {
	m := reportModule(t, "UTC")
	oldSlot(m, "p1", utc(11, 20, 12, 0), 100)
	queuePrint(m, "p1", printRecord("a.gcode", utc(11, 20, 12, 0), 15*time.Minute, 100))
	splitSlot(m, "p1", utc(11, 20, 13, 0), 50, 30)

	rep := reportOf(t, m, "day", "", "", "p1")
	if rep.Previous == nil {
		t.Fatal("no period before")
	}
	if p := *rep.Previous; p.SplitUnknownWh != 100 || p.SplitEstimated || p.PrintWh != 30 || p.IdleWh != 20 {
		t.Fatalf("the period before = %+v; want the old row as not known and the new one split", p)
	}
	// The same day as the span itself is worked out.
	if total := reportOf(t, m, "day", "2026-11-20", "", "p1").Total; total.PrintWh != 130 || !total.SplitEstimated || total.SplitUnknownWh != 0 {
		t.Fatalf("the day itself = %+v", total)
	}
}

// --- Files ---

func filePrint(file, outcome string, started time.Time, wh float64) PrintPowerRecord {
	rec := printRecord(file, started, 10*time.Minute, wh)
	rec.Outcome = outcome
	return rec
}

func unmeasured(rec PrintPowerRecord) PrintPowerRecord {
	rec.EnergyWh, rec.MeteredSec = 0, 0
	return rec
}

func fileNamed(t *testing.T, rep energyReport, name string) energyFile {
	t.Helper()
	for _, f := range rep.Files {
		if f.FileName == name {
			return f
		}
	}
	t.Fatalf("no file %q in %+v", name, rep.Files)
	return energyFile{}
}

func TestFilesAreGroupedByName(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.5}} })
	at := utc(11, 20, 8, 0)
	queuePrint(m, "p1", filePrint("a.gcode", "finished", at, 100))
	queuePrint(m, "p2", filePrint("a.gcode", "finished", at, 300))
	queuePrint(m, "p1", filePrint("A.gcode", "finished", at.Add(time.Hour), 1000))
	queuePrint(m, "p1", filePrint("Active Print", "finished", at.Add(2*time.Hour), 10))
	queuePrint(m, "p1", filePrint("", "finished", at.Add(3*time.Hour), 20))

	rep := reportOf(t, m, "month", "", "", "p1", "p2")
	if rep.FilesTotal != 3 || len(rep.Files) != 3 {
		t.Fatalf("%d files of %d: %+v", len(rep.Files), rep.FilesTotal, rep.Files)
	}
	if rep.Files[0].FileName != "A.gcode" || rep.Files[1].FileName != "a.gcode" || rep.Files[2].FileName != "" {
		t.Fatalf("order = %+v; want most energy first", rep.Files)
	}
	if f := fileNamed(t, rep, "a.gcode"); f.Prints != 2 || f.Measured != 2 || f.EnergyWh != 400 || *f.Cost != 0.2 || *f.AvgWh != 200 || *f.AvgCost != 0.1 {
		t.Fatalf("a.gcode on two printers = %+v", f)
	}
	if f := fileNamed(t, rep, ""); f.Prints != 2 || f.EnergyWh != 30 {
		t.Fatalf("the prints with no name = %+v", f)
	}
	// Equal energy: more prints first, then by name.
	queuePrint(m, "p2", filePrint("z.gcode", "finished", at.Add(4*time.Hour), 15))
	queuePrint(m, "p2", filePrint("z.gcode", "finished", at.Add(5*time.Hour), 15))
	queuePrint(m, "p2", filePrint("b.gcode", "finished", at.Add(6*time.Hour), 30))
	rep = reportOf(t, m, "month", "", "", "p1", "p2")
	if got := []string{rep.Files[2].FileName, rep.Files[3].FileName, rep.Files[4].FileName}; !reflect.DeepEqual(got, []string{"", "z.gcode", "b.gcode"}) {
		t.Fatalf("files with 30 Wh each are ordered %q", got)
	}
	// With no price there is no cost.
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = nil })
	if f := fileNamed(t, reportOf(t, m, "month", "", "", "p1", "p2"), "a.gcode"); f.Cost != nil || f.AvgCost != nil || *f.AvgWh != 200 {
		t.Fatalf("with no price: %+v", f)
	}
}

func TestAFileAverageCountsFinishedMeasuredPrintsOnly(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.5}} })
	at := utc(11, 20, 8, 0)
	queuePrint(m, "p1", filePrint("a.gcode", "finished", at, 100))
	queuePrint(m, "p1", filePrint("a.gcode", "FINISHED", at.Add(time.Hour), 200))
	queuePrint(m, "p1", filePrint("a.gcode", "cancelled", at.Add(2*time.Hour), 50))
	queuePrint(m, "p1", unmeasured(filePrint("a.gcode", "finished", at.Add(3*time.Hour), 0)))
	queuePrint(m, "p1", filePrint("b.gcode", "failed", at.Add(4*time.Hour), 10))

	rep := reportOf(t, m, "month", "", "", "p1")
	f := fileNamed(t, rep, "a.gcode")
	if f.Prints != 4 || f.NotFinished != 1 || f.Measured != 3 || f.EnergyWh != 350 || *f.Cost != 0.175 {
		t.Fatalf("a.gcode = %+v", f)
	}
	if f.AvgWh == nil || *f.AvgWh != 150 || *f.AvgCost != 0.075 {
		t.Fatalf("a.gcode averages %v Wh, want 150 over the two finished prints with a reading", f.AvgWh)
	}
	// A file that never finished has energy and no average.
	if f = fileNamed(t, rep, "b.gcode"); f.EnergyWh != 10 || f.AvgWh != nil || f.AvgCost != nil || f.NotFinished != 1 {
		t.Fatalf("b.gcode = %+v", f)
	}
}

func TestAnUnmeasuredFileHasNoEnergy(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.5}} })
	queuePrint(m, "p1", unmeasured(filePrint("a.gcode", "finished", utc(11, 20, 8, 0), 0)))

	f := fileNamed(t, reportOf(t, m, "month", "", "", "p1"), "a.gcode")
	if f.Prints != 1 || f.Measured != 0 || f.EnergyWh != 0 || f.AvgWh != nil || f.AvgCost != nil {
		t.Fatalf("a file with no reading = %+v", f)
	}
	rows := csvOf(t, exportOf(t, m, energyQuery{span: "month", visible: map[string]string{"p1": "Printer"}}, "files"))
	if want := []string{"a.gcode", "1", "0", "0", "", "", "", "", ""}; len(rows) != 2 || !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("rows = %q\nwant  %q", rows, want)
	}
}

func TestAnInterruptedPrintCountsAsNotFinished(t *testing.T) {
	m := reportModule(t, "UTC")
	at := utc(11, 20, 8, 0)
	for i, outcome := range []string{"interrupted", "FAILED", "Cancelled", "unknown", "", "finished"} {
		queuePrint(m, "p1", filePrint("a.gcode", outcome, at.Add(time.Duration(i)*time.Hour), 10))
	}
	if f := fileNamed(t, reportOf(t, m, "month", "", "", "p1"), "a.gcode"); f.Prints != 6 || f.NotFinished != 3 || *f.AvgWh != 10 {
		t.Fatalf("a.gcode = %+v; want 3 of 6 not finished", f)
	}
}

func TestTheFileListIsCappedAndCounted(t *testing.T) {
	m := reportModule(t, "UTC")
	first := utc(11, 2, 0, 0)
	for i := range 201 {
		queuePrint(m, "p1", printRecord(fmt.Sprintf("part-%03d.gcode", i), first.Add(time.Duration(i)*time.Hour), 30*time.Minute, float64(1000-i)))
	}
	rep := reportOf(t, m, "month", "", "", "p1")
	if len(rep.Files) != reportFileLimit || rep.FilesTotal != 201 {
		t.Fatalf("%d files shown of %d", len(rep.Files), rep.FilesTotal)
	}
	if rep.Files[0].FileName != "part-000.gcode" || rep.Files[199].FileName != "part-199.gcode" {
		t.Fatalf("the first is %s and the last shown %s", rep.Files[0].FileName, rep.Files[199].FileName)
	}
	// The CSV has them all.
	q := energyQuery{span: "month", visible: map[string]string{"p1": "Printer p1"}}
	if rows := strings.Count(exportOf(t, m, q, "files"), "\r\n"); rows != 202 {
		t.Fatalf("the CSV has %d lines, want the header and 201 files", rows)
	}
	// With no print the list is empty, not null.
	if raw, _ := json.Marshal(reportOf(t, m, "day", "2026-11-01", "", "p1")); !strings.Contains(string(raw), `"files":[]`) {
		t.Fatalf("a period with no print answers %s", raw)
	}
}

func TestFilesFollowThePrinterFilter(t *testing.T) {
	m := twoPrinters(t)
	if rep := reportOf(t, m, "month", "", "", "p1", "p2"); rep.FilesTotal != 2 {
		t.Fatalf("%d files for both printers", rep.FilesTotal)
	}
	rep := reportOf(t, m, "month", "", "p1", "p1", "p2")
	if rep.FilesTotal != 1 || rep.Files[0].FileName != "one.gcode" || rep.Files[0].EnergyWh != 40 {
		t.Fatalf("narrowed to p1: %+v", rep.Files)
	}
	// A printer the caller may not see adds no file.
	if rep = reportOf(t, m, "month", "", "", "p1"); rep.FilesTotal != 1 || rep.Files[0].FileName != "one.gcode" {
		t.Fatalf("for a caller who sees p1: %+v", rep.Files)
	}
	csv := exportOf(t, m, energyQuery{span: "month", device: "p2", visible: map[string]string{"p1": "One", "p2": "Two"}}, "files")
	if !strings.Contains(csv, "two.gcode") || strings.Contains(csv, "one.gcode") {
		t.Fatalf("the CSV narrowed to p2:\n%s", csv)
	}
}

func TestFilesCSVColumnsAndGuard(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.5}} })
	at := utc(11, 20, 8, 0)
	queuePrint(m, "p1", filePrint("=cmd()", "finished", at, 1500))
	queuePrint(m, "p1", filePrint("=cmd()", "failed", at.Add(time.Hour), 500))
	queuePrint(m, "p1", filePrint(`bracket "v2", final.gcode`, "cancelled", at.Add(2*time.Hour), 100))

	d, err := m.collectEnergy(energyQuery{span: "month", visible: map[string]string{"p1": "Printer"}, now: m.clock()})
	if err != nil {
		t.Fatal(err)
	}
	part := d.export("files", 0)
	if part.FileName != "power-files-month-2026-11-01.csv" || part.NextOffset != 0 {
		t.Fatalf("export = %q next %d", part.FileName, part.NextOffset)
	}
	rows := csvOf(t, part.CSV)
	if !reflect.DeepEqual(rows[0], strings.Split("file,prints,not_finished,measured_prints,energy_kwh,cost,currency,average_energy_kwh,average_cost", ",")) {
		t.Fatalf("header = %v", rows[0])
	}
	if want := []string{"'=cmd()", "2", "1", "2", "2.0000", "1.0000", "EUR", "1.5000", "0.7500"}; !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("row = %q\nwant  %q", rows[1], want)
	}
	if want := []string{`bracket "v2", final.gcode`, "1", "1", "1", "0.1000", "0.0500", "EUR", "", ""}; !reflect.DeepEqual(rows[2], want) {
		t.Fatalf("row = %q\nwant  %q", rows[2], want)
	}
	// With no price the cost cells are empty.
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = nil })
	rows = csvOf(t, exportOf(t, m, energyQuery{span: "month", visible: map[string]string{"p1": "Printer"}}, "files"))
	if want := []string{"'=cmd()", "2", "1", "2", "2.0000", "", "", "1.5000", ""}; !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("with no price: %q\nwant  %q", rows[1], want)
	}
}

// --- The summary ---

func summaryOf(t *testing.T, m *module, visible ...string) energySummary {
	t.Helper()
	seen := make(map[string]string, len(visible))
	for _, id := range visible {
		seen[id] = "Printer " + id
	}
	sum, err := m.buildEnergySummary(seen, m.clock())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	return sum
}

func TestTheSummaryHoldsTodayAndTheMonth(t *testing.T) {
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	slot(m, "p1", utc(11, 21, 8, 0), 100)
	slot(m, "p1", utc(11, 21, 0, 15), 30) // night
	slot(m, "p1", utc(11, 3, 12, 0), 1000)
	slot(m, "p2", utc(11, 21, 9, 0), 70)
	slot(m, "p2", utc(11, 20, 23, 45), 500)
	slot(m, "p3", utc(11, 21, 9, 0), 9000)
	slot(m, "p1", utc(10, 31, 23, 45), 4000) // the month before

	sum := summaryOf(t, m, "p2", "p1")
	month := reportOf(t, m, "month", "", "", "p1", "p2").Total
	day := reportOf(t, m, "day", "", "", "p1", "p2").Total
	if sum.ThisMonth.EnergyWh != 1700 || sum.ThisMonth.EnergyWh != month.EnergyWh || *sum.ThisMonth.Cost != *month.Cost || sum.ThisMonth.NightWh != month.NightWh {
		t.Fatalf("this month = %+v, the report says %+v", sum.ThisMonth, month)
	}
	if sum.Today.EnergyWh != 200 || sum.Today.EnergyWh != day.EnergyWh || *sum.Today.Cost != *day.Cost || sum.Today.ObservedSec != day.ObservedSec {
		t.Fatalf("today = %+v, the report says %+v", sum.Today, day)
	}
	if sum.Day != "2026-11-21" || sum.Month != "2026-11" || sum.Currency != "EUR" || !sum.PriceSet || !sum.Recording || sum.PriceChanged {
		t.Fatalf("summary = %+v", sum)
	}
	if len(sum.Printers) != 2 || sum.Printers[0].DeviceID != "p1" || sum.Printers[1].DeviceID != "p2" {
		t.Fatalf("printers = %+v; want p1 and p2 by id", sum.Printers)
	}
	if p := sum.Printers[0]; p.Today.EnergyWh != 130 || p.ThisMonth.EnergyWh != 1130 || *p.Today.Cost != 0.046 {
		t.Fatalf("p1 = %+v", p)
	}
	if p := sum.Printers[1]; p.Today.EnergyWh != 70 || p.ThisMonth.EnergyWh != 570 {
		t.Fatalf("p2 = %+v", p)
	}
	// A printer with no record today is listed with its month.
	if sum = summaryOf(t, m); len(sum.Printers) != 0 || sum.Printers == nil || sum.ThisMonth.EnergyWh != 0 {
		t.Fatalf("for a caller who sees no printer: %+v", sum)
	}
	// A second price in the month is told.
	changeEnergy(m, func(s *EnergySettings) {
		s.Tariffs = append(s.Tariffs, Tariff{From: utc(11, 10, 0, 0).Unix(), Price: 1})
	})
	if sum = summaryOf(t, m, "p1"); !sum.PriceChanged || *sum.PriceNow != 1 {
		t.Fatalf("with a second price: changed %v, now %v", sum.PriceChanged, sum.PriceNow)
	}
}

func TestTheSummaryFollowsTheSiteZone(t *testing.T) {
	// 10:07 UTC on the 21st is 23:07 on the 21st in Auckland.
	m := reportModule(t, "Pacific/Auckland")
	slot(m, "p1", utc(11, 20, 11, 30), 100) // 00:30 on the 21st
	slot(m, "p1", utc(11, 20, 10, 30), 20)  // 23:30 on the 20th
	slot(m, "p1", utc(10, 31, 11, 30), 300) // 00:30 on 1 November
	slot(m, "p1", utc(10, 31, 10, 30), 7)   // 23:30 on 31 October

	sum := summaryOf(t, m, "p1")
	if sum.Day != "2026-11-21" || sum.Month != "2026-11" || sum.Today.EnergyWh != 100 || sum.ThisMonth.EnergyWh != 420 {
		t.Fatalf("in Auckland: day %s, month %s, today %v, this month %v", sum.Day, sum.Month, sum.Today.EnergyWh, sum.ThisMonth.EnergyWh)
	}
	// Four hours later it is the 22nd there, and still the 21st in UTC.
	m.now = func() time.Time { return energyStart.Add(4 * time.Hour) }
	slot(m, "p1", utc(11, 21, 12, 0), 5)
	if sum = summaryOf(t, m, "p1"); sum.Day != "2026-11-22" || sum.Today.EnergyWh != 5 || sum.ThisMonth.EnergyWh != 425 {
		t.Fatalf("after midnight in Auckland: day %s, today %v, this month %v", sum.Day, sum.Today.EnergyWh, sum.ThisMonth.EnergyWh)
	}
}

func TestTheSummaryHasNoCostWithoutAPrice(t *testing.T) {
	m := reportModule(t, "UTC")
	slot(m, "p1", utc(11, 21, 8, 0), 100)
	sum := summaryOf(t, m, "p1")
	if sum.PriceSet || sum.PriceNow != nil || sum.NightNow || sum.Today.Cost != nil || sum.ThisMonth.Cost != nil || sum.Printers[0].Today.Cost != nil || sum.Today.EnergyWh != 100 {
		t.Fatalf("with no price: %+v", sum)
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"price_now":null`, `"price_set":false`, `"cost":null`, `"printers":[{"device_id":"p1"`, `"recording":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the answer %s lacks %s", raw, want)
		}
	}
	// With no data directory nothing is recorded, and the answer says so.
	bare, _ := clockedModule(energyStart)
	if sum, err = bare.buildEnergySummary(nil, energyStart); err != nil || sum.Recording || sum.Printers == nil {
		t.Fatalf("with no data directory: %+v, %v", sum, err)
	}
}

func TestTheSummaryPriceNowIsTheNightPriceAtNight(t *testing.T) {
	m := reportModule(t, "Europe/Prague")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{nightPrice(22, 6)} })
	// 10:07 UTC is 11:07 in Prague.
	if sum := summaryOf(t, m); sum.PriceNow == nil || *sum.PriceNow != 0.4 || sum.NightNow {
		t.Fatalf("by day: price %v, night %v", sum.PriceNow, sum.NightNow)
	}
	// 21:07 UTC is 22:07 in Prague.
	m.now = func() time.Time { return energyStart.Add(11 * time.Hour) }
	if sum := summaryOf(t, m); sum.PriceNow == nil || *sum.PriceNow != 0.2 || !sum.NightNow {
		t.Fatalf("at night: price %v, night %v", sum.PriceNow, sum.NightNow)
	}
}

func TestTheSummaryReadsNoPrintAndNoEarlierMonth(t *testing.T) {
	m := reportModule(t, "UTC")
	slot(m, "p1", utc(11, 20, 12, 0), 100)
	slot(m, "p1", utc(10, 20, 12, 0), 4000)
	mustSave(t, m)
	m.loadEnergy() // an empty cache, as after a start
	changeEnergy(m, func(s *EnergySettings) { s.TimeZone = "UTC" })
	damaged := printsPath(m.energyDir(), "2026-11")
	writeEnergyFile(t, damaged, `{"v":1,"prints":`)

	sum := summaryOf(t, m, "p1")
	if sum.ThisMonth.EnergyWh != 100 || sum.Today.EnergyWh != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if !exists(damaged) {
		t.Fatal("the summary read the prints file and set it aside")
	}
	if days := cachedDays(m); days["2026-10-20"] || !days["2026-11-20"] {
		t.Fatalf("the summary read the days %v; want November only", days)
	}
}

func TestAPositiveCostIsNeverRoundedToZero(t *testing.T) {
	for v, want := range map[float64]float64{0: 0, 0.00001: 0.0001, 0.00005: 0.0001, 0.00012: 0.0001, 1.23456: 1.2346, -0.00001: 0} {
		if got := roundCost(v); got != want {
			t.Errorf("roundCost(%v) = %v, want %v", v, got, want)
		}
	}
	m := reportModule(t, "UTC")
	changeEnergy(m, func(s *EnergySettings) { s.Tariffs = []Tariff{{Price: 0.3}} })
	slot(m, "p1", utc(11, 21, 8, 0), 0.01)
	slot(m, "p2", utc(11, 21, 8, 0), 0)
	queuePrint(m, "p1", printRecord("tiny.gcode", utc(11, 21, 8, 0), 15*time.Minute, 0.01))

	rep := reportOf(t, m, "day", "", "", "p1", "p2")
	if *rep.Total.Cost != 0.0001 || *rep.Total.IdleCost != 0.0001 || *rep.Prints[0].Cost != 0.0001 || *rep.PrintsCost != 0.0001 || *rep.Files[0].Cost != 0.0001 {
		t.Fatalf("a tiny amount: total %v, idle %v, print %v, prints %v, file %v", *rep.Total.Cost, *rep.Total.IdleCost, *rep.Prints[0].Cost, *rep.PrintsCost, *rep.Files[0].Cost)
	}
	// A plug that used nothing costs nothing.
	for _, p := range rep.Printers {
		if p.DeviceID == "p2" && *p.Cost != 0 {
			t.Fatalf("no energy costs %v", *p.Cost)
		}
	}
	if sum := summaryOf(t, m, "p1"); *sum.Today.Cost != 0.0001 {
		t.Fatalf("the summary says %v", *sum.Today.Cost)
	}
}

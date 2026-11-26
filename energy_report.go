package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Reading the energy record: the report of the Energy tab and its CSV export.
//
// Days and night hours follow one site time zone for every viewer, the one in
// the energy settings, else the zone of the server process. Cost is worked
// out here, by one rule for totals, chart points, prints and the CSV: each
// 15-minute slot is priced by the tariff that counted at its start, at the
// night price when its wall-clock time lies in the night hours.

type energyQuery struct {
	span, at, device string
	visible          map[string]string // printers the caller may see, id -> name
	known            map[string]bool   // every printer Connect lists, nil: unknown
	now              time.Time
	// asOf is the as_of an export was asked with, in unix seconds, 0 when
	// none was sent.
	asOf int64
	// snapshot leaves out what came after now: slots that start later and
	// prints that ended later. The parts of one export are read this way
	// with the same now, so a part that is asked for later lists the same
	// rows as the first.
	snapshot bool
	// totalsOnly reads the slots of the span alone: not the span before, no
	// print, and no estimate of the print share from the print records.
	totalsOnly bool
}

// exportAsOfWindow is how old the as_of of an export may be. A download asks
// for its parts one after the other and is over long before.
const exportAsOfWindow = time.Hour

var errEnergyQuery = errors.New("energy: bad query")

type energyFigures struct {
	EnergyWh    float64  `json:"energy_wh"`
	NightWh     float64  `json:"night_wh"`
	Cost        *float64 `json:"cost"` // null with no price
	ObservedSec int      `json:"observed_sec"`
	OnSec       int      `json:"on_sec"`
	MeteredSec  int      `json:"metered_sec"`
	// The energy split by whether a print ran. PrintWh went into a print,
	// IdleWh was used with the plug on and no print, and SplitUnknownWh is
	// what the record cannot tell. The three add up to EnergyWh.
	PrintWh        float64  `json:"print_wh"`
	IdleWh         float64  `json:"idle_wh"`
	IdleCost       *float64 `json:"idle_cost"` // null with no price
	SplitUnknownWh float64  `json:"split_unknown_wh"`
	// SplitEstimated says a part of the split was worked out from the print
	// records, for slots an earlier build recorded.
	SplitEstimated bool `json:"split_estimated,omitempty"`
}

type energyPoint struct {
	Start time.Time `json:"start"` // RFC 3339 with the site offset
	Day   string    `json:"day"`   // site date of Start
	Hour  int       `json:"hour"`  // site hour of Start, 0 unless span is day
	energyFigures
}

type energyPrinter struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	energyFigures
	Prints int `json:"prints"`
}

type energyPrint struct {
	DeviceID     string    `json:"device_id"`
	Name         string    `json:"name"`
	JobID        string    `json:"job_id,omitempty"`
	FileName     string    `json:"file_name"`
	Outcome      string    `json:"outcome,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	EndedLocal   string    `json:"ended_local"` // "2026-11-18T14:05:00", site wall clock
	DurationSec  int       `json:"duration_sec"`
	EnergyWh     float64   `json:"energy_wh"`
	NightWh      float64   `json:"night_wh"`
	Cost         *float64  `json:"cost"`
	MeteredSec   int       `json:"metered_sec"`
	PeakWatts    float64   `json:"peak_watts"`
	EndEstimated bool      `json:"end_estimated,omitempty"`
}

type energyReport struct {
	Span          string          `json:"span"`
	At            string          `json:"at"`      // first site day of the span
	PrevAt        string          `json:"prev_at"` // "" when the span before ends before Earliest
	NextAt        string          `json:"next_at"` // "" when this span holds the current day
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	Earliest      string          `json:"earliest"` // oldest site day still kept, "" with no record
	TimeZone      string          `json:"time_zone"`
	ZoneSet       bool            `json:"zone_set"`
	UTCOffsetSec  int             `json:"utc_offset_sec"`
	KeepDays      int             `json:"keep_days"`
	Currency      string          `json:"currency"`
	PriceSet      bool            `json:"price_set"`
	Night         bool            `json:"night"`         // a tariff with a night price touches the span
	PriceChanged  bool            `json:"price_changed"` // more than one tariff touches the span
	Device        string          `json:"device"`
	Recording     bool            `json:"recording"`
	CanConfigure  bool            `json:"can_configure"` // a hint for the page, set by the handler with mayHint
	Points        []energyPoint   `json:"points"`        // every period of the span, also the empty ones
	Total         energyFigures   `json:"total"`
	Previous      *energyFigures  `json:"previous"` // the span before, null with no observed time
	Printers      []energyPrinter `json:"printers"` // visible printers with a record in the span, most energy first, not narrowed by device, at most 500
	PrintersTotal int             `json:"printers_total"`
	Removed       int             `json:"removed"` // printers with a record in the span that Connect no longer lists
	Prints        []energyPrint   `json:"prints"`  // ended in the span, newest first, at most 200
	PrintsTotal   int             `json:"prints_total"`
	PrintsWh      float64         `json:"prints_wh"` // over all prints of the span
	PrintsCost    *float64        `json:"prints_cost"`
	PrintsDropped int             `json:"prints_dropped"`
	Files         []energyFile    `json:"files"` // the prints grouped by file name, most energy first, at most 200
	FilesTotal    int             `json:"files_total"`
}

// energyFile is the prints of one G-code file name, on every printer.
type energyFile struct {
	FileName    string   `json:"file_name"`    // "" when the name is not known
	Prints      int      `json:"prints"`       // all prints of the name
	NotFinished int      `json:"not_finished"` // failed, cancelled or interrupted
	Measured    int      `json:"measured"`     // prints with a reading
	EnergyWh    float64  `json:"energy_wh"`    // over the measured prints
	Cost        *float64 `json:"cost"`         // over the measured prints, null with no price
	AvgWh       *float64 `json:"avg_wh"`       // mean of the measured prints that finished, null with none
	AvgCost     *float64 `json:"avg_cost"`
}

type energySummaryPrinter struct {
	DeviceID  string        `json:"device_id"`
	Today     energyFigures `json:"today"`
	ThisMonth energyFigures `json:"this_month"`
}

// energySummary is the answer of GET /energy/summary: the current site day
// and month, and the price that counts now.
type energySummary struct {
	Day          string                 `json:"day"`   // the current site day
	Month        string                 `json:"month"` // "2026-11"
	Currency     string                 `json:"currency"`
	PriceSet     bool                   `json:"price_set"`
	PriceNow     *float64               `json:"price_now"` // per kWh in the current quarter hour, null with no price
	NightNow     bool                   `json:"night_now"`
	PriceChanged bool                   `json:"price_changed"` // more than one tariff touches the month
	Recording    bool                   `json:"recording"`
	Today        energyFigures          `json:"today"`
	ThisMonth    energyFigures          `json:"this_month"`
	Printers     []energySummaryPrinter `json:"printers"` // visible printers with a record this month, by id, at most 500
}

type energyExport struct {
	FileName   string `json:"file_name"`   // power-energy-month-2026-11-01.csv, power-prints-..., power-files-...
	CSV        string `json:"csv"`         // offset 0 starts with the header row
	NextOffset int    `json:"next_offset"` // 0: this was the last part
	AsOf       int64  `json:"as_of"`       // the moment the rows are read for, to send back with every later part
}

// --- Spans and periods ---

// siteMidnight is the first moment of a site day. Where a daylight-saving
// step skips midnight, that is the moment the clock jumps to.
func siteMidnight(year int, month time.Month, day int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, loc)
	want := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	for range 12 {
		y, m, d := t.Date()
		if !time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Before(want) {
			break
		}
		t = t.Add(slotLength)
	}
	return t
}

func siteDate(t time.Time) string { return t.Format(dayLayout) }

// spanStart is the first moment of the span that holds the site day of t.
func spanStart(span string, t time.Time) time.Time {
	y, m, d := t.Date()
	switch span {
	case "day":
	case "week":
		d -= (int(t.Weekday()) + 6) % 7 // back to Monday
	case "year":
		m, d = time.January, 1
	default:
		d = 1
	}
	return siteMidnight(y, m, d, t.Location())
}

// spanShift is the start of the span n spans after the one that starts at
// start.
func spanShift(span string, start time.Time, n int) time.Time {
	y, m, d := start.Date()
	switch span {
	case "day":
		d += n
	case "week":
		d += 7 * n
	case "year":
		y += n
	default:
		m += time.Month(n)
	}
	return siteMidnight(y, m, d, start.Location())
}

// spanEdges are the edges of the periods of a span: hours of a day (23, 24
// or 25 of them on a daylight-saving day), days of a week or a month, months
// of a year. They rise strictly.
func spanEdges(span string, from, to time.Time) []time.Time {
	edges := []time.Time{from}
	push := func(t time.Time) {
		if t.After(edges[len(edges)-1]) && t.Before(to) {
			edges = append(edges, t)
		}
	}
	y, m, d := from.Date()
	switch span {
	case "day":
		for t := from.Add(time.Hour); t.Before(to); t = t.Add(time.Hour) {
			push(t)
		}
	case "year":
		for i := 1; i < 12; i++ {
			push(siteMidnight(y, m+time.Month(i), 1, from.Location()))
		}
	default:
		for i := 1; i < 32; i++ {
			push(siteMidnight(y, m, d+i, from.Location()))
		}
	}
	return append(edges, to)
}

// --- Cost ---

// priceBook prices a slot: the tariff that counted at its start, at the
// night price when its site wall-clock time lies in the night hours.
type priceBook struct {
	tariffs []Tariff
	loc     *time.Location
}

func (b priceBook) tariffAt(slot int64) (Tariff, bool) {
	for i := len(b.tariffs) - 1; i >= 0; i-- {
		if b.tariffs[i].From <= slot {
			return b.tariffs[i], true
		}
	}
	return Tariff{}, false
}

func (b priceBook) priceAt(slot int64) (price float64, night, ok bool) {
	t, ok := b.tariffAt(slot)
	if !ok {
		return 0, false, false
	}
	if !t.Night {
		return t.Price, false, true
	}
	hour, minute, _ := time.Unix(slot, 0).In(b.loc).Clock()
	at, start, end := hour*60+minute, t.NightStart*60, t.NightEnd*60
	if start < end {
		night = at >= start && at < end
	} else {
		night = at >= start || at < end
	}
	if night {
		return t.NightPrice, true, true
	}
	return t.Price, false, true
}

// touching answers the tariffs that count somewhere in [from, to).
func (b priceBook) touching(from, to int64) []Tariff {
	var out []Tariff
	for i, t := range b.tariffs {
		until := int64(math.MaxInt64)
		if i+1 < len(b.tariffs) {
			until = b.tariffs[i+1].From
		}
		if t.From < to && until > from {
			out = append(out, t)
		}
	}
	return out
}

// flat answers the one price that counts from from to to, when there is
// one: a single tariff with no night price. Such a time needs no look at
// the record to be priced.
func (b priceBook) flat(from, to time.Time) (float64, bool) {
	first := from.UTC().Truncate(slotLength).Unix()
	list := b.touching(first, to.Unix()+1)
	if len(list) != 1 || list[0].Night || list[0].From > first {
		return 0, false
	}
	return list[0].Price, true
}

// meanPrice is the price per kWh of what one printer used between started
// and ended, and the share of it that fell into night hours. A slot weighs
// its energy times the share of it the time overlaps; with no energy
// recorded in that time, the overlap in seconds. ok is false when nothing
// weighs at all.
func (b priceBook) meanPrice(r *energyReader, id string, started, ended time.Time) (price, nightShare float64, ok bool) {
	lo, hi := float64(started.UnixNano())/1e9, float64(ended.UnixNano())/1e9
	var byEnergy, byTime struct{ weight, price, night float64 }
	for slot := started.UTC().Truncate(slotLength).Unix(); float64(slot) <= hi; slot += slotSec {
		overlap := math.Min(hi, float64(slot+slotSec)) - math.Max(lo, float64(slot))
		if hi == lo {
			overlap = 1
		}
		if overlap <= 0 {
			continue
		}
		slotPrice, night, _ := b.priceAt(slot)
		share := 0.0
		if night {
			share = 1
		}
		energy := r.whAt(id, slot) * overlap / slotSec
		byEnergy.weight += energy
		byEnergy.price += energy * slotPrice
		byEnergy.night += energy * share
		byTime.weight += overlap
		byTime.price += overlap * slotPrice
		byTime.night += overlap * share
	}
	by := byEnergy
	if by.weight <= 0 {
		by = byTime
	}
	if by.weight <= 0 {
		return 0, 0, false
	}
	return by.price / by.weight, by.night / by.weight, true
}

// --- Reading ---

// figuresAcc sums slots. prt is the energy that went into prints, unknown
// the energy the record cannot split, idleCost the cost of the rest, and
// est says a part of prt was worked out from the print records.
type figuresAcc struct {
	wh, nightWh, cost, obs, on, met float64
	prt, idleCost, unknown          float64
	est                             bool
}

func (a *figuresAcc) add(b figuresAcc) {
	a.wh += b.wh
	a.nightWh += b.nightWh
	a.cost += b.cost
	a.obs += b.obs
	a.on += b.on
	a.met += b.met
	a.prt += b.prt
	a.idleCost += b.idleCost
	a.unknown += b.unknown
	a.est = a.est || b.est
}

// energyReader reads slots and prints while io is held: days from the cache
// or the disk, with the part that is not on disk yet added.
type energyReader struct {
	store   *energyStore
	dir     string // the energy directory, "" with no data directory
	pending map[string]map[string]*[slotsPerDay]slotAcc
	queued  []pendingPrint
	running map[string]time.Time                        // printer -> start of the print that runs now
	wh      map[string]map[string]*[slotsPerDay]float64 // day -> printer -> energy of each slot
}

// daySlots answers every printer's slots of one UTC day.
func (r *energyReader) daySlots(day string) map[string][]cachedSlot {
	if r.dir == "" {
		return nil
	}
	devices, err := r.store.dayLocked(r.dir, day)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: read energy of %s: %v\n", logName, day, err)
		return nil
	}
	return devices
}

// eachSlot calls fn for every recorded slot that starts in [from, to).
func (r *energyReader) eachSlot(from, to time.Time, fn func(id string, start int64, a slotAcc)) {
	lo, hi := from.Unix(), to.Unix()
	for day := from.UTC().Truncate(dayLength); day.Before(to); day = day.Add(dayLength) {
		name, base := day.Format(dayLayout), day.Unix()
		devices, part := r.daySlots(name), r.pending[name]
		emit := func(id string, slot int, a slotAcc) {
			if start := base + int64(slot)*slotSec; start >= lo && start < hi && a.Obs > 0 {
				fn(id, start, a)
			}
		}
		for id, rows := range devices {
			if part[id] != nil {
				continue
			}
			for _, c := range rows {
				emit(id, int(c.Slot), c.acc())
			}
		}
		for id, add := range part {
			slots := *add
			for _, c := range devices[id] {
				slots[c.Slot].add(c.acc())
			}
			for i, a := range slots {
				emit(id, i, a)
			}
		}
	}
}

// whAt is the energy one printer's slot holds.
func (r *energyReader) whAt(id string, slot int64) float64 {
	t := time.Unix(slot, 0).UTC()
	day := t.Format(dayLayout)
	if r.wh == nil {
		r.wh = make(map[string]map[string]*[slotsPerDay]float64)
	}
	devices, ok := r.wh[day]
	if !ok {
		devices = make(map[string]*[slotsPerDay]float64)
		r.wh[day] = devices
	}
	slots, ok := devices[id]
	if !ok {
		slots = new([slotsPerDay]float64)
		for _, c := range r.daySlots(day)[id] {
			slots[c.Slot] += float64(c.Wh)
		}
		if add := r.pending[day][id]; add != nil {
			for i := range add {
				slots[i] += add[i].Wh
			}
		}
		devices[id] = slots
	}
	return slots[t.Hour()*4+t.Minute()/15]
}

// printsIn answers the prints that ended in [from, to), from the month files
// and from the queue, each once, and how many each printer's months dropped.
func (r *energyReader) printsIn(from, to time.Time) (prints []pendingPrint, dropped map[string]int) {
	dropped = make(map[string]int)
	type key struct {
		id      string
		started int64
	}
	seen := make(map[key]bool)
	take := func(id string, rec PrintPowerRecord) {
		if rec.EndedAt == nil || rec.EndedAt.Before(from) || !rec.EndedAt.Before(to) {
			return
		}
		k := key{id, rec.StartedAt.Unix()}
		if seen[k] {
			return
		}
		seen[k] = true
		prints = append(prints, pendingPrint{DeviceID: id, Record: rec})
	}
	if r.dir != "" {
		first := from.UTC()
		for month := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC); month.Before(to); month = month.AddDate(0, 1, 0) {
			f, state, err := readPrintsFile(printsPath(r.dir, month.Format(monthLayout)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: read prints of %s: %v\n", logName, month.Format(monthLayout), err)
				continue
			}
			if state != storedRead {
				continue
			}
			for id, list := range f.Prints {
				for _, rec := range list {
					take(id, rec)
				}
			}
			for id, n := range f.Dropped {
				dropped[id] += n
			}
		}
	}
	for _, p := range r.queued {
		take(p.DeviceID, p.Record)
	}
	return prints, dropped
}

// energyData is what a report and its export are made from, without the
// caps of the page.
type energyData struct {
	span          string
	settings      EnergySettings
	loc           *time.Location
	now           time.Time
	from, to      time.Time
	edges         []time.Time
	earliest      string
	prevAt        string
	nextAt        string
	book          priceBook
	priced        bool
	night         bool
	priceChanged  bool
	device        string
	recording     bool
	names         map[string]string
	periods       map[string][]figuresAcc // visible printer -> one sum per period
	previous      figuresAcc
	removed       int
	prints        []energyPrint  // of the span and the device asked for, newest first
	printCount    map[string]int // per visible printer, not narrowed by device
	printsDropped int
}

func round3(v float64) float64 { return math.Round(v*1e3) / 1e3 }

// roundCost rounds an amount to 4 decimals. An amount above zero never
// becomes zero: it reads as the smallest amount there is.
func roundCost(v float64) float64 {
	r := math.Round(v*1e4) / 1e4
	if v > 0 && r <= 0 {
		return 0.0001
	}
	return r
}

// clip keeps a text of unknown origin to a length a table can hold.
func clip(s string) string {
	const limit = 512
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

func (d *energyData) figures(a figuresAcc) energyFigures {
	f := energyFigures{
		EnergyWh:    round3(a.wh),
		NightWh:     round3(a.nightWh),
		ObservedSec: int(math.Round(a.obs)),
		OnSec:       int(math.Round(a.on)),
		MeteredSec:  int(math.Round(a.met)),
	}
	// The three parts are rounded so that they add up to the energy: the
	// idle part is what is left.
	f.PrintWh = math.Max(0, math.Min(round3(a.prt), f.EnergyWh))
	f.SplitUnknownWh = math.Max(0, math.Min(round3(a.unknown), round3(f.EnergyWh-f.PrintWh)))
	f.IdleWh = math.Max(0, round3(f.EnergyWh-f.PrintWh-f.SplitUnknownWh))
	f.SplitEstimated = a.est
	if d.priced {
		cost, idle := roundCost(a.cost), roundCost(math.Max(0, a.idleCost))
		f.Cost, f.IdleCost = &cost, &idle
	}
	return f
}

// shown says whether a printer's figures count for the device asked for.
func (d *energyData) shown(id string) bool {
	return d.device == "" || d.device == id
}

// energyReaderLocked makes a reader with a copy of what memory holds and the
// disk does not yet. The caller holds io.
func (m *module) energyReaderLocked() *energyReader {
	s := &m.energy
	reader := &energyReader{store: s, running: make(map[string]time.Time)}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.dataDir != "" {
		reader.dir = filepath.Join(m.dataDir, energyDirName)
	}
	reader.pending = make(map[string]map[string]*[slotsPerDay]slotAcc, len(s.pending))
	for day, devices := range s.pending {
		copied := make(map[string]*[slotsPerDay]slotAcc, len(devices))
		for id, slots := range devices {
			cp := *slots
			copied[id] = &cp
		}
		reader.pending[day] = copied
	}
	reader.queued = append([]pendingPrint(nil), s.pendingPrints...)
	for id, u := range m.usage {
		if u != nil && u.CurrentPrint != nil && u.CurrentPrint.Active {
			reader.running[id] = u.CurrentPrint.StartedAt
		}
	}
	return reader
}

// busySpans are the times each printer printed, merged and ascending.
type busySpans map[string][][2]float64

func (b busySpans) add(id string, from, to time.Time) {
	if to.After(from) {
		b[id] = append(b[id], [2]float64{float64(from.UnixNano()) / 1e9, float64(to.UnixNano()) / 1e9})
	}
}

func (b busySpans) merge() {
	for id, list := range b {
		sort.Slice(list, func(i, j int) bool { return list[i][0] < list[j][0] })
		out := list[:0]
		for _, span := range list {
			if n := len(out); n > 0 && span[0] <= out[n-1][1] {
				out[n-1][1] = math.Max(out[n-1][1], span[1])
				continue
			}
			out = append(out, span)
		}
		b[id] = out
	}
}

// overlap is how many seconds of [from, to) the printer printed.
func (b busySpans) overlap(id string, from, to float64) (sec float64) {
	list := b[id]
	for i := sort.Search(len(list), func(i int) bool { return list[i][1] > from }); i < len(list) && list[i][0] < to; i++ {
		sec += math.Min(to, list[i][1]) - math.Max(from, list[i][0])
	}
	return sec
}

// collectEnergy reads everything a report of q needs. io is held for the
// whole read, so a flush or a prune cannot run halfway through it.
func (m *module) collectEnergy(q energyQuery) (*energyData, error) {
	span := q.span
	switch span {
	case "":
		span = "month"
	case "day", "week", "month", "year":
	default:
		return nil, fmt.Errorf("%w: span must be day, week, month or year", errEnergyQuery)
	}
	settings := m.energySettings()
	loc := settings.location()
	now := q.now.In(loc)
	at := now
	if q.at != "" {
		day, err := time.Parse(dayLayout, q.at)
		if err != nil {
			return nil, fmt.Errorf("%w: at must be a date such as 2026-11-21", errEnergyQuery)
		}
		// A day in the future is read as the current day.
		if asked := siteMidnight(day.Year(), day.Month(), day.Day(), loc); !asked.After(now) {
			at = asked
		}
	}

	d := &energyData{
		span:       span,
		settings:   settings,
		loc:        loc,
		now:        now,
		book:       priceBook{tariffs: settings.Tariffs, loc: loc},
		priced:     len(settings.Tariffs) > 0,
		names:      q.visible,
		periods:    make(map[string][]figuresAcc),
		printCount: make(map[string]int),
	}
	if _, visible := q.visible[q.device]; visible {
		d.device = q.device
	}
	d.from = spanStart(span, at)
	d.to = spanShift(span, d.from, 1)
	d.edges = spanEdges(span, d.from, d.to)
	prevFrom := spanShift(span, d.from, -1)
	for _, t := range d.book.touching(d.from.Unix(), d.to.Unix()) {
		d.night = d.night || t.Night
	}
	d.priceChanged = len(d.book.touching(d.from.Unix(), d.to.Unix())) > 1

	s := &m.energy
	s.io.Lock()
	defer s.io.Unlock()

	reader := m.energyReaderLocked()
	d.recording = reader.dir != ""

	// The oldest day there is a record of: the oldest file, or what is still
	// only in memory, but nothing before the keep time.
	cut := keepCut(settings, q.now)
	first := ""
	if reader.dir != "" {
		s.refreshFirstDayLocked(reader.dir)
		first = s.firstDay
	}
	for day := range reader.pending {
		if first == "" || day < first {
			first = day
		}
	}
	var firstDay time.Time
	if first != "" {
		oldest, _ := time.Parse(dayLayout, first)
		firstDay = oldest
		if oldest.Before(cut) {
			oldest = cut
		}
		d.earliest = siteDate(oldest.In(loc))
		y, mo, day := oldest.In(loc).Date()
		if d.from.After(siteMidnight(y, mo, day, loc)) {
			d.prevAt = siteDate(prevFrom)
		}
	}
	if !now.Before(d.to) {
		d.nextAt = siteDate(d.to)
	}

	edges := make([]int64, len(d.edges))
	for i, t := range d.edges {
		edges[i] = t.Unix()
	}
	// Only days that can hold a record are read: none before the keep time
	// and none before the oldest record. at may name any day back to the
	// year 0, and every day that is read is a look at the disk.
	readFrom, readTo := prevFrom, d.to
	if q.totalsOnly {
		readFrom = d.from
	}
	if readFrom.Before(cut) {
		readFrom = cut
	}
	if readFrom.Before(firstDay) {
		readFrom = firstDay
	}
	if after := q.now.Add(time.Second); q.snapshot && after.Before(readTo) {
		readTo = after
	}

	// The same for the prints: no month before the keep time is read.
	printsFrom := d.from
	if printsFrom.Before(cut) {
		printsFrom = cut
	}
	var prints []pendingPrint
	var dropped map[string]int
	if !q.totalsOnly && printsFrom.Before(d.to) {
		prints, dropped = reader.printsIn(printsFrom, d.to)
	}

	// When each printer printed, for the slots that do not say it themselves.
	// It is made on first need: the prints that ended in the span, the ones
	// that ended after it, and the ones that run now. A printer whose months
	// dropped prints has no full list, so its slots are not split this way.
	var busy busySpans
	lost := make(map[string]bool)
	busyOf := func() busySpans {
		if busy != nil {
			return busy
		}
		busy = make(busySpans)
		for id, n := range dropped {
			lost[id] = lost[id] || n > 0
		}
		for _, p := range prints {
			busy.add(p.DeviceID, p.Record.StartedAt, *p.Record.EndedAt)
		}
		end := d.to.Add(31 * dayLength)
		if after := q.now.Add(time.Second); after.Before(end) {
			end = after
		}
		if d.to.Before(end) {
			later, laterDropped := reader.printsIn(d.to, end)
			for _, p := range later {
				busy.add(p.DeviceID, p.Record.StartedAt, *p.Record.EndedAt)
			}
			for id, n := range laterDropped {
				lost[id] = lost[id] || n > 0
			}
		}
		for id, started := range reader.running {
			busy.add(id, started, q.now)
		}
		busy.merge()
		return busy
	}

	fromUnix, cutUnix := d.from.Unix(), cut.Unix()
	removed := make(map[string]bool)
	collect := func(id string, start int64, a slotAcc) {
		if start < cutUnix {
			return
		}
		if _, visible := q.visible[id]; !visible {
			if start >= fromUnix && q.known != nil && !q.known[id] {
				removed[id] = true
			}
			return
		}
		add := figuresAcc{wh: a.Wh, obs: a.Obs, on: a.On, met: a.Met}
		price, night, priced := d.book.priceAt(start)
		if priced {
			add.cost = a.Wh / 1000 * price
			if night {
				add.nightWh = a.Wh
			}
		}
		// The print share: what the slot says itself, else what the print
		// records say, and never idle where neither is known.
		switch {
		case a.Wh <= 0:
		case a.splitExact():
			add.prt = math.Min(a.Prt, a.Wh)
		case start < fromUnix || q.totalsOnly:
			add.unknown = a.Wh
		default:
			if spans := busyOf(); lost[id] {
				add.unknown = a.Wh
			} else {
				if a.On > 0 {
					add.prt = a.Wh * math.Min(1, spans.overlap(id, float64(start), float64(start+slotSec))/a.On)
				}
				add.est = true
			}
		}
		if idle := a.Wh - add.prt - add.unknown; idle > 0 && priced {
			add.idleCost = idle / 1000 * price
		}
		if start < fromUnix {
			if d.shown(id) {
				d.previous.add(add)
			}
			return
		}
		row := d.periods[id]
		if row == nil {
			row = make([]figuresAcc, len(edges)-1)
			d.periods[id] = row
		}
		// The period that holds the start of the slot.
		i := sort.Search(len(edges), func(i int) bool { return edges[i] > start }) - 1
		if i >= 0 && i < len(row) {
			row[i].add(add)
		}
	}
	if first != "" && readFrom.Before(readTo) {
		reader.eachSlot(readFrom, readTo, collect)
	}
	d.removed = len(removed)

	for _, p := range prints {
		name, visible := q.visible[p.DeviceID]
		if !visible || p.Record.EndedAt.Before(cut) || (q.snapshot && p.Record.EndedAt.After(q.now)) {
			continue
		}
		d.printCount[p.DeviceID]++
		if !d.shown(p.DeviceID) {
			continue
		}
		d.prints = append(d.prints, d.printOf(reader, p.DeviceID, name, p.Record))
	}
	sort.SliceStable(d.prints, func(i, j int) bool {
		a, b := d.prints[i], d.prints[j]
		if !a.EndedAt.Equal(b.EndedAt) {
			return a.EndedAt.After(b.EndedAt)
		}
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.After(b.StartedAt)
		}
		return a.DeviceID < b.DeviceID
	})
	for id, n := range dropped {
		if _, visible := q.visible[id]; visible && d.shown(id) {
			d.printsDropped += n
		}
	}
	return d, nil
}

// printOf prices one print by its printer's own slots between its start and
// its end (meanPrice).
func (d *energyData) printOf(r *energyReader, id, name string, rec PrintPowerRecord) energyPrint {
	ended := *rec.EndedAt
	out := energyPrint{
		DeviceID:     id,
		Name:         name,
		JobID:        clip(rec.JobID),
		FileName:     clip(rec.FileName),
		Outcome:      clip(rec.Outcome),
		StartedAt:    rec.StartedAt.In(d.loc),
		EndedAt:      ended.In(d.loc),
		EndedLocal:   ended.In(d.loc).Format("2006-01-02T15:04:05"),
		DurationSec:  rec.DurationSec,
		EnergyWh:     round3(rec.EnergyWh),
		MeteredSec:   int(math.Round(rec.MeteredSec)),
		PeakWatts:    rec.PeakWatts,
		EndEstimated: rec.EndEstimated,
	}

	// Slots more than 31 days before the span are not read for one print.
	started := rec.StartedAt
	if floor := d.from.Add(-31 * dayLength); started.Before(floor) {
		started = floor
	}
	if started.After(ended) {
		started = ended
	}
	if price, night, ok := d.book.meanPrice(r, id, started, ended); ok {
		out.NightWh = round3(rec.EnergyWh * night)
		if d.priced {
			cost := roundCost(rec.EnergyWh / 1000 * price)
			out.Cost = &cost
		}
	}
	return out
}

// printerIDs are the visible printers with a record in the span, by name.
func (d *energyData) printerIDs() []string {
	ids := make([]string, 0, len(d.periods))
	for id := range d.periods {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if d.names[ids[i]] != d.names[ids[j]] {
			return d.names[ids[i]] < d.names[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// report is the answer of GET /energy.
func (d *energyData) report() energyReport {
	_, offset := d.now.Zone()
	rep := energyReport{
		Span:          d.span,
		At:            siteDate(d.from),
		PrevAt:        d.prevAt,
		NextAt:        d.nextAt,
		From:          d.from,
		To:            d.to,
		Earliest:      d.earliest,
		TimeZone:      d.settings.TimeZone,
		ZoneSet:       d.settings.TimeZone != "",
		UTCOffsetSec:  offset,
		KeepDays:      d.settings.KeepDays,
		Currency:      d.settings.Currency,
		PriceSet:      d.priced,
		Night:         d.night,
		PriceChanged:  d.priceChanged,
		Device:        d.device,
		Recording:     d.recording,
		Points:        make([]energyPoint, len(d.edges)-1),
		Printers:      []energyPrinter{},
		Removed:       d.removed,
		Prints:        d.prints,
		PrintsTotal:   len(d.prints),
		PrintsDropped: d.printsDropped,
	}

	points := make([]figuresAcc, len(d.edges)-1)
	var total figuresAcc
	for _, id := range d.printerIDs() {
		var sum figuresAcc
		for i, a := range d.periods[id] {
			sum.add(a)
			if d.shown(id) {
				points[i].add(a)
			}
		}
		if d.shown(id) {
			total.add(sum)
		}
		rep.Printers = append(rep.Printers, energyPrinter{
			DeviceID:      id,
			Name:          d.names[id],
			energyFigures: d.figures(sum),
			Prints:        d.printCount[id],
		})
	}
	for i := range points {
		start := d.edges[i]
		rep.Points[i] = energyPoint{Start: start, Day: siteDate(start), energyFigures: d.figures(points[i])}
		if d.span == "day" {
			rep.Points[i].Hour = start.Hour()
		}
	}
	rep.Total = d.figures(total)
	if d.previous.obs > 0 {
		previous := d.figures(d.previous)
		rep.Previous = &previous
	}

	sort.SliceStable(rep.Printers, func(i, j int) bool { return rep.Printers[i].EnergyWh > rep.Printers[j].EnergyWh })
	rep.PrintersTotal = len(rep.Printers)
	if len(rep.Printers) > reportPrinterLimit {
		rep.Printers = rep.Printers[:reportPrinterLimit]
	}

	printsCost := 0.0
	for _, p := range d.prints {
		rep.PrintsWh += p.EnergyWh
		if p.Cost != nil {
			printsCost += *p.Cost
		}
	}
	rep.PrintsWh = round3(rep.PrintsWh)
	if d.priced {
		printsCost = roundCost(printsCost)
		rep.PrintsCost = &printsCost
	}
	rep.Files = d.files()
	rep.FilesTotal = len(rep.Files)
	if len(rep.Files) > reportFileLimit {
		rep.Files = rep.Files[:reportFileLimit]
	}
	if len(rep.Prints) > reportPrintLimit {
		rep.Prints = rep.Prints[:reportPrintLimit]
	}
	if rep.Prints == nil {
		rep.Prints = []energyPrint{}
	}
	return rep
}

// notFinished says a print did not run to its end.
func notFinished(outcome string) bool {
	switch strings.ToLower(outcome) {
	case "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

// files groups the prints of the span by file name: one file on two
// printers is one row. Energy and cost count the prints with a reading, the
// averages the ones among them that finished. Most energy first.
func (d *energyData) files() []energyFile {
	type sums struct {
		energyFile
		cost, doneWh, doneCost float64
		done                   int
	}
	byName := make(map[string]*sums)
	var order []*sums
	for _, p := range d.prints {
		name := p.FileName
		if name == "Active Print" {
			name = ""
		}
		f := byName[name]
		if f == nil {
			f = &sums{energyFile: energyFile{FileName: name}}
			byName[name] = f
			order = append(order, f)
		}
		f.Prints++
		if notFinished(p.Outcome) {
			f.NotFinished++
		}
		if p.MeteredSec <= 0 {
			continue
		}
		cost := 0.0
		if p.Cost != nil {
			cost = *p.Cost
		}
		f.Measured++
		f.EnergyWh += p.EnergyWh
		f.cost += cost
		if strings.EqualFold(p.Outcome, "finished") {
			f.done++
			f.doneWh += p.EnergyWh
			f.doneCost += cost
		}
	}
	out := make([]energyFile, 0, len(order))
	for _, f := range order {
		f.EnergyWh = round3(f.EnergyWh)
		if d.priced {
			cost := roundCost(f.cost)
			f.Cost = &cost
		}
		if f.done > 0 {
			avg := round3(f.doneWh / float64(f.done))
			f.AvgWh = &avg
			if d.priced {
				cost := roundCost(f.doneCost / float64(f.done))
				f.AvgCost = &cost
			}
		}
		out = append(out, f.energyFile)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.EnergyWh != b.EnergyWh {
			return a.EnergyWh > b.EnergyWh
		}
		if a.Prints != b.Prints {
			return a.Prints > b.Prints
		}
		return a.FileName < b.FileName
	})
	return out
}

// summary is the answer of GET /energy/summary, from a read of the current
// month with totalsOnly.
func (d *energyData) summary() energySummary {
	sum := energySummary{
		Day:          siteDate(d.now),
		Month:        d.now.Format(monthLayout),
		Currency:     d.settings.Currency,
		PriceSet:     d.priced,
		PriceChanged: d.priceChanged,
		Recording:    d.recording,
		Printers:     []energySummaryPrinter{},
	}
	if price, night, ok := d.book.priceAt(d.now.UTC().Truncate(slotLength).Unix()); ok {
		sum.PriceNow, sum.NightNow = &price, night
	}
	// The period that holds now.
	today := sort.Search(len(d.edges), func(i int) bool { return d.edges[i].After(d.now) }) - 1
	ids := make([]string, 0, len(d.periods))
	for id := range d.periods {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var day, month figuresAcc
	for _, id := range ids {
		var own, ownDay figuresAcc
		for i, a := range d.periods[id] {
			own.add(a)
			if i == today {
				ownDay = a
			}
		}
		day.add(ownDay)
		month.add(own)
		if len(sum.Printers) < reportPrinterLimit {
			sum.Printers = append(sum.Printers, energySummaryPrinter{DeviceID: id, Today: d.figures(ownDay), ThisMonth: d.figures(own)})
		}
	}
	sum.Today, sum.ThisMonth = d.figures(day), d.figures(month)
	return sum
}

func (m *module) buildEnergySummary(visible map[string]string, now time.Time) (energySummary, error) {
	d, err := m.collectEnergy(energyQuery{span: "month", visible: visible, now: now, totalsOnly: true})
	if err != nil {
		return energySummary{}, err
	}
	return d.summary(), nil
}

func (m *module) buildEnergyReport(q energyQuery) (energyReport, error) {
	d, err := m.collectEnergy(q)
	if err != nil {
		return energyReport{}, err
	}
	return d.report(), nil
}

// --- CSV ---

var (
	periodsHeader = []string{"period_start", "period_end", "printer", "printer_id", "energy_kwh", "night_energy_kwh", "cost", "currency", "plug_on_hours", "measured_hours", "observed_hours"}
	filesHeader   = []string{"file", "prints", "not_finished", "measured_prints", "energy_kwh", "cost", "currency", "average_energy_kwh", "average_cost"}
	printsHeader  = []string{"started", "ended", "printer", "printer_id", "file", "job_id", "outcome", "duration_s", "energy_kwh", "night_energy_kwh", "cost", "currency", "peak_w", "measured_s", "end_estimated"}
)

// csvText guards a text cell of unknown origin: a spreadsheet reads a cell
// that starts with one of these characters as a formula. File names come
// from G-code.
func csvText(s string) string {
	s = clip(s)
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func csvTime(t time.Time) string { return t.Format("2006-01-02T15:04:05-07:00") }

func csvNumber(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// csvRows are the rows of one table of the export, with every row the page
// counts and no cap. An empty cell is "no price" in cost and currency, and
// "not measured" in energy_kwh; a measured zero is 0.0000.
func (d *energyData) csvRows(table string) (header []string, rows [][]string) {
	currency := ""
	if d.priced {
		currency = csvText(d.settings.Currency)
	}
	cells := func(wh, nightWh float64, cost *float64, measured bool) (energy, night, price, label string) {
		if !measured {
			return "", "", "", ""
		}
		energy = csvNumber(wh/1000, 4)
		if d.night {
			night = csvNumber(nightWh/1000, 4)
		}
		if cost != nil {
			price, label = csvNumber(*cost, 4), currency
		}
		return energy, night, price, label
	}

	if table == "prints" {
		for _, p := range d.prints {
			energy, night, price, label := cells(p.EnergyWh, p.NightWh, p.Cost, p.MeteredSec > 0)
			rows = append(rows, []string{
				csvTime(p.StartedAt), csvTime(p.EndedAt), csvText(p.Name), csvText(p.DeviceID),
				csvText(p.FileName), csvText(p.JobID), csvText(p.Outcome), strconv.Itoa(p.DurationSec),
				energy, night, price, label,
				csvNumber(p.PeakWatts, 1), strconv.Itoa(p.MeteredSec), strconv.FormatBool(p.EndEstimated),
			})
		}
		return printsHeader, rows
	}

	if table == "files" {
		amount := func(v *float64) string {
			if v == nil {
				return ""
			}
			return csvNumber(*v, 4)
		}
		for _, f := range d.files() {
			energy, _, price, label := cells(f.EnergyWh, 0, f.Cost, f.Measured > 0)
			average := ""
			if f.AvgWh != nil {
				average = csvNumber(*f.AvgWh/1000, 4)
			}
			rows = append(rows, []string{
				csvText(f.FileName), strconv.Itoa(f.Prints), strconv.Itoa(f.NotFinished), strconv.Itoa(f.Measured),
				energy, price, label, average, amount(f.AvgCost),
			})
		}
		return filesHeader, rows
	}

	ids := d.printerIDs()
	for i := 0; i+1 < len(d.edges); i++ {
		for _, id := range ids {
			a := d.periods[id][i]
			if !d.shown(id) || a.obs <= 0 {
				continue
			}
			f := d.figures(a)
			// A plug that was off the whole time is a real zero. One that
			// was on with no reading was not measured.
			energy, night, price, label := cells(a.wh, a.nightWh, f.Cost, a.on <= 0 || a.met > 0)
			rows = append(rows, []string{
				csvTime(d.edges[i]), csvTime(d.edges[i+1]), csvText(d.names[id]), csvText(id),
				energy, night, price, label,
				csvNumber(a.on/3600, 2), csvNumber(a.met/3600, 2), csvNumber(a.obs/3600, 2),
			})
		}
	}
	return periodsHeader, rows
}

func csvLine(row []string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	_ = w.Write(row)
	w.Flush()
	return buf.String()
}

// writeEnergyCSV writes the rows from offset on as CSV text: UTF-8, CRLF,
// comma, RFC 4180 quoting. A part ends after exportChunkRows rows or
// exportChunkBytes bytes, counted as the answer carries them, so one answer
// stays under the host's bound of 1 MiB. next is the offset of the part
// after this one, 0 when this was the last.
func writeEnergyCSV(header []string, rows [][]string, offset int) (text string, next int) {
	var b strings.Builder
	if offset <= 0 {
		offset = 0
		b.WriteString(csvLine(header))
	}
	size, written := b.Len(), 0
	for i := offset; i < len(rows); i++ {
		line := csvLine(rows[i])
		// A character can take up to six bytes in the JSON answer.
		encoded, _ := json.Marshal(line)
		if written > 0 && (written >= exportChunkRows || size+len(encoded) > exportChunkBytes) {
			return b.String(), i
		}
		b.WriteString(line)
		size += len(encoded)
		written++
	}
	return b.String(), 0
}

func (d *energyData) export(table string, offset int) energyExport {
	kind := "energy"
	if table == "prints" || table == "files" {
		kind = table
	}
	header, rows := d.csvRows(table)
	text, next := writeEnergyCSV(header, rows, offset)
	return energyExport{
		FileName:   fmt.Sprintf("power-%s-%s-%s.csv", kind, d.span, siteDate(d.from)),
		CSV:        text,
		NextOffset: next,
		AsOf:       d.now.Unix(),
	}
}

// --- Routes ---

// energyQueryOf reads and checks the query of both energy reads, before the
// host is asked anything.
func energyQueryOf(r *http.Request) (q energyQuery, table string, offset int, err error) {
	values := r.URL.Query()
	q.span, q.at, q.device = values.Get("span"), values.Get("at"), values.Get("device")
	switch q.span {
	case "", "day", "week", "month", "year":
	default:
		return q, "", 0, fmt.Errorf("%w: span must be day, week, month or year", errEnergyQuery)
	}
	if q.at != "" {
		if _, err = time.Parse(dayLayout, q.at); err != nil {
			return q, "", 0, fmt.Errorf("%w: at must be a date such as 2026-11-21", errEnergyQuery)
		}
	}
	switch table = values.Get("table"); table {
	case "":
		table = "periods"
	case "periods", "prints", "files":
	default:
		return q, "", 0, fmt.Errorf("%w: table must be periods, prints or files", errEnergyQuery)
	}
	if raw := values.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			return q, "", 0, fmt.Errorf("%w: offset must be a row number, 0 or more", errEnergyQuery)
		}
	}
	if raw := values.Get("as_of"); raw != "" {
		if q.asOf, err = strconv.ParseInt(raw, 10, 64); err != nil || q.asOf <= 0 {
			return q, "", 0, fmt.Errorf("%w: as_of must be the as_of of the first part", errEnergyQuery)
		}
	}
	return q, table, offset, nil
}

// exportMoment sets the moment an export is read for. Every part of one
// download is read for the moment of its first part, which that part answers
// as as_of and the page sends back. Else a print that ends between two parts
// moves every row down by one, and the joined file lists a row twice.
func exportMoment(q *energyQuery, now time.Time) error {
	moment := now.Truncate(time.Second)
	if q.asOf != 0 {
		moment = time.Unix(q.asOf, 0)
		if moment.After(now) || now.Sub(moment) > exportAsOfWindow {
			return fmt.Errorf("%w: as_of must be the as_of of the first part, and that part at most an hour old", errEnergyQuery)
		}
	}
	q.now, q.snapshot = moment, true
	return nil
}

// collectEnergyFor reads the energy record for the person behind r, kept to
// the printers that person may see. It answers the refusal itself. export
// reads it for one part of a CSV download.
func (m *module) collectEnergyFor(w http.ResponseWriter, r *http.Request, export bool) (d *energyData, table string, offset int, ok bool) {
	q, table, offset, err := energyQueryOf(r)
	if err == nil {
		q.now = m.clock()
		if export {
			err = exportMoment(&q, q.now)
		}
	}
	if err != nil {
		refuse(w, r, InvalidInput.New(strings.TrimPrefix(err.Error(), errEnergyQuery.Error()+": ")))
		return nil, "", 0, false
	}
	person := m.principalOf(r)
	if q.visible, err = m.visiblePrinters(r.Context(), person); err != nil {
		refuse(w, r, InternalError.New(err.Error()))
		return nil, "", 0, false
	}
	// Every printer Connect lists, to tell a removed printer from one this
	// person may not see. Without that list no printer counts as removed.
	if all, listErr := m.visiblePrinters(r.Context(), ""); listErr == nil {
		q.known = make(map[string]bool, len(all))
		for id := range all {
			q.known[id] = true
		}
	}
	if d, err = m.collectEnergy(q); err != nil {
		if errors.Is(err, errEnergyQuery) {
			refuse(w, r, InvalidInput.New(strings.TrimPrefix(err.Error(), errEnergyQuery.Error()+": ")))
		} else {
			refuse(w, r, InternalError.New(err.Error()))
		}
		return nil, "", 0, false
	}
	return d, table, offset, true
}

// energyRoutes are the routes of the Energy tab. The three reads ask nothing
// more than the host already took, and answer only printers the caller may
// see.
func (m *module) energyRoutes(mux *http.ServeMux) {
	m.energySettingsRoutes(mux)

	mux.HandleFunc("GET /energy", func(w http.ResponseWriter, r *http.Request) {
		d, _, _, ok := m.collectEnergyFor(w, r, false)
		if !ok {
			return
		}
		rep := d.report()
		rep.CanConfigure = m.mayHint(r.Context(), m.principalOf(r), mayConfigure, "")
		answer(w, http.StatusOK, rep)
	})

	mux.HandleFunc("GET /energy/summary", func(w http.ResponseWriter, r *http.Request) {
		visible, err := m.visiblePrinters(r.Context(), m.principalOf(r))
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		sum, err := m.buildEnergySummary(visible, m.clock())
		if err != nil {
			refuse(w, r, InternalError.New(err.Error()))
			return
		}
		answer(w, http.StatusOK, sum)
	})

	mux.HandleFunc("GET /energy/export", func(w http.ResponseWriter, r *http.Request) {
		d, table, offset, ok := m.collectEnergyFor(w, r, true)
		if !ok {
			return
		}
		answer(w, http.StatusOK, d.export(table, offset))
	})
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the core does not embed the zone database, and a container often has none
	"unicode"
	"unicode/utf8"
)

// The energy record: what every printer's plug used, kept in files under the
// granted data directory.
//
//	energy.json                    EnergySettings
//	energy/days/2026-11-21.json    one file per UTC day
//	energy/prints/2026-11.json     one file per UTC month a print ended in
//
// The files hold energy and time, never money. Cost is worked out when a
// report is read (energy_report.go), so a price or a time zone that changes
// later applies to what is already recorded.
//
// A day is cut into 96 slots of 15 minutes, keyed in UTC. Every zone offset
// and every daylight-saving step in use is a multiple of 15 minutes, so a day
// and the night hours of any zone are whole slots.
//
// Three facts stay apart: a time with no record (the extension was not
// running, or the plug did not answer), a time the plug was on but not
// measured, and a measured figure, which may be a real zero.

const (
	energyDirName      = "energy"
	energyDaysDir      = "days"
	energyPrintsDir    = "prints"
	energySettingsFile = "energy.json"
	energyFileVersion  = 1

	slotSec     = 900
	slotsPerDay = 96
	slotLength  = slotSec * time.Second
	dayLength   = 24 * time.Hour
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"

	energyFlushEvery = 60 * time.Second
	meterStaleAfter  = 90 * time.Second
	minSaneYear      = 2025    // an earlier clock is wrong: nothing is recorded
	maxSaneWatts     = 100_000 // more than any plug switches: such a reading is not counted

	// An automatic prune runs only while this many days of record lie inside
	// the keep time. A clock that jumped ahead leaves fewer.
	pruneTrustDays = 3

	defaultKeepDays  = 365
	minKeepDays      = 7
	maxKeepDays      = 3650
	defaultCurrency  = "EUR"
	maxCurrencyRunes = 8
	maxPrice         = 1000.0
	maxTariffs       = 100

	maxPrintsPerMonth  = 5000      // per printer
	maxCachedSlotRows  = 1_600_000 // parsed day files kept in memory, 20 bytes a row: 32 MB
	reportPrintLimit   = 200
	reportPrinterLimit = 500
	reportFileLimit    = 200
	exportChunkRows    = 2000
	exportChunkBytes   = 600 << 10
)

// slotRow is one printer's quarter hour:
// [slot 0..95, Wh, observed s, plug-on s, metered s, flags, split s, print Wh].
// flags is 0. Bit 1 is kept for energy read from the plug's own counter.
// "split s" is the observed time for which the build that recorded it said
// whether a print ran, and "print Wh" the energy that went into a print
// record in that time. An earlier build wrote six values, which reads as a
// split time of 0: not recorded. It also drops the two values from every row
// of a day it writes again, which reads the same way.
type slotRow [8]float64

type dayFile struct {
	V       int                  `json:"v"`
	Day     string               `json:"day"`     // "2026-11-21", UTC
	Devices map[string][]slotRow `json:"devices"` // ascending by slot, only slots with observed time
}

type printsFile struct {
	V       int                           `json:"v"`
	Month   string                        `json:"month"`  // "2026-11", UTC
	Prints  map[string][]PrintPowerRecord `json:"prints"` // by printer, oldest first
	Dropped map[string]int                `json:"dropped,omitempty"`
}

// slotAcc is the part of a slot that is not on disk yet.
// Kn is the observed time the print share is known for, and Prt that share.
type slotAcc struct{ Wh, Obs, On, Met, Kn, Prt float64 }

// splitExact says the print share covers all the slot holds, and not only
// the part a build that keeps it recorded. A slot with no split time at all
// is one of an earlier build, however short it is.
func (a slotAcc) splitExact() bool {
	return a.Kn > 0 && a.Kn >= a.Obs-1
}

func (a *slotAcc) add(b slotAcc) {
	a.Wh += b.Wh
	a.Obs += b.Obs
	a.On += b.On
	a.Met += b.Met
	a.Kn += b.Kn
	a.Prt += b.Prt
}

type pendingPrint struct {
	DeviceID string
	Record   PrintPowerRecord
}

type energyStore struct {
	// Under module.mu.
	pending       map[string]map[string]*[slotsPerDay]slotAcc // day -> printer -> slots
	pendingPrints []pendingPrint
	lastFlush     time.Time
	lastPruneDay  string

	// Under io. io is taken first; module.mu may be taken inside it for a
	// short copy. io is never taken while module.mu is held.
	io         sync.Mutex
	days       map[string]*cachedDay // parsed day files, written through on flush
	cachedRows int
	uses       uint64
	firstDay   string // oldest day file, "" when none
	// keepUnsure is set when energy.json was there but could not be used, so
	// the keep time in memory is the default and not what a person chose.
	// Nothing is deleted for its age until a keep time is saved again.
	keepUnsure bool
}

// cachedSlot is a slotRow packed for memory. Seconds are kept in tenths.
type cachedSlot struct {
	Slot         uint8
	Wh           float32
	Obs, On, Met uint16
	Kn           uint16
	Prt          float32
}

func (c cachedSlot) acc() slotAcc {
	return slotAcc{
		Wh: float64(c.Wh), Obs: float64(c.Obs) / 10, On: float64(c.On) / 10, Met: float64(c.Met) / 10,
		Kn: float64(c.Kn) / 10, Prt: float64(c.Prt),
	}
}

type cachedDay struct {
	devices map[string][]cachedSlot // nil map: a file of a newer version, which is not read
	used    uint64                  // counter for least-recently-used eviction
	stamp   fileStamp               // the file as it was when it was read or written
}

// fileStamp tells whether a file changed since it was read. A day file that
// was put there or replaced while the extension runs, from a backup for one,
// is then read again and not answered from the cache.
type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mod: info.ModTime(), size: info.Size()}
}

func (a fileStamp) same(b fileStamp) bool {
	return a.exists == b.exists && a.size == b.size && a.mod.Equal(b.mod)
}

// energyStep is what one tick adds. It is decided in one place, so the
// session, the print and the slot never disagree. Reading the plug's own
// counter later changes only this.
type energyStep struct {
	wh       float64
	observed bool // relay state known and the plug reachable
	on       bool
	metered  bool // on, with a fresh reading
	// printing says the energy of this step went into a print record. It is
	// set where that is done, so the slots and the print records agree.
	printing bool
}

// plugStaleLocked says the plug cannot be told to be in the state memory
// holds: it is reported away, or its broker is not connected. Such a plug
// keeps its last state and reading, which must not count.
func (m *module) plugStaleLocked(cfg DeviceConfig, p *PlugTelemetry) bool {
	if p.Online != nil && !*p.Online {
		return true
	}
	return cfg.usesMQTT() && !m.mqttUp
}

// meterWatts is the plug's power reading when it can be counted: a number
// from 0 to maxSaneWatts. "nan" is what some firmware reports for a sensor
// with no value, and one such reading in a sum would leave no number in the
// session, the print, the slot and every report of that day.
func meterWatts(p *PlugTelemetry) (float64, bool) {
	if p.PowerWatts == nil {
		return 0, false
	}
	w := *p.PowerWatts
	if !(w >= 0 && w <= maxSaneWatts) { // false for NaN too
		return 0, false
	}
	return w, true
}

// finiteOr answers v, or fallback when v is not a number a file can hold.
func finiteOr(v, fallback float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fallback
	}
	return v
}

func (m *module) energyStepLocked(cfg DeviceConfig, p *PlugTelemetry, now time.Time, dt time.Duration, plugOn bool) energyStep {
	if dt <= 0 || m.plugStaleLocked(cfg, p) {
		return energyStep{}
	}
	if !plugOn {
		return energyStep{observed: true}
	}
	step := energyStep{observed: true, on: true}
	watts, ok := meterWatts(p)
	if !ok {
		return step
	}
	// Tasmota over MQTT is asked for its reading on every tick, so a reading
	// that stopped coming is an old one. The other plugs report when they
	// like; one under a steady load may say nothing for a long time.
	if cfg.effectiveDriver() == DriverTasmota && !p.meterAt.IsZero() && now.Sub(p.meterAt) > meterStaleAfter {
		return step
	}
	step.metered = true
	step.wh = watts * dt.Hours()
	return step
}

// --- Recording (the caller holds m.mu) ---

// recordEnergyLocked adds one step to the slots between from and to.
func (m *module) recordEnergyLocked(deviceID string, from, to time.Time, step energyStep) {
	if m.dataDir == "" || !step.observed || !to.After(from) || to.UTC().Year() < minSaneYear {
		return
	}
	total := to.Sub(from).Seconds()
	// One energy that is not a number would block the file of its day.
	wh := finiteOr(step.wh, 0)
	for cur := from; cur.Before(to); {
		start := cur.UTC().Truncate(slotLength)
		end := start.Add(slotLength)
		if end.After(to) {
			end = to
		}
		sec := end.Sub(cur).Seconds()
		cur = end

		day := start.Format(dayLayout)
		devices := m.energy.pending[day]
		if devices == nil {
			devices = make(map[string]*[slotsPerDay]slotAcc)
			m.energy.pending[day] = devices
		}
		slots := devices[deviceID]
		if slots == nil {
			slots = new([slotsPerDay]slotAcc)
			devices[deviceID] = slots
		}
		acc := &slots[start.Hour()*4+start.Minute()/15]
		acc.Obs += sec
		if step.on {
			acc.On += sec
		}
		if step.metered {
			acc.Met += sec
		}
		acc.Wh += wh * sec / total
		acc.Kn += sec
		if step.printing {
			acc.Prt += wh * sec / total
		}
	}
}

// queuePrintLocked keeps a finished print for the next flush.
func (m *module) queuePrintLocked(deviceID string, rec PrintPowerRecord) {
	if m.dataDir == "" || rec.EndedAt == nil || rec.EndedAt.UTC().Year() < minSaneYear {
		return
	}
	ended := *rec.EndedAt
	rec.EndedAt = &ended
	rec.Active = false
	m.energy.pendingPrints = append(m.energy.pendingPrints, pendingPrint{DeviceID: deviceID, Record: rec})
}

// energyFlushDue is true when a print is queued, or when slots are pending
// and the last flush is energyFlushEvery old.
func (m *module) energyFlushDue(now time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.energy.pendingPrints) > 0 {
		return true
	}
	return len(m.energy.pending) > 0 && now.Sub(m.energy.lastFlush) >= energyFlushEvery
}

func (m *module) flushEnergyIfDue() {
	if !m.energyFlushDue(m.clock()) {
		return
	}
	if err := m.saveEnergy(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: save energy: %v\n", logName, err)
	}
}

// --- Files ---

// energyDir is the directory of the energy record, "" with no data directory.
func (m *module) energyDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.dataDir == "" {
		return ""
	}
	return filepath.Join(m.dataDir, energyDirName)
}

func dayPath(dir, day string) string {
	return filepath.Join(dir, energyDaysDir, day+".json")
}

func printsPath(dir, month string) string {
	return filepath.Join(dir, energyPrintsDir, month+".json")
}

// setAside renames a file that could not be read, so it is kept for a person
// to look at and is never written over.
func setAside(path string) error {
	bad := path + ".bad"
	if _, err := os.Lstat(bad); err == nil {
		bad = fmt.Sprintf("%s.bad.%d", path, time.Now().Unix())
	}
	return os.Rename(path, bad)
}

type storedState int

const (
	storedMissing storedState = iota // no file
	storedAside                      // a file that did not parse: set aside, read as no file
	storedRead
	storedNewer // written by a later version: left alone
)

// readStored reads one file of the energy record. A file that does not parse
// is set aside and read as empty. The version is read first and on its own:
// a later version may give the rest of the file another shape, and such a
// file is left alone, not set aside.
func readStored[T any](path string) (T, storedState, error) {
	var out T
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, storedMissing, nil
		}
		return out, storedMissing, err
	}
	var head struct {
		V int `json:"v"`
	}
	if json.Unmarshal(raw, &head) == nil && head.V > energyFileVersion {
		return out, storedNewer, nil
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s does not parse and is set aside: %v\n", logName, filepath.Base(path), err)
		var empty T
		if err = setAside(path); err != nil {
			return empty, storedMissing, err
		}
		return empty, storedAside, nil
	}
	return out, storedRead, nil
}

func readDayFile(path string) (dayFile, storedState, error) {
	return readStored[dayFile](path)
}

func readPrintsFile(path string) (printsFile, storedState, error) {
	return readStored[printsFile](path)
}

func slotIndex(v float64) (int, bool) {
	if v != math.Trunc(v) || v < 0 || v >= slotsPerDay {
		return 0, false
	}
	return int(v), true
}

func clampSec(v float64) float64 {
	return math.Max(0, math.Min(slotSec, v))
}

// unpackRows reads a printer's rows of a day file into slots.
func unpackRows(rows []slotRow) (slots [slotsPerDay]slotAcc, flags [slotsPerDay]float64) {
	for _, row := range rows {
		i, ok := slotIndex(row[0])
		if !ok {
			continue
		}
		slots[i].add(slotAcc{
			Wh: row[1], Obs: clampSec(row[2]), On: clampSec(row[3]), Met: clampSec(row[4]),
			Kn: clampSec(row[6]), Prt: math.Max(0, finiteOr(row[7], 0)),
		})
		flags[i] = row[5]
	}
	return slots, flags
}

// mergeDay adds the pending part to what a day file holds. Energy is rounded
// to three decimals and seconds to one, here and nowhere before, so no tick
// is rounded on its own.
func mergeDay(f *dayFile, day string, part map[string]*[slotsPerDay]slotAcc) {
	f.V = energyFileVersion
	f.Day = day
	if f.Devices == nil {
		f.Devices = make(map[string][]slotRow)
	}
	for id, add := range part {
		slots, flags := unpackRows(f.Devices[id])
		rows := make([]slotRow, 0, slotsPerDay)
		for i := range slots {
			slots[i].add(add[i])
			obs := math.Round(clampSec(slots[i].Obs)*10) / 10
			if obs <= 0 {
				continue
			}
			wh := finiteOr(math.Round(slots[i].Wh*1000)/1000, 0)
			rows = append(rows, slotRow{
				float64(i),
				wh,
				obs,
				math.Round(clampSec(slots[i].On)*10) / 10,
				math.Round(clampSec(slots[i].Met)*10) / 10,
				flags[i],
				// The print share: never for more time than was observed,
				// and never more energy than the slot holds.
				math.Min(obs, math.Round(clampSec(slots[i].Kn)*10)/10),
				math.Max(0, math.Min(wh, finiteOr(math.Round(slots[i].Prt*1000)/1000, 0))),
			})
		}
		if len(rows) > 0 {
			f.Devices[id] = rows
		}
	}
}

func packRows(rows []slotRow) []cachedSlot {
	slots, _ := unpackRows(rows)
	out := make([]cachedSlot, 0, len(rows))
	for i, a := range slots {
		if a.Obs <= 0 {
			continue
		}
		// A figure a float32 cannot hold would be infinite in the cache, and
		// no report with it could be answered.
		wh := math.Max(-math.MaxFloat32, math.Min(math.MaxFloat32, a.Wh))
		out = append(out, cachedSlot{
			Slot: uint8(i),
			Wh:   float32(wh),
			Obs:  uint16(math.Round(a.Obs * 10)),
			On:   uint16(math.Round(a.On * 10)),
			Met:  uint16(math.Round(a.Met * 10)),
			Kn:   uint16(math.Round(a.Kn * 10)),
			Prt:  float32(math.Max(0, math.Min(math.MaxFloat32, a.Prt))),
		})
	}
	return out
}

// putCacheLocked keeps a parsed day. devices is nil for a file that is there
// but not read, one of a newer version.
func (s *energyStore) putCacheLocked(day string, devices map[string][]slotRow, stamp fileStamp) map[string][]cachedSlot {
	s.dropCacheLocked(day)
	entry := &cachedDay{stamp: stamp}
	if devices != nil {
		entry.devices = make(map[string][]cachedSlot, len(devices))
		for id, rows := range devices {
			packed := packRows(rows)
			entry.devices[id] = packed
			s.cachedRows += len(packed)
		}
	}
	s.uses++
	entry.used = s.uses
	s.days[day] = entry

	// Past the bound, the days read longest ago go, but never the day just
	// read and never the current day, which every flush writes through.
	today := time.Now().UTC().Format(dayLayout)
	for s.cachedRows > maxCachedSlotRows {
		oldest := ""
		for name, c := range s.days {
			if name == day || name == today {
				continue
			}
			if oldest == "" || c.used < s.days[oldest].used {
				oldest = name
			}
		}
		if oldest == "" {
			break
		}
		s.dropCacheLocked(oldest)
	}
	return entry.devices
}

func (s *energyStore) dropCacheLocked(day string) {
	entry := s.days[day]
	if entry == nil {
		return
	}
	for _, rows := range entry.devices {
		s.cachedRows -= len(rows)
	}
	delete(s.days, day)
}

// dayLocked answers a day's slots from the cache, else from the disk. The
// caller holds io. dir is the energy directory.
func (s *energyStore) dayLocked(dir, day string) (map[string][]cachedSlot, error) {
	path := dayPath(dir, day)
	stamp := stampOf(path)
	if entry := s.days[day]; entry != nil && entry.stamp.same(stamp) {
		s.uses++
		entry.used = s.uses
		return entry.devices, nil
	}
	f, state, err := readDayFile(path)
	if err != nil {
		return nil, err
	}
	if state == storedNewer {
		// Kept, so the file is not read again on every report. There is one
		// such entry for a file at most.
		return s.putCacheLocked(day, nil, stamp), nil
	}
	if state != storedRead {
		// No file, or one that was set aside. That is not kept: a day with no
		// file has no row, so nothing would ever push its entry out, and a
		// report can ask for any day.
		s.dropCacheLocked(day)
		return nil, nil
	}
	if f.Devices == nil {
		f.Devices = map[string][]slotRow{}
	}
	return s.putCacheLocked(day, f.Devices, stamp), nil
}

// writeDayLocked adds the pending part of one day to its file. It reads the
// file, not the packed cache, so nothing is lost to the cache's precision.
func (s *energyStore) writeDayLocked(dir, day string, part map[string]*[slotsPerDay]slotAcc) error {
	path := dayPath(dir, day)
	f, state, err := readDayFile(path)
	if err != nil {
		return err
	}
	if state == storedNewer {
		fmt.Fprintf(os.Stderr, "%s: %s was written by a newer version and is left alone; the energy of that day is not kept\n", logName, filepath.Base(path))
		return nil
	}
	mergeDay(&f, day, part)
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err = writeFileAtomic(path, raw); err != nil {
		return err
	}
	s.putCacheLocked(day, f.Devices, stampOf(path))
	if s.firstDay == "" || day < s.firstDay {
		s.firstDay = day
	}
	return nil
}

// printStarts is the start second of every print a printer has in a file.
// A print is known by its printer and that second.
type printStarts map[string]map[int64]bool

func startsOf(f printsFile) printStarts {
	out := make(printStarts, len(f.Prints))
	for id, list := range f.Prints {
		out[id] = make(map[int64]bool, len(list))
		for _, rec := range list {
			out[id][rec.StartedAt.Unix()] = true
		}
	}
	return out
}

func (p printStarts) holds(id string, rec PrintPowerRecord) bool {
	return p[id][rec.StartedAt.Unix()]
}

func (p printStarts) add(id string, rec PrintPowerRecord) {
	if p[id] == nil {
		p[id] = make(map[int64]bool)
	}
	p[id][rec.StartedAt.Unix()] = true
}

// endOf is when a stored print ended, the zero time for a record with no end.
func endOf(rec PrintPowerRecord) time.Time {
	if rec.EndedAt == nil {
		return time.Time{}
	}
	return *rec.EndedAt
}

// writePrintsLocked adds finished prints to the file of the month each ended
// in. A print is stored once: one with the same printer and the same start
// second that is already there wins. It answers the prints it could not
// write.
func (s *energyStore) writePrintsLocked(dir string, prints []pendingPrint) (failed []pendingPrint, err error) {
	byMonth := make(map[string][]pendingPrint)
	for _, p := range prints {
		month := p.Record.EndedAt.UTC().Format(monthLayout)
		byMonth[month] = append(byMonth[month], p)
	}
	months := make([]string, 0, len(byMonth))
	for month := range byMonth {
		months = append(months, month)
	}
	sort.Strings(months)

	var errs []error
	for _, month := range months {
		if writeErr := writePrintsMonth(dir, month, byMonth[month]); writeErr != nil {
			errs = append(errs, fmt.Errorf("prints of %s: %w", month, writeErr))
			failed = append(failed, byMonth[month]...)
		}
	}
	return failed, errors.Join(errs...)
}

func writePrintsMonth(dir, month string, prints []pendingPrint) error {
	path := printsPath(dir, month)
	f, state, err := readPrintsFile(path)
	if err != nil {
		return err
	}
	if state == storedNewer {
		fmt.Fprintf(os.Stderr, "%s: %s was written by a newer version and is left alone; %d prints are not kept\n", logName, filepath.Base(path), len(prints))
		return nil
	}
	f.V = energyFileVersion
	f.Month = month
	if f.Prints == nil {
		f.Prints = make(map[string][]PrintPowerRecord)
	}

	// A print that crossed a month edge may already be in the file of the
	// month it started in, from an end that was found twice.
	have := startsOf(f)
	started := make(map[string]printStarts)
	touched := make(map[string]bool)
	changed := false
	for _, p := range prints {
		rec := p.Record
		if have.holds(p.DeviceID, rec) {
			continue
		}
		if other := rec.StartedAt.UTC().Format(monthLayout); other != month {
			of, seen := started[other]
			if !seen {
				stored, _, readErr := readPrintsFile(printsPath(dir, other))
				if readErr != nil {
					return readErr
				}
				of = startsOf(stored)
				started[other] = of
			}
			if of.holds(p.DeviceID, rec) {
				continue
			}
		}
		changed = true
		if len(f.Prints[p.DeviceID]) >= maxPrintsPerMonth {
			if f.Dropped == nil {
				f.Dropped = make(map[string]int)
			}
			f.Dropped[p.DeviceID]++
			continue
		}
		have.add(p.DeviceID, rec)
		f.Prints[p.DeviceID] = append(f.Prints[p.DeviceID], rec)
		touched[p.DeviceID] = true
	}
	for id := range touched {
		list := f.Prints[id]
		sort.SliceStable(list, func(i, j int) bool { return endOf(list[i]).Before(endOf(list[j])) })
	}
	if !changed {
		return nil
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, raw)
}

// saveEnergy writes what is pending. It reads each file, adds the pending
// part and writes it back, and nothing leaves memory before its write
// succeeded: the part of a file that could not be written goes back to
// pending.
func (m *module) saveEnergy() error {
	s := &m.energy
	s.io.Lock()
	defer s.io.Unlock()

	now := m.clock()
	m.mu.Lock()
	dataDir := m.dataDir
	pending, prints := s.pending, s.pendingPrints
	s.pending = make(map[string]map[string]*[slotsPerDay]slotAcc)
	s.pendingPrints = nil
	s.lastFlush = now
	m.mu.Unlock()
	if dataDir == "" {
		return nil
	}
	dir := filepath.Join(dataDir, energyDirName)

	var errs []error
	if len(pending) > 0 {
		days := make([]string, 0, len(pending))
		for day := range pending {
			days = append(days, day)
		}
		sort.Strings(days)
		mkErr := os.MkdirAll(filepath.Join(dir, energyDaysDir), 0o700)
		for _, day := range days {
			err := mkErr
			if err == nil {
				err = s.writeDayLocked(dir, day, pending[day])
			}
			if err == nil {
				continue
			}
			errs = append(errs, fmt.Errorf("day %s: %w", day, err))
			m.mu.Lock()
			m.restorePendingLocked(day, pending[day])
			m.mu.Unlock()
		}
	}

	if len(prints) > 0 {
		failed := prints
		err := os.MkdirAll(filepath.Join(dir, energyPrintsDir), 0o700)
		if err == nil {
			failed, err = s.writePrintsLocked(dir, prints)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if len(failed) > 0 {
			m.mu.Lock()
			s.pendingPrints = append(failed, s.pendingPrints...)
			m.mu.Unlock()
		}
	}

	today := now.UTC().Format(dayLayout)
	m.mu.Lock()
	pruneDue := s.lastPruneDay != today
	s.lastPruneDay = today
	m.mu.Unlock()
	if pruneDue {
		m.pruneEnergyLocked(dir, now, false)
	}
	return errors.Join(errs...)
}

// restorePendingLocked puts back the part of a day that could not be written,
// beside what was recorded since.
func (m *module) restorePendingLocked(day string, part map[string]*[slotsPerDay]slotAcc) {
	devices := m.energy.pending[day]
	if devices == nil {
		m.energy.pending[day] = part
		return
	}
	for id, slots := range part {
		have := devices[id]
		if have == nil {
			devices[id] = slots
			continue
		}
		for i := range slots {
			have[i].add(slots[i])
		}
	}
}

// loadEnergy reads the energy settings and looks at what the data directory
// holds. It loads no slot, so nothing memory holds is replaced.
func (m *module) loadEnergy() {
	m.mu.RLock()
	dataDir := m.dataDir
	m.mu.RUnlock()
	if dataDir == "" {
		return
	}
	s := &m.energy
	s.io.Lock()
	defer s.io.Unlock()

	// The defaults count when there is no file. When there is one that cannot
	// be used, they count too, but the keep time among them is then not one a
	// person chose, and it must not delete anything.
	settings := defaultEnergySettings()
	path := filepath.Join(dataDir, energySettingsFile)
	loaded, state, err := readStored[EnergySettings](path)
	unsure := true
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: read energy settings: %v\n", logName, err)
	case state == storedRead:
		settings = normalizeEnergySettings(loaded)
		unsure = false
	case state == storedMissing:
		unsure = false
	case state == storedNewer:
		fmt.Fprintf(os.Stderr, "%s: %s was written by a newer version and is set aside\n", logName, energySettingsFile)
		if err = setAside(path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: set %s aside: %v\n", logName, energySettingsFile, err)
		}
	}
	m.energyHeld.Store(&settings)
	s.keepUnsure = unsure
	if unsure {
		fmt.Fprintf(os.Stderr, "%s: the saved energy settings could not be used; no energy record is deleted for its age until the keep time is saved again\n", logName)
	}

	s.days = make(map[string]*cachedDay)
	s.cachedRows = 0
	s.firstDay = ""

	dir := filepath.Join(dataDir, energyDirName)
	for _, sub := range []string{energyDaysDir, energyPrintsDir} {
		entries, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, entry := range entries {
			// What a write that was cut off left behind.
			if strings.HasSuffix(entry.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(dir, sub, entry.Name()))
			}
		}
	}
	m.pruneEnergyLocked(dir, m.clock(), false)
	m.prices.reset()
}

// pruneEnergy deletes what is older than the keep time, the way the start
// and the first flush of a day do.
func (m *module) pruneEnergy(now time.Time) {
	dir := m.energyDir()
	if dir == "" {
		return
	}
	m.energy.io.Lock()
	defer m.energy.io.Unlock()
	m.pruneEnergyLocked(dir, now, false)
}

// keepCut is the moment before which no slot is shown, and before which
// whole files are deleted.
func keepCut(settings EnergySettings, now time.Time) time.Time {
	return now.Add(-time.Duration(settings.KeepDays) * dayLength)
}

// pruneEnergyLocked deletes the day files that lie wholly before the keep
// time and the print files of months that do. The day the keep time cuts
// through stays; a report leaves out its older slots. The caller holds io.
//
// asked is a prune a person asked for, by saving a shorter keep time. Every
// other prune goes by the clock alone, and deleting cannot be undone, so it
// holds back in two cases. It deletes nothing while the keep time is not one
// a person chose (keepUnsure). And it deletes nothing while fewer than
// pruneTrustDays days of record lie inside the keep time: a clock that
// jumped ahead by more than the keep time makes every file look old, and
// would otherwise take the whole record within a minute. A clock that stays
// there writes its own days, and the prune runs again once it has.
func (m *module) pruneEnergyLocked(dir string, now time.Time, asked bool) {
	s := &m.energy
	defer s.refreshFirstDayLocked(dir)
	if now.UTC().Year() < minSaneYear || (s.keepUnsure && !asked) {
		return
	}
	cut := keepCut(m.energySettings(), now)

	// old lists the files of a directory that lie wholly before the keep
	// time, also what was set aside beside them, and counts the record files
	// that stay.
	old := func(sub, layout string, end func(time.Time) time.Time) (names []string, kept int) {
		entries, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, entry := range entries {
			name := entry.Name()
			if len(name) < len(layout) || !strings.HasPrefix(name[len(layout):], ".json") {
				continue
			}
			start, err := time.Parse(layout, name[:len(layout)])
			switch {
			case err != nil:
			case end(start).After(cut):
				if name == name[:len(layout)]+".json" {
					kept++
				}
			default:
				names = append(names, name)
			}
		}
		return names, kept
	}
	days, kept := old(energyDaysDir, dayLayout, func(t time.Time) time.Time { return t.Add(dayLength) })
	months, _ := old(energyPrintsDir, monthLayout, func(t time.Time) time.Time { return t.AddDate(0, 1, 0) })
	if !asked && kept < pruneTrustDays {
		if len(days)+len(months) > 0 {
			fmt.Fprintf(os.Stderr, "%s: %d energy files are older than the keep time, but fewer than %d days of record lie inside it (%d). The clock may be wrong, so nothing is deleted yet\n", logName, len(days)+len(months), pruneTrustDays, kept)
		}
		return
	}

	for _, name := range days {
		if err := os.Remove(filepath.Join(dir, energyDaysDir, name)); err != nil {
			fmt.Fprintf(os.Stderr, "%s: delete %s: %v\n", logName, name, err)
			continue
		}
		s.dropCacheLocked(name[:len(dayLayout)])
	}
	for _, name := range months {
		if err := os.Remove(filepath.Join(dir, energyPrintsDir, name)); err != nil {
			fmt.Fprintf(os.Stderr, "%s: delete %s: %v\n", logName, name, err)
		}
	}
}

// refreshFirstDayLocked reads which day file is the oldest. It lists the
// directory, so files that were put there while the extension runs count.
func (s *energyStore) refreshFirstDayLocked(dir string) {
	first := ""
	entries, _ := os.ReadDir(filepath.Join(dir, energyDaysDir))
	for _, entry := range entries {
		day, isDay := strings.CutSuffix(entry.Name(), ".json")
		if !isDay || entry.IsDir() {
			continue
		}
		if _, err := time.Parse(dayLayout, day); err != nil {
			continue
		}
		if first == "" || day < first {
			first = day
		}
	}
	s.firstDay = first
}

// --- Settings ---

// EnergySettings is what energy.json holds.
type EnergySettings struct {
	V        int      `json:"v"`
	KeepDays int      `json:"keep_days"`
	TimeZone string   `json:"time_zone"` // IANA name, "" follows the server clock
	Currency string   `json:"currency"`  // a label
	Tariffs  []Tariff `json:"tariffs"`   // ascending by From, empty: no price
}

// Tariff is a price and the moment it counts from.
type Tariff struct {
	From       int64   `json:"from"`  // unix seconds of a slot start, 0: from the first record
	Price      float64 `json:"price"` // per kWh
	Night      bool    `json:"night,omitempty"`
	NightPrice float64 `json:"night_price,omitempty"`
	NightStart int     `json:"night_start,omitempty"` // hour 0..23 in the site zone
	NightEnd   int     `json:"night_end,omitempty"`   // hour 0..23, may be before NightStart
}

func defaultEnergySettings() EnergySettings {
	return EnergySettings{
		V:        energyFileVersion,
		KeepDays: defaultKeepDays,
		Currency: defaultCurrency,
		Tariffs:  []Tariff{},
	}
}

func (m *module) energySettings() EnergySettings {
	if s := m.energyHeld.Load(); s != nil {
		return *s
	}
	return defaultEnergySettings()
}

// location is the zone days and night hours follow: the one that is set, else
// the zone of the server process.
func (s EnergySettings) location() *time.Location {
	if s.TimeZone != "" {
		if loc, err := time.LoadLocation(s.TimeZone); err == nil {
			return loc
		}
	}
	return time.Local
}

func validPrice(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= maxPrice
}

func validHour(h int) bool { return h >= 0 && h <= 23 }

func validCurrency(label string) bool {
	if utf8.RuneCountInString(label) > maxCurrencyRunes {
		return false
	}
	return !strings.ContainsFunc(label, unicode.IsControl)
}

func validTariff(t Tariff) bool {
	if t.From < 0 || !validPrice(t.Price) {
		return false
	}
	if !t.Night {
		return true
	}
	return validPrice(t.NightPrice) && validHour(t.NightStart) && validHour(t.NightEnd) && t.NightStart != t.NightEnd
}

// normalizeEnergySettings makes what energy.json held usable. It is for the
// file only; a change a person asks for is checked and refused instead.
func normalizeEnergySettings(s EnergySettings) EnergySettings {
	s.V = energyFileVersion
	switch {
	case s.KeepDays == 0:
		s.KeepDays = defaultKeepDays
	case s.KeepDays < minKeepDays:
		s.KeepDays = minKeepDays
	case s.KeepDays > maxKeepDays:
		s.KeepDays = maxKeepDays
	}
	s.TimeZone = strings.TrimSpace(s.TimeZone)
	if s.TimeZone != "" {
		if _, err := time.LoadLocation(s.TimeZone); err != nil || strings.EqualFold(s.TimeZone, "Local") {
			fmt.Fprintf(os.Stderr, "%s: the saved time zone %q is not known; days follow the server clock\n", logName, s.TimeZone)
			s.TimeZone = ""
		}
	}
	s.Currency = strings.TrimSpace(s.Currency)
	if !validCurrency(s.Currency) {
		s.Currency = defaultCurrency
	}
	tariffs := make([]Tariff, 0, len(s.Tariffs))
	for _, t := range s.Tariffs {
		if !validTariff(t) {
			continue
		}
		if !t.Night {
			t.NightPrice, t.NightStart, t.NightEnd = 0, 0, 0
		}
		tariffs = append(tariffs, t)
	}
	sort.SliceStable(tariffs, func(i, j int) bool { return tariffs[i].From < tariffs[j].From })
	if len(tariffs) > maxTariffs {
		tariffs = tariffs[len(tariffs)-maxTariffs:]
	}
	if len(tariffs) > 0 {
		tariffs[0].From = 0
	}
	s.Tariffs = tariffs
	return s
}

// energySettingsChange is the body of PUT /energy/settings. A field that is
// not sent keeps its saved value.
type energySettingsChange struct {
	KeepDays   *int         `json:"keep_days"`
	Shorten    bool         `json:"shorten"`
	TimeZone   *string      `json:"time_zone"`
	Currency   *string      `json:"currency"`
	Price      *priceChange `json:"price"`
	PriceFrom  string       `json:"price_from"` // "", "now" or "start"
	ClearPrice bool         `json:"clear_price"`
}

type priceChange struct {
	Price      float64 `json:"price"`
	Night      bool    `json:"night"`
	NightPrice float64 `json:"night_price"`
	NightStart int     `json:"night_start"`
	NightEnd   int     `json:"night_end"`
}

func (p priceChange) tariff() Tariff {
	t := Tariff{Price: p.Price}
	if p.Night {
		t.Night, t.NightPrice, t.NightStart, t.NightEnd = true, p.NightPrice, p.NightStart, p.NightEnd
	}
	return t
}

func priceOf(t Tariff) priceChange {
	return priceChange{Price: t.Price, Night: t.Night, NightPrice: t.NightPrice, NightStart: t.NightStart, NightEnd: t.NightEnd}
}

func roundPrice(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// applyPrice puts a price into the list. The first price ever saved, and one
// asked for all history, covers every record. A later price counts from the
// slot it is saved in.
func applyPrice(list []Tariff, p priceChange, from string, now time.Time) []Tariff {
	entry := p.tariff()
	if len(list) == 0 || from == "start" {
		return []Tariff{entry}
	}
	if priceOf(list[len(list)-1]) == priceOf(entry) {
		return list
	}
	entry.From = now.UTC().Truncate(slotLength).Unix()
	out := make([]Tariff, 0, len(list)+1)
	for _, t := range list {
		// An entry of the same slot is replaced. A later one can only come
		// from a clock that was set back, and goes too.
		if t.From < entry.From {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		entry.From = 0
		return []Tariff{entry}
	}
	// A price changed and changed back within one slot is no change.
	if priceOf(out[len(out)-1]) != priceOf(entry) {
		out = append(out, entry)
	}
	for len(out) > maxTariffs {
		out = out[1:]
		out[0].From = 0
	}
	return out
}

// errEnergyInput marks a change that is refused as INVALID_INPUT.
var errEnergyInput = errors.New("energy: invalid input")

func energyInputError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errEnergyInput, fmt.Sprintf(format, args...))
}

// nextEnergySettings checks a change and answers the settings after it.
func nextEnergySettings(cur EnergySettings, c energySettingsChange, now time.Time) (EnergySettings, error) {
	next := cur
	next.V = energyFileVersion
	next.Tariffs = append([]Tariff{}, cur.Tariffs...)

	if c.KeepDays != nil {
		if *c.KeepDays < minKeepDays || *c.KeepDays > maxKeepDays {
			return cur, energyInputError("keep_days must be %d to %d", minKeepDays, maxKeepDays)
		}
		next.KeepDays = *c.KeepDays
	}
	if c.TimeZone != nil {
		zone := strings.TrimSpace(*c.TimeZone)
		if zone != "" {
			if strings.EqualFold(zone, "Local") {
				return cur, energyInputError("time_zone must name a zone, such as Europe/Prague, or be empty")
			}
			if _, err := time.LoadLocation(zone); err != nil {
				return cur, energyInputError("time_zone %q is not a known zone", zone)
			}
		}
		next.TimeZone = zone
	}
	if c.Currency != nil {
		label := strings.TrimSpace(*c.Currency)
		if !validCurrency(label) {
			return cur, energyInputError("currency must be at most %d characters with no control character", maxCurrencyRunes)
		}
		next.Currency = label
	}
	switch c.PriceFrom {
	case "", "now", "start":
	default:
		return cur, energyInputError("price_from must be now or start")
	}
	if c.Price != nil && c.ClearPrice {
		return cur, energyInputError("price and clear_price cannot be sent together")
	}
	if c.ClearPrice {
		next.Tariffs = []Tariff{}
	}
	if c.Price != nil {
		p := *c.Price
		if !validPrice(p.Price) {
			return cur, energyInputError("price must be 0 to %g", maxPrice)
		}
		p.Price = roundPrice(p.Price)
		if p.Night {
			if !validPrice(p.NightPrice) {
				return cur, energyInputError("night_price must be 0 to %g", maxPrice)
			}
			if !validHour(p.NightStart) || !validHour(p.NightEnd) || p.NightStart == p.NightEnd {
				return cur, energyInputError("night_start and night_end must be two different hours from 0 to 23")
			}
			p.NightPrice = roundPrice(p.NightPrice)
		}
		next.Tariffs = applyPrice(next.Tariffs, p, c.PriceFrom, now)
	}
	return next, nil
}

func sameEnergySettings(a, b EnergySettings) bool {
	return a.KeepDays == b.KeepDays && a.TimeZone == b.TimeZone && a.Currency == b.Currency && slices.Equal(a.Tariffs, b.Tariffs)
}

// changeEnergySettings checks a change, saves it and applies it. A refused
// change is an errEnergyInput and saves nothing.
func (m *module) changeEnergySettings(c energySettingsChange) (energySettingsResponse, error) {
	m.mu.RLock()
	dataDir := m.dataDir
	m.mu.RUnlock()

	// io also keeps two changes from being built on the same saved settings.
	s := &m.energy
	s.io.Lock()
	defer s.io.Unlock()

	now := m.clock()
	cur := m.energySettings()
	next, err := nextEnergySettings(cur, c, now)
	if err != nil {
		return energySettingsResponse{}, err
	}
	dir := ""
	if dataDir != "" {
		dir = filepath.Join(dataDir, energyDirName)
		s.refreshFirstDayLocked(dir)
	}
	// While the keep time is not one a person chose, the one that is sent
	// now is the first that counts, whatever memory holds: it is saved also
	// when it is the same number, and it needs the flag like a shorter one.
	chosen := s.keepUnsure && c.KeepDays != nil
	shorter := next.KeepDays < cur.KeepDays || chosen
	if shorter && !c.Shorten && s.firstDay != "" {
		if first, parseErr := time.Parse(dayLayout, s.firstDay); parseErr == nil && first.Before(keepCut(next, now)) {
			return energySettingsResponse{}, energyInputError("keeping %d days deletes the records before that, back to %s; send shorten to do so", next.KeepDays, s.firstDay)
		}
	}
	if chosen || !sameEnergySettings(cur, next) {
		if dataDir != "" {
			if err = writeJSONAtomic(filepath.Join(dataDir, energySettingsFile), next); err != nil {
				return energySettingsResponse{}, err
			}
		}
		m.energyHeld.Store(&next)
		m.prices.reset()
		if chosen {
			s.keepUnsure = false
		}
		if shorter && dir != "" {
			m.pruneEnergyLocked(dir, now, true)
		}
	}
	return m.energySettingsAnswerLocked(dataDir, now), nil
}

type energySettingsResponse struct {
	KeepDays       int          `json:"keep_days"`
	TimeZone       string       `json:"time_zone"`
	ZoneSet        bool         `json:"zone_set"`
	UTCOffsetSec   int          `json:"utc_offset_sec"`
	Currency       string       `json:"currency"`
	Price          *priceChange `json:"price"`           // the newest tariff, null with no price
	PriceSince     *time.Time   `json:"price_since"`     // null with no price or From 0
	Tariffs        []Tariff     `json:"tariffs"`         // never null
	Recording      bool         `json:"recording"`       // false with no data directory
	RecordingSince string       `json:"recording_since"` // UTC date of the oldest day file, "" when none
	// PrunePaused is true while nothing is deleted for its age, because the
	// saved settings could not be used. A change that carries keep_days ends
	// it.
	PrunePaused bool `json:"prune_paused"`
}

func (m *module) energySettingsAnswer() energySettingsResponse {
	m.mu.RLock()
	dataDir := m.dataDir
	m.mu.RUnlock()
	s := &m.energy
	s.io.Lock()
	defer s.io.Unlock()
	if dataDir != "" {
		s.refreshFirstDayLocked(filepath.Join(dataDir, energyDirName))
	}
	return m.energySettingsAnswerLocked(dataDir, m.clock())
}

// energySettingsAnswerLocked is the answer of both settings routes. The
// caller holds io and has read firstDay.
func (m *module) energySettingsAnswerLocked(dataDir string, now time.Time) energySettingsResponse {
	settings := m.energySettings()
	loc := settings.location()
	_, offset := now.In(loc).Zone()
	resp := energySettingsResponse{
		KeepDays:     settings.KeepDays,
		TimeZone:     settings.TimeZone,
		ZoneSet:      settings.TimeZone != "",
		UTCOffsetSec: offset,
		Currency:     settings.Currency,
		Tariffs:      append([]Tariff{}, settings.Tariffs...),
		Recording:    dataDir != "",
		PrunePaused:  m.energy.keepUnsure,
	}
	if resp.Recording {
		resp.RecordingSince = m.energy.firstDay
	}
	if n := len(settings.Tariffs); n > 0 {
		newest := settings.Tariffs[n-1]
		price := priceOf(newest)
		resp.Price = &price
		if newest.From > 0 {
			since := time.Unix(newest.From, 0).In(loc)
			resp.PriceSince = &since
		}
	}
	return resp
}

// energySettingsRoutes are the two routes of the energy settings. Reading
// them asks nothing more than the host already took: a viewer sees cost on
// the page anyway. Changing them takes configure_resource.
func (m *module) energySettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /energy/settings", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, m.energySettingsAnswer())
	})

	mux.HandleFunc("PUT /energy/settings", m.guard(mayConfigure, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
		if err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		if len(raw) > 16<<10 {
			refuse(w, r, InvalidInput.New("the body is larger than 16 KiB"))
			return
		}
		var change energySettingsChange
		if err = json.Unmarshal(raw, &change); err != nil {
			refuse(w, r, InvalidInput.New(err.Error()))
			return
		}
		resp, err := m.changeEnergySettings(change)
		switch {
		case errors.Is(err, errEnergyInput):
			refuse(w, r, InvalidInput.New(strings.TrimPrefix(err.Error(), errEnergyInput.Error()+": ")))
		case err != nil:
			refuse(w, r, InternalError.New(err.Error()))
		default:
			answer(w, http.StatusOK, resp)
		}
	}))
}

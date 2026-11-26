package main

import (
	"math"
	"sync"
	"time"
)

// Cost on the printer's page: the session and the prints of GET
// /devices/{device_id}, priced by the rule of the Energy tab. A time with
// one price and no night price needs no look at the record. Every other
// time is priced by the printer's own slots (meanPrice), and that price is
// kept, so the 5-second poll of the page does not read the record each time.

const (
	// costRefresh is how long the price of a running session or print is
	// kept before it is read again.
	costRefresh  = 60 * time.Second
	maxPriceMemo = 4096
)

// priceMemoKey names a time of one printer. ended is 0 while it runs.
type priceMemoKey struct {
	device         string
	started, ended int64 // unix nanoseconds
}

type priceMemoEntry struct {
	price float64
	ok    bool
	at    time.Time
}

// priceMemo keeps prices per kWh. mu is a leaf lock: it is never held while
// io or module.mu is taken.
type priceMemo struct {
	mu   sync.Mutex
	held map[priceMemoKey]priceMemoEntry
}

// reset forgets every kept price: the tariffs, the zone or the keep time
// changed.
func (p *priceMemo) reset() {
	p.mu.Lock()
	p.held = make(map[priceMemoKey]priceMemoEntry)
	p.mu.Unlock()
}

// pricedSession and pricedPrint are what the status answers. The stored
// types carry no cost.
type pricedSession struct {
	PowerSession
	Cost *float64 `json:"cost"` // null with no price
}

type pricedPrint struct {
	PrintPowerRecord
	Cost *float64 `json:"cost"` // null with no price, and for a print with no reading
}

// pricedSpan is one session or print to price.
type pricedSpan struct {
	started, ended time.Time
	running        bool
	energyWh       float64
	skip           bool // nothing was measured: no cost
}

// spanWindow is the time of a span that is read from the record: at most 31
// days back from its end (from now while it runs), and nothing from before
// the keep time. That is what the Energy tab reads for one print.
func spanWindow(span pricedSpan, settings EnergySettings, now time.Time) (start, end time.Time) {
	start, end = span.started, span.ended
	if span.running {
		end = now
	}
	floor := end.Add(-31 * dayLength)
	if cut := keepCut(settings, now); cut.After(floor) {
		floor = cut
	}
	if start.Before(floor) {
		start = floor
	}
	if start.After(end) {
		start = end
	}
	return start, end
}

// priceRecords prices spans of one printer. costs has one entry per span,
// nil with no price and for a span that is skipped.
func (m *module) priceRecords(deviceID string, now time.Time, spans []pricedSpan) (costs []*float64, priceSet bool, currency string) {
	settings := m.energySettings()
	costs = make([]*float64, len(spans))
	currency = settings.Currency
	if len(settings.Tariffs) == 0 {
		return costs, false, currency
	}
	book := priceBook{tariffs: settings.Tariffs, loc: settings.location()}

	type miss struct {
		i   int
		key priceMemoKey
	}
	var misses []miss
	set := func(i int, price float64) {
		cost := roundCost(finiteOr(spans[i].energyWh, 0) / 1000 * price)
		costs[i] = &cost
	}
	for i, span := range spans {
		if span.skip {
			continue
		}
		key := priceMemoKey{device: deviceID, started: span.started.UnixNano(), ended: span.ended.UnixNano()}
		if span.running {
			key.ended = 0
		}
		start, end := spanWindow(span, settings, now)
		if price, ok := book.flat(start, end); ok {
			set(i, price)
			continue
		}
		m.prices.mu.Lock()
		kept, found := m.prices.held[key]
		m.prices.mu.Unlock()
		if found && (!span.running || now.Sub(kept.at) < costRefresh && !now.Before(kept.at)) {
			if kept.ok {
				set(i, kept.price)
			}
			continue
		}
		misses = append(misses, miss{i: i, key: key})
	}
	if len(misses) == 0 {
		return costs, true, currency
	}

	// One look at the record for everything that is not kept. A change of
	// the settings holds io until it has forgotten the kept prices, so the
	// settings are read again here: a price of the old ones is never kept.
	read := make([]priceMemoEntry, len(misses))
	m.energy.io.Lock()
	settings = m.energySettings()
	if len(settings.Tariffs) > 0 {
		book = priceBook{tariffs: settings.Tariffs, loc: settings.location()}
		reader := m.energyReaderLocked()
		for n, miss := range misses {
			start, end := spanWindow(spans[miss.i], settings, now)
			price, _, ok := book.meanPrice(reader, deviceID, start, end)
			read[n] = priceMemoEntry{price: price, ok: ok && !math.IsNaN(price), at: now}
		}
		m.prices.mu.Lock()
		if len(m.prices.held)+len(misses) > maxPriceMemo {
			m.prices.held = make(map[priceMemoKey]priceMemoEntry)
		}
		for n, miss := range misses {
			m.prices.held[miss.key] = read[n]
		}
		m.prices.mu.Unlock()
	}
	m.energy.io.Unlock()

	for n, miss := range misses {
		if read[n].ok {
			set(miss.i, read[n].price)
		}
	}
	return costs, true, currency
}

// pricedUsage is the session and the prints of a status with their cost.
type pricedUsage struct {
	session  *pricedSession
	current  *pricedPrint
	history  []pricedPrint
	priceSet bool
	currency string
}

// priceUsage prices a copy of one printer's usage. No lock is held by the
// caller.
func (m *module) priceUsage(deviceID string, u *DeviceUsage, now time.Time) pricedUsage {
	var spans []pricedSpan
	printSpan := func(rec PrintPowerRecord) pricedSpan {
		span := pricedSpan{
			started: rec.StartedAt, running: rec.EndedAt == nil,
			energyWh: rec.EnergyWh, skip: rec.EnergyWh <= 0 && rec.MeteredSec <= 0,
		}
		if rec.EndedAt != nil {
			span.ended = *rec.EndedAt
		}
		return span
	}
	if u != nil {
		if s := u.Session; s != nil {
			span := pricedSpan{started: s.StartedAt, running: s.EndedAt == nil, energyWh: s.EnergyWh}
			if s.EndedAt != nil {
				span.ended = *s.EndedAt
			}
			spans = append(spans, span)
		}
		if u.CurrentPrint != nil {
			spans = append(spans, printSpan(*u.CurrentPrint))
		}
		for _, rec := range u.PrintHistory {
			spans = append(spans, printSpan(rec))
		}
	}
	costs, priceSet, currency := m.priceRecords(deviceID, now, spans)
	out := pricedUsage{priceSet: priceSet, currency: currency}
	if u == nil {
		return out
	}
	next := 0
	if u.Session != nil {
		out.session = &pricedSession{PowerSession: *u.Session, Cost: costs[next]}
		next++
	}
	if u.CurrentPrint != nil {
		out.current = &pricedPrint{PrintPowerRecord: *u.CurrentPrint, Cost: costs[next]}
		next++
	}
	for _, rec := range u.PrintHistory {
		out.history = append(out.history, pricedPrint{PrintPowerRecord: rec, Cost: costs[next]})
		next++
	}
	return out
}

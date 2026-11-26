package main

import (
	"testing"
	"time"
)

// A wait that was saved says nothing about what the printer did while the
// extension did not run. Until the printer was seen once, it stays on.
func TestARestoredWaitDoesNotActBeforeThePrinterIsSeen(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	wait := 5 * time.Minute
	running := printerPrinting
	running.JobID = "job-1"
	done := printerSafety{Connected: true, PrinterState: "FINISHED", CanTurnOff: true, LastJobID: "job-1", LastJobOutcome: "finished"}

	m := restartedModule(dir, armedPlug)
	m.autoOffDue("p", running, wait, now)
	m.autoOffDue("p", done, wait, now) // the wait begins
	m.saveAutoOffIfChanged()

	// Two hours later the printer runs its next print, and the host does not
	// report it as connected yet.
	later := now.Add(2 * time.Hour)
	away := printerSafety{CanTurnOff: true}
	restarted := restartedModule(dir, armedPlug)
	if restarted.autoOffDue("p", away, wait, later) || restarted.autoOffDue("p", away, wait, later.Add(5*time.Second)) {
		t.Fatal("a printer nobody has seen since the restart was turned off")
	}
	next := printerPrinting
	next.JobID = "job-2"
	if restarted.autoOffDue("p", next, wait, later.Add(10*time.Second)) {
		t.Fatal("a printer that prints was due")
	}
	if restarted.autoOffDue("p", away, wait, later.Add(15*time.Second)) {
		t.Fatal("a lost connection in the next print turned the printer off")
	}
}

func TestIdleOnlyRefusesAPrinterConnectCannotReach(t *testing.T) {
	away := printerSafety{PrinterState: "PRINTING", CanTurnOff: true}
	busy := printerSafety{Connected: true, PrinterState: "PRINTING", LockReason: "Printer is currently printing"}

	if _, _, refused := offRefusal(away, false); refused {
		t.Fatal("a person may not turn off a printer that is away")
	}
	if _, state, refused := offRefusal(away, true); !refused || state != stateUnreachable {
		t.Fatalf("idle only, printer away: refused %v with the state %q", refused, state)
	}
	if _, _, refused := offRefusal(printerIdle, true); refused {
		t.Fatal("idle only refused an idle printer")
	}
	for _, idleOnly := range []bool{false, true} {
		if reason, state, refused := offRefusal(busy, idleOnly); !refused || state != "PRINTING" || reason != busy.LockReason {
			t.Fatalf("a printing printer (idle only %v): refused %v, %q, %q", idleOnly, refused, state, reason)
		}
	}
}

// The idle time does not run while the turn-off after a print waits, so a
// short idle time never cuts a longer wait short.
func TestTheIdleTimeDoesNotRunDuringTheWaitAfterAPrint(t *testing.T) {
	wait := 1800
	m, clock, _, plug, events := steppedModule(t, DeviceConfig{AutoOffAfterPrint: true, CooldownSec: &wait}, Settings{IdleOffMin: 10})
	all := overviewOptions{visible: func(string) bool { return true }}

	step(m, clock, printerPrinting, 5*time.Second)
	step(m, clock, printerFinished, 5*time.Second)
	for range 25 {
		step(m, clock, printerFinished, time.Minute)
		if left := m.buildOverview(all).Devices[0].IdleOffRemainingSec; left != nil {
			t.Fatalf("the overview counts the idle time down during the wait: %d", *left)
		}
	}
	if plug.turnedOff() != 0 || len(events.told()) != 0 {
		t.Fatalf("the plug was turned off %d times before the wait was over: %+v", plug.turnedOff(), events.told())
	}

	step(m, clock, printerFinished, 5*time.Minute)
	told := events.told()
	if plug.turnedOff() != 1 || len(told) != 1 || told[0].auto().Reason != reasonPrintFinished {
		t.Fatalf("after the wait: %d turn-offs, %+v", plug.turnedOff(), told)
	}
	// The choice for one print is used up.
	if m.configs["p1"].AutoOffAfterPrint {
		t.Fatal("the printer is still armed after its one print")
	}
}

// A printer that drops off right after its print stopped gets the same grace
// for the print's outcome as one that is still there.
func TestAPrinterThatDropsOffWaitsForThePrintsOutcome(t *testing.T) {
	now := time.Now()
	running := printerPrinting
	running.JobID = "job-1"
	away := printerSafety{CanTurnOff: true}
	cancelled := away
	cancelled.LastJobID, cancelled.LastJobOutcome = "job-1", "cancelled"

	for _, tc := range []struct {
		name string
		last printerSafety
		due  bool
	}{
		{"no outcome", away, true},
		{"cancelled", cancelled, false},
	} {
		m := newModule(nil)
		m.autoOffDue("p", running, 0, now)
		if m.autoOffDue("p", printerIdle, 0, now) {
			t.Fatalf("%s: due before the outcome grace", tc.name)
		}
		if m.autoOffDue("p", away, 0, now.Add(5*time.Second)) {
			t.Fatalf("%s: a printer that dropped off was due within the outcome grace", tc.name)
		}
		if due := m.autoOffDue("p", tc.last, 0, now.Add(16*time.Second)); due != tc.due {
			t.Fatalf("%s: due %v after the grace, want %v", tc.name, due, tc.due)
		}
		if !tc.due && m.trackers["p"].kept != "cancelled" {
			t.Fatalf("%s: kept %q, want cancelled", tc.name, m.trackers["p"].kept)
		}
	}
}

func TestKeepOnPressedDuringTheLastLookKeepsThePrinterOn(t *testing.T) {
	m, clock, cfg, plug, events := steppedModule(t, DeviceConfig{}, Settings{IdleOffMin: 10})

	step(m, clock, printerIdle, 0)
	step(m, clock, printerIdle, 5*time.Minute)
	clock.add(5 * time.Minute)
	m.idleStep(t.Context(), "p1", cfg, printerIdle, func() (printerSafety, error) {
		if !m.keepOn("p1", clock.now()) {
			t.Error("keep on found no idle time")
		}
		return printerIdle, nil
	})
	if plug.turnedOff() != 0 || len(events.told()) != 0 {
		t.Fatalf("the plug was turned off after keep on: %d times, %+v", plug.turnedOff(), events.told())
	}

	// A whole idle time later it is turned off.
	step(m, clock, printerIdle, 5*time.Minute)
	step(m, clock, printerIdle, 5*time.Minute)
	if plug.turnedOff() != 1 {
		t.Fatalf("the plug was told to turn off %d times, want once", plug.turnedOff())
	}
}

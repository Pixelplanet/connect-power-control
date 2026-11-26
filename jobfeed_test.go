package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// jobFrame is one frame of the host's event stream about a print of the
// printer device, as the host renders it.
func jobFrame(t *testing.T, device, kind string, data any) json.RawMessage {
	t.Helper()
	origin, err := event.NewOrigin(event.KindDevice, device)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event.Envelope{
		SpecVersion: "1.0",
		ID:          "frame",
		Source:      event.SourceURN("site", origin),
		Type:        kind,
		Time:        energyStart,
		Class:       event.ClassState,
		Data:        body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func jobWindow(t *testing.T, m *module, frames ...json.RawMessage) {
	t.Helper()
	if err := m.onJobWindow(busapi.Window{Events: frames}); err != nil {
		t.Fatal(err)
	}
}

func hostJobOf(m *module, id string) (hostJob, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.hostJobs[id]
	return job, ok
}

func TestAJobSettledFrameSetsEndAndOutcome(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)

	// The server saw the print begin two seconds before the next sample.
	started := clock.now().Add(3 * time.Second)
	jobWindow(t, m, jobFrame(t, "p1", event.TypeDeviceJobStarted, event.JobStarted{
		JobID: "7", DeviceJobID: "311", Path: "/usb/benchy.gcode", Started: started,
	}))
	if job, ok := hostJobOf(m, "p1"); !ok || job.JobID != "7" || job.File != "benchy.gcode" || !job.Started.Equal(started) || job.Ended != nil {
		t.Fatalf("held after the start frame: %+v", job)
	}
	tick(m, clock, cfg, printingPrinter, 5*time.Second)
	if p := usageOf(m, "p1").CurrentPrint; p == nil || !p.StartedAt.Equal(started) {
		t.Fatalf("the print began %v, want the server's start %v", p, started)
	}
	for range 3 {
		tick(m, clock, cfg, printingPrinter, 5*time.Second)
	}

	// It was cancelled three seconds after the last sample. The printer
	// itself says nothing of that.
	ended := clock.now().Add(3 * time.Second)
	jobWindow(t, m, jobFrame(t, "p1", event.TypeDeviceJobSettled, event.JobSettled{
		JobID: "7", DeviceJobID: "311", Started: started, Ended: ended, Outcome: event.JobCancelled,
	}))
	tick(m, clock, cfg, idlePrinter, 5*time.Second)

	p := onePrintLeft(t, m)
	if p.Outcome != "cancelled" || !p.EndedAt.Equal(ended) || p.EndEstimated || p.DurationSec != 20 {
		t.Fatalf("print = %+v; want cancelled at %v after 20 s, not estimated", p, ended)
	}
}

// An outcome the server does not know leaves the answer to the printer.
func TestAnUnknownOutcomeOnTheStreamIsNotTaken(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	jobWindow(t, m, jobFrame(t, "p1", event.TypeDeviceJobSettled, event.JobSettled{
		JobID: "7", Started: clock.now(), Ended: clock.now(), Outcome: event.JobUnknown,
	}))
	stopped := printerSafety{Connected: true, PrinterState: "STOPPED", CanTurnOff: true}
	tick(m, clock, cfg, stopped, 5*time.Second)
	if p := onePrintLeft(t, m); p.Outcome != "cancelled" {
		t.Fatalf("print = %+v; want cancelled, as the printer says", p)
	}
}

func TestAFrameForAnotherJobIsIgnored(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	before := clock.now().Add(-time.Hour)
	jobWindow(t, m,
		jobFrame(t, "p1", event.TypeDeviceJobSettled, event.JobSettled{
			JobID: "6", Started: before, Ended: before.Add(time.Minute), Outcome: event.JobFailed,
		}),
		// Frames this module cannot use are passed over.
		json.RawMessage(`{"not an envelope`),
		jobFrame(t, "p1", "com.example.other", map[string]string{"job_id": "7"}),
		jobFrame(t, "p1", event.TypeDeviceJobSettled, "not a job"),
		jobFrame(t, "p1", event.TypeDeviceJobStarted, event.JobStarted{Started: clock.now()}),
	)
	if job, _ := hostJobOf(m, "p1"); job.JobID != "6" {
		t.Fatalf("held = %+v, want job 6", job)
	}

	// Job 7 runs and ends. What is held is about job 6.
	startPrint(m, clock, cfg)
	tick(m, clock, cfg, idlePrinter, 5*time.Second)
	if p := onePrintLeft(t, m); p.Outcome != "finished" || !p.EndedAt.Equal(clock.now()) || p.EndEstimated {
		t.Fatalf("print = %+v; want finished now, with nothing taken from job 6", p)
	}

	// The end of an older print does not push out the one that runs.
	running := clock.now()
	jobWindow(t, m,
		jobFrame(t, "p1", event.TypeDeviceJobStarted, event.JobStarted{JobID: "8", Started: running}),
		jobFrame(t, "p1", event.TypeDeviceJobSettled, event.JobSettled{
			JobID: "6", Started: before, Ended: before.Add(time.Minute), Outcome: event.JobFailed,
		}),
		jobFrame(t, "p2", event.TypeDeviceJobStarted, event.JobStarted{JobID: "9", Started: running}),
	)
	if job, _ := hostJobOf(m, "p1"); job.JobID != "8" || job.Ended != nil || job.Outcome != "" {
		t.Fatalf("held = %+v, want the running job 8", job)
	}
	if job, _ := hostJobOf(m, "p2"); job.JobID != "9" {
		t.Fatalf("held for p2 = %+v, want job 9", job)
	}
}

func TestAResyncClearsTheCache(t *testing.T) {
	m, clock := clockedModule(energyStart)
	if err := m.onJobWindow(busapi.Window{
		Events: []json.RawMessage{jobFrame(t, "p1", event.TypeDeviceJobStarted, event.JobStarted{JobID: "7", Started: clock.now()})},
		Cursor: "c1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := hostJobOf(m, "p1"); !ok || m.jobCursor != "c1" {
		t.Fatalf("held %v, cursor %q", m.hostJobs, m.jobCursor)
	}

	if err := m.onJobWindow(busapi.Window{Resync: true, Cursor: "c2"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := hostJobOf(m, "p1"); ok || m.jobCursor != "c2" {
		t.Fatalf("after a resync: held %v, cursor %q; want nothing held and c2", m.hostJobs, m.jobCursor)
	}
	// A window with no cursor keeps the last one.
	if err := m.onJobWindow(busapi.Window{}); err != nil || m.jobCursor != "c2" {
		t.Fatalf("cursor %q (%v), want c2", m.jobCursor, err)
	}
}

// followed runs followJobs against a host that answers each subscription
// with the next of answers, and stops it after the last. It answers the
// subscriptions that were made.
func followed(t *testing.T, m *module, answers ...error) []busapi.SubscribeParams {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var asked []busapi.SubscribeParams
	m.jobRetry = time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.followJobs(ctx, func(_ context.Context, params busapi.SubscribeParams, _ func(busapi.Window) error) error {
			asked = append(asked, params)
			if len(asked) > len(answers) {
				cancel()
				return context.Canceled
			}
			return answers[len(asked)-1]
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("followJobs did not return")
	}
	return asked
}

func TestAHostWithoutTheStreamTurnsTheFeedOff(t *testing.T) {
	m := newModule(nil)
	refusal := fmt.Errorf("busapi: %s: %w", busapi.MethodSubscribe, &wirerpc.Error{Code: wirerpc.CodeMethodNotFound, Message: "no such method"})
	asked := followed(t, m, refusal, nil, nil)
	if len(asked) != 1 {
		t.Fatalf("the stream was asked for %d times, want once", len(asked))
	}
	want := []string{event.TypeDeviceJobStarted, event.TypeDeviceJobSettled}
	if got := asked[0]; len(got.Types) != 2 || got.Types[0] != want[0] || got.Types[1] != want[1] || got.Principal != "" || got.Validate() != nil {
		t.Fatalf("subscription = %+v", got)
	}
}

func TestADeniedSubscriptionTurnsTheFeedOff(t *testing.T) {
	m := newModule(nil)
	asked := followed(t, m, fmt.Errorf("%s: %w", busapi.MethodSubscribe, busapi.ErrDenied), nil, nil)
	if len(asked) != 1 {
		t.Fatalf("the stream was asked for %d times, want once", len(asked))
	}
}

func TestATransientRefusalIsTriedAgain(t *testing.T) {
	m := newModule(nil)
	m.jobCursor = "c7"
	asked := followed(t, m,
		fmt.Errorf("%s: %w", busapi.MethodSubscribe, busapi.ErrNotReady),
		&wirerpc.Error{Code: wirerpc.CodeInternal, Message: "host failed"},
		nil, // the stream ended
		fmt.Errorf("%s: %w", busapi.MethodSubscribe, busapi.ErrInvalidSubscription),
	)
	if len(asked) != 5 {
		t.Fatalf("the stream was asked for %d times, want 5", len(asked))
	}
	for i, params := range asked[:4] {
		if params.Resume != "c7" {
			t.Fatalf("subscription %d reads on from %q, want c7", i, params.Resume)
		}
	}
	// A place the host no longer takes is given up.
	if asked[4].Resume != "" {
		t.Fatalf("after a refused subscription it reads on from %q, want the start", asked[4].Resume)
	}
}

func TestThePolledJobRowSetsTheEnd(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)

	// No stream: the job row the poll reads carries start, end and outcome.
	started := clock.now().Add(2 * time.Second)
	running := printingPrinter
	running.JobStarted = started
	for range 4 {
		tick(m, clock, cfg, running, 5*time.Second)
	}
	if p := usageOf(m, "p1").CurrentPrint; p == nil || !p.StartedAt.Equal(started) {
		t.Fatalf("the print began %v, want the server's start %v", p, started)
	}

	ended := clock.now().Add(time.Second)
	over := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "failed", LastJobEnded: &ended}
	tick(m, clock, cfg, over, 5*time.Second)
	if p := onePrintLeft(t, m); p.Outcome != "failed" || !p.EndedAt.Equal(ended) || p.EndEstimated || p.DurationSec != 19 {
		t.Fatalf("print = %+v; want failed at %v after 19 s", p, ended)
	}
}

// A start the server saw long before the first sample is a print that ran
// before it was seen here. The record begins with what was measured.
func TestAnOldStartIsNotTaken(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	tick(m, clock, cfg, idlePrinter, 0)
	running := printingPrinter
	running.JobStarted = clock.now().Add(-time.Hour)
	tick(m, clock, cfg, running, 5*time.Second)
	if p := usageOf(m, "p1").CurrentPrint; p == nil || !p.StartedAt.Equal(clock.now()) {
		t.Fatalf("the print began %v, want now %v", p, clock.now())
	}
}

// The gap rule wins over the server's end: a print that ended while nothing
// was sampled is closed at the last sample.
func TestAGapStillEndsThePrintAtTheLastSample(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	last := clock.now()
	ended := last.Add(5 * time.Minute)
	over := printerSafety{Connected: true, PrinterState: "IDLE", CanTurnOff: true, LastJobID: "7", LastJobOutcome: "finished", LastJobEnded: &ended}
	tick(m, clock, cfg, over, 10*time.Minute)
	if p := onePrintLeft(t, m); !p.EndedAt.Equal(last) || !p.EndEstimated || p.Outcome != "finished" {
		t.Fatalf("print = %+v; want finished at the last sample %v, estimated", p, last)
	}
}

// The server closes a print a moment before the printer's state stops naming
// it. That moment is not a second print.
func TestAStateThatLagsBehindTheServerStartsNoSecondPrint(t *testing.T) {
	m, clock, cfg := energyModule(t, energyStart)
	startPrint(m, clock, cfg)
	ended := clock.now().Add(3 * time.Second)
	lagging := printerSafety{
		Connected: true, PrinterState: "PRINTING", JobOver: true,
		JobID: "7", LastJobID: "7", LastJobOutcome: "finished", LastJobEnded: &ended,
	}
	for range 3 {
		tick(m, clock, cfg, lagging, 5*time.Second)
	}
	if p := onePrintLeft(t, m); p.Outcome != "finished" || !p.EndedAt.Equal(ended) || p.EndEstimated {
		t.Fatalf("print = %+v; want finished at %v", p, ended)
	}

	now := clock.now()
	closed := now.Add(-10 * time.Second)
	last := &deviceapi.Job{ID: "job-7", DeviceJobID: "311", Ended: &closed}
	longAgo := now.Add(-time.Hour)
	for name, tc := range map[string]struct {
		jobs  deviceapi.Jobs
		state string
		want  bool
	}{
		"the state names the closed print":    {deviceapi.Jobs{Last: last}, "311", true},
		"the state names another print":       {deviceapi.Jobs{Last: last}, "312", false},
		"the state names no print":            {deviceapi.Jobs{Last: last}, "", false},
		"the server holds a print open":       {deviceapi.Jobs{Current: &deviceapi.Job{ID: "job-8", DeviceJobID: "311"}, Last: last}, "311", false},
		"the print was closed long ago":       {deviceapi.Jobs{Last: &deviceapi.Job{ID: "job-7", DeviceJobID: "311", Ended: &longAgo}}, "311", false},
		"the last print has no end":           {deviceapi.Jobs{Last: &deviceapi.Job{ID: "job-7", DeviceJobID: "311"}}, "311", false},
		"the server knows no print":           {deviceapi.Jobs{}, "311", false},
		"the end lies ahead of this computer": {deviceapi.Jobs{Last: &deviceapi.Job{DeviceJobID: "311", Ended: timePtr(now.Add(time.Minute))}}, "311", false},
	} {
		if got := closedJobStillNamed(tc.jobs, tc.state, now); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// The host tells on its event stream when a print started and how it ended.
// Where it serves that stream to the extension, the print records take their
// start, end and outcome from it. Where it does not, the stream is left alone
// and the job row the 5-second poll reads is what counts (jobFacts).

const (
	jobFeedWindowMS = 1000
	jobFeedRetry    = 30 * time.Second
	// A host that is still starting says so. It is asked again sooner.
	jobFeedNotReadyRetry = 5 * time.Second
)

// hostJob is the newest print of one printer as the event stream told it.
type hostJob struct {
	JobID   string
	File    string
	Started time.Time
	Ended   *time.Time
	Outcome string
}

// jobSubscriber is busapi.Subscribe on the session with the host.
type jobSubscriber func(ctx context.Context, params busapi.SubscribeParams, onWindow func(busapi.Window) error) error

// followJobs reads the host's job events until ctx is done. A host that does
// not serve the stream, or that refuses it to this extension, ends it for
// good. Everything else is tried again.
func (m *module) followJobs(ctx context.Context, subscribe jobSubscriber) {
	for ctx.Err() == nil {
		m.mu.RLock()
		cursor := m.jobCursor
		m.mu.RUnlock()

		err := subscribe(ctx, busapi.SubscribeParams{
			Types:      []string{event.TypeDeviceJobStarted, event.TypeDeviceJobSettled},
			IntervalMS: jobFeedWindowMS,
			Resume:     cursor,
		}, m.onJobWindow)
		if ctx.Err() != nil {
			return
		}

		wait := jobFeedRetry
		refused, isRefusal := errors.AsType[*wirerpc.Error](err)
		switch {
		case isRefusal && refused.Code == wirerpc.CodeMethodNotFound:
			fmt.Fprintf(os.Stderr, "%s: the server offers no job events; print start and end are read from its job record\n", logName)
			return
		case errors.Is(err, busapi.ErrDenied):
			fmt.Fprintf(os.Stderr, "%s: the server does not allow reading its job events; print start and end are read from its job record\n", logName)
			return
		case errors.Is(err, busapi.ErrInvalidSubscription):
			// The place to read on from may be what the host no longer takes.
			m.mu.Lock()
			m.jobCursor = ""
			m.mu.Unlock()
		case errors.Is(err, busapi.ErrNotReady):
			wait = jobFeedNotReadyRetry
		}
		if m.jobRetry > 0 {
			wait = m.jobRetry
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// onJobWindow keeps what one window of the stream says about prints. A frame
// that cannot be read is skipped, so one bad frame never stops the stream.
func (m *module) onJobWindow(w busapi.Window) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if w.Resync {
		// The host could not send everything since the last window. What is
		// held may be out of date, and the poll fills in until new frames come.
		clear(m.hostJobs)
	}

	for _, raw := range w.Events {
		var env event.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		deviceID := jobDeviceOf(env)
		if deviceID == "" {
			continue
		}

		switch env.Type {
		case event.TypeDeviceJobStarted:
			var data event.JobStarted
			if err := json.Unmarshal(env.Data, &data); err != nil || data.JobID == "" {
				continue
			}
			if held, ok := m.hostJobs[deviceID]; ok && held.JobID != data.JobID && held.Started.After(data.Started) {
				continue
			}
			m.hostJobs[deviceID] = hostJob{
				JobID:   data.JobID,
				File:    cleanJobFileName(data.DisplayName, data.Path, data.DeviceJobID),
				Started: data.Started,
			}
		case event.TypeDeviceJobSettled:
			var data event.JobSettled
			if err := json.Unmarshal(env.Data, &data); err != nil || data.JobID == "" {
				continue
			}
			held := m.hostJobs[deviceID]
			if held.JobID != data.JobID {
				if held.Started.After(data.Started) {
					// The end of an older print than the one that is held.
					continue
				}
				held = hostJob{
					JobID:   data.JobID,
					File:    cleanJobFileName(data.DisplayName, data.Path, data.DeviceJobID),
					Started: data.Started,
				}
			}
			if !data.Ended.IsZero() {
				ended := data.Ended
				held.Ended = &ended
			}
			held.Outcome = strings.ToLower(strings.TrimSpace(data.Outcome))
			m.hostJobs[deviceID] = held
		}
	}

	if w.Cursor != "" {
		m.jobCursor = w.Cursor
	}
	return nil
}

// jobDeviceOf is the printer a job frame is about, or empty when the frame
// names none.
func jobDeviceOf(env event.Envelope) string {
	if _, origin, err := event.ParseSourceURN(env.Source); err == nil && origin.Kind() == event.KindDevice {
		return origin.ID()
	}
	if origin, err := event.ParseOrigin(env.Subject); err == nil && origin.Kind() == event.KindDevice {
		return origin.ID()
	}
	return ""
}

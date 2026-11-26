package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// The extension tells the host's event stream three things: a printer was
// turned off by itself, such a turn-off failed, and a plug or its broker went
// away while its printer was printing. The host gives each the type
// module.yaml names for its endpoint, and the operator's own webhook or MQTT
// sink delivers it. The extension itself sends nothing anywhere.
//
// A host that does not serve the call, and an operator who did not approve
// the endpoints, are both the normal case: nothing is told then, nothing is
// logged, and nothing else changes.

const (
	endpointAutoOffDone   = "auto_off_done"
	endpointAutoOffFailed = "auto_off_failed"
	endpointPlugLost      = "plug_lost"

	// emitTimeout bounds one event, so a host that does not answer cannot
	// hold up the tick.
	emitTimeout = 2 * time.Second
	// plugLostAfter is how long a plug or its broker must be away during a
	// print before that is told.
	plugLostAfter = 60 * time.Second

	reasonPrintFinished = "print_finished"
	reasonIdle          = "idle"

	causePlugOffline   = "plug_offline"
	causeBrokerOffline = "broker_offline"
)

// eventEmitter is the Emit of busapi.Client.
type eventEmitter interface {
	Emit(ctx context.Context, endpoint, key string, data any) error
}

// autoOffEvent is the payload of auto_off.done and auto_off.failed. It names
// the printer and never a plug's address, topic or entity.
type autoOffEvent struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	// Reason is print_finished or idle.
	Reason string `json:"reason"`
	// Error is why the turn-off failed, with addresses and secrets taken out.
	Error string `json:"error,omitempty"`
}

// plugLostEvent is the payload of plug.lost.
type plugLostEvent struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Driver     string `json:"driver"`
	// Cause is plug_offline or broker_offline.
	Cause string `json:"cause"`
}

// eventKey is the routing key of an event: the printer's id where the host
// takes it as one, else none.
func eventKey(deviceID string) string {
	if len(deviceID) > busapi.MaxKeyBytes {
		return ""
	}
	for i := range len(deviceID) {
		if deviceID[i] <= ' ' || deviceID[i] == 0x7f {
			return ""
		}
	}
	return deviceID
}

// emitEvent tells the host's event stream one thing. It never fails for its
// caller: an event that cannot be told is dropped. The caller does not hold
// m.mu.
func (m *module) emitEvent(ctx context.Context, endpoint, deviceID string, data any) {
	if m.events == nil || m.eventsOff.Load() {
		return
	}
	if _, off := m.eventOff.Load(endpoint); off {
		return
	}

	emitCtx, cancel := context.WithTimeout(ctx, emitTimeout)
	defer cancel()
	err := m.events.Emit(emitCtx, endpoint, eventKey(deviceID), data)
	if err == nil {
		return
	}

	// The host serves no bus.emit: an older core, or an install where no
	// event was approved. Nothing is tried again in this run.
	if refused, ok := errors.AsType[*wirerpc.Error](err); ok && refused.Code == wirerpc.CodeMethodNotFound {
		m.eventsOff.Store(true)
		return
	}
	// The operator approved other events and not this one.
	if errors.Is(err, busapi.ErrNoEndpoint) {
		m.eventOff.Store(endpoint, true)
		return
	}
	// Anything else is of the moment. This event is dropped, and said once.
	if _, logged := m.eventLogged.LoadOrStore(endpoint, true); !logged {
		fmt.Fprintf(os.Stderr, "%s: the event %s was not told to the server: %v\n", logName, endpoint, err)
	}
}

// lostWatch follows one printer's plug through a print.
type lostWatch struct {
	// printing says the printer was printing when it was last connected.
	printing bool
	jobID    string
	// since is from when the plug or its broker is away, and sent that this
	// print's event was told.
	since time.Time
	sent  bool
}

// plugLostDue follows a printer's plug for one tick. It answers true once per
// print, when the plug or its broker has been away for plugLostAfter while
// the printer prints. A printer that is away itself keeps what it last said:
// a print does not stop because Connect lost sight of it.
func (m *module) plugLostDue(deviceID string, cfg DeviceConfig, safety printerSafety, now time.Time) (cause string, due bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	watch := m.lost[deviceID]
	if safety.Connected {
		if !safety.printing() {
			delete(m.lost, deviceID)
			return "", false
		}
		if watch == nil {
			watch = &lostWatch{}
			m.lost[deviceID] = watch
		}
		if safety.JobID != "" && watch.jobID != "" && safety.JobID != watch.jobID {
			*watch = lostWatch{}
		}
		watch.printing = true
		if safety.JobID != "" {
			watch.jobID = safety.JobID
		}
	}
	if watch == nil || !watch.printing {
		return "", false
	}

	plug := m.plugs[cfg.plugKey()]
	switch {
	case plug != nil && plug.Online != nil && !*plug.Online:
		cause = causePlugOffline
	case cfg.usesMQTT() && m.settings().mqttConfigured() && !m.mqttUp:
		cause = causeBrokerOffline
	default:
		watch.since = time.Time{}
		return "", false
	}

	if watch.since.IsZero() {
		watch.since = now
	}
	if watch.sent || now.Sub(watch.since) < plugLostAfter {
		return "", false
	}
	watch.sent = true
	return cause, true
}

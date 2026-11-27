package ui

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
)

// The hub passes every event to every page that watches (a "viewer", one
// SSE connection each) and to the recorder, in one order. A viewer that
// reads too slowly is dropped, never waited for: a projector laptop that
// falls behind during an output flood must not hold up the server or the
// runs (the browser then reconnects and catches up).
const (
	// maxViewers is how many pages may watch at once.
	maxViewers = 8
	// viewerQueue is how many events may wait for one viewer.
	viewerQueue = 256
	// laneKeep is how much of each lane's newest output a page that
	// connects late gets (the page keeps no more either).
	laneKeep = 64 << 10
)

// errTooManyViewers is subscribe's error when maxViewers pages watch.
var errTooManyViewers = errors.New("too many pages are watching already")

// viewer is one page's SSE connection.
type viewer struct {
	ch   chan []byte   // the queued events, as JSON
	gone chan struct{} // closed when the hub drops the viewer
}

// hub is the server's one event stream.
type hub struct {
	logger *log.Logger

	mu      sync.Mutex
	viewers map[*viewer]struct{}
	closed  bool
	scene   scene
	rec     *recorder // nil when not recording
}

func newHub(logger *log.Logger, rec *recorder) *hub {
	return &hub{logger: logger, viewers: make(map[*viewer]struct{}), rec: rec, scene: newScene()}
}

// publish sends ev to every viewer and the recorder.
func (h *hub) publish(ev *Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		h.logger.Printf("BUG: encode a %s event: %v", ev.Type, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.scene.apply(ev)
	if h.rec != nil {
		if notice := h.rec.write(ev); notice != nil {
			// The recording stopped (it is full, or its file failed):
			// every page says so, and so does the log.
			h.logger.Print(notice.Notice.Text)
			h.scene.apply(notice)
			if n, err := json.Marshal(notice); err == nil {
				h.send(n)
			}
		}
	}
	h.send(data)
}

// send queues data for every viewer; mu is held.
func (h *hub) send(data []byte) {
	for v := range h.viewers {
		select {
		case v.ch <- data:
		default:
			h.drop(v, "it fell more than 256 events behind")
		}
	}
}

// drop ends a viewer's stream; mu is held.
func (h *hub) drop(v *viewer, why string) {
	if _, ok := h.viewers[v]; !ok {
		return
	}
	delete(h.viewers, v)
	close(v.gone)
	if why != "" {
		h.logger.Printf("dropped a page's event stream: %s (it reconnects by itself and catches up)", why)
	}
}

// subscribe adds a viewer and returns it with the events that bring a
// new page up to date (in order, before anything published later).
func (h *hub) subscribe() (*viewer, [][]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, errors.New("the server is stopping")
	}
	if len(h.viewers) >= maxViewers {
		return nil, nil, errTooManyViewers
	}
	var initial [][]byte
	for _, ev := range h.scene.events() {
		if data, err := json.Marshal(ev); err == nil {
			initial = append(initial, data)
		}
	}
	v := &viewer{ch: make(chan []byte, viewerQueue), gone: make(chan struct{})}
	h.viewers[v] = struct{}{}
	return v, initial, nil
}

// unsubscribe removes a viewer whose page went away.
func (h *hub) unsubscribe(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(v, "")
}

// close ends every stream; nothing is published afterwards.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v := range h.viewers {
		h.drop(v, "")
	}
	h.closed = true
}

// viewers counts the pages watching (tests).
func (h *hub) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

// scene is what a page that connects now needs to catch up: the newest
// event of each panel, every event of the newest scenario (each lane's
// output cut to its newest laneKeep bytes), and the newest notice.
type scene struct {
	latest   map[string]*Event // TypeInfo, TypeHost, TypeAudit, TypeVerify, TypeHistory, TypeNotice
	scenario *Event
	lanes    []*laneScene
	end      *Event
}

// laneScene is one lane of the newest scenario.
type laneScene struct {
	states  []*Event
	outputs []*Event
	bytes   int
	trimmed bool
	result  *Event
}

func newScene() scene { return scene{latest: make(map[string]*Event)} }

// apply takes in one published event.
func (s *scene) apply(ev *Event) {
	switch ev.Type {
	case TypeScenario:
		s.scenario, s.end = ev, nil
		s.lanes = make([]*laneScene, ev.Scenario.Clones)
		for i := range s.lanes {
			s.lanes[i] = &laneScene{}
		}
	case TypeState:
		if l := s.lane(ev.State.Lane); l != nil {
			l.states = append(l.states, ev)
		}
	case TypeOutput:
		if l := s.lane(ev.Output.Lane); l != nil {
			l.outputs = append(l.outputs, ev)
			l.bytes += len(ev.Output.Data)
			for l.bytes > laneKeep && len(l.outputs) > 1 {
				l.bytes -= len(l.outputs[0].Output.Data)
				l.outputs = l.outputs[1:]
				l.trimmed = true
			}
		}
	case TypeResult:
		if l := s.lane(ev.Result.Lane); l != nil {
			l.result = ev
		}
	case TypeEnd:
		s.end = ev
	default:
		s.latest[ev.Type] = ev
	}
}

func (s *scene) lane(i int) *laneScene {
	if i < 0 || i >= len(s.lanes) {
		return nil
	}
	return s.lanes[i]
}

// events returns the events that rebuild the scene on a new page.
func (s *scene) events() []*Event {
	var out []*Event
	for _, t := range []string{TypeInfo, TypeHistory, TypeAudit, TypeVerify, TypeHost} {
		if ev := s.latest[t]; ev != nil {
			out = append(out, ev)
		}
	}
	if s.scenario != nil {
		out = append(out, s.scenario)
		for _, l := range s.lanes {
			out = append(out, l.states...)
			for i, o := range l.outputs {
				if i == 0 && l.trimmed {
					cp := *o.Output
					cp.Gap = true
					o = &Event{Type: TypeOutput, Output: &cp}
				}
				out = append(out, o)
			}
			if l.result != nil {
				out = append(out, l.result)
			}
		}
		if s.end != nil {
			out = append(out, s.end)
		}
	}
	if ev := s.latest[TypeNotice]; ev != nil {
		out = append(out, ev)
	}
	return out
}

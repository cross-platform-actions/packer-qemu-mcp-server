// Package qmptest provides a stand-in for QEMU's monitor socket.
package qmptest

import (
	"bufio"
	"encoding/json"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Server speaks just enough QMP to stand in for a running QEMU: it greets a
// client, expects the capabilities handshake, and answers commands. Like QEMU
// it can interleave asynchronous events with command replies.
type Server struct {
	socket   string
	listener net.Listener
	silent   bool

	mu          sync.Mutex
	returns     map[string]any
	failures    map[string]qmpError
	events      map[string]string
	executed    []string
	arguments   map[string]map[string]any
	connections int
	screen      image.Point
	later       map[string]delayed
}

// delayed is a second answer, given once a command has been asked enough
// times: a guest that was running when the first key was sent and is not by
// the time the last one is.
type delayed struct {
	after int
	value any
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// Start runs a fake monitor that answers every command with an empty return.
func Start(t *testing.T) *Server {
	return StartAt(t, filepath.Join(SocketDir(t), "mon.sock"))
}

// StartAt runs a fake monitor on a given socket path, for callers that have
// already decided where QEMU's monitor belongs.
func StartAt(t *testing.T, socket string) *Server {
	return start(t, socket, false)
}

// StartSilent runs a fake monitor that accepts connections and then says
// nothing at all, standing in for a wedged QEMU.
func StartSilent(t *testing.T) *Server {
	return start(t, filepath.Join(SocketDir(t), "mon.sock"), true)
}

func start(t *testing.T, socket string, silent bool) *Server {
	t.Helper()

	s := &Server{
		socket:    socket,
		silent:    silent,
		returns:   map[string]any{},
		later:     map[string]delayed{},
		failures:  map[string]qmpError{},
		events:    map[string]string{},
		arguments: map[string]map[string]any{},
	}

	listener, err := net.Listen("unix", s.socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", s.socket, err)
	}
	s.listener = listener
	t.Cleanup(func() { listener.Close() })

	go s.accept()
	return s
}

// SocketDir returns a temporary directory short enough to hold a unix socket:
// the sockaddr_un path limit is around a hundred bytes, well below what the
// default temporary directory leaves room for on macOS.
func SocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "qmptest")
	if err != nil {
		t.Fatalf("creating socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Socket is the path clients connect to.
func (s *Server) Socket() string { return s.socket }

// Reply makes the given command return the given value.
func (s *Server) Reply(execute string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.returns[execute] = value
}

// ReplyAfter makes the given command return the given value once it has been
// executed the given number of times, and whatever Reply says before that.
func (s *Server) ReplyAfter(execute string, after int, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.later[execute] = delayed{after: after, value: value}
}

// Fail makes the given command return a QMP error.
func (s *Server) Fail(execute, class, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[execute] = qmpError{Class: class, Desc: desc}
}

// Draw gives the fake a screen of the given size, which it writes to the file
// a screendump names, the way QEMU does. A caller that measures a capture to
// work out where a point on it is needs a capture with a size.
func (s *Server) Draw(width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screen = image.Point{X: width, Y: height}
}

// EventBefore emits an asynchronous event just before the given command's
// reply, so that a client reading a single line sees the event instead.
func (s *Server) EventBefore(execute, event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[execute] = event
}

// Executed lists the commands received so far, in order, including the
// capabilities handshake.
func (s *Server) Executed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.executed...)
}

// Arguments returns the arguments the given command was last called with.
func (s *Server) Arguments(execute string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arguments[execute]
}

// Connections counts the clients served so far.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	s.connected()

	if s.silent {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		return
	}

	encoder := json.NewEncoder(conn)
	_ = encoder.Encode(map[string]any{
		"QMP": map[string]any{
			"version":      map[string]any{"qemu": map[string]int{"major": 10, "minor": 1, "micro": 2}},
			"capabilities": []string{"oob"},
		},
	})

	decoder := json.NewDecoder(conn)
	for {
		var command struct {
			Execute   string         `json:"execute"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := decoder.Decode(&command); err != nil {
			return
		}
		s.record(command.Execute, command.Arguments)
		s.paint(command.Execute, command.Arguments)
		s.answer(encoder, command.Execute)
	}
}

func (s *Server) answer(encoder *json.Encoder, execute string) {
	s.mu.Lock()
	event, hasEvent := s.events[execute]
	failure, failed := s.failures[execute]
	value, hasValue := s.returns[execute]
	if changed, ok := s.later[execute]; ok && s.counted(execute) > changed.after {
		value, hasValue = changed.value, true
	}
	s.mu.Unlock()

	if hasEvent {
		_ = encoder.Encode(map[string]any{
			"event":     event,
			"timestamp": map[string]int{"seconds": 1, "microseconds": 0},
		})
	}
	switch {
	case failed:
		_ = encoder.Encode(map[string]any{"error": failure})
	case hasValue:
		_ = encoder.Encode(map[string]any{"return": value})
	default:
		_ = encoder.Encode(map[string]any{"return": map[string]any{}})
	}
}

// paint writes the screen to the file a screendump asked for.
func (s *Server) paint(execute string, arguments map[string]any) {
	s.mu.Lock()
	screen := s.screen
	s.mu.Unlock()

	if execute != "screendump" || screen.X == 0 {
		return
	}
	filename, named := arguments["filename"].(string)
	if !named {
		return
	}

	file, err := os.Create(filename)
	if err != nil {
		return
	}
	defer file.Close()
	_ = png.Encode(file, image.NewRGBA(image.Rectangle{Max: screen}))
}

// counted is how many times a command has been executed, including this one.
// The caller holds the lock.
func (s *Server) counted(execute string) int {
	seen := 0
	for _, was := range s.executed {
		if was == execute {
			seen++
		}
	}
	return seen
}

func (s *Server) record(execute string, arguments map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executed = append(s.executed, execute)
	if arguments != nil {
		s.arguments[execute] = arguments
	}
}

func (s *Server) connected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connections++
}

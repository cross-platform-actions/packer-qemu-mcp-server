// Package control is how anything reaches a running build's supervisor.
//
// The supervisor holds the build's terminal for as long as the build lives,
// which is longer than any client's lifetime. Clients therefore attach and
// detach freely over a unix socket, exchanging newline-delimited JSON:
// {"id","method","params"} in, {"id","ok","result"} out.
package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// ErrNoSupervisor is returned when nothing is listening on the control
// socket, which is the normal state of a build that has finished.
var ErrNoSupervisor = errors.New("no supervisor is listening")

// DefaultTimeout bounds a call. Nothing the supervisor does in response to a
// call is slow: it writes a byte to a terminal or a command to a socket.
const DefaultTimeout = 10 * time.Second

// Handler answers one method call.
type Handler func(method string, params json.RawMessage) (any, error)

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Server answers calls on a control socket.
type Server struct {
	socket   string
	listener net.Listener
	handler  Handler
}

// Listen starts serving calls on the given socket path.
func Listen(socket string, handler Handler) (*Server, error) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socket, err)
	}
	// The socket can inject keystrokes into a virtual machine and kill a
	// build; keep it to its owner.
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("securing %s: %w", socket, err)
	}

	server := &Server{socket: socket, listener: listener, handler: handler}
	go server.accept()
	return server, nil
}

// Close stops serving and removes the socket, so that a client can tell a
// finished build from a running one.
func (s *Server) Close() error {
	err := s.listener.Close()
	_ = os.Remove(s.socket)
	return err
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

	decoder := json.NewDecoder(bufio.NewReader(conn))
	encoder := json.NewEncoder(conn)
	for {
		var req request
		if err := decoder.Decode(&req); err != nil {
			return
		}
		if err := encoder.Encode(s.answer(req)); err != nil {
			return
		}
	}
}

func (s *Server) answer(req request) response {
	result, err := s.handler(req.Method, req.Params)
	if err != nil {
		return response{ID: req.ID, Error: err.Error()}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return response{ID: req.ID, Error: fmt.Sprintf("encoding the result of %s: %s", req.Method, err)}
	}
	return response{ID: req.ID, OK: true, Result: encoded}
}

// Client calls a supervisor.
type Client struct {
	Timeout time.Duration

	socket string
}

// Dial returns a client for the supervisor listening on the given socket. No
// connection is made until a call is placed.
func Dial(socket string) *Client {
	return &Client{Timeout: DefaultTimeout, socket: socket}
}

// Call invokes a method and decodes its result into out, which may be nil.
func (c *Client) Call(method string, params, out any) error {
	deadline := time.Now().Add(c.Timeout)

	conn, err := net.DialTimeout("unix", c.socket, c.Timeout)
	if err != nil {
		return fmt.Errorf("%w on %s: %w", ErrNoSupervisor, c.socket, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	encoded, err := encodeParams(params)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(request{ID: 1, Method: method, Params: encoded}); err != nil {
		return fmt.Errorf("sending %s: %w", method, err)
	}

	var res response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return fmt.Errorf("reading the reply to %s: %w", method, err)
	}
	if !res.OK {
		return fmt.Errorf("%s failed: %s", method, res.Error)
	}
	if out == nil || len(res.Result) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result, out)
}

func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encoding parameters: %w", err)
	}
	return encoded, nil
}

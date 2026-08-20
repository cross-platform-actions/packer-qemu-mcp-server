package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCallReturnsTheHandlerResult(t *testing.T) {
	socket := listen(t, func(method string, params json.RawMessage) (any, error) {
		return map[string]string{"method": method}, nil
	})

	var result map[string]string
	require.NoError(t, Dial(socket).Call("continue", nil, &result))

	require.Equal(t, map[string]string{"method": "continue"}, result)
}

func TestCallPassesParameters(t *testing.T) {
	var seen json.RawMessage
	socket := listen(t, func(method string, params json.RawMessage) (any, error) {
		seen = params
		return nil, nil
	})

	require.NoError(t, Dial(socket).Call("abort", map[string]bool{"force": true}, nil))

	require.JSONEq(t, `{"force":true}`, string(seen))
}

func TestHandlerFailureIsReportedToTheCaller(t *testing.T) {
	socket := listen(t, func(string, json.RawMessage) (any, error) {
		return nil, errors.New("the guest is gone")
	})

	err := Dial(socket).Call("continue", nil, nil)

	require.ErrorContains(t, err, "the guest is gone")
}

func TestCallWithoutASupervisorIsRecognisable(t *testing.T) {
	err := Dial(filepath.Join(t.TempDir(), "ctl.sock")).Call("continue", nil, nil)

	require.ErrorIs(t, err, ErrNoSupervisor)
}

func TestSocketIsPrivate(t *testing.T) {
	socket := listen(t, nothing)

	info, err := os.Stat(socket)

	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestClosingRemovesTheSocket(t *testing.T) {
	socket := filepath.Join(socketDir(t), "ctl.sock")
	server, err := Listen(socket, nothing)
	require.NoError(t, err)

	require.NoError(t, server.Close())

	require.NoFileExists(t, socket)
}

func TestListeningTwiceOnTheSameSocketFails(t *testing.T) {
	socket := listen(t, nothing)

	_, err := Listen(socket, nothing)

	require.Error(t, err)
}

func TestAStuckHandlerDoesNotHangTheCaller(t *testing.T) {
	socket := listen(t, func(string, json.RawMessage) (any, error) {
		time.Sleep(time.Minute)
		return nil, nil
	})
	client := Dial(socket)
	client.Timeout = 100 * time.Millisecond

	err := client.Call("continue", nil, nil)

	require.ErrorContains(t, err, "timeout")
}

func TestCallersAreServedConcurrently(t *testing.T) {
	socket := listen(t, func(string, json.RawMessage) (any, error) {
		time.Sleep(50 * time.Millisecond)
		return nil, nil
	})
	done := make(chan error, 4)

	for range cap(done) {
		go func() { done <- Dial(socket).Call("status", nil, nil) }()
	}

	for range cap(done) {
		require.NoError(t, <-done)
	}
}

func listen(t *testing.T, handler Handler) string {
	t.Helper()

	socket := filepath.Join(socketDir(t), "ctl.sock")
	server, err := Listen(socket, handler)
	require.NoError(t, err)
	t.Cleanup(func() { server.Close() })
	return socket
}

// socketDir is short enough for a unix socket path, which the default
// temporary directory on macOS is not.
func socketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "ctl")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func nothing(string, json.RawMessage) (any, error) { return nil, nil }

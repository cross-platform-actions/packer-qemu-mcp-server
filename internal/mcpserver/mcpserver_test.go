package mcpserver

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/builds"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/buildstate"
	"github.com/cross-platform-actions/packer-qemu-mcp-server/internal/guest"
)

func TestTheServerIntroducesItselfByTheProjectName(t *testing.T) {
	// This is what a client displays and what a user writes in its
	// configuration, so it is worth pinning.
	session := connect(t, &stubBuilds{})

	require.Equal(t, "packer-qemu-mcp-server", session.InitializeResult().ServerInfo.Name)
}

func TestEveryToolInTheSurfaceIsOffered(t *testing.T) {
	session := connect(t, &stubBuilds{})

	tools, err := session.ListTools(context.Background(), nil)

	require.NoError(t, err)
	require.ElementsMatch(t,
		[]string{"start_build", "list_builds", "status", "wait_for_pause", "screenshot",
			"send_keys", "click", "continue", "abort"},
		names(tools.Tools))
}

func TestStartBuildPassesTheRequestOn(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	result := call(t, session, "start_build", map[string]any{
		"template":   "ubuntu.pkr.hcl",
		"vars":       map[string]any{"disk_size": "20G"},
		"extra_args": []any{"-only=qemu.ubuntu"},
	})

	require.False(t, result.IsError, text(result))
	require.Equal(t, "ubuntu.pkr.hcl", stub.request.Template)
	require.Equal(t, map[string]string{"disk_size": "20G"}, stub.request.Vars)
	require.Equal(t, []string{"-only=qemu.ubuntu"}, stub.request.ExtraArgs)
	require.Contains(t, text(result), "ubuntu-1")
}

func TestStartBuildReportsAFailureToTheAgent(t *testing.T) {
	session := connect(t, &stubBuilds{err: errors.New("cannot read the template")})

	result := call(t, session, "start_build", map[string]any{"template": "ubuntu.pkr.hcl"})

	require.True(t, result.IsError)
	require.Contains(t, text(result), "cannot read the template")
}

func TestStatusAsksForAFewLinesOfLogByDefault(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	call(t, session, "status", map[string]any{"build_id": "ubuntu-1"})

	require.Equal(t, "ubuntu-1", stub.id)
	require.Positive(t, stub.logLines)
}

func TestStatusCanBeAskedForMoreLog(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	call(t, session, "status", map[string]any{"build_id": "ubuntu-1", "log_lines": 200})

	require.Equal(t, 200, stub.logLines)
}

func TestScreenshotReturnsAPathToRead(t *testing.T) {
	stub := &stubBuilds{screenshot: "/tmp/pk-builds/ubuntu-1/shots/one.png"}
	session := connect(t, stub)

	result := call(t, session, "screenshot", map[string]any{"build_id": "ubuntu-1"})

	require.False(t, result.IsError, text(result))
	require.Contains(t, text(result), "/tmp/pk-builds/ubuntu-1/shots/one.png")
}

func TestContinueAndAbortReachTheBuild(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	require.False(t, call(t, session, "continue", map[string]any{"build_id": "ubuntu-1"}).IsError)
	require.False(t, call(t, session, "abort", map[string]any{"build_id": "ubuntu-1"}).IsError)

	require.Equal(t, []string{"continue", "abort"}, stub.told)
}

func TestContinueCanNameTheStepToRunTo(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	result := call(t, session, "continue", map[string]any{
		"build_id":    "ubuntu-1",
		"until_label": "Compiler tools",
	})

	require.False(t, result.IsError, text(result))
	require.Equal(t, "Compiler tools", stub.untilLabel)
	require.Contains(t, text(result), "Compiler tools")
}

func TestListBuildsNeedsNoArguments(t *testing.T) {
	stub := &stubBuilds{summaries: []builds.Summary{{ID: "ubuntu-1", Label: "boot menu"}}}
	session := connect(t, stub)

	result := call(t, session, "list_builds", map[string]any{})

	require.False(t, result.IsError, text(result))
	require.Contains(t, text(result), "ubuntu-1")
	require.False(t, stub.all)
}

// A short list on a host full of builds must not read as an empty host.
func TestListBuildsSaysWhatItLeftOut(t *testing.T) {
	stub := &stubBuilds{omitted: 3}
	session := connect(t, stub)

	result := call(t, session, "list_builds", map[string]any{})

	require.Contains(t, text(result), "3 older builds left out")
}

func TestListBuildsCanAskForTheWholeHistory(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	call(t, session, "list_builds", map[string]any{"all": true})

	require.True(t, stub.all)
}

func TestStartBuildCarriesTheFreezeSettings(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	call(t, session, "start_build", map[string]any{
		"template":  "ubuntu.pkr.hcl",
		"freeze":    false,
		"settle_ms": 750,
	})

	require.NotNil(t, stub.request.Freeze)
	require.False(t, *stub.request.Freeze)
	require.Equal(t, 750*time.Millisecond, stub.request.Settle)
}

func TestWaitForPauseHasABoundedDefault(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	result := call(t, session, "wait_for_pause", map[string]any{"build_id": "ubuntu-1"})

	require.False(t, result.IsError, text(result))
	require.Equal(t, "ubuntu-1", stub.id)
	require.Positive(t, stub.waited)
}

// An hour is the ceiling: a tool call that never returns is a session that
// cannot be steered.
func TestWaitForPauseWillNotBeAskedToWaitForever(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	call(t, session, "wait_for_pause", map[string]any{"build_id": "ubuntu-1", "timeout_seconds": 86400})

	require.Equal(t, time.Hour, stub.waited)
}

func TestSendKeysPassesTextAndKeysOn(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	result := call(t, session, "send_keys", map[string]any{
		"build_id": "ubuntu-1",
		"text":     "listdev",
		"keys":     []any{"ret"},
	})

	require.False(t, result.IsError, text(result))
	require.Equal(t, "listdev", stub.text)
	require.Equal(t, []string{"ret"}, stub.keys)
}

func TestClickDefaultsToOneLeftButtonPress(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	result := call(t, session, "click", map[string]any{"build_id": "ubuntu-1", "x": 100, "y": 200})

	require.False(t, result.IsError, text(result))
	require.Equal(t, "left", stub.button)
	require.Equal(t, 1, stub.times)
	require.Contains(t, text(result), "(100, 200)")
}

// An abort that returns while Packer is still deleting its output directory is
// an abort whose caller races the next build into the wreckage, so the tool
// says which of the two happened.
func TestAbortSaysWhetherTheBuildIsActuallyGone(t *testing.T) {
	stub := &stubBuilds{}
	session := connect(t, stub)

	stopping := call(t, session, "abort", map[string]any{"build_id": "ubuntu-1"})
	stub.finished = true
	stopped := call(t, session, "abort", map[string]any{"build_id": "ubuntu-1"})

	require.Contains(t, text(stopping), "has not finished yet")
	require.Contains(t, text(stopped), "has stopped")
}

func connect(t *testing.T, service Builds) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	_, err := New(service, "test").Connect(ctx, serverTransport, nil)
	require.NoError(t, err)

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, tool string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: arguments})
	require.NoError(t, err)
	return result
}

func text(result *mcp.CallToolResult) string {
	var joined string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			joined += text.Text
		}
	}
	return joined
}

func names(tools []*mcp.Tool) []string {
	found := make([]string, 0, len(tools))
	for _, tool := range tools {
		found = append(found, tool.Name)
	}
	return found
}

// stubBuilds records what the tools asked of the service.
type stubBuilds struct {
	request    builds.Request
	id         string
	untilLabel string
	logLines   int
	all        bool
	omitted    int
	waited     time.Duration
	text       string
	keys       []string
	button     string
	times      int
	finished   bool
	told       []string
	summaries  []builds.Summary
	screenshot string
	err        error
}

func (s *stubBuilds) Start(request builds.Request) (*builds.Build, error) {
	s.request = request
	if s.err != nil {
		return nil, s.err
	}
	return &builds.Build{
		Meta: buildstate.Meta{ID: "ubuntu-1", Command: []string{"packer", "build"}},
		Dir:  buildstate.At("/tmp/pk-builds/ubuntu-1"),
	}, nil
}

func (s *stubBuilds) List(all bool) (*builds.Listing, error) {
	s.all = all
	if s.err != nil {
		return nil, s.err
	}
	return &builds.Listing{Builds: s.summaries, Omitted: s.omitted}, nil
}

func (s *stubBuilds) Status(id string, logLines int) (*builds.Report, error) {
	s.id, s.logLines = id, logLines
	if s.err != nil {
		return nil, s.err
	}
	return &builds.Report{Summary: builds.Summary{ID: id, Finished: s.finished}}, nil
}

func (s *stubBuilds) WaitForPause(_ context.Context, id string, within time.Duration, logLines int) (*builds.Report, error) {
	s.waited = within
	return s.Status(id, logLines)
}

func (s *stubBuilds) SendKeys(id, text string, keys []string, _ time.Duration) error {
	s.id, s.text, s.keys = id, text, keys
	return s.err
}

func (s *stubBuilds) Click(id string, at image.Point, button string, times int) (*builds.Clicked, error) {
	s.id, s.button, s.times = id, button, times
	if s.err != nil {
		return nil, s.err
	}
	return &builds.Clicked{
		X:      at.X,
		Y:      at.Y,
		Screen: guest.Screen{Path: s.screenshot, Width: 1024, Height: 768},
	}, nil
}

func (s *stubBuilds) Screenshot(id string) (string, error) {
	s.id = id
	return s.screenshot, s.err
}

func (s *stubBuilds) Continue(id, untilLabel string) error {
	s.id, s.untilLabel = id, untilLabel
	s.told = append(s.told, "continue")
	return s.err
}

func (s *stubBuilds) Abort(_ context.Context, id string, logLines int) (*builds.Report, error) {
	s.told = append(s.told, "abort")
	if s.err != nil {
		return nil, s.err
	}
	return s.Status(id, logLines)
}

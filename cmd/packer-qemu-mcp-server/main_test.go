package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHelpFlagsPrintTheUsageAndSucceed(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errOut bytes.Buffer

			require.NoError(t, run([]string{arg}, &out, &errOut))

			require.Contains(t, out.String(), "packer-qemu-mcp-server [serve]")
			// Asking for the help is not an error, so nothing goes to stderr.
			require.Empty(t, errOut.String())
		})
	}
}

func TestVersionFlagsPrintTheVersion(t *testing.T) {
	for _, arg := range []string{"-v", "--version", "version"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer

			require.NoError(t, run([]string{arg}, &out, io.Discard))

			require.Equal(t, version+"\n", out.String())
		})
	}
}

func TestUnknownCommandFailsAndPrintsTheUsageToStderr(t *testing.T) {
	var out, errOut bytes.Buffer

	err := run([]string{"bogus"}, &out, &errOut)

	require.EqualError(t, err, "unknown command: bogus")
	require.Contains(t, errOut.String(), "packer-qemu-mcp-server [serve]")
	// stdout belongs to the MCP protocol; a mistake on the command line must
	// not put anything on it.
	require.Empty(t, out.String())
}

func TestUsageSaysThePkBuildsDirAndPackerBinAreEnvironmentVariables(t *testing.T) {
	var out bytes.Buffer

	usage(&out)

	require.Regexp(t, `(?s)Environment variables.*PK_BUILDS_DIR.*PACKER_BIN`, out.String())
}

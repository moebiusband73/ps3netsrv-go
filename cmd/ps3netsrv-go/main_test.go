package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/kongutil"
)

func TestTranslateArgs(t *testing.T) {
	k := kong.Must(new(app), kong.Name("ps3netsrv-go"), kongutil.OutputFileMapper, kongutil.BinSizeMapper)

	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.Mkdir("GAME", 0o755))
	require.NoError(t, os.Mkdir("server", 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join("PS3GAME", "PS3_GAME"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("PS3GAME", "PS3_GAME", "PARAM.SFO"), nil, 0o644))

	for _, tc := range []struct {
		args     []string
		expected []string
	}{
		{[]string{"GAME"}, []string{"make-iso", "GAME", "GAME.ISO"}},
		{[]string{"GAME" + string(filepath.Separator)}, []string{"make-iso", "GAME", "GAME.ISO"}},
		{[]string{"PS3GAME"}, []string{"make-iso", "--ps3-mode", "PS3GAME", "PS3GAME.ISO"}},
		{[]string{"server"}, []string{"server"}}, // subcommand name wins over directory
		{[]string{"GAME", "x"}, []string{"GAME", "x"}},
		{nil, nil},
	} {
		got := translateArgs(k, tc.args)
		require.Equal(t, tc.expected, got, "args %q", tc.args)

		if len(got) > 0 && got[0] == "make-iso" {
			_, err := k.Parse(got)
			require.NoError(t, err, "translated args must be parseable")
			require.NoError(t, os.Remove(got[len(got)-1])) // output file mapper creates it
		}
	}
}

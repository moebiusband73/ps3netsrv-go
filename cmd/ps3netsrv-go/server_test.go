package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

func TestMakeListenerUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets with permissions are not supported on windows")
	}

	dir := t.TempDir()

	t.Run("regular file is not removed", func(t *testing.T) {
		path := filepath.Join(dir, "regular")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		_, err := makeListener("unix:" + path)
		require.Error(t, err)

		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "data", string(content))
	})

	t.Run("stale socket is replaced", func(t *testing.T) {
		path := filepath.Join(dir, "s.sock")

		lis, err := makeListener("unix:" + path + ",600")
		require.NoError(t, err)
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

		// emulate unclean shutdown: socket file is left behind
		if ul, ok := lis.(interface{ SetUnlinkOnClose(bool) }); ok {
			ul.SetUnlinkOnClose(false)
		}
		require.NoError(t, lis.Close())

		lis, err = makeListener("unix:" + path)
		require.NoError(t, err)
		require.NoError(t, lis.Close())
	})

	t.Run("invalid permissions", func(t *testing.T) {
		_, err := makeListener("unix:" + filepath.Join(dir, "p.sock") + ",rw")
		require.Error(t, err)
	})
}

func TestS3OptionsConfig(t *testing.T) {
	opts := s3Options{Endpoint: "http://nas.local:9000/", BucketLookup: "path", AccessKey: "ak", SecretKey: "sk"}
	cfg, err := opts.config("s3://games/ps3/lib")
	require.NoError(t, err)
	require.Equal(t, "nas.local:9000", cfg.Endpoint)
	require.False(t, cfg.Secure)
	require.Equal(t, "games", cfg.Bucket)
	require.Equal(t, "ps3/lib", cfg.Prefix)
	require.Equal(t, minio.BucketLookupPath, cfg.BucketLookup)
	require.NotNil(t, cfg.Creds)

	opts = s3Options{Endpoint: "s3.amazonaws.com", BucketLookup: "auto"}
	cfg, err = opts.config("s3://bucket")
	require.NoError(t, err)
	require.True(t, cfg.Secure)
	require.Nil(t, cfg.Creds, "default credentials chain must be used")

	_, err = opts.config("s3://")
	require.Error(t, err)
}

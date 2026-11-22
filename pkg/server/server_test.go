package server_test

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/pkg/proto"
	"github.com/xakep666/ps3netsrv-go/pkg/server"
)

type state struct{}

type ctx = server.Context[state]

var errStub = errors.New("stub")

// stubHandler fails every operation without touching request data.
type stubHandler struct {
	panicOnOpenDir bool
}

func (stubHandler) Init(*ctx) error { return nil }
func (h stubHandler) HandleOpenDir(*ctx, string) (bool, error) {
	if h.panicOnOpenDir {
		panic("boom")
	}
	return false, errStub
}
func (stubHandler) HandleReadDir(*ctx) ([]fs.FileInfo, error)        { return nil, errStub }
func (stubHandler) HandleReadDirEntry(*ctx) (fs.FileInfo, error)     { return nil, errStub }
func (stubHandler) HandleStatFile(*ctx, string) (fs.FileInfo, error) { return nil, errStub }
func (stubHandler) HandleOpenFile(*ctx, string) (fs.FileInfo, error) { return nil, errStub }
func (stubHandler) HandleCloseFile(*ctx) error                       { return nil }
func (stubHandler) HandleCreateFile(*ctx, string) error              { return errStub }
func (stubHandler) HandleWriteFile(*ctx, io.Reader) (int32, error)   { return 0, errStub }
func (stubHandler) HandleDeleteFile(*ctx, string) error              { return errStub }
func (stubHandler) HandleMkdir(*ctx, string) error                   { return errStub }
func (stubHandler) HandleRmdir(*ctx, string) error                   { return errStub }
func (stubHandler) HandleGetDirSize(*ctx, string) (int64, error)     { return 0, errStub }
func (stubHandler) HandleReadFileCritical(*ctx, uint32, uint64, io.Writer) error {
	return errStub
}
func (stubHandler) HandleReadCD2048Critical(*ctx, uint32, uint32, io.Writer) error {
	return errStub
}
func (stubHandler) HandleReadFile(*ctx, uint32, uint64, server.ReadFileResponseWriter) error {
	return errStub
}

func startServer(t *testing.T, h server.Handler[state]) net.Conn {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &server.Server[state]{
		Handler: h,
		Logger:  slog.New(slog.DiscardHandler),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	return conn
}

func command(op proto.OpCode, tail ...any) []byte {
	var cmd [16]byte
	binary.BigEndian.PutUint16(cmd[:], uint16(op))
	pos := 2
	for _, v := range tail {
		n, err := binary.Encode(cmd[pos:], binary.BigEndian, v)
		if err != nil {
			panic(err)
		}
		pos += n
	}
	return cmd[:]
}

func TestWriteFilePayloadDrainedOnError(t *testing.T) {
	conn := startServer(t, stubHandler{})

	payload := []byte("0123456789")
	_, err := conn.Write(command(proto.CmdWriteFile, uint16(0), uint32(len(payload))))
	require.NoError(t, err)
	_, err = conn.Write(payload)
	require.NoError(t, err)

	var writeResult proto.WriteFileResult
	require.NoError(t, binary.Read(conn, binary.BigEndian, &writeResult))
	require.EqualValues(t, -1, writeResult.BytesWritten)

	// next command must be parsed correctly
	path := "/some/path"
	_, err = conn.Write(command(proto.CmdStatFile, uint16(len(path))))
	require.NoError(t, err)
	_, err = conn.Write([]byte(path))
	require.NoError(t, err)

	var statResult proto.StatFileResult
	require.NoError(t, binary.Read(conn, binary.BigEndian, &statResult))
	require.EqualValues(t, -1, statResult.FileSize)
}

func TestHandlerPanicDoesNotCrashServer(t *testing.T) {
	conn := startServer(t, stubHandler{panicOnOpenDir: true})

	path := "/"
	_, err := conn.Write(command(proto.CmdOpenDir, uint16(len(path))))
	require.NoError(t, err)
	_, err = conn.Write([]byte(path))
	require.NoError(t, err)

	// connection must be closed by server
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

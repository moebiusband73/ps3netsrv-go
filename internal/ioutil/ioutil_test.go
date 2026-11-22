package ioutil_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/ioutil"
)

// readSeekerOnly hides io.ReaderAt implementation.
type readSeekerOnly struct{ io.ReadSeeker }

func TestFillBuffer(t *testing.T) {
	for name, wrap := range map[string]func(*strings.Reader) io.ReadSeeker{
		"ReaderAt":   func(r *strings.Reader) io.ReadSeeker { return r },
		"ReadSeeker": func(r *strings.Reader) io.ReadSeeker { return readSeekerOnly{r} },
	} {
		t.Run(name, func(t *testing.T) {
			r := strings.NewReader("0123456789")
			_, err := r.Seek(2, io.SeekStart)
			require.NoError(t, err)

			buf := make([]byte, 3)
			require.NoError(t, ioutil.FillBuffer(wrap(r), 5, buf))
			require.Equal(t, "567", string(buf))

			// position must be preserved
			pos, err := r.Seek(0, io.SeekCurrent)
			require.NoError(t, err)
			require.EqualValues(t, 2, pos)

			require.ErrorIs(t, ioutil.FillBuffer(wrap(r), 8, buf), io.ErrUnexpectedEOF)
		})
	}
}

func TestCopyNShort(t *testing.T) {
	for name, c := range map[string]*ioutil.Copier{
		"plain":  ioutil.NewCopier(),
		"pooled": ioutil.NewPooledCopier(4),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := c.CopyN(&buf, strings.NewReader("abc"), 10)
			require.ErrorIs(t, err, io.EOF)
			require.EqualValues(t, 3, n)

			buf.Reset()
			n, err = c.CopyN(&buf, strings.NewReader("abcdef"), 4)
			require.NoError(t, err)
			require.EqualValues(t, 4, n)
			require.Equal(t, "abcd", buf.String())
		})
	}
}

package iso3k3y

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/iso9660"
)

func TestClear3k3yData(t *testing.T) {
	const imageSize = 0x2000
	expected := bytes.Repeat([]byte{0xff}, imageSize)
	for i := _3k3yMaskedDataBegin; i < _3k3yMaskedDataEnd; i++ {
		expected[i] = 0
	}

	for _, span := range []struct{ start, end int }{
		{0, imageSize},
		{0, 0xF70},
		{0xF70, 0x1070},
		{0x1000, 0x1800}, // sector 2, starts inside masked area
		{0xFFF, 0x1001},
		{0x1060, 0x2000},
		{0x1070, 0x2000},
	} {
		data := bytes.Repeat([]byte{0xff}, span.end-span.start)
		(*ISO3k3y)(nil).clear3k3yData(iso9660.SizeBytes(span.start), data)
		require.Equal(t, expected[span.start:span.end], data, "span [%#x;%#x)", span.start, span.end)
	}
}

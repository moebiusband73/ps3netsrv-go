//go:build linux

package filesystem

import "golang.org/x/sys/unix"

// openLargeFile is O_LARGEFILE on 32-bit targets (0x8000 on 386, 0x20000 on
// arm, ...) and 0 on 64-bit ones. Without it, os.Root's openat rejects files
// larger than 2 GiB on a 32-bit kernel with EOVERFLOW ("value too large for
// defined data type"), which breaks streaming of PS3 ISOs on legacy 32-bit NAS
// hardware.
const openLargeFile = unix.O_LARGEFILE

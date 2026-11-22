package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/alecthomas/kong"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/net/netutil"
	"golang.org/x/sync/errgroup"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	"github.com/xakep666/ps3netsrv-go/internal/ioutil"
	"github.com/xakep666/ps3netsrv-go/internal/logutil"
	"github.com/xakep666/ps3netsrv-go/internal/osutil/filesystem"
	"github.com/xakep666/ps3netsrv-go/internal/osutil/osuser"
	"github.com/xakep666/ps3netsrv-go/internal/osutil/socketactivation"
	"github.com/xakep666/ps3netsrv-go/internal/osutil/systemlog"
	"github.com/xakep666/ps3netsrv-go/pkg/fs"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/chd"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/cso"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/encryptediso"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/iso3k3y"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/s3root"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/seekablezstd"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/viso"
	"github.com/xakep666/ps3netsrv-go/pkg/iprange"
	"github.com/xakep666/ps3netsrv-go/pkg/server"
)

type serverApp struct {
	Root                  string           `help:"Root directory with games or S3 URL (s3://bucket/prefix, see S3 flags)." default:"." env:"PS3NETSRV_ROOT"`
	ListenAddr            string           `help:"Main server listen address." default:"0.0.0.0:38008" env:"PS3NETSRV_LISTEN_ADDR"`
	Debug                 bool             `help:"Enable debug log messages. DEPRECATED: use --log-level." env:"PS3NETSRV_DEBUG"`
	LogLevel              slog.Level       `help:"Logging level." default:"info" env:"PS3NETSRV_LOG_LEVEL"`
	JSONLog               bool             `help:"Output log messages in json format." env:"PS3NETSRV_JSON_LOG"`
	DebugServerListenAddr string           `help:"Enables debug server (with pprof) if provided." env:"PS3NETSRV_DEBUG_SERVER_LISTEN_ADDR"`
	ReadTimeout           time.Duration    `help:"Timeout for incoming commands. Connection will be closed on expiration. Use '0' to disable (by default). Enabling is recommended if you plan to host a lot of clients with possibly unstable connections." default:"0" env:"PS3NETSRV_READ_TIMEOUT"`
	MaxClients            int              `help:"Limit amount of connected clients. Negative or zero means no limit." env:"PS3NETSRV_MAX_CLIENTS"`
	ClientWhitelist       *iprange.IPRange `help:"Optional client IP whitelist. Formats: single IPv4/v6 ('192.168.0.2'), IPv4/v6 CIDR ('192.168.0.1/24'), IPv4 + subnet mask ('192.168.0.1/255.255.255.0), IPv4/IPv6 range ('192.168.0.1-192.168.0.255')." env:"PS3NETSRV_CLIENT_WHITELIST"`
	AllowWrite            bool             `help:"Allow writing/modifying filesystem operations." env:"PS3NETSRV_ALLOW_WRITE"`
	StrictRoot            bool             `help:"Stricter root protection from path traversal, referencing to outside symlinks, etc. Highly recommended if you plan to expose server outside of local network." env:"PS3NETSRV_STRICT_ROOT"`
	ShutdownIdleTimeout   time.Duration    `help:"Automatically shutdown server if no clients connected for provided amount of time. Zero or negative value to disable." env:"PS3NETSRV_SHUTDOWN_IDLE_TIMEOUT"`
	// default value found during debugging
	BufferSize int64 `help:"Size of buffer for data transfer. Change it only if you know what you doing." type:"binsize" default:"64k" env:"PS3NETSRV_BUFFER_SIZE"`
	SystemLog  bool  `help:"Send logs to system logger instead of stdout." env:"PS3NETSRV_SYSTEM_LOG"`

	S3 s3Options `embed:"" prefix:"s3-" envprefix:"PS3NETSRV_S3_" group:"S3 storage (used when root is s3://bucket/prefix)"`
}

type s3Options struct {
	Endpoint       string        `help:"S3 endpoint: host[:port], optionally with http:// or https:// scheme." default:"s3.amazonaws.com" env:"ENDPOINT"`
	Region         string        `help:"S3 region, detected automatically if empty." env:"REGION"`
	BucketLookup   string        `help:"Bucket addressing style: auto, dns (virtual-hosted) or path. Use 'path' for most self-hosted servers." enum:"auto,dns,path" default:"auto" env:"BUCKET_LOOKUP"`
	Insecure       bool          `help:"Use plain HTTP instead of HTTPS." env:"INSECURE"`
	AccessKey      string        `help:"S3 access key. If empty, AWS/MinIO environment variables, credential files or IAM are used." env:"ACCESS_KEY"`
	SecretKey      string        `help:"S3 secret key. Prefer environment variable or config file over command line." env:"SECRET_KEY"`
	CacheSize      int64         `help:"Size of in-memory cache for S3 data." type:"binsize" default:"128m" env:"CACHE_SIZE"`
	BlockSize      int64         `help:"Cache page size, also minimal request size." type:"binsize" default:"64k" env:"BLOCK_SIZE"`
	Readahead      int64         `help:"Maximal readahead window for sequential reads." type:"binsize" default:"8m" env:"READAHEAD"`
	MetadataTTL    time.Duration `help:"How long object listings and metadata are cached." default:"1m" env:"METADATA_TTL"`
	RequestTimeout time.Duration `help:"Timeout for S3 response headers and stalled response bodies." default:"30s" env:"REQUEST_TIMEOUT"`
}

func (o *s3Options) config(rootURL string) (s3root.Config, error) {
	bucket, prefix, err := s3root.ParseURL(rootURL)
	if err != nil {
		return s3root.Config{}, fmt.Errorf("parse s3 root: %w", err)
	}

	endpoint, secure := o.Endpoint, !o.Insecure
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, secure = rest, false
	} else if rest, ok = strings.CutPrefix(endpoint, "https://"); ok {
		endpoint, secure = rest, true
	}
	endpoint = strings.TrimSuffix(endpoint, "/")

	var lookup minio.BucketLookupType
	switch o.BucketLookup {
	case "dns":
		lookup = minio.BucketLookupDNS
	case "path":
		lookup = minio.BucketLookupPath
	default:
		lookup = minio.BucketLookupAuto
	}

	cfg := s3root.Config{
		Endpoint:       endpoint,
		Region:         o.Region,
		Secure:         secure,
		BucketLookup:   lookup,
		Bucket:         bucket,
		Prefix:         prefix,
		PageSize:       o.BlockSize,
		CacheSize:      o.CacheSize,
		ReadaheadMax:   o.Readahead,
		MetadataTTL:    o.MetadataTTL,
		RequestTimeout: o.RequestTimeout,
		Logger:         slog.Default(),
	}
	if o.AccessKey != "" || o.SecretKey != "" {
		cfg.Creds = credentials.NewStaticV4(o.AccessKey, o.SecretKey, "")
	}

	return cfg, nil
}

func (sapp *serverApp) Help() string {
	return `Serve data using NETISO protocol from provided root directory.

Flags '--listen-addr' and '--debug-server-listen-addr' supports following syntax:
	* "fd:<id>" - listen on inherited (fork/exec) file descriptor
	* "activated:<name>" - search inherited file descriptor by name in socket-activated environment, i.e. under "systemd-socket-activate"
	* "unix:@abstract_name" - listen on abstract unix domain socket
	* "unix:<path>[,0xxx]" - listen on unix domain socket at <path>. Useful if server is behind a proxy. 
	Use optional ",0xxx" suffix to control socket permissions, i.e. "unix:/var/run/ps3netsrv-go.socket,0770" for ug+rwx permissions
	* regular tcp listener otherwise

For better security it's recommended to run with following options:
	* '--strict-root' - prevents possible directory traversal
	* '--client-whitelist' if you don't have firewall to prevent connections from unwanted networks

It's also highly recommended to run server under non-root user with restricted access especially if '--allow-write' option is enabled.

Root may be an S3-compatible object store URL "s3://bucket/prefix" (read-only, see '--s3-*' flags).
Credentials are taken from '--s3-access-key'/'--s3-secret-key' or from standard AWS/MinIO environment variables,
credential files or IAM. Example for MinIO: '--root=s3://games --s3-endpoint=http://nas:9000 --s3-bucket-lookup=path'.

Option '--max-clients' may be used to limit amount of connected clients to control resources consumption.

Consider setting '--shutdown-idle-timeout' if server will be started by socket-activation.
When this option is set server will automatically shutdown itself if there are no connected clients duing provided period.
It's also recommended to have '--read-timeout' set in this case but not required for local network.
`
}

func (sapp *serverApp) setupLogger(k *kong.Kong) {
	level := sapp.LogLevel
	if sapp.Debug {
		level = slog.LevelDebug
	}

	var slogHandler slog.Handler

	var systemLogErr error
	if sapp.SystemLog {
		slogHandler, systemLogErr = systemlog.SystemLogHandler(level)
	}

	if slogHandler == nil {
		outFile, outIsFile := k.Stdout.(*os.File)
		if sapp.JSONLog {
			slogHandler = slog.NewJSONHandler(k.Stdout, &slog.HandlerOptions{
				Level: level,
			})
		} else if outIsFile {
			slogHandler = tint.NewTextHandler(outFile, &tint.Options{
				Level:   level,
				NoColor: !isatty.IsTerminal(outFile.Fd()),
			})
		} else {
			slogHandler = slog.NewTextHandler(k.Stdout, &slog.HandlerOptions{
				Level: level,
			})
		}
	}

	slogHandler = &handler.SlogContextHandler{Handler: slogHandler}

	slog.SetDefault(slog.New(slogHandler))

	if systemLogErr != nil {
		slog.Warn("System logger init failed, fallback to stdout", logutil.ErrorAttr(systemLogErr))
	}
}

func (sapp *serverApp) debugServer(ctx context.Context, idt *idleTracker) error {
	if sapp.DebugServerListenAddr == "" {
		return nil
	}

	socket, err := makeListener(sapp.DebugServerListenAddr)
	if err != nil {
		return fmt.Errorf("debug server listen failed: %w", err)
	}

	slog.Info("Debug sever listening...", "addr", logutil.ListenAddressValue(socket.Addr()))

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			idt.Connected() // prevent auto-shutdown if user fetches some data over http
			defer idt.Disconnected()
			http.DefaultServeMux.ServeHTTP(w, r)
		}),
	}
	context.AfterFunc(ctx, func() {
		_ = server.Close()
	})

	return server.Serve(socket)
}

func (sapp *serverApp) warnIPRange(listener net.Listener) {
	if sapp.ClientWhitelist == nil {
		return
	}

	var addrToCheck net.IP
	switch addr := listener.Addr().(type) {
	case *net.TCPAddr:
		addrToCheck = addr.IP
	case *net.UDPAddr:
		addrToCheck = addr.IP
	case *net.IPAddr:
		addrToCheck = addr.IP
	default:
		slog.Warn("Client whitelist has no effect for non-IP listener.", "addr", listener.Addr())
		return
	}
	if addrToCheck.IsUnspecified() {
		return
	}

	if !sapp.ClientWhitelist.Contains(addrToCheck) {
		slog.Warn("Listener address is not in client whitelist. This may cause connection problems.",
			"whitelist", sapp.ClientWhitelist)
	}
}

func (sapp *serverApp) isS3Root() bool {
	return s3root.IsURL(sapp.Root)
}

// openSystemRoot creates storage backend. Returned function must be called to release it.
func (sapp *serverApp) openSystemRoot(ctx context.Context) (fs.SystemRoot, func(), error) {
	switch {
	case sapp.isS3Root():
		cfg, err := sapp.S3.config(sapp.Root)
		if err != nil {
			return nil, nil, err
		}

		root, err := s3root.New(cfg)
		if err != nil {
			return nil, nil, err
		}

		checkCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout+5*time.Second)
		defer cancel()
		if err = root.Check(checkCtx); err != nil {
			_ = root.Close()
			return nil, nil, err
		}

		return root, sync.OnceFunc(func() { _ = root.Close() }), nil
	case sapp.StrictRoot:
		root, err := os.OpenRoot(sapp.Root)
		if err != nil {
			return nil, nil, fmt.Errorf("open root failed: %w", err)
		}

		// Wrap so large files (>2 GiB, i.e. every PS3 ISO) can be opened on
		// 32-bit platforms; os.Root's openat omits O_LARGEFILE.
		return filesystem.NewStrictSystemRoot(root), sync.OnceFunc(func() { _ = root.Close() }), nil
	default:
		return fs.NewRelaxedSystemRoot(sapp.Root), func() {}, nil
	}
}

func (sapp *serverApp) server(ctx context.Context, idt *idleTracker, sysRoot fs.SystemRoot) error {
	socket, err := makeListener(sapp.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}

	sapp.warnIPRange(socket)
	slog.Info("Listening...",
		"addr", logutil.ListenAddressValue(socket.Addr()),
		"root", sapp.Root,
	)

	var cop *ioutil.Copier
	if sapp.BufferSize > 0 {
		cop = ioutil.NewPooledCopier(sapp.BufferSize)
	} else {
		cop = ioutil.NewCopier()
	}

	s := server.Server[handler.State]{
		Handler: &handler.Handler{
			Fs: fs.NewFS(sysRoot,
				[]fs.FileOpener{
					viso.Opener{},
					chd.NewOpener(slog.Default()),
					cso.Opener{},
					seekablezstd.Opener{},
				},
				[]fs.FileWrapper{
					filesystem.FileTimesWrapper{}, // must be first to have original file here (system data needed)
					iso3k3y.KeyExtractionFileWrapper{},
					encryptediso.FileWrapper{},
					iso3k3y.FileWrapper{},
				},
			),
			AllowWrite: sapp.AllowWrite,
			Copier:     cop,
			OnConnect: func(ctx *handler.Context) error {
				idt.Connected()
				ctx.State.OnClose = func() error {
					idt.Disconnected()
					return nil
				}
				return nil
			},
		},
		ReadTimeout: sapp.ReadTimeout,
		Logger:      slog.Default(),
	}

	if sapp.MaxClients > 0 {
		socket = netutil.LimitListener(socket, sapp.MaxClients)
	}
	if sapp.ClientWhitelist != nil {
		socket = iprange.FilterListener(socket, sapp.ClientWhitelist, false)
	}

	context.AfterFunc(ctx, func() {
		_ = s.Close()
	})

	return s.Serve(socket)
}

func (sapp *serverApp) warnRoot() {
	if osuser.IsRoot() {
		if sapp.AllowWrite {
			slog.Warn("Running as root/administrator with write access is dangerous! This may damage your data!")
		} else {
			slog.Warn("Running as root/administrator is not recommended! Please run as a regular user.")
		}

		if !sapp.StrictRoot && !sapp.isS3Root() {
			slog.Warn("Running as root/administrator without strict root access is dangerous! This may lead to data stealing!")
		}
	}
}

func (sapp *serverApp) scanAndWarn(root fs.SystemRoot) {
	const maxEntries = 4096 // from ps3netsrv

	// notify user if file name can cause access issues
	const allowedChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_ !\"%&'()*+,-./:;<=>?[]"
	var allowedCharsSet [256]bool
	for _, char := range allowedChars {
		allowedCharsSet[char] = true
	}

	const maxBadNames = 10
	badNameSamples := make([]string, 0, maxBadNames)
	isBadName := func(name string) bool {
		return strings.ContainsFunc(name, func(r rune) bool {
			return r >= rune(len(allowedCharsSet)) || !allowedCharsSet[r]
		})
	}

	queue := []string{"."} // relative to root
	scanDir := func(path string) {
		slog.Debug("Checking dir for entries limit", "dir", path)

		dir, err := root.Open(path)
		if err != nil {
			return
		}

		defer dir.Close()

		var numEntries int
		for {
			entries, err := dir.ReadDir(maxEntries)
			if err != nil {
				break
			}

			numEntries += len(entries)
			for _, entry := range entries {
				if len(badNameSamples) <= maxBadNames && isBadName(entry.Name()) {
					relPath := filepath.Join(path, entry.Name())
					badNameSamples = append(badNameSamples, strconv.Quote(relPath))
				}

				if entry.IsDir() {
					queue = append(queue, filepath.Join(path, entry.Name()))
				}
			}
		}

		if numEntries > maxEntries {
			slog.Warn("Found directory that contains too many entries. Note that WebMan Mod has a limit of entries per directory so some items may be inaccessible.",
				"dir", path, "entries", numEntries, "limit", maxEntries)
		}
	}

	for len(queue) > 0 {
		dir := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		scanDir(dir)
	}

	if len(badNameSamples) > 0 {
		slog.Warn("Found files/directories with names containing unusual characters. These files can cause issues with WebMan Mod or other software on a console. Consider inspecting your collection and renaming files.",
			"allowed_characters", allowedChars, "name_examples", badNameSamples,
		)
	}
}

func (sapp *serverApp) setupRuntime() {
	if runtime.GOOS != "linux" {
		return
	}

	_, err := memlimit.Set(memlimit.WithLogger(slog.Default()))
	switch {
	case errors.Is(err, nil),
		errors.Is(err, memlimit.ErrCgroupsNotSupported),
		errors.Is(err, memlimit.ErrNoCgroup),
		errors.Is(err, memlimit.ErrNoLimit):
		// pass
	default:
		slog.Warn("memlimit setup failed", logutil.ErrorAttr(err))
	}
}

func (sapp *serverApp) Run(ctx context.Context, k *kong.Kong) error {
	if sapp.isS3Root() {
		if sapp.AllowWrite {
			return fmt.Errorf("'--allow-write' is not supported for S3 root")
		}
	} else {
		// do this manually because type:existingdir flags can't be read from config
		newRoot := kong.ExpandPath(sapp.Root)
		di, err := os.Stat(newRoot)
		if err != nil || !di.IsDir() {
			return fmt.Errorf("root %q is not exists or not a directory", sapp.Root)
		}
		sapp.Root = newRoot
	}

	sapp.setupLogger(k)
	sapp.setupRuntime()
	sapp.warnRoot()

	if sapp.isS3Root() && sapp.StrictRoot {
		slog.Info("'--strict-root' has no effect for S3 root")
	}

	ctx, idleCancel := context.WithCancel(ctx)
	defer idleCancel()

	sysRoot, closeRoot, err := sapp.openSystemRoot(ctx)
	if err != nil {
		return err
	}
	defer closeRoot()

	// abort in-flight storage requests on shutdown, otherwise server waits for them
	context.AfterFunc(ctx, closeRoot)

	go sapp.scanAndWarn(sysRoot) // asynchronously to not delay server startup

	idt := newIdleTracker(sapp.ShutdownIdleTimeout, func() {
		slog.Info("Idle timeout expired, shutting down")
		idleCancel()
	})
	defer idt.Cancel()

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return sapp.debugServer(ctx, idt)
	})
	eg.Go(func() error {
		return sapp.server(ctx, idt, sysRoot)
	})

	err = eg.Wait()
	switch {
	case errors.Is(err, nil):
		return nil
	case errors.Is(err, http.ErrServerClosed),
		errors.Is(err, context.Canceled),
		errors.Is(err, server.ErrServerClosed):
		return nil // expected errors when server is being closed
	default:
		return err
	}
}

type idleTracker struct {
	idleTimeout time.Duration

	// mu guards clientCount and timer state together:
	// separate atomic counter and timer operations may interleave and arm the timer while a client is connected.
	mu          sync.Mutex
	clientCount int
	idleTimer   *time.Timer
}

func newIdleTracker(idleTimeout time.Duration, expiredCallback func()) *idleTracker {
	if idleTimeout <= 0 {
		return nil
	}

	t := &idleTracker{idleTimeout: idleTimeout}
	t.idleTimer = time.AfterFunc(idleTimeout, func() {
		// timer may fire concurrently with Connected
		t.mu.Lock()
		idle := t.clientCount <= 0
		t.mu.Unlock()

		if idle {
			expiredCallback()
		}
	})
	return t
}

func (t *idleTracker) Connected() {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.clientCount++
	t.idleTimer.Stop()
}

func (t *idleTracker) Disconnected() {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.clientCount--
	if t.clientCount <= 0 {
		t.idleTimer.Reset(t.idleTimeout)
	}
}

func (t *idleTracker) Cancel() {
	if t == nil {
		return
	}
	t.idleTimer.Stop()
}

// makeListener can create listener from multiple sources:
// * if "addr" starts with "fd:" - from inherited file descriptor
// * if "addr" starts with "activated:" - searches inherited file descriptor by name in socket-activated environment
// * if "addr" starts with "unix:" - unix domain sockets (files or abstract), useful if server is behind a proxy
// * regular tcp listener otherwise
func makeListener(addr string) (net.Listener, error) {
	const (
		fdPrefix        = "fd:"
		activatedPrefix = "activated:"
		unixPrefix      = "unix:"
	)

	switch {
	case strings.HasPrefix(addr, fdPrefix): // listen on inherited file descriptor
		fd, err := strconv.ParseUint(addr[len(fdPrefix):], 10, strconv.IntSize)
		if err != nil {
			return nil, fmt.Errorf("parse fd: %w", err)
		}

		f := os.NewFile(uintptr(fd), "")
		defer f.Close() // can be safely closed here because FileListener dups fd with necessary opts

		return net.FileListener(f)
	case strings.HasPrefix(addr, activatedPrefix):
		// search a "named" file descriptor if we're inside socket-activated environment
		listeners, err := socketactivation.ActivationListeners(addr[len(activatedPrefix):])
		if err != nil {
			return nil, fmt.Errorf("get activated listeners: %w", err)
		}

		if len(listeners) == 0 {
			return nil, fmt.Errorf("no activated listeners found by name")
		}

		return listeners[0], nil
	case strings.HasPrefix(addr, unixPrefix):
		path := addr[len(unixPrefix):]
		if strings.HasPrefix(path, "@") {
			// abstract unix socket
			return net.Listen("unix", path)
		}

		path, perm, hasPerm := strings.Cut(path, ",") // get permissions

		var parsedPerm uint64
		if hasPerm {
			var err error
			if parsedPerm, err = strconv.ParseUint(perm, 8, 32); err != nil {
				return nil, fmt.Errorf("parse unix socket permissions %q: %w", perm, err)
			}
		}

		// remove stale socket left from previous run, but never touch other kinds of files
		if st, err := os.Lstat(path); err == nil {
			if st.Mode().Type() != os.ModeSocket {
				return nil, fmt.Errorf("%s exists and is not a socket", path)
			}
			if err = os.Remove(path); err != nil {
				return nil, fmt.Errorf("remove stale socket: %w", err)
			}
		}

		lis, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}

		if hasPerm {
			// apply permissions to file
			if err = os.Chmod(path, os.FileMode(parsedPerm)); err != nil {
				_ = lis.Close()
				return nil, fmt.Errorf("chmod: %w", err)
			}
		}

		return lis, nil
	}

	// if address is ipv4 we should pass "tcp4" net to listen only on ipv4 addresses

	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return net.Listen("tcp", addr)
	}

	ipAddr, err := netip.ParseAddr(host)
	if err != nil {
		return net.Listen("tcp", addr)
	}

	if ipAddr.Is4() {
		return net.Listen("tcp4", addr)
	}

	return net.Listen("tcp", addr)
}

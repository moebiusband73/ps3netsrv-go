// Package s3root implements read-only [pkgfs.SystemRoot] backed by an S3-compatible object store.
//
// Directories are emulated using "/"-delimited key prefixes. Reads are served through a shared page cache
// with per-file sequential readahead because consoles issue many small reads and object store latency is high.
package s3root

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
)

const URLScheme = "s3"

var (
	// ErrReadOnly returned for modifying operations.
	ErrReadOnly = fmt.Errorf("s3 storage is read-only: %w", fs.ErrPermission)

	// ErrObjectChanged returned if object was replaced while it was open.
	ErrObjectChanged = errors.New("object changed during read")
)

// Config contains S3 root parameters. Zero values are replaced with defaults.
type Config struct {
	Endpoint     string // host[:port], without scheme
	Region       string
	Secure       bool // use https
	BucketLookup minio.BucketLookupType

	Bucket string
	Prefix string // key prefix (directory inside a bucket), optional

	// Creds used for authentication, see [DefaultCredentials].
	Creds *credentials.Credentials

	// Transport is a base http transport, mostly for tests. Default is cloned [http.DefaultTransport].
	Transport http.RoundTripper

	PageSize     int64 // cache unit and minimal request size
	CacheSize    int64 // total page cache size in bytes
	ReadaheadMin int64 // initial readahead window for sequential reads
	ReadaheadMax int64 // maximal readahead window

	MetadataTTL    time.Duration // TTL for existing objects and directory listings
	NegativeTTL    time.Duration // TTL for missing objects
	RequestTimeout time.Duration // timeout for response headers and for stalled response bodies

	MaxConcurrentRequests int

	Logger *slog.Logger
}

const (
	DefaultPageSize              = 64 << 10
	DefaultCacheSize             = 128 << 20
	DefaultReadaheadMin          = 512 << 10
	DefaultReadaheadMax          = 8 << 20
	DefaultMetadataTTL           = time.Minute
	DefaultNegativeTTL           = 30 * time.Second
	DefaultRequestTimeout        = 30 * time.Second
	DefaultMaxConcurrentRequests = 16
)

func (c *Config) setDefaults() {
	if c.PageSize <= 0 {
		c.PageSize = DefaultPageSize
	}
	if c.CacheSize <= 0 {
		c.CacheSize = DefaultCacheSize
	}
	if c.ReadaheadMin <= 0 {
		c.ReadaheadMin = DefaultReadaheadMin
	}
	if c.ReadaheadMax <= 0 {
		c.ReadaheadMax = DefaultReadaheadMax
	}
	c.ReadaheadMin = max(c.ReadaheadMin, c.PageSize)
	c.ReadaheadMax = max(c.ReadaheadMax, c.ReadaheadMin)
	if c.MetadataTTL <= 0 {
		c.MetadataTTL = DefaultMetadataTTL
	}
	if c.NegativeTTL <= 0 {
		c.NegativeTTL = DefaultNegativeTTL
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.MaxConcurrentRequests <= 0 {
		c.MaxConcurrentRequests = DefaultMaxConcurrentRequests
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Creds == nil {
		c.Creds = DefaultCredentials()
	}
}

// ParseURL parses "s3://bucket/prefix" into bucket and prefix.
func ParseURL(rawURL string) (bucket, prefix string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}

	if u.Scheme != URLScheme {
		return "", "", fmt.Errorf("unexpected scheme %q", u.Scheme)
	}

	if u.Host == "" {
		return "", "", fmt.Errorf("bucket is not specified")
	}

	return u.Host, strings.Trim(u.Path, "/"), nil
}

// IsURL reports whether provided root is an S3 URL.
func IsURL(root string) bool {
	return strings.HasPrefix(root, URLScheme+"://")
}

// DefaultCredentials returns chain: AWS env, MinIO env, AWS credentials file, MinIO client config, IAM.
func DefaultCredentials() *credentials.Credentials {
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.EnvMinio{},
		&credentials.FileAWSCredentials{},
		&credentials.FileMinioClient{},
		&credentials.IAM{},
	})
}

// Root is a read-only [pkgfs.SystemRoot] for S3 bucket.
type Root struct {
	cfg    Config
	client *minio.Client
	core   minio.Core

	bucket string
	prefix string // empty or ends with "/"

	meta  *metaCache
	pages *pageCache
	reqs  chan struct{} // limits concurrent GET requests

	ctx    context.Context
	cancel context.CancelFunc
}

var _ pkgfs.SystemRoot = (*Root)(nil)

func New(cfg Config) (*Root, error) {
	cfg.setDefaults()

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("bucket is not specified")
	}

	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = cfg.RequestTimeout
		t.MaxIdleConnsPerHost = cfg.MaxConcurrentRequests
		transport = t
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        cfg.Creds,
		Secure:       cfg.Secure,
		Transport:    transport,
		Region:       cfg.Region,
		BucketLookup: cfg.BucketLookup,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Root{
		cfg:    cfg,
		client: client,
		core:   minio.Core{Client: client},
		bucket: cfg.Bucket,
		prefix: prefix,
		meta:   newMetaCache(cfg.MetadataTTL, cfg.NegativeTTL),
		pages:  newPageCache(cfg.CacheSize),
		reqs:   make(chan struct{}, cfg.MaxConcurrentRequests),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Check verifies that bucket exists and is accessible.
func (r *Root) Check(ctx context.Context) error {
	ok, err := r.client.BucketExists(ctx, r.bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", r.bucket, mapError(err))
	}
	if !ok {
		return fmt.Errorf("bucket %q: %w", r.bucket, fs.ErrNotExist)
	}

	// listing permission is required to serve directories
	for obj := range r.client.ListObjectsIter(ctx, r.bucket, minio.ListObjectsOptions{Prefix: r.prefix, MaxKeys: 1}) {
		if obj.Err != nil {
			return fmt.Errorf("list bucket %q: %w", r.bucket, mapError(obj.Err))
		}
		break
	}

	return nil
}

// Close cancels all in-flight requests.
func (r *Root) Close() error {
	r.cancel()
	return nil
}

func (r *Root) String() string {
	return URLScheme + "://" + r.bucket + "/" + r.prefix
}

// key converts root-relative path to object key (without trailing slash).
// Leading "/" before cleaning makes impossible to escape the prefix with "..".
func (r *Root) key(name string) (string, error) {
	if runtime.GOOS == "windows" && (filepath.IsAbs(name) || filepath.VolumeName(name) != "") {
		return "", fs.ErrNotExist
	}

	rel := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(name)), "/")
	if rel == "" {
		return r.rootKey(), nil
	}

	return r.prefix + rel, nil
}

// rootKey is a key of the root "directory": prefix without trailing slash.
func (r *Root) rootKey() string {
	return strings.TrimSuffix(r.prefix, "/")
}

func (r *Root) isRootKey(key string) bool {
	return key == r.rootKey()
}

// dirPrefix returns listing prefix for directory key.
func (r *Root) dirPrefix(key string) string {
	if r.isRootKey(key) {
		return r.prefix
	}

	return key + "/"
}

func (r *Root) Open(name string) (handler.File, error) {
	info, err := r.stat(r.ctx, name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if info.isDir {
		return newDir(r, name, info), nil
	}

	return newFile(r, name, info), nil
}

func (r *Root) Stat(name string) (fs.FileInfo, error) {
	info, err := r.stat(r.ctx, name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}

	return info.fileInfo(), nil
}

func (r *Root) Create(name string) (handler.WritableFile, error) {
	return nil, &fs.PathError{Op: "create", Path: name, Err: ErrReadOnly}
}

func (r *Root) Remove(name string) error {
	return &fs.PathError{Op: "remove", Path: name, Err: ErrReadOnly}
}

func (r *Root) Mkdir(name string, _ os.FileMode) error {
	return &fs.PathError{Op: "mkdir", Path: name, Err: ErrReadOnly}
}

// stat resolves object or "directory" info using metadata cache.
func (r *Root) stat(ctx context.Context, name string) (objInfo, error) {
	key, err := r.key(name)
	if err != nil {
		return objInfo{}, err
	}

	if r.isRootKey(key) {
		return objInfo{key: key, name: ".", isDir: true}, nil
	}

	if info, found, exists := r.meta.get(key, r.parentPrefix(key)); found {
		if !exists {
			return objInfo{}, fs.ErrNotExist
		}
		return info, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	obj, err := r.client.StatObject(ctx, r.bucket, key, minio.StatObjectOptions{})
	switch {
	case err == nil:
		info := objInfoFromObject(key, obj)
		r.meta.put(key, info)
		return info, nil
	case !errors.Is(mapError(err), fs.ErrNotExist):
		return objInfo{}, mapError(err)
	}

	// no object with such key, maybe it's a "directory"
	for obj := range r.client.ListObjectsIter(ctx, r.bucket, minio.ListObjectsOptions{Prefix: key + "/", MaxKeys: 1}) {
		if obj.Err != nil {
			return objInfo{}, mapError(obj.Err)
		}

		info := objInfo{key: key, name: path.Base(key), isDir: true}
		if obj.Key == key+"/" { // directory marker
			info.modTime = obj.LastModified
		}
		r.meta.put(key, info)
		return info, nil
	}

	r.meta.putMissing(key)
	return objInfo{}, fs.ErrNotExist
}

// parentPrefix returns listing prefix of key's parent directory.
func (r *Root) parentPrefix(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i+1]
	}

	return ""
}

// acquire limits amount of concurrent requests.
func (r *Root) acquire(ctx context.Context) error {
	select {
	case r.reqs <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Root) release() {
	<-r.reqs
}

// mapError converts S3 errors to [io/fs] errors.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	resp := minio.ToErrorResponse(err)
	switch {
	case resp.Code == "NoSuchKey", resp.Code == "NoSuchBucket", resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	case resp.Code == "AccessDenied", resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", fs.ErrPermission, err)
	case resp.Code == "InvalidObjectState":
		return fmt.Errorf("object is archived and must be restored first: %w: %w", fs.ErrPermission, err)
	case resp.Code == "PreconditionFailed", resp.StatusCode == http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %w", ErrObjectChanged, err)
	default:
		return err
	}
}

// objInfo is a cached object or directory metadata.
type objInfo struct {
	key     string
	name    string
	size    int64
	modTime time.Time
	etag    string
	isDir   bool
}

func objInfoFromObject(key string, obj minio.ObjectInfo) objInfo {
	return objInfo{
		key:     key,
		name:    path.Base(key),
		size:    obj.Size,
		modTime: obj.LastModified,
		etag:    strings.Trim(obj.ETag, `"`),
	}
}

func (i objInfo) fileInfo() fs.FileInfo {
	return fileInfo{i}
}

type fileInfo struct {
	info objInfo
}

func (fi fileInfo) Name() string       { return fi.info.name }
func (fi fileInfo) Size() int64        { return fi.info.size }
func (fi fileInfo) ModTime() time.Time { return fi.info.modTime }
func (fi fileInfo) IsDir() bool        { return fi.info.isDir }
func (fi fileInfo) Sys() any           { return nil }

func (fi fileInfo) Mode() fs.FileMode {
	if fi.info.isDir {
		return fs.ModeDir | 0o555
	}

	return 0o444
}

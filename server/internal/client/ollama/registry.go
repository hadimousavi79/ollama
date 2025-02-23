// Package ollama provides a client for interacting with an Ollama registry
// which pushes and pulls model manifests and layers as defined by the
// [ollama.com/manifest].
package ollama

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	"github.com/ollama/ollama/server/internal/cache/blob"
	"github.com/ollama/ollama/server/internal/chunks"
	"github.com/ollama/ollama/server/internal/internal/backoff"
	"github.com/ollama/ollama/server/internal/internal/names"
	"github.com/ollama/ollama/server/internal/internal/syncs"

	_ "embed"
)

// Errors
var (
	// ErrManifestNotFound is returned when a manifest is not found in the
	// cache or registry.
	ErrManifestNotFound = errors.New("manifest not found")

	// ErrManifestInvalid is returned when a manifest found in a local or
	// remote cache is invalid.
	ErrManifestInvalid = errors.New("invalid manifest")

	// ErrMissingModel is returned when the model part of a name is missing
	// or invalid.
	ErrNameInvalid = errors.New("invalid name; must be in the form {scheme://}{host/}{namespace/}[model]{:tag}{@digest}")

	// ErrCached is passed to [Trace.PushUpdate] when a layer already
	// exists. It is a non-fatal error and is never returned by [Registry.Push].
	ErrCached = errors.New("cached")
)

// Defaults
const (
	// DefaultChunkingThreshold is the threshold at which a layer should be
	// split up into chunks when downloading.
	DefaultChunkingThreshold = 128 << 20

	// DefaultMaxChunkSize is the default maximum size of a chunk to
	// download. It is configured based on benchmarks and aims to strike a
	// balance between download speed and memory usage.
	DefaultMaxChunkSize = 8 << 20
)

// DefaultCache returns a new disk cache for storing models. If the
// OLLAMA_MODELS environment variable is set, it uses that directory;
// otherwise, it uses $HOME/.ollama/models.
func DefaultCache() (*blob.DiskCache, error) {
	dir := os.Getenv("OLLAMA_MODELS")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".ollama", "models")
	}
	return blob.Open(dir)
}

// RegistryError is the standard error returned by Ollama APIs.
type RegistryError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RegistryError) Error() string {
	return fmt.Sprintf("registry responded with status %d: %s %s", e.Status, e.Code, e.Message)
}

func (e *RegistryError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Attr{Key: "status", Value: slog.IntValue(e.Status)},
		slog.Attr{Key: "code", Value: slog.StringValue(e.Code)},
		slog.Attr{Key: "message", Value: slog.StringValue(e.Message)},
	)
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *RegistryError) UnmarshalJSON(b []byte) error {
	type E RegistryError
	var v struct{ Errors []E }
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if len(v.Errors) == 0 {
		return fmt.Errorf("no messages in error response: %s", string(b))
	}
	*e = RegistryError(v.Errors[0]) // our registry only returns one error.
	return nil
}

// TODO(bmizerany): make configurable on [Registry]
var defaultName = func() names.Name {
	n := names.Parse("ollama.com/library/_:latest")
	if !n.IsFullyQualified() {
		panic("default name is not fully qualified")
	}
	return n
}()

// Registry is a client for performing push and pull operations against an
// Ollama registry.
type Registry struct {
	// UserAgent is the User-Agent header to send with requests to the
	// registry. If empty, the User-Agent is determined by HTTPClient.
	UserAgent string

	// Key is the key used to authenticate with the registry.
	//
	// Currently, only Ed25519 keys are supported.
	Key crypto.PrivateKey

	// HTTPClient is the HTTP client used to make requests to the registry.
	//
	// If nil, [http.DefaultClient] is used.
	//
	// As a quick note: If a Registry function that makes a call to a URL
	// with the "https+insecure" scheme, the client will be cloned and the
	// transport will be set to skip TLS verification, unless the client's
	// Transport done not have a Clone method with the same signature as
	// [http.Transport.Clone], which case, the call will fail.
	HTTPClient *http.Client

	// MaxStreams is the maximum number of concurrent streams to use when
	// pushing or pulling models. If zero, the number of streams is
	// determined by [runtime.GOMAXPROCS].
	//
	// Clients that want "unlimited" streams should set this to a large
	// number.
	MaxStreams int

	// ChunkingThreshold is the maximum size of a layer to download in a single
	// request. If zero, [DefaultChunkingThreshold] is used.
	ChunkingThreshold int64

	// MaxChunkSize is the maximum size of a chunk to download. If zero,
	// the default is [DefaultMaxChunkSize].
	//
	// It is only used when a layer is larger than [MaxChunkingThreshold].
	MaxChunkSize int64
}

// RegistryFromEnv returns a new Registry configured from the environment. The
// key is read from $HOME/.ollama/id_ed25519, MaxStreams is set to the
// value of OLLAMA_REGISTRY_MAXSTREAMS, and ChunkingDirectory is set to the
// system's temporary directory.
//
// It returns an error if any configuration in the environment is invalid.
func RegistryFromEnv() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(home, ".ollama/id_ed25519"))
	if err != nil {
		return nil, err
	}

	var rc Registry
	rc.Key, err = ssh.ParseRawPrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	maxStreams := os.Getenv("OLLAMA_REGISTRY_MAXSTREAMS")
	if maxStreams != "" {
		var err error
		rc.MaxStreams, err = strconv.Atoi(maxStreams)
		if err != nil {
			return nil, fmt.Errorf("invalid OLLAMA_REGISTRY_MAXSTREAMS: %w", err)
		}
	}
	return &rc, nil
}

type PushParams struct {
	// From is an optional destination name for the model. If empty, the
	// destination name is the same as the source name.
	From string
}

// parseName parses name using [names.ParseExtended] and then merges the name with the
// default name, and checks that the name is fully qualified. If a digest is
// present, it parse and returns it with the other fields as their zero values.
//
// It returns an error if the name is not fully qualified, or if the digest, if
// any, is invalid.
//
// The scheme is returned as provided by [names.ParseExtended].
func parseName(s string) (scheme string, n names.Name, d blob.Digest, err error) {
	scheme, n, ds := names.ParseExtended(s)
	n = names.Merge(n, defaultName)
	if ds != "" {
		// Digest is present. Validate it.
		d, err = blob.ParseDigest(ds)
		if err != nil {
			return "", names.Name{}, blob.Digest{}, err
		}
	}

	// The name check is deferred until after the digest check because we
	// say that digests take precedence over names, and so should there
	// errors when being parsed.
	if !n.IsFullyQualified() {
		return "", names.Name{}, blob.Digest{}, ErrNameInvalid
	}

	scheme = cmp.Or(scheme, "https")
	return scheme, n, d, nil
}

func (r *Registry) maxStreams() int {
	n := cmp.Or(r.MaxStreams, runtime.GOMAXPROCS(0))

	// Large downloads require a writter stream, so ensure we have at least
	// two streams to avoid a deadlock.
	return max(n, 2)
}

func (r *Registry) maxChunkingThreshold() int64 {
	return cmp.Or(r.ChunkingThreshold, DefaultChunkingThreshold)
}

// chunkSizeFor returns the chunk size for a layer of the given size. If the
// size is less than or equal to the max chunking threshold, the size is
// returned; otherwise, the max chunk size is returned.
func (r *Registry) maxChunkSize() int64 {
	return cmp.Or(r.MaxChunkSize, DefaultMaxChunkSize)
}

// Push pushes the model with the name in the cache to the remote registry.
func (r *Registry) Push(ctx context.Context, c *blob.DiskCache, name string, p *PushParams) error {
	if p == nil {
		p = &PushParams{}
	}

	m, err := ResolveLocal(c, cmp.Or(p.From, name))
	if err != nil {
		return err
	}

	// Before much else happens, check layers at not null, the blobs exist,
	// and the sizes match. This prevents long uploads followed by
	// disappointment.
	for _, l := range m.Layers {
		if l == nil {
			return fmt.Errorf("%w: null layer", ErrManifestInvalid)
		}
		info, err := c.Get(l.Digest)
		if err != nil {
			return fmt.Errorf("error getting %s: %w", l.Digest.Short(), err)
		}
		if info.Size != l.Size {
			return fmt.Errorf("size mismatch for %s: %d != %d", l.Digest.Short(), info.Size, l.Size)
		}
	}

	t := traceFromContext(ctx)

	scheme, n, _, err := parseName(name)
	if err != nil {
		// This should never happen since ResolveLocal should have
		// already validated the name.
		panic(err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var g errgroup.Group
	g.SetLimit(r.maxStreams())
	for _, l := range m.Layers {
		var progress atomic.Int64
		g.Go(func() (err error) {
			defer func() { t.update(l, progress.Load(), err) }()

			t.update(l, 0, nil)

			startURL := fmt.Sprintf("%s://%s/v2/%s/%s/blobs/uploads/?digest=%s",
				scheme,
				n.Host(),
				n.Namespace(),
				n.Model(),
				l.Digest,
			)
			res, err := r.doOK(ctx, "POST", startURL, nil)
			if err != nil {
				return err
			}
			res.Body.Close()

			f, err := os.Open(c.GetFile(l.Digest))
			if err != nil {
				return err
			}
			defer f.Close()

			uploadURL := res.Header.Get("Location")
			if uploadURL == "" {
				t.update(l, l.Size, ErrCached)
				return nil
			}

			req, err := r.newRequest(ctx, "PUT", uploadURL, f)
			if err != nil {
				return fmt.Errorf("invalid upload URL returned from registry: %q: %w", uploadURL, err)
			}
			req.ContentLength = l.Size

			res, err = doOK(r.client(), req)
			if err == nil {
				res.Body.Close()
			}
			return err
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// Commit
	path := fmt.Sprintf("%s://%s/v2/%s/%s/manifests/%s",
		scheme,
		n.Host(),
		n.Namespace(),
		n.Model(),
		n.Tag(),
	)
	res, err := r.doOK(ctx, "PUT", path, bytes.NewReader(m.Data))
	if err == nil {
		res.Body.Close()
	}
	// TODO(bmizerany): add a "commit" trace event
	return err
}

func canRetry(err error) bool {
	var re *RegistryError
	if !errors.As(err, &re) {
		return false
	}
	return re.Status >= 500
}

// Pull pulls the model with the given name from the remote registry into the
// cache.
//
// For layers larger then [Registry.MaxChunkSize], the layer is downloaded in
// chunks of the specified size, and then reassembled and verified. This is
// typically slower than splitting the model up across layers, and is mostly
// utilized for layers of type equal to "application/vnd.ollama.image".
func (r *Registry) Pull(ctx context.Context, c *blob.DiskCache, name string) error {
	scheme, n, _, err := parseName(name)
	if err != nil {
		return err
	}

	m, err := r.Resolve(ctx, name)
	if err != nil {
		return err
	}
	if len(m.Layers) == 0 {
		return fmt.Errorf("%w: no layers", ErrManifestInvalid)
	}

	exists := func(l *Layer) bool {
		info, err := c.Get(l.Digest)
		return err == nil && info.Size == l.Size
	}

	t := traceFromContext(ctx)

	var g errgroup.Group
	g.SetLimit(r.maxStreams())

	for _, l := range m.Layers {
		if exists(l) {
			t.update(l, l.Size, ErrCached)
			continue
		}

		blobURL := fmt.Sprintf("%s://%s/v2/%s/%s/blobs/%s", scheme, n.Host(), n.Namespace(), n.Model(), l.Digest)
		req, err := r.newRequest(ctx, "GET", blobURL, nil)
		if err != nil {
			t.update(l, 0, err)
			continue
		}

		t.update(l, 0, nil)

		if l.Size <= r.maxChunkingThreshold() {
			g.Go(func() error {
				res, err := doOK(r.client(), req)
				if err != nil {
					return err
				}
				err = c.Put(l.Digest, res.Body, l.Size)
				if err == nil {
					t.update(l, l.Size, nil)
				}
				return err
			})
		} else {
			q := syncs.NewRelayReader()

			g.Go(func() (err error) {
				defer func() { q.CloseWithError(err) }()
				return c.Put(l.Digest, q, l.Size)
			})

			var progress atomic.Int64

			// We want to avoid extra round trips per chunk due to
			// redirects from the registry to the blob store, so
			// fire an initial request to get the final URL and
			// then use that URL for the chunk requests.
			req.Header.Set("Range", "bytes=0-0")
			res, err := doOK(r.client(), req)
			if err != nil {
				return err
			}
			req = res.Request.WithContext(req.Context())

			streamNo := 0
			tws := make([]*bufio.Writer, r.maxStreams()-1)
			for chunk := range chunks.Of(l.Size, r.maxChunkSize()) {
				ticket := q.Take()
				bufIdx := streamNo % len(tws)
				streamNo++
				g.Go(func() (err error) {
					defer func() {
						if err != nil {
							q.CloseWithError(err)
						}
						ticket.Close()
						t.update(l, progress.Load(), err)
					}()

					for _, err := range backoff.Loop(ctx, 3*time.Second) {
						if err != nil {
							return err
						}

						err := func() error {
							req := req.Clone(req.Context())
							req.Header.Set("Range", fmt.Sprintf("bytes=%s", chunk))
							res, err := doOK(r.client(), req)
							if err != nil {
								return err
							}
							defer res.Body.Close()

							tw := tws[bufIdx]
							if tw == nil {
								tw = bufio.NewWriterSize(nil, int(r.maxChunkSize()))
								tws[bufIdx] = tw
							}
							tw.Reset(ticket)
							defer tw.Reset(nil) // release ticket

							_, err = io.CopyN(tw, res.Body, chunk.Size())
							if err != nil {
								return maybeUnexpectedEOF(err)
							}
							if err := tw.Flush(); err != nil {
								return err
							}

							total := progress.Add(chunk.Size())
							if total >= l.Size {
								q.Close()
							}
							return nil
						}()
						if !canRetry(err) {
							return err
						}
					}
					return nil
				})
			}
		}
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// store the manifest blob
	md := blob.DigestFromBytes(m.Data)
	if err := blob.PutBytes(c, md, m.Data); err != nil {
		return err
	}

	// commit the manifest with a link
	return c.Link(m.Name, md)
}

// Manifest represents a [ollama.com/manifest].
type Manifest struct {
	Name   string   `json:"-"` // the canonical name of the model
	Data   []byte   `json:"-"` // the raw data of the manifest
	Layers []*Layer `json:"layers"`
}

var emptyDigest, _ = blob.ParseDigest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// Layer returns the layer with the given
// digest, or nil if not found.
func (m *Manifest) Layer(d blob.Digest) *Layer {
	for _, l := range m.Layers {
		if l.Digest == d {
			return l
		}
	}
	return nil
}

// MarshalJSON implements json.Marshaler.
//
// NOTE: It adds an empty config object to the manifest, which is required by
// the registry, but not used by the client. In the future, the config object
// will not be required by the registry and this will should be removed.
func (m Manifest) MarshalJSON() ([]byte, error) {
	type M Manifest
	var v = struct {
		M

		// This is ignored, mostly, by the registry But, if not
		// present, it will cause an error to be returned during the
		// last phase of the commit which expects it, but does nothing
		// with it. This will be fixed in a future release of
		// ollama.com.
		Config *Layer `json:"config"`
	}{
		M:      M(m),
		Config: &Layer{Digest: emptyDigest},
	}
	return json.Marshal(v)
}

// unmarshalManifest unmarshals the data into a manifest, and sets the name
// field to the string representation of the name.
//
// It panics if the name is not fully qualified. Callers should ensure the name
// is fully qualified before calling this function.
func unmarshalManifest(n names.Name, data []byte) (*Manifest, error) {
	if !n.IsFullyQualified() {
		panic(fmt.Sprintf("unmarshalManifest: name is not fully qualified: %s", n.String()))
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m.Name = n.String()
	m.Data = data
	return &m, nil
}

// Layer is a layer in a model.
type Layer struct {
	Digest    blob.Digest `json:"digest"`
	MediaType string      `json:"mediaType"`
	Size      int64       `json:"size"`
}

// ResolveLocal resolves a name to a Manifest in the local cache. The name is
// parsed using [names.ParseExtended] but the scheme is ignored.
func ResolveLocal(c *blob.DiskCache, name string) (*Manifest, error) {
	_, n, d, err := parseName(name)
	if err != nil {
		return nil, err
	}
	if !d.IsValid() {
		d, err = c.Resolve(n.String())
		if err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(c.GetFile(d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrManifestNotFound, name)
		}
		return nil, err
	}
	m, err := unmarshalManifest(n, data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, errors.Join(ErrManifestInvalid, err))
	}
	return m, nil
}

// Resolve resolves a name to a Manifest in the remote registry.
func (r *Registry) Resolve(ctx context.Context, name string) (*Manifest, error) {
	scheme, n, d, err := parseName(name)
	if err != nil {
		return nil, err
	}

	manifestURL := fmt.Sprintf("%s://%s/v2/%s/%s/manifests/%s", scheme, n.Host(), n.Namespace(), n.Model(), n.Tag())
	if d.IsValid() {
		manifestURL = fmt.Sprintf("%s://%s/v2/%s/%s/blobs/%s", scheme, n.Host(), n.Namespace(), n.Model(), d)
	}

	res, err := r.doOK(ctx, "GET", manifestURL, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	// TODO(bmizerany): return digest here
	m, err := unmarshalManifest(n, data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, errors.Join(ErrManifestInvalid, err))
	}
	return m, nil
}

func (r *Registry) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

// newRequest constructs a new request, ready to use, with the given method,
// url, and body, presigned with client Key and UserAgent.
func (r *Registry) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	if r.Key != nil {
		token, err := makeAuthToken(r.Key)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// doOK makes a request with the given client and request, and returns the
// response if the status code is 200. If the status code is not 200, an Error
// is parsed from the response body and returned. If any other error occurs, it
// is returned.
func doOK(c *http.Client, r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https+insecure" {
		// TODO(bmizerany): clone client.Transport, set
		// InsecureSkipVerify, etc.

		type cloner interface {
			Clone() *http.Transport
		}

		// Attempt to configure the transport to skip TLS verification
		// if we can clone it, otherwise fall through and let the http
		// client complain and the scheme being invalid.
		x, ok := cmp.Or(c.Transport, http.DefaultTransport).(cloner)
		if ok {
			tr := x.Clone()
			tr.TLSClientConfig = cmp.Or(tr.TLSClientConfig, &tls.Config{})
			tr.TLSClientConfig.InsecureSkipVerify = true

			cc := *c // shallow copy
			cc.Transport = tr
			c = &cc

			r = r.Clone(r.Context())
			r.URL.Scheme = "https"

			// fall through
		}
	}

	res, err := c.Do(r)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		out, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		var re RegistryError
		if err := json.Unmarshal(out, &re); err != nil {
			// Use the raw body if we can't parse it as an error object.
			re.Message = string(out)
		}
		re.Status = res.StatusCode
		return nil, &re
	}
	return res, nil
}

// doOK is a convenience method for making a request with newRequest and
// passing it to doOK with r.client().
func (r *Registry) doOK(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := r.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return doOK(r.client(), req)
}

// makeAuthToken creates an Ollama auth token for the given private key.
//
// NOTE: This format is OLD, overly complex, and should be replaced. We're
// inheriting it from the original Ollama client and ollama.com
// implementations, so we need to support it for now.
func makeAuthToken(key crypto.PrivateKey) (string, error) {
	privKey, _ := key.(*ed25519.PrivateKey)
	if privKey == nil {
		return "", fmt.Errorf("unsupported private key type: %T", key)
	}

	url := fmt.Sprintf("https://ollama.com?ts=%d", time.Now().Unix())
	// Part 1: the checkData (e.g. the URL with a timestamp)

	// Part 2: the public key
	pubKeyShort, err := func() ([]byte, error) {
		sshPubKey, err := ssh.NewPublicKey(privKey.Public())
		if err != nil {
			return nil, err
		}
		pubKeyParts := bytes.Fields(ssh.MarshalAuthorizedKey(sshPubKey))
		if len(pubKeyParts) < 2 {
			return nil, fmt.Errorf("malformed public key: %q", pubKeyParts)
		}
		pubKeyShort := pubKeyParts[1]
		return pubKeyShort, nil
	}()
	if err != nil {
		return "", err
	}

	// Part 3: the signature
	sig := ed25519.Sign(*privKey, []byte(checkData(url)))

	// Assemble the token: <checkData>:<pubKey>:<signature>
	var b strings.Builder
	io.WriteString(&b, base64.StdEncoding.EncodeToString([]byte(url)))
	b.WriteByte(':')
	b.Write(pubKeyShort)
	b.WriteByte(':')
	io.WriteString(&b, base64.StdEncoding.EncodeToString(sig))

	return b.String(), nil
}

// The original spec for Ollama tokens was to use the SHA256 of the zero
// string as part of the signature. I'm not sure why that was, but we still
// need it to verify the signature.
var zeroSum = func() string {
	sha256sum := sha256.Sum256(nil)
	x := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(sha256sum[:])))
	return x
}()

// checkData takes a URL and creates the original string format of the
// data signature that is used by the ollama client to sign requests
func checkData(url string) string {
	return fmt.Sprintf("GET,%s,%s", url, zeroSum)
}

func maybeUnexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

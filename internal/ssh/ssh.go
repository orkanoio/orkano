// Package ssh is the minimal SSH transport Orkano uses to reach a node before
// the cluster exists: the install preflight probes a node over it, and orkano
// init later runs the k3s bootstrap over the same client. Authentication is
// public-key only and the host key must be pinned — there is deliberately no
// insecure-skip option, because this connection bootstraps the entire cluster
// and a man-in-the-middle on it is total compromise (the caller layers any
// first-contact host-key confirmation on top).
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// DefaultTimeout bounds the TCP dial and SSH handshake when Config.Timeout is
// unset.
const DefaultTimeout = 15 * time.Second

const defaultPort = "22"

// Result is the outcome of running one command on the node. A command that runs
// to completion with a non-zero exit is a Result with that ExitStatus, not a Go
// error: the caller decides what a non-zero exit means.
type Result struct {
	Stdout     string
	Stderr     string
	ExitStatus int
}

// Config configures a Client. PrivateKey and HostKey are required: there is no
// password auth and no host-key bypass.
type Config struct {
	// Addr is the node address as "host" or "host:port"; the port defaults to 22.
	Addr string
	User string
	// PrivateKey is a PEM-encoded private key for public-key auth (OpenSSH or
	// PKCS#8/PKCS#1 PEM). Passphrase-protected keys are not supported yet and
	// surface as a parse error from New.
	PrivateKey []byte
	// HostKey is the node's public key in authorized-keys format; the handshake
	// is pinned to it. A mismatch fails the connection.
	HostKey []byte
	// Timeout bounds the dial and handshake; DefaultTimeout when zero.
	Timeout time.Duration
}

// Client is a lazily-connected SSH client. It connects on the first Connect or
// Run and caches the connection for subsequent commands; it is safe for
// sequential use (the preflight runner probes one check at a time).
type Client struct {
	addr      string
	user      string
	auth      []gossh.AuthMethod
	hostKeyCB gossh.HostKeyCallback
	timeout   time.Duration

	mu   sync.Mutex
	conn *gossh.Client
}

// New validates the configuration and key material and returns a Client that
// has not yet connected. It errors on a missing field, an unparseable or
// passphrase-protected private key, or an unparseable host key.
func New(cfg Config) (*Client, error) {
	if cfg.User == "" {
		return nil, errors.New("ssh: user is required")
	}
	if cfg.Addr == "" {
		return nil, errors.New("ssh: addr is required")
	}
	if len(cfg.PrivateKey) == 0 {
		return nil, errors.New("ssh: private key is required")
	}
	signer, err := gossh.ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("ssh: parse private key: %w", err)
	}
	if len(cfg.HostKey) == 0 {
		return nil, errors.New("ssh: host key is required (host-key pinning is mandatory)")
	}
	hostPub, _, _, _, err := gossh.ParseAuthorizedKey(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("ssh: parse host key: %w", err)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		addr:      withDefaultPort(cfg.Addr),
		user:      cfg.User,
		auth:      []gossh.AuthMethod{gossh.PublicKeys(signer)},
		hostKeyCB: gossh.FixedHostKey(hostPub),
		timeout:   timeout,
	}, nil
}

func withDefaultPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, defaultPort)
	}
	return addr
}

// Connect establishes the connection if it is not already open. It is
// idempotent; Run calls it implicitly.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx)
}

func (c *Client) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	start := time.Now()
	d := net.Dialer{Timeout: c.timeout}
	netConn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("ssh: dial %s: %w", c.addr, err)
	}
	// The x/crypto handshake takes no context, so a deadline on the raw conn is
	// how the timeout (and any ctx deadline) is enforced. Base it on the instant
	// before the dial so dial+handshake share ONE Timeout budget, not two.
	deadline := start.Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = netConn.SetDeadline(deadline)

	cc, chans, reqs, err := gossh.NewClientConn(netConn, c.addr, &gossh.ClientConfig{
		User:            c.user,
		Auth:            c.auth,
		HostKeyCallback: c.hostKeyCB,
		// No Timeout here: that field only bounds gossh.Dial's own TCP dial,
		// which we do not use — the netConn deadline above is the sole bound.
	})
	if err != nil {
		_ = netConn.Close()
		return fmt.Errorf("ssh: handshake with %s: %w", c.addr, err)
	}
	_ = netConn.SetDeadline(time.Time{}) // clear; commands manage their own ctx
	c.conn = gossh.NewClient(cc, chans, reqs)
	return nil
}

// Run executes cmd on the node and returns its output and exit status. Only a
// failure to run the command at all (connection lost, no exit status reported)
// returns an error; a clean run with a non-zero exit returns a Result with that
// ExitStatus and a nil error. Run honours ctx: on cancellation the command is
// signalled, the session torn down, and ctx's error returned.
func (c *Client) Run(ctx context.Context, cmd string) (Result, error) {
	c.mu.Lock()
	if err := c.connectLocked(ctx); err != nil {
		c.mu.Unlock()
		return Result{}, err
	}
	conn := c.conn
	c.mu.Unlock()

	sess, err := conn.NewSession()
	if err != nil {
		// The cached connection is broken; drop it so the next Run reconnects
		// rather than failing forever on the same dead client.
		c.invalidate(conn)
		return Result{}, fmt.Errorf("ssh: new session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGKILL)
		_ = sess.Close() // unblock the goroutine's blocked Run
		<-done
		return Result{}, fmt.Errorf("ssh: run %q: %w", cmd, ctx.Err())
	case runErr := <-done:
		res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		if runErr == nil {
			return res, nil
		}
		var exitErr *gossh.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitStatus = exitErr.ExitStatus()
			return res, nil
		}
		// A non-exit error means the connection itself failed, not the command;
		// drop it so a later Run reconnects instead of wedging permanently.
		c.invalidate(conn)
		return Result{}, fmt.Errorf("ssh: run %q: %w", cmd, runErr)
	}
}

// invalidate drops the cached connection if it is still the one that failed, so
// the next Run reconnects. A command's own non-zero exit is NOT a transport
// failure and never reaches here; only a broken connection does.
func (c *Client) invalidate(broken *gossh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == broken {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// ScanHostKey opens a connection to addr and returns the host key it presents,
// in authorized-keys format, without authenticating. It exists for first
// contact with a fresh node: orkano init shows the key's fingerprint and trusts
// it only on explicit confirmation, then pins it via New's HostKey. The host key
// is delivered during the handshake, before authentication, so no credentials
// are needed — and capturing it here keeps New itself free of any insecure
// host-key bypass.
func ScanHostKey(ctx context.Context, addr string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	addr = withDefaultPort(addr)

	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	netConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	defer func() { _ = netConn.Close() }()

	deadline := start.Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = netConn.SetDeadline(deadline)

	var captured gossh.PublicKey
	// errHostKeyCaptured aborts the handshake the instant the host key arrives:
	// there is no point completing key exchange or attempting auth just to read
	// a value the server already sent.
	errHostKeyCaptured := errors.New("host key captured")
	cc, chans, reqs, err := gossh.NewClientConn(netConn, addr, &gossh.ClientConfig{
		User: "orkano-scan",
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			captured = key
			return errHostKeyCaptured
		},
	})
	if err == nil {
		// Unexpected: the server accepted us without presenting a key we rejected.
		_ = cc.Close()
		go gossh.DiscardRequests(reqs)
		go func() {
			for newCh := range chans {
				_ = newCh.Reject(gossh.Prohibited, "scan only")
			}
		}()
	}
	if captured == nil {
		return nil, fmt.Errorf("ssh: scan host key from %s: %w", addr, err)
	}
	return gossh.MarshalAuthorizedKey(captured), nil
}

// FingerprintSHA256 returns the SHA256 fingerprint of an authorized-keys host
// key (the "SHA256:…" form ssh prints), for showing a node's identity to a user
// before they trust it. It errors if the bytes do not parse as a public key.
func FingerprintSHA256(authorizedKey []byte) (string, error) {
	pub, _, _, _, err := gossh.ParseAuthorizedKey(authorizedKey)
	if err != nil {
		return "", fmt.Errorf("ssh: parse host key: %w", err)
	}
	return gossh.FingerprintSHA256(pub), nil
}

// Close closes the underlying connection if open. It is safe to call more than
// once.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

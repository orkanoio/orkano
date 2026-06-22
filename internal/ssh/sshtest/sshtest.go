// Package sshtest provides an in-process SSH server for tests, in the spirit of
// net/http/httptest: a real server speaking the SSH wire protocol, with command
// execution dispatched to a Go handler so a test can script a node's responses
// without a VM. It is the seam that lets the SSH transport and the preflight
// checks be exercised against genuine SSH end to end.
package sshtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// ExecHandler produces the result of running cmd on the fake node: its stdout,
// stderr, and exit status.
type ExecHandler func(cmd string) (stdout, stderr string, exitStatus int)

// Server is a running in-process SSH server. Build a client against it with
// ssh.Config{Addr: Addr, User: User, PrivateKey: ClientPrivateKey, HostKey:
// HostKeyAuthorized}.
type Server struct {
	// Addr is the listener address, host:port.
	Addr string
	// User is the username the server authorises.
	User string
	// HostKeyAuthorized is the server's host public key in authorized-keys
	// format — pin it as the client's HostKey.
	HostKeyAuthorized []byte
	// ClientPrivateKey is the PEM private key the client must present.
	ClientPrivateKey []byte

	handler ExecHandler

	ln      net.Listener
	cfg     *gossh.ServerConfig
	wg      sync.WaitGroup
	mu      sync.Mutex
	active  []net.Conn
	conns   int
	closing bool
}

// Option configures a Server.
type Option func(*Server)

// WithUser sets the username the server authorises (default "orkano").
func WithUser(u string) Option { return func(s *Server) { s.User = u } }

// New starts a server dispatching exec requests to handler. It panics on setup
// failure (a test helper, like httptest.NewServer). Close it when done.
func New(handler ExecHandler, opts ...Option) *Server {
	s := &Server{User: "orkano", handler: handler}
	for _, o := range opts {
		o(s)
	}

	hostSigner := mustSigner()
	s.HostKeyAuthorized = gossh.MarshalAuthorizedKey(hostSigner.PublicKey())

	clientPEM, clientAuthorized := GenerateKey()
	s.ClientPrivateKey = clientPEM
	authorizedClient, _, _, _, err := gossh.ParseAuthorizedKey(clientAuthorized)
	if err != nil {
		panic("sshtest: parse client key: " + err.Error())
	}

	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(c gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if c.User() == s.User && bytes.Equal(key.Marshal(), authorizedClient.Marshal()) {
				return &gossh.Permissions{}, nil
			}
			return nil, errors.New("sshtest: unauthorized")
		},
	}
	cfg.AddHostKey(hostSigner)
	s.cfg = cfg

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		panic("sshtest: listen: " + err.Error())
	}
	s.ln = ln
	s.Addr = ln.Addr().String()

	s.wg.Add(1)
	go s.serve()
	return s
}

// GenerateKey returns a fresh ed25519 keypair as (PKCS#8 PEM private key,
// authorized-keys public key) — useful for negative tests (a key the server
// does not authorise, or a host key it does not present).
func GenerateKey() (privatePEM, authorizedPublic []byte) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("sshtest: generate key: " + err.Error())
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic("sshtest: marshal key: " + err.Error())
	}
	privatePEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		panic("sshtest: ssh public key: " + err.Error())
	}
	return privatePEM, gossh.MarshalAuthorizedKey(sshPub)
}

func mustSigner() gossh.Signer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("sshtest: generate host key: " + err.Error())
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		panic("sshtest: host signer: " + err.Error())
	}
	return signer
}

// Connections reports how many TCP connections the server has accepted — a test
// uses it to prove the client caches one connection across commands.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// Close stops the server and closes every accepted connection.
func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	conns := s.active
	s.active = nil
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, nc := range conns {
		_ = nc.Close()
	}
	s.wg.Wait()
}

// DropConns closes every currently-accepted connection but leaves the listener
// open, simulating a transport drop so a test can exercise client reconnect.
func (s *Server) DropConns() {
	s.mu.Lock()
	conns := s.active
	s.active = nil
	s.mu.Unlock()
	for _, nc := range conns {
		_ = nc.Close()
	}
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns++
		s.active = append(s.active, nc)
		s.mu.Unlock()

		s.wg.Add(1)
		go s.handleConn(nc)
	}
}

func (s *Server) handleConn(nc net.Conn) {
	defer s.wg.Done()
	defer func() { _ = nc.Close() }()

	_, chans, reqs, err := gossh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	// Track the spawned goroutines so Close's wg.Wait is a true happens-before
	// over everything a connection started (the Add runs while handleConn is
	// still counted, so the counter never momentarily hits zero).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		gossh.DiscardRequests(reqs)
	}()

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(gossh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleSession(ch, requests)
		}()
	}
}

func (s *Server) handleSession(ch gossh.Channel, requests <-chan *gossh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			return
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}
		stdout, stderr, code := s.handler(payload.Command)
		_, _ = io.WriteString(ch, stdout)
		_, _ = io.WriteString(ch.Stderr(), stderr)
		sendExitStatus(ch, code)
		return
	}
}

func sendExitStatus(ch gossh.Channel, code int) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code)) //nolint:gosec // exit codes are small non-negative ints
	_, _ = ch.SendRequest("exit-status", false, b[:])
}

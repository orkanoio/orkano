package ssh_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	orkssh "github.com/orkanoio/orkano/internal/ssh"
	"github.com/orkanoio/orkano/internal/ssh/sshtest"
)

func TestScanHostKeyReturnsPresentedKey(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	got, err := orkssh.ScanHostKey(context.Background(), srv.Addr, 5*time.Second)
	if err != nil {
		t.Fatalf("ScanHostKey: %v", err)
	}
	if !bytes.Equal(got, srv.HostKeyAuthorized) {
		t.Errorf("scanned key\n %q\nwant\n %q", got, srv.HostKeyAuthorized)
	}

	// The scanned key is pinnable: New must accept it as the HostKey and connect.
	c, err := orkssh.New(orkssh.Config{Addr: srv.Addr, User: srv.User, PrivateKey: srv.ClientPrivateKey, HostKey: got})
	if err != nil {
		t.Fatalf("New with scanned host key: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Errorf("Connect with scanned host key: %v", err)
	}
}

func TestScanHostKeyUnreachable(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	addr := srv.Addr
	srv.Close() // address now refused

	if _, err := orkssh.ScanHostKey(context.Background(), addr, time.Second); err == nil {
		t.Fatal("want an error scanning a closed address")
	}
}

func TestFingerprintSHA256(t *testing.T) {
	srv := sshtest.New(func(string) (string, string, int) { return "", "", 0 })
	defer srv.Close()

	fp, err := orkssh.FingerprintSHA256(srv.HostKeyAuthorized)
	if err != nil {
		t.Fatalf("FingerprintSHA256: %v", err)
	}
	if len(fp) < len("SHA256:") || fp[:7] != "SHA256:" {
		t.Errorf("fingerprint %q is not in SHA256: form", fp)
	}

	if _, err := orkssh.FingerprintSHA256([]byte("not a key")); err == nil {
		t.Error("want an error for non-key bytes")
	}
}

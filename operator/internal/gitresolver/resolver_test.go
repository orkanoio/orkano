package gitresolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func publicLookup(_ context.Context, host string) ([]net.IPAddr, error) {
	if host == "" {
		return nil, errors.New("empty host")
	}
	return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
}

func unusedDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unexpected dial")
}

func resolverWithRoundTripper(lookup lookupIPFunc, roundTripper http.RoundTripper) *Resolver {
	r := newResolver(lookup, unusedDial)
	r.client.Transport = roundTripper
	return r
}

func response(status int, contentType string, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       body,
	}
}

func advertisement(lines ...string) string {
	var out strings.Builder
	out.WriteString(packet("# service=git-upload-pack\n"))
	out.WriteString("0000")
	for _, line := range lines {
		out.WriteString(packet(line))
	}
	out.WriteString("0000")
	return out.String()
}

func packet(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

func TestResolveCommit(t *testing.T) {
	advertised := advertisement(
		shaA+" HEAD\x00multi_ack symref=HEAD:refs/heads/main\n",
		shaB+" refs/heads/main\n",
		shaC+" refs/heads/release/v1\n",
		shaA+" refs/tags/v1.0.0\n",
		shaB+" refs/tags/v2.0.0\n",
		shaC+" refs/tags/v2.0.0^{}\n",
		shaA+" refs/heads/shared\n",
		shaB+" refs/tags/shared\n",
	)
	r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got, want := req.URL.String(), "https://git.example/team/repo.git/info/refs?service=git-upload-pack"; got != want {
			t.Fatalf("request URL = %q, want %q", got, want)
		}
		if got := req.Header.Get("Accept"); got != advertisementMediaType {
			t.Fatalf("Accept = %q, want %q", got, advertisementMediaType)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty for anonymous Git", got)
		}
		return response(http.StatusOK, advertisementMediaType+"; charset=utf-8", io.NopCloser(strings.NewReader(advertised))), nil
	}))

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "empty means HEAD", want: shaA},
		{name: "explicit HEAD", ref: "HEAD", want: shaA},
		{name: "short branch", ref: "main", want: shaB},
		{name: "full branch", ref: "refs/heads/release/v1", want: shaC},
		{name: "lightweight tag", ref: "refs/tags/v1.0.0", want: shaA},
		{name: "annotated tag prefers peeled object", ref: "v2.0.0", want: shaC},
		{name: "branch wins an ambiguous short name", ref: "shared", want: shaA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ResolveCommit(context.Background(), "https://git.example/team/repo.git/", tc.ref)
			if err != nil {
				t.Fatalf("ResolveCommit: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SHA = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveCommitRejectsRepositoryURL(t *testing.T) {
	tests := []string{
		"",
		" https://git.example/repo.git",
		"https://git.example/repo.git\n",
		"http://git.example/repo.git",
		"ssh://git.example/repo.git",
		"https:///repo.git",
		"https://user@git.example/repo.git",
		"https://user:secret@git.example/repo.git",
		"https://git.example:8443/repo.git",
		"https://git.example/repo.git?token=secret",
		"https://git.example/repo.git#main",
		"https://127.0.0.1/repo.git",
		"https://[::1]/repo.git",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			called := false
			r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("unexpected request")
			}))
			_, err := r.ResolveCommit(context.Background(), raw, "main")
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
			if called {
				t.Fatal("invalid URL reached the HTTP transport")
			}
		})
	}
}

func TestResolveCommitRejectsNonPublicDNSAnswers(t *testing.T) {
	tests := []struct {
		name string
		ips  []string
	}{
		{name: "unspecified IPv4", ips: []string{"0.0.0.0"}},
		{name: "private IPv4", ips: []string{"10.0.0.1"}},
		{name: "loopback IPv4", ips: []string{"127.0.0.1"}},
		{name: "link-local IPv4", ips: []string{"169.254.2.3"}},
		{name: "carrier-grade NAT", ips: []string{"100.64.0.1"}},
		{name: "benchmark network", ips: []string{"198.18.0.1"}},
		{name: "multicast IPv4", ips: []string{"224.0.0.1"}},
		{name: "private IPv6", ips: []string{"fd00::1"}},
		{name: "loopback IPv6", ips: []string{"::1"}},
		{name: "link-local IPv6", ips: []string{"fe80::1"}},
		{name: "multicast IPv6", ips: []string{"ff02::1"}},
		{name: "mixed public and private", ips: []string{"8.8.8.8", "10.0.0.1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(context.Context, string) ([]net.IPAddr, error) {
				addresses := make([]net.IPAddr, 0, len(tc.ips))
				for _, ip := range tc.ips {
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP(ip)})
				}
				return addresses, nil
			}
			called := false
			r := resolverWithRoundTripper(lookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("unexpected request")
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
			if called {
				t.Fatal("non-public DNS answer reached the HTTP transport")
			}
		})
	}
}

func TestResolveCommitDNSFailureIsTransient(t *testing.T) {
	dnsErr := errors.New("temporary DNS failure")
	r := resolverWithRoundTripper(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, dnsErr
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected request")
	}))
	_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
	if !errors.Is(err, dnsErr) {
		t.Fatalf("error = %v, want wrapped DNS error", err)
	}
	if errors.Is(err, ErrUnresolvable) {
		t.Fatalf("DNS failure must remain transient: %v", err)
	}
}

func TestResolveCommitHTTPStatusClassification(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(fmt.Sprintf("%d is permanent", status), func(t *testing.T) {
			r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(status, "text/plain", io.NopCloser(strings.NewReader("no"))), nil
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
		})
	}
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprintf("%d is transient", status), func(t *testing.T) {
			r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(status, "text/plain", io.NopCloser(strings.NewReader("retry"))), nil
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
			if err == nil || errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want transient error", err)
			}
		})
	}
}

func TestResolveCommitTransportAndReadFailuresAreTransient(t *testing.T) {
	transportErr := errors.New("connection reset")
	r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	}))
	_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
	if !errors.Is(err, transportErr) || errors.Is(err, ErrUnresolvable) {
		t.Fatalf("transport error = %v, want transient wrapped error", err)
	}

	readErr := errors.New("body read failed")
	r = resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, advertisementMediaType, readCloser{Reader: errorReader{err: readErr}}), nil
	}))
	_, err = r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
	if !errors.Is(err, readErr) || errors.Is(err, ErrUnresolvable) {
		t.Fatalf("read error = %v, want transient wrapped error", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type readCloser struct{ io.Reader }

func (readCloser) Close() error { return nil }

func TestResolveCommitRejectsMalformedRefBeforeRequest(t *testing.T) {
	tests := []string{
		strings.Repeat("a", maxRefLength+1),
		"refs/remotes/origin/main",
		"../main",
		"main..old",
		"main lock",
		"main~1",
		"main^{}",
		"main:other",
		"main?query",
		"main*glob",
		"main[0]",
		"main\\branch",
		"main@{yesterday}",
		"refs/heads/.hidden",
		"refs/tags/release.lock",
		"refs/heads/trailing.",
	}
	for _, ref := range tests {
		t.Run(ref, func(t *testing.T) {
			called := false
			r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("unexpected request")
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", ref)
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
			if called {
				t.Fatal("malformed ref reached the HTTP transport")
			}
		})
	}
}

func TestResolveCommitRejectsMalformedAdvertisement(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "missing content type", body: advertisement(shaA + " HEAD\n")},
		{name: "wrong content type", contentType: "text/html", body: advertisement(shaA + " HEAD\n")},
		{name: "wrong service", contentType: advertisementMediaType, body: packet("# service=git-receive-pack\n") + "0000"},
		{name: "preamble not flushed", contentType: advertisementMediaType, body: packet("# service=git-upload-pack\n") + packet(shaA+" HEAD\n") + "0000"},
		{name: "bad packet length", contentType: advertisementMediaType, body: packet("# service=git-upload-pack\n") + "0000zzzz"},
		{name: "packet runs past body", contentType: advertisementMediaType, body: packet("# service=git-upload-pack\n") + "00000020short"},
		{name: "uppercase object id", contentType: advertisementMediaType, body: advertisement(strings.ToUpper(shaA) + " HEAD\n")},
		{name: "short object id", contentType: advertisementMediaType, body: advertisement("abc HEAD\n")},
		{name: "missing ref name", contentType: advertisementMediaType, body: advertisement(shaA + " \n")},
		{name: "unterminated ref line", contentType: advertisementMediaType, body: packet("# service=git-upload-pack\n") + "0000" + packet(shaA+" HEAD") + "0000"},
		{name: "missing final flush", contentType: advertisementMediaType, body: packet("# service=git-upload-pack\n") + "0000" + packet(shaA+" HEAD\n")},
		{name: "bytes after final flush", contentType: advertisementMediaType, body: advertisement(shaA+" HEAD\n") + "junk"},
		{name: "conflicting duplicate", contentType: advertisementMediaType, body: advertisement(shaA+" HEAD\n", shaB+" HEAD\n")},
		{name: "target absent", contentType: advertisementMediaType, body: advertisement(shaA + " refs/heads/other\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tc.contentType, io.NopCloser(strings.NewReader(tc.body))), nil
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
		})
	}
}

func TestResolveCommitBoundsAdvertisement(t *testing.T) {
	r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, advertisementMediaType, io.NopCloser(strings.NewReader(strings.Repeat("x", maxAdvertisementBytes+1)))), nil
	}))
	_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("error = %v, want ErrUnresolvable", err)
	}
}

func TestResolveCommitRedirectValidation(t *testing.T) {
	t.Run("public HTTPS redirect", func(t *testing.T) {
		var hosts []string
		lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
			hosts = append(hosts, host)
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		requests := 0
		r := resolverWithRoundTripper(lookup, roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				resp := response(http.StatusFound, "", io.NopCloser(strings.NewReader("")))
				resp.Header.Set("Location", "https://mirror.example/repo.git/info/refs?service=git-upload-pack")
				return resp, nil
			}
			return response(http.StatusOK, advertisementMediaType, io.NopCloser(strings.NewReader(advertisement(shaA+" HEAD\n")))), nil
		}))
		got, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "")
		if err != nil {
			t.Fatalf("ResolveCommit: %v", err)
		}
		if got != shaA {
			t.Fatalf("SHA = %q, want %q", got, shaA)
		}
		if got, want := strings.Join(hosts, ","), "git.example,mirror.example"; got != want {
			t.Fatalf("validated hosts = %q, want %q", got, want)
		}
	})

	tests := []struct {
		name     string
		location string
		lookup   lookupIPFunc
	}{
		{name: "HTTP", location: "http://mirror.example/repo.git/info/refs?service=git-upload-pack", lookup: publicLookup},
		{name: "credentials", location: "https://user@mirror.example/repo.git/info/refs?service=git-upload-pack", lookup: publicLookup},
		{name: "non-443 port", location: "https://mirror.example:8443/repo.git/info/refs?service=git-upload-pack", lookup: publicLookup},
		{name: "changed query", location: "https://mirror.example/repo.git/info/refs?service=git-upload-pack&token=x", lookup: publicLookup},
		{name: "fragment", location: "https://mirror.example/repo.git/info/refs?service=git-upload-pack#x", lookup: publicLookup},
		{name: "private DNS", location: "https://internal.example/repo.git/info/refs?service=git-upload-pack", lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host == "git.example" {
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}}, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			r := resolverWithRoundTripper(tc.lookup, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				resp := response(http.StatusFound, "", io.NopCloser(strings.NewReader("")))
				resp.Header.Set("Location", tc.location)
				return resp, nil
			}))
			_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable", err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1 (redirect target must not be fetched)", requests)
			}
		})
	}
}

func TestResolveCommitStopsRedirectLoop(t *testing.T) {
	requests := 0
	r := resolverWithRoundTripper(publicLookup, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		resp := response(http.StatusFound, "", io.NopCloser(strings.NewReader("")))
		resp.Header.Set("Location", req.URL.String())
		return resp, nil
	}))
	_, err := r.ResolveCommit(context.Background(), "https://git.example/repo.git", "main")
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("error = %v, want ErrUnresolvable", err)
	}
	if requests != maxRedirects {
		t.Fatalf("requests = %d, want %d", requests, maxRedirects)
	}
}

func TestDialContextReresolvesAndDialsOnlyVettedAddresses(t *testing.T) {
	lookups := 0
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("1.1.1.1")}}, nil
	}
	var dialed []string
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	r := newResolver(lookup, func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if strings.HasPrefix(address, "8.8.8.8:") {
			return nil, errors.New("first address unavailable")
		}
		return client, nil
	})

	if _, err := r.publicIPs(context.Background(), "git.example"); err != nil {
		t.Fatalf("pre-request validation: %v", err)
	}
	conn, err := r.dialContext(context.Background(), "tcp", "git.example:443")
	if err != nil {
		t.Fatalf("dialContext: %v", err)
	}
	_ = conn.Close()
	if lookups != 2 {
		t.Fatalf("DNS lookups = %d, want 2 (pre-request + immediately before dial)", lookups)
	}
	if got, want := strings.Join(dialed, ","), "8.8.8.8:443,1.1.1.1:443"; got != want {
		t.Fatalf("dialed = %q, want %q", got, want)
	}
}

func TestDialContextRejectsDNSRebinding(t *testing.T) {
	lookups := 0
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dials := 0
	r := newResolver(lookup, func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	})
	if _, err := r.publicIPs(context.Background(), "git.example"); err != nil {
		t.Fatalf("pre-request validation: %v", err)
	}
	_, err := r.dialContext(context.Background(), "tcp", "git.example:443")
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("error = %v, want ErrUnresolvable", err)
	}
	if dials != 0 {
		t.Fatalf("underlying dials = %d, want 0", dials)
	}
}

func TestDialContextRejectsOtherPorts(t *testing.T) {
	r := newResolver(publicLookup, unusedDial)
	_, err := r.dialContext(context.Background(), "tcp", "git.example:80")
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("error = %v, want ErrUnresolvable", err)
	}
}

func TestNewDisablesAmbientProxy(t *testing.T) {
	r := New()
	transport, ok := r.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", r.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("transport.Proxy is non-nil; ambient proxy settings must be disabled")
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		addr := net.ParseIP(raw)
		parsed, ok := netipAddrFromIP(addr)
		if !ok || !isPublicIP(parsed) {
			t.Errorf("isPublicIP(%s) = false, want true", raw)
		}
	}
}

func netipAddrFromIP(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	return addr.Unmap(), ok
}

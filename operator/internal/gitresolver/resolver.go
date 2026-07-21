// Package gitresolver resolves anonymous HTTPS Git refs without accepting
// credentials or allowing the operator to reach private network addresses.
package gitresolver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	advertisementMediaType = "application/x-git-upload-pack-advertisement"
	maxAdvertisementBytes  = 4 << 20
	maxRefLength           = 250
	maxRedirects           = 10
)

// ErrUnresolvable marks a repository URL or ref that cannot be used by the
// anonymous generic-Git source path. Callers may permanently drop work that
// wraps this error. DNS, transport, response-read, and server-side failures do
// not wrap it and remain retryable.
var ErrUnresolvable = errors.New("git: repository or ref cannot be resolved")

type lookupIPFunc func(context.Context, string) ([]net.IPAddr, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// Resolver resolves refs through Git's smart-HTTP upload-pack advertisement.
// Construct one with New; its zero value is not usable.
type Resolver struct {
	lookupIP lookupIPFunc
	dial     dialContextFunc
	client   *http.Client
}

// New returns a resolver that permits anonymous HTTPS requests only to hosts
// whose complete DNS answer consists of public IP addresses. The host is
// resolved once before the request and again immediately before dialing; the
// dial uses the vetted IP literal, preventing DNS rebinding between validation
// and connection establishment.
func New() *Resolver {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return newResolver(net.DefaultResolver.LookupIPAddr, dialer.DialContext)
}

func newResolver(lookup lookupIPFunc, dial dialContextFunc) *Resolver {
	r := &Resolver{lookupIP: lookup, dial: dial}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            r.dialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           20,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ExpectContinueTimeout:  time.Second,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	r.client = &http.Client{
		Transport:     transport,
		CheckRedirect: r.checkRedirect,
		Timeout:       30 * time.Second,
	}
	return r
}

// ResolveCommit returns the lowercase, full 40-character object ID advertised
// for ref. An empty ref and "HEAD" resolve the advertised HEAD. A short ref
// first tries refs/heads/<ref>, then refs/tags/<ref>; an annotated tag prefers
// its peeled ^{} object.
func (r *Resolver) ResolveCommit(ctx context.Context, repoURL, ref string) (string, error) {
	if r == nil || r.lookupIP == nil || r.dial == nil || r.client == nil {
		return "", errors.New("git: resolver is not initialized")
	}
	if err := validateRef(ref); err != nil {
		return "", err
	}

	repository, err := parseRepositoryURL(repoURL)
	if err != nil {
		return "", err
	}
	if _, err := r.publicIPs(ctx, repository.Hostname()); err != nil {
		return "", fmt.Errorf("git: validating repository host %q: %w", repository.Hostname(), err)
	}

	endpoint := *repository
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/info/refs"
	endpoint.RawPath = ""
	endpoint.RawQuery = url.Values{"service": {"git-upload-pack"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("git: building upload-pack advertisement request: %w: %w", err, ErrUnresolvable)
	}
	req.Header.Set("Accept", advertisementMediaType)
	req.Header.Set("User-Agent", "orkano-git-resolver/1")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("git: fetching upload-pack advertisement: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		// A repository or intermediary may recover from a request timeout or
		// rate limit. Leave these unwrapped so the dispatcher nacks the doorbell
		// and retries it instead of permanently dropping the manual deploy.
		return "", fmt.Errorf("git: upload-pack advertisement answered %s", resp.Status)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", fmt.Errorf("git: upload-pack advertisement answered %s: %w", resp.Status, ErrUnresolvable)
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("git: upload-pack advertisement answered %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("git: upload-pack advertisement answered %s: %w", resp.Status, ErrUnresolvable)
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, advertisementMediaType) {
		return "", fmt.Errorf("git: upload-pack response has content type %q, want %s: %w", resp.Header.Get("Content-Type"), advertisementMediaType, ErrUnresolvable)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdvertisementBytes+1))
	if err != nil {
		return "", fmt.Errorf("git: reading upload-pack advertisement: %w", err)
	}
	if len(body) > maxAdvertisementBytes {
		return "", fmt.Errorf("git: upload-pack advertisement exceeds %d bytes: %w", maxAdvertisementBytes, ErrUnresolvable)
	}

	refs, err := parseAdvertisement(body)
	if err != nil {
		return "", err
	}
	sha, ok := resolveRef(refs, ref)
	if !ok {
		return "", fmt.Errorf("git: ref %q is not advertised by %s: %w", displayRef(ref), repository.Redacted(), ErrUnresolvable)
	}
	return sha, nil
}

func parseRepositoryURL(raw string) (*url.URL, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return nil, fmt.Errorf("git: repository URL is empty or contains whitespace/control characters: %w", ErrUnresolvable)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("git: parsing repository URL: %w: %w", err, ErrUnresolvable)
	}
	if err := validateURLShape(u, false); err != nil {
		return nil, err
	}
	u.Scheme = "https"
	return u, nil
}

func validateURLShape(u *url.URL, allowServiceQuery bool) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.Opaque != "" {
		return fmt.Errorf("git: repository URL must be an absolute HTTPS URL: %w", ErrUnresolvable)
	}
	if u.User != nil {
		return fmt.Errorf("git: repository URL must not contain credentials: %w", ErrUnresolvable)
	}
	if u.Fragment != "" {
		return fmt.Errorf("git: repository URL must not contain a fragment: %w", ErrUnresolvable)
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("git: repository URL port %q is not 443: %w", port, ErrUnresolvable)
	}
	if !allowServiceQuery {
		if u.RawQuery != "" || u.ForceQuery {
			return fmt.Errorf("git: repository URL must not contain a query: %w", ErrUnresolvable)
		}
		return nil
	}
	query := u.Query()
	values, ok := query["service"]
	if !ok || len(query) != 1 || len(values) != 1 || values[0] != "git-upload-pack" {
		return fmt.Errorf("git: redirected upload-pack URL changed its service query: %w", ErrUnresolvable)
	}
	return nil
}

func (r *Resolver) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("git: stopped after %d redirects: %w", maxRedirects, ErrUnresolvable)
	}
	if err := validateURLShape(req.URL, true); err != nil {
		return err
	}
	req.URL.Scheme = "https"
	if _, err := r.publicIPs(req.Context(), req.URL.Hostname()); err != nil {
		return fmt.Errorf("git: validating redirect host %q: %w", req.URL.Hostname(), err)
	}
	return nil
}

func (r *Resolver) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("git: parsing dial address %q: %w: %w", address, err, ErrUnresolvable)
	}
	if port != "443" {
		return nil, fmt.Errorf("git: refusing dial to port %q: %w", port, ErrUnresolvable)
	}
	ips, err := r.publicIPs(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("git: validating dial host %q: %w", host, err)
	}

	var lastErr error
	for _, ip := range ips {
		if (network == "tcp4" && !ip.Is4()) || (network == "tcp6" && !ip.Is6()) {
			continue
		}
		conn, dialErr := r.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no address matched the requested network")
	}
	return nil, fmt.Errorf("git: dialing %s: %w", host, lastErr)
}

func (r *Resolver) publicIPs(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if literal.Zone() != "" || !isPublicIP(literal) {
			return nil, fmt.Errorf("git: host resolves to non-public address %s: %w", literal, ErrUnresolvable)
		}
		return []netip.Addr{literal}, nil
	}

	resolved, err := r.lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("git: resolving %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("git: resolving %q returned no addresses", host)
	}

	addresses := make([]netip.Addr, 0, len(resolved))
	seen := make(map[netip.Addr]struct{}, len(resolved))
	for _, resolvedIP := range resolved {
		addr, ok := netip.AddrFromSlice(resolvedIP.IP)
		if !ok {
			return nil, fmt.Errorf("git: resolver returned an invalid address for %q", host)
		}
		addr = addr.Unmap()
		if resolvedIP.Zone != "" || !isPublicIP(addr) {
			return nil, fmt.Errorf("git: host %q resolves to non-public address %s: %w", host, addr, ErrUnresolvable)
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addresses = append(addresses, addr)
	}
	return addresses, nil
}

var nonPublicSpecialUse = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func isPublicIP(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicSpecialUse {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func validateRef(ref string) error {
	if len(ref) > maxRefLength {
		return fmt.Errorf("git: ref exceeds %d bytes: %w", maxRefLength, ErrUnresolvable)
	}
	if ref == "" || ref == "HEAD" {
		return nil
	}
	if strings.HasPrefix(ref, "refs/") && !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return fmt.Errorf("git: ref %q is outside refs/heads and refs/tags: %w", ref, ErrUnresolvable)
	}
	full := ref
	if !strings.HasPrefix(full, "refs/") {
		full = "refs/heads/" + full
	}
	if !validGitRefName(full) {
		return fmt.Errorf("git: ref %q is malformed: %w", ref, ErrUnresolvable)
	}
	return nil
}

func validGitRefName(ref string) bool {
	if ref == "@" || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return strings.IndexFunc(ref, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r)
	}) < 0
}

type packetReader struct {
	data   []byte
	offset int
}

func (r *packetReader) next() (payload []byte, flush bool, err error) {
	if len(r.data)-r.offset < 4 {
		return nil, false, errors.New("truncated packet length")
	}
	header := r.data[r.offset : r.offset+4]
	length, parseErr := strconv.ParseUint(string(header), 16, 16)
	if parseErr != nil {
		return nil, false, fmt.Errorf("invalid packet length %q", header)
	}
	r.offset += 4
	if length == 0 {
		return nil, true, nil
	}
	if length < 4 {
		return nil, false, fmt.Errorf("unsupported special packet %q", header)
	}
	payloadLength := int(length) - 4
	if payloadLength > len(r.data)-r.offset {
		return nil, false, errors.New("packet extends beyond response")
	}
	payload = r.data[r.offset : r.offset+payloadLength]
	r.offset += payloadLength
	return payload, false, nil
}

func parseAdvertisement(body []byte) (map[string]string, error) {
	reader := packetReader{data: body}
	service, flush, err := reader.next()
	if err != nil || flush || string(service) != "# service=git-upload-pack\n" {
		return nil, fmt.Errorf("git: response is not a git-upload-pack advertisement: %w", ErrUnresolvable)
	}
	_, flush, err = reader.next()
	if err != nil || !flush {
		return nil, fmt.Errorf("git: upload-pack service preamble is not flush-terminated: %w", ErrUnresolvable)
	}

	refs := make(map[string]string)
	terminated := false
	for reader.offset < len(body) {
		line, isFlush, readErr := reader.next()
		if readErr != nil {
			return nil, fmt.Errorf("git: parsing upload-pack advertisement: %w: %w", readErr, ErrUnresolvable)
		}
		if isFlush {
			terminated = true
			break
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return nil, fmt.Errorf("git: upload-pack advertisement contains an unterminated ref: %w", ErrUnresolvable)
		}
		line = line[:len(line)-1]
		if nul := bytesIndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		shaBytes, refBytes, ok := cutBytes(line, ' ')
		sha, name := string(shaBytes), string(refBytes)
		if !ok || !isLowerHexSHA(sha) || !validAdvertisedRef(name) {
			return nil, fmt.Errorf("git: upload-pack advertisement contains a malformed ref: %w", ErrUnresolvable)
		}
		if previous, exists := refs[name]; exists && previous != sha {
			return nil, fmt.Errorf("git: upload-pack advertisement repeats ref %q with different object IDs: %w", name, ErrUnresolvable)
		}
		refs[name] = sha
	}
	if !terminated || reader.offset != len(body) {
		return nil, fmt.Errorf("git: upload-pack advertisement is not cleanly flush-terminated: %w", ErrUnresolvable)
	}
	return refs, nil
}

func validAdvertisedRef(ref string) bool {
	if ref == "HEAD" || ref == ".have" || ref == "capabilities^{}" {
		return true
	}
	if strings.HasSuffix(ref, "^{}") {
		ref = strings.TrimSuffix(ref, "^{}")
		if !strings.HasPrefix(ref, "refs/tags/") {
			return false
		}
	}
	return strings.HasPrefix(ref, "refs/") && validGitRefName(ref)
}

func bytesIndexByte(value []byte, target byte) int {
	for i, b := range value {
		if b == target {
			return i
		}
	}
	return -1
}

func cutBytes(value []byte, separator byte) (before, after []byte, ok bool) {
	index := bytesIndexByte(value, separator)
	if index < 0 {
		return value, nil, false
	}
	return value[:index], value[index+1:], true
}

func isLowerHexSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func resolveRef(refs map[string]string, ref string) (string, bool) {
	if ref == "" || ref == "HEAD" {
		sha, ok := refs["HEAD"]
		return sha, ok
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		sha, ok := refs[ref]
		return sha, ok
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return resolveTag(refs, ref)
	}
	if sha, ok := refs["refs/heads/"+ref]; ok {
		return sha, true
	}
	return resolveTag(refs, "refs/tags/"+ref)
}

func resolveTag(refs map[string]string, tag string) (string, bool) {
	if sha, ok := refs[tag+"^{}"]; ok {
		return sha, true
	}
	sha, ok := refs[tag]
	return sha, ok
}

func displayRef(ref string) string {
	if ref == "" {
		return "HEAD"
	}
	return ref
}

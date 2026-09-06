package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Fetcher performs explicit URL fetches for Spec.URL. It exists to be
// safe by default for a memory server: the SSRF guard validates the
// address at dial time and refuses loopback, private, link-local,
// multicast and unspecified targets (including the cloud-metadata
// range), redirects re-dial through the same guard, proxy environment
// variables are ignored so the guard always sees the real destination,
// and the response body is size-capped.
type Fetcher struct {
	MaxBytes int64         // response body cap; <=0 uses DefaultMaxBytes
	Timeout  time.Duration // whole-request cap; <=0 uses DefaultTimeout

	allowPrivate bool // in-package test hook; the CLI wiring never sets it
}

// NewFetcher builds the production fetcher: guarded, never private.
func NewFetcher(maxBytes int64, timeout time.Duration) *Fetcher {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Fetcher{MaxBytes: maxBytes, Timeout: timeout}
}

// Fetch GETs rawURL, returning the capped body and the response's
// content type. Only http and https URLs are fetchable, userinfo is
// rejected (and never echoed into errors), and non-2xx responses fail
// with the status.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (body []byte, contentType string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: parse %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("fetch %s: only http and https URLs are supported", u.Scheme+"://"+u.Host)
	}
	if u.User != nil {
		safe := *u
		safe.User = nil
		return nil, "", fmt.Errorf("fetch %s: userinfo in URLs is not accepted", safe.String())
	}
	client := &http.Client{
		Timeout: f.timeout(),
		Transport: &http.Transport{
			Proxy:       nil, // never the env: the dial guard must see the real destination
			DialContext: f.guardedDial,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("fetch: too many redirects")
			}
			return nil // every redirect target re-dials through the SSRF guard
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("fetch %s: HTTP %s", u.Host, resp.Status)
	}
	b, err := readLimited(resp.Body, f.maxBytes())
	if err != nil {
		return nil, "", err
	}
	return b, resp.Header.Get("Content-Type"), nil
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return f.MaxBytes
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout <= 0 {
		return DefaultTimeout
	}
	return f.Timeout
}

// guardedDial resolves addr and dials only validated public IPs, so the
// checked address is exactly the connected address (no DNS-rebinding
// window between a pre-check and the dial).
func (f *Fetcher) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %s: %w", host, err)
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var dialErrs []error
	public := 0
	for _, ip := range ips {
		if !f.allowPrivate && !isPublicIP(ip) {
			continue
		}
		public++
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErrs = append(dialErrs, err)
	}
	if public == 0 {
		return nil, fmt.Errorf("fetch: ssrf guard: %s has no public address (private, loopback and link-local targets are refused)", host)
	}
	return nil, fmt.Errorf("fetch: dial %s: %w", host, errors.Join(dialErrs...))
}

// isPublicIP reports whether ip is a routable public address: not
// loopback, RFC 1918 / ULA private, link-local (covers cloud metadata),
// CGNAT, multicast or unspecified.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 {
			return false // CGNAT 100.64.0.0/10
		}
	}
	return true
}

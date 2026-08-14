package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxUpstreamRedirects = 10

var prohibitedUpstreamPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}

type outboundPolicy struct {
	allowPrivateNetworks bool
	resolver             ipResolver
}

type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

func newUpstreamHTTPClient(allowPrivateNetworks bool) *http.Client {
	policy := outboundPolicy{
		allowPrivateNetworks: allowPrivateNetworks,
		resolver:             net.DefaultResolver,
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.dialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxUpstreamRedirects {
				return errors.New("upstream redirect limit exceeded")
			}
			return policy.validateURL(req.Context(), req.URL)
		},
	}
}

func validateUpstreamURL(upstream *url.URL) error {
	if upstream == nil {
		return errors.New("upstream URL is required")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return fmt.Errorf("unsupported upstream URL scheme %q", upstream.Scheme)
	}
	if upstream.User != nil {
		return errors.New("upstream URL must not contain user information")
	}
	if upstream.Hostname() == "" {
		return errors.New("upstream URL host is required")
	}
	return nil
}

func validateLiteralUpstreamHost(upstream *url.URL, allowPrivateNetworks bool) error {
	address, err := netip.ParseAddr(upstream.Hostname())
	if err == nil && !allowPrivateNetworks && prohibitedUpstreamAddress(address) {
		return errors.New("upstream URL resolves to a prohibited network address")
	}
	if strings.EqualFold(strings.TrimSuffix(upstream.Hostname(), "."), "localhost") && !allowPrivateNetworks {
		return errors.New("upstream URL resolves to a prohibited network address")
	}
	return nil
}

func (p outboundPolicy) validateURL(ctx context.Context, target *url.URL) error {
	if err := validateUpstreamURL(target); err != nil {
		return err
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "localhost" && !p.allowPrivateNetworks {
		return errors.New("upstream URL resolves to a prohibited network address")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !p.allowPrivateNetworks && prohibitedUpstreamAddress(address) {
			return errors.New("upstream URL resolves to a prohibited network address")
		}
		return nil
	}
	addresses, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve upstream host: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("upstream host resolved without addresses")
	}
	for _, address := range addresses {
		if !p.allowPrivateNetworks && prohibitedUpstreamAddress(address) {
			return errors.New("upstream URL resolves to a prohibited network address")
		}
	}
	return nil
}

func (p outboundPolicy) dialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse upstream address: %w", err)
		}
		addresses, err := p.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve upstream host: %w", err)
		}
		for _, resolved := range addresses {
			if !p.allowPrivateNetworks && prohibitedUpstreamAddress(resolved) {
				return nil, errors.New("upstream URL resolves to a prohibited network address")
			}
		}
		var dialErrors []error
		for _, resolved := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.String(), port))
			if err == nil {
				return connection, nil
			}
			dialErrors = append(dialErrors, err)
		}
		return nil, errors.Join(dialErrors...)
	}
}

func prohibitedUpstreamAddress(address netip.Addr) bool {
	address = address.Unmap()
	if address.IsUnspecified() ||
		address.IsLoopback() ||
		address.IsPrivate() ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		address.IsMulticast() {
		return true
	}
	for _, prefix := range prohibitedUpstreamPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

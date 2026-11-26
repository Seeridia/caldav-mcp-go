package caldav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
)

const maxFeedBytes = 5 << 20

func newFeedHTTPClient() *http.Client {
	// A source URL is account-controlled. Use direct connections without
	// proxies, cookies, CalDAV credentials, or cross-request URL disclosure.
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext:            dialPublicFeed,
			TLSHandshakeTimeout:    10 * time.Second,
			IdleConnTimeout:        30 * time.Second,
			MaxIdleConns:           16,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 || !validFeedURL(req.URL) ||
				(via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https") {
				return http.ErrUseLastResponse
			}
			req.Header.Del("Referer")
			return nil
		},
	}
}

func validFeedURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Opaque == "" && u.Fragment == ""
}

var globalIPv6 = netip.MustParsePrefix("2000::/3")

// IsGlobalUnicast includes several special-use ranges. Exclude those as well
// as private addresses, and require global-unicast space for IPv6.
var specialFeedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicFeedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalIPv6.Contains(ip)) {
		return false
	}
	for _, network := range specialFeedNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

func dialPublicFeed(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	// Pass the checked address to the dialer so a second DNS lookup cannot
	// change the destination. HTTP still uses the original host for TLS.
	for _, ip := range addresses {
		if publicFeedIP(ip) {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
	}
	return nil, errors.New("could not connect to a public subscription feed address")
}

func (c *Client) subscriptionDocument(ctx context.Context, collection collection) (*ical.Calendar, error) {
	if collection.source == "" {
		return nil, fmt.Errorf("%w: subscribed calendar has no feed source", calendar.ErrBackend)
	}
	target, err := url.Parse(collection.source)
	if err != nil {
		return nil, backend("invalid subscription feed URL", err)
	}
	if target.Scheme == "webcal" || target.Scheme == "webcals" {
		target.Scheme = "https"
	}
	if !validFeedURL(target) {
		return nil, fmt.Errorf("%w: invalid subscription feed URL", calendar.ErrBackend)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, backend("prepare subscription feed request", err)
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.1")
	res, err := c.feedHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The HTTP client's URL error includes the potentially signed source.
		return nil, fmt.Errorf("%w: fetch subscription feed", calendar.ErrBackend)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: subscription feed returned HTTP %d", calendar.ErrBackend, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFeedBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: read subscription feed", calendar.ErrBackend)
	}
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("%w: subscription feed is too large", calendar.ErrBackend)
	}
	document, err := ical.NewDecoder(bytes.NewReader(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))).Decode()
	if err != nil {
		return nil, backend("decode subscription feed", err)
	}
	return document, nil
}

func (c *Client) subscriptionEvent(ctx context.Context, collection collection, uid string) (calendar.Event, error) {
	document, err := c.subscriptionDocument(ctx, collection)
	if err != nil {
		return calendar.Event{}, err
	}
	for _, event := range document.Events() {
		if text(event.Props, ical.PropUID) == uid && event.Props.Get(ical.PropRecurrenceID) == nil {
			result, err := c.decodeEvent(collection.ID, event)
			return result.Event, err
		}
	}
	return calendar.Event{}, calendar.ErrNotFound
}

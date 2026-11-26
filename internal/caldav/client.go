package caldav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	dav "github.com/emersion/go-webdav/caldav"
)

var errReportPrecondition = errors.New("CalDAV REPORT returned HTTP 412")

type Client struct {
	dav      *dav.Client
	feedHTTP *http.Client
	http     webdav.HTTPClient
	origin   url.URL
	zone     *time.Location
}

func New(endpoint, username, password string, zone *time.Location) (*Client, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, errors.New("CALDAV_URL must be an HTTP(S) endpoint without credentials, query, or fragment")
	}
	httpClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) || req.URL.User != nil ||
				len(via) >= 5 || via[0].Method == http.MethodPut || via[0].Method == http.MethodDelete {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	authenticated := webdav.HTTPClientWithBasicAuth(httpClient, username, password)
	client, err := dav.NewClient(reportClient{authenticated}, base.String())
	if err != nil {
		return nil, errors.New("invalid CalDAV endpoint")
	}
	return &Client{
		dav: client, http: authenticated, feedHTTP: newFeedHTTPClient(), zone: zone,
		origin: url.URL{Scheme: base.Scheme, Host: base.Host},
	}, nil
}

type reportClient struct {
	webdav.HTTPClient
}

func (c reportClient) Do(req *http.Request) (*http.Response, error) {
	res, err := c.HTTPClient.Do(req)
	if err == nil && req.Method == "REPORT" && res.StatusCode == http.StatusPreconditionFailed {
		res.Body.Close()
		return nil, errReportPrecondition
	}
	return res, err
}

func backend(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", calendar.ErrBackend, operation)
}

func (c *Client) OccurrenceReader(ctx context.Context) (calendar.OccurrenceReader, error) {
	collections, err := c.collections(ctx)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, id string, start, end time.Time) ([]calendar.Occurrence, error) {
		for _, collection := range collections {
			if collection.ID != id {
				continue
			}
			if collection.ReadOnly {
				return c.subscriptionOccurrences(ctx, collection, start, end)
			}
			return c.reportOccurrences(ctx, id, start, end)
		}
		return nil, calendar.ErrNotFound
	}, nil
}

func (c *Client) reportOccurrences(ctx context.Context, id string, start, end time.Time) ([]calendar.Occurrence, error) {
	// DATE and floating DATE-TIME can use different time zones on the
	// server. Widen discovery and apply the exact window locally.
	from, to := start.UTC().Add(-24*time.Hour), end.UTC().Add(24*time.Hour)
	// Request expansion without an allprop/allcomp selector. iCloud silently
	// omits matching events when that selector accompanies a time range.
	const report = `<c:calendar-query xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:d="DAV:">
<d:prop><c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data></d:prop>
<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`
	const layout = "20060102T150405Z"
	first, last := from.Format(layout), to.Format(layout)
	result := []calendar.Occurrence{}
	err := c.readCalendarReport(ctx, id, fmt.Sprintf(report, first, last, first, last), func(_ string, document *ical.Calendar) (bool, error) {
		for _, event := range document.Events() {
			if isSeries(event) {
				return false, fmt.Errorf("%w: server did not expand a recurring event", calendar.ErrBackend)
			}
			occurrence, err := c.decodeEvent(id, event)
			if err != nil {
				return false, err
			}
			if inWindow(occurrence, start, end) {
				result = append(result, occurrence)
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) find(ctx context.Context, collection, uid string) (string, error) {
	objects, err := c.dav.QueryCalendar(ctx, collection, &dav.CalendarQuery{
		CompRequest: dav.CalendarCompRequest{Name: ical.CompCalendar, AllProps: true, AllComps: true},
		CompFilter: dav.CompFilter{Name: ical.CompCalendar, Comps: []dav.CompFilter{{
			Name:  ical.CompEvent,
			Props: []dav.PropFilter{{Name: ical.PropUID, TextMatch: &dav.TextMatch{Text: uid}}},
		}}},
	})
	if errors.Is(err, errReportPrecondition) {
		return c.findWithoutUIDFilter(ctx, collection, uid)
	}
	if err != nil {
		return "", backend("query events by UID", err)
	}
	for _, object := range objects {
		for _, event := range object.Data.Events() {
			if text(event.Props, ical.PropUID) == uid {
				if path.Dir(object.Path) != path.Clean(collection) || path.Clean(object.Path) != object.Path {
					return "", fmt.Errorf("%w: event resource is outside its calendar", calendar.ErrBackend)
				}
				return object.Path, nil
			}
		}
	}
	return "", calendar.ErrNotFound
}

func (c *Client) resource(ctx context.Context, method, resourcePath, etag string, data *ical.Calendar) (*http.Response, error) {
	target := c.origin
	target.Path = resourcePath
	var body io.Reader
	if data != nil {
		var encoded bytes.Buffer
		if err := ical.NewEncoder(&encoded).Encode(data); err != nil {
			return nil, backend("encode event", err)
		}
		body = &encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, backend("prepare calendar request", err)
	}
	if data != nil {
		req.Header.Set("Content-Type", ical.MIMEType)
	}
	if etag == "*" {
		req.Header.Set("If-None-Match", etag)
	} else if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, backend(method+" event", err)
	}
	if res.StatusCode/100 == 2 {
		return res, nil
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return nil, calendar.ErrNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return nil, calendar.ErrConflict
	default:
		return nil, fmt.Errorf("%w: %s event returned HTTP %d", calendar.ErrBackend, method, res.StatusCode)
	}
}

type eventResource struct {
	document *ical.Calendar
	event    ical.Event
	path     string
	etag     string
}

func (c *Client) load(ctx context.Context, collection, uid string) (eventResource, error) {
	if !strings.ContainsAny(uid, "/\\") {
		// New resources and many existing calendars use UID.ics as the path.
		// readEvent verifies the UID before callers can update or delete it.
		event, err := c.readEvent(ctx, path.Join(collection, uid+".ics"), uid)
		if !errors.Is(err, calendar.ErrNotFound) {
			return event, err
		}
	}
	resourcePath, err := c.find(ctx, collection, uid)
	if err != nil {
		return eventResource{}, err
	}
	return c.readEvent(ctx, resourcePath, uid)
}

func (c *Client) readEvent(ctx context.Context, resourcePath, uid string) (eventResource, error) {
	var out eventResource
	res, err := c.resource(ctx, http.MethodGet, resourcePath, "", nil)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	document, err := ical.NewDecoder(res.Body).Decode()
	if err != nil {
		return out, backend("decode event", err)
	}
	for _, event := range document.Events() {
		if text(event.Props, ical.PropUID) == uid && event.Props.Get(ical.PropRecurrenceID) == nil {
			return eventResource{document: document, event: event, path: resourcePath, etag: res.Header.Get("ETag")}, nil
		}
	}
	return out, calendar.ErrNotFound
}

func (c *Client) conditionalWrite(ctx context.Context, method, target, etag string, data *ical.Calendar) error {
	if etag == "" || strings.HasPrefix(etag, "W/") {
		return fmt.Errorf("%w: server did not provide a strong ETag", calendar.ErrBackend)
	}
	res, err := c.resource(ctx, method, target, etag, data)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

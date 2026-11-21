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
	"slices"
	"strings"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	dav "github.com/emersion/go-webdav/caldav"
)

var errReportPrecondition = errors.New("CalDAV REPORT returned HTTP 412")

type Client struct {
	dav    *dav.Client
	http   webdav.HTTPClient
	origin url.URL
	zone   *time.Location
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
		dav: client, http: authenticated, zone: zone,
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

func (c *Client) Calendars(ctx context.Context) ([]calendar.Calendar, error) {
	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, backend("find principal", err)
	}
	home, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, backend("find calendar home", err)
	}
	collections, err := c.dav.FindCalendars(ctx, home)
	if err != nil {
		return nil, backend("list calendars", err)
	}
	result := make([]calendar.Calendar, 0, len(collections))
	for _, collection := range collections {
		if len(collection.SupportedComponentSet) > 0 && !slices.Contains(collection.SupportedComponentSet, ical.CompEvent) {
			continue
		}
		result = append(result, calendar.Calendar{ID: collection.Path, Name: collection.Name})
	}
	slices.SortFunc(result, func(a, b calendar.Calendar) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (c *Client) calendarPath(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", errors.New("calendar_id is required")
	}
	calendars, err := c.Calendars(ctx)
	if err != nil {
		return "", err
	}
	for _, candidate := range calendars {
		if candidate.ID == id {
			return id, nil
		}
	}
	return "", calendar.ErrNotFound
}

func backend(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", calendar.ErrBackend, operation)
}

func (c *Client) Occurrences(ctx context.Context, id string, start, end time.Time) ([]calendar.Occurrence, error) {
	// The service resolves the calendar ID before calling Occurrences.
	// DATE and floating DATE-TIME can use different time zones on the
	// server. Widen discovery and apply the exact window locally.
	from, to := start.UTC().Add(-24*time.Hour), end.UTC().Add(24*time.Hour)
	objects, err := c.query(ctx, id, dav.CompFilter{
		Name: ical.CompEvent, Start: from, End: to,
	}, &dav.CalendarExpandRequest{Start: from, End: to})
	if err != nil {
		return nil, backend("query events", err)
	}
	result := []calendar.Occurrence{}
	for _, object := range objects {
		for _, event := range object.Data.Events() {
			if isSeries(event) {
				return nil, fmt.Errorf("%w: server did not expand a recurring event", calendar.ErrBackend)
			}
			occurrence, err := c.decodeEvent(id, event)
			if err != nil {
				return nil, err
			}
			overlaps := occurrence.StartsAt.Before(end) && occurrence.EndsAt.After(start)
			instant := occurrence.StartsAt.Equal(occurrence.EndsAt) &&
				!occurrence.StartsAt.Before(start) && occurrence.StartsAt.Before(end)
			if overlaps || instant {
				result = append(result, occurrence)
			}
		}
	}
	return result, nil
}

func (c *Client) query(ctx context.Context, collection string, filter dav.CompFilter, expand *dav.CalendarExpandRequest) ([]dav.CalendarObject, error) {
	return c.dav.QueryCalendar(ctx, collection, &dav.CalendarQuery{
		CompRequest: dav.CalendarCompRequest{Name: ical.CompCalendar, AllProps: true, AllComps: true, Expand: expand},
		CompFilter:  dav.CompFilter{Name: ical.CompCalendar, Comps: []dav.CompFilter{filter}},
	})
}

func (c *Client) find(ctx context.Context, collection, uid string) (string, error) {
	objects, err := c.query(ctx, collection, dav.CompFilter{
		Name:  ical.CompEvent,
		Props: []dav.PropFilter{{Name: ical.PropUID, TextMatch: &dav.TextMatch{Text: uid}}},
	}, nil)
	if errors.Is(err, errReportPrecondition) {
		// Some servers (including iCloud) do not support UID text filtering.
		objects, err = c.dav.QueryCalendar(ctx, collection, &dav.CalendarQuery{
			CompRequest: dav.CalendarCompRequest{Name: ical.CompCalendar, Comps: []dav.CalendarCompRequest{
				{Name: ical.CompEvent, Props: []string{ical.PropUID}},
			}},
			CompFilter: dav.CompFilter{Name: ical.CompCalendar, Comps: []dav.CompFilter{{Name: ical.CompEvent}}},
		})
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

func (c *Client) load(ctx context.Context, ref calendar.EventRef) (eventResource, error) {
	if strings.TrimSpace(ref.UID) == "" {
		return eventResource{}, errors.New("uid is required")
	}
	collection, err := c.calendarPath(ctx, ref.CalendarID)
	if err != nil {
		return eventResource{}, err
	}
	if !strings.ContainsAny(ref.UID, "/\\") {
		// New resources and many existing calendars use UID.ics as the path.
		// readEvent verifies the UID before callers can update or delete it.
		event, err := c.readEvent(ctx, path.Join(collection, ref.UID+".ics"), ref.UID)
		if !errors.Is(err, calendar.ErrNotFound) {
			return event, err
		}
	}
	resourcePath, err := c.find(ctx, collection, ref.UID)
	if err != nil {
		return eventResource{}, err
	}
	return c.readEvent(ctx, resourcePath, ref.UID)
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

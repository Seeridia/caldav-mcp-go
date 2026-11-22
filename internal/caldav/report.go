package caldav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
)

const uidReport = `<c:calendar-query xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:d="DAV:">
<d:prop><c:calendar-data><c:comp name="VCALENDAR"><c:comp name="VEVENT"><c:prop name="UID"/></c:comp></c:comp></c:calendar-data></d:prop>
<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter>
</c:calendar-query>`

type reportResponse struct {
	Href       string `xml:"DAV: href"`
	Properties []struct {
		Status string `xml:"DAV: status"`
		Prop   struct {
			Data string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: propstat"`
}

func (c *Client) findWithoutUIDFilter(ctx context.Context, collection, uid string) (string, error) {
	var resourcePath string
	err := c.readCalendarReport(ctx, collection, uidReport, func(href string, document *ical.Calendar) (bool, error) {
		for _, event := range document.Events() {
			if text(event.Props, ical.PropUID) == uid {
				resourcePath = href
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	if resourcePath == "" {
		return "", calendar.ErrNotFound
	}
	return resourcePath, nil
}

func (c *Client) readCalendarReport(ctx context.Context, collection, body string, visit func(string, *ical.Calendar) (bool, error)) error {
	target := c.origin
	target.Path = collection
	req, err := http.NewRequestWithContext(ctx, "REPORT", target.String(), strings.NewReader(body))
	if err != nil {
		return backend("prepare calendar REPORT", err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	res, err := c.http.Do(req)
	if err != nil {
		return backend("query events", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMultiStatus {
		return fmt.Errorf("%w: calendar REPORT returned HTTP %d", calendar.ErrBackend, res.StatusCode)
	}
	decoder := xml.NewDecoder(res.Body)
	rootSeen := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !rootSeen {
				return fmt.Errorf("%w: calendar REPORT returned no XML", calendar.ErrBackend)
			}
			return nil
		}
		if err != nil {
			return backend("decode calendar REPORT", err)
		}
		element, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if !rootSeen {
			if element.Name != (xml.Name{Space: "DAV:", Local: "multistatus"}) {
				return fmt.Errorf("%w: invalid calendar REPORT response", calendar.ErrBackend)
			}
			rootSeen = true
			continue
		}
		if element.Name != (xml.Name{Space: "DAV:", Local: "response"}) {
			continue
		}
		var item reportResponse
		if err := decoder.DecodeElement(&item, &element); err != nil {
			return backend("decode calendar REPORT", err)
		}
		href, err := url.Parse(item.Href)
		if err != nil || item.Href == "" {
			return fmt.Errorf("%w: invalid event resource path", calendar.ErrBackend)
		}
		href = target.ResolveReference(href)
		// iCloud can include the collection itself without calendar data.
		if path.Clean(href.Path) == path.Clean(collection) {
			continue
		}
		if path.Dir(href.Path) != path.Clean(collection) || path.Clean(href.Path) != href.Path {
			return fmt.Errorf("%w: event resource is outside its calendar", calendar.ErrBackend)
		}
		data := ""
		for _, prop := range item.Properties {
			status := strings.Fields(prop.Status)
			if len(status) >= 2 && status[1] == "200" && prop.Prop.Data != "" {
				data = prop.Prop.Data
				break
			}
		}
		if data == "" {
			return fmt.Errorf("%w: event resource omitted from calendar REPORT", calendar.ErrBackend)
		}
		document, err := ical.NewDecoder(strings.NewReader(data)).Decode()
		if err != nil {
			return backend("decode calendar REPORT event", err)
		}
		if stop, err := visit(href.Path, document); stop || err != nil {
			return err
		}
	}
}

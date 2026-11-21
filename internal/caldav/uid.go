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

type uidResponse struct {
	Href       string `xml:"DAV: href"`
	Properties []struct {
		Status string `xml:"DAV: status"`
		Prop   struct {
			Data string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: propstat"`
}

// iCloud rejects UID text filters and includes a collection response without
// calendar data in the unfiltered REPORT. Ignore that entry and inspect the
// event resources one at a time.
func (c *Client) findWithoutUIDFilter(ctx context.Context, collection, uid string) (string, error) {
	target := c.origin
	target.Path = collection
	req, err := http.NewRequestWithContext(ctx, "REPORT", target.String(), strings.NewReader(uidReport))
	if err != nil {
		return "", backend("prepare UID lookup", err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	res, err := c.http.Do(req)
	if err != nil {
		return "", backend("query events by UID", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMultiStatus {
		return "", fmt.Errorf("%w: UID lookup returned HTTP %d", calendar.ErrBackend, res.StatusCode)
	}
	decoder := xml.NewDecoder(res.Body)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", calendar.ErrNotFound
		}
		if err != nil {
			return "", backend("decode UID lookup", err)
		}
		element, ok := token.(xml.StartElement)
		if !ok || element.Name != (xml.Name{Space: "DAV:", Local: "response"}) {
			continue
		}
		var item uidResponse
		if err := decoder.DecodeElement(&item, &element); err != nil {
			return "", backend("decode UID lookup", err)
		}
		href, err := url.Parse(item.Href)
		if err != nil || item.Href == "" {
			return "", fmt.Errorf("%w: invalid event resource path", calendar.ErrBackend)
		}
		if path.Clean(href.Path) == path.Clean(collection) {
			continue
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
			return "", fmt.Errorf("%w: event resource omitted from UID lookup", calendar.ErrBackend)
		}
		document, err := ical.NewDecoder(strings.NewReader(data)).Decode()
		if err != nil {
			return "", backend("decode UID lookup event", err)
		}
		for _, event := range document.Events() {
			if text(event.Props, ical.PropUID) == uid {
				if path.Dir(href.Path) != path.Clean(collection) || path.Clean(href.Path) != href.Path {
					return "", fmt.Errorf("%w: event resource is outside its calendar", calendar.ErrBackend)
				}
				return href.Path, nil
			}
		}
	}
}

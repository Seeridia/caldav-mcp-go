package caldav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
)

const calendarHomeQuery = `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/"><d:prop>
<d:resourcetype/><d:displayname/><c:supported-calendar-component-set/><cs:source/>
</d:prop></d:propfind>`

type collection struct {
	calendar.Calendar
	source string
}

type calendarComponent struct {
	Name string `xml:"name,attr"`
}

type collectionProperties struct {
	Type *struct {
		Calendar   *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
		Subscribed *struct{} `xml:"http://calendarserver.org/ns/ subscribed"`
	} `xml:"DAV: resourcetype"`
	Name       *string `xml:"DAV: displayname"`
	Components *struct {
		Items []calendarComponent `xml:"urn:ietf:params:xml:ns:caldav comp"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	Source *struct {
		Href string `xml:"DAV: href"`
		Text string `xml:",chardata"`
	} `xml:"http://calendarserver.org/ns/ source"`
}

func (c *Client) Calendars(ctx context.Context) ([]calendar.Calendar, error) {
	collections, err := c.collections(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]calendar.Calendar, 0, len(collections))
	for _, collection := range collections {
		result = append(result, collection.Calendar)
	}
	return result, nil
}

func (c *Client) collections(ctx context.Context) ([]collection, error) {
	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, backend("find principal", err)
	}
	home, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, backend("find calendar home", err)
	}
	target := c.origin
	target.Path = home
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", target.String(), strings.NewReader(calendarHomeQuery))
	if err != nil {
		return nil, backend("prepare calendar home listing", err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, backend("list calendars", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("%w: calendar home listing returned HTTP %d", calendar.ErrBackend, res.StatusCode)
	}
	var listing struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Href       string `xml:"DAV: href"`
			Properties []struct {
				Status string               `xml:"DAV: status"`
				Prop   collectionProperties `xml:"DAV: prop"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&listing); err != nil {
		return nil, backend("decode calendar home listing", err)
	}
	result := make([]collection, 0, len(listing.Responses))
	for _, item := range listing.Responses {
		var props collectionProperties
		for _, group := range item.Properties {
			status := strings.Fields(group.Status)
			if len(status) < 2 || status[1] != "200" {
				continue
			}
			if group.Prop.Type != nil {
				props.Type = group.Prop.Type
			}
			if group.Prop.Name != nil {
				props.Name = group.Prop.Name
			}
			if group.Prop.Components != nil {
				props.Components = group.Prop.Components
			}
			if group.Prop.Source != nil {
				props.Source = group.Prop.Source
			}
		}
		if props.Type == nil || (props.Type.Calendar == nil && props.Type.Subscribed == nil) {
			continue
		}
		if props.Components != nil && len(props.Components.Items) > 0 &&
			!slices.ContainsFunc(props.Components.Items, func(comp calendarComponent) bool {
				return comp.Name == ical.CompEvent
			}) {
			continue
		}
		href, err := url.Parse(strings.TrimSpace(item.Href))
		if err != nil || href.Path == "" || href.User != nil || href.RawQuery != "" || href.ForceQuery || href.Fragment != "" {
			return nil, fmt.Errorf("%w: invalid calendar collection path", calendar.ErrBackend)
		}
		href = target.ResolveReference(href)
		if href.Scheme != target.Scheme || !strings.EqualFold(href.Host, target.Host) {
			return nil, fmt.Errorf("%w: calendar collection is outside CalDAV origin", calendar.ErrBackend)
		}
		if path.Clean(href.Path) == path.Clean(home) {
			continue
		}
		entry := collection{Calendar: calendar.Calendar{ID: href.Path, ReadOnly: props.Type.Subscribed != nil}}
		if props.Name != nil {
			entry.Name = *props.Name
		}
		if entry.ReadOnly && props.Source != nil {
			entry.source = strings.TrimSpace(props.Source.Href)
			if entry.source == "" {
				entry.source = strings.TrimSpace(props.Source.Text)
			}
		}
		result = append(result, entry)
	}
	slices.SortFunc(result, func(a, b collection) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (c *Client) resolveCollection(ctx context.Context, id string) (collection, error) {
	if id == "" {
		return collection{}, errors.New("calendar_id is required")
	}
	collections, err := c.collections(ctx)
	if err != nil {
		return collection{}, err
	}
	for _, candidate := range collections {
		if candidate.ID == id {
			return candidate, nil
		}
	}
	return collection{}, calendar.ErrNotFound
}

func (c *Client) writableCalendar(ctx context.Context, id string) (string, error) {
	collection, err := c.resolveCollection(ctx, id)
	if err != nil {
		return "", err
	}
	if collection.ReadOnly {
		return "", errors.New("subscribed calendars are read-only")
	}
	return collection.ID, nil
}

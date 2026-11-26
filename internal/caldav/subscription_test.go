package caldav

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
)

const fixtureEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:sample\r\nDTSTART:20261126T100000Z\r\nDTEND:20261126T110000Z\r\nSUMMARY:Sample event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestSubscriptionDiscoveryAndRead(t *testing.T) {
	var requests atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request to feed: method %s, authorization present = %t", r.Method, r.Header.Get("Authorization") != "")
		}
		io.WriteString(w, fixtureEvent)
	}))
	defer feed.Close()
	const personal, subscription, missing = "/home/personal/", "/home/subscribed/", "/home/no-source/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "REPORT" && r.URL.Path == personal {
			writeDiscoveryXML(w, fmt.Sprintf("<d:response><d:href>%sresource.ics</d:href><d:propstat><d:prop><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>", personal, fixtureEvent))
			return
		}
		if r.Method != "PROPFIND" {
			t.Errorf("unexpected CalDAV request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/":
			writeDiscoveryXML(w, davProperties("/", "<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>"))
		case "/principal/":
			writeDiscoveryXML(w, davProperties("/principal/", "<c:calendar-home-set><d:href>/home/</d:href></c:calendar-home-set>"))
		case "/home/":
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Depth") != "1" || !strings.Contains(string(body), "source") {
				t.Errorf("calendar home request did not ask for subscription source")
			}
			writeDiscoveryXML(w,
				davProperties("/home/", "<d:resourcetype><d:collection/></d:resourcetype>")+
					davProperties(personal, "<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Personal</d:displayname>")+
					davProperties(subscription, "<d:resourcetype><d:collection/><cs:subscribed/></d:resourcetype><d:displayname>Subscription</d:displayname><cs:source><d:href>"+feed.URL+"/sample.ics?token=synthetic</d:href></cs:source>")+
					davProperties(missing, "<d:resourcetype><d:collection/><cs:subscribed/></d:resourcetype>")+
					davProperties("/home/tasks/", "<d:resourcetype><c:calendar/></d:resourcetype><c:supported-calendar-component-set><c:comp name=\"VTODO\"/></c:supported-calendar-component-set>"))
		default:
			t.Errorf("unexpected PROPFIND path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test", "secret", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	client.feedHTTP = feed.Client()
	service := calendar.NewService(client, time.UTC)
	calendars, err := service.Calendars(t.Context())
	if err != nil || len(calendars) != 3 || calendars[1].ID != personal || calendars[2].ID != subscription || !calendars[2].ReadOnly || calendars[1].ReadOnly {
		t.Fatalf("unexpected calendar list: %+v, %v", calendars, err)
	}
	for _, id := range []string{subscription, personal} {
		result, err := service.List(t.Context(), calendar.ListInput{CalendarID: id, Start: "2026-11-26", End: "2026-11-27"})
		if err != nil || len(result.Events) != 1 || result.Events[0].UID != "sample" {
			t.Fatalf("unexpected list result for %s: %+v, %v", id, result, err)
		}
	}
	available, err := service.Available(t.Context(), calendar.AvailabilityInput{
		CalendarIDs: []string{subscription, personal}, Start: "2026-11-26", End: "2026-11-27",
		WorkStart: "10:00", WorkEnd: "12:00", DurationMinutes: 60,
	})
	if err != nil || !available.Complete || len(available.Slots) != 1 || available.Slots[0].Start.Hour() != 11 {
		t.Fatalf("unexpected availability: %+v, %v", available, err)
	}

	ref := calendar.EventRef{CalendarID: subscription, UID: "sample"}
	if event, err := service.Event(t.Context(), ref); err != nil || event.UID != "sample" {
		t.Fatalf("unexpected get result: %+v, %v", event, err)
	}
	if _, err := service.List(t.Context(), calendar.ListInput{CalendarID: missing, Start: "2026-11-26", End: "2026-11-27"}); !errors.Is(err, calendar.ErrBackend) {
		t.Fatalf("missing feed source: %v", err)
	}
	if _, err := client.Create(t.Context(), subscription, calendar.Changes{}); err == nil {
		t.Fatal("create succeeded on a subscription")
	}
	if _, err := client.Update(t.Context(), ref, calendar.Changes{}); err == nil {
		t.Fatal("update succeeded on a subscription")
	}
	if err := client.Delete(t.Context(), ref); err == nil {
		t.Fatal("delete succeeded on a subscription")
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("feed requests = %d, want 3", got)
	}
}

func writeDiscoveryXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, "<d:multistatus xmlns:d=\"DAV:\" xmlns:c=\"urn:ietf:params:xml:ns:caldav\" xmlns:cs=\"http://calendarserver.org/ns/\">%s</d:multistatus>", body)
}

func davProperties(href, props string) string {
	return fmt.Sprintf("<d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>", href, props)
}

func TestSubscriptionRecurrences(t *testing.T) {
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0",
		"BEGIN:VEVENT", "UID:weekly", "DTSTART;VALUE=DATE:20261123", "DTEND;VALUE=DATE:20261125",
		"RRULE:FREQ=WEEKLY;UNTIL=20261214", "EXDATE;VALUE=DATE:20261130,20261214", "RDATE;VALUE=DATE:20261220", "END:VEVENT",
		"BEGIN:VEVENT", "UID:weekly", "RECURRENCE-ID;VALUE=DATE:20261207", "DTSTART;VALUE=DATE:20261208", "END:VEVENT",
		"BEGIN:VEVENT", "UID:weekly", "RECURRENCE-ID;VALUE=DATE:20261220", "STATUS:CANCELLED", "END:VEVENT",
		"BEGIN:VEVENT", "UID:timed", "DTSTART;TZID=America/New_York:20261025T100000", "DTEND;TZID=America/New_York:20261025T110000",
		"RRULE:FREQ=WEEKLY;COUNT=2", "END:VEVENT", "END:VCALENDAR", "",
	}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Join(lines, "\r\n"))
	}))
	defer feed.Close()
	client := &Client{feedHTTP: feed.Client(), zone: time.UTC}
	collection := collection{Calendar: calendar.Calendar{ID: "/subscription/"}, source: feed.URL}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	occurrences, err := client.subscriptionOccurrences(t.Context(), collection, start, end)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, item := range occurrences {
		got[item.RecurrenceID] = item.Start + "/" + item.End
	}
	want := map[string]string{
		"2026-11-01T10:00:00-05:00": "2026-11-01T10:00:00-05:00/2026-11-01T11:00:00-05:00",
		"2026-11-23":                "2026-11-23/2026-11-25",
		"2026-12-07":                "2026-12-08/2026-12-09",
	}
	if len(occurrences) != len(want) || !maps.Equal(got, want) {
		t.Fatalf("occurrences = %v, want %v", got, want)
	}
}

func TestFeedRequestRefusesLocalDestination(t *testing.T) {
	var requests atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, fixtureEvent)
	}))
	defer feed.Close()
	client := &Client{feedHTTP: newFeedHTTPClient(), zone: time.UTC}
	collection := collection{source: feed.URL + "?token=synthetic"}
	_, err := client.subscriptionDocument(t.Context(), collection)
	if !errors.Is(err, calendar.ErrBackend) || strings.Contains(err.Error(), "synthetic") || requests.Load() != 0 {
		t.Fatalf("local request was not blocked with a redacted error: %v, requests = %d", err, requests.Load())
	}
}

func TestSubscriptionRedirectDoesNotForwardSource(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" {
			t.Error("redirect forwarded a private header")
		}
		io.WriteString(w, fixtureEvent)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	feedClient := newFeedHTTPClient()
	transport := feedClient.Transport.(*http.Transport)
	transport.DialContext = (&net.Dialer{}).DialContext // Allow the two local test servers.
	defer transport.CloseIdleConnections()
	client := &Client{feedHTTP: feedClient, zone: time.UTC}
	if _, err := client.subscriptionDocument(t.Context(), collection{source: origin.URL + "?token=synthetic"}); err != nil {
		t.Fatal(err)
	}
}

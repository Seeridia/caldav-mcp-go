package caldav

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
)

func TestOccurrencesDateTimeDecoding(t *testing.T) {
	zone := time.FixedZone("UTC+08:00", 8*60*60)
	windowStart := time.Date(2030, time.January, 2, 0, 0, 0, 0, zone)
	cases := []struct {
		name, property, reason, wantStart string
	}{
		{"unknown time zone", "DTSTART;TZID=Example/Synthetic:20300103T093000", "unresolvable iCalendar time zone", ""},
		{"malformed date-time", "DTSTART:synthetic-invalid-value", "invalid iCalendar date/time value", ""},
		{"unsupported value type", "DTSTART;VALUE=TEXT:synthetic-invalid-value", "unsupported iCalendar date/time value type", ""},
		{"UTC", "DTSTART:20300102T013000Z", "", "2030-01-02T01:30:00Z"},
		{"IANA time zone", "DTSTART;TZID=Asia/Shanghai:20300102T093000", "", "2030-01-02T09:30:00+08:00"},
		{"legacy offset", `DTSTART;TZID="UTC+08:00":20300102T093000`, "", "2030-01-02T09:30:00+08:00"},
		{"floating", "DTSTART:20300102T093000", "", "2030-01-02T09:30:00+08:00"},
		{"all-day", "DTSTART;VALUE=DATE:20300102", "", "2030-01-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "REPORT" || r.URL.Path != "/calendar/" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response>
<d:href>/calendar/example.ics</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><c:calendar-data><![CDATA[`+
					strings.Join([]string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Example//EN", "BEGIN:VEVENT", "UID:test", "DTSTAMP:20300101T000000Z", tc.property, "END:VEVENT", "END:VCALENDAR", ""}, "\r\n")+
					`]]></c:calendar-data></d:prop></d:propstat></d:response></d:multistatus>`)
			}))
			defer server.Close()
			client, err := New(server.URL, "test", "test", zone)
			if err != nil {
				t.Fatal(err)
			}
			for _, span := range []time.Duration{12 * time.Hour, 24 * time.Hour} {
				events, err := client.Occurrences(t.Context(), "/calendar/", windowStart, windowStart.Add(span))
				if tc.reason != "" {
					want := fmt.Sprintf("%v: decode event start: %s", calendar.ErrBackend, tc.reason)
					if !errors.Is(err, calendar.ErrBackend) || err.Error() != want || events != nil {
						t.Fatalf("window %s: got %v, %v; want nil events and %q", span, events, err, want)
					}
					continue
				}
				if err != nil || len(events) != 1 || events[0].Event.Start != tc.wantStart {
					t.Fatalf("window %s: got %v, %v; want one event starting at %s", span, events, err, tc.wantStart)
				}
			}
		})
	}
}

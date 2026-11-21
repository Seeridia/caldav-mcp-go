package mcpserver

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func New(service *calendar.Service, token string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "caldav-mcp-go", Version: "0.1.0"}, nil)
	register(server, "list_calendars", "List event calendars and their IDs.", true, false,
		func(ctx context.Context, _ struct{}) (calendarList, error) {
			items, err := service.Calendars(ctx)
			return calendarList{Calendars: items}, err
		})
	register(server, "list_events", "List events and recurring occurrences over a date range, optionally filtered by text.", true, false, service.List)
	register(server, "get_event", "Get the event or series by calendar ID and UID.", true, false, service.Event)
	register(server, "create_event", "Create a timed or all-day event, optionally with display reminders.", false, false, service.Create)
	register(server, "update_event", "Update selected fields of an event or series master. A supplied reminders list replaces supported display reminders.", false, true, service.Update)
	register(server, "delete_event", "Delete an event or the entire recurring series.", false, true,
		func(ctx context.Context, ref calendar.EventRef) (deleteResult, error) {
			err := service.Delete(ctx, ref)
			return deleteResult{Deleted: err == nil}, err
		})
	register(server, "find_available_slots", "Find fixed-length, nonoverlapping openings across selected calendars. Check whether the result is complete.", true, false, service.Available)

	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: 64 << 10})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(key), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		transport.ServeHTTP(w, r)
	})
}

type calendarList struct {
	Calendars []calendar.Calendar `json:"calendars"`
}

type deleteResult struct {
	Deleted bool `json:"deleted"`
}

func register[In, Out any](server *mcp.Server, name, description string, readOnly, destructive bool,
	run func(context.Context, In) (Out, error)) {
	mcp.AddTool(server, &mcp.Tool{
		Name: name, Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		output, err := run(ctx, input)
		return nil, output, err
	})
}

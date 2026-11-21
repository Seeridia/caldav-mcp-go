package calendar

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("calendar or event not found")
	ErrConflict = errors.New("event changed on the server; read it again before retrying")
	ErrBackend  = errors.New("CalDAV request failed")
)

type Calendar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Event struct {
	CalendarID   string `json:"calendar_id"`
	UID          string `json:"uid"`
	RecurrenceID string `json:"recurrence_id,omitempty"`
	Summary      string `json:"summary"`
	Start        string `json:"start"`
	End          string `json:"end"`
	AllDay       bool   `json:"all_day"`
	Busy         bool   `json:"busy"`
	Description  string `json:"description,omitempty"`
	Location     string `json:"location,omitempty"`
	Status       string `json:"status,omitempty"`
	Reminders    []int  `json:"reminders"`
}

type EventRef struct {
	CalendarID string `json:"calendar_id" jsonschema:"Calendar ID from list_calendars"`
	UID        string `json:"uid" jsonschema:"Event UID"`
}

type CreateInput struct {
	CalendarID  string `json:"calendar_id" jsonschema:"Calendar ID from list_calendars"`
	Summary     string `json:"summary" jsonschema:"Event title"`
	Start       string `json:"start" jsonschema:"RFC3339 timestamp with offset or YYYY-MM-DD for an all-day event"`
	End         string `json:"end,omitempty" jsonschema:"Exclusive end in the same format as start; defaults to one hour or one day"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	Busy        *bool  `json:"busy,omitempty" jsonschema:"Blocks availability; defaults to true"`
	Reminders   []int  `json:"reminders,omitempty" jsonschema:"Display reminders in whole minutes before start; zero means at start"`
}

type UpdateInput struct {
	CalendarID  string  `json:"calendar_id" jsonschema:"Calendar ID from list_calendars"`
	UID         string  `json:"uid" jsonschema:"Event or recurring series UID"`
	Summary     *string `json:"summary,omitempty"`
	Start       *string `json:"start,omitempty" jsonschema:"RFC3339 timestamp or YYYY-MM-DD; changing only start retains duration"`
	End         *string `json:"end,omitempty" jsonschema:"Exclusive end; use the same format as start"`
	Description *string `json:"description,omitempty" jsonschema:"Pass an empty string to clear"`
	Location    *string `json:"location,omitempty" jsonschema:"Pass an empty string to clear"`
	Busy        *bool   `json:"busy,omitempty"`
	Reminders   *[]int  `json:"reminders,omitempty" jsonschema:"Replace display reminders in minutes before start; pass an empty array to clear"`
}

type EventTime struct {
	time.Time
	AllDay bool
}

type Changes struct {
	Summary, Description, Location *string
	Start, End                     *EventTime
	Busy                           *bool
	Reminders                      *[]int
}

type Occurrence struct {
	Event
	StartsAt, EndsAt time.Time
}

type Store interface {
	Calendars(context.Context) ([]Calendar, error)
	Occurrences(context.Context, string, time.Time, time.Time) ([]Occurrence, error)
	Event(context.Context, EventRef) (Event, error)
	Create(context.Context, string, Changes) (Event, error)
	Update(context.Context, EventRef, Changes) (Event, error)
	Delete(context.Context, EventRef) error
}

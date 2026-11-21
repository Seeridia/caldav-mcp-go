package calendar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const maxRange = 367 * 24 * time.Hour

type Service struct {
	store Store
	zone  *time.Location
}

func NewService(store Store, zone *time.Location) *Service {
	return &Service{store: store, zone: zone}
}

func (s *Service) Calendars(ctx context.Context) ([]Calendar, error) {
	return s.store.Calendars(ctx)
}

type ListInput struct {
	CalendarID string `json:"calendar_id" jsonschema:"Calendar ID from list_calendars"`
	Start      string `json:"start" jsonschema:"Inclusive RFC3339 timestamp or date in the default time zone"`
	End        string `json:"end" jsonschema:"Exclusive RFC3339 timestamp or date in the default time zone"`
	Query      string `json:"query,omitempty" jsonschema:"Case-insensitive text in title, location, or description"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum results; defaults to 100, up to 500"`
}

type ListResult struct {
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated"`
}

func (s *Service) List(ctx context.Context, in ListInput) (ListResult, error) {
	var out ListResult
	start, end, err := parseRange(in.Start, in.End, s.zone)
	if err != nil {
		return out, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return out, errors.New("limit must be between 1 and 500")
	}
	calendars, err := s.store.Calendars(ctx)
	if err != nil {
		return out, err
	}
	if !slices.ContainsFunc(calendars, func(c Calendar) bool { return c.ID == in.CalendarID }) {
		return out, ErrNotFound
	}
	events, err := s.store.Occurrences(ctx, in.CalendarID, start, end)
	if err != nil {
		return out, err
	}
	slices.SortStableFunc(events, func(a, b Occurrence) int { return a.StartsAt.Compare(b.StartsAt) })
	out.Events = []Event{}
	query := strings.ToLower(strings.TrimSpace(in.Query))
	for _, event := range events {
		if query != "" && !strings.Contains(strings.ToLower(event.Summary+"\n"+event.Location+"\n"+event.Description), query) {
			continue
		}
		if len(out.Events) == limit {
			out.Truncated = true
			break
		}
		out.Events = append(out.Events, event.Event)
	}
	return out, nil
}

func (s *Service) Event(ctx context.Context, ref EventRef) (Event, error) {
	return s.store.Event(ctx, ref)
}

func (s *Service) Create(ctx context.Context, in CreateInput) (Event, error) {
	start, err := parseTime(in.Start, s.zone)
	if err != nil {
		return Event{}, fmt.Errorf("start: %w", err)
	}
	end := EventTime{Time: start.Add(time.Hour), AllDay: start.AllDay}
	if start.AllDay {
		end.Time = start.AddDate(0, 0, 1)
	}
	if in.End != "" {
		end, err = parseTime(in.End, s.zone)
		if err != nil {
			return Event{}, fmt.Errorf("end: %w", err)
		}
	}
	if err := ValidateTimes(start, end); err != nil {
		return Event{}, err
	}
	reminders, err := normalizeReminders(&in.Reminders)
	if err != nil {
		return Event{}, err
	}
	in.Summary = strings.TrimSpace(in.Summary)
	if in.Summary == "" {
		return Event{}, errors.New("summary is required")
	}
	return s.store.Create(ctx, in.CalendarID, Changes{
		Summary: &in.Summary, Start: &start, End: &end, Description: &in.Description,
		Location: &in.Location, Busy: in.Busy, Reminders: reminders,
	})
}

func (s *Service) Update(ctx context.Context, in UpdateInput) (Event, error) {
	start, err := s.optionalTime("start", in.Start)
	if err != nil {
		return Event{}, err
	}
	end, err := s.optionalTime("end", in.End)
	if err != nil {
		return Event{}, err
	}
	reminders, err := normalizeReminders(in.Reminders)
	if err != nil {
		return Event{}, err
	}
	if in.Summary != nil {
		summary := strings.TrimSpace(*in.Summary)
		if summary == "" {
			return Event{}, errors.New("summary cannot be empty")
		}
		in.Summary = &summary
	}
	ref := EventRef{CalendarID: in.CalendarID, UID: in.UID}
	if start == nil && end == nil && in.Summary == nil && in.Description == nil && in.Location == nil && in.Busy == nil && reminders == nil {
		return s.Event(ctx, ref)
	}
	return s.store.Update(ctx, ref, Changes{
		Summary: in.Summary, Start: start, End: end, Description: in.Description,
		Location: in.Location, Busy: in.Busy, Reminders: reminders,
	})
}

func (s *Service) Delete(ctx context.Context, ref EventRef) error {
	return s.store.Delete(ctx, ref)
}

func parseTime(value string, zone *time.Location) (EventTime, error) {
	if t, err := time.ParseInLocation(time.DateOnly, value, zone); err == nil {
		return EventTime{Time: t, AllDay: true}, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil && t.Nanosecond() == 0 {
		return EventTime{Time: t}, nil
	}
	return EventTime{}, errors.New("use YYYY-MM-DD or an RFC3339 timestamp with offset and whole seconds")
}

func (s *Service) optionalTime(name string, value *string) (*EventTime, error) {
	if value == nil {
		return nil, nil
	}
	t, err := parseTime(*value, s.zone)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &t, nil
}

func parseRange(from, to string, zone *time.Location) (time.Time, time.Time, error) {
	start, err := parseTime(from, zone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseTime(to, zone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	if !start.Before(end.Time) || end.Sub(start.Time) > maxRange {
		return time.Time{}, time.Time{}, errors.New("range must have a positive duration of at most 367 days")
	}
	return start.Time, end.Time, nil
}

func ValidateTimes(start, end EventTime) error {
	if start.AllDay != end.AllDay || !start.Before(end.Time) {
		return errors.New("start and end must use the same format and end must follow start")
	}
	return nil
}

func normalizeReminders(value *[]int) (*[]int, error) {
	if value == nil {
		return nil, nil
	}
	if len(*value) > 20 {
		return nil, errors.New("at most 20 reminders are allowed")
	}
	minutes := slices.Clone(*value)
	for _, minute := range minutes {
		if minute < 0 || minute > 525600 {
			return nil, errors.New("reminders must be between 0 and 525600 minutes before start")
		}
	}
	slices.Sort(minutes)
	minutes = slices.Compact(minutes)
	return &minutes, nil
}

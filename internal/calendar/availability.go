package calendar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

type AvailabilityInput struct {
	CalendarIDs     []string `json:"calendar_ids" jsonschema:"Calendar IDs to check together"`
	Start           string   `json:"start" jsonschema:"Inclusive RFC3339 timestamp or local date"`
	End             string   `json:"end" jsonschema:"Exclusive RFC3339 timestamp or local date"`
	DurationMinutes int      `json:"duration_minutes" jsonschema:"Minimum length of a candidate slot in minutes"`
	WorkStart       string   `json:"work_start,omitempty" jsonschema:"Local HH:MM; defaults to 09:00"`
	WorkEnd         string   `json:"work_end,omitempty" jsonschema:"Local HH:MM; defaults to 17:00"`
	WeekdaysOnly    bool     `json:"weekdays_only,omitempty" jsonschema:"Restrict the search to Monday through Friday"`
	TimeZone        string   `json:"time_zone,omitempty" jsonschema:"IANA time zone for work hours and date-only inputs"`
	Limit           int      `json:"limit,omitempty" jsonschema:"Maximum slots to return; defaults to 10"`
}

type Slot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type Availability struct {
	Slots                []Slot   `json:"slots"`
	Complete             bool     `json:"complete"`
	UnavailableCalendars []string `json:"unavailable_calendars,omitempty"`
}

func (s *Service) Available(ctx context.Context, in AvailabilityInput) (Availability, error) {
	out := Availability{Slots: []Slot{}}
	zone := s.zone
	if in.TimeZone != "" && in.TimeZone != "Local" {
		var err error
		zone, err = time.LoadLocation(in.TimeZone)
		if err != nil {
			return out, errors.New("time_zone must be a valid IANA time zone")
		}
	}
	if in.TimeZone == "Local" {
		return out, errors.New("time_zone must be an explicit IANA time zone")
	}
	start, end, err := parseRange(in.Start, in.End, zone)
	if err != nil {
		return out, err
	}
	if len(in.CalendarIDs) < 1 || len(in.CalendarIDs) > 20 || in.DurationMinutes < 1 || in.DurationMinutes > 1440 {
		return out, errors.New("provide 1 to 20 calendar IDs and a duration of 1 to 1440 minutes")
	}
	workStart, workEnd, err := workHours(in.WorkStart, in.WorkEnd)
	if err != nil {
		return out, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 100 {
		return out, errors.New("limit must be between 1 and 100")
	}
	read, err := s.store.OccurrenceReader(ctx)
	if err != nil {
		return out, err
	}
	busy := []Slot{}
	seen := map[string]bool{}
	for _, id := range in.CalendarIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		events, err := read(ctx, id, start, end)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.UnavailableCalendars = append(out.UnavailableCalendars, id)
			continue
		}
		for _, event := range events {
			if event.Busy && event.StartsAt.Before(event.EndsAt) {
				busy = append(busy, Slot{Start: event.StartsAt, End: event.EndsAt})
			}
		}
	}
	out.Complete = len(out.UnavailableCalendars) == 0
	if !out.Complete {
		return out, nil
	}
	busy = mergeSlots(busy)
	local := start.In(zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	duration := time.Duration(in.DurationMinutes) * time.Minute
	for ; day.Before(end) && len(out.Slots) < limit; day = day.AddDate(0, 0, 1) {
		if in.WeekdaysOnly && (day.Weekday() == time.Saturday || day.Weekday() == time.Sunday) {
			continue
		}
		from, to := timeOnDay(day, workStart), timeOnDay(day, workEnd)
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		out.Slots = appendSlots(out.Slots, busy, from, to, duration, limit, zone)
	}
	return out, nil
}

func workHours(from, to string) (time.Time, time.Time, error) {
	if from == "" {
		from = "09:00"
	}
	if to == "" {
		to = "17:00"
	}
	start, err1 := time.Parse("15:04", from)
	end, err2 := time.Parse("15:04", to)
	if err1 != nil || err2 != nil || !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("work_start and work_end must be ordered HH:MM values on the same day")
	}
	return start, end, nil
}

func timeOnDay(day, clock time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, day.Location())
}

func mergeSlots(slots []Slot) []Slot {
	slices.SortFunc(slots, func(a, b Slot) int { return a.Start.Compare(b.Start) })
	merged := slots[:0]
	for _, slot := range slots {
		if len(merged) == 0 || merged[len(merged)-1].End.Before(slot.Start) {
			merged = append(merged, slot)
		} else if slot.End.After(merged[len(merged)-1].End) {
			merged[len(merged)-1].End = slot.End
		}
	}
	return merged
}

func appendSlots(out, busy []Slot, start, end time.Time, duration time.Duration, limit int, zone *time.Location) []Slot {
	add := func(from, to time.Time) {
		for len(out) < limit && !from.Add(duration).After(to) {
			next := from.Add(duration)
			out = append(out, Slot{Start: from.In(zone), End: next.In(zone)})
			from = next
		}
	}
	first := sort.Search(len(busy), func(i int) bool { return busy[i].End.After(start) })
	for _, slot := range busy[first:] {
		if !slot.Start.Before(end) {
			break
		}
		add(start, slot.Start)
		start = slot.End
		if len(out) == limit || !start.Before(end) {
			return out
		}
	}
	add(start, end)
	return out
}

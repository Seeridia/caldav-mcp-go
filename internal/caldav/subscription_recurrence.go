package caldav

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

const maxSubscriptionInstances = 100_000

type subscriptionInstance struct {
	uid string
	at  time.Time
}

func (c *Client) subscriptionOccurrences(ctx context.Context, collection collection, start, end time.Time) ([]calendar.Occurrence, error) {
	document, err := c.subscriptionDocument(ctx, collection)
	if err != nil {
		return nil, err
	}
	result := []calendar.Occurrence{}
	masters := []ical.Event{}
	overrides := map[subscriptionInstance]bool{}
	cancelled := map[string]bool{}
	appendInWindow := func(event calendar.Occurrence) {
		if inWindow(event, start, end) {
			result = append(result, event)
		}
	}
	events := document.Events()
	for _, event := range events {
		if event.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		if subscriptionCancelled(event) {
			cancelled[text(event.Props, ical.PropUID)] = true
		} else {
			masters = append(masters, event)
		}
	}
	for _, event := range events {
		prop := event.Props.Get(ical.PropRecurrenceID)
		uid := text(event.Props, ical.PropUID)
		if prop == nil || cancelled[uid] {
			continue
		}
		if prop.Params.Get("RANGE") != "" {
			return nil, fmt.Errorf("%w: subscription overrides with RANGE are unsupported", calendar.ErrBackend)
		}
		at, err := decodeDateTime(prop, c.zone)
		if err != nil {
			return nil, dateTimeError("decode subscription recurrence ID", prop, err)
		}
		key := subscriptionInstance{uid: uid, at: at.UTC()}
		if overrides[key] {
			return nil, fmt.Errorf("%w: duplicate subscription recurrence override", calendar.ErrBackend)
		}
		overrides[key] = true
		if !subscriptionCancelled(event) {
			occurrence, err := c.decodeEvent(collection.ID, event)
			if err != nil {
				return nil, err
			}
			appendInWindow(occurrence)
		}
	}
	remaining := maxSubscriptionInstances
	for _, event := range masters {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		base, err := c.decodeEvent(collection.ID, event)
		if err != nil {
			return nil, err
		}
		if event.Props.Get("EXRULE") != nil {
			return nil, fmt.Errorf("%w: subscription EXRULE is unsupported", calendar.ErrBackend)
		}
		if !isSeries(event) && event.Props.Get(ical.PropExceptionDates) == nil {
			if !overrides[subscriptionInstance{uid: base.UID, at: base.StartsAt.UTC()}] {
				appendInWindow(base)
			}
			continue
		}
		set, err := subscriptionRecurrence(event, base.StartsAt, end, c.zone)
		if err != nil {
			return nil, err
		}
		next := set.Iterator()
		for at, ok := next(); ok && at.Before(end); at, ok = next() {
			remaining--
			if remaining < 0 {
				return nil, fmt.Errorf("%w: subscription recurrence expansion limit exceeded", calendar.ErrBackend)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !overrides[subscriptionInstance{uid: base.UID, at: at.UTC()}] {
				appendInWindow(subscriptionOccurrenceAt(base, at.In(base.StartsAt.Location())))
			}
		}
	}
	return result, nil
}

func subscriptionCancelled(event ical.Event) bool {
	return strings.EqualFold(raw(event.Props, ical.PropStatus), "CANCELLED")
}

func subscriptionRecurrence(event ical.Event, start, end time.Time, zone *time.Location) (*rrule.Set, error) {
	var set rrule.Set
	set.RDate(start)
	if prop := event.Props.Get(ical.PropRecurrenceRule); prop != nil && start.Before(end) {
		option, err := rrule.StrToROptionInLocation(prop.Value, start.Location())
		if err != nil {
			return nil, backend("decode subscription recurrence rule", err)
		}
		combinations := max(1, len(option.Byhour)) * max(1, len(option.Byminute)) * max(1, len(option.Bysecond))
		if option.Freq > rrule.HOURLY || combinations > 96 || option.Interval < 0 || option.Interval > 10_000 {
			return nil, fmt.Errorf("%w: subscription recurrence rule exceeds supported frequency", calendar.ErrBackend)
		}
		option.Dtstart = start
		if option.Until.IsZero() || option.Until.After(end) {
			option.Until = end
		}
		rule, err := rrule.NewRRule(*option)
		if err != nil {
			return nil, backend("build subscription recurrence rule", err)
		}
		set.RRule(rule)
	}
	for _, name := range []string{ical.PropRecurrenceDates, ical.PropExceptionDates} {
		for _, prop := range event.Props[name] {
			for value := range strings.SplitSeq(prop.Value, ",") {
				date := prop
				date.Value = value
				at, err := decodeDateTime(&date, zone)
				if err != nil {
					return nil, dateTimeError("decode subscription recurrence date", &date, err)
				}
				if name == ical.PropRecurrenceDates {
					set.RDate(at)
				} else {
					set.ExDate(at)
				}
			}
		}
	}
	return &set, nil
}

func subscriptionOccurrenceAt(base calendar.Occurrence, at time.Time) calendar.Occurrence {
	layout := time.RFC3339
	end := at.Add(base.EndsAt.Sub(base.StartsAt))
	if base.AllDay {
		layout = time.DateOnly
		// Compute calendar-day length without counting daylight-saving changes.
		startDate, _ := time.Parse(time.DateOnly, base.Start)
		endDate, _ := time.Parse(time.DateOnly, base.End)
		end = at.AddDate(0, 0, int(endDate.Sub(startDate)/(24*time.Hour)))
	}
	base.StartsAt, base.EndsAt = at, end
	base.Start, base.End = at.Format(layout), end.Format(layout)
	base.RecurrenceID = base.Start
	return base
}

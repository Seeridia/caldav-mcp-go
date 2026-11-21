package caldav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Seeridia/caldav-mcp-go/internal/calendar"
	"github.com/emersion/go-ical"
)

func (c *Client) Event(ctx context.Context, ref calendar.EventRef) (calendar.Event, error) {
	resource, err := c.load(ctx, ref)
	if err != nil {
		return calendar.Event{}, err
	}
	result, err := c.decodeEvent(ref.CalendarID, resource.event)
	return result.Event, err
}

func (c *Client) Create(ctx context.Context, id string, changes calendar.Changes) (calendar.Event, error) {
	collection, err := c.calendarPath(ctx, id)
	if err != nil {
		return calendar.Event{}, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return calendar.Event{}, err
	}
	uid := hex.EncodeToString(token)
	document := ical.NewCalendar()
	document.Props.SetText(ical.PropProductID, "-//caldav-mcp-go//EN")
	document.Props.SetText(ical.PropVersion, "2.0")
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	applyChanges(*event, changes)
	result, err := c.decodeEvent(id, *event)
	if err != nil {
		return calendar.Event{}, err
	}
	document.Children = append(document.Children, event.Component)
	if err := c.conditionalWrite(ctx, http.MethodPut, path.Join(collection, uid+".ics"), "*", document); err != nil {
		return calendar.Event{}, err
	}
	return result.Event, nil
}

func (c *Client) Update(ctx context.Context, ref calendar.EventRef, changes calendar.Changes) (calendar.Event, error) {
	resource, err := c.load(ctx, ref)
	if err != nil {
		return calendar.Event{}, err
	}
	if changes.Start != nil || changes.End != nil {
		if isSeries(resource.event) || len(resource.document.Events()) > 1 {
			return calendar.Event{}, errors.New("changing the time of a recurring series is unsupported")
		}
		if err := c.completeTimes(resource.event, &changes); err != nil {
			return calendar.Event{}, err
		}
	}
	applyChanges(resource.event, changes)
	result, err := c.decodeEvent(ref.CalendarID, resource.event)
	if err != nil {
		return calendar.Event{}, err
	}
	if err := c.conditionalWrite(ctx, http.MethodPut, resource.path, resource.etag, resource.document); err != nil {
		return calendar.Event{}, err
	}
	return result.Event, nil
}

func (c *Client) Delete(ctx context.Context, ref calendar.EventRef) error {
	resource, err := c.load(ctx, ref)
	if err != nil {
		return err
	}
	return c.conditionalWrite(ctx, http.MethodDelete, resource.path, resource.etag, nil)
}

func (c *Client) completeTimes(event ical.Event, changes *calendar.Changes) error {
	oldStart, oldEnd, err := c.eventTimes(event)
	if err != nil {
		return err
	}
	start, end := oldStart, oldEnd
	if changes.Start != nil {
		start = *changes.Start
	}
	if changes.End != nil {
		end = *changes.End
	} else if start.AllDay == oldStart.AllDay {
		if start.AllDay {
			days := int(oldEnd.Sub(oldStart.Time).Round(24*time.Hour) / (24 * time.Hour))
			end.Time = start.AddDate(0, 0, days)
		} else {
			end.Time = start.Add(oldEnd.Sub(oldStart.Time))
		}
	}
	if err := calendar.ValidateTimes(start, end); err != nil {
		return err
	}
	changes.End = &end
	return nil
}

func (c *Client) eventTimes(event ical.Event) (calendar.EventTime, calendar.EventTime, error) {
	var start, end calendar.EventTime
	first := event.Props.Get(ical.PropDateTimeStart)
	if first == nil {
		return start, end, fmt.Errorf("%w: event has no start time", calendar.ErrBackend)
	}
	start.AllDay = first.ValueType() == ical.ValueDate || len(first.Value) == 8
	end.AllDay = start.AllDay
	var err error
	start.Time, err = first.DateTime(c.zone)
	if err != nil {
		return start, end, backend("decode event start", err)
	}
	end.Time, err = event.DateTimeEnd(c.zone)
	if err != nil {
		return start, end, backend("decode event end", err)
	}
	if start.AllDay && event.Props.Get(ical.PropDateTimeEnd) == nil {
		days := 1
		if prop := event.Props.Get(ical.PropDuration); prop != nil {
			duration, err := prop.Duration()
			if err != nil || duration < 0 || duration%(24*time.Hour) != 0 {
				return start, end, fmt.Errorf("%w: invalid all-day duration", calendar.ErrBackend)
			}
			days = int(duration / (24 * time.Hour))
		}
		end.Time = start.AddDate(0, 0, days)
	}
	if end.Before(start.Time) {
		return start, end, fmt.Errorf("%w: event ends before it starts", calendar.ErrBackend)
	}
	return start, end, nil
}

func (c *Client) decodeEvent(id string, event ical.Event) (calendar.Occurrence, error) {
	start, end, err := c.eventTimes(event)
	if err != nil {
		return calendar.Occurrence{}, err
	}
	layout := time.RFC3339
	if start.AllDay {
		layout = time.DateOnly
	}
	status := strings.ToUpper(raw(event.Props, ical.PropStatus))
	recurrence := ""
	if prop := event.Props.Get(ical.PropRecurrenceID); prop != nil {
		occurrence, err := prop.DateTime(c.zone)
		if err != nil {
			return calendar.Occurrence{}, backend("decode recurrence ID", err)
		}
		recurrence = occurrence.Format(layout)
	}
	result := calendar.Event{
		CalendarID: id, UID: text(event.Props, ical.PropUID), RecurrenceID: recurrence,
		Summary: text(event.Props, ical.PropSummary), Start: start.Format(layout), End: end.Format(layout),
		AllDay: start.AllDay, Busy: status != "CANCELLED" && !strings.EqualFold(raw(event.Props, ical.PropTransparency), "TRANSPARENT"),
		Description: text(event.Props, ical.PropDescription), Location: text(event.Props, ical.PropLocation),
		Status: status, Reminders: []int{},
	}
	for _, component := range event.Children {
		if minute, ok := displayReminder(component); ok {
			result.Reminders = append(result.Reminders, minute)
		}
	}
	slices.Sort(result.Reminders)
	return calendar.Occurrence{
		Event: result, StartsAt: start.Time, EndsAt: end.Time,
	}, nil
}

func applyChanges(event ical.Event, changes calendar.Changes) {
	setText(event.Props, ical.PropSummary, changes.Summary)
	setText(event.Props, ical.PropDescription, changes.Description)
	setText(event.Props, ical.PropLocation, changes.Location)
	if changes.Start != nil {
		setTime(event.Props, ical.PropDateTimeStart, *changes.Start)
	}
	if changes.End != nil {
		event.Props.Del(ical.PropDuration)
		setTime(event.Props, ical.PropDateTimeEnd, *changes.End)
	}
	if changes.Busy != nil {
		value := "OPAQUE"
		if !*changes.Busy {
			value = "TRANSPARENT"
		}
		event.Props.SetText(ical.PropTransparency, value)
	}
	if changes.Reminders != nil {
		event.Children = slices.DeleteFunc(event.Children, func(component *ical.Component) bool {
			_, managed := displayReminder(component)
			return managed
		})
		for _, minute := range *changes.Reminders {
			alarm := ical.NewComponent(ical.CompAlarm)
			alarm.Props.SetText(ical.PropAction, "DISPLAY")
			alarm.Props.SetText(ical.PropDescription, text(event.Props, ical.PropSummary))
			trigger := ical.NewProp(ical.PropTrigger)
			trigger.SetDuration(-time.Duration(minute) * time.Minute)
			alarm.Props.Set(trigger)
			event.Children = append(event.Children, alarm)
		}
	}
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
}

func displayReminder(component *ical.Component) (int, bool) {
	if component.Name != ical.CompAlarm || !strings.EqualFold(raw(component.Props, ical.PropAction), "DISPLAY") {
		return 0, false
	}
	trigger := component.Props.Get(ical.PropTrigger)
	if trigger == nil || component.Props.Get(ical.PropRepeat) != nil {
		return 0, false
	}
	related := strings.ToUpper(trigger.Params.Get("RELATED"))
	if related != "" && related != "START" {
		return 0, false
	}
	delay, err := trigger.Duration()
	if err != nil || delay > 0 || delay%time.Minute != 0 {
		return 0, false
	}
	return int(-(delay / time.Minute)), true
}

func setTime(props ical.Props, name string, value calendar.EventTime) {
	if value.AllDay {
		props.SetDate(name, value.Time)
	} else {
		props.SetDateTime(name, value.UTC())
	}
}

func setText(props ical.Props, name string, value *string) {
	if value == nil {
		return
	}
	if *value == "" {
		props.Del(name)
	} else {
		props.SetText(name, *value)
	}
}

func text(props ical.Props, name string) string {
	value, _ := props.Text(name)
	return value
}

func raw(props ical.Props, name string) string {
	if property := props.Get(name); property != nil {
		return property.Value
	}
	return ""
}

func isSeries(event ical.Event) bool {
	return event.Props.Get(ical.PropRecurrenceRule) != nil || event.Props.Get(ical.PropRecurrenceDates) != nil
}

package mailkit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// Calendar invitations in email (iTIP, RFC 5546): read shows what the invitation is for, and
// respond_invite answers it (accept, maybe, decline) with a reply the organizer's calendar understands.
// The answer is an email, so the user approves it like any other.

type invite struct {
	Method    string `json:"method"` // REQUEST (an invitation), CANCEL, REPLY, …
	Title     string `json:"title,omitempty"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	AllDay    bool   `json:"all_day,omitempty"`
	Location  string `json:"location,omitempty"`
	Organizer string `json:"organizer,omitempty"`
	Attendees int    `json:"attendees,omitempty"`
	Recurring bool   `json:"recurring,omitempty"`
	Note      string `json:"note,omitempty"`

	cal *ical.Calendar
}

// parseInvite reads a text/calendar part.
func parseInvite(data []byte) *invite {
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil
	}
	inv := &invite{cal: cal, Method: strings.ToUpper(propText(cal.Component, ical.PropMethod))}
	for _, ev := range cal.Events() {
		inv.Title = propText(ev.Component, ical.PropSummary)
		inv.Location = propText(ev.Component, ical.PropLocation)
		if p := ev.Props.Get(ical.PropDateTimeStart); p != nil {
			inv.AllDay = p.ValueType() == ical.ValueDate
		}
		if t, err := ev.DateTimeStart(time.Local); err == nil {
			inv.Start = stampTime(t, inv.AllDay)
		}
		if t, err := ev.DateTimeEnd(time.Local); err == nil {
			inv.End = stampTime(t, inv.AllDay)
		}
		if o := ev.Props.Get(ical.PropOrganizer); o != nil {
			inv.Organizer = strings.TrimPrefix(strings.ToLower(o.Value), "mailto:")
		}
		inv.Attendees = len(ev.Props.Values(ical.PropAttendee))
		inv.Recurring = ev.Props.Get(ical.PropRecurrenceRule) != nil
		break
	}
	if inv.Method == "" && inv.Title == "" {
		return nil
	}
	if inv.Method == "REQUEST" {
		inv.Note = "An invitation: answer with " + tool("respond_invite") + ". Check the user's calendar for conflicts first if you can."
	}
	return inv
}

func propText(c *ical.Component, name string) string {
	if p := c.Props.Get(name); p != nil {
		v, _ := p.Text()
		return strings.TrimSpace(v)
	}
	return ""
}

func stampTime(t time.Time, allDay bool) string {
	if allDay {
		return t.Format("2006-01-02")
	}
	return t.Format(time.RFC3339)
}

type inviteIn struct {
	UID      uint32 `json:"uid" jsonschema:"the email with the invitation"`
	Mailbox  string `json:"mailbox,omitempty" jsonschema:"its folder, default INBOX"`
	Response string `json:"response" jsonschema:"accept, maybe or decline"`
	Comment  string `json:"comment,omitempty" jsonschema:"a short note to the organizer"`
	Account  string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

var partstat = map[string][2]string{"accept": {"ACCEPTED", "Accepted"}, "maybe": {"TENTATIVE", "Tentative"},
	"tentative": {"TENTATIVE", "Tentative"}, "decline": {"DECLINED", "Declined"}}

func (x *integration) respondInvite(ctx context.Context, h host, in inviteIn) (any, error) {
	ps, ok := partstat[strings.ToLower(strings.TrimSpace(in.Response))]
	if !ok {
		return nil, errors.New("response is accept, maybe or decline")
	}
	c, s, err := session(h)
	if err != nil {
		return nil, err
	}
	sp, err := findSpecials(c, s)
	if err != nil {
		logout(c)
		return nil, err
	}
	box := sp.resolve(in.Mailbox)
	if err := x.folderAllowed(h, box); err != nil {
		logout(c)
		return nil, err
	}
	raw, err := fetchRaw(c, box, in.UID)
	logout(c)
	if err != nil {
		return nil, err
	}
	m, err := parseMessage(raw, 0)
	if err != nil {
		return nil, err
	}
	if hidden, _ := privacyOf(h).hidden(m.From, m.Subject, m.Text); hidden {
		return nil, errors.New("that email is private (the user's privacy filter)")
	}
	if m.Invite == nil || m.Invite.Method != "REQUEST" {
		return nil, errors.New("this email has no invitation to answer")
	}
	reply, organizer, err := inviteReply(m.Invite.cal, s, ps[0], strings.TrimSpace(in.Comment))
	if err != nil {
		return nil, err
	}
	body := map[string]string{"ACCEPTED": "I'll be there.", "TENTATIVE": "I might be there.", "DECLINED": "I can't make it."}[ps[0]]
	if in.Comment != "" {
		body = strings.TrimSpace(in.Comment)
	}
	d := draft{To: []string{organizer}, Subject: ps[1] + ": " + m.Invite.Title, Body: body, calendar: reply}
	msg, err := compose(s, d, "", strings.TrimSpace(m.References+" "+m.MessageID))
	if err != nil {
		return nil, err
	}
	return x.askSend(ctx, h, s, msg, body+"\n\n("+strings.ToLower(ps[1])+" in the organizer's calendar: "+m.Invite.Title+", "+m.Invite.Start+")", 0, 0)
}

// inviteReply builds the iTIP REPLY: the event's identity and times, and the user as attendee with their answer.
func inviteReply(cal *ical.Calendar, s Settings, status, comment string) (string, string, error) {
	evs := cal.Events()
	if len(evs) == 0 {
		return "", "", errors.New("the invitation has no event")
	}
	ev := evs[0]
	org := ev.Props.Get(ical.PropOrganizer)
	if org == nil {
		return "", "", errors.New("the invitation names no organizer")
	}
	organizer := strings.TrimPrefix(strings.TrimPrefix(org.Value, "mailto:"), "MAILTO:")
	if !strings.Contains(organizer, "@") {
		return "", "", errors.New("the organizer has no email address")
	}
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropProductID, "-//Rubi//Mail//EN")
	out.Props.SetText(ical.PropVersion, "2.0")
	out.Props.SetText(ical.PropMethod, "REPLY")
	re := ical.NewEvent()
	for _, name := range []string{ical.PropUID, ical.PropSequence, ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropDuration,
		ical.PropRecurrenceID, ical.PropSummary, ical.PropOrganizer} {
		if p := ev.Props.Get(name); p != nil {
			re.Props.Set(p)
		}
	}
	if re.Props.Get(ical.PropUID) == nil {
		return "", "", errors.New("the invitation has no UID")
	}
	re.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	me := ical.NewProp(ical.PropAttendee)
	me.Value = "mailto:" + s.Address
	for _, a := range ev.Props.Values(ical.PropAttendee) { // keep how the organizer wrote the user's address
		if strings.EqualFold(strings.TrimPrefix(strings.ToLower(a.Value), "mailto:"), s.Address) {
			me.Value = a.Value
		}
	}
	me.Params.Set(ical.ParamParticipationStatus, status)
	if s.FromName != "" {
		me.Params.Set(ical.ParamCommonName, s.FromName)
	}
	re.Props.Set(me)
	if comment != "" {
		re.Props.SetText(ical.PropComment, comment)
	}
	out.Children = append(out.Children, re.Component)
	var b bytes.Buffer
	if err := ical.NewEncoder(&b).Encode(out); err != nil {
		return "", "", err
	}
	return b.String(), organizer, nil
}

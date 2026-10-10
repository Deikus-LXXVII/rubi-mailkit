package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// The plugin's page in the Rubi panel: emails waiting to go out (with Cancel, the undo of undo send),
// snoozed mail, and the rules for new mail. Only the signed-in user reaches it, never the agent, so its
// buttons act without asking.

type panelArgs struct {
	Account string `json:"account"`
	ID      string `json:"id"`
}

func registerPanel(p *rubiplugin.Plugin, x *integration) {
	p.OnPanel = func(ctx context.Context, h *rubiplugin.Host, op string, raw json.RawMessage) (any, error) {
		return x.panel(h, op, raw)
	}
}

func (x *integration) panel(b base, op string, raw json.RawMessage) (any, error) {
	if op == "view" {
		return x.panelView(b)
	}
	var in panelArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
	}
	a, err := accountFor(b, in.Account)
	if err != nil {
		return nil, err
	}
	switch op {
	case "cancel_send":
		return x.cancelSend(a, in.ID)
	case "unsnooze":
		x.mu.Lock()
		st, err := x.loadState(a)
		found := false
		if err == nil {
			for _, s := range st.Snoozed {
				if s.ID == in.ID {
					s.Until, found = time.Now().UTC(), true
				}
			}
			if found {
				err = a.SaveState(st)
			}
		}
		x.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("it's back already")
		}
		x.kickJobs()
		return map[string]any{"status": "coming_back"}, nil
	case "remove_rule":
		return x.changeRules(a, ruleIn{Op: "remove", RuleID: in.ID})
	}
	return nil, fmt.Errorf("unknown page action %q", op)
}

type viewAction struct {
	Label   string    `json:"label"`
	Op      string    `json:"op"`
	Args    panelArgs `json:"args"`
	Danger  bool      `json:"danger,omitempty"`
	Confirm string    `json:"confirm,omitempty"`
}

type viewItem struct {
	Title   string       `json:"title"`
	Detail  string       `json:"detail,omitempty"`
	Actions []viewAction `json:"actions,omitempty"`
}

type viewSection struct {
	Title string     `json:"title"`
	Note  string     `json:"note,omitempty"`
	Empty string     `json:"empty,omitempty"`
	Items []viewItem `json:"items"`
}

func (x *integration) panelView(b base) (any, error) {
	list, err := accounts(b)
	if err != nil {
		return nil, err
	}
	var sends, snz, rules []viewItem
	refresh := 0
	now := time.Now()
	for _, a := range list {
		st, err := x.loadState(a)
		if err != nil {
			return nil, err
		}
		who := ""
		if len(list) > 1 {
			who = a.label + " · "
		}
		for _, q := range st.Queued {
			left := q.SendAt.Sub(now).Round(time.Second)
			when := "goes out at " + q.SendAt.Local().Format("Mon 2 Jan 15:04:05")
			if left < time.Hour {
				when = fmt.Sprintf("goes out in %s", max(left, 0))
				refresh = 3
			}
			sends = append(sends, viewItem{Title: "To " + q.Mail.To + ": " + q.Mail.Subject, Detail: who + when,
				Actions: []viewAction{{Label: "Cancel", Op: "cancel_send", Args: panelArgs{a.id, q.ID}, Danger: true}}})
		}
		for _, s := range st.Snoozed {
			title := s.Subject
			if s.Count > 1 {
				title = fmt.Sprintf("%s and %d more", s.Subject, s.Count-1)
			}
			snz = append(snz, viewItem{Title: title, Detail: who + s.From + " · back " + s.Until.Local().Format("Mon 2 Jan 15:04"),
				Actions: []viewAction{{Label: "Bring back now", Op: "unsnooze", Args: panelArgs{a.id, s.ID}}}})
		}
		for _, r := range st.Rules {
			detail := r.describe()
			if r.Hits > 0 {
				detail += fmt.Sprintf(" · used %d times", r.Hits)
			}
			rules = append(rules, viewItem{Title: r.Name, Detail: who + detail,
				Actions: []viewAction{{Label: "Remove", Op: "remove_rule", Args: panelArgs{a.id, r.ID}, Danger: true,
					Confirm: "Remove the rule \"" + r.Name + "\"?"}}})
		}
	}
	return map[string]any{
		"title": prov.Name,
		"sections": []viewSection{
			{Title: "Waiting to send", Note: "Emails you approved wait a moment (Undo send in the settings) or until the time your agent chose.", Empty: "Nothing waiting.", Items: sends},
			{Title: "Snoozed", Empty: "No snoozed mail.", Items: snz},
			{Title: "Rules for new mail", Note: "Your agent proposes rules; you approve each.", Empty: "No rules.", Items: rules},
		},
		"refresh": refresh,
	}, nil
}

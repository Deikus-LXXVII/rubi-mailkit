package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Rules: what to do with new mail in the inbox, for any mail service (Gmail's own filters aren't reachable
// over IMAP). "Mail from accounting@: label Finance and tell me." The agent proposes a rule, the user
// approves it (a standing rule acts without asking again), and can remove it in the Rubi panel. Rules run
// when new mail arrives. Mail hidden by the privacy filter is never moved, labelled or marked; a rule may
// still say that such mail came (sender only).

type rule struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	From     []string  `json:"from,omitempty"`
	Words    []string  `json:"words,omitempty"`
	MoveTo   string    `json:"move_to,omitempty"`   // archive, trash, spam or a folder
	Label    string    `json:"label,omitempty"`     // Gmail: add a label (stays in the inbox)
	MarkRead bool      `json:"mark_read,omitempty"` //
	Star     bool      `json:"star,omitempty"`
	Notify   bool      `json:"notify,omitempty"` // wake the agent
	Note     string    `json:"note,omitempty"`   // what the agent should do then
	Agent    string    `json:"agent,omitempty"`  // who gets the notice
	Created  time.Time `json:"created"`
	Hits     int       `json:"hits,omitempty"`
	LastHit  time.Time `json:"last_hit,omitzero"`
}

type ruleIn struct {
	Op       string   `json:"op" jsonschema:"list, add or remove"`
	Name     string   `json:"name,omitempty" jsonschema:"for add: a short name, e.g. Invoices to Finance"`
	From     []string `json:"from,omitempty" jsonschema:"for add: sender addresses or domains; any of them"`
	Words    []string `json:"words,omitempty" jsonschema:"for add: words in the subject or text; any of them. With from, both must match"`
	MoveTo   string   `json:"move_to,omitempty" jsonschema:"for add: archive, trash, spam or a folder"`
	Label    string   `json:"label,omitempty" jsonschema:"for add (Gmail): a label to add"`
	MarkRead bool     `json:"mark_read,omitempty"`
	Star     bool     `json:"star,omitempty"`
	Notify   bool     `json:"notify,omitempty" jsonschema:"for add: wake you when it matches"`
	Note     string   `json:"note,omitempty" jsonschema:"for add with notify: what you should do then (shown back to you)"`
	Agent    string   `json:"agent,omitempty" jsonschema:"for add with notify: your Bot's name as connected to Rubi, so it wakes you"`
	RuleID   string   `json:"rule_id,omitempty" jsonschema:"for remove"`
	Account  string   `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

const maxRules = 50

func registerRules(p *rubiplugin.Plugin, x *integration) {
	addTool(p, "rules", "Rules for new mail in the inbox: from given senders and/or with given words, move it (archive, trash, spam, a folder), add a label (Gmail), mark it read or starred, and/or wake you. op list shows them; add and remove ask the user (a rule then acts on its own). Private mail is never moved or marked.",
		func(in ruleIn) string { return in.Account },
		func(ctx context.Context, a *account, in ruleIn) (any, error) { return x.rules(ctx, a, in) })
	onExecute(p, kindRules, func(ctx context.Context, a *account, _ string, payload json.RawMessage) (any, error) {
		var in ruleIn
		if err := json.Unmarshal(payload, &in); err != nil {
			return nil, err
		}
		return x.changeRules(a, in)
	})
}

func (r *rule) describe() string {
	var when, do []string
	if len(r.From) > 0 {
		when = append(when, "from "+strings.Join(r.From, " or "))
	}
	if len(r.Words) > 0 {
		when = append(when, "with \""+strings.Join(r.Words, "\" or \"")+"\"")
	}
	if r.MoveTo != "" {
		do = append(do, "move to "+r.MoveTo)
	}
	if r.Label != "" {
		do = append(do, "label "+r.Label)
	}
	if r.MarkRead {
		do = append(do, "mark read")
	}
	if r.Star {
		do = append(do, "star")
	}
	if r.Notify {
		do = append(do, "tell the agent")
	}
	return "Mail " + strings.Join(when, " ") + ": " + strings.Join(do, ", ")
}

func (x *integration) rules(ctx context.Context, h host, in ruleIn) (any, error) {
	switch strings.ToLower(strings.TrimSpace(in.Op)) {
	case "list", "":
		st, err := x.loadState(h)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range st.Rules {
			out = append(out, map[string]any{"rule_id": r.ID, "name": r.Name, "does": r.describe(), "hits": r.Hits})
		}
		return map[string]any{"rules": out}, nil
	case "add":
		in.Op = "add"
		r, err := ruleFrom(in)
		if err != nil {
			return nil, err
		}
		pv := map[string]any{"rule": r.Name, "does": r.describe()}
		if r.Note != "" {
			pv["agent_note"] = r.Note
		}
		if r.MoveTo == "trash" || r.MoveTo == "spam" {
			pv["warning"] = "Matching mail leaves the inbox without asking."
		}
		return h.Submit(ctx, rubiplugin.Request{Kind: kindRules, Summary: "Add the mail rule \"" + r.Name + "\"", Preview: pv, Payload: in,
			Options: []rubiplugin.Option{{Key: "allow", Label: "Add rule"}}})
	case "remove":
		in.Op = "remove"
		st, err := x.loadState(h)
		if err != nil {
			return nil, err
		}
		for _, r := range st.Rules {
			if r.ID == in.RuleID {
				return h.Submit(ctx, rubiplugin.Request{Kind: kindRules, Summary: "Remove the mail rule \"" + r.Name + "\"",
					Preview: map[string]any{"rule": r.Name, "does": r.describe()}, Payload: ruleIn{Op: "remove", RuleID: r.ID},
					Options: []rubiplugin.Option{{Key: "allow", Label: "Remove rule"}}})
			}
		}
		return nil, errors.New("no such rule; list them with op list")
	}
	return nil, errors.New("op is list, add or remove")
}

func ruleFrom(in ruleIn) (*rule, error) {
	r := &rule{Name: strings.TrimSpace(in.Name), From: clean(in.From), Words: clean(in.Words), MoveTo: strings.TrimSpace(in.MoveTo),
		Label: strings.TrimSpace(in.Label), MarkRead: in.MarkRead, Star: in.Star, Notify: in.Notify, Note: strings.TrimSpace(in.Note),
		Agent: strings.TrimSpace(in.Agent)}
	if r.Name == "" {
		return nil, errors.New("give the rule a name")
	}
	if len(r.From)+len(r.Words) == 0 {
		return nil, errors.New("say which mail: from and/or words")
	}
	if r.MoveTo == "" && r.Label == "" && !r.MarkRead && !r.Star && !r.Notify {
		return nil, errors.New("say what to do: move_to, label, mark_read, star and/or notify")
	}
	if r.Label != "" && !prov.Gmail {
		return nil, errors.New("labels are Gmail's; use move_to with a folder")
	}
	if len(r.Note) > 500 || len(r.Name) > 100 || len(r.From) > 20 || len(r.Words) > 20 {
		return nil, errors.New("the rule is too long")
	}
	return r, nil
}

func clean(list []string) []string {
	var out []string
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// changeRules runs an approved add or remove.
func (x *integration) changeRules(h host, in ruleIn) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	switch in.Op {
	case "add":
		r, err := ruleFrom(in)
		if err != nil {
			return nil, err
		}
		if len(st.Rules) >= maxRules {
			return nil, fmt.Errorf("at most %d rules", maxRules)
		}
		r.ID, r.Created = randomID("rule_"), time.Now().UTC()
		st.Rules = append(st.Rules, r)
		if err := h.SaveState(st); err != nil {
			return nil, err
		}
		h.Audit("rule_added", map[string]any{"rule_id": r.ID, "does": r.describe()})
		go x.kickJobs() // start listening for new mail (x.mu is held here)
		return map[string]any{"status": "added", "rule_id": r.ID, "does": r.describe()}, nil
	case "remove":
		for i, r := range st.Rules {
			if r.ID == in.RuleID {
				st.Rules = append(st.Rules[:i], st.Rules[i+1:]...)
				h.Audit("rule_removed", map[string]any{"rule_id": r.ID})
				return map[string]any{"status": "removed", "rule_id": r.ID}, h.SaveState(st)
			}
		}
		return nil, errors.New("that rule is gone already")
	}
	return nil, errors.New("unknown rule change")
}

// applyRules runs the rules on one new message in box. text is its text when it was fetched.
func (x *integration) applyRules(h host, c *imapclient.Client, sp *specials, box string, st *state, hd header, private bool, text string, now time.Time) {
	moved := false
	for _, r := range st.Rules {
		if hd.date.Before(r.Created.Add(-2*time.Minute)) && !hd.date.IsZero() {
			continue // only mail that arrived after the rule was made
		}
		if !(&watch{From: r.From, Keywords: r.Words}).matches(hd.from, hd.subject, text, private) {
			continue
		}
		r.Hits++
		r.LastHit = now.UTC()
		done := []string{}
		if !private && !moved {
			t := []target{{uid: imap.UID(hd.uid), id: normIDBr(hd.messageID), from: hd.from, subject: hd.subject}}
			if r.Star {
				if setFlag(c, box, uidSet(t), imap.FlagFlagged, true) == nil {
					done = append(done, "starred")
				}
			}
			if r.MarkRead {
				if setFlag(c, box, uidSet(t), imap.FlagSeen, true) == nil {
					done = append(done, "marked read")
				}
			}
			if r.Label != "" {
				if _, err := x.labelTargets(c, *sp, box, t, []string{r.Label}, nil); err == nil {
					done = append(done, "labelled "+r.Label)
				}
			}
			if r.MoveTo != "" {
				dest := sp.resolve(r.MoveTo)
				if _, err := x.moveTargets(c, *sp, box, t, dest); err == nil {
					done, moved = append(done, "moved to "+dest), true
				} else {
					h.Logf("rule %s couldn't move a message: %v", r.ID, err)
				}
			}
		}
		h.Audit("rule_matched", map[string]any{"rule_id": r.ID, "private": private})
		if !r.Notify {
			continue
		}
		msg := map[string]any{"mailbox": box, "uid": hd.uid, "from": hd.from, "subject": hd.subject, "date": hd.date, "done": done}
		if moved {
			msg["note"] = "It was moved, so its uid in " + box + " no longer applies; find it with " + tool("search") + "."
		}
		if private {
			msg = map[string]any{"mailbox": box, "uid": hd.uid, "from": senderOnly(hd.from), "date": hd.date, "private": true, "note": privateNote()}
		}
		_, _ = h.EmitTo(r.Agent, "rule", map[string]any{"rule": map[string]any{"id": r.ID, "name": r.Name, "note": r.Note}, "message": msg})
	}
}

func normIDBr(id string) string {
	if n := normID(id); n != "" {
		return "<" + n + ">"
	}
	return ""
}

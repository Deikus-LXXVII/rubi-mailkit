package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Bulk actions: "archive everything from:newsletter older_than:30d". The plugin finds the matches, shows
// the user how many and from whom, and acts on exactly those after one approval. New mail that arrives
// meanwhile isn't touched. Private mail is left out; the whole action undoes at once.

type bulkIn struct {
	Query   string `json:"query" jsonschema:"which mail, in Gmail's search syntax (as in search)"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"limit to one folder; default: as search"`
	Action  string `json:"action" jsonschema:"archive, trash, spam, move, label, unlabel, read, unread, star or unstar"`
	To      string `json:"to,omitempty" jsonschema:"for move: the folder"`
	Label   string `json:"label,omitempty" jsonschema:"for label and unlabel (Gmail)"`
	Max     int    `json:"max,omitempty" jsonschema:"at most this many emails, default 500, max 1000"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

// bulkSet is what the user approves: these messages, this action.
type bulkSet struct {
	Action string              `json:"action"`
	To     string              `json:"to,omitempty"`
	Label  string              `json:"label,omitempty"`
	Boxes  map[string][]uint32 `json:"boxes"`
}

const (
	maxBulk      = 1000
	bulkPeek     = 16 << 10 // the start of each message the privacy filter reads in a bulk action
	bulkPreviewN = 5
)

func registerBulk(p *rubiplugin.Plugin, x *integration) {
	addTool(p, "bulk", "Act on all mail a query finds (up to 1000): archive, trash, spam, move, label/unlabel (Gmail), read/unread, star/unstar. The user sees how many emails and from whom and approves once; exactly those are changed, private mail is skipped, and "+tool("undo")+" reverses it all.",
		func(in bulkIn) string { return in.Account },
		func(ctx context.Context, a *account, in bulkIn) (any, error) { return x.requestBulk(ctx, a, in) })
	onExecute(p, kindBulk, func(ctx context.Context, a *account, _ string, payload json.RawMessage) (any, error) {
		var set bulkSet
		if err := json.Unmarshal(payload, &set); err != nil {
			return nil, err
		}
		return x.runBulk(a, set)
	})
}

var bulkActions = map[string]string{"archive": "Archive", "trash": "Move to the trash", "spam": "Report as spam", "move": "Move",
	"label": "Label", "unlabel": "Remove the label from", "read": "Mark as read", "unread": "Mark as unread", "star": "Star", "unstar": "Unstar"}

func (x *integration) requestBulk(ctx context.Context, h host, in bulkIn) (any, error) {
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	verb, ok := bulkActions[in.Action]
	if !ok {
		return nil, errors.New("action is archive, trash, spam, move, label, unlabel, read, unread, star or unstar")
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, errors.New("give a query: which mail")
	}
	if in.Action == "move" && strings.TrimSpace(in.To) == "" {
		return nil, errors.New("say where to move them (to)")
	}
	if (in.Action == "label" || in.Action == "unlabel") && (!prov.Gmail || strings.TrimSpace(in.Label) == "") {
		return nil, errors.New("label and unlabel need a label, and work in Gmail")
	}
	limit := in.Max
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, maxBulk)
	c, s, err := session(h)
	if err != nil {
		return nil, err
	}
	defer logout(c)
	msgs, _, err := x.querySearch(h, c, s, searchQuery{Query: in.Query, Mailbox: in.Mailbox, max: limit})
	if err != nil {
		return nil, err
	}
	set := bulkSet{Action: in.Action, To: strings.TrimSpace(in.To), Label: strings.TrimSpace(in.Label), Boxes: map[string][]uint32{}}
	byBox := map[string][]summary{}
	for _, m := range msgs {
		byBox[m.Mailbox] = append(byBox[m.Mailbox], m)
	}
	p := privacyOf(h)
	senders := map[string]int{}
	var examples []string
	count, private := 0, 0
	for box, list := range byBox {
		hiddenBody := map[uint32]bool{}
		if p.any() {
			hiddenBody = peekPrivate(c, box, list, p)
		}
		for _, m := range list {
			if hidden, _ := p.hidden(m.From, m.Subject, ""); hidden || hiddenBody[m.UID] {
				private++
				continue
			}
			set.Boxes[box] = append(set.Boxes[box], m.UID)
			count++
			senders[senderOnly(m.From)]++
			if len(examples) < 3 {
				examples = append(examples, m.Subject)
			}
		}
	}
	if count == 0 {
		out := map[string]any{"status": "nothing_found", "message": "No mail matches (or only private mail)."}
		return out, nil
	}
	type sc struct {
		who string
		n   int
	}
	var top []sc
	for w, n := range senders {
		top = append(top, sc{w, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n || top[i].n == top[j].n && top[i].who < top[j].who })
	var who []string
	for i, t := range top {
		if i == bulkPreviewN {
			who = append(who, fmt.Sprintf("and %d more senders", len(top)-bulkPreviewN))
			break
		}
		who = append(who, fmt.Sprintf("%s (%d)", t.who, t.n))
	}
	target := ""
	switch in.Action {
	case "move":
		target = " to " + set.To
	case "label", "unlabel":
		target = " (" + set.Label + ")"
	}
	pv := map[string]any{"query": in.Query, "emails": count, "from": strings.Join(who, "\n"), "for_example": strings.Join(examples, "\n")}
	if private > 0 {
		pv["skipped"] = fmt.Sprintf("%d private (left as they are)", private)
	}
	if len(msgs) >= limit {
		pv["note"] = fmt.Sprintf("Only the newest %d matches; run it again for the rest.", limit)
	}
	return h.Submit(ctx, rubiplugin.Request{Kind: kindBulk, Summary: fmt.Sprintf("%s %d emails%s", verb, count, target), Preview: pv,
		Payload: set, Options: []rubiplugin.Option{{Key: "allow", Label: "Allow"}}})
}

// peekPrivate reads the start of each message and reports which ones the privacy filter hides.
func peekPrivate(c *imapclient.Client, box string, list []summary, p privacyConfig) map[uint32]bool {
	out := map[uint32]bool{}
	if _, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		for _, m := range list {
			out[m.UID] = true // can't check: leave them alone
		}
		return out
	}
	byUID := map[uint32]summary{}
	var uids []imap.UID
	for _, m := range list {
		byUID[m.UID] = m
		uids = append(uids, imap.UID(m.UID))
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: bulkPeek}}
	for start := 0; start < len(uids); start += 200 {
		chunk := uids[start:min(start+200, len(uids))]
		msgs, err := c.Fetch(imap.UIDSetNum(chunk...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		got := map[uint32]bool{}
		if err == nil {
			for _, m := range msgs {
				got[uint32(m.UID)] = true
				s := byUID[uint32(m.UID)]
				text := ""
				if pm, err := parseMessage(m.FindBodySection(section), 0); err == nil {
					text = pm.Text
				}
				if hidden, _ := p.hidden(s.From, s.Subject, text); hidden {
					out[uint32(m.UID)] = true
				}
			}
		}
		for _, u := range chunk {
			if !got[uint32(u)] {
				out[uint32(u)] = true
			}
		}
	}
	return out
}

func (x *integration) runBulk(h host, set bulkSet) (any, error) {
	c, s, err := session(h)
	if err != nil {
		return nil, err
	}
	defer logout(c)
	sp, err := findSpecials(c, s)
	if err != nil {
		return nil, err
	}
	multi := &undoRec{Op: "multi"}
	changed := 0
	var failed []string
	for box, uids := range set.Boxes {
		if err := x.folderAllowed(h, box); err != nil {
			failed = append(failed, box+": closed")
			continue
		}
		if _, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			failed = append(failed, box+": "+err.Error())
			continue
		}
		want := make([]imap.UID, len(uids))
		for i, u := range uids {
			want[i] = imap.UID(u)
		}
		sums, err := fetchSummaries(c, box, want, 0)
		if err != nil {
			failed = append(failed, box+": "+err.Error())
			continue
		}
		var ts []target
		for _, m := range sums {
			ts = append(ts, target{uid: imap.UID(m.UID), id: m.info.messageID, from: m.From, subject: m.Subject, seen: m.Seen, starred: m.Starred})
		}
		if len(ts) == 0 {
			continue
		}
		var rec *undoRec
		switch set.Action {
		case "archive", "trash", "spam", "move":
			dest := set.To
			if set.Action != "move" {
				dest = set.Action
			}
			rec, err = x.moveTargets(c, sp, box, ts, sp.resolve(dest))
		case "label":
			rec, err = x.labelTargets(c, sp, box, ts, []string{set.Label}, nil)
		case "unlabel":
			rec, err = x.labelTargets(c, sp, box, ts, nil, []string{set.Label})
		case "read", "unread":
			rec, err = markTargets(c, box, ts, set.Action)
		case "star", "unstar":
			rec, err = markTargets(c, box, ts, set.Action+"red")
		}
		if err != nil {
			failed = append(failed, box+": "+err.Error())
			continue
		}
		changed += len(ts)
		if rec != nil {
			multi.Parts = append(multi.Parts, rec)
		}
	}
	out := map[string]any{"status": "done", "changed": changed}
	if len(failed) > 0 {
		out["failed"] = failed
	}
	if len(multi.Parts) > 0 && x.saveUndo(h, multi) == nil {
		out["undo_id"] = multi.ID
	}
	h.Audit("mail_bulk", map[string]any{"action": set.Action, "count": changed})
	return out, nil
}

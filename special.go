package mailkit

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// specials are the server's special folders, found by their special-use marks (RFC 6154), else by the
// usual names. The agent can say "trash" or "archive" and get the right folder on every server.
type specials struct {
	Sent, Drafts, Trash, Junk, Archive, All string
	boxes                                   []mailbox
}

var specialNames = map[string][]string{
	"Trash":   {"Deleted Messages", "Trash", "Bin", "[Gmail]/Trash", "[Gmail]/Bin", "Корзина", "Удаленные"},
	"Junk":    {"Junk", "Spam", "[Gmail]/Spam", "Спам"},
	"Archive": {"Archive", "Archives", "Архив"},
	"All":     {"[Gmail]/All Mail", "[Google Mail]/All Mail"},
}

func findSpecials(c *imapclient.Client, s Settings) (specials, error) {
	boxes, err := listMailboxes(c)
	if err != nil {
		return specials{}, err
	}
	sp := specials{Sent: s.Sent, Drafts: s.Drafts, boxes: boxes}
	slot := map[string]*string{"Sent": &sp.Sent, "Drafts": &sp.Drafts, "Trash": &sp.Trash, "Junk": &sp.Junk,
		"Archive": &sp.Archive, "All": &sp.All}
	marked := map[string]bool{}
	for _, b := range boxes {
		for _, a := range b.Attrs {
			if p, ok := slot[a]; ok && !marked[a] {
				*p, marked[a] = b.Name, true
			}
		}
	}
	for key, names := range specialNames {
		if *slot[key] != "" {
			continue
		}
		for _, n := range names {
			if sp.has(n) {
				*slot[key] = n
				break
			}
		}
	}
	return sp, nil
}

func (sp specials) has(name string) bool {
	for _, b := range sp.boxes {
		if b.Name == name || strings.EqualFold(b.Name, "INBOX") && strings.EqualFold(name, "INBOX") {
			return true
		}
	}
	return false
}

// resolve turns inbox, sent, drafts, trash, spam, archive or all into the server's folder name; any other
// name is matched case-insensitively against the folders, or kept as given.
func (sp specials) resolve(name string) string {
	n := strings.TrimSpace(name)
	switch strings.ToLower(n) {
	case "", "inbox":
		return "INBOX"
	case "sent":
		return orDefault(sp.Sent, n)
	case "drafts", "draft":
		return orDefault(sp.Drafts, n)
	case "trash", "bin", "deleted":
		return orDefault(sp.Trash, n)
	case "spam", "junk":
		return orDefault(sp.Junk, n)
	case "archive":
		if sp.Archive != "" {
			return sp.Archive
		}
		return orDefault(sp.All, n)
	case "all", "all mail":
		return orDefault(sp.All, n)
	}
	if sp.has(n) {
		return n
	}
	for _, b := range sp.boxes {
		if strings.EqualFold(b.Name, n) {
			return b.Name
		}
	}
	return n
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// special reports whether box is one of the folders the server needs (never renamed or deleted).
func (sp specials) special(box string) bool {
	if strings.EqualFold(box, "INBOX") {
		return true
	}
	for _, b := range []string{sp.Sent, sp.Drafts, sp.Trash, sp.Junk, sp.Archive, sp.All} {
		if b != "" && b == box {
			return true
		}
	}
	for _, b := range sp.boxes {
		if b.Name == box && len(b.Attrs) > 0 {
			return true
		}
	}
	return strings.HasPrefix(box, "[Gmail]") && !strings.Contains(strings.TrimPrefix(box, "[Gmail]"), "/") ||
		box == "[Gmail]/Important" || box == "[Gmail]/Starred"
}

// ---- query search ----

// extraTerms turns the separate filters into query terms, so both can be used together.
func (q searchQuery) extraTerms() string {
	var terms []string
	quote := func(v string) string { return `"` + strings.ReplaceAll(strings.TrimSpace(v), `"`, "") + `"` }
	for k, v := range map[string]string{"from": q.From, "to": q.To, "subject": q.Subject} {
		if strings.TrimSpace(v) != "" {
			terms = append(terms, k+":"+quote(v))
		}
	}
	if strings.TrimSpace(q.Text) != "" {
		terms = append(terms, quote(q.Text))
	}
	if q.Since != "" {
		terms = append(terms, "after:"+strings.ReplaceAll(q.Since, "-", "/"))
	}
	if q.Before != "" {
		terms = append(terms, "before:"+strings.ReplaceAll(q.Before, "-", "/"))
	}
	if q.UnseenOnly {
		terms = append(terms, "is:unread")
	}
	sort.Strings(terms)
	return strings.Join(terms, " ")
}

// querySearch runs a query in Gmail's syntax: Gmail answers it itself, elsewhere the operators are
// translated and every open folder except Trash and Spam is searched (as Gmail does), unless the query
// or mailbox names one. content reports whether the query looks at subjects or text.
func (x *integration) querySearch(h host, c *imapclient.Client, s Settings, q searchQuery) (msgs []summary, content bool, err error) {
	query := strings.TrimSpace(q.Query + " " + q.extraTerms())
	alts, perr := parseQuery(query)
	content = perr != nil || byContent(alts)
	sp, err := findSpecials(c, s)
	if err != nil {
		return nil, content, err
	}
	inBox := ""
	if perr == nil {
		if inBox, err = queryBox(alts); err != nil && !prov.Gmail {
			return nil, content, err
		}
	}
	if prov.Gmail {
		box := sp.resolve(q.Mailbox)
		if q.Mailbox == "" {
			box = "INBOX"
			if sp.All != "" && x.folderAllowed(h, sp.All) == nil {
				box = sp.All
			}
			switch strings.ToLower(inBox) { // All Mail has no trash or spam: Gmail looks there only when told
			case "trash", "bin":
				box = sp.Trash
			case "spam":
				box = sp.Junk
			}
		}
		if err := x.folderAllowed(h, box); err != nil {
			return nil, content, err
		}
		pw, err := h.Secret("app_password")
		if err != nil {
			return nil, content, err
		}
		limit := q.limit()
		if q.max > 0 {
			limit = 0 // a bulk action needs no labels
		}
		uids, meta, err := gmailLookup(s, pw, box, query, nil, limit)
		if err != nil {
			return nil, content, err
		}
		if _, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return nil, content, fmt.Errorf("can't open folder %q: %w", box, err)
		}
		msgs, err = fetchSummaries(c, box, uids, q.limit())
		withMeta(msgs, meta)
		return msgs, content, err
	}
	if perr != nil {
		return nil, content, perr
	}
	crit, err := toCriteria(alts, time.Now())
	if err != nil {
		return nil, content, err
	}
	var boxes []string
	switch {
	case inBox != "" && !strings.EqualFold(inBox, "anywhere"):
		boxes = []string{sp.resolve(inBox)}
	case q.Mailbox != "":
		boxes = []string{sp.resolve(q.Mailbox)}
	}
	if len(boxes) == 1 {
		if err := x.folderAllowed(h, boxes[0]); err != nil {
			return nil, content, err
		}
	} else {
		anywhere := strings.EqualFold(inBox, "anywhere")
		for _, b := range sp.boxes {
			if (b.Name == sp.Trash || b.Name == sp.Junk || b.Name == sp.Drafts) && !anywhere || x.folderAllowed(h, b.Name) != nil {
				continue
			}
			if boxes = append(boxes, b.Name); len(boxes) >= 40 {
				break
			}
		}
	}
	for _, b := range boxes {
		found, err := searchBox(c, b, crit, q.limit())
		if err != nil {
			if len(boxes) == 1 {
				return nil, content, err
			}
			continue // a folder that can't be searched shouldn't hide the others
		}
		msgs = append(msgs, found...)
	}
	sortNewest(msgs)
	if len(msgs) > q.limit() {
		msgs = msgs[:q.limit()]
	}
	return msgs, content, nil
}

func sortNewest(msgs []summary) {
	at := func(m summary) time.Time {
		t, _ := time.Parse(time.RFC3339, m.Date)
		return t
	}
	sort.SliceStable(msgs, func(i, j int) bool { return at(msgs[i]).After(at(msgs[j])) })
}

var errNoUIDs = errors.New("give uid or uids")

// withMeta adds Gmail's labels and conversation to messages.
func withMeta(msgs []summary, meta map[imap.UID]gmMeta) {
	for i := range msgs {
		if m, ok := meta[imap.UID(msgs[i].UID)]; ok {
			msgs[i].Labels, msgs[i].info.thread = m.Labels, m.Thread
		}
	}
}

// gmailMeta fetches labels and conversations for messages found without Gmail's search.
func gmailMeta(h host, s Settings, box string, msgs []summary) {
	if !prov.Gmail || len(msgs) == 0 {
		return
	}
	pw, err := h.Secret("app_password")
	if err != nil {
		return
	}
	uids := make([]imap.UID, len(msgs))
	for i, m := range msgs {
		uids[i] = imap.UID(m.UID)
	}
	if _, meta, err := gmailLookup(s, pw, box, "", uids, len(uids)); err == nil {
		withMeta(msgs, meta)
	}
}

package mailkit

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

// Reply tracking. While at least one sent email is tracked, the watcher fetches the headers of mail that
// arrived since its last check (by UID) and matches them to tracked Message-IDs:
//
//  1. In-Reply-To / References contain a tracked Message-ID → match "headers" (reliable);
//  2. otherwise the sender is one of the original recipients and the subject is the same after
//     stripping Re:/Fwd:/Ответ: → match "subject" (heuristic).
//
// Only headers are fetched and nothing is marked as read. With nothing tracked, the server isn't contacted.

type tracked struct {
	ID         string    `json:"id"`
	MessageID  string    `json:"message_id"`
	Subject    string    `json:"subject"`
	Recipients []string  `json:"recipients"`
	SentAt     time.Time `json:"sent_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Stopped    bool      `json:"stopped,omitempty"`
	Replies    []string  `json:"replies,omitempty"` // Message-IDs already reported
}

type cursor struct {
	UIDValidity uint32 `json:"uid_validity"`
	LastUID     uint32 `json:"last_uid"`
}

type state struct {
	Tracked []*tracked        `json:"tracked"`
	Watches []*watch          `json:"watches,omitempty"`
	Grants  []*grant          `json:"grants,omitempty"` // temporary folder access the user approved
	Cursors map[string]cursor `json:"cursors"`
	Undo    []*undoRec        `json:"undo,omitempty"`    // how to reverse recent changes to the mailbox
	Queued  []*queued         `json:"queued,omitempty"`  // approved emails waiting to go out
	Snoozed []*snoozed        `json:"snoozed,omitempty"` // mail put away until a time
	Rules   []*rule           `json:"rules,omitempty"`   // what to do with new mail, set by the user
}

func (s *state) active(now time.Time) []*tracked {
	out := []*tracked{}
	for _, t := range s.Tracked {
		if !t.Stopped && now.Before(t.ExpiresAt) {
			out = append(out, t)
		}
	}
	return out
}

// prune drops entries that ended more than a week ago.
func (s *state) prune(now time.Time) {
	kept := s.Tracked[:0]
	for _, t := range s.Tracked {
		if now.Before(t.ExpiresAt.Add(7*24*time.Hour)) && !(t.Stopped && now.After(t.SentAt.Add(7*24*time.Hour))) {
			kept = append(kept, t)
		}
	}
	s.Tracked = kept
	var ws []*watch
	for _, w := range s.Watches {
		if w.active(now) || now.Before(w.Expires.Add(7*24*time.Hour)) && !w.Stopped {
			ws = append(ws, w)
		}
	}
	s.Watches = ws
	var gs []*grant
	for _, g := range s.Grants {
		if now.Before(g.Until) {
			gs = append(gs, g)
		}
	}
	s.Grants = gs
}

func (x *integration) loadState(h host) (*state, error) {
	st := &state{Cursors: map[string]cursor{}}
	if err := h.LoadState(st); err != nil {
		return nil, err
	}
	if st.Cursors == nil {
		st.Cursors = map[string]cursor{}
	}
	return st, nil
}

func (x *integration) track(h host, m mailPayload, days int) (*tracked, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t := &tracked{ID: randomID("trk_"), MessageID: normID(m.MessageID), Subject: m.Subject,
		Recipients: lowerAll(m.Envelope), SentAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	st.prune(now)
	st.Tracked = append(st.Tracked, t)
	if err := h.SaveState(st); err != nil {
		return nil, err
	}
	h.Audit("tracking_started", map[string]any{"tracking_id": t.ID, "days": days})
	return t, nil
}

func (x *integration) stopTracking(h host, id string) (bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return false, err
	}
	for _, t := range st.Tracked {
		if t.ID == id && !t.Stopped {
			t.Stopped = true
			return true, h.SaveState(st)
		}
	}
	return false, nil
}

// ---- lifecycle ----

func (x *integration) Start(b base) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.stop != nil {
		return nil
	}
	a, err := accountFor(b, "")
	if err != nil {
		return err
	}
	var s Settings
	if err := a.Settings(&s); err != nil {
		return err
	}
	interval := time.Duration(max(s.WatchIntervalSeconds, 30)) * time.Second
	x.stop, x.pollNow, x.jobsNow = make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)
	go x.loop(b, interval, x.stop, x.pollNow)
	go x.jobsLoop(b, x.stop, x.jobsNow)
	return nil
}

func (x *integration) Stop() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.stop != nil {
		close(x.stop)
		x.stop, x.pollNow, x.jobsNow = nil, nil, nil
	}
}

func (x *integration) loop(b base, interval time.Duration, stop, now chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-now:
		}
		x.pollAll(b)
	}
}

// pollAll checks every connected account once.
func (x *integration) pollAll(b base) {
	list, err := accounts(b)
	if err != nil {
		b.Logf("listing accounts failed: %v", err)
		return
	}
	for _, a := range list {
		if err := x.poll(a); err != nil {
			a.Logf("mail check failed: %v", err)
		}
	}
}

// poll checks new mail once. Exported to tests through the package.
func (x *integration) poll(h host) error {
	x.mu.Lock()
	st, err := x.loadState(h)
	x.mu.Unlock()
	if err != nil {
		return err
	}
	now := time.Now()
	active := st.active(now)
	watches := st.activeWatches(now)
	if len(active) == 0 && len(watches) == 0 && len(st.Rules) == 0 {
		return nil // nothing to follow: the server isn't contacted
	}
	if x.folderAllowed(h, "INBOX") != nil {
		return nil // the user closed INBOX to the agent: no replies or matches from it
	}
	c, s, err := session(h)
	if err != nil {
		return err
	}
	defer logout(c)
	oldest := now
	for _, t := range active {
		if t.SentAt.Before(oldest) {
			oldest = t.SentAt
		}
	}
	for _, w := range watches {
		if w.Created.Before(oldest) {
			oldest = w.Created
		}
	}
	p := privacyOf(h)
	needText := false
	for _, w := range watches {
		needText = needText || len(w.Keywords) > 0
	}
	box := "INBOX"
	cur := st.Cursors[box]
	headers, next, err := newHeaders(c, box, cur, oldest.Add(-24*time.Hour))
	if err != nil {
		return err
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	st, err = x.loadState(h) // reload: a send may have added tracking meanwhile
	if err != nil {
		return err
	}
	texts := 0
	var sp *specials
	ruleWords := false
	for _, r := range st.Rules {
		ruleWords = ruleWords || len(r.Words) > 0
	}
	for _, hd := range headers {
		private, _ := p.hidden(hd.from, hd.subject, "")
		x.checkWatches(h, c, box, st, hd, private, needText, &texts, now)
		if len(st.Rules) > 0 {
			if sp == nil {
				found, err := findSpecials(c, s)
				if err != nil {
					return err
				}
				sp = &found
			}
			text := ""
			if ruleWords && !private && texts < maxTextsPerPoll {
				texts++
				if raw, err := fetchRaw(c, box, hd.uid); err == nil {
					if m, err := parseMessage(raw, 20000); err == nil {
						if hidden, _ := p.hidden(hd.from, hd.subject, m.Text); hidden {
							private = true
						} else {
							text = m.Text
						}
					}
				}
			}
			x.applyRules(h, c, sp, box, st, hd, private, text, now)
		}
		t, how := match(hd, st.active(now), s.Address)
		if t == nil {
			continue
		}
		id := normID(hd.messageID)
		if id == "" {
			id = "uid:" + box + ":" + strconv.FormatUint(uint64(hd.uid), 10)
		}
		if contains(t.Replies, id) {
			continue
		}
		t.Replies = append(t.Replies, id)
		reply := map[string]any{"mailbox": box, "uid": hd.uid, "from": hd.from, "subject": hd.subject, "date": hd.date, "match": how}
		if private {
			reply = map[string]any{"mailbox": box, "uid": hd.uid, "from": senderOnly(hd.from), "private": true,
				"date": hd.date, "match": how, "note": privateNote()}
		}
		_, _ = h.Emit("reply", map[string]any{
			"tracking_id": t.ID,
			"original":    map[string]any{"subject": t.Subject, "message_id": "<" + t.MessageID + ">", "sent_at": t.SentAt},
			"reply":       reply,
		})
		h.Audit("reply_found", map[string]any{"tracking_id": t.ID, "match": how})
	}
	st.Cursors[box] = next
	return h.SaveState(st)
}

type header struct {
	uid        uint32
	messageID  string
	inReplyTo  string
	references string
	from       string
	fromAddr   string
	subject    string
	date       time.Time
}

func newHeaders(c *imapclient.Client, box string, cur cursor, since time.Time) ([]header, cursor, error) {
	sel, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, cur, err
	}
	if sel.UIDValidity != cur.UIDValidity {
		cur = cursor{UIDValidity: sel.UIDValidity}
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(cur.LastUID+1), 0) // LastUID+1:*
	res, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}, Since: since}, nil).Wait()
	if err != nil {
		return nil, cur, err
	}
	var uids []imap.UID
	for _, u := range res.AllUIDs() {
		if uint32(u) > cur.LastUID {
			uids = append(uids, u)
		}
	}
	if len(uids) > 500 {
		uids = uids[len(uids)-500:]
	}
	if len(uids) == 0 {
		return nil, cur, nil
	}
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true,
		HeaderFields: []string{"From", "Subject", "Date", "Message-ID", "In-Reply-To", "References"}}
	msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, cur, err
	}
	var out []header
	for _, m := range msgs {
		raw := m.FindBodySection(section)
		mr, err := mail.CreateReader(bytes.NewReader(append(raw, '\r', '\n')))
		if err != nil {
			continue
		}
		hd := header{uid: uint32(m.UID)}
		mh := mr.Header
		hd.subject, _ = mh.Subject()
		hd.messageID = mh.Get("Message-ID")
		hd.inReplyTo = mh.Get("In-Reply-To")
		hd.references = mh.Get("References")
		hd.from = headerAddrs(mh, "From")
		if list, err := mh.AddressList("From"); err == nil && len(list) > 0 {
			hd.fromAddr = strings.ToLower(list[0].Address)
		}
		hd.date, _ = mh.Date()
		out = append(out, hd)
		if uint32(m.UID) > cur.LastUID {
			cur.LastUID = uint32(m.UID)
		}
	}
	return out, cur, nil
}

var (
	reMsgID  = regexp.MustCompile(`<[^<>\s]+>`)
	rePrefix = regexp.MustCompile(`(?i)^\s*(re|fw|fwd|aw|sv|wg|ответ|отв|пересл)(\[\d+\])?\s*:\s*`)
	reSpaces = regexp.MustCompile(`\s+`)
)

func normID(s string) string { return strings.ToLower(strings.Trim(strings.TrimSpace(s), "<>")) }

func normSubject(s string) string {
	for {
		t := rePrefix.ReplaceAllString(s, "")
		if t == s {
			break
		}
		s = t
	}
	return strings.ToLower(strings.TrimSpace(reSpaces.ReplaceAllString(s, " ")))
}

func match(hd header, active []*tracked, ownAddress string) (*tracked, string) {
	own := strings.ToLower(ownAddress)
	id := normID(hd.messageID)
	// eligible rules out the tracked email itself (e.g. it lands in INBOX when sent to yourself) and the
	// user's own messages, unless the user was a recipient of the tracked email (writing to yourself).
	eligible := func(t *tracked) bool {
		if id != "" && id == t.MessageID {
			return false
		}
		return hd.fromAddr == "" || hd.fromAddr != own || contains(t.Recipients, own)
	}
	byID := map[string]*tracked{}
	for _, t := range active {
		byID[t.MessageID] = t
	}
	for _, ref := range reMsgID.FindAllString(hd.inReplyTo+" "+hd.references, -1) {
		if t := byID[normID(ref)]; t != nil && eligible(t) {
			return t, "headers"
		}
	}
	subj := normSubject(hd.subject)
	for _, t := range active {
		if eligible(t) && subj != "" && subj == normSubject(t.Subject) && contains(t.Recipients, hd.fromAddr) &&
			(hd.date.IsZero() || hd.date.After(t.SentAt.Add(-2*time.Minute))) {
			return t, "subject"
		}
	}
	return nil, ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func lowerAll(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = strings.ToLower(v)
	}
	return out
}

// maxTextsPerPoll bounds how many bodies a poll fetches for word watches.
const maxTextsPerPoll = 30

// checkWatches emits a watch event for each watch a new message matches.
func (x *integration) checkWatches(h host, c *imapclient.Client, box string, st *state, hd header, private, needText bool, texts *int, now time.Time) {
	var text string
	fetched := false
	for _, w := range st.activeWatches(now) {
		if hd.date.Before(w.Created.Add(-2*time.Minute)) && !hd.date.IsZero() {
			continue // only mail that arrived after the watch was set
		}
		if needText && len(w.Keywords) > 0 && !private && !fetched && *texts < maxTextsPerPoll {
			fetched = true
			*texts++
			if raw, err := fetchRaw(c, box, hd.uid); err == nil {
				if m, err := parseMessage(raw, 20000); err == nil {
					text = m.Text
					if hidden, _ := privacyOf(h).hidden(hd.from, hd.subject, text); hidden {
						private, text = true, ""
					}
				}
			}
		}
		if !w.matches(hd.from, hd.subject, text, private) {
			continue
		}
		w.Hits++
		w.LastHit = now.UTC()
		msg := map[string]any{"mailbox": box, "uid": hd.uid, "from": hd.from, "subject": hd.subject, "date": hd.date}
		if private {
			msg = map[string]any{"mailbox": box, "uid": hd.uid, "from": senderOnly(hd.from), "date": hd.date,
				"private": true, "note": privateNote()}
		} else if text != "" {
			snippet := strings.Join(strings.Fields(text), " ")
			if len([]rune(snippet)) > 300 {
				snippet = string([]rune(snippet)[:300]) + "…"
			}
			msg["snippet"] = snippet
		}
		_, _ = h.EmitTo(w.Agent, "watch", map[string]any{
			"watch":   map[string]any{"id": w.ID, "name": w.Name, "note": w.Note, "hits": w.Hits},
			"message": msg,
			"hint":    "Read it with " + tool("read") + "(uid) if you need more. Stop the watch with " + tool("unwatch") + " when it's done.",
		})
		h.Audit("watch_matched", map[string]any{"watch_id": w.ID, "private": private})
	}
}

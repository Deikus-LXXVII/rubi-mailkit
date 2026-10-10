package mailkit

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-imap/v2"
)

// Search queries in Gmail's syntax: from:anna subject:"invoice" is:unread larger:5M newer_than:7d -label:work
// OR between terms, "exact phrases", and a minus to exclude. Gmail runs the query itself (X-GM-RAW), so every
// Gmail operator works there; other servers get the common operators translated to IMAP search.

type queryTerm struct {
	key, value string // key is "" for plain words
	neg        bool
}

// queryAlt is one AND-ed element: one term, or several joined by OR.
type queryAlt []queryTerm

// tokenize splits a query into words, keeping "quoted phrases" and key:"quoted values" together.
func tokenize(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case unicode.IsSpace(r) && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

var reKey = regexp.MustCompile(`^([a-z_]+):(.+)$`)

func parseQuery(q string) ([]queryAlt, error) {
	var out []queryAlt
	orNext := false
	for _, tok := range tokenize(q) {
		if tok == "OR" || tok == "|" {
			if len(out) == 0 {
				return nil, errors.New("OR needs a term on each side")
			}
			orNext = true
			continue
		}
		if strings.ContainsAny(tok, "(){}") && !strings.HasPrefix(tok, `"`) {
			return nil, errors.New("brackets aren't supported here; use OR between single terms")
		}
		t := queryTerm{}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			t.neg, tok = true, tok[1:]
		}
		if m := reKey.FindStringSubmatch(tok); m != nil {
			t.key, t.value = m[1], m[2]
		} else {
			t.value = tok
		}
		t.value = strings.Trim(t.value, `"`)
		if t.value == "" {
			continue
		}
		if orNext {
			out[len(out)-1] = append(out[len(out)-1], t)
			orNext = false
		} else {
			out = append(out, queryAlt{t})
		}
	}
	if orNext {
		return nil, errors.New("OR needs a term on each side")
	}
	return out, nil
}

// headerKeys are operators that only look at who the mail is from or to: they reveal nothing about content.
var headerKeys = map[string]string{"from": "From", "to": "To", "cc": "Cc", "bcc": "Bcc", "deliveredto": "Delivered-To", "list": "List-Id"}

// metaKeys don't look at content either.
var metaKeys = map[string]bool{"is": true, "in": true, "label": true, "has": true, "larger": true, "smaller": true,
	"size": true, "after": true, "before": true, "older": true, "newer": true, "newer_than": true, "older_than": true,
	"since": true, "category": true}

// byContent: does the query look at subjects or text? Such queries must not find private mail.
func byContent(alts []queryAlt) bool {
	for _, a := range alts {
		for _, t := range a {
			if _, ok := headerKeys[t.key]; ok || metaKeys[t.key] && !(t.key == "has" && t.value != "attachment") {
				continue
			}
			return true
		}
	}
	return false
}

// queryBox returns the folder an in:/label: term names ("" if none).
func queryBox(alts []queryAlt) (string, error) {
	box := ""
	for _, a := range alts {
		for _, t := range a {
			if t.key != "in" && t.key != "label" {
				continue
			}
			if len(a) > 1 || t.neg {
				return "", errors.New("in:/label: can't be combined with OR or minus here")
			}
			if box != "" {
				return "", errors.New("only one in:/label: per search here")
			}
			box = t.value
		}
	}
	return box, nil
}

// toCriteria translates a query for servers without Gmail's search. now is for newer_than/older_than.
func toCriteria(alts []queryAlt, now time.Time) (*imap.SearchCriteria, error) {
	crit := &imap.SearchCriteria{}
	for _, a := range alts {
		var parts []imap.SearchCriteria
		for _, t := range a {
			if t.key == "in" || t.key == "label" {
				continue
			}
			c, err := termCriteria(t, now)
			if err != nil {
				return nil, err
			}
			parts = append(parts, c)
		}
		if len(parts) == 0 {
			continue
		}
		one := parts[0]
		for _, p := range parts[1:] {
			one = imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{one, p}}}
		}
		crit.And(&one)
	}
	return crit, nil
}

func termCriteria(t queryTerm, now time.Time) (imap.SearchCriteria, error) {
	var c imap.SearchCriteria
	v := t.value
	switch t.key {
	case "":
		c.Text = []string{v}
	case "subject":
		c.Header = []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: v}}
	case "is":
		switch strings.ToLower(v) {
		case "unread":
			c.NotFlag = []imap.Flag{imap.FlagSeen}
		case "read":
			c.Flag = []imap.Flag{imap.FlagSeen}
		case "starred", "flagged":
			c.Flag = []imap.Flag{imap.FlagFlagged}
		case "unstarred":
			c.NotFlag = []imap.Flag{imap.FlagFlagged}
		case "answered", "replied":
			c.Flag = []imap.Flag{imap.FlagAnswered}
		default:
			return c, fmt.Errorf("is:%s works only in Gmail; here: is:unread, is:read, is:starred, is:answered", v)
		}
	case "has":
		if strings.ToLower(v) != "attachment" {
			return c, fmt.Errorf("has:%s works only in Gmail; here: has:attachment", v)
		}
		c.Header = []imap.SearchCriteriaHeaderField{{Key: "Content-Type", Value: "multipart/mixed"}}
	case "larger", "smaller", "size":
		n, err := parseSize(v)
		if err != nil {
			return c, err
		}
		if t.key == "smaller" {
			c.Smaller = n
		} else {
			c.Larger = n
		}
	case "after", "since", "newer":
		d, err := parseDay(v)
		if err != nil {
			return c, err
		}
		c.Since = d
	case "before", "older":
		d, err := parseDay(v)
		if err != nil {
			return c, err
		}
		c.Before = d
	case "newer_than", "older_than":
		d, err := parseAge(v, now)
		if err != nil {
			return c, err
		}
		if t.key == "newer_than" {
			c.Since = d
		} else {
			c.Before = d
		}
	default:
		h, ok := headerKeys[t.key]
		if !ok {
			return c, fmt.Errorf("%s: works only in Gmail. Here: from: to: cc: bcc: subject: is: has:attachment larger: smaller: after: before: newer_than: older_than: in: list: deliveredto:, words, \"phrases\", OR and -", t.key)
		}
		c.Header = []imap.SearchCriteriaHeaderField{{Key: h, Value: v}}
	}
	if t.neg {
		return imap.SearchCriteria{Not: []imap.SearchCriteria{c}}, nil
	}
	return c, nil
}

func parseSize(v string) (int64, error) {
	v = strings.ToUpper(strings.TrimSpace(v))
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "M") || strings.HasSuffix(v, "MB"):
		mult, v = 1<<20, strings.TrimSuffix(strings.TrimSuffix(v, "B"), "M")
	case strings.HasSuffix(v, "K") || strings.HasSuffix(v, "KB"):
		mult, v = 1<<10, strings.TrimSuffix(strings.TrimSuffix(v, "B"), "K")
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("sizes look like 500K or 5M, got %q", v)
	}
	return int64(n * float64(mult)), nil
}

func parseDay(v string) (time.Time, error) {
	for _, f := range []string{"2006/01/02", "2006-01-02", "2006/1/2", "2006-1-2"} {
		if d, err := time.Parse(f, v); err == nil {
			return d, nil
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 1e8 { // Gmail also takes Unix seconds
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("dates look like 2026/10/01, got %q", v)
}

func parseAge(v string, now time.Time) (time.Time, error) {
	if len(v) < 2 {
		return time.Time{}, fmt.Errorf("ages look like 7d, 2m or 1y, got %q", v)
	}
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n < 0 {
		return time.Time{}, fmt.Errorf("ages look like 7d, 2m or 1y, got %q", v)
	}
	switch v[len(v)-1] {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -7*n), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	case 'y':
		return now.AddDate(-n, 0, 0), nil
	}
	return time.Time{}, fmt.Errorf("ages look like 7d, 2m or 1y, got %q", v)
}

// ---- threads ----

// threadInfo is what grouping needs about a message; summary carries it unexported.
type threadInfo struct {
	messageID string
	parents   []string // In-Reply-To and References
	subject   string
}

func msgIDs(s string) []string {
	ids := reMsgID.FindAllString(s, -1)
	for i := range ids {
		ids[i] = strings.ToLower(ids[i])
	}
	return ids
}

type thread struct {
	Thread       int      `json:"thread"` // the messages of this thread carry the same number
	Subject      string   `json:"subject"`
	Count        int      `json:"count"`
	Unread       int      `json:"unread,omitempty"`
	Last         string   `json:"last,omitempty"`
	Participants []string `json:"participants,omitempty"`
}

// groupThreads joins messages that answer each other (Message-ID, In-Reply-To, References), and replies
// ("Re: x") with the same subject as their original, and numbers each message's thread. Threads come
// newest first.
func groupThreads(msgs []summary) []thread {
	parent := make([]int, len(msgs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[rb] = ra
		}
	}
	byID := map[string]int{}
	for i, m := range msgs {
		if m.info.messageID != "" {
			if j, ok := byID[m.info.messageID]; ok {
				union(j, i)
			} else {
				byID[m.info.messageID] = i
			}
		}
	}
	bySubject := map[string]int{}
	for i, m := range msgs {
		for _, p := range m.info.parents {
			if j, ok := byID[p]; ok {
				union(j, i)
			} else {
				byID[p] = i // two replies to the same unseen original belong together too
			}
		}
		if norm := normSubject(m.info.subject); norm != "" {
			if j, ok := bySubject[norm]; ok && (isReply(m.info.subject) || isReply(msgs[j].info.subject)) {
				union(j, i)
			} else if !ok {
				bySubject[norm] = i
			}
		}
	}
	groups := map[int]*thread{}
	var order []int
	for i, m := range msgs {
		r := find(i)
		t := groups[r]
		if t == nil {
			t = &thread{}
			groups[r] = t
			order = append(order, r)
		}
		t.Count++
		if !m.Seen {
			t.Unread++
		}
		if !m.Private {
			if t.Subject == "" || isReply(t.Subject) && !isReply(m.Subject) {
				t.Subject = m.Subject
			}
			if later(m.Date, t.Last) {
				t.Last = m.Date
			}
		}
		if who := senderOnly(m.From); who != "" && len(t.Participants) < 6 && !contains(t.Participants, who) {
			t.Participants = append(t.Participants, who)
		}
	}
	out := make([]*thread, 0, len(order))
	for _, r := range order {
		out = append(out, groups[r])
	}
	sort.SliceStable(out, func(i, j int) bool { return later(out[i].Last, out[j].Last) })
	res := make([]thread, len(out))
	for i, t := range out {
		t.Thread = i + 1
		res[i] = *t
	}
	for i := range msgs {
		msgs[i].Thread = groups[find(i)].Thread
	}
	return res
}

// later compares two RFC 3339 dates (which may be in different time zones).
func later(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return a > b
	}
	return ta.After(tb)
}

func isReply(subject string) bool {
	return normSubject(subject) != strings.ToLower(strings.TrimSpace(reSpaces.ReplaceAllString(subject, " ")))
}

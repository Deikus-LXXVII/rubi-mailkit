package mailkit

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/emersion/go-imap/v2"
)

// Gmail's own search (X-GM-RAW) takes a query exactly as typed in the Gmail search box. go-imap doesn't
// speak it, so a short separate connection runs just LOGIN, EXAMINE and UID SEARCH. Every argument goes as
// a literal, so nothing the agent writes can break out into another command.

// Seams for tests.
var (
	gmailLookup = gmailLookupRaw
	gmailThread = gmailThreadRaw
)

type rawConn struct {
	c   net.Conn
	r   *bufio.Reader
	tag int
}

// gmMeta is what Gmail knows about a message beyond IMAP: its conversation and its labels.
type gmMeta struct {
	Thread string
	Labels []string
}

func rawOpen(s Settings, password string) (*rawConn, error) {
	conn, err := dialRaw(s.IMAPAddr)
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %w", prov.Name, err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	rc := &rawConn{c: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	if _, err := rc.line(); err != nil { // greeting
		conn.Close()
		return nil, err
	}
	if _, err := rc.run("LOGIN", lit(s.user()), lit(password)); err != nil {
		conn.Close()
		return nil, authError{}
	}
	return rc, nil
}

func (rc *rawConn) close() {
	_, _ = rc.run("LOGOUT")
	rc.c.Close()
}

func (rc *rawConn) examine(box string) error {
	if _, err := rc.run("EXAMINE", lit(utf7(box))); err != nil {
		return fmt.Errorf("can't open folder %q: %w", box, err)
	}
	return nil
}

func searchUIDs(lines []string) []imap.UID {
	var uids []imap.UID
	for _, l := range lines {
		if !strings.HasPrefix(strings.ToUpper(l), "* SEARCH") {
			continue
		}
		for _, f := range strings.Fields(l)[2:] {
			if n, err := strconv.ParseUint(f, 10, 32); err == nil && n > 0 {
				uids = append(uids, imap.UID(n))
			}
		}
	}
	return uids
}

// gmailRawSearch runs a Gmail query in box and returns the matching UIDs.
func gmailRawSearch(s Settings, password, box, query string) ([]imap.UID, error) {
	uids, _, err := gmailLookupRaw(s, password, box, query, nil, 0)
	return uids, err
}

// gmailLookupRaw searches box with a Gmail query (unless query is empty: then uids are the messages) and
// fetches the conversation and labels of the newest limit of them (none if limit is 0).
func gmailLookupRaw(s Settings, password, box, query string, uids []imap.UID, limit int) ([]imap.UID, map[imap.UID]gmMeta, error) {
	rc, err := rawOpen(s, password)
	if err != nil {
		return nil, nil, err
	}
	defer rc.close()
	if err := rc.examine(box); err != nil {
		return nil, nil, err
	}
	if query != "" {
		lines, err := rc.run("UID SEARCH CHARSET UTF-8 X-GM-RAW", lit(query))
		if err != nil {
			return nil, nil, fmt.Errorf("Gmail didn't accept the search: %w", err)
		}
		uids = searchUIDs(lines)
	}
	if limit <= 0 || len(uids) == 0 {
		return uids, nil, nil
	}
	newest := append([]imap.UID{}, uids...)
	sort.Slice(newest, func(i, j int) bool { return newest[i] < newest[j] })
	if len(newest) > limit {
		newest = newest[len(newest)-limit:]
	}
	meta, err := rc.meta(newest)
	if err != nil {
		return uids, nil, nil // labels are a bonus; the search stands
	}
	return uids, meta, nil
}

// gmailThreadRaw finds, in all (All Mail), the messages of the conversation of uid in box.
func gmailThreadRaw(s Settings, password, box string, uid imap.UID, all string) ([]imap.UID, error) {
	rc, err := rawOpen(s, password)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	if err := rc.examine(box); err != nil {
		return nil, err
	}
	meta, err := rc.meta([]imap.UID{uid})
	if err != nil || meta[uid].Thread == "" {
		return nil, errors.New("Gmail didn't say which conversation it belongs to")
	}
	if err := rc.examine(all); err != nil {
		return nil, err
	}
	lines, err := rc.run("UID SEARCH X-GM-THRID " + meta[uid].Thread)
	if err != nil {
		return nil, err
	}
	return searchUIDs(lines), nil
}

// meta fetches X-GM-THRID and X-GM-LABELS of uids in the open folder.
func (rc *rawConn) meta(uids []imap.UID) (map[imap.UID]gmMeta, error) {
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(u)
	}
	lines, err := rc.run("UID FETCH " + set.String() + " (UID X-GM-THRID X-GM-LABELS)")
	if err != nil {
		return nil, err
	}
	out := map[imap.UID]gmMeta{}
	for _, l := range lines {
		uid, m, ok := parseGmFetch(l)
		if ok {
			out[uid] = m
		}
	}
	return out, nil
}

// parseGmFetch reads "* 3 FETCH (X-GM-THRID 1650 X-GM-LABELS (\\Inbox "My label") UID 12)".
func parseGmFetch(line string) (imap.UID, gmMeta, bool) {
	i := strings.Index(strings.ToUpper(line), " FETCH (")
	if !strings.HasPrefix(line, "* ") || i < 0 {
		return 0, gmMeta{}, false
	}
	toks := sexpTokens(line[i+7:])
	var m gmMeta
	var uid imap.UID
	for k := 0; k < len(toks); k++ {
		switch strings.ToUpper(toks[k]) {
		case "UID":
			if k+1 < len(toks) {
				n, _ := strconv.ParseUint(toks[k+1], 10, 32)
				uid = imap.UID(n)
			}
		case "X-GM-THRID":
			if k+1 < len(toks) {
				if _, err := strconv.ParseUint(toks[k+1], 10, 64); err == nil {
					m.Thread = toks[k+1]
				}
			}
		case "X-GM-LABELS":
			if k+1 < len(toks) && toks[k+1] == "(" {
				for k += 2; k < len(toks) && toks[k] != ")"; k++ {
					m.Labels = append(m.Labels, labelName(toks[k]))
				}
			}
		}
	}
	return uid, m, uid != 0
}

// sexpTokens splits an IMAP list into "(", ")", atoms and the contents of quoted strings.
func sexpTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == ' ':
			i++
		case c == '(' || c == ')':
			out = append(out, string(c))
			i++
		case c == '"':
			var b strings.Builder
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
			out = append(out, b.String())
			i++
		default:
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '(' && s[j] != ')' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

// labelName turns Gmail's label as IMAP shows it into what the user sees.
func labelName(l string) string {
	switch strings.ToLower(l) {
	case "\\inbox":
		return "INBOX"
	case "\\important":
		return "Important"
	case "\\starred":
		return "Starred"
	case "\\sent":
		return "Sent"
	case "\\draft":
		return "Draft"
	}
	return fromUTF7(strings.TrimPrefix(l, "\\"))
}

// fromUTF7 decodes IMAP's modified UTF-7.
func fromUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		enc := s[i+1 : i+j]
		i += j
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.ReplaceAll(enc, ",", "/"))
		if err != nil || len(raw)%2 != 0 {
			b.WriteString("&" + enc + "-")
			continue
		}
		u := make([]uint16, len(raw)/2)
		for k := range u {
			u[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		b.WriteString(string(utf16.Decode(u)))
	}
	return b.String()
}

type literal []byte

func lit(s string) literal { return literal(s) }

// run sends one command and returns its untagged response lines.
func (rc *rawConn) run(cmd string, args ...literal) ([]string, error) {
	rc.tag++
	tag := "r" + strconv.Itoa(rc.tag)
	if _, err := io.WriteString(rc.c, tag+" "+cmd); err != nil {
		return nil, err
	}
	for _, a := range args {
		if _, err := fmt.Fprintf(rc.c, " {%d}\r\n", len(a)); err != nil {
			return nil, err
		}
		l, err := rc.line()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(l, "+") {
			return nil, errors.New(strings.TrimSpace(l))
		}
		if _, err := rc.c.Write(a); err != nil {
			return nil, err
		}
	}
	if _, err := io.WriteString(rc.c, "\r\n"); err != nil {
		return nil, err
	}
	var out []string
	for {
		l, err := rc.line()
		if err != nil {
			return nil, err
		}
		if rest, ok := strings.CutPrefix(l, tag+" "); ok {
			if strings.HasPrefix(strings.ToUpper(rest), "OK") {
				return out, nil
			}
			return out, errors.New(strings.TrimSpace(rest))
		}
		if len(out) < 10000 {
			out = append(out, l)
		}
	}
}

// line reads one response line, including any literal it carries.
func (rc *rawConn) line() (string, error) {
	var b strings.Builder
	for {
		l, err := rc.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		l = strings.TrimRight(l, "\r\n")
		b.WriteString(l)
		if b.Len() > 16<<20 { // a search over a huge mailbox lists many UIDs
			return "", errors.New("response too long")
		}
		i := strings.LastIndexByte(l, '{')
		if i < 0 || !strings.HasSuffix(l, "}") {
			return b.String(), nil
		}
		n, err := strconv.Atoi(strings.TrimSuffix(l[i+1:len(l)-1], "+"))
		if err != nil || n < 0 || n > 4<<20 {
			return b.String(), nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(rc.r, buf); err != nil {
			return "", err
		}
		b.Write(buf)
	}
}

// utf7 encodes a folder name in IMAP's modified UTF-7 (RFC 3501 5.1.3).
func utf7(s string) string {
	var b strings.Builder
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		u := utf16.Encode(run)
		buf := make([]byte, 2*len(u))
		for i, c := range u {
			buf[2*i], buf[2*i+1] = byte(c>>8), byte(c)
		}
		b.WriteString("&" + strings.ReplaceAll(base64.RawStdEncoding.EncodeToString(buf), "/", ",") + "-")
		run = nil
	}
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				b.WriteString("&-")
			} else {
				b.WriteRune(r)
			}
		} else {
			run = append(run, r)
		}
	}
	flush()
	return b.String()
}

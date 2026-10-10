package mailkit

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/emersion/go-imap/v2"
)

// Gmail's own search (X-GM-RAW) takes a query exactly as typed in the Gmail search box. go-imap doesn't
// speak it, so a short separate connection runs just LOGIN, EXAMINE and UID SEARCH. Every argument goes as
// a literal, so nothing the agent writes can break out into another command.

// rawSearch runs a Gmail query in box and returns the matching UIDs (a seam for tests).
var rawSearch = gmailRawSearch

type rawConn struct {
	c   net.Conn
	r   *bufio.Reader
	tag int
}

func gmailRawSearch(s Settings, password, box, query string) ([]imap.UID, error) {
	conn, err := dialRaw(s.IMAPAddr)
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %w", prov.Name, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	rc := &rawConn{c: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	if _, err := rc.line(); err != nil { // greeting
		return nil, err
	}
	if _, err := rc.run("LOGIN", lit(s.Address), lit(password)); err != nil {
		return nil, authError{}
	}
	defer rc.run("LOGOUT")
	if _, err := rc.run("EXAMINE", lit(utf7(box))); err != nil {
		return nil, fmt.Errorf("can't open folder %q: %w", box, err)
	}
	lines, err := rc.run("UID SEARCH CHARSET UTF-8 X-GM-RAW", lit(query))
	if err != nil {
		return nil, fmt.Errorf("Gmail didn't accept the search: %w", err)
	}
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
	return uids, nil
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

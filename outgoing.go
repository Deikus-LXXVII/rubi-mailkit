package mailkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Outgoing mail beyond plain text: attachments, a formatted (HTML) version, forwarding, and drafts that
// can be listed, edited, deleted and sent.
//
// Files come from the agent as base64 (the plugin never reads a path the agent names: that could be any
// file on the machine), or are taken from other emails. The agent can only pass on attachments it couldn't
// open itself when the user reviews the email before it goes out, or when it only becomes a draft.

type attachIn struct {
	Filename      string `json:"filename,omitempty" jsonschema:"file name; for an attachment of an email, default its own"`
	ContentType   string `json:"content_type,omitempty" jsonschema:"e.g. application/pdf; default from the file name"`
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"the file, base64"`
	FromUID       uint32 `json:"from_uid,omitempty" jsonschema:"or take an attachment of this email"`
	FromMailbox   string `json:"from_mailbox,omitempty" jsonschema:"that email's folder, default INBOX"`
	Index         int    `json:"index,omitempty" jsonschema:"which of its attachments, 1 for the first (as read lists them)"`
}

type outFile struct {
	name, ctype string
	data        []byte
}

const (
	maxAgentFiles = 2 << 20  // what the agent can pass in one call (plugin messages are at most 4 MB)
	maxOutFiles   = 20 << 20 // all attachments of one email (25 MB at most servers, after base64)
	stashOver     = 1 << 20  // bigger emails wait for approval on disk, not in the approval itself
)

// passOn reports whether attachments the agent can't open may go into an outgoing email: when it is only
// a draft, when the user approves every email before it is sent, or when the agent may open them anyway.
func passOn(h host, draftOnly bool) bool {
	return draftOnly || h.Level(kindSend) != rubiplugin.None || attachmentMode(h) == "free"
}

// resolveFiles turns the attachment requests into files.
func (x *integration) resolveFiles(h host, c *imapclient.Client, sp specials, list []attachIn, draftOnly bool) ([]outFile, error) {
	var out []outFile
	agent, total := 0, 0
	for i, a := range list {
		var f outFile
		switch {
		case a.ContentBase64 != "" && a.FromUID != 0:
			return nil, fmt.Errorf("attachment %d: give content_base64 or from_uid, not both", i+1)
		case a.ContentBase64 != "":
			data, err := decodeB64(a.ContentBase64)
			if err != nil {
				return nil, fmt.Errorf("attachment %d isn't valid base64", i+1)
			}
			if agent += len(data); agent > maxAgentFiles {
				return nil, errors.New("files from you can be 2 MB in all; attach bigger ones from emails (from_uid)")
			}
			if strings.TrimSpace(a.Filename) == "" {
				return nil, fmt.Errorf("attachment %d needs a filename", i+1)
			}
			f = outFile{name: safeName(a.Filename), data: data}
		case a.FromUID != 0:
			if attachmentMode(h) == "never" || !passOn(h, draftOnly) {
				return nil, errors.New("the user's settings don't let you pass on attachments of emails here; save it as a draft (" + tool("draft") + ") for the user to finish")
			}
			box := sp.resolve(a.FromMailbox)
			if err := x.folderAllowed(h, box); err != nil {
				return nil, err
			}
			raw, err := fetchRaw(c, box, a.FromUID)
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
			idx := a.Index
			if idx == 0 && len(m.Attachments) == 1 {
				idx = 1
			}
			if idx < 1 || idx > len(m.Attachments) {
				return nil, fmt.Errorf("email %d has %d attachments; give index", a.FromUID, len(m.Attachments))
			}
			data, err := attachmentBody(raw, idx)
			if err != nil {
				return nil, err
			}
			src := m.Attachments[idx-1]
			f = outFile{name: safeName(orDefault(strings.TrimSpace(a.Filename), src.Filename)), ctype: src.ContentType, data: data}
		default:
			return nil, fmt.Errorf("attachment %d: give content_base64 or from_uid", i+1)
		}
		if a.ContentType != "" {
			f.ctype = a.ContentType
		}
		if f.ctype == "" {
			f.ctype = mime.TypeByExtension(strings.ToLower(filepath.Ext(f.name)))
		}
		if mt, _, err := mime.ParseMediaType(f.ctype); err != nil || !strings.Contains(mt, "/") {
			f.ctype = "application/octet-stream"
		} else {
			f.ctype = mt
		}
		if total += len(f.data); total > maxOutFiles {
			return nil, errors.New("the attachments are larger than 20 MB in all")
		}
		out = append(out, f)
	}
	if len(out) > 20 {
		return nil, errors.New("at most 20 attachments")
	}
	return out, nil
}

func decodeB64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if i := strings.Index(s, ";base64,"); strings.HasPrefix(s, "data:") && i > 0 {
		s = s[i+8:]
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// writeBody writes the Content-Type headers and body: plain text, plus the HTML version and attachments
// when there are any.
func writeBody(b *bytes.Buffer, text, htmlBody string, files []outFile) error {
	if htmlBody == "" && len(files) == 0 {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		return writeQP(b, text)
	}
	mixed := boundary()
	alt := boundary()
	textPart := func() error {
		if htmlBody == "" {
			b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
			return writeQP(b, text)
		}
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + alt + "\"\r\n\r\n")
		b.WriteString("--" + alt + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQP(b, text); err != nil {
			return err
		}
		b.WriteString("\r\n--" + alt + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQP(b, htmlBody); err != nil {
			return err
		}
		b.WriteString("\r\n--" + alt + "--\r\n")
		return nil
	}
	if len(files) == 0 {
		return textPart()
	}
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + mixed + "\"\r\n\r\n--" + mixed + "\r\n")
	if err := textPart(); err != nil {
		return err
	}
	for _, f := range files {
		b.WriteString("\r\n--" + mixed + "\r\n")
		b.WriteString("Content-Type: " + mime.FormatMediaType(f.ctype, map[string]string{"name": f.name}) + "\r\n")
		b.WriteString("Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": f.name}) + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(f.data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--" + mixed + "--\r\n")
	return nil
}

func writeQP(b *bytes.Buffer, s string) error {
	qp := quotedprintable.NewWriter(b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return err
	}
	return qp.Close()
}

func boundary() string {
	r := make([]byte, 12)
	_, _ = rand.Read(r)
	return "rubi-" + hex.EncodeToString(r)
}

func fileList(files []outFile) string {
	var parts []string
	for _, f := range files {
		parts = append(parts, f.name+" ("+humanSize(len(f.data))+")")
	}
	return strings.Join(parts, ", ")
}

// ---- big emails wait on disk ----

// stash keeps a big composed email in the plugin's private folder while it waits for approval; the
// approval carries only its name and hash, so exactly the approved email goes out.
func stash(m *mailPayload) error {
	if len(m.Raw) <= stashOver {
		return nil
	}
	dir, err := privateDir("outbox", 8*24*time.Hour)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "message.eml"), m.Raw, 0o600); err != nil {
		os.RemoveAll(dir)
		return err
	}
	sum := sha256.Sum256(m.Raw)
	m.RawFile, m.RawHash, m.Raw = filepath.Base(dir), hex.EncodeToString(sum[:]), nil
	return nil
}

var reHexName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// body returns the email, from the approval or from the outbox.
func (m mailPayload) body() ([]byte, error) {
	if m.RawFile == "" {
		return m.Raw, nil
	}
	if !reHexName.MatchString(m.RawFile) {
		return nil, errors.New("bad outbox name")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(home, "outbox", m.RawFile, "message.eml"))
	if err != nil {
		return nil, errors.New("the email waited too long and was removed; ask again")
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != m.RawHash {
		return nil, errors.New("the stored email changed; ask again")
	}
	return raw, nil
}

// dropStash removes a stored email once it was sent or saved.
func (m mailPayload) dropStash() {
	if m.RawFile == "" || !reHexName.MatchString(m.RawFile) {
		return
	}
	if home, err := os.UserHomeDir(); err == nil {
		os.RemoveAll(filepath.Join(home, "outbox", m.RawFile))
	}
}

// ---- forwarding ----

type forwardIn struct {
	UID                uint32   `json:"uid" jsonschema:"the email to forward"`
	Mailbox            string   `json:"mailbox,omitempty" jsonschema:"its folder, default INBOX"`
	To                 []string `json:"to"`
	Cc                 []string `json:"cc,omitempty"`
	Bcc                []string `json:"bcc,omitempty"`
	Body               string   `json:"body,omitempty" jsonschema:"your note above the forwarded email"`
	WithoutAttachments bool     `json:"without_attachments,omitempty" jsonschema:"leave its attachments out"`
	DraftOnly          bool     `json:"draft_only,omitempty" jsonschema:"save it as a draft instead of sending"`
	TrackDays          int      `json:"track_days,omitempty" jsonschema:"days to watch for replies if the user picks 'Send and notify on reply' (default 14, max 60)"`
	Account            string   `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

var reFwd = regexp.MustCompile(`(?i)^\s*(fwd?|пересл)\s*:`)
var reScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)

func (x *integration) forward(ctx context.Context, h host, in forwardIn) (any, error) {
	if in.UID == 0 {
		return nil, errors.New("give the uid of the email to forward")
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
	if err != nil {
		logout(c)
		return nil, err
	}
	orig, err := parseMessage(raw, -1)
	if err != nil {
		logout(c)
		return nil, err
	}
	if hidden, _ := privacyOf(h).hidden(orig.From, orig.Subject, orig.Text); hidden {
		logout(c)
		return nil, errors.New("that email is private (the user's privacy filter); ask to reveal it first")
	}
	d := draft{To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: orig.Subject}
	if !reFwd.MatchString(d.Subject) {
		d.Subject = "Fwd: " + d.Subject
	}
	head := "---------- Forwarded message ---------\nFrom: " + orig.From + "\nDate: " + orig.Date + "\nSubject: " + orig.Subject + "\nTo: " + orig.To
	if orig.Cc != "" {
		head += "\nCc: " + orig.Cc
	}
	d.Body = strings.TrimSpace(in.Body + "\n\n" + head + "\n\n" + orig.Text)
	if orig.html != "" {
		note := strings.ReplaceAll(html.EscapeString(strings.TrimSpace(in.Body)), "\n", "<br>")
		d.HTMLBody = "<div>" + note + "</div><br><div>" + strings.ReplaceAll(html.EscapeString(head), "\n", "<br>") + "</div><br>" +
			reScript.ReplaceAllString(orig.html, "")
	}
	note := ""
	if !in.WithoutAttachments && len(orig.Attachments) > 0 {
		if attachmentMode(h) != "never" && passOn(h, in.DraftOnly) {
			for i := range orig.Attachments {
				d.Attachments = append(d.Attachments, attachIn{FromUID: in.UID, FromMailbox: box, Index: i + 1})
			}
		} else {
			note = "Forwarded without its attachments: the user's settings don't let you pass them on. Use draft_only to let the user add them."
		}
	}
	if d.files, err = x.resolveFiles(h, c, sp, d.Attachments, in.DraftOnly); err != nil {
		logout(c)
		return nil, err
	}
	logout(c)
	msg, err := compose(s, d, "", strings.TrimSpace(orig.References+" "+orig.MessageID))
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if in.DraftOnly {
		r, err := x.storeDraft(ctx, h, s, msg, d.Body, 0)
		if err != nil {
			return nil, err
		}
		res, _ = r.(map[string]any)
	} else {
		r, err := x.askSend(ctx, h, s, msg, d.Body, in.TrackDays, 0)
		if err != nil {
			return nil, err
		}
		res = r
	}
	if note != "" && res != nil {
		res["note"] = note
	}
	return res, nil
}

// ---- drafts ----

type draftIn struct {
	draft
	ReplaceUID uint32 `json:"replace_uid,omitempty" jsonschema:"edit: the uid of a draft this one replaces (the old one goes to the trash)"`
}

type draftRef struct {
	UID       uint32 `json:"uid" jsonschema:"the draft (from drafts)"`
	TrackDays int    `json:"track_days,omitempty" jsonschema:"for send_draft: days to watch for replies if the user picks 'Send and notify on reply' (default 14, max 60)"`
	Account   string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

// storeDraft saves a composed draft now, or asks first if the user requires that.
func (x *integration) storeDraft(ctx context.Context, h host, s Settings, msg *composed, body string, replace uint32) (any, error) {
	if msg.bcc != "" { // a draft keeps its Bcc (taken out again when it is sent)
		msg.raw = append([]byte("Bcc: "+msg.bcc+"\r\n"), msg.raw...)
	}
	p := payloadOf(msg, 0)
	p.ReplaceUID = replace
	if err := stash(&p); err != nil {
		return nil, err
	}
	if h.Level(kindDraft) == rubiplugin.None {
		return x.saveDraft(h, p)
	}
	pv := preview(s, msg, body)
	if replace != 0 {
		pv["replaces"] = fmt.Sprintf("draft %d (moved to the trash)", replace)
	}
	return h.Submit(ctx, rubiplugin.Request{Kind: kindDraft, Summary: "Save draft to " + msg.to + ": \"" + msg.subject + "\"",
		Preview: pv, Options: []rubiplugin.Option{{Key: "save", Label: "Save draft"}}, Payload: p})
}

// askSend asks the user to approve sending a composed email.
func (x *integration) askSend(ctx context.Context, h host, s Settings, msg *composed, body string, trackDays int, draftUID uint32) (map[string]any, error) {
	days := trackDays
	if days <= 0 {
		days = s.TrackDays
	}
	days = min(max(days, 1), 60)
	p := payloadOf(msg, days)
	p.DraftUID = draftUID
	if err := stash(&p); err != nil {
		return nil, err
	}
	return h.Submit(ctx, rubiplugin.Request{Kind: kindSend,
		Summary:  "Send email to " + msg.to + ": \"" + msg.subject + "\"",
		Question: "Notify you when a reply arrives?",
		Preview:  preview(s, msg, body),
		Options:  sendOptions,
		Payload:  p})
}

// sendDraft asks to send a draft exactly as it is in the Drafts folder.
func (x *integration) sendDraft(ctx context.Context, h host, in draftRef) (any, error) {
	if in.UID == 0 {
		return nil, errors.New("give the draft's uid (from " + tool("drafts") + ")")
	}
	c, s, err := session(h)
	if err != nil {
		return nil, err
	}
	defer logout(c)
	if err := x.folderAllowed(h, s.Drafts); err != nil {
		return nil, err
	}
	raw, err := fetchRaw(c, s.Drafts, in.UID)
	if err != nil {
		return nil, err
	}
	m, err := parseMessage(raw, 0)
	if err != nil {
		return nil, err
	}
	if hidden, _ := privacyOf(h).hidden(m.From, m.Subject, m.Text); hidden {
		return nil, errors.New("that draft is private (the user's privacy filter)")
	}
	msg, err := fromDraft(s, raw)
	if err != nil {
		return nil, err
	}
	return x.askSend(ctx, h, s, msg, m.Text, in.TrackDays, in.UID)
}

// fromDraft prepares a saved draft for sending: recipients from its headers, Bcc taken out of what
// recipients see, a Message-ID and Date if it has none.
func fromDraft(s Settings, raw []byte) (*composed, error) {
	e, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && !gomessage.IsUnknownCharset(err) {
		return nil, fmt.Errorf("can't parse the draft: %w", err)
	}
	h := mail.Header{Header: e.Header}
	list := func(key string) []*mail.Address {
		as, _ := h.AddressList(key)
		return as
	}
	to, cc, bcc := list("To"), list("Cc"), list("Bcc")
	if len(to)+len(cc)+len(bcc) == 0 {
		return nil, errors.New("the draft has no recipients")
	}
	if n := len(to) + len(cc) + len(bcc); n > 20 {
		return nil, fmt.Errorf("too many recipients (%d, max 20)", n)
	}
	m := &composed{}
	for _, a := range append(append(append([]*mail.Address{}, to...), cc...), bcc...) {
		m.envelope = append(m.envelope, a.Address)
	}
	join := func(as []*mail.Address) string {
		var p []string
		for _, a := range as {
			p = append(p, a.String())
		}
		return strings.Join(p, ", ")
	}
	m.to, m.cc, m.bcc = join(to), join(cc), join(bcc)
	m.subject, _ = h.Subject()
	h.Del("Bcc")
	if id, _ := h.MessageID(); id == "" {
		h.GenerateMessageIDWithHostname(orDefault(domainOf(s.Address), prov.MailDomain))
	}
	id, _ := h.MessageID()
	m.messageID = "<" + id + ">"
	if !h.Has("Date") {
		h.SetDate(time.Now())
	}
	if !h.Has("From") {
		h.SetAddressList("From", []*mail.Address{{Name: s.FromName, Address: s.Address}})
	}
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, h.Header.Header); err != nil {
		return nil, err
	}
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		b.Write(raw[i+4:])
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		b.Write(raw[i+2:])
	}
	m.raw = b.Bytes()
	return m, nil
}

func domainOf(addr string) string {
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		return addr[at+1:]
	}
	return ""
}

// afterDraftSent removes a draft that was just sent (it is in Sent now).
func afterDraftSent(c *imapclient.Client, s Settings, m mailPayload) {
	if m.DraftUID == 0 {
		return
	}
	if _, err := c.Select(s.Drafts, nil).Wait(); err != nil {
		return
	}
	set := imap.UIDSetNum(imap.UID(m.DraftUID))
	if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err == nil {
		_ = c.UIDExpunge(set).Close()
	}
}

// ---- source view ----

// sourceView shows a message's source: every header (Received, DKIM, Authentication-Results, …) and the
// MIME structure, with text parts decoded and attachment contents left out (they open only through the
// attachment tool, under the user's setting).
func sourceView(raw []byte, showNames bool, maxChars int) (string, bool) {
	var b strings.Builder
	e, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && !gomessage.IsUnknownCharset(err) {
		return "", false
	}
	var walk func(e *gomessage.Entity, depth int)
	walk = func(e *gomessage.Entity, depth int) {
		if b.Len() > 4*maxChars {
			return
		}
		mt, params, _ := e.Header.ContentType()
		disp, dparams, _ := e.Header.ContentDisposition()
		attached := disp == "attachment" || dparams["filename"] != "" || params["name"] != "" && !strings.HasPrefix(mt, "text/")
		if depth > 0 {
			b.WriteString("\n")
		}
		if attached && !showNames {
			b.WriteString("[an attachment: hidden by the user's setting]\n")
			return
		}
		var hb bytes.Buffer
		_ = textproto.WriteHeader(&hb, e.Header.Header)
		b.WriteString(strings.ReplaceAll(hb.String(), "\r\n", "\n"))
		if mr := e.MultipartReader(); mr != nil {
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				walk(p, depth+1)
			}
			return
		}
		if attached || !strings.HasPrefix(mt, "text/") {
			n, _ := io.Copy(io.Discard, io.LimitReader(e.Body, 100<<20))
			b.WriteString(fmt.Sprintf("[%s, %s: contents left out; open attachments with %s]\n", orDefault(mt, "data"), humanSize(int(n)), tool("attachment")))
			return
		}
		body, _ := io.ReadAll(io.LimitReader(e.Body, 1<<20))
		b.WriteString(strings.ReplaceAll(string(body), "\r\n", "\n"))
		b.WriteString("\n")
	}
	walk(e, 0)
	out := b.String()
	if r := []rune(out); len(r) > maxChars {
		return string(r[:maxChars]), true
	}
	return out, false
}

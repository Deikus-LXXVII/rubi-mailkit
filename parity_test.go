package mailkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/mail"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

func TestParseQuery(t *testing.T) {
	alts, err := parseQuery(`from:anna subject:"big invoice" -is:read larger:5M OR smaller:10K "exact words" newer_than:7d`)
	if err != nil {
		t.Fatal(err)
	}
	if len(alts) != 6 || alts[1][0].value != "big invoice" || !alts[2][0].neg || len(alts[3]) != 2 || alts[4][0].key != "" {
		t.Fatalf("%+v", alts)
	}
	if !byContent(alts) {
		t.Fatal("subject: and words look at content")
	}
	headerOnly, _ := parseQuery("from:anna is:unread newer_than:2d has:attachment in:inbox")
	if byContent(headerOnly) {
		t.Fatal("from/is/newer_than/has:attachment don't look at content")
	}
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	crit, err := toCriteria(alts, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(crit.Header) != 2 || len(crit.Not) != 1 || len(crit.Or) != 1 || crit.Or[0][0].Larger != 5<<20 ||
		crit.Or[0][1].Smaller != 10<<10 || len(crit.Text) != 1 || !crit.Since.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("%+v", crit)
	}
	if _, err := toCriteria(must(parseQuery("category:promotions")), now); err == nil || !strings.Contains(err.Error(), "only in Gmail") {
		t.Fatalf("category: %v", err)
	}
	for _, bad := range []string{"OR x", "x OR", "(a b)"} {
		if _, err := parseQuery(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if utf7("Входящие & co") != "&BBIERQQ+BDQETwRJBDgENQ- &- co" {
		t.Fatalf("utf7: %s", utf7("Входящие & co"))
	}
}

func TestQuerySearchAndThreads(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Trip plans\nMessage-ID: <t1@example.com>\nDate: Wed, 07 Oct 2026 10:00:00 +0000\n\nShall we go to Riga?\n")
	deliver(t, addr, "Sent Messages", "From: me@icloud.com\nTo: anna@example.com\nSubject: Re: Trip plans\nMessage-ID: <t2@icloud.com>\nIn-Reply-To: <t1@example.com>\nReferences: <t1@example.com>\nDate: Wed, 07 Oct 2026 11:00:00 +0000\n\nYes, Riga in May.\n")
	deliver(t, addr, "Archive", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Re: Trip plans\nMessage-ID: <t3@example.com>\nIn-Reply-To: <t2@icloud.com>\nReferences: <t1@example.com> <t2@icloud.com>\nDate: Wed, 07 Oct 2026 12:00:00 +0000\n\nBooked!\n")
	deliver(t, addr, "INBOX", "From: Bob <bob@example.com>\nTo: me@icloud.com\nSubject: Lunch\nMessage-ID: <b1@example.com>\nDate: Thu, 08 Oct 2026 10:00:00 +0000\n\nLunch tomorrow?\n")
	deliver(t, addr, "Deleted Messages", "From: Anna <anna@example.com>\nSubject: Old\nMessage-ID: <old@example.com>\n\nold\n")

	res, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "from:anna OR from:bob"}})
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, res)
	msgs, threads := m["messages"].([]any), m["threads"].([]any)
	if len(msgs) != 3 || len(threads) != 2 {
		t.Fatalf("all folders but trash, two threads: %v", m)
	}
	trip := threads[1].(map[string]any)
	if trip["subject"] != "Trip plans" || trip["count"].(float64) != 2 {
		t.Fatalf("thread: %v", trip)
	}
	// The thread tool finds the whole conversation, the reply in Sent included, oldest first.
	var archived uint32
	for _, v := range msgs {
		if mm := v.(map[string]any); mm["mailbox"] == "Archive" {
			archived = uint32(mm["uid"].(float64))
		}
	}
	res, err = x.doThread(h, threadIn{UID: archived, Mailbox: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	conv := asMap(t, res)["messages"].([]any)
	if len(conv) != 3 || !strings.Contains(conv[0].(map[string]any)["text"].(string), "Riga?") ||
		conv[1].(map[string]any)["mailbox"] != "Sent Messages" {
		t.Fatalf("thread: %v", conv)
	}
	// in:trash looks there; in:anywhere everywhere.
	res, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "in:trash from:anna"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 1 {
		t.Fatalf("in:trash: %v", got)
	}
	res, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "subject:lunch -from:anna"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 1 || got[0].(map[string]any)["from"] != "Bob <bob@example.com>" {
		t.Fatalf("minus: %v", got)
	}
	// Closed folders aren't searched.
	h.config = map[string]any{"folder_access": "selected", "folders": []string{"INBOX"}}
	res, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "from:anna"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 1 {
		t.Fatalf("closed folders searched: %v", got)
	}
}

func TestQueryDoesNotFindPrivateMail(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: noreply@bank.example\nSubject: 482913 is your code\nMessage-ID: <p1@bank.example>\n\nYour verification code is 482913\n")
	res, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "482913"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 0 {
		t.Fatalf("a content query found private mail: %v", got)
	}
	res, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "from:bank.example"}})
	got := asMap(t, res)["messages"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["private"] != true || got[0].(map[string]any)["subject"] != "" {
		t.Fatalf("a sender query shows private mail as sender only: %v", got)
	}
}

func TestGmailRawSearchProtocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var seen []string
		io.WriteString(conn, "* OK Gimap ready\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- seen
				return
			}
			line = strings.TrimRight(line, "\r\n")
			// Read the literals that follow.
			for strings.HasSuffix(line, "}") {
				i := strings.LastIndex(line, "{")
				n := 0
				for _, c := range line[i+1 : len(line)-1] {
					n = n*10 + int(c-'0')
				}
				io.WriteString(conn, "+ go ahead\r\n")
				buf := make([]byte, n)
				io.ReadFull(r, buf)
				rest, _ := r.ReadString('\n')
				line = line[:i] + "<" + string(buf) + ">" + strings.TrimRight(rest, "\r\n")
			}
			seen = append(seen, line)
			tag := strings.Fields(line)[0]
			switch {
			case strings.Contains(line, "UID SEARCH"):
				io.WriteString(conn, "* SEARCH 4 9 12\r\n"+tag+" OK SEARCH completed\r\n")
			case strings.Contains(line, "LOGOUT"):
				io.WriteString(conn, "* BYE\r\n"+tag+" OK\r\n")
				got <- seen
				return
			default:
				io.WriteString(conn, tag+" OK\r\n")
			}
		}
	}()
	old := dialRaw
	dialRaw = func(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }
	defer func() { dialRaw = old }()
	uids, err := gmailRawSearch(Settings{IMAPAddr: ln.Addr().String(), Address: "me@gmail.com"}, `pa"ss`, "[Gmail]/Вся почта",
		"category:promotions\r\nr9 DELETE INBOX")
	if err != nil || len(uids) != 3 || uids[2] != 12 {
		t.Fatalf("%v %v", uids, err)
	}
	lines := <-got
	want := []string{`r1 LOGIN <me@gmail.com> <pa"ss>`, "r2 EXAMINE <[Gmail]/&BBIEQQRP- &BD8EPgRHBEIEMA->",
		"r3 UID SEARCH CHARSET UTF-8 X-GM-RAW <category:promotions\r\nr9 DELETE INBOX>", "r4 LOGOUT"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n%q\nwant\n%q", lines, want)
	}
}

func TestGmailQuery(t *testing.T) {
	p := testProvider
	p.Gmail, p.ID, p.ToolPrefix = true, "gmail", "gmail"
	use(p)
	defer use(testProvider)
	x, h, addr, _ := setup(t)
	c, _ := dialIMAP(addr)
	_ = c.Login(testUser, testPass).Wait()
	_ = c.Create("[Gmail]/All Mail", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrAll}}).Wait()
	_ = c.Create("Work", nil).Wait()
	logout(c)
	deliver(t, addr, "[Gmail]/All Mail", "From: shop@example.com\nSubject: Sale\nMessage-ID: <s1@example.com>\n\nsale\n")
	deliver(t, addr, "[Gmail]/All Mail", "From: anna@example.com\nSubject: Hi\nMessage-ID: <a1@example.com>\n\nhi\n")
	var box, query string
	rawSearch = func(_ Settings, pw, b, q string) ([]imap.UID, error) {
		box, query = b, q
		return []imap.UID{1}, nil
	}
	defer func() { rawSearch = gmailRawSearch }()
	res, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Query: "category:promotions", UnseenOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	got := asMap(t, res)["messages"].([]any)
	if box != "[Gmail]/All Mail" || query != "category:promotions is:unread" || len(got) != 1 || got[0].(map[string]any)["subject"] != "Sale" {
		t.Fatalf("box=%q query=%q %v", box, query, got)
	}

	// Labels: adding copies into the label's folder, removing deletes that copy; both undo.
	res, err = x.doManage(h, manageOp{Op: "label", Mailbox: "[Gmail]/All Mail", UIDs: []uint32{2}, Add: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}
	undoID := asMap(t, res)["undo_id"].(string)
	if n := countIn(t, addr, "Work"); n != 1 {
		t.Fatalf("label added: %d", n)
	}
	if _, err := x.doManage(h, manageOp{Op: "undo", UndoID: undoID}); err != nil {
		t.Fatal(err)
	}
	if n := countIn(t, addr, "Work"); n != 0 {
		t.Fatalf("label undone: %d", n)
	}
	// Archiving from All Mail never takes mail out of it (that would bin it on Gmail).
	if _, err := x.doManage(h, manageOp{Op: "move", Mailbox: "[Gmail]/All Mail", UIDs: []uint32{2}, To: "Work"}); err != nil {
		t.Fatal(err)
	}
	if countIn(t, addr, "[Gmail]/All Mail") != 2 || countIn(t, addr, "Work") != 1 {
		t.Fatal("moving out of All Mail must copy")
	}
	if _, err := x.doManage(h, manageOp{Op: "label", Mailbox: "Work", UIDs: []uint32{uidOf(t, h, "Work", "Hi")}, Remove: []string{"[Gmail]/All Mail"}}); err == nil {
		t.Fatal("All Mail isn't a label to remove")
	}
}

func uidOf(t *testing.T, h host, box, subject string) uint32 {
	t.Helper()
	c, _, err := session(h)
	if err != nil {
		t.Fatal(err)
	}
	defer logout(c)
	msgs, err := search(c, searchQuery{Mailbox: box, Subject: subject})
	if err != nil || len(msgs) == 0 {
		t.Fatalf("no %q in %s: %v", subject, box, err)
	}
	return msgs[0].UID
}

func countIn(t *testing.T, addr, box string) int {
	t.Helper()
	c, err := dialIMAP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer logout(c)
	_ = c.Login(testUser, testPass).Wait()
	msgs, err := search(c, searchQuery{Mailbox: box, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

func TestOrganizeAndUndo(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: shop@example.com\nSubject: Sale\nMessage-ID: <s1@example.com>\n\nsale\n")
	deliver(t, addr, "INBOX", "From: noreply@bank.example\nSubject: Code\nMessage-ID: <c1@bank.example>\n\nYour verification code is 482913\n")
	deliver(t, addr, "INBOX", "From: anna@example.com\nSubject: Hi\nMessage-ID: <a1@example.com>\n\nhi\n")

	res, err := x.manage(context.Background(), h, manageOp{Op: "move", UIDs: []uint32{1, 2}, To: "trash"})
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, res)
	if m["changed"].(float64) != 1 || m["skipped_private"] == nil {
		t.Fatalf("private mail is left alone: %v", m)
	}
	if countIn(t, addr, "Deleted Messages") != 1 || countIn(t, addr, "INBOX") != 2 {
		t.Fatal("not moved to the trash")
	}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "undo", UndoID: m["undo_id"].(string)}); err != nil {
		t.Fatal(err)
	}
	if countIn(t, addr, "Deleted Messages") != 0 || countIn(t, addr, "INBOX") != 3 {
		t.Fatal("undo didn't bring it back")
	}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "undo", UndoID: m["undo_id"].(string)}); err == nil {
		t.Fatal("undo twice")
	}

	// Marks, and their undo.
	res, err = x.manage(context.Background(), h, manageOp{Op: "mark", UIDs: []uint32{3}, As: "starred"})
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ := session(h)
	got, _ := search(c, searchQuery{Subject: "Hi"})
	logout(c)
	if len(got) != 1 || !got[0].Starred {
		t.Fatalf("starred: %+v", got)
	}
	_, _ = x.manage(context.Background(), h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)})
	c, _, _ = session(h)
	got, _ = search(c, searchQuery{Subject: "Hi"})
	logout(c)
	if got[0].Starred {
		t.Fatal("unstar on undo")
	}

	// Approval when the user wants it.
	h.levels = map[string]rubiplugin.Level{kindOrganize: rubiplugin.Strong}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "move", UIDs: []uint32{3}, To: "archive"}); err != nil || len(h.pending) != 1 ||
		!strings.Contains(h.pending[0].Summary, "Archive") || !strings.Contains(h.pending[0].Preview.(map[string]any)["messages"].(string), "anna@example.com: Hi") {
		t.Fatalf("approval: %v %+v", err, h.pending)
	}
	h.levels = nil

	// Folders: create, rename, undo; a folder with mail isn't deleted; the mailbox's own folders stay.
	res, err = x.manage(context.Background(), h, manageOp{Op: "folder", Folder: "create", Name: "Receipts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "move", UIDs: []uint32{uidOf(t, h, "INBOX", "Sale")}, To: "receipts"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.doManage(h, manageOp{Op: "folder", Folder: "delete", Name: "Receipts"}); err == nil || !strings.Contains(err.Error(), "move them out") {
		t.Fatalf("delete with mail: %v", err)
	}
	res, err = x.manage(context.Background(), h, manageOp{Op: "folder", Folder: "rename", Name: "Receipts", NewName: "Bills"})
	if err != nil || countIn(t, addr, "Bills") != 1 {
		t.Fatalf("rename: %v", err)
	}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)}); err != nil || countIn(t, addr, "Receipts") != 1 {
		t.Fatalf("undo rename: %v", err)
	}
	if _, err := x.doManage(h, manageOp{Op: "folder", Folder: "rename", Name: "trash", NewName: "x"}); err == nil {
		t.Fatal("renamed the trash")
	}
	h.levels = map[string]rubiplugin.Level{kindFolderDelete: rubiplugin.Strong} // its default
	if _, err := x.manage(context.Background(), h, manageOp{Op: "folder", Folder: "delete", Name: "Receipts"}); err != nil {
		t.Fatal(err)
	}
	if len(h.pending) != 2 || h.pending[1].Kind != kindFolderDelete {
		t.Fatalf("deleting a folder always asks: %+v", h.pending)
	}
}

func TestAttachmentsForwardAndDrafts(t *testing.T) {
	x, h, addr, sent := setup(t)
	h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.Strong}
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Report\nMessage-ID: <r1@example.com>\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/html\n\n<p>See the <b>report</b></p>\n--b\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=report.pdf\nContent-Transfer-Encoding: base64\n\nJVBERi0xLjQK\n--b--\n")

	// Send with a file from the agent and an attachment of another email.
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 mine"))
	_, err := x.requestSend(context.Background(), h, sendIn{draft: draft{To: []string{"bob@example.com"}, Subject: "Files", Body: "Here.",
		HTMLBody: "<p>Here.</p>", Attachments: []attachIn{{Filename: "мой отчёт.pdf", ContentBase64: pdf}, {FromUID: 1, Index: 1}}}})
	if err != nil || len(h.pending) != 1 {
		t.Fatalf("send: %v", err)
	}
	if a := h.pending[0].Preview.(map[string]string)["attachments"]; !strings.Contains(a, "мой отчёт.pdf") || !strings.Contains(a, "report.pdf") {
		t.Fatalf("preview: %q", a)
	}
	if _, err := x.send(h, h.pending[0].Payload.(mailPayload), false); err != nil {
		t.Fatal(err)
	}
	m, err := parseMessage([]byte((*sent)[0].raw), 0)
	if err != nil || m.Text != "Here." || !m.HasHTML || len(m.Attachments) != 2 || m.Attachments[0].Filename != "мой отчёт.pdf" {
		t.Fatalf("sent: %+v %v", m, err)
	}
	if body, _ := attachmentBody([]byte((*sent)[0].raw), 2); string(body) != "%PDF-1.4\n" {
		t.Fatalf("attachment of the email: %q", body)
	}

	// Forward: note, the original's text and HTML, its attachment, a Fwd: subject.
	if _, err := x.forward(context.Background(), h, forwardIn{UID: 1, To: []string{"bob@example.com"}, Body: "FYI"}); err != nil {
		t.Fatal(err)
	}
	fw := h.pending[1]
	if fw.Kind != kindSend || fw.Preview.(map[string]string)["subject"] != "Fwd: Report" {
		t.Fatalf("forward: %+v", fw)
	}
	raw, _ := fw.Payload.(mailPayload).body()
	fm, _ := parseMessage(raw, 0)
	if !strings.HasPrefix(fm.Text, "FYI") || !strings.Contains(fm.Text, "From: Anna <anna@example.com>") || !strings.Contains(fm.Text, "See the report") ||
		len(fm.Attachments) != 1 || !strings.Contains(string(raw), "References: <r1@example.com>") {
		t.Fatalf("forwarded: %+v", fm)
	}
	// When the user doesn't review sending and attachments need asking, attachments stay behind.
	h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.None}
	res, err := x.forward(context.Background(), h, forwardIn{UID: 1, To: []string{"bob@example.com"}})
	if err != nil || !strings.Contains(asMap(t, res)["note"].(string), "without its attachments") {
		t.Fatalf("forward unreviewed: %v %v", res, err)
	}
	if _, err := x.requestSend(context.Background(), h, sendIn{draft: draft{To: []string{"x@example.com"}, Subject: "s",
		Attachments: []attachIn{{FromUID: 1, Index: 1}}}}); err == nil {
		t.Fatal("passed on an attachment the agent can't open without review")
	}
	h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.Strong}

	// Drafts: save, edit (the old one goes to the trash), delete, send.
	if _, err := x.draft(context.Background(), h, draftIn{draft: draft{To: []string{"bob@example.com"}, Bcc: []string{"boss@example.com"}, Subject: "Plan", Body: "v1"}}); err != nil {
		t.Fatal(err)
	}
	list, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Mailbox: "drafts"}})
	drafts := asMap(t, list)["messages"].([]any)
	if len(drafts) != 1 {
		t.Fatalf("drafts: %v", drafts)
	}
	v1 := uint32(drafts[0].(map[string]any)["uid"].(float64))
	res, err = x.draft(context.Background(), h, draftIn{draft: draft{To: []string{"bob@example.com"}, Bcc: []string{"boss@example.com"}, Subject: "Plan", Body: "v2"}, ReplaceUID: v1})
	if err != nil || asMap(t, res)["undo_id"] == nil || countIn(t, addr, "Drafts") != 1 || countIn(t, addr, "Deleted Messages") != 1 {
		t.Fatalf("edit: %v %v", res, err)
	}
	list, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Mailbox: "drafts"}})
	v2 := uint32(asMap(t, list)["messages"].([]any)[0].(map[string]any)["uid"].(float64))
	if _, err := x.sendDraft(context.Background(), h, draftRef{UID: v2}); err != nil {
		t.Fatal(err)
	}
	sd := h.pending[len(h.pending)-1]
	pl := sd.Payload.(mailPayload)
	if len(pl.Envelope) != 2 || pl.DraftUID != v2 || strings.Contains(string(pl.Raw), "boss@example.com") {
		t.Fatalf("send draft: %+v", pl)
	}
	n := len(*sent)
	if _, err := x.send(h, pl, false); err != nil || len(*sent) != n+1 || countIn(t, addr, "Drafts") != 0 {
		t.Fatalf("draft sent and removed: %v drafts=%d", err, countIn(t, addr, "Drafts"))
	}
	if !strings.Contains((*sent)[n].raw, "v2") || countIn(t, addr, "Sent Messages") < 1 {
		t.Fatal("sent the draft's content")
	}
}

func TestBigEmailWaitsOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	big := bytes.Repeat([]byte("x"), stashOver+10)
	p := mailPayload{Raw: big}
	if err := stash(&p); err != nil || p.Raw != nil || p.RawFile == "" {
		t.Fatalf("stash: %v %+v", err, p.RawFile)
	}
	b, _ := json.Marshal(p)
	if len(b) > 1000 {
		t.Fatal("the approval carries the whole email")
	}
	got, err := p.body()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("body: %v", err)
	}
	home, _ := os.UserHomeDir()
	_ = os.WriteFile(home+"/outbox/"+p.RawFile+"/message.eml", []byte("changed"), 0o600)
	if _, err := p.body(); err == nil {
		t.Fatal("a changed email was accepted")
	}
	p.RawFile = "../../etc"
	if _, err := p.body(); err == nil {
		t.Fatal("path outside the outbox")
	}
}

func TestSourceView(t *testing.T) {
	raw := []byte("Received: from mx.example.com\r\nDKIM-Signature: v=1; d=example.com\r\nFrom: a@example.com\r\nSubject: S\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=secret.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nU0VDUkVUQ09OVEVOVA==\r\n--b--\r\n")
	src, _ := sourceView(raw, true, 20000)
	if !strings.Contains(src, "DKIM-Signature") || !strings.Contains(src, "hello") || !strings.Contains(src, "secret.pdf") ||
		strings.Contains(src, "U0VDUkVU") || strings.Contains(src, "SECRETCONTENT") {
		t.Fatalf("source:\n%s", src)
	}
	if src, _ := sourceView(raw, false, 20000); strings.Contains(src, "secret.pdf") {
		t.Fatal("attachment names shown although the user hides attachments")
	}
	var _ = mail.Address{}
}

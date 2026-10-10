//go:build integration

package mailkit

// Integration tests against a real IMAP server (Dovecot, testdata/Dockerfile):
//
//	docker build -t rubi-dovecot testdata && docker run -d -p 127.0.0.1:1143:143 rubi-dovecot
//	MAILKIT_IMAP=127.0.0.1:1143 go test -tags integration -run Real ./...
//
// Every test signs in as a fresh user, so it starts with an empty mailbox.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

type real struct {
	t    *testing.T
	x    *integration
	h    *fakeHost
	user string
	sent *[]sentMail
}

func realSetup(t *testing.T) *real {
	t.Helper()
	addr := os.Getenv("MAILKIT_IMAP")
	if addr == "" {
		t.Skip("set MAILKIT_IMAP to a test IMAP server (see the top of this file)")
	}
	plain := func(a string, o *imapclient.Options) (*imapclient.Client, error) {
		conn, err := net.Dial("tcp", a)
		if err != nil {
			return nil, err
		}
		return imapclient.New(conn, o), nil
	}
	dialIMAP = func(a string) (*imapclient.Client, error) { return plain(a, nil) }
	dialIdle = plain
	defaultIMAPAddr = addr
	user := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + randomID("")[:6] + "@test.example"
	x := &integration{}
	secrets := map[string]string{"app_password": "pass"}
	settings, _, err := x.Validate(context.Background(), map[string]string{"address": user}, secrets)
	if err != nil {
		t.Fatalf("validate against the real server: %v", err)
	}
	raw, _ := json.Marshal(settings)
	var s Settings
	_ = json.Unmarshal(raw, &s)
	if s.Drafts != "Drafts" || s.Sent != "Sent" {
		t.Fatalf("special-use folders not found: %+v", s)
	}
	var sent []sentMail
	sendMail = func(_, u, pw, from string, to []string, msg []byte) error {
		sent = append(sent, sentMail{from: from, to: to, raw: string(msg)})
		return nil
	}
	return &real{t: t, x: x, h: &fakeHost{settings: raw, secrets: map[string]string{"app_password": "pass"}}, user: user, sent: &sent}
}

func (r *real) deliver(box, raw string, flags ...imap.Flag) {
	r.t.Helper()
	c, _, err := session(r.h)
	if err != nil {
		r.t.Fatal(err)
	}
	defer logout(c)
	if err := appendMessage(c, box, flags, []byte(strings.ReplaceAll(raw, "\n", "\r\n"))); err != nil {
		r.t.Fatal(err)
	}
}

func (r *real) count(box string) int {
	r.t.Helper()
	c, _, err := session(r.h)
	if err != nil {
		r.t.Fatal(err)
	}
	defer logout(c)
	msgs, err := search(c, searchQuery{Mailbox: box, Limit: 50})
	if err != nil {
		r.t.Fatal(err)
	}
	return len(msgs)
}

func (r *real) uid(box, subject string) uint32 {
	return uidOf(r.t, r.h, box, subject)
}

func TestRealSearchThreadsAndOrganize(t *testing.T) {
	r := realSetup(t)
	r.deliver("INBOX", "From: Anna <anna@example.com>\nTo: me\nSubject: Trip\nMessage-ID: <t1@example.com>\nDate: Wed, 07 Oct 2026 10:00:00 +0000\n\nRiga?\n")
	r.deliver("Sent", "From: me\nTo: anna@example.com\nSubject: Re: Trip\nMessage-ID: <t2@test.example>\nIn-Reply-To: <t1@example.com>\nReferences: <t1@example.com>\nDate: Wed, 07 Oct 2026 11:00:00 +0000\n\nYes.\n", imap.FlagSeen)
	r.deliver("INBOX", "From: shop@example.com\nSubject: Sale\nMessage-ID: <s1@example.com>\nDate: Thu, 08 Oct 2026 10:00:00 +0000\n\nsale\n")

	res, err := r.x.doRead(r.h, readPayload{Op: "search", Search: &searchQuery{Query: "from:anna OR to:anna"}})
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, res)
	if len(m["messages"].([]any)) != 2 || len(m["threads"].([]any)) != 1 {
		t.Fatalf("query across folders into one thread: %v", m)
	}
	conv, err := r.x.doThread(r.h, threadIn{UID: r.uid("INBOX", "Trip")})
	if err != nil || len(asMap(t, conv)["messages"].([]any)) != 2 {
		t.Fatalf("thread: %v %v", conv, err)
	}

	// Trash and undo, with the server's UIDPLUS answer.
	res, err = r.x.manage(context.Background(), r.h, manageOp{Op: "move", UIDs: []uint32{r.uid("INBOX", "Sale")}, To: "trash"})
	if err != nil || r.count("Trash") != 1 {
		t.Fatalf("trash: %v %v", res, err)
	}
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)}); err != nil || r.count("INBOX") != 2 || r.count("Trash") != 0 {
		t.Fatalf("undo: %v", err)
	}
	// Marks.
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "mark", UIDs: []uint32{r.uid("INBOX", "Sale")}, As: "read"}); err != nil {
		t.Fatal(err)
	}
	res, _ = r.x.doRead(r.h, readPayload{Op: "search", Search: &searchQuery{Query: "is:unread"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 1 {
		t.Fatalf("is:unread after marking: %v", got)
	}
	// Folders: create, move in, can't delete with mail, rename and undo.
	if _, err := r.x.doManage(r.h, manageOp{Op: "folder", Folder: "create", Name: "Work/Clients"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "move", UIDs: []uint32{r.uid("INBOX", "Sale")}, To: "work/clients"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.x.doManage(r.h, manageOp{Op: "folder", Folder: "delete", Name: "Work/Clients"}); err == nil {
		t.Fatal("deleted a folder with mail on a real server")
	}
	res, err = r.x.doManage(r.h, manageOp{Op: "folder", Folder: "rename", Name: "Work/Clients", NewName: "Customers"})
	if err != nil || r.count("Customers") != 1 {
		t.Fatalf("rename: %v", err)
	}
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)}); err != nil || r.count("Work/Clients") != 1 {
		t.Fatalf("undo rename: %v", err)
	}
}

func TestRealBulkSnoozeAndDrafts(t *testing.T) {
	r := realSetup(t)
	for i := 0; i < 4; i++ {
		r.deliver("INBOX", "From: news@shop.example\nSubject: Deal "+string(rune('A'+i))+"\nMessage-ID: <d"+string(rune('a'+i))+"@shop.example>\n\ndeal\n")
	}
	r.deliver("INBOX", "From: anna@example.com\nSubject: Hi\nMessage-ID: <hi@example.com>\n\nhi\n")
	if _, err := r.x.requestBulk(context.Background(), r.h, bulkIn{Query: "from:shop.example", Action: "archive"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.x.runBulk(r.h, r.h.pending[0].Payload.(bulkSet))
	if err != nil || r.count("Archive") != 4 || r.count("INBOX") != 1 {
		t.Fatalf("bulk: %v %v", res, err)
	}
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)}); err != nil || r.count("INBOX") != 5 {
		t.Fatalf("bulk undo: %v", err)
	}

	// Snooze and come back unread.
	if _, err := r.x.manage(context.Background(), r.h, manageOp{Op: "snooze", UIDs: []uint32{r.uid("INBOX", "Hi")}, Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r.x.mu.Lock()
	st, _ := r.x.loadState(r.h)
	st.Snoozed[0].Until = time.Now().Add(-time.Second)
	_ = r.h.SaveState(st)
	r.x.mu.Unlock()
	r.x.runJobs(r.h)
	if r.count(snoozeBox) != 0 || r.count("INBOX") != 5 {
		t.Fatalf("snooze back: snoozed=%d inbox=%d", r.count(snoozeBox), r.count("INBOX"))
	}

	// Drafts: save with Bcc, edit, send the saved draft.
	r.h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.Strong}
	if _, err := r.x.draft(context.Background(), r.h, draftIn{draft: draft{To: []string{"bob@example.com"}, Bcc: []string{"boss@example.com"}, Subject: "Plan", Body: "v1"}}); err != nil {
		t.Fatal(err)
	}
	v1 := r.uid("Drafts", "Plan")
	if _, err := r.x.draft(context.Background(), r.h, draftIn{draft: draft{To: []string{"bob@example.com"}, Bcc: []string{"boss@example.com"}, Subject: "Plan", Body: "v2"}, ReplaceUID: v1}); err != nil {
		t.Fatal(err)
	}
	if r.count("Drafts") != 1 || r.count("Trash") != 1 {
		t.Fatal("edit didn't replace the draft")
	}
	if _, err := r.x.sendDraft(context.Background(), r.h, draftRef{UID: r.uid("Drafts", "Plan")}); err != nil {
		t.Fatal(err)
	}
	p := r.h.pending[len(r.h.pending)-1].Payload.(mailPayload)
	if _, err := r.x.send(r.h, p, false); err != nil || len(*r.sent) != 1 || len((*r.sent)[0].to) != 2 || strings.Contains((*r.sent)[0].raw, "boss@") {
		t.Fatalf("send draft: %v %+v", err, *r.sent)
	}
	if r.count("Drafts") != 0 || r.count("Sent") != 1 {
		t.Fatalf("after sending: drafts=%d sent=%d", r.count("Drafts"), r.count("Sent"))
	}
}

func TestRealIdleAndRules(t *testing.T) {
	r := realSetup(t)
	if _, err := r.x.changeRules(r.h, ruleIn{Op: "add", Name: "Shop", From: []string{"shop.example"}, MoveTo: "archive", Star: true}); err != nil {
		t.Fatal(err)
	}
	r.x.pollNow = make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go r.x.idle(r.h, stop)
	time.Sleep(500 * time.Millisecond)
	r.deliver("INBOX", "From: shop@shop.example\nSubject: Order shipped\nMessage-ID: <o1@shop.example>\nDate: "+time.Now().Format(time.RFC1123Z)+"\n\nshipped\n")
	select {
	case <-r.x.pollNow:
	case <-time.After(5 * time.Second):
		t.Fatal("IDLE didn't report new mail")
	}
	if err := r.x.poll(r.h); err != nil {
		t.Fatal(err)
	}
	if r.count("Archive") != 1 || r.count("INBOX") != 0 {
		t.Fatalf("rule: archive=%d inbox=%d", r.count("Archive"), r.count("INBOX"))
	}
	res, _ := r.x.doRead(r.h, readPayload{Op: "search", Search: &searchQuery{Query: "in:archive is:starred"}})
	if got := asMap(t, res)["messages"].([]any); len(got) != 1 {
		t.Fatalf("starred by the rule: %v", got)
	}
}

package mailkit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/mail"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

func TestUndoSendAndSendLater(t *testing.T) {
	x, h, _, sent := setup(t)
	h.config = map[string]any{"undo_send": "30"}
	msg, _ := compose(defaultSettingsFor(t, h), draft{To: []string{"bob@example.com"}, Subject: "Hi", Body: "x"}, "", "")
	res, err := x.deliver(h, payloadOf(msg, 0), false)
	m := asMap(t, res)
	if err != nil || m["status"] != "scheduled" || len(*sent) != 0 {
		t.Fatalf("queued: %v %v", m, err)
	}
	// The panel page shows it with Cancel; cancelling stops it.
	view := asMap(t, must(x.panelView(&fakeBaseFor{h})))
	items := view["sections"].([]any)[0].(map[string]any)["items"].([]any)
	if len(items) != 1 || view["refresh"].(float64) == 0 {
		t.Fatalf("view: %v", view)
	}
	if _, err := x.cancelSend(h, m["send_id"].(string)); err != nil {
		t.Fatal(err)
	}
	x.runJobs(h)
	if len(*sent) != 0 {
		t.Fatal("a cancelled email went out")
	}
	// Due ones go out; send later tells the agent.
	p := payloadOf(msg, 0)
	p.SendAt = time.Now().Add(time.Hour)
	res, _ = x.deliver(h, p, false)
	x.mu.Lock()
	st, _ := x.loadState(h)
	st.Queued[0].SendAt = time.Now().Add(-time.Second)
	_ = h.SaveState(st)
	x.mu.Unlock()
	x.runJobs(h)
	if len(*sent) != 1 || len(h.events) != 1 || h.events[0]["type"] != "scheduled" {
		t.Fatalf("sent later: %d %v", len(*sent), h.events)
	}
	if _, err := sendTime("2020-01-01 10:00"); err == nil {
		t.Fatal("a time in the past")
	}
	h.config = map[string]any{"undo_send": "0"}
	if res, _ := x.deliver(h, payloadOf(msg, 0), false); asMap(t, res)["status"] != "sent" {
		t.Fatal("undo send off sends at once")
	}
}

// fakeBaseFor lets a single-account fakeHost serve as the plugin's base.
type fakeBaseFor struct{ h *fakeHost }

func (b *fakeBaseFor) SettingsFor(_ string, v any) error { return b.h.Settings(v) }
func (b *fakeBaseFor) SecretFor(_, k string) (string, error) {
	return b.h.Secret(k)
}
func (b *fakeBaseFor) ConfigFor(_ string, v any) error { return b.h.Config(v) }
func (b *fakeBaseFor) Accounts() ([]rubiplugin.AccountInfo, error) {
	return []rubiplugin.AccountInfo{{ID: testUser, Label: testUser, Default: true}}, nil
}
func (b *fakeBaseFor) LoadState(v any) error           { return b.h.LoadState(v) } // the single-account layout
func (b *fakeBaseFor) SaveState(v any) error           { return nil }
func (b *fakeBaseFor) Level(k string) rubiplugin.Level { return b.h.Level(k) }
func (b *fakeBaseFor) Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error) {
	return b.h.Submit(ctx, r)
}
func (b *fakeBaseFor) EmitTo(a, t string, d map[string]any) (string, error) {
	return b.h.EmitTo(a, t, d)
}
func (b *fakeBaseFor) Audit(string, map[string]any) {}
func (b *fakeBaseFor) Logf(string, ...any)          {}

func TestSnoozeAndRules(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: anna@example.com\nSubject: Later\nMessage-ID: <l1@example.com>\n\nlater\n", imap.FlagSeen)
	res, err := x.manage(context.Background(), h, manageOp{Op: "snooze", UIDs: []uint32{1}, Until: time.Now().Add(time.Hour)})
	if err != nil || countIn(t, addr, "INBOX") != 0 || countIn(t, addr, snoozeBox) != 1 || asMap(t, res)["undo_id"] == nil {
		t.Fatalf("snooze: %v %v", res, err)
	}
	x.mu.Lock()
	st, _ := x.loadState(h)
	st.Snoozed[0].Until = time.Now().Add(-time.Second)
	_ = h.SaveState(st)
	x.mu.Unlock()
	x.runJobs(h)
	c, _, _ := session(h)
	back, _ := search(c, searchQuery{})
	logout(c)
	if len(back) != 1 || back[0].Seen || len(h.events) != 1 || h.events[0]["type"] != "snooze" {
		t.Fatalf("back unread with an event: %+v %v", back, h.events)
	}

	// A rule, once approved, acts on new mail; private mail is only reported.
	if _, err := x.rules(context.Background(), h, ruleIn{Op: "add", Name: "Invoices", From: []string{"billing.example"}, MoveTo: "archive", Notify: true}); err != nil {
		t.Fatal(err)
	}
	if len(h.pending) != 1 || h.pending[0].Kind != kindRules {
		t.Fatalf("rules ask: %+v", h.pending)
	}
	if _, err := x.changeRules(h, h.pending[0].Payload.(ruleIn)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	deliver(t, addr, "INBOX", "From: billing.example <inv@billing.example>\nSubject: Invoice 7\nMessage-ID: <i7@billing.example>\nDate: "+time.Now().Format(time.RFC1123Z)+"\n\nPay 10\n")
	deliver(t, addr, "INBOX", "From: inv@billing.example\nSubject: 123456 is your code\nMessage-ID: <c7@billing.example>\nDate: "+time.Now().Format(time.RFC1123Z)+"\n\ncode\n")
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	if countIn(t, addr, "Archive") != 1 || countIn(t, addr, "INBOX") != 2 {
		t.Fatalf("rule moved: archive=%d inbox=%d", countIn(t, addr, "Archive"), countIn(t, addr, "INBOX"))
	}
	var notices []map[string]any
	for _, e := range h.events {
		if e["type"] == "rule" {
			notices = append(notices, e["data"].(map[string]any))
		}
	}
	if len(notices) != 2 || notices[1]["message"].(map[string]any)["private"] != true {
		t.Fatalf("rule notices: %v", notices)
	}
	if _, err := ruleFrom(ruleIn{Name: "x", From: []string{"a"}}); err == nil {
		t.Fatal("a rule that does nothing")
	}
}

func TestBulk(t *testing.T) {
	x, h, addr, _ := setup(t)
	for i := 0; i < 5; i++ {
		deliver(t, addr, "INBOX", "From: news@shop.example\nSubject: Deal\nMessage-ID: <d"+string(rune('a'+i))+"@shop.example>\n\ndeal\n")
	}
	deliver(t, addr, "INBOX", "From: news@shop.example\nSubject: Your code\nMessage-ID: <code@shop.example>\n\nYour verification code is 482913\n")
	deliver(t, addr, "INBOX", "From: anna@example.com\nSubject: Hi\nMessage-ID: <hi@example.com>\n\nhi\n")
	if _, err := x.requestBulk(context.Background(), h, bulkIn{Query: "from:shop.example", Action: "archive"}); err != nil {
		t.Fatal(err)
	}
	req := h.pending[0]
	pv := req.Preview.(map[string]any)
	if req.Kind != kindBulk || pv["emails"] != 5 || !strings.Contains(pv["from"].(string), "news@shop.example (5)") || pv["skipped"] == nil {
		t.Fatalf("bulk preview: %v %v", req.Summary, pv)
	}
	deliver(t, addr, "INBOX", "From: news@shop.example\nSubject: New deal\nMessage-ID: <new@shop.example>\n\nnew\n")
	res, err := x.runBulk(h, req.Payload.(bulkSet))
	if err != nil || asMap(t, res)["changed"].(float64) != 5 || countIn(t, addr, "Archive") != 5 || countIn(t, addr, "INBOX") != 3 {
		t.Fatalf("bulk: %v %v", res, err)
	}
	if _, err := x.manage(context.Background(), h, manageOp{Op: "undo", UndoID: asMap(t, res)["undo_id"].(string)}); err != nil || countIn(t, addr, "INBOX") != 8 {
		t.Fatalf("bulk undo: %v", err)
	}
}

func TestUnsubscribe(t *testing.T) {
	x, h, addr, sent := setup(t)
	deliver(t, addr, "INBOX", "From: news@shop.example\nSubject: Deal\nList-Unsubscribe: <https://shop.example/u/1>, <mailto:unsub@shop.example?subject=stop>\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\n\ndeal\n")
	deliver(t, addr, "INBOX", "From: list@club.example\nSubject: News\nList-Unsubscribe: <mailto:leave@club.example>\n\nnews\n")
	deliver(t, addr, "INBOX", "From: x@local.example\nSubject: Evil\nList-Unsubscribe: <https://127.0.0.1/u>\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\n\nx\n")
	var posted string
	unsubPost = func(_ context.Context, u string) (int, error) { posted = u; return 200, nil }
	defer func() { unsubPost = nil }()
	for _, uid := range []uint32{1, 2, 3} {
		if _, err := x.requestUnsubscribe(context.Background(), h, unsubIn{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	if h.pending[0].Kind != kindUnsubscribe || !strings.Contains(h.pending[0].Preview.(map[string]any)["how"].(string), "shop.example") {
		t.Fatalf("approval: %+v", h.pending[0])
	}
	if _, err := x.unsubscribe(context.Background(), h, h.pending[0].Payload.(unsubPlan)); err != nil || posted != "https://shop.example/u/1" {
		t.Fatalf("one click: %v %q", err, posted)
	}
	if _, err := x.unsubscribe(context.Background(), h, h.pending[1].Payload.(unsubPlan)); err != nil || len(*sent) != 1 || (*sent)[0].to[0] != "leave@club.example" {
		t.Fatalf("mailto: %v %v", err, *sent)
	}
	if _, err := x.unsubscribe(context.Background(), h, h.pending[2].Payload.(unsubPlan)); err == nil {
		t.Fatal("followed an address into the local network")
	}
}

func TestSecuritySigns(t *testing.T) {
	use(func() Provider { p := testProvider; p.AuthServIDs = []string{"google.com"}; return p }())
	defer use(testProvider)
	raw := "Authentication-Results: mx.google.com; spf=fail smtp.mailfrom=paypa1.com; dkim=none; dmarc=fail header.from=paypa1.com\r\n" +
		"Authentication-Results: evil.example; spf=pass; dkim=pass; dmarc=pass\r\n" +
		"From: \"service@paypal.com\" <service@paypa1.com>\r\nReply-To: help@paypal-refunds.net\r\nSubject: Verify\r\nContent-Type: text/html\r\n\r\n" +
		`<a href="https://paypa1-login.example/x">www.paypal.com</a>`
	m, err := parseMessage([]byte(raw), 0)
	if err != nil || m.Security == nil {
		t.Fatal(err)
	}
	w := strings.Join(m.Security.Warnings, "\n")
	for _, want := range []string{"DMARC failed", "looks like paypal", "name shows service@paypal.com", "Replies would go to", "shows www.paypal.com but leads to"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in\n%s", want, w)
		}
	}
	if m.Security.DMARC != "fail" {
		t.Fatalf("the forged pass was trusted: %+v", m.Security)
	}
	for d, bad := range map[string]bool{"applebees.com": false, "apple.com": false, "apple-id-verify.com": true, "rnicrosoft.com": true, "xn--pple-43d.com": true} {
		if (lookalike(d) != "") != bad {
			t.Errorf("%s: %q", d, lookalike(d))
		}
	}
}

func TestNewRecipientWarning(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: anna@friends.example\nSubject: Hi\n\nhi\n")
	s := defaultSettingsFor(t, h)
	known, _ := compose(s, draft{To: []string{"anna@friends.example"}, Subject: "x"}, "", "")
	fresh, _ := compose(s, draft{To: []string{"boss@newcorp.example"}, Subject: "x"}, "", "")
	if w := x.newRecipients(h, s, known); w != "" {
		t.Fatalf("known: %q", w)
	}
	if w := x.newRecipients(h, s, fresh); !strings.Contains(w, "newcorp.example") {
		t.Fatalf("fresh: %q", w)
	}
}

func TestReplyAllQuoteSignature(t *testing.T) {
	x, h, addr, _ := setup(t)
	h.config = map[string]any{"signature": "Test User\nRubi"}
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com, bob@example.com\nCc: carol@example.com\nSubject: Plan\nMessage-ID: <p1@example.com>\nDate: Wed, 07 Oct 2026 10:00:00 +0000\n\nLine one\nLine two\n")
	msg, _, err := x.prepare(h, &draft{ReplyToUID: 1, ReplyAll: true, Quote: true, Body: "Sounds good."}, false)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := parseMessage(msg.raw, 0)
	if m.To != "Anna <anna@example.com>" || !strings.Contains(m.Cc, "bob@example.com") || !strings.Contains(m.Cc, "carol@example.com") || strings.Contains(m.Cc, "me@icloud.com") {
		t.Fatalf("recipients: to=%q cc=%q", m.To, m.Cc)
	}
	if !strings.Contains(m.Text, "Sounds good.\n\n-- \nTest User\nRubi\n\nOn ") || !strings.Contains(m.Text, "> Line two") || m.Subject != "Re: Plan" {
		t.Fatalf("text:\n%s", m.Text)
	}
	msg, _, _ = x.prepare(h, &draft{To: []string{"x@example.com"}, Body: "b", NoSignature: true}, false)
	if strings.Contains(string(msg.raw), "Rubi") {
		t.Fatal("no_signature")
	}
}

const inviteICS = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Test//EN\nMETHOD:REQUEST\nBEGIN:VEVENT\nUID:ev-1@example.com\nSEQUENCE:2\nDTSTAMP:20261001T100000Z\nDTSTART:20261015T090000Z\nDTEND:20261015T100000Z\nSUMMARY:Planning\nLOCATION:Room 4\nORGANIZER;CN=Anna:mailto:anna@example.com\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:ME@icloud.com\nEND:VEVENT\nEND:VCALENDAR\n"

func TestInvites(t *testing.T) {
	x, h, addr, _ := setup(t)
	h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.Strong}
	deliver(t, addr, "INBOX", "From: anna@example.com\nTo: me@icloud.com\nSubject: Invitation: Planning\nMessage-ID: <inv@example.com>\nMIME-Version: 1.0\nContent-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/plain\n\nJoin us\n--b\nContent-Type: text/calendar; method=REQUEST; charset=utf-8\n\n"+inviteICS+"\n--b--\n")
	res, err := x.doRead(h, readPayload{Op: "read", Read: &readIn{UID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	inv := asMap(t, res)["message"].(map[string]any)["invite"].(map[string]any)
	if inv["method"] != "REQUEST" || inv["title"] != "Planning" || inv["location"] != "Room 4" || inv["organizer"] != "anna@example.com" {
		t.Fatalf("invite: %v", inv)
	}
	if _, err := x.respondInvite(context.Background(), h, inviteIn{UID: 1, Response: "accept"}); err != nil {
		t.Fatal(err)
	}
	p := h.pending[0].Payload.(mailPayload)
	raw := string(p.Raw)
	if p.Envelope[0] != "anna@example.com" || !strings.Contains(raw, "method=REPLY") || !strings.Contains(raw, "PARTSTAT=3DACCEPTED") ||
		!strings.Contains(raw, "UID:ev-1@example.com") || !strings.Contains(raw, "Subject: Accepted: Planning") || !strings.Contains(raw, "mailto:ME@icloud.com") {
		t.Fatalf("reply:\n%s", raw)
	}
}

func TestCustomServers(t *testing.T) {
	s := Settings{Address: "me@yandex.ru"}
	if err := customServers(&s, map[string]string{}); err != nil || s.IMAPAddr != "imap.yandex.ru:993" || s.SMTPAddr != "smtp.yandex.ru:465" {
		t.Fatalf("%+v %v", s, err)
	}
	s = Settings{Address: "me@corp.example"}
	if err := customServers(&s, map[string]string{"imap_server": "mail.corp.example", "smtp_server": "mail.corp.example:587", "username": "me"}); err != nil ||
		s.IMAPAddr != "mail.corp.example:993" || s.SMTPAddr != "mail.corp.example:587" || s.user() != "me" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []map[string]string{{}, {"imap_server": "a b", "smtp_server": "x.example"}} {
		s = Settings{Address: "me@corp.example"}
		if customServers(&s, bad) == nil {
			t.Errorf("%v should fail", bad)
		}
	}
	if err := customServers(&Settings{Address: "me@outlook.com"}, map[string]string{}); err == nil || !strings.Contains(err.Error(), "Outlook") {
		t.Fatal(err)
	}
}

func TestGmailFetchParse(t *testing.T) {
	uid, m, ok := parseGmFetch(`* 3 FETCH (X-GM-THRID 1650000000000000001 X-GM-LABELS (\Inbox \Important "My label" &BCAEMAQxBD4EQgQw-) UID 12)`)
	if !ok || uid != 12 || m.Thread != "1650000000000000001" || strings.Join(m.Labels, "|") != "INBOX|Important|My label|Работа" {
		t.Fatalf("%d %+v", uid, m)
	}
	msgs := []summary{{UID: 1, info: threadInfo{thread: "9", subject: "a"}}, {UID: 2, info: threadInfo{thread: "9", subject: "b"}}, {UID: 3, info: threadInfo{subject: "c"}}}
	if threads := groupThreads(msgs); len(threads) != 2 {
		t.Fatalf("Gmail conversations: %+v", threads)
	}
}

func TestIdleKicksPoll(t *testing.T) {
	x, h, addr, _ := setup(t)
	x.pollNow = make(chan struct{}, 1)
	stop := make(chan struct{})
	go x.idle(h, stop)
	defer close(stop)
	time.Sleep(200 * time.Millisecond)
	deliver(t, addr, "INBOX", "From: a@example.com\nSubject: New\n\nnew\n")
	select {
	case <-x.pollNow:
	case <-time.After(3 * time.Second):
		t.Fatal("new mail didn't wake the check")
	}
	var _ = mail.Address{}
}

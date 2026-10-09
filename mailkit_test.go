package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

const (
	testUser = "me@icloud.com"
	testPass = "abcd-efgh-ijkl-mnop"
	workUser = "work@icloud.com"
	workPass = "qrst-uvwx-yzab-cdef"
)

// testProvider is shaped like iCloud Mail.
var testProvider = Provider{
	ID: "icloud-mail", Name: "iCloud Mail", Version: "v2.0.0", MinRubi: "v0.6.0", ToolPrefix: "icloud_mail",
	Source: "https://github.com/Deikus-LXXVII/rubi-icloud-mail", AddressLabel: "iCloud email address",
	PasswordLabel: "App-specific password", IMAPAddr: "imap.mail.me.com:993", SMTPAddr: "smtp.mail.me.com:587",
	Drafts: "Drafts", Sent: "Sent Messages", MailDomain: "icloud.com", AuthError: "iCloud rejected the login",
}

func TestMain(m *testing.M) {
	use(testProvider)
	os.Exit(m.Run())
}

// startIMAP runs an in-memory IMAP server shaped like iCloud (special-use Drafts and "Sent Messages").
func startIMAP(t *testing.T) string {
	t.Helper()
	mem := imapmemserver.New()
	for _, login := range [][2]string{{testUser, testPass}, {workUser, workPass}} {
		u := imapmemserver.NewUser(login[0], login[1])
		_ = u.Create("INBOX", nil)
		_ = u.Create("Drafts", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrDrafts}})
		_ = u.Create("Sent Messages", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrSent}})
		mem.AddUser(u)
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	dialIMAP = func(addr string) (*imapclient.Client, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return imapclient.New(conn, nil), nil
	}
	return ln.Addr().String()
}

// deliver appends a raw message to a folder, as if it arrived from outside.
func deliver(t *testing.T, addr, box, raw string, flags ...imap.Flag) {
	t.Helper()
	c, err := dialIMAP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(testUser, testPass).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(c, box, flags, []byte(strings.ReplaceAll(raw, "\n", "\r\n"))); err != nil {
		t.Fatal(err)
	}
}

type sentMail struct {
	from string
	to   []string
	raw  string
}

// fakeHost stands in for Rubi. Approvals stay pending; tests execute them explicitly.
type fakeHost struct {
	mu       sync.Mutex
	settings json.RawMessage
	secrets  map[string]string
	state    json.RawMessage
	levels   map[string]rubiplugin.Level
	events   []map[string]any
	pending  []rubiplugin.Request
	config   map[string]any
}

func (h *fakeHost) Settings(v any) error {
	if h.settings == nil {
		return errors.New("not set up")
	}
	return json.Unmarshal(h.settings, v)
}
func (h *fakeHost) Secret(k string) (string, error) { return h.secrets[k], nil }
func (h *fakeHost) Level(kind string) rubiplugin.Level {
	if l, ok := h.levels[kind]; ok {
		return l
	}
	return rubiplugin.None
}
func (h *fakeHost) Submit(ctx context.Context, req rubiplugin.Request) (map[string]any, error) {
	h.pending = append(h.pending, req)
	return map[string]any{"status": "awaiting_approval"}, nil
}
func (h *fakeHost) Emit(typ string, data map[string]any) (string, error) {
	return h.EmitTo("", typ, data)
}
func (h *fakeHost) EmitTo(agent, typ string, data map[string]any) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, map[string]any{"type": typ, "data": data, "agent": agent})
	return "evt", nil
}

// Config merges the manifest defaults with what the test set, like Rubi does.
func (h *fakeHost) Config(v any) error {
	cfg := map[string]any{}
	for _, f := range manifest().Config {
		cfg[f.Key] = f.Default
	}
	for k, x := range h.config {
		cfg[k] = x
	}
	b, _ := json.Marshal(cfg)
	return json.Unmarshal(b, v)
}
func (h *fakeHost) Audit(string, map[string]any) {}
func (h *fakeHost) LoadState(v any) error {
	if h.state == nil {
		return nil
	}
	return json.Unmarshal(h.state, v)
}
func (h *fakeHost) SaveState(v any) error {
	b, err := json.Marshal(v)
	h.state = b
	return err
}
func (h *fakeHost) Logf(string, ...any) {}

func setup(t *testing.T) (*integration, *fakeHost, string, *[]sentMail) {
	t.Helper()
	addr := startIMAP(t)
	defaultIMAPAddr = addr
	x := &integration{}
	fields := map[string]string{"address": " Me@iCloud.com ", "from_name": "Test User"}
	if _, _, err := x.Validate(context.Background(), fields, map[string]string{"app_password": "wrong"}); !errors.Is(err, errAuth) {
		t.Fatalf("wrong password: %v", err)
	}
	secrets := map[string]string{"app_password": " " + testPass + " "}
	settings, account, err := x.Validate(context.Background(), fields, secrets)
	if err != nil || account != testUser || secrets["app_password"] != testPass {
		t.Fatalf("validate: %v account=%q", err, account)
	}
	raw, _ := json.Marshal(settings)
	var s Settings
	_ = json.Unmarshal(raw, &s)
	if s.Drafts != "Drafts" || s.Sent != "Sent Messages" || s.IMAPAddr != addr {
		t.Fatalf("settings: %+v", s)
	}
	var sent []sentMail
	sendMail = func(_, user, pw, from string, to []string, msg []byte) error {
		if user != testUser || pw != testPass {
			return errAuth
		}
		sent = append(sent, sentMail{from: from, to: to, raw: string(msg)})
		return nil
	}
	return x, &fakeHost{settings: raw, secrets: map[string]string{"app_password": testPass}}, addr, &sent
}

func TestReadDoesNotMarkSeen(t *testing.T) {
	_, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: =?utf-8?q?=D0=92=D1=81=D1=82=D1=80=D0=B5=D1=87=D0=B0?=\nMessage-ID: <a1@example.com>\nDate: Wed, 07 Oct 2026 10:00:00 +0000\nContent-Type: text/plain; charset=utf-8\n\nПривет! В 15:00?\n")
	deliver(t, addr, "INBOX", "From: shop@example.com\nTo: me@icloud.com\nSubject: Sale\nMessage-ID: <s1@example.com>\nContent-Type: text/html\n\n<html><head><style>x{}</style></head><body><p>Big <b>sale</b></p><script>steal()</script></body></html>\n")

	c, _, err := session(h)
	if err != nil {
		t.Fatal(err)
	}
	defer logout(c)
	msgs, err := search(c, searchQuery{Subject: "Встреча"})
	if err != nil || len(msgs) != 1 || msgs[0].From != "Anna <anna@example.com>" || msgs[0].Seen {
		t.Fatalf("search: %+v %v", msgs, err)
	}
	raw, err := fetchRaw(c, "INBOX", msgs[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := parseMessage(raw, 0)
	if m.Subject != "Встреча" || !strings.Contains(m.Text, "Привет") || m.MessageID != "<a1@example.com>" {
		t.Fatalf("parsed: %+v", m)
	}
	after, _ := search(c, searchQuery{Subject: "Встреча"})
	if after[0].Seen {
		t.Fatal("reading marked the message as read")
	}
	all, _ := search(c, searchQuery{})
	raw2, _ := fetchRaw(c, "INBOX", all[0].UID) // newest first: the HTML one
	m2, _ := parseMessage(raw2, 0)
	if strings.Contains(m2.Text, "steal") || !strings.Contains(m2.Text, "Big sale") {
		t.Fatalf("html to text: %q", m2.Text)
	}
}

func TestComposeHeaders(t *testing.T) {
	s := defaultSettings()
	s.Address, s.FromName = testUser, "Тест Тестович"
	m, err := compose(s, draft{To: []string{"Anna <anna@example.com>"}, Bcc: []string{"hidden@example.com"},
		Subject: "Встреча", Body: "line1\nline2 — ok"}, "<a1@example.com>", "<a0@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(m.raw)
	for _, want := range []string{"In-Reply-To: <a1@example.com>", "References: <a0@example.com> <a1@example.com>",
		"Subject: =?utf-8?q?Re:_", "line1\r\nline2"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "hidden@example.com") {
		t.Error("Bcc leaked into headers")
	}
	if len(m.envelope) != 2 || m.envelope[1] != "hidden@example.com" {
		t.Errorf("envelope: %v", m.envelope)
	}
	if _, err := compose(s, draft{To: []string{"not an address"}}, "", ""); err == nil {
		t.Error("invalid address accepted")
	}
}

func TestSendGatedTrackedAndReplyDetected(t *testing.T) {
	x, h, addr, sent := setup(t)
	h.levels = map[string]rubiplugin.Level{kindSend: rubiplugin.Strong}

	// Pending approval: nothing is sent.
	if _, err := x.requestSend(context.Background(), h, sendIn{draft: draft{To: []string{"anna@example.com"},
		Subject: "Meeting", Body: "15:00?"}}); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 0 || len(h.pending) != 1 || h.pending[0].Kind != kindSend {
		t.Fatalf("sent before approval: %v", h.pending)
	}
	// The payload survives the trip through Rubi as JSON.
	b, _ := json.Marshal(h.pending[0].Payload)
	var msg mailPayload
	if err := json.Unmarshal(b, &msg); err != nil || msg.TrackDays != 14 {
		t.Fatalf("payload: %v %+v", err, msg)
	}

	// The user picks "Send and notify on reply".
	res, err := x.send(h, msg, true)
	if err != nil {
		t.Fatal(err)
	}
	out := res.(map[string]any)
	if len(*sent) != 1 || out["saved_to_sent"] != true || out["tracking"] == nil {
		t.Fatalf("send result: %+v sent=%d", out, len(*sent))
	}

	// Nothing arrived yet.
	if err := x.poll(h); err != nil || len(h.events) != 0 {
		t.Fatalf("poll: %v events=%v", err, h.events)
	}

	// A threaded reply, an unrelated mail with the same subject, our own copy, and a reply without headers.
	deliver(t, addr, "INBOX", fmt.Sprintf("From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Re: Meeting\nMessage-ID: <r1@example.com>\nIn-Reply-To: %s\nDate: %s\n\nYes!\n", msg.MessageID, time.Now().Format(time.RFC1123Z)))
	deliver(t, addr, "INBOX", "From: spam@example.com\nTo: me@icloud.com\nSubject: Re: Meeting\nMessage-ID: <x1@example.com>\n\nbuy\n")
	deliver(t, addr, "INBOX", "From: me@icloud.com\nTo: anna@example.com\nSubject: Re: Meeting\nMessage-ID: <own@icloud.com>\n\nmine\n")
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 1 {
		t.Fatalf("want 1 reply event, got %v", h.events)
	}
	reply := h.events[0]["data"].(map[string]any)["reply"].(map[string]any)
	if reply["match"] != "headers" || !strings.Contains(reply["from"].(string), "anna@example.com") {
		t.Fatalf("reply: %v", reply)
	}

	deliver(t, addr, "INBOX", "From: anna@example.com\nTo: me@icloud.com\nSubject: Ответ: Re: Meeting\nMessage-ID: <r2@example.com>\n\nalso yes\n")
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 || h.events[1]["data"].(map[string]any)["reply"].(map[string]any)["match"] != "subject" {
		t.Fatalf("subject heuristic: %v", h.events)
	}
	if err := x.poll(h); err != nil || len(h.events) != 2 {
		t.Fatalf("duplicates after re-poll: %v", h.events)
	}

	// Stop tracking: nothing more is reported and iCloud isn't contacted.
	st, _ := x.loadState(h)
	if ok, _ := x.stopTracking(h, st.Tracked[0].ID); !ok {
		t.Fatal("stop tracking")
	}
	deliver(t, addr, "INBOX", fmt.Sprintf("From: anna@example.com\nSubject: Re: Meeting\nMessage-ID: <r3@example.com>\nIn-Reply-To: %s\n\nthird\n", msg.MessageID))
	if err := x.poll(h); err != nil || len(h.events) != 2 {
		t.Fatalf("reported after stop: %v", h.events)
	}
}

func TestNormalizeSubject(t *testing.T) {
	for in, want := range map[string]string{"Re: RE:  Fwd: Meeting": "meeting", "Ответ: Встреча": "встреча", "AW[2]: x": "x"} {
		if got := normSubject(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSelfAddressedTracking(t *testing.T) {
	own := "me@icloud.com"
	now := time.Now()
	toSelf := &tracked{ID: "t1", MessageID: "orig@icloud.com", Subject: "Test", Recipients: []string{own}, SentAt: now, ExpiresAt: now.Add(time.Hour)}
	toAnna := &tracked{ID: "t2", MessageID: "orig2@icloud.com", Subject: "Plan", Recipients: []string{"anna@example.com"}, SentAt: now, ExpiresAt: now.Add(time.Hour)}
	active := []*tracked{toSelf, toAnna}

	// The original itself arriving in INBOX is never a reply, even though subject and sender match.
	if tr, _ := match(header{messageID: "<orig@icloud.com>", fromAddr: own, subject: "Test"}, active, own); tr != nil {
		t.Fatal("the tracked email itself counted as a reply")
	}
	// Replying to yourself counts when you wrote to yourself.
	if tr, how := match(header{messageID: "<r@icloud.com>", fromAddr: own, subject: "Re: Test", inReplyTo: "<orig@icloud.com>"}, active, own); tr != toSelf || how != "headers" {
		t.Fatalf("self reply: %v %s", tr, how)
	}
	// Your own follow-up in a thread with someone else is not their reply.
	if tr, _ := match(header{messageID: "<f@icloud.com>", fromAddr: own, subject: "Re: Plan", inReplyTo: "<orig2@icloud.com>"}, active, own); tr != nil {
		t.Fatal("own follow-up counted as a reply")
	}
}

func TestPrivacyFilter(t *testing.T) {
	p := privacyConfig{HideCodes: true, HideResets: true, Senders: []string{"bank.example"}, Keywords: []string{"medical"}}
	for _, c := range []struct {
		from, subject, body string
		hidden              bool
	}{
		{"Apple <noreply@apple.com>", "Your verification code", "", true},
		{"x@shop.com", "482913 is your login code", "", true},
		{"x@shop.com", "Ваш код: 4821", "", true},
		{"x@shop.com", "Код подтверждения", "", true},
		{"x@shop.com", "Your 2FA settings", "", true},
		{"x@shop.com", "Reset your password", "", true},
		{"Google <no-reply@accounts.google.com>", "Security alert: new sign-in on Mac", "", false},
		{"x@bank.ru", "Подозрительная активность", "", false},
		{"Alerts <info@mail.bank.example>", "Statement", "", true},
		{"x@clinic.com", "Results", "your medical report", true},
		{"x@shop.com", "Use promo code SPRING", "", false},
		{"x@shop.com", "Order 123456 shipped", "", false},
		{"x@hotpot.com", "Hotpot menu", "", false},
		{"bob@example.com", "Lunch?", "", false},
		{"x@acme.com", "Acme", "Your code: 482193", true}, // the code only in the body
		{"x@acme.com", "Acme", "Use this login link to continue", true},
		{"bob@example.com", "Notes", "The meeting is at 1530 in room 4", false},
	} {
		if got, _ := p.hidden(c.from, c.subject, c.body); got != c.hidden {
			t.Errorf("%q / %q: hidden=%v, want %v", c.from, c.subject, got, c.hidden)
		}
	}
	if got, _ := (privacyConfig{}).hidden("x@a.com", "Your verification code", ""); got {
		t.Error("built-in list applied while turned off")
	}
	// Each category has its own switch.
	alerts := privacyConfig{HideAlerts: true}
	if got, _ := alerts.hidden("x@a.com", "New sign-in to your account", ""); !got {
		t.Error("sign-in alert shown while hidden")
	}
	if got, _ := alerts.hidden("x@a.com", "Reset your password", ""); got {
		t.Error("password reset hidden while its switch is off")
	}
	// An alert carrying a code stays hidden by the code switch.
	if got, why := (privacyConfig{HideCodes: true}).hidden("x@a.com", "New sign-in", "Your verification code is 1234"); !got || why != "sign-in code or confirmation link" {
		t.Errorf("alert with a code: %v %q", got, why)
	}
	if senderOnly(`"Your code is 1234" <noreply@x.com>`) != "noreply@x.com" {
		t.Error("display name leaked")
	}
}

func TestPrivateMailInSearchAndRead(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: Apple <noreply@apple.com>\nTo: me@icloud.com\nSubject: Your verification code\nMessage-ID: <c1@x>\n\nYour code is 829114\n")
	deliver(t, addr, "INBOX", "From: anna@example.com\nTo: me@icloud.com\nSubject: Lunch\nMessage-ID: <l1@x>\n\nTomorrow?\n")

	res, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "verification") || !strings.Contains(string(b), `"private":true`) || !strings.Contains(string(b), "Lunch") {
		t.Fatalf("search: %s", b)
	}
	// Searching by content must not even reveal that a private message matched.
	res, _ = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Text: "829114"}})
	if b, _ := json.Marshal(res); strings.Contains(string(b), `"uid"`) {
		t.Fatalf("content search found the private email: %s", b)
	}
	msgs := res.(map[string]any)["messages"].([]summary)
	_ = msgs
	all, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}})
	var uid uint32
	for _, m := range all.(map[string]any)["messages"].([]summary) {
		if m.Private {
			uid = m.UID
		}
	}
	out, err := x.doRead(h, readPayload{Op: "read", Read: &readIn{UID: uid}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "829114") || !strings.Contains(string(b), `"status":"private"`) {
		t.Fatalf("read: %s", b)
	}
	// Revealing needs the user's approval; the approved action returns the email.
	if _, err := x.requestReveal(context.Background(), h, revealIn{UID: uid}); err == nil {
		t.Fatal("reveal without a reason")
	}
	if _, err := x.requestReveal(context.Background(), h, revealIn{UID: uid, Reason: "sign in to Apple"}); err != nil {
		t.Fatal(err)
	}
	last := h.pending[len(h.pending)-1]
	if last.Kind != kindPrivate {
		t.Fatalf("reveal request: %+v", last)
	}
	shown, err := x.reveal(h, revealIn{UID: uid}, "show")
	if b, _ := json.Marshal(shown); err != nil || !strings.Contains(string(b), "829114") {
		t.Fatalf("reveal: %s %v", b, err)
	}
}

func TestWatches(t *testing.T) {
	x, h, addr, _ := setup(t)
	ctx := context.Background()
	add := func(w watchIn) {
		t.Helper()
		from, _ := cleanTerms(w.From)
		words, _ := cleanTerms(w.Keywords)
		now := time.Now().UTC().Add(-time.Minute)
		if _, err := x.addWatch(h, &watch{ID: randomID("wch_"), Name: w.Name, From: from, Keywords: words, Agent: w.Agent,
			Created: now, Expires: now.Add(24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = ctx
	add(watchIn{Name: "Ozon", From: []string{"ozon.ru"}, Agent: "Mail"})
	add(watchIn{Name: "Parcel", Keywords: []string{"RB123456789"}})
	add(watchIn{Name: "Bank words", Keywords: []string{"code"}})

	deliver(t, addr, "INBOX", fmt.Sprintf("From: Ozon <news@mail.ozon.ru>\nTo: me@icloud.com\nSubject: Order shipped\nDate: %s\nMessage-ID: <o1@x>\n\nOn its way.\n", time.Now().Format(time.RFC1123Z)))
	deliver(t, addr, "INBOX", fmt.Sprintf("From: post@pochta.example\nTo: me@icloud.com\nSubject: Update\nDate: %s\nMessage-ID: <p1@x>\n\nParcel RB123456789 arrived.\n", time.Now().Format(time.RFC1123Z)))
	deliver(t, addr, "INBOX", fmt.Sprintf("From: Ozon <id@ozon.ru>\nTo: me@icloud.com\nSubject: Your login code 554433\nDate: %s\nMessage-ID: <o2@x>\n\ncode 554433\n", time.Now().Format(time.RFC1123Z)))
	deliver(t, addr, "INBOX", fmt.Sprintf("From: bob@example.com\nTo: me@icloud.com\nSubject: Hi\nDate: %s\nMessage-ID: <b1@x>\n\nnothing\n", time.Now().Format(time.RFC1123Z)))
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range h.events {
		if e["type"] != "watch" {
			continue
		}
		d := e["data"].(map[string]any)
		w := d["watch"].(map[string]any)["name"].(string)
		m := d["message"].(map[string]any)
		b, _ := json.Marshal(m)
		got = append(got, fmt.Sprintf("%s->%v private=%v", w, e["agent"], m["private"] == true))
		if strings.Contains(string(b), "554433") {
			t.Fatalf("private content in a watch event: %s", b)
		}
	}
	want := []string{"Ozon->Mail private=false", "Parcel-> private=false", "Ozon->Mail private=true"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("watch events:\n got  %v\n want %v", got, want)
	}
	// A second poll reports nothing new.
	n := len(h.events)
	_ = x.poll(h)
	if len(h.events) != n {
		t.Fatal("duplicate watch events")
	}
}

func TestFolderAccess(t *testing.T) {
	x, h, _, _ := setup(t)
	h.config = map[string]any{"folder_access": "selected", "folders": []string{"INBOX"}, "folder_requests": "ask"}
	if _, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}}); err != nil {
		t.Fatalf("INBOX: %v", err)
	}
	_, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Mailbox: "Sent Messages"}})
	if err == nil || !strings.Contains(err.Error(), tool("folder_access")) {
		t.Fatalf("closed folder: %v", err)
	}
	list, _ := x.doRead(h, readPayload{Op: "list"})
	if b, _ := json.Marshal(list); !strings.Contains(string(b), `"closed":["Drafts","Sent Messages"]`) {
		t.Fatalf("list: %s", b)
	}
	// After the user approves an hour of access, the folder opens.
	x.mu.Lock()
	st, _ := x.loadState(h)
	st.Grants = append(st.Grants, &grant{Mailbox: "Sent Messages", Until: time.Now().Add(time.Hour)})
	_ = h.SaveState(st)
	x.mu.Unlock()
	if _, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Mailbox: "Sent Messages"}}); err != nil {
		t.Fatalf("granted: %v", err)
	}
	// "Never": the agent is told it can't ask.
	h.config["folder_requests"] = "never"
	_, err = x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Mailbox: "Drafts"}})
	if err == nil || strings.Contains(err.Error(), tool("folder_access")) {
		t.Fatalf("never: %v", err)
	}
}

func TestRevealBatch(t *testing.T) {
	x, h, addr, _ := setup(t)
	for i, code := range []string{"111111", "222222", "333333"} {
		deliver(t, addr, "INBOX", fmt.Sprintf("From: Svc%d <noreply@svc%d.com>\nTo: me@icloud.com\nSubject: Your login code %s\nMessage-ID: <c%d@x>\n\ncode %s\n", i, i, code, i, code))
	}
	deliver(t, addr, "INBOX", "From: anna@example.com\nTo: me@icloud.com\nSubject: Lunch\nMessage-ID: <l@x>\n\nhi\n")
	all, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}})
	var uids []uint32
	for _, m := range all.(map[string]any)["messages"].([]summary) {
		uids = append(uids, m.UID)
	}
	res, err := x.requestReveal(context.Background(), h, revealIn{UIDs: uids, Reason: "log in to three services"})
	if err != nil {
		t.Fatal(err)
	}
	req := h.pending[len(h.pending)-1]
	if len(req.Items) != 3 || res.(map[string]any)["not_private"] == nil {
		t.Fatalf("batch request: %d items, %v", len(req.Items), res)
	}
	// The user ticks two of the three.
	b, _ := json.Marshal(req.Payload)
	var payload revealIn
	_ = json.Unmarshal(b, &payload)
	out, err := x.reveal(h, payload, "items:"+req.Items[0].Key+","+req.Items[2].Key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	if strings.Count(string(got), `"uid"`) != 2 || !strings.Contains(string(got), "111111") || !strings.Contains(string(got), "333333") {
		t.Fatalf("revealed: %s", got)
	}
	if strings.Contains(string(got), "222222") {
		t.Fatalf("an unticked email was revealed: %s", got)
	}
}

// The privacy filter can't be sidestepped through the body: a short preview or a content search must not
// reveal a code that only the body carries.
func TestPrivateBodyCantBeProbed(t *testing.T) {
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: shop@acme.example\nTo: me@icloud.com\nSubject: Acme\nMessage-ID: <a1@x>\n\n482193\n\nThis is your verification code for Acme.\n")
	all, err := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}})
	if err != nil {
		t.Fatal(err)
	}
	var uid uint32
	for _, m := range all.(map[string]any)["messages"].([]summary) {
		uid = m.UID
	}
	out, err := x.doRead(h, readPayload{Op: "read", Read: &readIn{UID: uid, MaxChars: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "4821") {
		t.Fatalf("short preview leaked the code: %s", b)
	}
	for _, q := range []string{"code is", "4821", "482193"} {
		res, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{Text: q}})
		if b, _ := json.Marshal(res); strings.Contains(string(b), `"uid"`) {
			t.Fatalf("content search %q found the private email: %s", q, b)
		}
	}
}

const withAttachment = "From: anna@example.com\nTo: me@icloud.com\nSubject: The contract\nMessage-ID: <a1@x>\n" +
	"MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=B\n\n" +
	"--B\nContent-Type: text/plain\n\nSee the attached file.\n" +
	"--B\nContent-Type: text/plain; name=\"contract.txt\"\nContent-Disposition: attachment; filename=\"../contract.txt\"\n\nTerms: pay 100.\n" +
	"--B--\n"

// The user chooses whether attachments open freely, after their approval, or not at all.
func TestAttachmentSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	x, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", withAttachment)
	all, _ := x.doRead(h, readPayload{Op: "search", Search: &searchQuery{}})
	uid := all.(map[string]any)["messages"].([]summary)[0].UID

	// Default: ask. The agent must say why; the user approves; then it gets the file.
	if _, err := x.requestAttachment(context.Background(), h, attachmentIn{UID: uid, Index: 1}); err == nil {
		t.Fatal("asked without a reason")
	}
	res, err := x.requestAttachment(context.Background(), h, attachmentIn{UID: uid, Index: 1, Reason: "summarize the contract"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "pay 100") {
		t.Fatalf("content given before approval: %s", b)
	}
	last := h.pending[len(h.pending)-1]
	if last.Kind != kindAttachment {
		t.Fatalf("request: %+v", last)
	}
	payload, _ := json.Marshal(last.Payload)
	var in attachmentIn
	_ = json.Unmarshal(payload, &in)
	got, err := x.openAttachment(h, in)
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	path := out["file"].(string)
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "pay 100") || filepath.Base(path) != "contract.txt" {
		t.Fatalf("file %s: %v %q", path, err, b)
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", st.Mode().Perm())
	}

	// Free: at once, no approval.
	if h.config == nil {
		h.config = map[string]any{}
	}
	h.config["attachments"] = "free"
	n := len(h.pending)
	if got, err := x.requestAttachment(context.Background(), h, attachmentIn{UID: uid, Name: "../contract.txt"}); err != nil ||
		!strings.Contains(got.(map[string]any)["text"].(string), "pay 100") || len(h.pending) != n {
		t.Fatalf("free: %v %v", got, err)
	}

	// Never: refused, and the agent sees only how many there are.
	h.config["attachments"] = "never"
	if _, err := x.requestAttachment(context.Background(), h, attachmentIn{UID: uid, Index: 1, Reason: "x"}); err == nil {
		t.Fatal("opened while closed")
	}
	if _, err := x.openAttachment(h, in); err == nil {
		t.Fatal("an approval from before the setting changed still opened it")
	}
	read, _ := x.doRead(h, readPayload{Op: "read", Read: &readIn{UID: uid}})
	if b, _ := json.Marshal(read); strings.Contains(string(b), "contract.txt") || !strings.Contains(string(b), `"attachments_hidden":1`) {
		t.Fatalf("read with attachments closed: %s", b)
	}
}

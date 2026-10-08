package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// fakeBase stands in for Rubi with several connected accounts.
type fakeBase struct {
	mu       sync.Mutex
	accounts []rubiplugin.AccountInfo
	settings map[string]json.RawMessage
	secrets  map[string]string // account → app password
	config   map[string]map[string]any
	state    json.RawMessage
	levels   map[string]rubiplugin.Level
	pending  []rubiplugin.Request
	events   []map[string]any
}

func (b *fakeBase) ref(account string) string {
	if account == "" {
		for _, a := range b.accounts {
			if a.Default {
				return a.ID
			}
		}
	}
	return account
}
func (b *fakeBase) SettingsFor(account string, v any) error {
	raw, ok := b.settings[b.ref(account)]
	if !ok {
		return errors.New("unknown account")
	}
	return json.Unmarshal(raw, v)
}
func (b *fakeBase) SecretFor(account, key string) (string, error) {
	return b.secrets[b.ref(account)], nil
}
func (b *fakeBase) ConfigFor(account string, v any) error {
	cfg := map[string]any{}
	for _, f := range manifest().Config {
		cfg[f.Key] = f.Default
	}
	for k, x := range b.config[b.ref(account)] {
		cfg[k] = x
	}
	raw, _ := json.Marshal(cfg)
	return json.Unmarshal(raw, v)
}
func (b *fakeBase) Accounts() ([]rubiplugin.AccountInfo, error) { return b.accounts, nil }
func (b *fakeBase) LoadState(v any) error {
	if b.state == nil {
		return nil
	}
	return json.Unmarshal(b.state, v)
}
func (b *fakeBase) SaveState(v any) error {
	raw, err := json.Marshal(v)
	b.state = raw
	return err
}
func (b *fakeBase) Level(kind string) rubiplugin.Level {
	if l, ok := b.levels[kind]; ok {
		return l
	}
	return rubiplugin.None
}
func (b *fakeBase) Submit(_ context.Context, r rubiplugin.Request) (map[string]any, error) {
	b.pending = append(b.pending, r)
	return map[string]any{"status": "awaiting_approval"}, nil
}
func (b *fakeBase) EmitTo(agent, typ string, data map[string]any) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, map[string]any{"type": typ, "data": data, "agent": agent})
	return "evt", nil
}
func (b *fakeBase) Audit(string, map[string]any) {}
func (b *fakeBase) Logf(string, ...any)          {}

func TestSeveralAccounts(t *testing.T) {
	x, single, addr, sent := setup(t)
	work := defaultSettings()
	work.Address, work.IMAPAddr = workUser, addr
	workRaw, _ := json.Marshal(work)
	// A state saved by the single-account version, with a watch on the personal mailbox.
	if _, err := x.addWatch(single, &watch{ID: "wch_old", Name: "old", From: []string{"shop.example"},
		Created: testNow(), Expires: testNow().AddDate(0, 0, 30)}); err != nil {
		t.Fatal(err)
	}
	b := &fakeBase{
		accounts: []rubiplugin.AccountInfo{{ID: testUser, Label: testUser, Default: true}, {ID: workUser, Label: workUser}},
		settings: map[string]json.RawMessage{testUser: single.settings, workUser: workRaw},
		secrets:  map[string]string{testUser: testPass, workUser: workPass},
		config:   map[string]map[string]any{workUser: {"folder_access": "selected", "folders": []string{"INBOX"}, "folder_requests": "never"}},
		state:    single.state,
		levels:   map[string]rubiplugin.Level{kindSend: rubiplugin.Strong},
	}
	ctx := context.Background()

	// Picking accounts: default, by address (any case), unknown.
	me, err := accountFor(b, "")
	if err != nil || me.id != testUser {
		t.Fatalf("default: %v %v", me, err)
	}
	wk, err := accountFor(b, " WORK@icloud.com ")
	if err != nil || wk.id != workUser {
		t.Fatalf("by address: %v %v", wk, err)
	}
	if _, err := accountFor(b, "nobody@icloud.com"); err == nil || !strings.Contains(err.Error(), workUser) {
		t.Fatalf("unknown account: %v", err)
	}

	// The old state now belongs to the default account; the work account starts empty.
	if st, _ := x.loadState(me); len(st.Watches) != 1 {
		t.Fatalf("legacy state not migrated: %+v", st)
	}
	if st, _ := x.loadState(wk); len(st.Watches) != 0 {
		t.Fatalf("work account sees the personal watches: %+v", st)
	}
	if _, err := x.addWatch(wk, &watch{ID: "wch_work", Name: "boss", From: []string{"boss@example.com"},
		Created: testNow(), Expires: testNow().AddDate(0, 0, 30)}); err != nil {
		t.Fatal(err)
	}
	if st, _ := x.loadState(me); len(st.Watches) != 1 || st.Watches[0].ID != "wch_old" {
		t.Fatalf("personal state changed: %+v", st.Watches)
	}

	// Each account has its own folder settings.
	if err := x.folderAllowed(me, "Sent Messages"); err != nil {
		t.Fatalf("personal: %v", err)
	}
	if err := x.folderAllowed(wk, "Sent Messages"); err == nil {
		t.Fatal("work account's closed folder was readable")
	}

	// Sending from the work account: the approval names it, and the approved send uses its login.
	if _, err := x.requestSend(ctx, wk, sendIn{draft: draft{To: []string{"anna@example.com"}, Subject: "Report", Body: "Attached."}}); err != nil {
		t.Fatal(err)
	}
	req := b.pending[0]
	if !strings.Contains(req.Summary, "("+workUser+")") || req.Preview.(map[string]string)["account"] != workUser {
		t.Fatalf("approval doesn't name the account: %q %v", req.Summary, req.Preview)
	}
	payload, _ := json.Marshal(req.Payload)
	sendMail = func(_, user, pw, from string, to []string, msg []byte) error {
		if user != workUser || pw != workPass || from != workUser {
			return errAuth
		}
		*sent = append(*sent, sentMail{from: from, to: to, raw: string(msg)})
		return nil
	}
	res, err := execute(ctx, b, "send", payload, func(ctx context.Context, a *account, option string, p json.RawMessage) (any, error) {
		var m mailPayload
		if err := json.Unmarshal(p, &m); err != nil {
			return nil, err
		}
		return x.send(a, m, false)
	})
	if err != nil || res.(map[string]any)["status"] != "sent" || len(*sent) != 1 {
		t.Fatalf("send from work: %v %v", res, err)
	}

	// A payload from before accounts existed runs on the default account.
	old, _ := json.Marshal(readPayload{Op: "list"})
	var ran string
	_, _ = execute(ctx, b, "", old, func(_ context.Context, a *account, _ string, p json.RawMessage) (any, error) {
		ran = a.id
		var r readPayload
		return nil, json.Unmarshal(p, &r)
	})
	if ran != testUser {
		t.Fatalf("legacy payload ran on %q", ran)
	}

	// New mail in both mailboxes: each account's watch fires, and the event names the account.
	deliver(t, addr, "INBOX", "From: shop@shop.example\nTo: me@icloud.com\nSubject: Shipped\nMessage-ID: <p1@shop.example>\n\nOn its way.\n")
	deliverTo(t, addr, workUser, workPass, "INBOX", "From: Boss <boss@example.com>\nTo: work@icloud.com\nSubject: Today\nMessage-ID: <w1@example.com>\n\nCall me.\n")
	x.pollAll(b)
	got := map[string]string{}
	for _, e := range b.events {
		d := e["data"].(map[string]any)
		got[d["account"].(string)] = d["watch"].(map[string]any)["id"].(string)
	}
	if got[testUser] != "wch_old" || got[workUser] != "wch_work" || len(b.events) != 2 {
		t.Fatalf("watch events: %v", b.events)
	}
}

// deliverTo appends a message to another user's folder.
func deliverTo(t *testing.T, addr, user, pass, box, raw string) {
	t.Helper()
	c, err := dialIMAP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(user, pass).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(c, box, nil, []byte(strings.ReplaceAll(raw, "\n", "\r\n"))); err != nil {
		t.Fatal(err)
	}
}

func testNow() time.Time { return time.Now().UTC() }

// Gmail: the app password is pasted with spaces, and the server files sent mail by itself.
func TestGmailStyleProvider(t *testing.T) {
	p := testProvider
	p.SavesSent = true
	p.CleanPassword = func(s string) string { return strings.Join(strings.Fields(s), "") }
	use(p)
	defer use(testProvider)
	x, h, addr, sent := setup(t)
	secrets := map[string]string{"app_password": " abcd-efgh -ijkl-mnop "}
	if _, _, err := x.Validate(context.Background(), map[string]string{"address": testUser}, secrets); err != nil ||
		secrets["app_password"] != testPass {
		t.Fatalf("spaced password: %v %q", err, secrets["app_password"])
	}
	msg, err := compose(defaultSettingsFor(t, h), draft{To: []string{"anna@example.com"}, Subject: "Hi", Body: "x"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := x.send(h, payloadOf(msg, 0), false)
	if err != nil || res.(map[string]any)["saved_to_sent"] != true || len(*sent) != 1 {
		t.Fatalf("send: %v %v", res, err)
	}
	c, err := dialIMAP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Login(testUser, testPass).Wait()
	if msgs, _ := search(c, searchQuery{Mailbox: "Sent Messages"}); len(msgs) != 0 {
		t.Fatalf("a copy was appended to Sent: %d", len(msgs))
	}
}

func defaultSettingsFor(t *testing.T, h host) Settings {
	t.Helper()
	var s Settings
	if err := h.Settings(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

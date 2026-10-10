package mailkit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Work that happens later: an approved email waits a few seconds before it goes out (so it can still be
// cancelled) or until the time the agent asked for; snoozed mail comes back to the inbox at its time; and
// an IDLE connection per mailbox tells the plugin about new mail at once, instead of every two minutes.

var undoSetting = rubiplugin.ConfigField{Key: "undo_send", Label: "Undo send", Type: "choice", Default: "30", PerAccount: true,
	Help: "After you approve an email, it waits this long before it goes out; you can cancel it in the meantime (in the Rubi panel, or your agent can).",
	Options: []rubiplugin.Option{
		{Key: "0", Label: "Send at once"},
		{Key: "10", Label: "10 seconds"},
		{Key: "30", Label: "30 seconds"},
		{Key: "60", Label: "1 minute"},
		{Key: "300", Label: "5 minutes"}}}

var signatureSetting = rubiplugin.ConfigField{Key: "signature", Label: "Signature", Type: "text", Default: "", PerAccount: true,
	Help: "Added under emails your agent writes from this mailbox (it can leave it out for one). Empty: none."}

const (
	maxSchedule = 30 * 24 * time.Hour
	snoozeBox   = "Rubi/Snoozed"
	jobsTick    = 5 * time.Second
)

// queued is an approved email waiting to go out.
type queued struct {
	ID      string      `json:"id"`
	SendAt  time.Time   `json:"send_at"`
	Mail    mailPayload `json:"mail"`
	Track   bool        `json:"track,omitempty"`
	Planned bool        `json:"planned,omitempty"` // the agent chose the time (send later), not just the undo delay
}

// snoozed is mail put away until a time.
type snoozed struct {
	ID      string    `json:"id"`
	Until   time.Time `json:"until"`
	IDs     []string  `json:"ids"`
	Subject string    `json:"subject,omitempty"`
	From    string    `json:"from,omitempty"`
	Count   int       `json:"count"`
}

func undoDelay(h host) time.Duration {
	var cfg struct {
		UndoSend string `json:"undo_send"`
	}
	if err := h.Config(&cfg); err != nil {
		return 30 * time.Second
	}
	n, err := strconv.Atoi(cfg.UndoSend)
	if err != nil || n < 0 {
		return 30 * time.Second
	}
	return time.Duration(min(n, 300)) * time.Second
}

// parseWhen reads a time: RFC 3339, "2026-10-12 09:00" (local time), or a delay like 3h, 2d, 1w.
func parseWhen(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, f := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, v, time.Local); err == nil {
			return t, nil
		}
	}
	if len(v) >= 2 {
		if n, err := strconv.Atoi(v[:len(v)-1]); err == nil && n > 0 {
			switch v[len(v)-1] {
			case 'm':
				return now.Add(time.Duration(n) * time.Minute), nil
			case 'h':
				return now.Add(time.Duration(n) * time.Hour), nil
			case 'd':
				return now.AddDate(0, 0, n), nil
			case 'w':
				return now.AddDate(0, 0, 7*n), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("times look like 2026-10-12 09:00, an RFC 3339 time, or 3h / 2d; got %q", v)
}

// deliver runs an approved send: at once, or queued for the undo delay or the planned time.
func (x *integration) deliver(h host, m mailPayload, track bool) (any, error) {
	now := time.Now().UTC()
	at := now.Add(undoDelay(h))
	planned := !m.SendAt.IsZero() && m.SendAt.After(at)
	if planned {
		at = m.SendAt
	}
	if !at.After(now) {
		return x.send(h, m, track)
	}
	q := &queued{ID: randomID("snd_"), SendAt: at, Mail: m, Track: track, Planned: planned}
	x.mu.Lock()
	st, err := x.loadState(h)
	if err == nil {
		st.Queued = append(st.Queued, q)
		err = h.SaveState(st)
	}
	x.mu.Unlock()
	if err != nil {
		return nil, err
	}
	x.kickJobs()
	h.Audit("send_queued", map[string]any{"send_id": q.ID, "send_at": at, "to": m.To})
	return map[string]any{"status": "scheduled", "send_id": q.ID, "send_at": at.Format(time.RFC3339),
		"note": "It goes out at send_at. Until then " + tool("cancel_send") + "(send_id) or the user's Cancel in the Rubi panel stops it."}, nil
}

// cancelSend takes a queued email back.
func (x *integration) cancelSend(h host, id string) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	for i, q := range st.Queued {
		if q.ID == id {
			st.Queued = append(st.Queued[:i], st.Queued[i+1:]...)
			if err := h.SaveState(st); err != nil {
				return nil, err
			}
			q.Mail.dropStash()
			h.Audit("send_cancelled", map[string]any{"send_id": id})
			return map[string]any{"status": "cancelled", "send_id": id, "subject": q.Mail.Subject}, nil
		}
	}
	return nil, errors.New("no such email waiting to be sent (it may have gone out already)")
}

func (x *integration) scheduled(h host) (any, error) {
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	sends := []map[string]any{}
	for _, q := range st.Queued {
		sends = append(sends, map[string]any{"send_id": q.ID, "send_at": q.SendAt.Format(time.RFC3339), "to": q.Mail.To, "subject": q.Mail.Subject})
	}
	snz := []map[string]any{}
	for _, s := range st.Snoozed {
		snz = append(snz, map[string]any{"snooze_id": s.ID, "until": s.Until.Format(time.RFC3339), "emails": s.Count, "subject": s.Subject})
	}
	return map[string]any{"waiting_to_send": sends, "snoozed": snz}, nil
}

// snooze puts mail away in Rubi/Snoozed until a time; it comes back to the inbox unread.
func (x *integration) snooze(h host, c *imapclient.Client, sp specials, box string, ts []target, until time.Time) (*undoRec, error) {
	if !sp.has(snoozeBox) {
		if err := c.Create(snoozeBox, nil).Wait(); err != nil {
			return nil, fmt.Errorf("couldn't create %s: %w", snoozeBox, err)
		}
		sp.boxes = append(sp.boxes, mailbox{Name: snoozeBox})
	}
	ids := idsOf(ts)
	if len(ids) == 0 {
		return nil, errors.New("these messages have no Message-ID, so they can't be snoozed")
	}
	rec, err := x.moveTargets(c, sp, box, ts, snoozeBox)
	if err != nil {
		return nil, err
	}
	s := &snoozed{ID: randomID("snz_"), Until: until.UTC(), IDs: ids, Subject: ts[0].subject, From: senderOnly(ts[0].from), Count: len(ts)}
	x.mu.Lock()
	st, err := x.loadState(h)
	if err == nil {
		st.Snoozed = append(st.Snoozed, s)
		err = h.SaveState(st)
	}
	x.mu.Unlock()
	return rec, err
}

// ---- the jobs loop ----

func (x *integration) kickJobs() {
	x.mu.Lock()
	ch := x.jobsNow
	x.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (x *integration) jobsLoop(b base, stop, now chan struct{}) {
	t := time.NewTicker(jobsTick)
	defer t.Stop()
	defer x.stopIdlers()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-now:
		}
		list, err := accounts(b)
		if err != nil {
			continue
		}
		for _, a := range list {
			x.runJobs(a)
		}
		x.manageIdlers(list)
	}
}

// runJobs sends what is due and brings back snoozed mail that is due.
func (x *integration) runJobs(h host) {
	now := time.Now()
	x.mu.Lock()
	st, err := x.loadState(h)
	if err != nil {
		x.mu.Unlock()
		return
	}
	var due []*queued
	var keep []*queued
	for _, q := range st.Queued {
		if !q.SendAt.After(now) {
			due = append(due, q)
		} else {
			keep = append(keep, q)
		}
	}
	var back, stay []*snoozed
	for _, s := range st.Snoozed {
		if !s.Until.After(now) {
			back = append(back, s)
		} else {
			stay = append(stay, s)
		}
	}
	if len(due)+len(back) > 0 {
		st.Queued, st.Snoozed = keep, stay
		if err := h.SaveState(st); err != nil {
			x.mu.Unlock()
			return
		}
	}
	x.mu.Unlock()
	for _, q := range due {
		res, err := x.send(h, q.Mail, q.Track)
		switch {
		case err != nil:
			h.Logf("a queued email failed: %v", err)
			_, _ = h.Emit("scheduled", map[string]any{"send_id": q.ID, "status": "failed", "subject": q.Mail.Subject, "error": err.Error()})
		case q.Planned:
			out := map[string]any{"send_id": q.ID, "status": "sent", "subject": q.Mail.Subject, "to": q.Mail.To}
			if r, ok := res.(map[string]any); ok && r["tracking"] != nil {
				out["tracking"] = r["tracking"]
			}
			_, _ = h.Emit("scheduled", out)
		}
	}
	if len(back) > 0 {
		x.unsnooze(h, back)
	}
}

func (x *integration) unsnooze(h host, list []*snoozed) {
	c, _, err := session(h)
	if err != nil {
		h.Logf("snoozed mail can't come back yet: %v", err)
		x.mu.Lock()
		if st, err := x.loadState(h); err == nil { // try again on the next round
			st.Snoozed = append(st.Snoozed, list...)
			_ = h.SaveState(st)
		}
		x.mu.Unlock()
		return
	}
	defer logout(c)
	for _, s := range list {
		set, err := findByID(c, snoozeBox, s.IDs, false)
		if err != nil || len(set) == 0 {
			continue // moved or deleted meanwhile
		}
		if prov.Gmail { // a label: add Inbox, remove the Snoozed label
			if _, err := c.Copy(set, "INBOX").Wait(); err != nil {
				continue
			}
			_ = removeByID(c, snoozeBox, s.IDs)
		} else if _, err := c.Move(set, "INBOX").Wait(); err != nil {
			continue
		}
		if back, err := findByID(c, "INBOX", s.IDs, false); err == nil && len(back) > 0 {
			_ = c.Store(back, &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
		}
		_, _ = h.Emit("snooze", map[string]any{"snooze_id": s.ID, "mailbox": "INBOX", "emails": s.Count, "subject": s.Subject, "from": s.From})
		h.Audit("snooze_back", map[string]any{"snooze_id": s.ID})
	}
}

// ---- IDLE ----

type idler struct {
	stop chan struct{}
}

// needsWatching: is anything following this mailbox's new mail?
func (x *integration) needsWatching(h host) bool {
	st, err := x.loadState(h)
	if err != nil {
		return false
	}
	now := time.Now()
	return (len(st.active(now)) > 0 || len(st.activeWatches(now)) > 0 || len(st.Rules) > 0) && x.folderAllowed(h, "INBOX") == nil
}

func (x *integration) manageIdlers(list []*account) {
	want := map[string]*account{}
	for _, a := range list {
		if x.needsWatching(a) {
			want[a.id] = a
		}
	}
	x.idleMu.Lock()
	defer x.idleMu.Unlock()
	if x.idlers == nil {
		x.idlers = map[string]*idler{}
	}
	for id, i := range x.idlers {
		if want[id] == nil {
			close(i.stop)
			delete(x.idlers, id)
		}
	}
	for id, a := range want {
		if x.idlers[id] == nil {
			i := &idler{stop: make(chan struct{})}
			x.idlers[id] = i
			go x.idle(a, i.stop)
		}
	}
}

func (x *integration) stopIdlers() {
	x.idleMu.Lock()
	defer x.idleMu.Unlock()
	for id, i := range x.idlers {
		close(i.stop)
		delete(x.idlers, id)
	}
}

// idle keeps an IDLE connection on INBOX and asks for a mail check whenever the server says new mail came.
func (x *integration) idle(h host, stop chan struct{}) {
	wait := func(d time.Duration) bool {
		select {
		case <-stop:
			return false
		case <-time.After(d):
			return true
		}
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		if err := x.idleOnce(h, stop); err != nil {
			h.Logf("listening for new mail paused: %v", err)
		}
		if !wait(time.Minute) {
			return
		}
	}
}

func (x *integration) idleOnce(h host, stop chan struct{}) error {
	var s Settings
	if err := h.Settings(&s); err != nil {
		return err
	}
	pw, err := h.Secret("app_password")
	if err != nil {
		return err
	}
	var last atomic.Uint32
	var once sync.Once
	c, err := dialIdle(s.IMAPAddr, &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil && *d.NumMessages > last.Load() {
				last.Store(*d.NumMessages)
				x.kickPoll()
			}
		},
	}})
	if err != nil {
		return err
	}
	defer once.Do(func() { c.Close() })
	if err := c.Login(s.user(), pw).Wait(); err != nil {
		return authError{}
	}
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return err
	}
	last.Store(sel.NumMessages)
	cmd, err := c.Idle()
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-stop:
		_ = cmd.Close()
		<-done
		_ = c.Logout().Wait()
		return nil
	case err := <-done:
		return err
	}
}

func (x *integration) kickPoll() {
	x.mu.Lock()
	ch := x.pollNow
	x.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

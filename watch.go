package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Watches let the agent follow new mail: from given senders (addresses or domains) and/or containing given
// words, e.g. delivery updates for an order. Every new matching email wakes the Bot that set the watch, with
// the watch's note. Watches expire (30 days by default) and the agent removes them when done.
//
// Private mail (see privacy.go) only triggers a watch by its sender, and the event then carries no subject
// or text: matching words would reveal its content.

type watch struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	From     []string  `json:"from,omitempty"`
	Keywords []string  `json:"keywords,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Note     string    `json:"note,omitempty"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Hits     int       `json:"hits"`
	LastHit  time.Time `json:"last_hit,omitempty"`
	Stopped  bool      `json:"stopped,omitempty"`
}

const (
	maxWatches      = 50
	maxWatchTerms   = 20
	defaultWatchDay = 30
	maxWatchDays    = 180
)

func (w *watch) active(now time.Time) bool { return !w.Stopped && now.Before(w.Expires) }

func (s *state) activeWatches(now time.Time) []*watch {
	out := []*watch{}
	for _, w := range s.Watches {
		if w.active(now) {
			out = append(out, w)
		}
	}
	return out
}

// matches reports whether a message triggers the watch. text may be empty if the body wasn't fetched.
func (w *watch) matches(from, subject, text string, private bool) bool {
	fromL := strings.ToLower(from)
	if len(w.From) > 0 {
		ok := false
		for _, f := range w.From {
			ok = ok || senderMatches(fromL, f)
		}
		if !ok {
			return false
		}
	}
	if len(w.Keywords) == 0 {
		return len(w.From) > 0
	}
	if private {
		return false
	}
	hay := strings.ToLower(subject + "\n" + text)
	for _, k := range w.Keywords {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

type watchIn struct {
	Name     string   `json:"name" jsonschema:"short name, e.g. Order 4711 delivery"`
	From     []string `json:"from,omitempty" jsonschema:"sender addresses or domains (e.g. ozon.ru); any of them"`
	Keywords []string `json:"keywords,omitempty" jsonschema:"words in the subject or text (e.g. tracking number, 'delivered'); any of them. With from, both must match"`
	Days     int      `json:"days,omitempty" jsonschema:"how long to watch, default 30, max 180"`
	Agent    string   `json:"agent,omitempty" jsonschema:"your Bot's name as connected to Rubi, so new mail wakes you"`
	Note     string   `json:"note,omitempty" jsonschema:"what to do when it triggers (your own note, sent back with each match)"`
	Account  string   `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

type watchIDIn struct {
	ID      string `json:"id"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

func cleanTerms(list []string) ([]string, error) {
	out := []string{}
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			if len(s) > 200 {
				return nil, errors.New("keep each sender or word under 200 characters")
			}
			out = append(out, s)
		}
	}
	if len(out) > maxWatchTerms {
		return nil, fmt.Errorf("at most %d senders and %d words per watch", maxWatchTerms, maxWatchTerms)
	}
	return out, nil
}

func registerWatches(p *rubiplugin.Plugin, x *integration) {
	addTool(p, "watch", "Watch for new mail and get woken for each match: from senders (addresses or domains) and/or containing words, e.g. delivery updates for an order. Pass your Bot name as agent and a note on what to do. Each match wakes you (costs the user's quota), so keep watches specific and remove them when done.",
		func(in watchIn) string { return in.Account },
		func(ctx context.Context, h *account, in watchIn) (any, error) {
			from, err := cleanTerms(in.From)
			if err != nil {
				return nil, err
			}
			words, err := cleanTerms(in.Keywords)
			if err != nil {
				return nil, err
			}
			if len(from) == 0 && len(words) == 0 {
				return nil, errors.New("give at least one sender or word")
			}
			days := in.Days
			if days <= 0 {
				days = defaultWatchDay
			}
			days = min(days, maxWatchDays)
			name := strings.TrimSpace(in.Name)
			if name == "" {
				name = strings.Join(append(append([]string{}, from...), words...), ", ")
			}
			now := time.Now().UTC()
			w := &watch{ID: randomID("wch_"), Name: name, From: from, Keywords: words, Agent: strings.TrimSpace(in.Agent),
				Note: strings.TrimSpace(in.Note), Created: now, Expires: now.Add(time.Duration(days) * 24 * time.Hour)}
			if len(w.Note) > 1000 {
				return nil, errors.New("keep the note under 1000 characters")
			}
			if h.Level(kindWatch) == rubiplugin.None {
				return x.addWatch(h, w)
			}
			summary := "Watch new mail: " + name
			return h.Submit(ctx, rubiplugin.Request{Kind: kindWatch, Summary: summary,
				Preview: map[string]any{"watch": name, "from": strings.Join(from, ", "), "words": strings.Join(words, ", "),
					"until": w.Expires.Format("2006-01-02"), "wakes": w.Agent},
				Options: []rubiplugin.Option{{Key: "allow", Label: "Allow"}}, Payload: w})
		})
	onExecute(p, kindWatch, func(ctx context.Context, h *account, _ string, payload json.RawMessage) (any, error) {
		var w watch
		if err := json.Unmarshal(payload, &w); err != nil {
			return nil, err
		}
		return x.addWatch(h, &w)
	})

	addTool(p, "watches", "Active mail watches, with how often each triggered.", accountOf,
		func(ctx context.Context, h *account, _ accountIn) (any, error) {
			st, err := x.loadState(h)
			if err != nil {
				return nil, err
			}
			return map[string]any{"watches": st.activeWatches(time.Now())}, nil
		})

	addTool(p, "unwatch", "Stop a mail watch (e.g. the parcel was delivered).",
		func(in watchIDIn) string { return in.Account },
		func(ctx context.Context, h *account, in watchIDIn) (any, error) {
			x.mu.Lock()
			defer x.mu.Unlock()
			st, err := x.loadState(h)
			if err != nil {
				return nil, err
			}
			for _, w := range st.Watches {
				if w.ID == in.ID && !w.Stopped {
					w.Stopped = true
					h.Audit("watch_stopped", map[string]any{"watch_id": w.ID})
					return map[string]any{"stopped": true}, h.SaveState(st)
				}
			}
			return map[string]any{"stopped": false}, nil
		})
}

func (x *integration) addWatch(h host, w *watch) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if len(st.activeWatches(now)) >= maxWatches {
		return nil, fmt.Errorf("at most %d active watches; remove some first", maxWatches)
	}
	st.prune(now)
	st.Watches = append(st.Watches, w)
	if err := h.SaveState(st); err != nil {
		return nil, err
	}
	h.Audit("watch_started", map[string]any{"watch_id": w.ID, "senders": len(w.From), "words": len(w.Keywords)})
	x.kick()
	out := map[string]any{"status": "watching", "watch": w}
	if w.Agent == "" {
		out["note"] = "No agent given: matches go to the Bots subscribed to " + prov.Name + " events (or the default Bot)."
	}
	return out, nil
}

// kick makes the watcher check soon (it may have been idle with nothing to watch).
func (x *integration) kick() {
	if x.pollNow != nil {
		select {
		case x.pollNow <- struct{}{}:
		default:
		}
	}
}

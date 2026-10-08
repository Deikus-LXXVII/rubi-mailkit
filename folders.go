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

// Folder access. The user chooses which folders the agent can see (all, or only some), and whether the
// agent may ask for the others; access the user approves (passkey or password) lasts an hour or a day.
// These settings are user-only: the agent can't change them. Writing a draft or a copy to Sent isn't
// reading, so it works regardless.

type folderConfig struct {
	Access   string   `json:"folder_access"`   // "all" | "selected"
	Folders  []string `json:"folders"`         // allowed when Access is "selected"
	Requests string   `json:"folder_requests"` // "ask" | "never"
}

type grant struct {
	Mailbox string    `json:"mailbox"`
	Until   time.Time `json:"until"`
}

var folderSettings = []rubiplugin.ConfigField{
	{Key: "folder_access", Label: "Folders your agent can see", Type: "choice", Default: "all", PerAccount: true,
		Options: []rubiplugin.Option{{Key: "all", Label: "All folders"}, {Key: "selected", Label: "Only the folders checked below"}}},
	{Key: "folders", Label: "Allowed folders", Type: "list", Dynamic: true, Default: []string{"INBOX"}, PerAccount: true,
		Help: "Used when \"Only the folders checked below\" is chosen."},
	{Key: "folder_requests", Label: "Other folders", Type: "choice", Default: "ask", PerAccount: true,
		Options: []rubiplugin.Option{
			{Key: "ask", Label: "My agent may ask; I approve with my passkey or password"},
			{Key: "never", Label: "My agent can't ask"}}},
}

func foldersOf(h host) folderConfig {
	f := folderConfig{Access: "all", Requests: "ask"}
	if err := h.Config(&f); err != nil {
		h.Logf("folder settings unavailable, allowing INBOX only: %v", err)
		return folderConfig{Access: "selected", Folders: []string{"INBOX"}, Requests: "never"}
	}
	return f
}

func sameBox(a, b string) bool {
	a, b = orInbox(a), orInbox(b)
	if strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX") {
		return true
	}
	return a == b
}

// folderAllowed returns nil if the agent may read box, or an explanation it can act on.
func (x *integration) folderAllowed(h host, box string) error {
	f := foldersOf(h)
	if f.Access != "selected" {
		return nil
	}
	for _, b := range f.Folders {
		if sameBox(b, box) {
			return nil
		}
	}
	if st, err := x.loadState(h); err == nil {
		now := time.Now()
		for _, g := range st.Grants {
			if sameBox(g.Mailbox, box) && now.Before(g.Until) {
				return nil
			}
		}
	}
	if f.Requests == "never" {
		return fmt.Errorf("the user doesn't let you access the folder %q", orInbox(box))
	}
	return fmt.Errorf("the folder %q is closed to you. If you need it, call "+tool("folder_access")+"(mailbox, reason): "+
		"the user approves with their passkey or password", orInbox(box))
}

type folderIn struct {
	Mailbox string `json:"mailbox"`
	Reason  string `json:"reason" jsonschema:"why you need it, shown to the user"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

func registerFolders(p *rubiplugin.Plugin, x *integration) {
	p.ConfigOptions = func(ctx context.Context, h *rubiplugin.Host, ref, key string) ([]rubiplugin.Option, error) {
		if key != "folders" {
			return nil, nil
		}
		a, err := accountFor(h, ref)
		if err != nil {
			return nil, err
		}
		c, _, err := session(a)
		if err != nil {
			return nil, err
		}
		defer logout(c)
		boxes, err := listMailboxes(c)
		if err != nil {
			return nil, err
		}
		out := make([]rubiplugin.Option, 0, len(boxes))
		for _, b := range boxes {
			out = append(out, rubiplugin.Option{Key: b.Name, Label: b.Name})
		}
		return out, nil
	}

	addTool(p, "folder_access", "Ask the user for access to a mail folder they closed to you. They approve with their passkey or password, for an hour or a day.",
		func(in folderIn) string { return in.Account },
		func(ctx context.Context, h *account, in folderIn) (any, error) {
			if strings.TrimSpace(in.Mailbox) == "" || strings.TrimSpace(in.Reason) == "" {
				return nil, errors.New("give the folder (mailbox) and why you need it (reason)")
			}
			err := x.folderAllowed(h, in.Mailbox)
			if err == nil {
				return map[string]any{"status": "already_allowed"}, nil
			}
			if foldersOf(h).Requests == "never" {
				return nil, err
			}
			return h.Submit(ctx, rubiplugin.Request{Kind: kindFolder,
				Summary: "Let your agent read the folder " + in.Mailbox,
				Preview: map[string]any{"folder": in.Mailbox, "reason": strings.TrimSpace(in.Reason)},
				Options: []rubiplugin.Option{{Key: "1h", Label: "Allow for 1 hour"}, {Key: "24h", Label: "Allow for 24 hours"}},
				Payload: folderIn{Mailbox: in.Mailbox}})
		})
	onExecute(p, kindFolder, func(ctx context.Context, h *account, option string, payload json.RawMessage) (any, error) {
		var in folderIn
		if err := json.Unmarshal(payload, &in); err != nil {
			return nil, err
		}
		d := time.Hour
		if option == "24h" {
			d = 24 * time.Hour
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		st, err := x.loadState(h)
		if err != nil {
			return nil, err
		}
		until := time.Now().Add(d).UTC()
		st.prune(time.Now())
		st.Grants = append(st.Grants, &grant{Mailbox: in.Mailbox, Until: until})
		if err := h.SaveState(st); err != nil {
			return nil, err
		}
		h.Audit("folder_granted", map[string]any{"mailbox": in.Mailbox, "until": until})
		return map[string]any{"status": "allowed", "mailbox": in.Mailbox, "until": until}, nil
	})
}

package mailkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Several accounts. Rubi keeps each connected mailbox's settings, password and per-account settings
// (which folders the agent sees) apart; the privacy filter applies to all of them. The plugin works on
// one account at a time through an *account, which narrows the host to that mailbox: its settings, its
// secret, its part of the plugin state, and approvals and events that carry its name.

// base is what the plugin needs from Rubi (*rubiplugin.Host; a fake in tests).
type base interface {
	SettingsFor(account string, v any) error
	SecretFor(account, key string) (string, error)
	ConfigFor(account string, v any) error
	Accounts() ([]rubiplugin.AccountInfo, error)
	LoadState(v any) error
	SaveState(v any) error
	Level(kind string) rubiplugin.Level
	Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error)
	EmitTo(agent, typ string, data map[string]any) (string, error)
	Audit(event string, fields map[string]any)
	Logf(format string, args ...any)
}

// accountIn is the optional account argument every tool takes.
type accountIn struct {
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

// account is the host narrowed to one connected mailbox. It implements host.
type account struct {
	b         base
	id, label string
	isDefault bool
	defaultID string // where state from the single-account version goes
	several   bool   // more than one account is connected: name it in approvals
}

// accounts lists the connected mailboxes, the default one first.
func accounts(b base) ([]*account, error) {
	list, err := b.Accounts()
	if err != nil {
		return nil, err
	}
	def := 0
	for i, a := range list {
		if a.Default {
			def = i
			break
		}
	}
	out := make([]*account, 0, len(list))
	for i, a := range list {
		acc := &account{b: b, id: a.ID, label: a.Label, isDefault: i == def, defaultID: list[def].ID, several: len(list) > 1}
		if acc.label == "" {
			acc.label = a.ID
		}
		if acc.isDefault {
			out = append([]*account{acc}, out...)
		} else {
			out = append(out, acc)
		}
	}
	return out, nil
}

// accountFor finds a connected mailbox by address or id ("" = the default one).
func accountFor(b base, ref string) (*account, error) {
	list, err := accounts(b)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no %s account is connected", prov.Name)
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return list[0], nil
	}
	var names []string
	for _, a := range list {
		if ref == strings.ToLower(a.id) || ref == strings.ToLower(a.label) {
			return a, nil
		}
		names = append(names, a.label)
	}
	return nil, fmt.Errorf("no connected %s account %q; connected: %s", prov.Name, ref, strings.Join(names, ", "))
}

func (a *account) Settings(v any) error               { return a.b.SettingsFor(a.id, v) }
func (a *account) Secret(key string) (string, error)  { return a.b.SecretFor(a.id, key) }
func (a *account) Config(v any) error                 { return a.b.ConfigFor(a.id, v) }
func (a *account) Level(kind string) rubiplugin.Level { return a.b.Level(kind) }

// Logf writes to the plugin's log, a plain file on disk: accounts appear there by a short code, not by
// their address or name.
func (a *account) Logf(format string, args ...any) {
	sum := sha256.Sum256([]byte(a.id))
	a.b.Logf("[account "+hex.EncodeToString(sum[:3])+"] "+format, args...)
}
func (a *account) Emit(typ string, data map[string]any) (string, error) {
	return a.EmitTo("", typ, data)
}

func (a *account) EmitTo(agent, typ string, data map[string]any) (string, error) {
	data["account"] = a.label
	return a.b.EmitTo(agent, typ, data)
}

func (a *account) Audit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["account"] = a.id
	a.b.Audit(event, fields)
}

// scoped is an approval payload together with the account it belongs to.
type scoped struct {
	Account string          `json:"rubi_account"`
	Data    json.RawMessage `json:"data"`
}

// Submit asks for approval of an action on this account; the account goes with the payload, and the
// user sees which mailbox it is when there are several.
func (a *account) Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error) {
	data, err := json.Marshal(r.Payload)
	if err != nil {
		return nil, err
	}
	r.Payload = scoped{Account: a.id, Data: data}
	if a.several {
		r.Summary += " (" + a.label + ")"
		switch p := r.Preview.(type) {
		case map[string]string:
			p["account"] = a.label
		case map[string]any:
			p["account"] = a.label
		}
	}
	return a.b.Submit(ctx, r)
}

// State. The plugin state holds each account's part under "accounts". State saved before accounts
// existed belongs to the default account.

type allState struct {
	Accounts map[string]json.RawMessage `json:"accounts"`
}

func (a *account) loadAll() (allState, error) {
	var raw map[string]json.RawMessage
	if err := a.b.LoadState(&raw); err != nil {
		return allState{}, err
	}
	all := allState{Accounts: map[string]json.RawMessage{}}
	if acc, ok := raw["accounts"]; ok {
		if err := json.Unmarshal(acc, &all.Accounts); err != nil {
			return all, err
		}
		if all.Accounts == nil {
			all.Accounts = map[string]json.RawMessage{}
		}
		return all, nil
	}
	if len(raw) > 0 && a.defaultID != "" { // the state of the single-account version
		legacy, _ := json.Marshal(raw)
		all.Accounts[a.defaultID] = legacy
	}
	return all, nil
}

func (a *account) LoadState(v any) error {
	all, err := a.loadAll()
	if err != nil {
		return err
	}
	if s, ok := all.Accounts[a.id]; ok {
		return json.Unmarshal(s, v)
	}
	return nil
}

func (a *account) SaveState(v any) error {
	all, err := a.loadAll()
	if err != nil {
		return err
	}
	if all.Accounts[a.id], err = json.Marshal(v); err != nil {
		return err
	}
	return a.b.SaveState(all)
}

// addTool registers a tool that works on one account, picked by the account argument.
func addTool[T any](p *rubiplugin.Plugin, name, description string, ref func(T) string,
	fn func(ctx context.Context, a *account, in T) (any, error)) {
	rubiplugin.AddTool(p, tool(name), description,
		func(ctx context.Context, h *rubiplugin.Host, in T) (any, error) {
			a, err := accountFor(h, ref(in))
			if err != nil {
				return nil, err
			}
			return fn(ctx, a, in)
		})
}

// onExecute runs an approved action on the account it was requested for.
func onExecute(p *rubiplugin.Plugin, kind string, fn func(ctx context.Context, a *account, option string, payload json.RawMessage) (any, error)) {
	p.OnExecute(kind, func(ctx context.Context, h *rubiplugin.Host, option string, payload json.RawMessage) (any, error) {
		return execute(ctx, h, option, payload, fn)
	})
}

func execute(ctx context.Context, b base, option string, payload json.RawMessage,
	fn func(ctx context.Context, a *account, option string, payload json.RawMessage) (any, error)) (any, error) {
	var sc scoped
	_ = json.Unmarshal(payload, &sc)
	data := sc.Data
	if len(data) == 0 { // requested before accounts existed: the default account
		data = payload
	}
	a, err := accountFor(b, sc.Account)
	if err != nil {
		return nil, err
	}
	return fn(ctx, a, option, data)
}

package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Unsubscribing with the sender's own List-Unsubscribe: one POST to its web address when it supports
// one-click (RFC 8058), else an email to its unsubscribe address. The agent never has to open the sender's
// site. Each unsubscribe is approved by the user (a button in the chat by default).

type unsubIn struct {
	UID     uint32 `json:"uid" jsonschema:"an email from the mailing list"`
	Mailbox string `json:"mailbox,omitempty" jsonschema:"its folder, default INBOX"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

type unsubPlan struct {
	Method  string `json:"method"` // post | mailto
	URL     string `json:"url,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	From    string `json:"from"` // the list, for the receipt
}

func registerUnsubscribe(p *rubiplugin.Plugin, x *integration) {
	addTool(p, "unsubscribe", "Unsubscribe from the mailing list an email came from, the sender's own way (List-Unsubscribe): one click on their server, or an email to their unsubscribe address. The user approves first.",
		func(in unsubIn) string { return in.Account },
		func(ctx context.Context, a *account, in unsubIn) (any, error) {
			return x.requestUnsubscribe(ctx, a, in)
		})
	onExecute(p, kindUnsubscribe, func(ctx context.Context, a *account, _ string, payload json.RawMessage) (any, error) {
		var plan unsubPlan
		if err := json.Unmarshal(payload, &plan); err != nil {
			return nil, err
		}
		return x.unsubscribe(ctx, a, plan)
	})
}

func (x *integration) requestUnsubscribe(ctx context.Context, h host, in unsubIn) (any, error) {
	if in.UID == 0 {
		return nil, errors.New("give the uid of an email from the list")
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
	logout(c)
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
	plan, err := planUnsubscribe(m)
	if err != nil {
		return nil, err
	}
	how := "one click on " + hostOf(plan.URL)
	if plan.Method == "mailto" {
		how = "an email to " + plan.To
	}
	return h.Submit(ctx, rubiplugin.Request{Kind: kindUnsubscribe, Summary: "Unsubscribe from " + plan.From,
		Preview: map[string]any{"list": plan.From, "example": m.Subject, "how": how},
		Options: []rubiplugin.Option{{Key: "unsubscribe", Label: "Unsubscribe"}}, Payload: plan})
}

func planUnsubscribe(m *message) (unsubPlan, error) {
	plan := unsubPlan{From: m.From}
	var web string
	for _, u := range m.Unsubscribe {
		l := strings.ToLower(u)
		switch {
		case strings.HasPrefix(l, "https://") && m.OneClick && plan.Method == "":
			plan.Method, plan.URL = "post", u
		case strings.HasPrefix(l, "mailto:") && plan.To == "":
			mu, err := url.Parse(u)
			if err != nil || mu.Opaque == "" {
				continue
			}
			to, _ := url.PathUnescape(mu.Opaque)
			if !strings.Contains(to, "@") || strings.ContainsAny(to, "\r\n,;") {
				continue
			}
			plan.To = to
			plan.Subject = orDefault(mu.Query().Get("subject"), "unsubscribe")
			plan.Body = orDefault(mu.Query().Get("body"), "unsubscribe")
		case strings.HasPrefix(l, "http"):
			web = u
		}
	}
	if plan.Method == "" && plan.To != "" {
		plan.Method = "mailto"
	}
	if plan.Method == "" {
		if web != "" {
			return plan, fmt.Errorf("this list unsubscribes only on its web page (%s); it isn't done by one request. Tell the user, or open it only if they ask", web)
		}
		return plan, errors.New("this email doesn't say how to unsubscribe (no List-Unsubscribe)")
	}
	return plan, nil
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Hostname()
	}
	return u
}

func (x *integration) unsubscribe(ctx context.Context, h host, plan unsubPlan) (any, error) {
	switch plan.Method {
	case "post":
		if err := oneClick(ctx, plan.URL); err != nil {
			return nil, err
		}
	case "mailto":
		var s Settings
		if err := h.Settings(&s); err != nil {
			return nil, err
		}
		msg, err := compose(s, draft{To: []string{plan.To}, Subject: plan.Subject, Body: plan.Body}, "", "")
		if err != nil {
			return nil, err
		}
		if _, err := x.send(h, payloadOf(msg, 0), false); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown way to unsubscribe")
	}
	h.Audit("unsubscribed", map[string]any{"list": plan.From, "method": plan.Method})
	return map[string]any{"status": "unsubscribed", "list": plan.From, "method": plan.Method,
		"note": "Some lists take a few days to stop."}, nil
}

// unsubPost is a seam for tests.
var unsubPost = func(ctx context.Context, u string) (int, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return rubiplugin.Dial(ctx, addr)
		}, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// oneClick posts the RFC 8058 unsubscribe request, to public https addresses only.
func oneClick(ctx context.Context, u string) error {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || pu.Hostname() == "" || pu.User != nil {
		return errors.New("the unsubscribe address isn't a plain https address")
	}
	if !publicHost(ctx, pu.Hostname()) {
		return errors.New("the unsubscribe address points into a private network; not following it")
	}
	code, err := unsubPost(ctx, u)
	if err != nil {
		return fmt.Errorf("the list's server didn't answer: %w", err)
	}
	if code >= 400 {
		return fmt.Errorf("the list's server refused (HTTP %d); the user may have to unsubscribe on the web", code)
	}
	return nil
}

// publicHost refuses names and addresses of the local machine or private networks.
func publicHost(ctx context.Context, host string) bool {
	l := strings.ToLower(host)
	if l == "localhost" || strings.HasSuffix(l, ".localhost") || strings.HasSuffix(l, ".local") || strings.HasSuffix(l, ".internal") || !strings.Contains(l, ".") {
		return false
	}
	check := func(ip net.IP) bool {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() ||
			ip.Equal(net.IPv4(169, 254, 169, 254)))
	}
	if ip := net.ParseIP(host); ip != nil {
		return check(ip)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip", host)
	if err != nil {
		return true // a machine whose DNS goes through its egress proxy: the proxy resolves it
	}
	for _, ip := range ips {
		// 198.18.0.0/15 is what some sandboxes' fake DNS answers; the proxy then resolves for real.
		if !check(ip) && !(ip.To4() != nil && ip.To4()[0] == 198 && (ip.To4()[1] == 18 || ip.To4()[1] == 19)) {
			return false
		}
	}
	return true
}

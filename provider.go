// Package mailkit is the shared engine of the Rubi mail plugins (iCloud Mail, Gmail, …): read, search and
// draft freely (by default), send only after the user's approval, get notified about replies and about
// new mail the agent watches for, keep private mail (sign-in codes and the like) away from the agent, and
// limit which folders it sees. A plugin is a Provider plus a one-line main:
//
//	func main() { mailkit.Main(provider) }
//
// A plugin can have several connected accounts. Every tool takes an optional account (the address);
// without it the default account is used.
package mailkit

import (
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Provider describes one mail service that signs in with an app password over IMAP and SMTP.
type Provider struct {
	ID          string // plugin id, e.g. "gmail"
	Name        string // e.g. "Gmail"
	Version     string
	MinRubi     string
	Description string
	Needs       string // what the user needs to connect, shown before setup
	Source      string // source repository URL
	ToolPrefix  string // tool names are <ToolPrefix>_search, …

	AddressLabel       string
	AddressPlaceholder string
	AddressHelp        string
	PasswordLabel      string
	PasswordHelp       string
	PasswordURL        string // where the user creates the app password
	PasswordLink       string // label of the PasswordURL button
	// CleanPassword normalizes what the user pasted (default: trim spaces at the ends).
	CleanPassword func(string) string

	IMAPAddr string // host:port, implicit TLS
	SMTPAddr string // host:port, STARTTLS
	Drafts   string // folder names when the server doesn't mark them (special-use)
	Sent     string
	// SavesSent is true when the server files mail sent over SMTP in Sent by itself (Gmail); otherwise
	// the plugin appends a copy.
	SavesSent bool
	// MailDomain is used in Message-IDs when the address has none.
	MailDomain string
	// AuthError is shown when the server rejects the login.
	AuthError string
}

// PublisherKey is the Rubi-Project plugin signing key (public half).
const PublisherKey = "MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="

// prov is the provider this binary serves; set by Main (and by tests).
var prov Provider

var kindRead, kindDraft, kindSend, kindWatch, kindPrivate, kindFolder, kindAttachment string

func use(p Provider) {
	if p.CleanPassword == nil {
		p.CleanPassword = strings.TrimSpace
	}
	prov = p
	kindRead, kindDraft, kindSend = p.ID+".read", p.ID+".draft", p.ID+".send"
	kindWatch, kindPrivate, kindFolder = p.ID+".watch", p.ID+".private", p.ID+".folder"
	kindAttachment = p.ID + ".attachment"
	defaultIMAPAddr, defaultSMTPAddr = p.IMAPAddr, p.SMTPAddr
}

// tool is the full name of one of the plugin's tools.
func tool(name string) string { return prov.ToolPrefix + "_" + name }

// Main runs the plugin for a provider.
func Main(p Provider) {
	use(p)
	newPlugin(&integration{}).Main()
}

var sendOptions = []rubiplugin.Option{
	{Key: "send", Label: "Send"},
	{Key: "send_track", Label: "Send and notify on reply", Meaning: "send, then watch for replies and notify you"},
}

func manifest() rubiplugin.Manifest {
	p := prov
	return rubiplugin.Manifest{
		ID:          p.ID,
		Name:        p.Name,
		Version:     p.Version,
		Description: p.Description,
		Needs:       p.Needs,
		Publisher:   rubiplugin.Publisher{Name: "Rubi-Project", Key: PublisherKey, URL: p.Source},
		Source:      p.Source,
		MinRubi:     p.MinRubi,
		Entry:       p.ID,
		Fields: []rubiplugin.Field{
			{Key: "address", Label: p.AddressLabel, Type: "email", Placeholder: p.AddressPlaceholder, Required: true, Help: p.AddressHelp},
			{Key: "from_name", Label: "Your name (shown to recipients)", Type: "text", Placeholder: "Optional"},
		},
		Secrets: []rubiplugin.Secret{{Key: "app_password", Label: p.PasswordLabel, Help: p.PasswordHelp,
			HelpURL: p.PasswordURL, HelpLink: p.PasswordLink}},
		Actions: []rubiplugin.Action{
			{Kind: kindRead, Title: "Read and search mail", DefaultLevel: rubiplugin.None},
			{Kind: kindDraft, Title: "Save drafts", DefaultLevel: rubiplugin.None},
			{Kind: kindSend, Title: "Send email", DefaultLevel: rubiplugin.Strong, Options: sendOptions},
			{Kind: kindWatch, Title: "Watch for new mail (wakes your agent)", DefaultLevel: rubiplugin.None},
			{Kind: kindPrivate, Title: "Show a private email", DefaultLevel: rubiplugin.Strong, Locked: true,
				Options: []rubiplugin.Option{{Key: "show", Label: "Show it to my agent"}}},
			{Kind: kindFolder, Title: "Open a closed folder for a while", DefaultLevel: rubiplugin.Strong, Locked: true},
			{Kind: kindAttachment, Title: "Give an attachment to your agent", DefaultLevel: rubiplugin.Strong, Locked: true,
				Options: []rubiplugin.Option{{Key: "show", Label: "Give it to my agent"}}},
		},
		Events: []rubiplugin.EventType{
			{Type: "reply", Untrusted: []string{"reply.from", "reply.subject"}},
			{Type: "watch", Untrusted: []string{"message.from", "message.subject", "message.snippet"}},
		},
		Config: append(append([]rubiplugin.ConfigField{}, folderSettings...), attachmentSetting,
			rubiplugin.ConfigField{Key: "hide_codes", Label: "Hide sign-in codes, one-time passwords and confirmation links", Type: "bool", Default: true,
				Help: "Built-in list, English and Russian, plus subjects like \"482913 is your code\"."},
			rubiplugin.ConfigField{Key: "hide_password_resets", Label: "Hide password reset emails", Type: "bool", Default: true},
			rubiplugin.ConfigField{Key: "hide_sign_in_alerts", Label: "Hide sign-in and security alerts", Type: "bool", Default: false,
				Help: "New sign-ins, new devices, suspicious activity. Off by default, so your agent can warn you; an alert that contains a code stays hidden by the first switch."},
			rubiplugin.ConfigField{Key: "hidden_senders", Label: "Hidden senders", Type: "list", Default: []string{},
				Help: "Email addresses or domains, one per line (e.g. bank.com). Your agent can't see mail from them."},
			rubiplugin.ConfigField{Key: "hidden_keywords", Label: "Hidden words", Type: "list", Default: []string{},
				Help: "Words or phrases, one per line. Your agent can't see mail whose subject or text contains one."},
		),
		Egress: []string{p.IMAPAddr, p.SMTPAddr},
	}
}

package mailkit

import (
	"errors"
	"net"
	"regexp"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Other mail services: any IMAP and SMTP server that takes an app password (Yandex, Mail.ru, Fastmail,
// Yahoo, Zoho, GMX, a server of one's own). Well-known services fill in their servers by themselves.

type preset struct{ imap, smtp string }

var presets = map[string]preset{
	"yandex.ru": {"imap.yandex.ru:993", "smtp.yandex.ru:465"}, "yandex.com": {"imap.yandex.com:993", "smtp.yandex.com:465"},
	"ya.ru":   {"imap.yandex.ru:993", "smtp.yandex.ru:465"},
	"mail.ru": {"imap.mail.ru:993", "smtp.mail.ru:465"}, "inbox.ru": {"imap.mail.ru:993", "smtp.mail.ru:465"},
	"list.ru": {"imap.mail.ru:993", "smtp.mail.ru:465"}, "bk.ru": {"imap.mail.ru:993", "smtp.mail.ru:465"},
	"fastmail.com": {"imap.fastmail.com:993", "smtp.fastmail.com:465"}, "fastmail.fm": {"imap.fastmail.com:993", "smtp.fastmail.com:465"},
	"yahoo.com": {"imap.mail.yahoo.com:993", "smtp.mail.yahoo.com:465"}, "aol.com": {"imap.aol.com:993", "smtp.aol.com:465"},
	"zoho.com": {"imap.zoho.com:993", "smtp.zoho.com:465"}, "zohomail.com": {"imap.zoho.com:993", "smtp.zoho.com:465"},
	"gmx.com": {"imap.gmx.com:993", "mail.gmx.com:587"}, "gmx.net": {"imap.gmx.net:993", "mail.gmx.net:587"},
	"gmx.de": {"imap.gmx.net:993", "mail.gmx.net:587"}, "web.de": {"imap.web.de:993", "smtp.web.de:587"},
	"rambler.ru": {"imap.rambler.ru:993", "smtp.rambler.ru:465"},
}

var customFields = []rubiplugin.Field{
	{Key: "imap_server", Label: "IMAP server", Type: "text", Placeholder: "imap.example.com:993",
		Help: "Leave empty for Yandex, Mail.ru, Fastmail, Yahoo, AOL, Zoho, GMX, Web.de, Rambler: Rubi knows them. Otherwise your provider's IMAP over TLS (usually port 993)."},
	{Key: "smtp_server", Label: "SMTP server", Type: "text", Placeholder: "smtp.example.com:465",
		Help: "Port 465 (TLS) or 587 (STARTTLS)."},
	{Key: "username", Label: "Login name", Type: "text", Placeholder: "Optional",
		Help: "Only if your provider's login isn't your email address."},
}

var reHostPort = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$`)

// customServers fills in the servers of an account from the setup fields or a known service.
func customServers(s *Settings, fields map[string]string) error {
	imapAddr, smtpAddr := strings.TrimSpace(fields["imap_server"]), strings.TrimSpace(fields["smtp_server"])
	if imapAddr == "" || smtpAddr == "" {
		p, ok := presets[strings.ToLower(domainOf(s.Address))]
		if !ok {
			if strings.HasSuffix(domainOf(s.Address), "outlook.com") || strings.HasSuffix(domainOf(s.Address), "hotmail.com") || strings.HasSuffix(domainOf(s.Address), "live.com") {
				return errors.New("Outlook.com no longer accepts app passwords for IMAP (only Microsoft sign-in), so it can't be connected this way")
			}
			return errors.New("enter your provider's IMAP and SMTP servers (see its help pages for \"IMAP settings\")")
		}
		imapAddr, smtpAddr = orDefault(imapAddr, p.imap), orDefault(smtpAddr, p.smtp)
	}
	var err error
	if s.IMAPAddr, err = hostPort(imapAddr, "993"); err != nil {
		return errors.New("the IMAP server looks like imap.example.com:993")
	}
	if s.SMTPAddr, err = hostPort(smtpAddr, "465"); err != nil {
		return errors.New("the SMTP server looks like smtp.example.com:465")
	}
	s.Username = strings.TrimSpace(fields["username"])
	return nil
}

func hostPort(v, port string) (string, error) {
	v = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(v), "imaps://"), "smtps://")
	if !reHostPort.MatchString(v) || !strings.Contains(v, ".") {
		return "", errors.New("bad server")
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		v = net.JoinHostPort(v, port)
	}
	return v, nil
}

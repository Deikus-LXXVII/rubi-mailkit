package mailkit

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/mail"
)

// Phishing signs, shown with every email the agent reads: whether the receiving server confirmed the
// sender (SPF, DKIM, DMARC, from the server's own Authentication-Results, not one a sender could add),
// look-alike domains (paypa1.com, xn--…), a display name naming another address, and links whose text
// shows one site while they lead to another. Before sending, the user is told when an email goes to a
// domain they have never written to or heard from.

type security struct {
	SPF      string   `json:"spf,omitempty"`
	DKIM     string   `json:"dkim,omitempty"`
	DMARC    string   `json:"dmarc,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

var (
	reAuthResult = regexp.MustCompile(`(?i)\b(spf|dkim|dmarc)\s*=\s*([a-z]+)`)
	reLure       = regexp.MustCompile(`secure|login|verif|account|support|update|service|billing|help|^id|id$|pay|wallet|confirm|auth|signin|alert|refund|bonus`)
	reNameAddr   = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
)

// brands are often imitated; their look-alikes get a warning.
var brands = []string{"paypal", "apple", "icloud", "google", "gmail", "microsoft", "outlook", "amazon", "netflix",
	"facebook", "instagram", "whatsapp", "telegram", "sberbank", "tinkoff", "tbank", "yandex", "ozon", "wildberries",
	"gosuslugi", "steam", "steampowered", "binance", "coinbase", "github", "dropbox", "docusign", "linkedin", "alfabank", "vtb"}

func checkSecurity(h mail.Header, from string, htmlBody string) *security {
	sec := &security{}
	// The receiving server puts its Authentication-Results on top. Further down may be ones the sender
	// wrote itself, so only the topmost trusted ones count.
	fields := h.FieldsByKey("Authentication-Results")
	for fields.Next() {
		v := fields.Value()
		id := strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
		if !trustedAuthServ(id) {
			if len(prov.AuthServIDs) == 0 {
				break // unknown server: only the topmost header
			}
			continue
		}
		for _, m := range reAuthResult.FindAllStringSubmatch(v, -1) {
			k, res := strings.ToLower(m[1]), strings.ToLower(m[2])
			switch k {
			case "spf":
				if sec.SPF == "" {
					sec.SPF = res
				}
			case "dkim":
				if sec.DKIM == "" || res == "pass" {
					sec.DKIM = res
				}
			case "dmarc":
				if sec.DMARC == "" {
					sec.DMARC = res
				}
			}
		}
		if len(prov.AuthServIDs) == 0 {
			break
		}
	}
	switch {
	case sec.DMARC == "fail":
		sec.Warnings = append(sec.Warnings, "The sender's domain didn't confirm this email (DMARC failed): it may be forged.")
	case sec.SPF != "" && sec.SPF != "pass" && sec.DKIM != "" && sec.DKIM != "pass" && sec.DMARC != "pass":
		sec.Warnings = append(sec.Warnings, "Neither SPF nor DKIM confirmed the sender: be careful with this email.")
	}
	addr := strings.ToLower(senderOnly(from))
	if list, err := h.AddressList("From"); err == nil && len(list) > 0 {
		addr = strings.ToLower(list[0].Address) // not an address written into the display name
	}
	domain := domainOf(addr)
	if w := lookalike(domain); w != "" {
		sec.Warnings = append(sec.Warnings, "The sender's domain "+w)
	}
	if list, err := h.AddressList("From"); err == nil && len(list) == 1 {
		for _, inName := range reNameAddr.FindAllString(list[0].Name, -1) {
			if !strings.EqualFold(inName, list[0].Address) {
				sec.Warnings = append(sec.Warnings, "The sender's name shows "+inName+", but the email comes from "+list[0].Address+".")
				break
			}
		}
	}
	if rt, err := h.AddressList("Reply-To"); err == nil && len(rt) > 0 && domain != "" {
		if d := strings.ToLower(domainOf(rt[0].Address)); d != "" && siteOf(d) != siteOf(domain) && (lookalike(d) != "" || brandIn(domain) != "") {
			sec.Warnings = append(sec.Warnings, "Replies would go to "+d+", not to the sender's domain "+domain+".")
		}
	}
	if htmlBody != "" {
		n := 0
		for _, l := range htmlLinks(htmlBody) {
			if w := linkMismatch(l); w != "" && n < 3 {
				sec.Warnings = append(sec.Warnings, w)
				n++
			}
		}
	}
	if sec.SPF == "" && sec.DKIM == "" && sec.DMARC == "" && len(sec.Warnings) == 0 {
		return nil
	}
	return sec
}

func trustedAuthServ(id string) bool {
	if len(prov.AuthServIDs) == 0 {
		return true
	}
	for _, s := range prov.AuthServIDs {
		if id == s || strings.HasSuffix(id, "."+s) {
			return true
		}
	}
	return false
}

// siteOf is the registrable part of a domain, roughly: the last two labels (three for co.uk-style ones).
func siteOf(d string) string {
	parts := strings.Split(strings.Trim(strings.ToLower(d), "."), ".")
	n := 2
	if len(parts) >= 3 && len(parts[len(parts)-2]) <= 3 && len(parts[len(parts)-1]) == 2 {
		n = 3
	}
	if len(parts) <= n {
		return strings.Join(parts, ".")
	}
	return strings.Join(parts[len(parts)-n:], ".")
}

var confusables = strings.NewReplacer("0", "o", "1", "l", "3", "e", "5", "s", "rn", "m", "vv", "w", "-", "", "_", "")

// brandIn returns the imitated brand a domain's name contains, if its own name isn't that brand.
func brandIn(domain string) string {
	b := siteOf(domain)
	label := strings.SplitN(b, ".", 2)[0]
	norm := confusables.Replace(label)
	for _, br := range brands {
		if label == br {
			return ""
		}
	}
	for _, br := range brands {
		if !strings.Contains(norm, br) {
			continue
		}
		// paypa1, rnicrosoft; or the brand with words phishing domains add (apple-id-verify). Names that
		// merely contain a brand (applebees) pass.
		if norm != strings.NewReplacer("-", "", "_", "").Replace(label) || reLure.MatchString(strings.Replace(norm, br, "", 1)) {
			return br
		}
	}
	return ""
}

// lookalike explains why a domain looks like an imitation ("" if it doesn't).
func lookalike(domain string) string {
	if domain == "" {
		return ""
	}
	d := strings.ToLower(domain)
	if strings.Contains(d, "xn--") {
		return d + " is written with look-alike letters from other alphabets (punycode)."
	}
	scripts := map[string]bool{}
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			scripts["cyrillic"] = true
		case unicode.Is(unicode.Greek, r):
			scripts["greek"] = true
		case r < 128 && unicode.IsLetter(r):
			scripts["latin"] = true
		}
	}
	if len(scripts) > 1 {
		return d + " mixes letters of different alphabets."
	}
	if br := brandIn(d); br != "" {
		return d + " looks like " + br + " but isn't it."
	}
	return ""
}

// linkMismatch warns when a link's text shows one site but it leads to another.
func linkMismatch(l link) string {
	text := strings.ToLower(strings.TrimSpace(l.Text))
	if !strings.Contains(text, ".") || strings.Contains(text, " ") || strings.HasPrefix(strings.ToLower(l.URL), "mailto:") {
		return ""
	}
	shown := text
	if !strings.Contains(shown, "://") {
		shown = "https://" + shown
	}
	su, err1 := url.Parse(shown)
	hu, err2 := url.Parse(l.URL)
	if err1 != nil || err2 != nil || su.Hostname() == "" || hu.Hostname() == "" || !strings.Contains(su.Hostname(), ".") {
		return ""
	}
	if siteOf(su.Hostname()) != siteOf(hu.Hostname()) {
		return "A link shows " + su.Hostname() + " but leads to " + hu.Hostname() + "."
	}
	return ""
}

// newRecipients names recipient domains the user has never written to nor heard from.
func (x *integration) newRecipients(h host, s Settings, msg *composed) string {
	own := strings.ToLower(domainOf(s.Address))
	seen := map[string]bool{}
	var domains []string
	for _, a := range msg.envelope {
		d := strings.ToLower(domainOf(a))
		if d == "" || d == own || seen[d] || len(domains) >= 5 {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return ""
	}
	c, _, err := session(h)
	if err != nil {
		return ""
	}
	defer logout(c)
	var fresh []string
	for _, d := range domains {
		known := false
		for _, look := range []struct{ box, hdr string }{{s.Sent, "To"}, {s.Sent, "Cc"}, {"INBOX", "From"}} {
			if _, err := c.Select(look.box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
				continue
			}
			res, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: look.hdr, Value: "@" + d}}}, nil).Wait()
			if err == nil && len(res.AllUIDs()) > 0 {
				known = true
				break
			}
		}
		if !known {
			fresh = append(fresh, d)
		}
	}
	if len(fresh) == 0 {
		return ""
	}
	return "First email to " + strings.Join(fresh, ", ") + ": you have never written to or heard from anyone there."
}

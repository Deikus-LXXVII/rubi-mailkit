package mailkit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// The formatted (HTML) version of an email, for what plain text loses: buttons such as "Unsubscribe",
// layouts, link targets. The agent gets the links with their text, and a copy of the HTML it can open in a
// browser. The copy loads nothing from the internet (tracking images would tell the sender the email was
// read, and from where): a Content-Security-Policy allows only inline styles and embedded images, and
// redirects in the page are removed.

type link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

const (
	maxLinks  = 200
	viewTTL   = 30 * time.Minute
	cspHeader = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data: cid:; font-src data:; form-action 'none'; base-uri 'none'">`
)

var (
	reMetaRefresh = regexp.MustCompile(`(?is)<meta[^>]+http-equiv\s*=\s*["']?refresh[^>]*>`)
	reBase        = regexp.MustCompile(`(?is)<base[^>]*>`)
	reListUnsub   = regexp.MustCompile(`<([^>]+)>`)
)

// htmlLinks lists the links of an HTML body (http, https and mailto), with their visible text.
func htmlLinks(body string) []link {
	z := html.NewTokenizer(strings.NewReader(body))
	var out []link
	seen := map[string]bool{}
	var cur *link
	var text strings.Builder
	for len(out) < maxLinks {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken:
			name, attr := z.TagName()
			if string(name) != "a" || !attr {
				continue
			}
			for {
				k, v, more := z.TagAttr()
				if string(k) == "href" {
					u := strings.TrimSpace(string(v))
					l := strings.ToLower(u)
					if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:") {
						cur = &link{URL: u}
						text.Reset()
					}
				}
				if !more {
					break
				}
			}
		case html.TextToken:
			if cur != nil {
				text.Write(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" && cur != nil {
				cur.Text = strings.Join(strings.Fields(text.String()), " ")
				if r := []rune(cur.Text); len(r) > 120 {
					cur.Text = string(r[:120])
				}
				if key := cur.Text + "\x00" + cur.URL; !seen[key] {
					seen[key] = true
					out = append(out, *cur)
				}
				cur = nil
			}
		}
	}
	return out
}

// safeHTML is a copy of the email's HTML that loads nothing from the internet when opened.
func safeHTML(body string) string {
	body = reMetaRefresh.ReplaceAllString(body, "")
	body = reBase.ReplaceAllString(body, "")
	return cspHeader + "\n" + body
}

// saveView writes the safe copy to a private folder for a while and returns its path.
func saveView(body string) (string, error) {
	dir, err := privateDir("views", viewTTL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "email.html")
	if err := os.WriteFile(path, []byte(safeHTML(body)), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	time.AfterFunc(viewTTL, func() { os.RemoveAll(dir) })
	return path, nil
}

// listUnsubscribe reads the List-Unsubscribe header (RFC 2369): the sender's own way to unsubscribe, by
// web address or email. oneClick means a single POST to the web address unsubscribes (RFC 8058).
func listUnsubscribe(value, post string) (targets []string, oneClick bool) {
	for _, m := range reListUnsub.FindAllStringSubmatch(value, 4) {
		u := strings.TrimSpace(m[1])
		l := strings.ToLower(u)
		if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "mailto:") {
			targets = append(targets, u)
		}
	}
	return targets, strings.Contains(strings.ToLower(post), "list-unsubscribe=one-click")
}

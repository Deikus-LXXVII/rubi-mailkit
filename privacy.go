package mailkit

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Privacy filter. Some mail should never reach the agent: sign-in codes, one-time passwords, password
// resets, optionally sign-in alerts, and whatever else the user adds. Hidden mail shows up only as "a private email from <sender>";
// the agent can ask to see one, and the user approves with their passkey or password (the reveal tool).
//
// The settings are user-only (manifest config): the user changes them in the Rubi panel with approval,
// and the agent has no way to change them.

type privacyConfig struct {
	HideCodes  bool     `json:"hide_codes"`
	HideResets bool     `json:"hide_password_resets"`
	HideAlerts bool     `json:"hide_sign_in_alerts"`
	Senders    []string `json:"hidden_senders"`
	Keywords   []string `json:"hidden_keywords"`
}

// Built-in phrases by category, matched in lower case. Each category has its own switch, so the user can,
// for example, hide codes but let the agent see sign-in alerts and warn them about suspicious activity.
var (
	codePhrases = []string{
		"verification code", "security code", "confirmation code", "login code", "log-in code", "sign-in code",
		"sign in code", "authentication code", "access code", "one-time code", "one-time password",
		"one time password", "one-time passcode", "passcode", "otp", "2fa", "two-factor", "two-step",
		"magic link", "verify your email", "confirm your email", "verify your identity", "login link", "log-in link",
		"sign-in link", "sign in link", "your code", "security key", "recovery code", "backup code", "verify it's you",
		"ваш код", "код безопасности", "ссылка для входа", "резервный код",
		"код подтверждения", "код для входа", "код входа", "проверочный код", "одноразовый код",
		"одноразовый пароль", "код доступа", "код авторизации", "подтверждение входа", "подтвердите адрес",
		"подтвердите почту",
	}
	resetPhrases = []string{
		"password reset", "reset your password", "reset password", "change your password", "password change",
		"сброс пароля", "восстановление пароля", "смена пароля", "изменение пароля", "сбросить пароль",
	}
	alertPhrases = []string{
		"new sign-in", "new sign in", "new login", "new device", "sign in to your account", "suspicious",
		"unusual activity", "unusual sign-in", "security alert", "was your account accessed",
		"новый вход", "вход в аккаунт", "новое устройство", "подозрительн", "необычная активность",
		"оповещение безопасности",
	}
)

var (
	codeWord   = regexp.MustCompile(`(?i)(^|[^\p{L}])(code|codes|pin|код|кода|пин)([^\p{L}]|$)`)
	codeDigits = regexp.MustCompile(`(^|[^\d])\d{4,8}([^\d]|$)`)
	addrRe     = regexp.MustCompile(`[^\s<>"',;]+@[^\s<>"',;]+`)
)

// hidden reports whether a message must stay hidden from the agent, and why (for the user's eyes only).
// body may be empty when only headers are known.
func (p privacyConfig) hidden(from, subject, body string) (bool, string) {
	fromL := strings.ToLower(from)
	for _, s := range p.Senders {
		if senderMatches(fromL, s) {
			return true, "hidden sender " + strings.TrimSpace(s)
		}
	}
	subj := strings.ToLower(subject)
	bodyL := strings.ToLower(body)
	if len(bodyL) > 20000 {
		bodyL = bodyL[:20000]
	}
	for _, k := range p.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && (strings.Contains(subj, k) || strings.Contains(bodyL, k)) {
			return true, "hidden word " + k
		}
	}
	if p.HideCodes {
		if anyPhrase(subj, bodyL, codePhrases) {
			return true, "sign-in code or confirmation link"
		}
		// "123456 is your code", "Ваш код: 4821", in the subject or the opening lines of the body (where
		// such mail puts the code under an innocent subject, "Acme" / "Your code: 482193").
		head := bodyL
		if r := []rune(head); len(r) > 300 {
			head = string(r[:300])
		}
		for _, t := range []string{subj, head} {
			if codeWord.MatchString(t) && codeDigits.MatchString(t) {
				return true, "sign-in code or confirmation link"
			}
		}
	}
	if p.HideResets && anyPhrase(subj, bodyL, resetPhrases) {
		return true, "password reset"
	}
	if p.HideAlerts && anyPhrase(subj, bodyL, alertPhrases) {
		return true, "sign-in alert"
	}
	return false, ""
}

// any reports whether the filter can hide anything at all.
func (p privacyConfig) any() bool {
	return p.HideCodes || p.HideResets || p.HideAlerts || len(p.Senders) > 0 || len(p.Keywords) > 0
}

func anyPhrase(subj, body string, phrases []string) bool {
	for _, k := range phrases {
		if containsPhrase(subj, k) || containsPhrase(body, k) {
			return true
		}
	}
	return false
}

// containsPhrase matches short tokens ("otp", "2fa") as whole words, longer phrases anywhere.
func containsPhrase(text, phrase string) bool {
	if len([]rune(phrase)) > 4 {
		return strings.Contains(text, phrase)
	}
	for i := 0; ; {
		j := strings.Index(text[i:], phrase)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(phrase)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if !wordRune(before) && !wordRune(after) {
			return true
		}
		i = start + 1
	}
}

func wordRune(r rune) bool { return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// senderMatches checks an address ("a@b.com") or a domain ("b.com", "@b.com") against a From header.
func senderMatches(fromLower, entry string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	if e == "" {
		return false
	}
	for _, addr := range addrRe.FindAllString(fromLower, -1) {
		if strings.Contains(e, "@") && !strings.HasPrefix(e, "@") {
			if addr == e {
				return true
			}
			continue
		}
		domain := strings.TrimPrefix(e, "@")
		at := strings.LastIndex(addr, "@")
		host := addr[at+1:]
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// senderOnly is what the agent may see of a hidden message's sender: the address, without a display name
// (names like "Your code is 1234" have been seen in the wild).
func senderOnly(from string) string {
	if a := addrRe.FindString(from); a != "" {
		return strings.Trim(a, "<>")
	}
	return "(hidden)"
}

func privacyOf(h host) privacyConfig {
	p := privacyConfig{HideCodes: true, HideResets: true}
	if err := h.Config(&p); err != nil {
		h.Logf("privacy settings unavailable, hiding codes, resets and alerts: %v", err)
		return privacyConfig{HideCodes: true, HideResets: true, HideAlerts: true}
	}
	return p
}

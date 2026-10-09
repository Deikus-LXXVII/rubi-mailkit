# rubi-mailkit

The shared engine of the [Rubi](https://github.com/Deikus-LXXVII/rubi) mail plugins:
[iCloud Mail](https://github.com/Deikus-LXXVII/rubi-icloud-mail) and
[Gmail](https://github.com/Deikus-LXXVII/rubi-gmail). Both sign in with an app password over IMAP and
SMTP, and behave the same way:

- read, search and draft freely (by default); never marks mail as read;
- send only after the user's approval, optionally watching for replies;
- watches: wake the agent for new mail from given senders or with given words;
- a privacy filter (sign-in codes, password resets, chosen senders and words) the agent can't see past
  without the user's passkey or password;
- folder access: all folders or only some, and whether the agent may ask for the others;
- the formatted version on request (`read` with `format: "html"`): the links with their text and a copy of
  the email that loads nothing from the internet (no tracking images), for buttons like "Unsubscribe"
  that plain text loses; mailing lists also show the sender's own unsubscribe address (List-Unsubscribe);
- attachments: the agent may open them freely, may ask (the user approves each), or can't open them at
  all and sees only how many there are; attachments of private mail always need approval. An opened
  attachment is a file in a private folder, deleted after 30 minutes;
- several accounts: every tool takes an optional `account` (the address), the default one otherwise.
  Folder access is set per account; the privacy filter applies to all of them.

A plugin is a `Provider` and a one-line `main`:

```go
func main() { mailkit.Main(provider) }
```

This module is a library; it isn't installed on its own. MIT licensed.

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
- search in Gmail's syntax (`from:anna is:unread larger:5M newer_than:7d "exact phrase" -label:work`,
  `OR`): Gmail runs the query itself (X-GM-RAW), so every Gmail operator works there; other servers get
  the common operators translated, across every folder except Trash and Spam. Results come grouped into
  threads, and `thread` reads a whole conversation (from the inbox, Sent and the archive) at once;
- organizing: archive, trash, spam, move, Gmail labels, read and star marks, creating, renaming and
  deleting folders or labels. Everything but deleting a folder can be undone for 7 days; deleting a
  folder always asks the user, and on servers where that deletes mail only an empty folder can go.
  Mail hidden by the privacy filter is never touched;
- forwarding with the original's attachments, attachments when sending (base64 from the agent, or
  attachments of other emails; the plugin never reads a file path the agent names), a formatted (HTML)
  version, and `read` with `format: "raw"` for the source (every header, the MIME structure; attachment
  contents stay behind the attachment setting). Attachments the agent can't open only go out when the
  user reviews the email or it is a draft;
- drafts: list, edit (the old version goes to the trash), delete, and send a saved draft (also one the
  user wrote) after approval;
- several accounts: every tool takes an optional `account` (the address), the default one otherwise.
  Folder access is set per account; the privacy filter applies to all of them.

A plugin is a `Provider` and a one-line `main`:

```go
func main() { mailkit.Main(provider) }
```

This module is a library; it isn't installed on its own. MIT licensed.

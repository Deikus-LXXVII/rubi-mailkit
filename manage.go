package mailkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Organizing the mailbox: archive, trash, spam, move to a folder, Gmail labels, read and star marks, and
// creating, renaming and deleting folders and labels. Everything except deleting a folder can be undone
// for 7 days with the undo id the result carries. Mail hidden by the user's privacy filter is never
// touched (a planted instruction can't make the agent bin the user's security alerts), and mail is only
// taken out of folders the agent may open.

type manageIn struct {
	UID     uint32   `json:"uid,omitempty" jsonschema:"one message"`
	UIDs    []uint32 `json:"uids,omitempty" jsonschema:"several messages from the same folder (at most 50)"`
	Mailbox string   `json:"mailbox,omitempty" jsonschema:"their folder, default INBOX"`
	Account string   `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

type moveIn struct {
	manageIn
	To string `json:"to" jsonschema:"archive, trash, spam, inbox, or a folder (on Gmail: a label; the message leaves its current folder)"`
}

type labelIn struct {
	manageIn
	Add    []string `json:"add,omitempty" jsonschema:"labels to add"`
	Remove []string `json:"remove,omitempty" jsonschema:"labels to remove (removing INBOX archives)"`
}

type markIn struct {
	manageIn
	As string `json:"as" jsonschema:"read, unread, starred or unstarred"`
}

type undoIn struct {
	UndoID  string `json:"undo_id"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

type folderOpIn struct {
	Op      string `json:"op" jsonschema:"create, rename or delete"`
	Name    string `json:"name" jsonschema:"the folder or label; nested ones with / (Work/Clients)"`
	NewName string `json:"new_name,omitempty" jsonschema:"for rename"`
	Account string `json:"account,omitempty" jsonschema:"which connected mailbox (its address); default: the default one"`
}

// manageOp is one organizing action, as approved and as run.
type manageOp struct {
	Op      string    `json:"op"` // move | label | mark | undo | folder
	Mailbox string    `json:"mailbox,omitempty"`
	UIDs    []uint32  `json:"uids,omitempty"`
	To      string    `json:"to,omitempty"`
	Add     []string  `json:"add,omitempty"`
	Remove  []string  `json:"remove,omitempty"`
	As      string    `json:"as,omitempty"`
	UndoID  string    `json:"undo_id,omitempty"`
	Folder  string    `json:"folder_op,omitempty"`
	Name    string    `json:"name,omitempty"`
	NewName string    `json:"new_name,omitempty"`
	Until   time.Time `json:"until,omitzero"`
}

// undoRec says how to reverse one action.
type undoRec struct {
	ID       string     `json:"id"`
	Created  time.Time  `json:"created"`
	Op       string     `json:"op"` // move | copy | label | mark | folder_create | folder_rename
	From     string     `json:"from,omitempty"`
	To       string     `json:"to,omitempty"`
	IDs      []string   `json:"ids,omitempty"`       // Message-IDs
	DestUIDs []uint32   `json:"dest_uids,omitempty"` // where moved messages landed, when the server said
	UIDs     []uint32   `json:"uids,omitempty"`
	As       string     `json:"as,omitempty"`
	Added    []string   `json:"added,omitempty"`
	Removed  []string   `json:"removed,omitempty"`
	Used     bool       `json:"used,omitempty"`
	Parts    []*undoRec `json:"parts,omitempty"` // a bulk action: several folders
}

const (
	maxManage = 50
	undoTTL   = 7 * 24 * time.Hour
)

func (in manageIn) all() []uint32 {
	return revealIn{UID: in.UID, UIDs: in.UIDs}.all()
}

func registerManage(p *rubiplugin.Plugin, x *integration) {
	addTool(p, "move", "Move messages: to archive, trash, spam, inbox or a folder. Undo with "+tool("undo")+" and the undo_id it returns (7 days). Messages hidden by the user's privacy filter are skipped.",
		func(in moveIn) string { return in.Account },
		func(ctx context.Context, a *account, in moveIn) (any, error) {
			if strings.TrimSpace(in.To) == "" {
				return nil, errors.New("say where to move them (to)")
			}
			return x.manage(ctx, a, manageOp{Op: "move", Mailbox: in.Mailbox, UIDs: in.all(), To: in.To})
		})
	addTool(p, "mark", "Mark messages as read, unread, starred or unstarred. Undo with "+tool("undo")+".",
		func(in markIn) string { return in.Account },
		func(ctx context.Context, a *account, in markIn) (any, error) {
			return x.manage(ctx, a, manageOp{Op: "mark", Mailbox: in.Mailbox, UIDs: in.all(), As: strings.ToLower(strings.TrimSpace(in.As))})
		})
	if prov.Gmail {
		addTool(p, "label", "Add or remove Gmail labels (a message can have several). Removing INBOX archives. The labels must exist: create one with "+tool("manage_folder")+". Undo with "+tool("undo")+".",
			func(in labelIn) string { return in.Account },
			func(ctx context.Context, a *account, in labelIn) (any, error) {
				return x.manage(ctx, a, manageOp{Op: "label", Mailbox: in.Mailbox, UIDs: in.all(), Add: in.Add, Remove: in.Remove})
			})
	}
	addTool(p, "undo", "Undo a move, label change, mark, or folder created or renamed, by the undo_id its result gave (within 7 days).",
		func(in undoIn) string { return in.Account },
		func(ctx context.Context, a *account, in undoIn) (any, error) {
			return x.manage(ctx, a, manageOp{Op: "undo", UndoID: strings.TrimSpace(in.UndoID)})
		})
	what := "folder"
	if prov.Gmail {
		what = "label"
	}
	addTool(p, "manage_folder", "Create, rename or delete a "+what+". Deleting asks the user first"+
		map[bool]string{true: " (the mail keeps its other labels and stays in All Mail).", false: " and works only on an empty folder."}[prov.Gmail],
		func(in folderOpIn) string { return in.Account },
		func(ctx context.Context, a *account, in folderOpIn) (any, error) {
			return x.manage(ctx, a, manageOp{Op: "folder", Folder: strings.ToLower(strings.TrimSpace(in.Op)),
				Name: strings.TrimSpace(in.Name), NewName: strings.TrimSpace(in.NewName)})
		})
	for _, kind := range []string{kindOrganize, kindFolders, kindFolderDelete} {
		onExecute(p, kind, func(ctx context.Context, a *account, _ string, payload json.RawMessage) (any, error) {
			var op manageOp
			if err := json.Unmarshal(payload, &op); err != nil {
				return nil, err
			}
			return x.doManage(a, op)
		})
	}
}

func (op manageOp) kind() string {
	switch {
	case op.Op == "folder" && op.Folder == "delete":
		return kindFolderDelete
	case op.Op == "folder":
		return kindFolders
	}
	return kindOrganize
}

// manage checks an action and runs it, or asks the user first if they require that.
func (x *integration) manage(ctx context.Context, h host, op manageOp) (any, error) {
	switch op.Op {
	case "move", "label", "mark", "snooze":
		if len(op.UIDs) == 0 {
			return nil, errNoUIDs
		}
		if len(op.UIDs) > maxManage {
			return nil, fmt.Errorf("at most %d messages at once", maxManage)
		}
	case "undo":
		if op.UndoID == "" {
			return nil, errors.New("give the undo_id")
		}
	case "folder":
		if err := checkFolderName(op.Name); err != nil {
			return nil, err
		}
		switch op.Folder {
		case "create", "delete":
		case "rename":
			if err := checkFolderName(op.NewName); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("op is create, rename or delete")
		}
	}
	if op.Op == "mark" && !map[string]bool{"read": true, "unread": true, "starred": true, "unstarred": true}[op.As] {
		return nil, errors.New("as is read, unread, starred or unstarred")
	}
	if op.Op == "label" && len(op.Add)+len(op.Remove) == 0 {
		return nil, errors.New("give labels to add or remove")
	}
	kind := op.kind()
	if h.Level(kind) == rubiplugin.None {
		return x.doManage(h, op)
	}
	summary, pv, err := x.describeManage(h, op)
	if err != nil {
		return nil, err
	}
	return h.Submit(ctx, rubiplugin.Request{Kind: kind, Summary: summary, Preview: pv, Payload: op,
		Options: []rubiplugin.Option{{Key: "allow", Label: "Allow"}}})
}

func checkFolderName(n string) error {
	if n == "" {
		return errors.New("give the folder name")
	}
	if len(n) > 200 || strings.IndexFunc(n, func(r rune) bool { return unicode.IsControl(r) || r == '*' || r == '%' }) >= 0 {
		return errors.New("that folder name isn't allowed")
	}
	return nil
}

// describeManage writes what the user approves.
func (x *integration) describeManage(h host, op manageOp) (string, map[string]any, error) {
	pv := map[string]any{}
	switch op.Op {
	case "folder":
		switch op.Folder {
		case "create":
			return "Create the folder \"" + op.Name + "\"", map[string]any{"folder": op.Name}, nil
		case "rename":
			return "Rename the folder \"" + op.Name + "\" to \"" + op.NewName + "\"", map[string]any{"folder": op.Name, "new_name": op.NewName}, nil
		}
		pv["folder"] = op.Name
		if prov.Gmail {
			pv["mail"] = "The mail keeps its other labels and stays in All Mail. This can't be undone."
		} else {
			pv["mail"] = "Only an empty folder can be deleted. This can't be undone."
		}
		return "Delete the folder \"" + op.Name + "\"", pv, nil
	case "undo":
		return "Undo an earlier change to your mail", map[string]any{"undo_id": op.UndoID}, nil
	}
	c, s, err := session(h)
	if err != nil {
		return "", nil, err
	}
	defer logout(c)
	sp, err := findSpecials(c, s)
	if err != nil {
		return "", nil, err
	}
	box := sp.resolve(op.Mailbox)
	if err := x.folderAllowed(h, box); err != nil {
		return "", nil, err
	}
	targets, private, _, err := x.targets(h, c, box, op.UIDs)
	if err != nil {
		return "", nil, err
	}
	var lines []string
	for i, t := range targets {
		if i == 10 {
			lines = append(lines, fmt.Sprintf("and %d more", len(targets)-10))
			break
		}
		lines = append(lines, senderOnly(t.from)+": "+t.subject)
	}
	pv["messages"] = strings.Join(lines, "\n")
	pv["folder"] = box
	if len(private) > 0 {
		pv["skipped"] = fmt.Sprintf("%d private (left as they are)", len(private))
	}
	n := fmt.Sprintf("%d emails", len(targets))
	if len(targets) == 1 {
		n = "1 email"
	}
	switch op.Op {
	case "move":
		return "Move " + n + " to " + sp.resolve(op.To), pv, nil
	case "label":
		pv["add"], pv["remove"] = strings.Join(op.Add, ", "), strings.Join(op.Remove, ", ")
		return "Change the labels of " + n, pv, nil
	case "snooze":
		return "Snooze " + n + " until " + op.Until.Local().Format("Mon 2 Jan 15:04"), pv, nil
	}
	return "Mark " + n + " as " + op.As, pv, nil
}

type target struct {
	uid           imap.UID
	id            string
	from, subject string
	seen, starred bool
}

// targets fetches the messages an action is about, leaving out what the privacy filter hides.
func (x *integration) targets(h host, c *imapclient.Client, box string, uids []uint32) (ok []target, private, missing []uint32, err error) {
	if _, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, nil, nil, fmt.Errorf("can't open folder %q: %w", box, err)
	}
	want := make([]imap.UID, len(uids))
	for i, u := range uids {
		want[i] = imap.UID(u)
	}
	sums, err := fetchSummaries(c, box, want, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	got := map[uint32]bool{}
	p := privacyOf(h)
	for _, m := range sums {
		got[m.UID] = true
		hidden, _ := p.hidden(m.From, m.Subject, "")
		if !hidden && p.any() {
			hidden = true // unless the whole message shows it isn't private
			if raw, err := fetchRaw(c, box, m.UID); err == nil {
				if full, err := parseMessage(raw, 0); err == nil {
					hidden, _ = p.hidden(m.From, m.Subject, full.Text)
				}
			}
		}
		if hidden {
			private = append(private, m.UID)
			continue
		}
		ok = append(ok, target{uid: imap.UID(m.UID), id: m.info.messageID, from: m.From, subject: m.Subject, seen: m.Seen, starred: m.Starred})
	}
	for _, u := range uids {
		if !got[u] {
			missing = append(missing, u)
		}
	}
	return ok, private, missing, nil
}

func (x *integration) doManage(h host, op manageOp) (any, error) {
	c, s, err := session(h)
	if err != nil {
		return nil, err
	}
	defer logout(c)
	sp, err := findSpecials(c, s)
	if err != nil {
		return nil, err
	}
	switch op.Op {
	case "undo":
		return x.undo(h, c, sp, op.UndoID)
	case "folder":
		return x.folderOp(h, c, sp, op)
	}
	box := sp.resolve(op.Mailbox)
	if err := x.folderAllowed(h, box); err != nil {
		return nil, err
	}
	targets, private, missing, err := x.targets(h, c, box, op.UIDs)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"mailbox": box}
	if len(private) > 0 {
		out["skipped_private"] = private
		out["private_note"] = "Hidden by the user's privacy filter, so left as they are."
	}
	if len(missing) > 0 {
		out["not_found"] = missing
	}
	if len(targets) == 0 {
		out["status"] = "nothing_changed"
		return out, nil
	}
	var rec *undoRec
	switch op.Op {
	case "move":
		rec, err = x.moveTargets(c, sp, box, targets, sp.resolve(op.To))
	case "mark":
		rec, err = markTargets(c, box, targets, op.As)
	case "label":
		rec, err = x.labelTargets(c, sp, box, targets, op.Add, op.Remove)
	case "snooze":
		rec, err = x.snooze(h, c, sp, box, targets, op.Until)
		out["until"] = op.Until.Format(time.RFC3339)
	default:
		return nil, fmt.Errorf("unknown action %q", op.Op)
	}
	if err != nil {
		return nil, err
	}
	out["status"], out["changed"] = "done", len(targets)
	if rec != nil {
		if err := x.saveUndo(h, rec); err == nil {
			out["undo_id"] = rec.ID
		}
	}
	h.Audit("mail_"+op.Op, map[string]any{"mailbox": box, "count": len(targets), "to": op.To, "as": op.As})
	return out, nil
}

func uidSet(ts []target) imap.UIDSet {
	var set imap.UIDSet
	for _, t := range ts {
		set.AddNum(t.uid)
	}
	return set
}

func idsOf(ts []target) []string {
	var ids []string
	for _, t := range ts {
		if t.id != "" {
			ids = append(ids, t.id)
		}
	}
	return ids
}

// moveTargets moves messages from box to dest. On Gmail, taking a message out of All Mail would bin it,
// so from All Mail (other than to trash or spam) it is copied: Gmail adds the label.
func (x *integration) moveTargets(c *imapclient.Client, sp specials, box string, ts []target, dest string) (*undoRec, error) {
	if dest == box || strings.EqualFold(dest, "INBOX") && strings.EqualFold(box, "INBOX") {
		return nil, errors.New("they are already in " + box)
	}
	if !sp.has(dest) {
		return nil, fmt.Errorf("there is no folder %q; create it with %s or use list_mailboxes", dest, tool("manage_folder"))
	}
	if _, err := c.Select(box, nil).Wait(); err != nil {
		return nil, err
	}
	rec := &undoRec{Op: "move", From: box, To: dest, IDs: idsOf(ts)}
	if prov.Gmail && box == sp.All && dest != sp.Trash && dest != sp.Junk {
		data, err := c.Copy(uidSet(ts), dest).Wait()
		if err != nil {
			return nil, fmt.Errorf("couldn't move them: %w", err)
		}
		rec.Op = "copy"
		rec.DestUIDs = nums(data.DestUIDs)
		return rec, nil
	}
	data, err := c.Move(uidSet(ts), dest).Wait()
	if err != nil {
		return nil, fmt.Errorf("couldn't move them: %w", err)
	}
	if set, ok := data.DestUIDs.(imap.UIDSet); ok {
		rec.DestUIDs = nums(set)
	}
	return rec, nil
}

func nums(set imap.UIDSet) []uint32 {
	list, _ := set.Nums()
	out := make([]uint32, len(list))
	for i, u := range list {
		out[i] = uint32(u)
	}
	return out
}

func markTargets(c *imapclient.Client, box string, ts []target, as string) (*undoRec, error) {
	flag, add := imap.FlagSeen, true
	switch as {
	case "unread":
		add = false
	case "starred":
		flag = imap.FlagFlagged
	case "unstarred":
		flag, add = imap.FlagFlagged, false
	}
	var changed []target
	for _, t := range ts {
		has := t.seen
		if flag == imap.FlagFlagged {
			has = t.starred
		}
		if has != add {
			changed = append(changed, t)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if err := setFlag(c, box, uidSet(changed), flag, add); err != nil {
		return nil, err
	}
	rec := &undoRec{Op: "mark", From: box, As: as}
	for _, t := range changed {
		rec.UIDs = append(rec.UIDs, uint32(t.uid))
	}
	return rec, nil
}

func setFlag(c *imapclient.Client, box string, set imap.UIDSet, flag imap.Flag, add bool) error {
	if _, err := c.Select(box, nil).Wait(); err != nil {
		return err
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	return c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{flag}}, nil).Close()
}

// labelTargets adds and removes Gmail labels. A label is a folder over IMAP: copying into it adds the
// label, deleting from it removes the label (the message stays in All Mail).
func (x *integration) labelTargets(c *imapclient.Client, sp specials, box string, ts []target, add, remove []string) (*undoRec, error) {
	rec := &undoRec{Op: "label", From: box, IDs: idsOf(ts)}
	for _, l := range add {
		dest := sp.resolve(l)
		if dest == sp.Trash || dest == sp.Junk || dest == sp.All {
			return nil, errors.New("use " + tool("move") + " for trash, spam and archive")
		}
		if !sp.has(dest) {
			return nil, fmt.Errorf("there is no label %q; create it with %s", l, tool("manage_folder"))
		}
		if dest == box {
			continue
		}
		if _, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return nil, err
		}
		if _, err := c.Copy(uidSet(ts), dest).Wait(); err != nil {
			return nil, fmt.Errorf("couldn't add the label %q: %w", l, err)
		}
		rec.Added = append(rec.Added, dest)
	}
	for _, l := range remove {
		from := sp.resolve(l)
		if from == sp.Trash || from == sp.Junk || from == sp.All || from == sp.Sent || from == sp.Drafts {
			return nil, errors.New("that can't be removed as a label; use " + tool("move"))
		}
		if len(rec.IDs) == 0 {
			return nil, errors.New("these messages have no Message-ID, so their labels can't be removed here")
		}
		if err := removeByID(c, from, rec.IDs); err != nil {
			return nil, fmt.Errorf("couldn't remove the label %q: %w", l, err)
		}
		rec.Removed = append(rec.Removed, from)
	}
	return rec, nil
}

// findByID returns the UIDs in box of the messages with these Message-IDs.
func findByID(c *imapclient.Client, box string, ids []string, readOnly bool) (imap.UIDSet, error) {
	var opts *imap.SelectOptions
	if readOnly {
		opts = &imap.SelectOptions{ReadOnly: true}
	}
	if _, err := c.Select(box, opts).Wait(); err != nil {
		return nil, err
	}
	var set imap.UIDSet
	for _, id := range ids {
		res, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}}}, nil).Wait()
		if err != nil {
			return nil, err
		}
		for _, u := range res.AllUIDs() {
			set.AddNum(u)
		}
	}
	return set, nil
}

// removeByID deletes the copies in box of the messages with these Message-IDs (on Gmail: removes the label).
func removeByID(c *imapclient.Client, box string, ids []string) error {
	set, err := findByID(c, box, ids, false)
	if err != nil || len(set) == 0 {
		return err
	}
	if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		return err
	}
	return c.UIDExpunge(set).Close()
}

// ---- undo ----

func (x *integration) saveUndo(h host, rec *undoRec) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	rec.ID, rec.Created = randomID("undo_"), now
	var kept []*undoRec
	for _, u := range st.Undo {
		if now.Sub(u.Created) < undoTTL && !u.Used {
			kept = append(kept, u)
		}
	}
	if len(kept) >= 50 {
		kept = kept[len(kept)-49:]
	}
	st.Undo = append(kept, rec)
	return h.SaveState(st)
}

func (x *integration) takeUndo(h host, id string) (*undoRec, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(h)
	if err != nil {
		return nil, err
	}
	for _, u := range st.Undo {
		if u.ID == id {
			if u.Used {
				return nil, errors.New("that was already undone")
			}
			if time.Since(u.Created) > undoTTL {
				return nil, errors.New("that is too old to undo (7 days)")
			}
			u.Used = true
			return u, h.SaveState(st)
		}
	}
	return nil, errors.New("no such undo_id for this mailbox")
}

func (x *integration) undo(h host, c *imapclient.Client, sp specials, id string) (any, error) {
	rec, err := x.takeUndo(h, id)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"status": "undone"}
	parts := []*undoRec{rec}
	if rec.Op == "multi" {
		parts = rec.Parts
	}
	var failed []string
	for _, p := range parts {
		if err := undoOne(c, sp, p, out); err != nil {
			if len(parts) == 1 {
				return nil, err
			}
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		out["status"], out["failed"] = "partly_undone", failed
	}
	h.Audit("mail_undo", map[string]any{"op": rec.Op})
	return out, nil
}

func undoOne(c *imapclient.Client, sp specials, rec *undoRec, out map[string]any) error {
	var err error
	switch rec.Op {
	case "move", "copy":
		set := imap.UIDSet{}
		if len(rec.DestUIDs) > 0 {
			if _, err := c.Select(rec.To, nil).Wait(); err != nil {
				return err
			}
			for _, u := range rec.DestUIDs {
				set.AddNum(imap.UID(u))
			}
		} else if set, err = findByID(c, rec.To, rec.IDs, false); err != nil {
			return err
		}
		if len(set) == 0 {
			return errors.New("the messages aren't in " + rec.To + " any more")
		}
		switch {
		case rec.Op == "copy": // Gmail: the label was added; remove it again
			err = c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
			if err == nil {
				err = c.UIDExpunge(set).Close()
			}
		case prov.Gmail && rec.To == sp.All: // archived: copying back to the inbox adds the label again
			_, err = c.Copy(set, rec.From).Wait()
		default:
			_, err = c.Move(set, rec.From).Wait()
		}
		if err != nil {
			return fmt.Errorf("couldn't undo: %w", err)
		}
		out["mailbox"] = rec.From
	case "mark":
		opposite := map[string]string{"read": "unread", "unread": "read", "starred": "unstarred", "unstarred": "starred"}[rec.As]
		var ts []target
		for _, u := range rec.UIDs {
			ts = append(ts, target{uid: imap.UID(u), seen: opposite == "unread", starred: opposite == "unstarred"})
		}
		if _, err := markTargets(c, rec.From, ts, opposite); err != nil {
			return err
		}
	case "label":
		for _, l := range rec.Added {
			if err := removeByID(c, l, rec.IDs); err != nil {
				return fmt.Errorf("couldn't undo: %w", err)
			}
		}
		for _, l := range rec.Removed {
			src := sp.All
			if src == "" {
				src = rec.From
			}
			set, err := findByID(c, src, rec.IDs, true)
			if err != nil {
				return err
			}
			if len(set) > 0 {
				if _, err := c.Copy(set, l).Wait(); err != nil {
					return fmt.Errorf("couldn't undo: %w", err)
				}
			}
		}
	case "folder_create":
		if err := deleteEmptyFolder(c, rec.To); err != nil {
			return err
		}
	case "folder_rename":
		if err := c.Rename(rec.To, rec.From, nil).Wait(); err != nil {
			return fmt.Errorf("couldn't rename it back: %w", err)
		}
	}
	return nil
}

// ---- folders ----

func (x *integration) folderOp(h host, c *imapclient.Client, sp specials, op manageOp) (any, error) {
	name := op.Name
	switch op.Folder {
	case "create":
		if sp.has(name) || sp.resolve(name) != name {
			return nil, fmt.Errorf("%q already exists", name)
		}
		if err := c.Create(name, nil).Wait(); err != nil {
			return nil, fmt.Errorf("couldn't create it: %w", err)
		}
		h.Audit("folder_created", map[string]any{"folder": name})
		out := map[string]any{"status": "created", "folder": name}
		if rec := (&undoRec{Op: "folder_create", To: name}); x.saveUndo(h, rec) == nil {
			out["undo_id"] = rec.ID
		}
		if x.folderAllowed(h, name) != nil {
			out["note"] = "The user lets you open only some folders, so you can move mail into this one but not read it."
		}
		return out, nil
	case "rename", "delete":
		name = sp.resolve(name)
		if !sp.has(name) {
			return nil, fmt.Errorf("there is no folder %q", op.Name)
		}
		if sp.special(name) {
			return nil, fmt.Errorf("%q is one of the mailbox's own folders and can't be changed", name)
		}
		if err := x.folderAllowed(h, name); err != nil {
			return nil, err
		}
	}
	if op.Folder == "rename" {
		if sp.has(op.NewName) {
			return nil, fmt.Errorf("%q already exists", op.NewName)
		}
		if err := c.Rename(name, op.NewName, nil).Wait(); err != nil {
			return nil, fmt.Errorf("couldn't rename it: %w", err)
		}
		h.Audit("folder_renamed", map[string]any{"folder": name, "new_name": op.NewName})
		out := map[string]any{"status": "renamed", "folder": op.NewName}
		if rec := (&undoRec{Op: "folder_rename", From: name, To: op.NewName}); x.saveUndo(h, rec) == nil {
			out["undo_id"] = rec.ID
		}
		return out, nil
	}
	if prov.Gmail {
		if err := c.Delete(name).Wait(); err != nil {
			return nil, fmt.Errorf("couldn't delete it: %w", err)
		}
	} else if err := deleteEmptyFolder(c, name); err != nil {
		return nil, err
	}
	h.Audit("folder_deleted", map[string]any{"folder": name})
	return map[string]any{"status": "deleted", "folder": name}, nil
}

// deleteEmptyFolder deletes a folder only if no mail is in it: on most servers deleting a folder deletes
// its mail for good.
func deleteEmptyFolder(c *imapclient.Client, name string) error {
	st, err := c.Status(name, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		return fmt.Errorf("can't check %q: %w", name, err)
	}
	if st.NumMessages != nil && *st.NumMessages > 0 {
		return fmt.Errorf("%q has %d emails; move them out first (deleting a folder deletes its mail)", name, *st.NumMessages)
	}
	if err := c.Delete(name).Wait(); err != nil {
		return fmt.Errorf("couldn't delete it: %w", err)
	}
	return nil
}

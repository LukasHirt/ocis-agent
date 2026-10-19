package storetest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() {
	register("Conversations", runConversations)
	register("Messages", runMessages)
	register("ChatCounts", runChatCounts)
}

func addConv(t *testing.T, st store.Store, user, title string) store.Conversation {
	t.Helper()
	var c store.Conversation
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		c, err = tx.InsertConversation(store.Conversation{UserID: user, Title: title})
		return err
	})
	return c
}

func addMsg(t *testing.T, st store.Store, user, conv, role, content, job string) store.Message {
	t.Helper()
	var m store.Message
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		m, err = tx.InsertMessage(store.Message{UserID: user, ConversationID: conv, Role: role, Content: content, JobID: job})
		return err
	})
	return m
}

func viewErr(st store.Store, fn func(tx store.Tx) error) error { return st.View(bg(), fn) }

func txErr(st store.Store, fn func(tx store.Tx) error) error { return st.InTx(bg(), fn) }

func convIDs(cs []store.Conversation) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func msgIDs(ms []store.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func listConvs(t *testing.T, st store.Store, user string, f store.ConversationFilter) []store.Conversation {
	t.Helper()
	var out []store.Conversation
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListConversations(user, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func listMsgs(t *testing.T, st store.Store, user, conv string, f store.MessageFilter) []store.Message {
	t.Helper()
	var out []store.Message
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListMessages(user, conv, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func recentMsgs(t *testing.T, st store.Store, user, conv string, n int, exclude string) []store.Message {
	t.Helper()
	var out []store.Message
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListRecentMessages(user, conv, n, exclude)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func runConversations(t *testing.T, f Factory) {
	t.Run("insert, get, rename, scoped to the user", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "Plans")
		if c.ID == "" || c.UserID != "u1" || c.Title != "Plans" || !c.CreatedAt.Equal(clk.Now()) || !c.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("conversation = %+v", c)
		}
		// a given id is kept, an empty title is allowed
		mustTx(t, st, func(tx store.Tx) error {
			got, err := tx.InsertConversation(store.Conversation{ID: "fixed", UserID: "u1"})
			if err != nil || got.ID != "fixed" || got.Title != "" {
				return fmt.Errorf("fixed id: %+v %v", got, err)
			}
			return nil
		})
		if err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertConversation(store.Conversation{ID: "fixed", UserID: "u1"})
			return err
		}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate id: %v", err)
		}
		if err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertConversation(store.Conversation{UserID: "ghost"})
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: %v", err)
		}
		clk.Advance(time.Second)
		err := st.InTx(bg(), func(tx store.Tx) error {
			got, err := tx.GetConversation("u1", c.ID)
			if err != nil || got != c {
				return fmt.Errorf("get = %+v %v", got, err)
			}
			if _, err := tx.GetConversation("u2", c.ID); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("foreign get: %v", err)
			}
			if _, err := tx.GetConversation("u1", "nope"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown get: %v", err)
			}
			if err := tx.RenameConversation("u2", c.ID, "stolen"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("foreign rename: %v", err)
			}
			if err := tx.RenameConversation("u1", "nope", "x"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown rename: %v", err)
			}
			if err := tx.RenameConversation("u1", c.ID, "Renamed"); err != nil {
				return err
			}
			got, err = tx.GetConversation("u1", c.ID)
			if err != nil || got.Title != "Renamed" || !got.UpdatedAt.Equal(clk.Now()) || !got.CreatedAt.Equal(c.CreatedAt) {
				return fmt.Errorf("after rename = %+v %v", got, err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})

	t.Run("titles are validated", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		c := addConv(t, st, "u1", "ok")
		ok := []string{"", "Grüße 日本語", strings.Repeat("ä", 100)}
		bad := []string{strings.Repeat("ä", 101), "a\nb", "a\x00b", "a b", "a b", "a‮b", "a⁦b", "a‎b", "a\u0085b", "a\x7fb", "a\xffb"}
		for _, title := range ok {
			if err := txErr(st, func(tx store.Tx) error {
				if _, err := tx.InsertConversation(store.Conversation{UserID: "u1", Title: title}); err != nil {
					return err
				}
				return tx.RenameConversation("u1", c.ID, title)
			}); err != nil {
				t.Errorf("title %q: %v", title, err)
			}
		}
		for _, title := range bad {
			err := txErr(st, func(tx store.Tx) error {
				_, err := tx.InsertConversation(store.Conversation{UserID: "u1", Title: title})
				return err
			})
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("insert %q: %v", title, err)
			}
			err = txErr(st, func(tx store.Tx) error { return tx.RenameConversation("u1", c.ID, title) })
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("rename %q: %v", title, err)
			}
		}
		if err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertConversation(store.Conversation{Title: "x"})
			return err
		}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no user id: %v", err)
		}
	})

	t.Run("a user holds at most 200 conversations", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		mustTx(t, st, func(tx store.Tx) error {
			for i := range 200 {
				if _, err := tx.InsertConversation(store.Conversation{UserID: "u1", Title: fmt.Sprint(i)}); err != nil {
					return err
				}
			}
			return nil
		})
		err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertConversation(store.Conversation{UserID: "u1"})
			return err
		})
		if !errors.Is(err, store.ErrLimit) || errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrConflict) {
			t.Fatalf("201st: %v", err)
		}
		addConv(t, st, "u2", "other user is not limited")
		// deleting one makes room
		first := listConvs(t, st, "u1", store.ConversationFilter{Limit: 1})[0]
		mustTx(t, st, func(tx store.Tx) error { _, err := tx.DeleteConversation("u1", first.ID); return err })
		addConv(t, st, "u1", "again")
		if err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertConversation(store.Conversation{UserID: "u1"})
			return err
		}); !errors.Is(err, store.ErrLimit) {
			t.Errorf("full again: %v", err)
		}
	})

	t.Run("list is newest-updated first with a keyset cursor, ties by id", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		var all []store.Conversation
		for i := range 8 {
			all = append(all, addConv(t, st, "u1", fmt.Sprint(i)))
			if i%2 == 1 { // pairs share an updated_at
				clk.Advance(time.Second)
			}
		}
		addConv(t, st, "u2", "other")
		listed := convIDs(listConvs(t, st, "u1", store.ConversationFilter{}))
		if len(listed) != 8 {
			t.Fatalf("listed = %v", listed)
		}
		for i := 1; i < len(listed); i++ {
			if listed[i-1] <= listed[i] && listConvs(t, st, "u1", store.ConversationFilter{})[i-1].UpdatedAt.Equal(listConvs(t, st, "u1", store.ConversationFilter{})[i].UpdatedAt) {
				t.Errorf("tie not id DESC: %v", listed)
			}
		}
		for _, c := range all {
			pos := indexOf(listed, c.ID)
			got := convIDs(listConvs(t, st, "u1", store.ConversationFilter{Before: &store.PageKey{At: c.UpdatedAt, ID: c.ID}}))
			if !equal(got, listed[pos+1:]) {
				t.Errorf("before %s = %v, want %v", c.ID, got, listed[pos+1:])
			}
		}
		if got := listConvs(t, st, "u1", store.ConversationFilter{Limit: 3}); len(got) != 3 {
			t.Errorf("limit = %d", len(got))
		}
		if got := listConvs(t, st, "nobody", store.ConversationFilter{}); len(got) != 0 {
			t.Errorf("unknown user = %v", got)
		}
		for _, k := range []*store.PageKey{{At: all[0].UpdatedAt}, {ID: all[0].ID}} {
			if err := viewErr(st, func(tx store.Tx) error {
				_, err := tx.ListConversations("u1", store.ConversationFilter{Before: k})
				return err
			}); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("incomplete cursor %+v: %v", k, err)
			}
		}
		// a message bumps the conversation to the top
		clk.Advance(time.Minute)
		addMsg(t, st, "u1", all[0].ID, store.RoleUser, "hi", "")
		if top := listConvs(t, st, "u1", store.ConversationFilter{Limit: 1})[0]; top.ID != all[0].ID {
			t.Errorf("top = %s, want the bumped %s", top.ID, all[0].ID)
		}
	})

	t.Run("a multi-page walk sees every conversation once while rows change", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		for i := range 10 { // all on ONE updated_at
			addConv(t, st, "u1", fmt.Sprint(i))
		}
		want := convIDs(listConvs(t, st, "u1", store.ConversationFilter{}))
		var seen []string
		var before *store.PageKey
		for page := 0; ; page++ {
			list := listConvs(t, st, "u1", store.ConversationFilter{Limit: 3, Before: before})
			if len(list) == 0 {
				break
			}
			seen = append(seen, convIDs(list)...)
			last := list[len(list)-1]
			before = &store.PageKey{At: last.UpdatedAt, ID: last.ID}
			// the cursor row is renamed and a not yet seen row is deleted
			mustTx(t, st, func(tx store.Tx) error { return tx.RenameConversation("u1", list[0].ID, "r") })
			clk.Advance(time.Second)
			addConv(t, st, "u1", fmt.Sprintf("new-%d", page))
			if page > 10 {
				t.Fatal("the walk does not end")
			}
		}
		// renamed rows moved to the top (newer updated_at) and new ones arrived
		// above the cursor: every original row is still seen exactly once
		counts := map[string]int{}
		for _, id := range seen {
			counts[id]++
		}
		for _, id := range want {
			if counts[id] != 1 {
				t.Errorf("%s seen %d times in %v", id, counts[id], seen)
			}
		}
	})

	t.Run("delete removes the messages and returns the live chat jobs of the conversation", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "doomed")
		keep := addConv(t, st, "u1", "keep")
		chatJob := func(user, ref, kind string) store.Job {
			var j store.Job
			mustTx(t, st, func(tx store.Tx) error {
				var err error
				j, err = tx.InsertJob(store.Job{UserID: user, TriggerKind: kind, TriggerRef: ref, Goal: "g"})
				return err
			})
			return j
		}
		queued := chatJob("u1", c.ID, "chat")
		running := chatJob("u1", c.ID, "chat")
		done := chatJob("u1", c.ID, "chat")
		otherConv := chatJob("u1", keep.ID, "chat")
		otherKind := chatJob("u1", c.ID, "sweep")
		foreign := chatJob("u2", c.ID, "chat")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", done.ID) })
		_ = running
		addMsg(t, st, "u1", c.ID, store.RoleUser, "q", queued.ID)
		addMsg(t, st, "u1", keep.ID, store.RoleUser, "kept", otherConv.ID)

		if err := txErr(st, func(tx store.Tx) error { _, err := tx.DeleteConversation("u2", c.ID); return err }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("foreign delete: %v", err)
		}
		if err := txErr(st, func(tx store.Tx) error { _, err := tx.DeleteConversation("u1", "nope"); return err }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown delete: %v", err)
		}
		var ids []string
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			ids, err = tx.DeleteConversation("u1", c.ID)
			return err
		})
		want := []string{queued.ID, running.ID}
		if len(ids) != 2 || !(ids[0] == want[0] && ids[1] == want[1] || ids[0] == want[1] && ids[1] == want[0]) {
			t.Errorf("job ids = %v, want %v (not %s %s %s)", ids, want, done.ID, otherKind.ID, foreign.ID)
		}
		err := viewErr(st, func(tx store.Tx) error {
			if _, err := tx.GetConversation("u1", c.ID); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("still there: %v", err)
			}
			if _, err := tx.ListMessages("u1", c.ID, store.MessageFilter{}); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("messages of a deleted conversation: %v", err)
			}
			for _, id := range []string{queued.ID, otherConv.ID, done.ID} {
				if _, err := tx.GetJob("u1", id); err != nil {
					return fmt.Errorf("job %s deleted: %v", id, err)
				}
			}
			if n := len(mustMsgs(t, tx, "u1", keep.ID)); n != 1 {
				return fmt.Errorf("other conversation's messages = %d", n)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		// messages are gone from the table itself, not just hidden
		if err := txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertMessage(store.Message{UserID: "u1", ConversationID: c.ID, Role: store.RoleUser, Content: "x"})
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("insert into a deleted conversation: %v", err)
		}
	})

	t.Run("deleting a user removes their conversations and messages", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "a")
		addMsg(t, st, "u1", c.ID, store.RoleUser, "x", "")
		c2 := addConv(t, st, "u2", "b")
		addMsg(t, st, "u2", c2.ID, store.RoleUser, "y", "")
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("u1") })
		if got := listConvs(t, st, "u1", store.ConversationFilter{}); len(got) != 0 {
			t.Errorf("conversations = %v", got)
		}
		if err := viewErr(st, func(tx store.Tx) error {
			if n, err := tx.CountConversations("u1"); err != nil || n != 0 {
				return fmt.Errorf("count = %d %v", n, err)
			}
			if _, err := tx.ListMessages("u1", c.ID, store.MessageFilter{}); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("messages: %v", err)
			}
			if n := len(mustMsgs(t, tx, "u2", c2.ID)); n != 1 {
				return fmt.Errorf("u2 messages = %d", n)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
}

func mustMsgs(t *testing.T, tx store.Tx, user, conv string) []store.Message {
	t.Helper()
	ms, err := tx.ListMessages(user, conv, store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func runMessages(t *testing.T, f Factory) {
	t.Run("insert checks role, size, ownership and the job, and bumps the conversation", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "c")
		j1 := addJob(t, st, "u1", "g")
		j2 := addJob(t, st, "u2", "g")
		clk.Advance(time.Minute)
		m := addMsg(t, st, "u1", c.ID, store.RoleUser, "hello", j1.ID)
		if m.ID == "" || m.UserID != "u1" || m.ConversationID != c.ID || m.Role != "user" || m.Content != "hello" || m.JobID != j1.ID || !m.CreatedAt.Equal(clk.Now()) {
			t.Fatalf("message = %+v", m)
		}
		_ = viewErr(st, func(tx store.Tx) error {
			got, err := tx.GetConversation("u1", c.ID)
			if err != nil || !got.UpdatedAt.Equal(clk.Now()) || !got.CreatedAt.Before(got.UpdatedAt) {
				t.Errorf("conversation after message = %+v %v", got, err)
			}
			return nil
		})
		ins := func(m store.Message) error {
			return txErr(st, func(tx store.Tx) error { _, err := tx.InsertMessage(m); return err })
		}
		mk := func(mut func(*store.Message)) store.Message {
			m := store.Message{UserID: "u1", ConversationID: c.ID, Role: store.RoleAssistant, Content: "x"}
			mut(&m)
			return m
		}
		// validation
		for name, m := range map[string]store.Message{
			"empty content":      mk(func(m *store.Message) { m.Content = "" }),
			"bad role":           mk(func(m *store.Message) { m.Role = "tool" }),
			"empty role":         mk(func(m *store.Message) { m.Role = "" }),
			"user over cap":      mk(func(m *store.Message) { m.Role = store.RoleUser; m.Content = strings.Repeat("a", 16385) }),
			"assistant over cap": mk(func(m *store.Message) { m.Content = strings.Repeat("a", 65537) }),
			"system over cap":    mk(func(m *store.Message) { m.Role = store.RoleSystem; m.Content = strings.Repeat("a", 65537) }),
			"bad utf-8":          mk(func(m *store.Message) { m.Content = "a\xffb" }),
			"no user":            mk(func(m *store.Message) { m.UserID = "" }),
			"no conversation":    mk(func(m *store.Message) { m.ConversationID = "" }),
		} {
			if err := ins(m); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// the caps are in bytes and inclusive
		for _, m := range []store.Message{
			mk(func(m *store.Message) { m.Role = store.RoleUser; m.Content = strings.Repeat("a", 16384) }),
			mk(func(m *store.Message) { m.Content = strings.Repeat("a", 65536) }),
			mk(func(m *store.Message) { m.Role = store.RoleSystem; m.Content = strings.Repeat("a", 65536) }),
			mk(func(m *store.Message) { m.Content = "multi\nline\ttext" }),
		} {
			if err := ins(m); err != nil {
				t.Errorf("at the cap (%d bytes, %s): %v", len(m.Content), m.Role, err)
			}
		}
		if err := ins(mk(func(m *store.Message) { m.Role = store.RoleUser; m.Content = strings.Repeat("ä", 8193) })); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("16386 bytes in 8193 runes: %v", err)
		}
		// ownership
		for name, m := range map[string]store.Message{
			"foreign conversation": mk(func(m *store.Message) { m.UserID = "u2" }),
			"unknown conversation": mk(func(m *store.Message) { m.ConversationID = "nope" }),
			"foreign job":          mk(func(m *store.Message) { m.JobID = j2.ID }),
			"unknown job":          mk(func(m *store.Message) { m.JobID = "nope" }),
		} {
			if err := ins(m); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := ins(store.Message{ID: m.ID, UserID: "u1", ConversationID: c.ID, Role: store.RoleUser, Content: "dup"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate id: %v", err)
		}
	})

	t.Run("a failed insert changes nothing", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		c := addConv(t, st, "u1", "c")
		clk.Advance(time.Minute)
		_ = txErr(st, func(tx store.Tx) error {
			_, err := tx.InsertMessage(store.Message{UserID: "u1", ConversationID: c.ID, Role: "bad", Content: "x"})
			return err
		})
		_ = viewErr(st, func(tx store.Tx) error {
			got, _ := tx.GetConversation("u1", c.ID)
			if !got.UpdatedAt.Equal(c.UpdatedAt) {
				t.Errorf("updated_at moved to %v", got.UpdatedAt)
			}
			return nil
		})
	})

	t.Run("list is newest first, keyset paged, scoped to the user", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "c")
		other := addConv(t, st, "u1", "other")
		c2 := addConv(t, st, "u2", "c2")
		var all []store.Message
		for i := range 9 {
			all = append(all, addMsg(t, st, "u1", c.ID, store.RoleUser, fmt.Sprint(i), ""))
			if i%3 == 2 { // triples share a created_at
				clk.Advance(time.Second)
			}
		}
		addMsg(t, st, "u1", other.ID, store.RoleUser, "elsewhere", "")
		addMsg(t, st, "u2", c2.ID, store.RoleUser, "foreign", "")
		listed := msgIDs(listMsgs(t, st, "u1", c.ID, store.MessageFilter{}))
		if len(listed) != 9 {
			t.Fatalf("listed = %d", len(listed))
		}
		got := listMsgs(t, st, "u1", c.ID, store.MessageFilter{})
		for i := 1; i < len(got); i++ {
			a, b := got[i-1], got[i]
			if a.CreatedAt.Before(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.ID <= b.ID) {
				t.Errorf("order broken at %d: %+v then %+v", i, a, b)
			}
		}
		for _, m := range all {
			pos := indexOf(listed, m.ID)
			want := listed[pos+1:]
			if got := msgIDs(listMsgs(t, st, "u1", c.ID, store.MessageFilter{Before: &store.PageKey{At: m.CreatedAt, ID: m.ID}})); !equal(got, want) {
				t.Errorf("before %s = %v, want %v", m.ID, got, want)
			}
		}
		if got := listMsgs(t, st, "u1", c.ID, store.MessageFilter{Limit: 4}); len(got) != 4 {
			t.Errorf("limit = %d", len(got))
		}
		_ = viewErr(st, func(tx store.Tx) error {
			if _, err := tx.ListMessages("u2", c.ID, store.MessageFilter{}); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("foreign conversation: %v", err)
			}
			if _, err := tx.ListMessages("u1", "nope", store.MessageFilter{}); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown conversation: %v", err)
			}
			if _, err := tx.ListMessages("u1", c.ID, store.MessageFilter{Before: &store.PageKey{ID: "x"}}); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("zero cursor time: %v", err)
			}
			if _, err := tx.ListMessages("u1", c.ID, store.MessageFilter{Before: &store.PageKey{At: all[0].CreatedAt}}); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("empty cursor id: %v", err)
			}
			return nil
		})
	})

	t.Run("a multi-page walk sees every message once while rows change", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		c := addConv(t, st, "u1", "c")
		for i := range 10 { // ONE created_at
			addMsg(t, st, "u1", c.ID, store.RoleUser, fmt.Sprint(i), "")
		}
		want := msgIDs(listMsgs(t, st, "u1", c.ID, store.MessageFilter{}))
		var seen []string
		var before *store.PageKey
		for page := 0; ; page++ {
			list := listMsgs(t, st, "u1", c.ID, store.MessageFilter{Limit: 3, Before: before})
			if len(list) == 0 {
				break
			}
			seen = append(seen, msgIDs(list)...)
			last := list[len(list)-1]
			before = &store.PageKey{At: last.CreatedAt, ID: last.ID}
			clk.Advance(time.Second)
			addMsg(t, st, "u1", c.ID, store.RoleAssistant, fmt.Sprintf("new-%d", page), "")
			if page > 10 {
				t.Fatal("the walk does not end")
			}
		}
		if !equal(seen, want) {
			t.Errorf("walk = %v, want %v", seen, want)
		}
	})

	t.Run("recent messages: last n, oldest first, user and assistant only, one job excluded", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		c := addConv(t, st, "u1", "c")
		cOther := addConv(t, st, "u1", "other")
		j := addJob(t, st, "u1", "g")
		jx := addJob(t, st, "u1", "g2")
		var seq []store.Message // oldest first, all roles
		add := func(role, content, job string) {
			seq = append(seq, addMsg(t, st, "u1", c.ID, role, content, job))
			if len(seq)%2 == 0 { // pairs share a created_at
				clk.Advance(time.Second)
			}
		}
		add(store.RoleUser, "m0", "")
		add(store.RoleAssistant, "m1", "")
		add(store.RoleSystem, "m2-system", j.ID)
		add(store.RoleUser, "m3", jx.ID)
		add(store.RoleAssistant, "m4", "")
		add(store.RoleUser, "m5", j.ID) // the message of the running job
		addMsg(t, st, "u1", cOther.ID, store.RoleUser, "elsewhere", "")
		contents := func(ms []store.Message) []string {
			var out []string
			for _, m := range ms {
				out = append(out, m.Content)
			}
			return out
		}
		if got := contents(recentMsgs(t, st, "u1", c.ID, 50, "")); !equal(got, []string{"m0", "m1", "m3", "m4", "m5"}) {
			t.Errorf("all = %v", got)
		}
		if got := contents(recentMsgs(t, st, "u1", c.ID, 50, j.ID)); !equal(got, []string{"m0", "m1", "m3", "m4"}) {
			t.Errorf("excluding j = %v (the system message of j is out by role, m5 by job)", got)
		}
		if got := contents(recentMsgs(t, st, "u1", c.ID, 2, j.ID)); !equal(got, []string{"m3", "m4"}) {
			t.Errorf("last 2 = %v", got)
		}
		if got := contents(recentMsgs(t, st, "u1", c.ID, 3, "")); !equal(got, []string{"m3", "m4", "m5"}) {
			t.Errorf("last 3 = %v", got)
		}
		if got := recentMsgs(t, st, "u1", c.ID, 1, "no-such-job"); len(got) != 1 || got[0].Content != "m5" {
			t.Errorf("unknown exclude = %+v", got)
		}
		// n is capped at 50
		for i := range 60 {
			addMsg(t, st, "u1", c.ID, store.RoleUser, fmt.Sprintf("bulk-%02d", i), "")
		}
		got := recentMsgs(t, st, "u1", c.ID, 1000, "")
		if len(got) != 50 || got[0].Content != "bulk-10" || got[49].Content != "bulk-59" {
			t.Errorf("cap: %d rows, first %q last %q", len(got), got[0].Content, got[len(got)-1].Content)
		}
		_ = viewErr(st, func(tx store.Tx) error {
			if _, err := tx.ListRecentMessages("u2", c.ID, 5, ""); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("foreign conversation: %v", err)
			}
			if _, err := tx.ListRecentMessages("u1", "nope", 5, ""); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown conversation: %v", err)
			}
			for _, n := range []int{0, -1} {
				if _, err := tx.ListRecentMessages("u1", c.ID, n, ""); !errors.Is(err, store.ErrInvalid) {
					t.Errorf("n=%d: %v", n, err)
				}
			}
			return nil
		})
	})
}

func runChatCounts(t *testing.T, f Factory) {
	t.Run("jobs are counted by state and trigger kind for one user", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		mk := func(user, kind string) store.Job {
			var j store.Job
			mustTx(t, st, func(tx store.Tx) error {
				var err error
				j, err = tx.InsertJob(store.Job{UserID: user, TriggerKind: kind, Goal: "g"})
				return err
			})
			return j
		}
		a := mk("u1", "chat")
		mk("u1", "chat")
		mk("u1", "sweep")
		mk("u2", "chat")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", a.ID) })
		c := mustClaim(t, st, "w") // the oldest runnable: u1's second chat job
		_ = c
		count := func(user string, states []store.JobState, kind string) int {
			var n int
			if err := viewErr(st, func(tx store.Tx) error {
				var err error
				n, err = tx.CountJobsInStates(user, states, kind)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return n
		}
		q, r, x := []store.JobState{store.JobQueued}, []store.JobState{store.JobRunning}, []store.JobState{store.JobCancelled}
		for name, tc := range map[string]struct {
			user   string
			states []store.JobState
			kind   string
			want   int
		}{
			"queued chat u1":      {"u1", q, "chat", 0},
			"queued sweep u1":     {"u1", q, "sweep", 1},
			"queued any u1":       {"u1", q, "", 1},
			"running chat u1":     {"u1", r, "chat", 1},
			"cancelled chat":      {"u1", x, "chat", 1},
			"queued+running any":  {"u1", append(append([]store.JobState{}, q...), r...), "", 2},
			"all states any kind": {"u1", nil, "", 3},
			"other user":          {"u2", nil, "chat", 1},
			"unknown user":        {"ghost", nil, "", 0},
		} {
			if got := count(tc.user, tc.states, tc.kind); got != tc.want {
				t.Errorf("%s = %d, want %d", name, got, tc.want)
			}
		}
		if err := viewErr(st, func(tx store.Tx) error {
			_, err := tx.CountJobsInStates("u1", []store.JobState{"bogus"}, "")
			return err
		}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("unknown state: %v", err)
		}
	})

	t.Run("conversations are counted per user", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		addConv(t, st, "u1", "a")
		addConv(t, st, "u1", "b")
		addConv(t, st, "u2", "c")
		_ = viewErr(st, func(tx store.Tx) error {
			for u, want := range map[string]int{"u1": 2, "u2": 1, "ghost": 0} {
				if n, err := tx.CountConversations(u); err != nil || n != want {
					t.Errorf("%s = %d %v, want %d", u, n, err, want)
				}
			}
			return nil
		})
	})

	t.Run("change marks move with jobs, messages, audit and policy and not otherwise", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		marks := func(user string) store.ChangeMarks {
			var m store.ChangeMarks
			if err := viewErr(st, func(tx store.Tx) error {
				var err error
				m, err = tx.ChangeMarks(user)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return m
		}
		if z := marks("ghost"); z != (store.ChangeMarks{}) {
			t.Errorf("unknown user = %+v", z)
		}
		base := marks("u1")
		if base.PolicyAt == 0 || base.JobsAt != 0 || base.MessagesAt != 0 || base.AuditSeq != 0 {
			t.Errorf("fresh user = %+v (only the default policy exists)", base)
		}
		clk.Advance(time.Second)
		if got := marks("u1"); got != base {
			t.Errorf("time alone moved the marks: %+v -> %+v", base, got)
		}

		// jobs
		j := addJob(t, st, "u1", "g")
		m1 := marks("u1")
		if m1.JobsAt != clk.Now().UnixMilli() || m1.MessagesAt != 0 || m1.PolicyAt != base.PolicyAt || m1.AuditSeq != 0 {
			t.Errorf("after a job = %+v", m1)
		}
		clk.Advance(time.Second)
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		m2 := marks("u1")
		if m2.JobsAt != clk.Now().UnixMilli() || m2.JobsAt <= m1.JobsAt {
			t.Errorf("after a state change = %+v", m2)
		}

		// messages
		clk.Advance(time.Second)
		c := addConv(t, st, "u1", "c")
		addMsg(t, st, "u1", c.ID, store.RoleUser, "x", "")
		m3 := marks("u1")
		if m3.MessagesAt != clk.Now().UnixMilli() || m3.JobsAt != m2.JobsAt {
			t.Errorf("after a message = %+v", m3)
		}

		// audit
		clk.Advance(time.Second)
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: "test"})
			return err
		})
		m4 := marks("u1")
		if m4.AuditSeq != 1 || m4.MessagesAt != m3.MessagesAt {
			t.Errorf("after an audit event = %+v", m4)
		}
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: "test"})
			return err
		})
		if m := marks("u1"); m.AuditSeq != 2 {
			t.Errorf("second audit event = %+v", m)
		}

		// policy
		clk.Advance(time.Second)
		mustTx(t, st, func(tx store.Tx) error {
			p, err := tx.GetPolicy("u1")
			if err != nil {
				return err
			}
			p.ProactiveEnabled = !p.ProactiveEnabled
			return tx.PutPolicy(p)
		})
		m5 := marks("u1")
		if m5.PolicyAt != clk.Now().UnixMilli() || m5.PolicyAt <= base.PolicyAt || m5.JobsAt != m2.JobsAt {
			t.Errorf("after a policy change = %+v", m5)
		}

		// another user's activity changes nothing
		clk.Advance(time.Second)
		addJob(t, st, "u2", "other")
		c2 := addConv(t, st, "u2", "c")
		addMsg(t, st, "u2", c2.ID, store.RoleUser, "x", "")
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.AppendAudit(audit.Event{UserID: "u2", Kind: "test"})
			return err
		})
		if got := marks("u1"); got != marks("u1") || got != m5 {
			t.Errorf("another user's changes moved the marks: %+v -> %+v", m5, got)
		}
	})
}

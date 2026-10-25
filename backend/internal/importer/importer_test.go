package importer

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kshitijv97/between-us/backend/internal/ingest"
	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/store/sqlite"
	"github.com/kshitijv97/between-us/backend/internal/synth"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"

	_ "github.com/kshitijv97/between-us/backend/internal/ingest/telegram"
	_ "github.com/kshitijv97/between-us/backend/internal/ingest/whatsapp"
)

// env is a migrated SQLite store with a local tenant scope.
type env struct {
	ctx context.Context
	st  store.Store
	im  *Importer
}

func newEnv(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn, u, err := st.EnsureLocalTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx = tenancy.WithScope(ctx, tenancy.Scope{TenantID: tn.ID, UserID: u.ID})
	return env{ctx: ctx, st: st, im: &Importer{Store: st, BatchSize: 250}}
}

func whatsapp(t *testing.T, n int) ingest.Source {
	t.Helper()
	var buf bytes.Buffer
	if err := synth.WriteWhatsApp(&buf, synth.DefaultConfig(n)); err != nil {
		t.Fatal(err)
	}
	return ingest.BytesSource{Data: buf.Bytes(), FileName: "WhatsApp Chat with Bob.txt"}
}

func telegram(t *testing.T, n int) ingest.Source {
	t.Helper()
	var buf bytes.Buffer
	if err := synth.WriteTelegram(&buf, synth.DefaultConfig(n)); err != nil {
		t.Fatal(err)
	}
	return ingest.BytesSource{Data: buf.Bytes(), FileName: "chat.json"}
}

func req(src ingest.Source, tz string) Request {
	return Request{PreviewRequest: PreviewRequest{Source: src, Timezone: tz}}
}

func TestWhatsAppImportAndIncrementalReimport(t *testing.T) {
	e := newEnv(t)

	first, err := e.im.Run(e.ctx, req(whatsapp(t, 1000), "Asia/Kolkata"))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	// 1000 generated messages + the encryption notice (a system row).
	if first.Inserted != 1001 || first.Duplicates != 0 {
		t.Fatalf("inserted=%d duplicates=%d", first.Inserted, first.Duplicates)
	}
	if first.Chat.Title != "Bob" || first.Chat.Platform != "whatsapp" || first.Chat.MessageCount != 1001 {
		t.Errorf("chat = %+v", first.Chat)
	}
	parts, err := e.st.ListParticipants(e.ctx, first.Chat.ID)
	if err != nil || len(parts) != 2 {
		t.Fatalf("participants = %v, %v", parts, err)
	}

	// A longer export of the same chat (same seed → same first 1000 messages)
	// adds only the new tail (FR-ING-010).
	r := req(whatsapp(t, 1500), "Asia/Kolkata")
	r.ChatID = first.Chat.ID
	second, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if second.Inserted != 500 || second.Duplicates != 1001 {
		t.Errorf("re-import inserted=%d duplicates=%d, want 500/1001", second.Inserted, second.Duplicates)
	}
	if n, _ := e.st.CountMessages(e.ctx, first.Chat.ID); n != 1501 {
		t.Errorf("visible messages = %d, want 1501", n)
	}
	if parts, _ := e.st.ListParticipants(e.ctx, first.Chat.ID); len(parts) != 2 {
		t.Errorf("re-import created extra participants: %d", len(parts))
	}
}

func TestReimportWithDifferentTimezoneDoesNotDuplicate(t *testing.T) {
	e := newEnv(t)
	first, err := e.im.Run(e.ctx, req(whatsapp(t, 300), "Asia/Kolkata"))
	if err != nil {
		t.Fatal(err)
	}
	r := req(whatsapp(t, 300), "Europe/London")
	r.ChatID = first.Chat.ID
	again, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if again.Inserted != 0 {
		t.Errorf("timezone change duplicated %d messages", again.Inserted)
	}
}

func TestIdenticalFileShortCircuits(t *testing.T) {
	e := newEnv(t)
	src := whatsapp(t, 50)
	r := req(src, "UTC")
	r.FileSHA256 = []byte("same-digest-for-test-purposes-32")
	first, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.ChatID = first.Chat.ID
	if _, err := e.im.Run(e.ctx, r); !errors.Is(err, ErrAlreadyImported) {
		t.Fatalf("err = %v, want ErrAlreadyImported", err)
	}
}

func TestTelegramImportAndReimport(t *testing.T) {
	e := newEnv(t)
	first, err := e.im.Run(e.ctx, req(telegram(t, 800), "UTC"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if first.Inserted != 800 || first.Chat.Platform != "telegram" {
		t.Fatalf("inserted=%d platform=%s", first.Inserted, first.Chat.Platform)
	}
	r := req(telegram(t, 1000), "UTC")
	r.ChatID = first.Chat.ID
	second, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 200 || second.Duplicates != 800 {
		t.Errorf("inserted=%d duplicates=%d", second.Inserted, second.Duplicates)
	}

	// Cross-platform re-import is refused.
	r = req(whatsapp(t, 10), "UTC")
	r.ChatID = first.Chat.ID
	if _, err := e.im.Run(e.ctx, r); !errors.Is(err, ErrPlatformMismatch) {
		t.Errorf("err = %v, want ErrPlatformMismatch", err)
	}
}

func TestMeKeyMarksParticipant(t *testing.T) {
	e := newEnv(t)
	pv, err := e.im.Preview(e.ctx, req(whatsapp(t, 50), "UTC").PreviewRequest)
	if err != nil {
		t.Fatal(err)
	}
	r := req(whatsapp(t, 50), "UTC")
	r.MeKey = pv.Report.Participants[0].Key
	res, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := e.st.ListParticipants(e.ctx, res.Chat.ID)
	var me int
	for _, p := range parts {
		if p.IsMe {
			me++
			if !strings.EqualFold(p.DisplayName, pv.Report.Participants[0].Display) {
				t.Errorf("wrong participant marked as me: %q", p.DisplayName)
			}
		}
	}
	if me != 1 {
		t.Errorf("%d participants marked as me, want 1", me)
	}
}

const groupChat = "01/02/2024, 10:00 - Alice: hi all\n" +
	"01/02/2024, 10:01 - Bob: hello\n" +
	"01/02/2024, 10:02 - Chandra: namaste\n" +
	"01/02/2024, 10:03 - Alice: kaise ho?\n"

func TestMoreThanTwoSendersRequiresSelection(t *testing.T) {
	e := newEnv(t)
	src := ingest.BytesSource{Data: []byte(groupChat), FileName: "group.txt"}
	_, err := e.im.Run(e.ctx, req(src, "UTC"))
	if !errors.Is(err, ErrTooManyParticipants) {
		t.Fatalf("err = %v, want ErrTooManyParticipants", err)
	}
	if chats, _ := e.st.ListChats(e.ctx); len(chats) != 0 {
		t.Fatalf("refused import left %d chats", len(chats))
	}

	pv, err := e.im.Preview(e.ctx, req(src, "UTC").PreviewRequest)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, p := range pv.Report.Participants {
		keys[p.Display] = p.Key
	}
	r := req(src, "UTC")
	r.Participants = []string{keys["Alice"], keys["Chandra"]}
	res, err := e.im.Run(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 3 || res.Filtered != 1 {
		t.Errorf("inserted=%d filtered=%d, want 3/1", res.Inserted, res.Filtered)
	}
}

func TestUnrecognisedFormatWritesNothing(t *testing.T) {
	e := newEnv(t)
	src := ingest.BytesSource{Data: []byte("just some notes\nnothing like a chat\n"), FileName: "notes.txt"}
	if _, err := e.im.Run(e.ctx, req(src, "UTC")); !errors.Is(err, ingest.ErrUnrecognised) {
		t.Fatalf("err = %v, want ErrUnrecognised", err)
	}
	if chats, _ := e.st.ListChats(e.ctx); len(chats) != 0 {
		t.Errorf("chats = %d, want 0", len(chats))
	}
}

func TestCancelledImportRollsBack(t *testing.T) {
	e := newEnv(t)
	// The sink cancels the context after the first batch has been inserted.
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.im.Store = &cancelAfterFirstInsert{Store: e.st, cancel: cancel}

	_, err := e.im.Run(ctx, req(whatsapp(t, 2000), "UTC"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if chats, _ := e.st.ListChats(e.ctx); len(chats) != 0 {
		t.Errorf("cancelled first import left %d chats", len(chats))
	}
}

// cancelAfterFirstInsert cancels the import's context once rows were written,
// proving that partially written imports are cleaned up.
type cancelAfterFirstInsert struct {
	store.Store
	cancel context.CancelFunc
	calls  int
}

func (c *cancelAfterFirstInsert) InsertMessages(ctx context.Context, msgs []store.Message) (int, error) {
	n, err := c.Store.InsertMessages(ctx, msgs)
	c.calls++
	if c.calls == 1 {
		c.cancel()
	}
	return n, err
}

func TestRunRequiresScope(t *testing.T) {
	e := newEnv(t)
	if _, err := e.im.Run(context.Background(), req(whatsapp(t, 5), "UTC")); !errors.Is(err, tenancy.ErrNoScope) {
		t.Fatalf("err = %v, want ErrNoScope", err)
	}
}

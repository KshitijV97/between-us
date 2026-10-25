// Package storetest is the behavioural contract every store.Store
// implementation must pass. Dialect packages call Run from their tests with a
// factory for fresh, migrated stores:
//
//	func TestContract(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) store.Store { ... })
//	}
//
// The suite is dialect-neutral on purpose: SQLite runs it in M1 and Postgres
// (with RLS) reuses it unchanged in M5, so both behave identically, including
// tenant isolation (MT-001, acceptance criterion 7).
package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/store"
	"github.com/kshitijv97/between-us/backend/internal/tenancy"
)

// TenantCreator is implemented by stores that can create additional tenants.
// store.Store gains tenant creation with hosted sign-up (M5); until then the
// isolation tests use this optional interface and are skipped without it.
type TenantCreator interface {
	CreateTenant(ctx context.Context, name, slug string) (store.Tenant, error)
}

// Run executes the whole contract suite. newStore must return an empty,
// migrated store that is closed by the caller's t.Cleanup.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, e *env)
	}{
		{"LocalTenantIsIdempotent", testLocalTenant},
		{"MissingScopeIsRejected", testMissingScope},
		{"Chats", testChats},
		{"Participants", testParticipants},
		{"Imports", testImports},
		{"InsertDedupAndVisibility", testInsertDedupAndVisibility},
		{"InsertIntoFinishedImportIsRejected", testInsertIntoFinishedImport},
		{"FailImportRemovesMessages", testFailImport},
		{"ReplyResolution", testReplyResolution},
		{"ListMessagesWindowAndPagination", testListMessages},
		{"SearchMessages", testSearch},
		{"SearchSanitisesQueries", testSearchSanitisation},
		{"UnicodeAndMetaRoundTrip", testUnicodeRoundTrip},
		{"DeleteChatCascades", testDeleteChat},
		{"TenantIsolation", testTenantIsolation},
		{"ConcurrentReadsDuringWrites", testConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			tenant, user, err := s.EnsureLocalTenant(ctx)
			if err != nil {
				t.Fatalf("EnsureLocalTenant: %v", err)
			}
			tt.fn(t, &env{
				s:      s,
				tenant: tenant,
				user:   user,
				ctx:    tenancy.WithScope(ctx, tenancy.Scope{TenantID: tenant.ID, UserID: user.ID}),
			})
		})
	}
}

// env is the per-test fixture: a store and a context scoped to the local tenant.
type env struct {
	s      store.Store
	tenant store.Tenant
	user   store.User
	ctx    context.Context
}

// base is a fixed reference instant; all fixture times derive from it.
var base = time.Date(2024, 3, 9, 18, 30, 0, 0, time.UTC)

func (e *env) chat(t *testing.T, title string) store.Chat {
	t.Helper()
	c, err := e.s.CreateChat(e.ctx, store.Chat{Title: title, Platform: "whatsapp", AnalysisTZ: "Asia/Kolkata"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return c
}

func (e *env) imp(t *testing.T, chatID uuid.UUID) store.Import {
	t.Helper()
	im, err := e.s.CreateImport(e.ctx, store.Import{ChatID: chatID, Platform: "whatsapp", Timezone: "Asia/Kolkata",
		FileName: "export.txt", FileSize: 123, FileSHA256: sum(chatID.String())})
	if err != nil {
		t.Fatalf("CreateImport: %v", err)
	}
	return im
}

// msg builds a text message whose fingerprint derives from key, so equal keys
// collide exactly like re-imported messages do.
func msg(c store.Chat, im store.Import, ordinal int64, body, key string) store.Message {
	sent := base.Add(time.Duration(ordinal) * time.Minute)
	return store.Message{
		ChatID: c.ID, ImportID: im.ID, Ordinal: ordinal, SentAt: sent,
		LocalDate: sent.Format(time.DateOnly), Kind: "text", Body: body, Fingerprint: sum(key),
	}
}

func (e *env) insert(t *testing.T, msgs ...store.Message) int {
	t.Helper()
	n, err := e.s.InsertMessages(e.ctx, msgs)
	if err != nil {
		t.Fatalf("InsertMessages: %v", err)
	}
	return n
}

func (e *env) complete(t *testing.T, im store.Import) {
	t.Helper()
	im.Report = json.RawMessage(`{"messages":1}`)
	if err := e.s.CompleteImport(e.ctx, im); err != nil {
		t.Fatalf("CompleteImport: %v", err)
	}
}

func (e *env) count(t *testing.T, chatID uuid.UUID) int64 {
	t.Helper()
	n, err := e.s.CountMessages(e.ctx, chatID)
	if err != nil {
		t.Fatalf("CountMessages: %v", err)
	}
	return n
}

func (e *env) search(t *testing.T, chatID uuid.UUID, q string) []string {
	t.Helper()
	ms, err := e.s.SearchMessages(e.ctx, chatID, q, 50)
	if err != nil {
		t.Fatalf("SearchMessages(%q): %v", q, err)
	}
	return bodies(ms)
}

func sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func bodies(ms []store.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Body
	}
	return out
}

func wantErr(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: err = %v, want %v", what, err, target)
	}
}

// ---------------------------------------------------------------- tests

func testLocalTenant(t *testing.T, e *env) {
	t2, u2, err := e.s.EnsureLocalTenant(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t2.ID != e.tenant.ID || u2.ID != e.user.ID {
		t.Errorf("second call returned a new tenant/user: %v/%v vs %v/%v", t2.ID, u2.ID, e.tenant.ID, e.user.ID)
	}
	if e.tenant.Slug != "local" || e.tenant.Status != "active" || u2.AuthProvider != "local" {
		t.Errorf("unexpected local identity: %+v %+v", e.tenant, u2)
	}
}

func testMissingScope(t *testing.T, e *env) {
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	calls := map[string]func() error{
		"CreateChat":        func() error { _, err := e.s.CreateChat(ctx, store.Chat{Title: "x", Platform: "whatsapp"}); return err },
		"GetChat":           func() error { _, err := e.s.GetChat(ctx, id); return err },
		"ListChats":         func() error { _, err := e.s.ListChats(ctx); return err },
		"DeleteChat":        func() error { return e.s.DeleteChat(ctx, id) },
		"ListParticipants":  func() error { _, err := e.s.ListParticipants(ctx, id); return err },
		"UpsertParticipant": func() error { _, err := e.s.UpsertParticipant(ctx, store.Participant{ChatID: id}); return err },
		"CreateImport":      func() error { _, err := e.s.CreateImport(ctx, store.Import{ChatID: id}); return err },
		"GetImport":         func() error { _, err := e.s.GetImport(ctx, id); return err },
		"ListImports":       func() error { _, err := e.s.ListImports(ctx, id); return err },
		"FindBySHA":         func() error { _, err := e.s.FindCompletedImportBySHA256(ctx, id, sum("x")); return err },
		"InsertMessages": func() error {
			_, err := e.s.InsertMessages(ctx, []store.Message{{ChatID: id, ImportID: id, Fingerprint: sum("x")}})
			return err
		},
		"MaxOrdinal":     func() error { _, err := e.s.MaxOrdinal(ctx, id); return err },
		"CompleteImport": func() error { return e.s.CompleteImport(ctx, store.Import{ID: id}) },
		"FailImport":     func() error { return e.s.FailImport(ctx, id, "x", nil) },
		"ListMessages":   func() error { _, err := e.s.ListMessages(ctx, store.MessageQuery{ChatID: id}); return err },
		"CountMessages":  func() error { _, err := e.s.CountMessages(ctx, id); return err },
		"SearchMessages": func() error { _, err := e.s.SearchMessages(ctx, id, "x", 10); return err },
	}
	for name, call := range calls {
		wantErr(t, name, call(), tenancy.ErrNoScope)
	}
}

func testChats(t *testing.T, e *env) {
	other := uuid.Must(uuid.NewV7())
	a, err := e.s.CreateChat(e.ctx, store.Chat{TenantID: other, Title: "Alice & Bob", Platform: "whatsapp", AnalysisTZ: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	if a.TenantID != e.tenant.ID {
		t.Errorf("TenantID = %v, must come from the scope (%v)", a.TenantID, e.tenant.ID)
	}
	if a.CreatedBy != e.user.ID || string(a.Settings) != "{}" || a.ID == uuid.Nil {
		t.Errorf("defaults not applied: %+v", a)
	}
	time.Sleep(2 * time.Millisecond) // distinct created_at for ordering
	b := e.chat(t, "Second")

	got, err := e.s.GetChat(e.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Alice & Bob" || got.AnalysisTZ != "Asia/Kolkata" || got.Platform != "whatsapp" || got.MessageCount != 0 {
		t.Errorf("GetChat = %+v", got)
	}
	list, err := e.s.ListChats(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Errorf("ListChats order = %v, want newest first", list)
	}
	_, err = e.s.GetChat(e.ctx, uuid.Must(uuid.NewV7()))
	wantErr(t, "GetChat(missing)", err, store.ErrNotFound)
}

func testParticipants(t *testing.T, e *env) {
	c := e.chat(t, "p")
	alice, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "Alice", PlatformKeys: []string{"alice", "+911234567890"}})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	alice.Alias, alice.IsMe = "Me", true
	if _, err := e.s.UpsertParticipant(e.ctx, alice); err != nil {
		t.Fatalf("update: %v", err)
	}
	ps, err := e.s.ListParticipants(e.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != alice.ID || ps[1].ID != bob.ID {
		t.Fatalf("ListParticipants = %+v", ps)
	}
	if !ps[0].IsMe || ps[0].Alias != "Me" || !slices.Equal(ps[0].PlatformKeys, []string{"alice", "+911234567890"}) {
		t.Errorf("alice = %+v", ps[0])
	}
	if ps[1].PlatformKeys == nil {
		t.Error("PlatformKeys should be an empty slice, not nil")
	}
	bob.IsMe = true
	_, err = e.s.UpsertParticipant(e.ctx, bob)
	wantErr(t, "second is_me", err, store.ErrConflict)

	_, err = e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: uuid.Must(uuid.NewV7()), DisplayName: "x"})
	wantErr(t, "unknown chat", err, store.ErrNotFound)
}

func testImports(t *testing.T, e *env) {
	c := e.chat(t, "i")
	first := e.imp(t, c.ID)
	if first.Status != store.ImportProcessing || first.TenantID != e.tenant.ID || first.CreatedBy != e.user.ID {
		t.Errorf("defaults: %+v", first)
	}
	got, err := e.s.GetImport(e.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FileName != "export.txt" || got.FileSize != 123 || !slices.Equal(got.FileSHA256, sum(c.ID.String())) || got.Timezone != "Asia/Kolkata" {
		t.Errorf("GetImport = %+v", got)
	}

	_, err = e.s.FindCompletedImportBySHA256(e.ctx, c.ID, sum(c.ID.String()))
	wantErr(t, "FindBySHA before completion", err, store.ErrNotFound)

	first.Variant, first.TimePrecision = "android-24h-dmy", "minute"
	first.InsertedCount, first.DuplicateCount = 7, 2
	e.complete(t, first)
	found, err := e.s.FindCompletedImportBySHA256(e.ctx, c.ID, sum(c.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != first.ID || found.Status != store.ImportCompleted || found.CompletedAt == nil ||
		found.Variant != "android-24h-dmy" || found.TimePrecision != "minute" || found.InsertedCount != 7 || found.DuplicateCount != 2 {
		t.Errorf("completed import = %+v", found)
	}
	var rep map[string]int
	if err := json.Unmarshal(found.Report, &rep); err != nil || rep["messages"] != 1 {
		t.Errorf("report = %s (%v)", found.Report, err)
	}

	time.Sleep(2 * time.Millisecond)
	second := e.imp(t, c.ID)
	list, err := e.s.ListImports(e.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID {
		t.Errorf("ListImports = %v, want newest first", list)
	}
	_, err = e.s.CreateImport(e.ctx, store.Import{ChatID: uuid.Must(uuid.NewV7()), Platform: "whatsapp"})
	wantErr(t, "CreateImport(unknown chat)", err, store.ErrNotFound)
}

func testInsertDedupAndVisibility(t *testing.T, e *env) {
	c := e.chat(t, "v")
	im1 := e.imp(t, c.ID)
	batch := []store.Message{msg(c, im1, 1, "hi", "k1"), msg(c, im1, 2, "hello", "k2"), msg(c, im1, 3, "bye", "k3")}
	if n := e.insert(t, batch...); n != 3 {
		t.Fatalf("inserted %d, want 3", n)
	}
	if n := e.insert(t, batch...); n != 0 {
		t.Errorf("re-insert inserted %d, want 0 (dedup on fingerprint)", n)
	}
	if max, err := e.s.MaxOrdinal(e.ctx, c.ID); err != nil || max != 3 {
		t.Errorf("MaxOrdinal = %d, %v (must include in-progress import)", max, err)
	}
	// Invisible until the import completes (NFR-REL-002).
	if n := e.count(t, c.ID); n != 0 {
		t.Errorf("visible before completion: %d", n)
	}
	if hits := e.search(t, c.ID, "hello"); len(hits) != 0 {
		t.Errorf("search found unfinished import: %v", hits)
	}
	e.complete(t, im1)
	if n := e.count(t, c.ID); n != 3 {
		t.Errorf("count after completion = %d", n)
	}
	got, err := e.s.GetChat(e.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 3 || got.FirstMessageAt == nil || !got.FirstMessageAt.Equal(base.Add(time.Minute)) ||
		got.LastMessageAt == nil || !got.LastMessageAt.Equal(base.Add(3*time.Minute)) {
		t.Errorf("chat counters = %d %v %v", got.MessageCount, got.FirstMessageAt, got.LastMessageAt)
	}

	// Incremental re-import: two known messages plus one new.
	im2 := e.imp(t, c.ID)
	n := e.insert(t, msg(c, im2, 4, "hello", "k2"), msg(c, im2, 5, "bye", "k3"), msg(c, im2, 6, "new", "k4"))
	if n != 1 {
		t.Errorf("incremental insert = %d, want 1", n)
	}
	if n := e.count(t, c.ID); n != 3 {
		t.Errorf("new message visible before its import completed: %d", n)
	}
	e.complete(t, im2)
	if n := e.count(t, c.ID); n != 4 {
		t.Errorf("count after second import = %d, want 4", n)
	}
	if got, _ := e.s.GetChat(e.ctx, c.ID); got.MessageCount != 4 {
		t.Errorf("MessageCount = %d, want 4", got.MessageCount)
	}

	// Same fingerprint in a different chat is not a duplicate.
	c2 := e.chat(t, "other")
	im3 := e.imp(t, c2.ID)
	if n := e.insert(t, msg(c2, im3, 1, "hi", "k1")); n != 1 {
		t.Errorf("fingerprint must be unique per chat only; inserted %d", n)
	}
}

func testInsertIntoFinishedImport(t *testing.T, e *env) {
	c := e.chat(t, "f")
	im := e.imp(t, c.ID)
	e.complete(t, im)
	_, err := e.s.InsertMessages(e.ctx, []store.Message{msg(c, im, 1, "late", "late")})
	wantErr(t, "insert into completed import", err, store.ErrConflict)

	_, err = e.s.InsertMessages(e.ctx, []store.Message{{ChatID: c.ID, ImportID: e.imp(t, c.ID).ID, Kind: "text"}})
	if err == nil {
		t.Error("message without fingerprint must be rejected")
	}
}

func testFailImport(t *testing.T, e *env) {
	c := e.chat(t, "fail")
	ok := e.imp(t, c.ID)
	e.insert(t, msg(c, ok, 1, "kept message", "a"))
	e.complete(t, ok)

	bad := e.imp(t, c.ID)
	e.insert(t, msg(c, bad, 2, "doomed message", "b"), msg(c, bad, 3, "doomed too", "c"))
	if err := e.s.FailImport(e.ctx, bad.ID, "The file ended unexpectedly.", json.RawMessage(`{"messages":2}`)); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetImport(e.ctx, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ImportFailed || got.Error != "The file ended unexpectedly." || string(got.Report) != `{"messages":2}` {
		t.Errorf("failed import = %+v", got)
	}
	if max, _ := e.s.MaxOrdinal(e.ctx, c.ID); max != 1 {
		t.Errorf("MaxOrdinal = %d, failed import's rows must be deleted", max)
	}
	if hits := e.search(t, c.ID, "doomed"); len(hits) != 0 {
		t.Errorf("search still finds failed rows: %v", hits)
	}
	if hits := e.search(t, c.ID, "kept"); len(hits) != 1 {
		t.Errorf("completed rows lost: %v", hits)
	}
	// The same messages can be imported again later.
	retry := e.imp(t, c.ID)
	if n := e.insert(t, msg(c, retry, 2, "doomed message", "b")); n != 1 {
		t.Errorf("retry inserted %d, want 1", n)
	}
	wantErr(t, "FailImport(missing)", e.s.FailImport(e.ctx, uuid.Must(uuid.NewV7()), "x", nil), store.ErrNotFound)
}

func testReplyResolution(t *testing.T, e *env) {
	c := e.chat(t, "replies")
	im1 := e.imp(t, c.ID)
	m1 := msg(c, im1, 1, "question?", "t1")
	m1.PlatformMsgID = "100"
	m2 := msg(c, im1, 2, "answer", "t2")
	m2.PlatformMsgID, m2.ReplyToPlatformID = "101", "100"
	m3 := msg(c, im1, 3, "reply to a message not in the export", "t3")
	m3.PlatformMsgID, m3.ReplyToPlatformID = "102", "99"
	e.insert(t, m1, m2, m3)
	e.complete(t, im1)

	im2 := e.imp(t, c.ID)
	m4 := msg(c, im2, 4, "late reply to the first", "t4")
	m4.PlatformMsgID, m4.ReplyToPlatformID = "103", "100"
	e.insert(t, m4)
	e.complete(t, im2)

	ms, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 {
		t.Fatalf("got %d messages", len(ms))
	}
	first := ms[0].ID
	if ms[0].ReplyToID != nil {
		t.Errorf("m1 should not be a reply")
	}
	if ms[1].ReplyToID == nil || *ms[1].ReplyToID != first {
		t.Errorf("m2.ReplyToID = %v, want %d", ms[1].ReplyToID, first)
	}
	if ms[2].ReplyToID != nil || ms[2].ReplyToPlatformID != "99" {
		t.Errorf("reply to a missing target must stay unresolved: %+v", ms[2])
	}
	if ms[3].ReplyToID == nil || *ms[3].ReplyToID != first {
		t.Errorf("cross-import reply not resolved: %v", ms[3].ReplyToID)
	}
}

func testListMessages(t *testing.T, e *env) {
	c := e.chat(t, "list")
	im := e.imp(t, c.ID)
	var batch []store.Message
	for i := int64(1); i <= 10; i++ {
		batch = append(batch, msg(c, im, i, fmt.Sprintf("m%02d", i), fmt.Sprint(i)))
	}
	e.insert(t, batch...)
	e.complete(t, im)

	var pages [][]string
	after := int64(0)
	for {
		ms, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: c.ID, AfterOrdinal: after, Limit: 4})
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 0 {
			break
		}
		pages = append(pages, bodies(ms))
		after = ms[len(ms)-1].Ordinal
	}
	if len(pages) != 3 || !slices.Equal(pages[0], []string{"m01", "m02", "m03", "m04"}) || !slices.Equal(pages[2], []string{"m09", "m10"}) {
		t.Errorf("pages = %v", pages)
	}

	// Half-open window [m03, m06).
	ms, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: c.ID, From: base.Add(3 * time.Minute), To: base.Add(6 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(ms); !slices.Equal(got, []string{"m03", "m04", "m05"}) {
		t.Errorf("window = %v", got)
	}
	m := ms[0]
	if m.TenantID != e.tenant.ID || m.ChatID != c.ID || m.ImportID != im.ID || m.Kind != "text" ||
		m.LocalDate != base.Add(3*time.Minute).Format(time.DateOnly) || !m.SentAt.Equal(base.Add(3*time.Minute)) ||
		!slices.Equal(m.Fingerprint, sum("3")) || m.ID == 0 {
		t.Errorf("round-trip mismatch: %+v", m)
	}
	_, err = e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: uuid.Must(uuid.NewV7())})
	wantErr(t, "ListMessages(unknown chat)", err, store.ErrNotFound)
}

func testSearch(t *testing.T, e *env) {
	c := e.chat(t, "search")
	im := e.imp(t, c.ID)
	e.insert(t,
		msg(c, im, 1, "Let's meet at the café tomorrow", "s1"),
		msg(c, im, 2, "kal milte hain", "s2"),
		msg(c, im, 3, "कल मिलते हैं", "s3"),
		msg(c, im, 4, "Love you ❤️ see you at 7", "s4"),
		msg(c, im, 5, "meeting moved to Friday", "s5"),
	)
	e.complete(t, im)
	cases := map[string][]string{
		"cafe":       {"Let's meet at the café tomorrow"}, // diacritics folded
		"CAFÉ":       {"Let's meet at the café tomorrow"}, // case-insensitive
		"milte":      {"kal milte hain"},
		"मिलते":      {"कल मिलते हैं"},
		"mil*":       {"kal milte hain"},
		"meet*":      {"Let's meet at the café tomorrow", "meeting moved to Friday"},
		"see 7":      {"Love you ❤️ see you at 7"}, // implicit AND
		"nothing":    {},
		"meet zebra": {},
	}
	for q, want := range cases {
		got := e.search(t, c.ID, q)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("search %q = %q, want %q", q, got, want)
		}
	}
	// Another chat's identical text is never returned.
	other := e.chat(t, "other")
	oim := e.imp(t, other.ID)
	e.insert(t, msg(other, oim, 1, "kal milte hain", "o1"))
	e.complete(t, oim)
	if got := e.search(t, c.ID, "milte"); len(got) != 1 {
		t.Errorf("search leaked across chats: %v", got)
	}
}

func testSearchSanitisation(t *testing.T, e *env) {
	c := e.chat(t, "fts")
	im := e.imp(t, c.ID)
	e.insert(t, msg(c, im, 1, `he said "AND" then NEAR(the end) foo`, "q1"))
	e.complete(t, im)
	for _, q := range []string{`"`, `""`, `AND`, `OR NOT`, `foo*`, `NEAR(`, `)(`, `*`, `-`, `^foo`, `col:foo`, `   `, ``, `'; DROP TABLE messages; --`} {
		if _, err := e.s.SearchMessages(e.ctx, c.ID, q, 10); err != nil {
			t.Errorf("SearchMessages(%q) error: %v", q, err)
		}
	}
	if got := e.search(t, c.ID, `AND`); len(got) != 1 {
		t.Errorf("operator words must be searchable as text: %v", got)
	}
	if got := e.search(t, c.ID, `NEAR(the`); len(got) != 1 {
		t.Errorf(`"NEAR(the" should match as plain text: %v`, got)
	}
}

func testUnicodeRoundTrip(t *testing.T, e *env) {
	c := e.chat(t, "unicode")
	im := e.imp(t, c.ID)
	texts := []string{
		"👩‍❤️‍👨 family emoji ZWJ",
		"नमस्ते, कैसे हो?",
		"مرحبا كيف حالك",
		"multi\nline\n\nmessage",
		"",
	}
	pid := func() *uuid.UUID {
		p, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "Ålice 🌸"})
		if err != nil {
			t.Fatal(err)
		}
		return &p.ID
	}()
	var batch []store.Message
	for i, s := range texts {
		m := msg(c, im, int64(i+1), s, fmt.Sprint("u", i))
		m.ParticipantID = pid
		batch = append(batch, m)
	}
	media := msg(c, im, 9, "", "media")
	media.Kind, media.MediaType, media.SystemType = "media", "voice_note", ""
	media.IsEdited, media.IsForwarded, media.IsDeleted = true, true, false
	media.Meta = json.RawMessage(`{"media":{"duration_sec":12,"omitted":true}}`)
	sys := msg(c, im, 10, "", "sys")
	sys.Kind, sys.SystemType = "system", "encryption_notice"
	batch = append(batch, media, sys)
	e.insert(t, batch...)
	e.complete(t, im)

	ms, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != len(texts)+2 {
		t.Fatalf("got %d rows", len(ms))
	}
	for i, s := range texts {
		if ms[i].Body != s || ms[i].ParticipantID == nil || *ms[i].ParticipantID != *pid {
			t.Errorf("row %d = %q (participant %v), want %q", i, ms[i].Body, ms[i].ParticipantID, s)
		}
	}
	gm := ms[len(texts)]
	if gm.Kind != "media" || gm.MediaType != "voice_note" || !gm.IsEdited || !gm.IsForwarded || gm.IsDeleted || gm.ParticipantID != nil {
		t.Errorf("media row = %+v", gm)
	}
	var meta map[string]map[string]any
	if err := json.Unmarshal(gm.Meta, &meta); err != nil || meta["media"]["duration_sec"] != float64(12) {
		t.Errorf("meta = %s (%v)", gm.Meta, err)
	}
	if s := ms[len(texts)+1]; s.Kind != "system" || s.SystemType != "encryption_notice" || string(s.Meta) != "{}" {
		t.Errorf("system row = %+v", s)
	}
}

func testDeleteChat(t *testing.T, e *env) {
	doomed := e.chat(t, "doomed")
	keep := e.chat(t, "keep")
	for _, c := range []store.Chat{doomed, keep} {
		if _, err := e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: c.ID, DisplayName: "A"}); err != nil {
			t.Fatal(err)
		}
		im := e.imp(t, c.ID)
		var batch []store.Message
		for i := int64(1); i <= 120; i++ {
			batch = append(batch, msg(c, im, i, fmt.Sprintf("shared words number %d", i), fmt.Sprint(i)))
		}
		e.insert(t, batch...)
		e.complete(t, im)
	}
	if err := e.s.DeleteChat(e.ctx, doomed.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.GetChat(e.ctx, doomed.ID)
	wantErr(t, "GetChat(deleted)", err, store.ErrNotFound)
	_, err = e.s.ListParticipants(e.ctx, doomed.ID)
	wantErr(t, "ListParticipants(deleted)", err, store.ErrNotFound)
	_, err = e.s.ListImports(e.ctx, doomed.ID)
	wantErr(t, "ListImports(deleted)", err, store.ErrNotFound)
	wantErr(t, "DeleteChat twice", e.s.DeleteChat(e.ctx, doomed.ID), store.ErrNotFound)

	list, _ := e.s.ListChats(e.ctx)
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Errorf("ListChats after delete = %v", list)
	}
	if n := e.count(t, keep.ID); n != 120 {
		t.Errorf("other chat lost messages: %d", n)
	}
	if got := e.search(t, keep.ID, "shared words"); len(got) != 50 { // capped by the search limit
		t.Errorf("search in surviving chat = %d hits", len(got))
	}
}

func testTenantIsolation(t *testing.T, e *env) {
	tc, ok := e.s.(TenantCreator)
	if !ok {
		t.Skip("store cannot create extra tenants (store.Store gains tenant creation in M5)")
	}
	tb, err := tc.CreateTenant(context.Background(), "Tenant B", "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	b := &env{s: e.s, tenant: tb, ctx: tenancy.WithScope(context.Background(), tenancy.Scope{TenantID: tb.ID})}
	bChat := b.chat(t, "B's chat")
	bPart, err := b.s.UpsertParticipant(b.ctx, store.Participant{ChatID: bChat.ID, DisplayName: "B person"})
	if err != nil {
		t.Fatal(err)
	}
	bImp := b.imp(t, bChat.ID)
	b.insert(t, msg(bChat, bImp, 1, "secret of tenant b", "b1"))
	b.complete(t, bImp)
	bOpen := b.imp(t, bChat.ID) // still processing

	aChat := e.chat(t, "A's chat")
	aImp := e.imp(t, aChat.ID)

	// Every method, called with tenant A's scope against B's IDs, must act as
	// if the rows did not exist.
	_, err = e.s.GetChat(e.ctx, bChat.ID)
	wantErr(t, "GetChat", err, store.ErrNotFound)
	if list, _ := e.s.ListChats(e.ctx); len(list) != 1 || list[0].ID != aChat.ID {
		t.Errorf("ListChats leaked: %v", list)
	}
	wantErr(t, "DeleteChat", e.s.DeleteChat(e.ctx, bChat.ID), store.ErrNotFound)
	_, err = e.s.ListParticipants(e.ctx, bChat.ID)
	wantErr(t, "ListParticipants", err, store.ErrNotFound)
	_, err = e.s.UpsertParticipant(e.ctx, store.Participant{ChatID: bChat.ID, DisplayName: "intruder"})
	wantErr(t, "UpsertParticipant into B chat", err, store.ErrNotFound)
	_, err = e.s.UpsertParticipant(e.ctx, store.Participant{ID: bPart.ID, ChatID: aChat.ID, DisplayName: "hijack"})
	wantErr(t, "UpsertParticipant with B's participant ID", err, store.ErrNotFound)
	_, err = e.s.CreateImport(e.ctx, store.Import{ChatID: bChat.ID, Platform: "whatsapp"})
	wantErr(t, "CreateImport", err, store.ErrNotFound)
	_, err = e.s.GetImport(e.ctx, bImp.ID)
	wantErr(t, "GetImport", err, store.ErrNotFound)
	_, err = e.s.ListImports(e.ctx, bChat.ID)
	wantErr(t, "ListImports", err, store.ErrNotFound)
	_, err = e.s.FindCompletedImportBySHA256(e.ctx, bChat.ID, sum(bChat.ID.String()))
	wantErr(t, "FindCompletedImportBySHA256", err, store.ErrNotFound)
	_, err = e.s.InsertMessages(e.ctx, []store.Message{msg(bChat, bOpen, 2, "injected", "x1")})
	wantErr(t, "InsertMessages into B chat", err, store.ErrNotFound)
	_, err = e.s.InsertMessages(e.ctx, []store.Message{msg(aChat, aImp, 1, "ok", "a1"), msg(bChat, bOpen, 2, "injected", "x2")})
	wantErr(t, "InsertMessages mixed batch", err, store.ErrNotFound)
	_, err = e.s.MaxOrdinal(e.ctx, bChat.ID)
	wantErr(t, "MaxOrdinal", err, store.ErrNotFound)
	wantErr(t, "CompleteImport", e.s.CompleteImport(e.ctx, bOpen), store.ErrNotFound)
	wantErr(t, "FailImport", e.s.FailImport(e.ctx, bImp.ID, "x", nil), store.ErrNotFound)
	_, err = e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: bChat.ID})
	wantErr(t, "ListMessages", err, store.ErrNotFound)
	_, err = e.s.CountMessages(e.ctx, bChat.ID)
	wantErr(t, "CountMessages", err, store.ErrNotFound)
	_, err = e.s.SearchMessages(e.ctx, bChat.ID, "secret", 10)
	wantErr(t, "SearchMessages", err, store.ErrNotFound)

	// A's own data never includes B's rows, and B's data is intact.
	if max, _ := e.s.MaxOrdinal(e.ctx, aChat.ID); max != 0 {
		t.Errorf("mixed batch partially inserted (max ordinal %d)", max)
	}
	if n := b.count(t, bChat.ID); n != 1 {
		t.Errorf("B's messages changed: %d", n)
	}
	if got, err := b.s.GetImport(b.ctx, bImp.ID); err != nil || got.Status != store.ImportCompleted {
		t.Errorf("B's import changed: %+v %v", got, err)
	}
	if ps, _ := b.s.ListParticipants(b.ctx, bChat.ID); len(ps) != 1 || ps[0].DisplayName != "B person" {
		t.Errorf("B's participants changed: %+v", ps)
	}
	if got := b.search(t, bChat.ID, "secret"); len(got) != 1 {
		t.Errorf("B lost its own search results: %v", got)
	}
}

func testConcurrency(t *testing.T, e *env) {
	c := e.chat(t, "concurrent")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 4 {
		wg.Go(func() {
			for i := range 10 {
				im, err := e.s.CreateImport(e.ctx, store.Import{ChatID: c.ID, Platform: "whatsapp"})
				if err != nil {
					errs <- err
					return
				}
				m := msg(c, im, int64(w*100+i+1), "concurrent", fmt.Sprintf("w%d-%d", w, i))
				if _, err := e.s.InsertMessages(e.ctx, []store.Message{m}); err != nil {
					errs <- err
					return
				}
				if err := e.s.CompleteImport(e.ctx, im); err != nil {
					errs <- err
					return
				}
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := e.s.ListMessages(e.ctx, store.MessageQuery{ChatID: c.ID}); err != nil {
					errs <- err
					return
				}
				if _, err := e.s.ListChats(e.ctx); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := e.count(t, c.ID); n != 40 {
		t.Errorf("count = %d, want 40", n)
	}
}

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMaintainTrimsByAgeAndRowCap proves the request log is bounded: rows older
// than the cutoff and rows beyond the cap are deleted, while the hourly
// aggregates the panel charts are preserved. Before this the table grew
// without limit and the WAL was never checkpointed back down.
func TestMaintainTrimsByAgeAndRowCap(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	now := time.Now()
	// Three old rows (20 days) and five recent ones.
	for i := 0; i < 3; i++ {
		if err := st.InsertRequest(ctx, &RequestRow{
			Ts: now.AddDate(0, 0, -20), Method: "POST", Path: "/v1/messages",
			IncomingModel: "m", TargetModel: "t", Status: 200,
		}); err != nil {
			t.Fatalf("insert old: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := st.InsertRequest(ctx, &RequestRow{
			Ts: now.Add(-time.Duration(i) * time.Minute), Method: "POST", Path: "/v1/messages",
			IncomingModel: "m", TargetModel: "t", Status: 200,
		}); err != nil {
			t.Fatalf("insert recent: %v", err)
		}
	}

	if err := st.Maintain(ctx, now.AddDate(0, 0, -14), 4); err != nil {
		t.Fatalf("maintain: %v", err)
	}
	rows, err := st.ListRequests(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("kept %d rows, want the 4 most recent", len(rows))
	}

	// The aggregate survives the trim: 8 requests were inserted.
	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.TotalRequests != 8 {
		t.Fatalf("total requests = %d, want 8 (stats_hourly must not be trimmed)", sum.TotalRequests)
	}
}

func TestMaintainNoopArguments(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Maintain(context.Background(), time.Time{}, 0); err != nil {
		t.Fatalf("maintain with no limits: %v", err)
	}
	var nilStore *Store
	if err := nilStore.Maintain(context.Background(), time.Now(), 10); err != nil {
		t.Fatalf("nil store maintain: %v", err)
	}
}

// TestRedactSecretsCoversBotTokens keeps a Telegram token out of the persisted
// request log. It leaks through tool output and error bodies, not JSON fields.
func TestRedactSecretsCoversBotTokens(t *testing.T) {
	in := "token=123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw " +
		"https://api.telegram.org/bot987654321:BBHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage " +
		"\nOPENAI_API_KEY=hunter2\nand sk-abcdefghijklmnopqrstuvwx " +
		`{"output":"setup\nOPENAI_API_KEY=hunter3\nGITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyz"}`
	got := redactSecrets(in)
	for _, leaked := range []string{
		"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw",
		"987654321:BBHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw",
		"hunter2",
		"hunter3",
		"ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"sk-abcdefghijklmnopqrstuvwx",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redactSecrets left %q in %q", leaked, got)
		}
	}
}

// TestRedactSecretsCoversZenKeys keeps the keys this deployment actually uses
// (oc_sk_...) out of the persisted log. The old pattern only matched sk-.
func TestRedactSecretsCoversZenKeys(t *testing.T) {
	in := "authorization: Bearer oc_sk_a6b7c8d9e0f1a2b3c4d5 and sk_proj_0123456789abcdef and sk-abcdefghijklmnopqrstuvwx"
	got := redactSecrets(in)
	for _, leaked := range []string{
		"oc_sk_a6b7c8d9e0f1a2b3c4d5",
		"sk_proj_0123456789abcdef",
		"sk-abcdefghijklmnopqrstuvwx",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redactSecrets left %q in %q", leaked, got)
		}
	}
}

func TestMetaIntRoundTrip(t *testing.T) {
	path := t.TempDir() + "/test.db"
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, ok := st.MetaInt(ctx, "missing"); ok {
		t.Fatal("missing key must not resolve")
	}
	if err := st.SetMetaInt(ctx, "last_vacuum_ms", 12345); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	if got, ok := st.MetaInt(ctx, "last_vacuum_ms"); !ok || got != 12345 {
		t.Fatalf("meta = %d/%v, want 12345/true", got, ok)
	}
	if err := st.SetMetaInt(ctx, "last_vacuum_ms", 67890); err != nil {
		t.Fatalf("overwrite meta: %v", err)
	}
	_ = st.Close()

	// The value must survive a restart: process uptime is not a schedule.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if got, ok := st2.MetaInt(ctx, "last_vacuum_ms"); !ok || got != 67890 {
		t.Fatalf("persisted meta = %d/%v, want 67890/true", got, ok)
	}
}

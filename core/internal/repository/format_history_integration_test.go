package repository

import (
	"context"
	"testing"
)

func TestIntegration_FormatHistory_RecordListDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `TRUNCATE format_history RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	repo := NewFormatHistoryRepository(db)

	// First record inserts with use_count 1.
	const fmtStr = "host:port:*:user:pass"
	if err := repo.Record(ctx, fmtStr); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Recording the same format again bumps the count instead of duplicating.
	if err := repo.Record(ctx, fmtStr); err != nil {
		t.Fatalf("record again: %v", err)
	}
	if err := repo.Record(ctx, "user|pass|host|port"); err != nil {
		t.Fatalf("record other: %v", err)
	}

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	var found *int
	for i := range entries {
		if entries[i].Format == fmtStr {
			if entries[i].UseCount != 2 {
				t.Errorf("use_count = %d, want 2", entries[i].UseCount)
			}
			id := entries[i].ID
			found = &id
		}
	}
	if found == nil {
		t.Fatal("recorded format not returned by List")
	}

	if err := repo.Delete(ctx, *found); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(entries) != 1 || entries[0].Format != "user|pass|host|port" {
		t.Fatalf("after delete got %+v, want only the pipe format", entries)
	}
}

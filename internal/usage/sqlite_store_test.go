package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestSQLiteStore_BasicOperations(t *testing.T) {
	// Create temp directory for test database
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_usage.db")

	// Create store
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Ensure schema
	ctx := context.Background()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema failed: %v", err)
	}

	// Insert a test record
	testDetail := RequestDetail{
		Timestamp: time.Now(),
		Source:    "test-source",
		AuthIndex: "0",
		Tokens: TokenStats{
			InputTokens:     100,
			OutputTokens:    50,
			ReasoningTokens: 0,
			CachedTokens:    25,
			TotalTokens:     150,
		},
		Failed: false,
	}

	err = store.InsertRecord(ctx, "test-api", "test-model", testDetail)
	if err != nil {
		t.Fatalf("InsertRecord failed: %v", err)
	}

	// Load all records
	records, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	// Verify loaded data
	if len(records) != 1 {
		t.Fatalf("Expected 1 API in records, got %d", len(records))
	}

	apiRecords, ok := records["test-api"]
	if !ok {
		t.Fatal("Expected 'test-api' in records")
	}

	modelRecords, ok := apiRecords["test-model"]
	if !ok {
		t.Fatal("Expected 'test-model' in API records")
	}

	if len(modelRecords) != 1 {
		t.Fatalf("Expected 1 record, got %d", len(modelRecords))
	}

	loaded := modelRecords[0]
	if loaded.Source != testDetail.Source {
		t.Errorf("Source mismatch: expected %q, got %q", testDetail.Source, loaded.Source)
	}
	if loaded.AuthIndex != testDetail.AuthIndex {
		t.Errorf("AuthIndex mismatch: expected %q, got %q", testDetail.AuthIndex, loaded.AuthIndex)
	}
	if loaded.Tokens.InputTokens != testDetail.Tokens.InputTokens {
		t.Errorf("InputTokens mismatch: expected %d, got %d", testDetail.Tokens.InputTokens, loaded.Tokens.InputTokens)
	}
	if loaded.Tokens.TotalTokens != testDetail.Tokens.TotalTokens {
		t.Errorf("TotalTokens mismatch: expected %d, got %d", testDetail.Tokens.TotalTokens, loaded.Tokens.TotalTokens)
	}
	if loaded.Failed != testDetail.Failed {
		t.Errorf("Failed mismatch: expected %v, got %v", testDetail.Failed, loaded.Failed)
	}
}

func TestEnablePersistence_LoadsExistingData(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_usage.db")

	// First, create some data
	{
		store, err := NewSQLiteStore(dbPath)
		if err != nil {
			t.Fatalf("NewSQLiteStore failed: %v", err)
		}

		ctx := context.Background()
		if err := store.EnsureSchema(ctx); err != nil {
			store.Close()
			t.Fatalf("EnsureSchema failed: %v", err)
		}

		// Insert test records
		for i := 0; i < 3; i++ {
			detail := RequestDetail{
				Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
				Source:    "test-source",
				AuthIndex: "0",
				Tokens: TokenStats{
					InputTokens:  int64(100 * (i + 1)),
					OutputTokens: int64(50 * (i + 1)),
					TotalTokens:  int64(150 * (i + 1)),
				},
				Failed: false,
			}
			err := store.InsertRecord(ctx, "test-api", "test-model", detail)
			if err != nil {
				store.Close()
				t.Fatalf("InsertRecord failed: %v", err)
			}
		}
		store.Close()
	}

	// Reset statistics to clean state
	defaultRequestStatistics = NewRequestStatistics()

	// Enable persistence (should load the data)
	SetStatisticsEnabled(true)
	err := EnablePersistence(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	defer DisablePersistence()

	// Verify the data was loaded
	stats := GetRequestStatistics()
	snapshot := stats.Snapshot()

	if snapshot.TotalRequests != 3 {
		t.Errorf("Expected 3 total requests, got %d", snapshot.TotalRequests)
	}

	if snapshot.TotalTokens != 450+300+150 {
		t.Errorf("Expected total tokens %d, got %d", 450+300+150, snapshot.TotalTokens)
	}
}

func TestSQLitePersistencePlugin_HandlesUsage(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_usage.db")

	// Create store and plugin
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema failed: %v", err)
	}

	plugin := NewSQLitePersistencePlugin(store)
	defer plugin.Stop()

	// Simulate usage record
	record := coreusage.Record{
		Provider:    "test-provider",
		Model:       "test-model",
		APIKey:      "test-api-key",
		Source:      "test-source",
		AuthIndex:   "0",
		RequestedAt: time.Now(),
		Failed:      false,
		Detail: coreusage.Detail{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}

	// Send record to plugin
	plugin.HandleUsage(ctx, record)

	// Give it a moment to process (async)
	time.Sleep(100 * time.Millisecond)

	// Verify record was persisted
	records, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("Expected records to be persisted")
	}

	apiRecords, ok := records["test-api-key"]
	if !ok {
		t.Fatal("Expected 'test-api-key' in records")
	}

	modelRecords, ok := apiRecords["test-model"]
	if !ok {
		t.Fatal("Expected 'test-model' in API records")
	}

	if len(modelRecords) == 0 {
		t.Fatal("Expected at least one record")
	}
}

func TestSQLiteStore_FileCreation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	// Verify file was created
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("Database file was not created")
	}
}

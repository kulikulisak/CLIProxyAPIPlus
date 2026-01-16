package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// SQLiteStore handles SQLite persistence for usage records.
type SQLiteStore struct {
	db     *sql.DB
	dbPath string
	mu     sync.Mutex
}

// NewSQLiteStore opens or creates the SQLite database at the given path.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("usage sqlite: open database: %w", err)
	}

	// Set reasonable connection limits for embedded use
	db.SetMaxOpenConns(1) // SQLite doesn't benefit from multiple writers
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage sqlite: ping database: %w", err)
	}

	return &SQLiteStore{
		db:     db,
		dbPath: dbPath,
	}, nil
}

// EnsureSchema creates the usage_records table and indexes if they don't exist.
func (s *SQLiteStore) EnsureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create table
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS usage_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			api_name TEXT NOT NULL,
			model_name TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			auth_index TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		return fmt.Errorf("usage sqlite: create table: %w", err)
	}

	// Create indexes
	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_usage_timestamp ON usage_records(timestamp)
	`)
	if err != nil {
		return fmt.Errorf("usage sqlite: create timestamp index: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_usage_api_model ON usage_records(api_name, model_name)
	`)
	if err != nil {
		return fmt.Errorf("usage sqlite: create api_model index: %w", err)
	}

	return nil
}

// InsertRecord persists a single usage record to the database.
func (s *SQLiteStore) InsertRecord(ctx context.Context, apiName, modelName string, detail RequestDetail) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_records
		(api_name, model_name, timestamp, source, auth_index,
		 input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, failed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		apiName, modelName, detail.Timestamp.Format(time.RFC3339Nano),
		detail.Source, detail.AuthIndex,
		detail.Tokens.InputTokens, detail.Tokens.OutputTokens,
		detail.Tokens.ReasoningTokens, detail.Tokens.CachedTokens,
		detail.Tokens.TotalTokens,
		boolToInt(detail.Failed),
	)
	if err != nil {
		log.WithError(err).Warn("usage: failed to persist record to SQLite")
	}
	return err
}

// LoadAll loads all persisted records and returns them grouped by API and model.
func (s *SQLiteStore) LoadAll(ctx context.Context) (map[string]map[string][]RequestDetail, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT api_name, model_name, timestamp, source, auth_index,
		       input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, failed
		FROM usage_records
		ORDER BY timestamp
	`)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite: query records: %w", err)
	}
	defer rows.Close()

	result := make(map[string]map[string][]RequestDetail)

	for rows.Next() {
		var (
			apiName, modelName, timestampStr, source, authIndex string
			inputTokens, outputTokens, reasoningTokens          int64
			cachedTokens, totalTokens                           int64
			failed                                              int
		)

		err := rows.Scan(
			&apiName, &modelName, &timestampStr, &source, &authIndex,
			&inputTokens, &outputTokens, &reasoningTokens, &cachedTokens, &totalTokens,
			&failed,
		)
		if err != nil {
			return nil, fmt.Errorf("usage sqlite: scan row: %w", err)
		}

		timestamp, err := time.Parse(time.RFC3339Nano, timestampStr)
		if err != nil {
			log.WithError(err).Warnf("usage sqlite: invalid timestamp %q, skipping record", timestampStr)
			continue
		}

		detail := RequestDetail{
			Timestamp: timestamp,
			Source:    source,
			AuthIndex: authIndex,
			Tokens: TokenStats{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				ReasoningTokens: reasoningTokens,
				CachedTokens:    cachedTokens,
				TotalTokens:     totalTokens,
			},
			Failed: failed != 0,
		}

		if result[apiName] == nil {
			result[apiName] = make(map[string][]RequestDetail)
		}
		result[apiName][modelName] = append(result[apiName][modelName], detail)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite: iterate rows: %w", err)
	}

	return result, nil
}

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// boolToInt converts a boolean to an integer (0 or 1) for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// writeRequest represents a queued write operation.
type writeRequest struct {
	apiName   string
	modelName string
	detail    RequestDetail
}

// SQLitePersistencePlugin implements coreusage.Plugin to persist records to SQLite.
type SQLitePersistencePlugin struct {
	store   *SQLiteStore
	writeCh chan writeRequest
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewSQLitePersistencePlugin creates a new persistence plugin with async writer.
func NewSQLitePersistencePlugin(store *SQLiteStore) *SQLitePersistencePlugin {
	ctx, cancel := context.WithCancel(context.Background())
	plugin := &SQLitePersistencePlugin{
		store:   store,
		writeCh: make(chan writeRequest, 1000), // Buffer up to 1000 records
		ctx:     ctx,
		cancel:  cancel,
	}

	// Start background writer
	plugin.wg.Add(1)
	go plugin.runWriter()

	return plugin
}

// HandleUsage implements coreusage.Plugin.
// It queues the usage record for async persistence.
func (p *SQLitePersistencePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	// Derive API name from APIKey (same logic as LoggerPlugin)
	apiName := record.APIKey
	if apiName == "" {
		apiName = "unknown"
	}

	detail := RequestDetail{
		Timestamp: record.RequestedAt,
		Source:    record.Source,
		AuthIndex: record.AuthIndex,
		Tokens: TokenStats{
			InputTokens:     record.Detail.InputTokens,
			OutputTokens:    record.Detail.OutputTokens,
			ReasoningTokens: record.Detail.ReasoningTokens,
			CachedTokens:    record.Detail.CachedTokens,
			TotalTokens:     record.Detail.TotalTokens,
		},
		Failed: record.Failed,
	}

	// Non-blocking send to write channel
	select {
	case p.writeCh <- writeRequest{
		apiName:   apiName,
		modelName: record.Model,
		detail:    detail,
	}:
	default:
		log.Warn("usage persistence: queue full, record dropped")
	}
}

// runWriter processes the write queue in the background.
func (p *SQLitePersistencePlugin) runWriter() {
	defer p.wg.Done()

	for {
		select {
		case req := <-p.writeCh:
			_ = p.store.InsertRecord(p.ctx, req.apiName, req.modelName, req.detail)
		case <-p.ctx.Done():
			// Drain remaining items before exit
			for len(p.writeCh) > 0 {
				req := <-p.writeCh
				_ = p.store.InsertRecord(context.Background(), req.apiName, req.modelName, req.detail)
			}
			return
		}
	}
}

// Stop gracefully shuts down the plugin and drains the queue.
func (p *SQLitePersistencePlugin) Stop() {
	p.cancel()
	p.wg.Wait()
}

// Global persistence plugin instance
var (
	persistencePlugin *SQLitePersistencePlugin
	persistenceMu     sync.Mutex
)

// EnablePersistence initializes SQLite persistence and loads existing data.
// It must be called after SetStatisticsEnabled(true) and before the server starts.
func EnablePersistence(ctx context.Context, dbPath string) error {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()

	if persistencePlugin != nil {
		return fmt.Errorf("usage persistence: already enabled")
	}

	// Open database
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		return err
	}

	// Ensure schema exists
	if err := store.EnsureSchema(ctx); err != nil {
		store.Close()
		return err
	}

	// Load existing records
	records, err := store.LoadAll(ctx)
	if err != nil {
		store.Close()
		return err
	}

	// Merge into in-memory statistics
	stats := GetRequestStatistics()
	recordCount := 0
	for apiName, models := range records {
		for modelName, details := range models {
			for _, detail := range details {
				// Use the internal record method to populate statistics
				stats.recordDetail(apiName, modelName, detail)
				recordCount++
			}
		}
	}

	log.Infof("usage persistence: loaded %d records from database", recordCount)

	// Create and register persistence plugin
	persistencePlugin = NewSQLitePersistencePlugin(store)
	coreusage.RegisterPlugin(persistencePlugin)

	return nil
}

// DisablePersistence stops the persistence plugin and closes the database.
func DisablePersistence() {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()

	if persistencePlugin != nil {
		persistencePlugin.Stop()
		persistencePlugin.store.Close()
		persistencePlugin = nil
	}
}

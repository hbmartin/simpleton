package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db          *sql.DB
	objectsPath string
}

func Open(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	objects := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		return nil, err
	}
	databaseURL := sqliteFileURL(filepath.Join(root, "simpleton.db"))
	query := databaseURL.Query()
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	databaseURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", databaseURL.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, objectsPath: objects}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteFileURL(filename string) url.URL {
	slashed := filepath.ToSlash(filename)
	if len(filename) >= 3 && ((filename[0] >= 'A' && filename[0] <= 'Z') || (filename[0] >= 'a' && filename[0] <= 'z')) &&
		filename[1] == ':' && (filename[2] == '\\' || filename[2] == '/') {
		return url.URL{Scheme: "file", Path: "/" + strings.ReplaceAll(filename, "\\", "/")}
	}
	if strings.HasPrefix(filename, `\\`) || strings.HasPrefix(filename, "//") {
		unc := strings.TrimPrefix(strings.ReplaceAll(filename, "\\", "/"), "//")
		host, path, found := strings.Cut(unc, "/")
		if found && host != "" {
			return url.URL{Scheme: "file", Host: host, Path: "/" + path}
		}
	}
	return url.URL{Scheme: "file", Path: slashed}
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PutObject(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(s.objectsPath, digest[:2])
	path := filepath.Join(dir, digest[2:])
	if _, err := os.Stat(path); err == nil {
		return digest, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".object-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return digest, nil
		}
		return "", err
	}
	return digest, nil
}

func (s *Store) ObjectPath(digest string) (string, error) {
	if len(digest) != 64 {
		return "", errors.New("invalid SHA-256 digest")
	}
	return filepath.Join(s.objectsPath, digest[:2], digest[2:]), nil
}

func (s *Store) SaveEvidence(ctx context.Context, pack domain.EvidencePack) (string, error) {
	data, err := json.Marshal(pack)
	if err != nil {
		return "", err
	}
	digest, err := s.PutObject(data)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs(run_id, created_at, repository, base_revision, head_revision, semantic_advisory, evidence_digest, blocker_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			created_at=excluded.created_at,
			repository=excluded.repository,
			base_revision=excluded.base_revision,
			head_revision=excluded.head_revision,
			semantic_advisory=excluded.semantic_advisory,
			evidence_digest=excluded.evidence_digest,
			blocker_count=excluded.blocker_count`,
		pack.RunID, pack.CreatedAt.UTC().Format(time.RFC3339Nano), pack.Provenance.Repository,
		pack.Provenance.BaseRevision, pack.Provenance.HeadRevision, pack.SemanticAdvisory, digest, len(pack.Blockers),
	)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM method_results WHERE run_id = ?`, pack.RunID); err != nil {
		return "", err
	}
	for sequence, method := range pack.Methods {
		payload, marshalErr := json.Marshal(method)
		if marshalErr != nil {
			return "", marshalErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO method_results(run_id, method_id, sequence, language, status, duration_ms, budget, cost_usd, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			pack.RunID, method.ID, sequence, method.Language, method.Status, method.DurationMS, method.Budget, method.CostUSD, payload,
		); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM opportunities WHERE run_id = ?`, pack.RunID); err != nil {
		return "", err
	}
	for _, opportunity := range pack.Scoping.Opportunities {
		payload, marshalErr := json.Marshal(opportunity)
		if marshalErr != nil {
			return "", marshalErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO opportunities(run_id, opportunity_id, rank, payload) VALUES (?, ?, ?, ?)`,
			pack.RunID, opportunity.ID, opportunity.Rank, payload,
		); err != nil {
			return "", err
		}
	}
	reviewPayload, err := json.Marshal(pack.AdvisoryReview)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO managed_reviews(run_id, status, model_id, prompt_id, duration_ms, cost_usd, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET status=excluded.status, model_id=excluded.model_id,
			prompt_id=excluded.prompt_id, duration_ms=excluded.duration_ms, cost_usd=excluded.cost_usd, payload=excluded.payload`,
		pack.RunID, pack.AdvisoryReview.Status, pack.AdvisoryReview.ModelID, pack.AdvisoryReview.PromptID,
		pack.AdvisoryReview.DurationMS, pack.AdvisoryReview.CostUSD, reviewPayload,
	); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return digest, nil
}

type Outcome struct {
	RunID             string    `json:"run_id"`
	ReviewerAction    string    `json:"reviewer_action,omitempty"`
	Accepted          *bool     `json:"accepted,omitempty"`
	Reverted          *bool     `json:"reverted,omitempty"`
	ReviewCycles      *int      `json:"review_cycles,omitempty"`
	ReviewMinutes     *int      `json:"review_minutes,omitempty"`
	EscapedRegression *bool     `json:"escaped_regression,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
}

func (s *Store) RecordOutcome(ctx context.Context, outcome Outcome) error {
	if outcome.RunID == "" {
		return errors.New("run_id is required")
	}
	if outcome.RecordedAt.IsZero() {
		outcome.RecordedAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO outcomes(run_id, recorded_at, payload) VALUES (?, ?, ?)`,
		outcome.RunID, outcome.RecordedAt.Format(time.RFC3339Nano), encoded)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("outcome for run %q was not recorded", outcome.RunID)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			repository TEXT NOT NULL,
			base_revision TEXT NOT NULL,
			head_revision TEXT NOT NULL,
			semantic_advisory INTEGER NOT NULL,
			evidence_digest TEXT NOT NULL,
			blocker_count INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			recorded_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			FOREIGN KEY(run_id) REFERENCES runs(run_id)
		);
		CREATE TABLE IF NOT EXISTS method_results (
			run_id TEXT NOT NULL,
			method_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			language TEXT NOT NULL,
			status TEXT NOT NULL,
			duration_ms INTEGER NOT NULL,
			budget TEXT NOT NULL,
			cost_usd REAL,
			payload BLOB NOT NULL,
			PRIMARY KEY(run_id, method_id, sequence),
			FOREIGN KEY(run_id) REFERENCES runs(run_id)
		);
		CREATE TABLE IF NOT EXISTS opportunities (
			run_id TEXT NOT NULL,
			opportunity_id TEXT NOT NULL,
			rank REAL NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY(run_id, opportunity_id),
			FOREIGN KEY(run_id) REFERENCES runs(run_id)
		);
		CREATE TABLE IF NOT EXISTS managed_reviews (
			run_id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			model_id TEXT NOT NULL,
			prompt_id TEXT NOT NULL,
			duration_ms INTEGER NOT NULL,
			cost_usd REAL,
			payload BLOB NOT NULL,
			FOREIGN KEY(run_id) REFERENCES runs(run_id)
		);
		CREATE INDEX IF NOT EXISTS outcomes_run_id ON outcomes(run_id);
		CREATE INDEX IF NOT EXISTS method_results_status ON method_results(status);
		CREATE INDEX IF NOT EXISTS opportunities_rank ON opportunities(rank DESC);
	`)
	return err
}

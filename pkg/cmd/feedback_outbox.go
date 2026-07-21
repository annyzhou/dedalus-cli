package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

const (
	feedbackOutboxVersion  = 1
	feedbackOutboxMaxFile  = 2 << 20
	feedbackOutboxMaxItems = 100
)

type feedbackPendingRequest struct {
	Version         int       `json:"version"`
	IdempotencyKey  string    `json:"idempotency_key"`
	Endpoint        string    `json:"endpoint"`
	AuthFingerprint string    `json:"auth_fingerprint"`
	Message         string    `json:"message"`
	ContentType     string    `json:"content_type"`
	Body            []byte    `json:"body"`
	CreatedAt       time.Time `json:"created_at"`
	path            string
}

func feedbackOutboxDirectory() (string, error) {
	if override := os.Getenv("DEDALUS_FEEDBACK_OUTBOX_DIR"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find feedback outbox home: %w", err)
	}
	return filepath.Join(home, ".dedalus", "feedback"), nil
}

func feedbackAuthFingerprint(command *cli.Command) string {
	root := command.Root()
	value := strings.Join([]string{
		root.String("api-key"),
		root.String("x-api-key"),
		root.String("dedalus-org-id"),
	}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func loadFeedbackOutbox() ([]feedbackPendingRequest, error) {
	dir, err := feedbackOutboxDirectory()
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect feedback outbox: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("feedback outbox is not a private directory: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read feedback outbox: %w", err)
	}
	pending := make([]feedbackPendingRequest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if len(pending) >= feedbackOutboxMaxItems {
			return nil, fmt.Errorf("feedback outbox contains more than %d pending reports", feedbackOutboxMaxItems)
		}
		path := filepath.Join(dir, entry.Name())
		item, readErr := readFeedbackPending(path)
		if readErr != nil {
			return nil, readErr
		}
		pending = append(pending, item)
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].IdempotencyKey < pending[j].IdempotencyKey
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	return pending, nil
}

func readFeedbackPending(path string) (feedbackPendingRequest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("inspect pending feedback: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > feedbackOutboxMaxFile {
		return feedbackPendingRequest{}, fmt.Errorf("pending feedback is not a valid private file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return feedbackPendingRequest{}, fmt.Errorf("pending feedback permissions are too broad: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("open pending feedback: %w", err)
	}
	defer file.Close()
	var pending feedbackPendingRequest
	decoder := json.NewDecoder(io.LimitReader(file, feedbackOutboxMaxFile+1))
	if err := decoder.Decode(&pending); err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("decode pending feedback: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return feedbackPendingRequest{}, fmt.Errorf("pending feedback has trailing data: %s", path)
	}
	if pending.Version != feedbackOutboxVersion ||
		!feedbackRequestID.MatchString(pending.IdempotencyKey) ||
		pending.Endpoint == "" || pending.AuthFingerprint == "" ||
		pending.Message == "" || pending.ContentType == "" || len(pending.Body) == 0 ||
		pending.CreatedAt.IsZero() {
		return feedbackPendingRequest{}, fmt.Errorf("pending feedback has an unsupported format: %s", path)
	}
	pending.path = path
	return pending, nil
}

func saveFeedbackPending(pending feedbackPendingRequest) (feedbackPendingRequest, error) {
	dir, err := feedbackOutboxDirectory()
	if err != nil {
		return feedbackPendingRequest{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("create feedback outbox: %w", err)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("inspect feedback outbox: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return feedbackPendingRequest{}, fmt.Errorf("feedback outbox is not a directory: %s", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("secure feedback outbox: %w", err)
	}
	target := filepath.Join(dir, pending.IdempotencyKey+".json")
	temporary, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("create pending feedback: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return feedbackPendingRequest{}, fmt.Errorf("secure pending feedback: %w", err)
	}
	if err := json.NewEncoder(temporary).Encode(pending); err != nil {
		temporary.Close()
		return feedbackPendingRequest{}, fmt.Errorf("encode pending feedback: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return feedbackPendingRequest{}, fmt.Errorf("sync pending feedback: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("close pending feedback: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return feedbackPendingRequest{}, fmt.Errorf("commit pending feedback: %w", err)
	}
	pending.path = target
	return pending, nil
}

func removeFeedbackPending(pending feedbackPendingRequest) error {
	if err := os.Remove(pending.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove delivered feedback from outbox: %w", err)
	}
	return nil
}

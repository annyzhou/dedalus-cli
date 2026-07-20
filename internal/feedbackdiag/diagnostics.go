package feedbackdiag

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dedalus-labs/dedalus-go/option"
	"github.com/google/uuid"
)

const (
	maxFileBytes = 10 << 20
	maxRows      = 1_000
	maxDirBytes  = 50 << 20
	retention    = 10 * 24 * time.Hour
	receiptAge   = 24 * time.Hour
)

// Event is the closed set of fields written to the local feedback log.
type Event struct {
	Time        time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Level       string    `json:"level"`
	Target      string    `json:"target"`
	Command     string    `json:"command,omitempty"`
	Route       string    `json:"route,omitempty"`
	MachineID   string    `json:"machine_id,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	ProcessUUID string    `json:"process_uuid"`
	StatusCode  int       `json:"status_code,omitempty"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
}

// Failure is the most recent failed request receipt and its bounded log file.
type Failure struct {
	Event
	Path string
}

// Session owns one append-only log partition for one CLI process.
type Session struct {
	mu    sync.Mutex
	file  *os.File
	path  string
	rows  int
	bytes int64
	uuid  string
	now   func() time.Time
}

// Directory returns the private directory that stores first-party diagnostics.
func Directory() (string, error) {
	if configured := os.Getenv("DEDALUS_DEBUG_DIR"); configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".dedalus", "debug"), nil
}

// Open creates one private append-only partition after enforcing retention caps.
func Open(dir string) (*Session, error) {
	now := time.Now
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create debug directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure debug directory: %w", err)
	}
	if err := sweep(dir, now()); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.jsonl", now().UnixNano(), id))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open feedback log: %w", err)
	}
	return &Session{file: file, path: path, uuid: id, now: now}, nil
}

// Close releases the process partition.
func (s *Session) Close() error {
	return s.file.Close()
}

// Middleware records request lifecycle facts without headers, bodies, or errors.
func (s *Session) Middleware() option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		command := req.Header.Get("X-Dedalus-CLI-Command")
		route, machineID := machineRoute(req.URL.Path)
		started := s.now()
		if err := s.write(Event{
			Time: started, Kind: "request_started", Level: "info", Target: "http",
			Command: command, Route: route, MachineID: machineID, ProcessUUID: s.uuid,
		}); err != nil {
			return nil, err
		}

		resp, err := next(req)
		event := Event{
			Time: s.now(), Kind: "request_completed", Level: "info", Target: "http",
			Command: command, Route: route, MachineID: machineID, ProcessUUID: s.uuid,
		}
		event.DurationMS = max(0, event.Time.Sub(started).Milliseconds())
		if resp != nil {
			event.StatusCode = resp.StatusCode
			event.RequestID = resp.Header.Get("X-Request-ID")
		}
		if err != nil || event.StatusCode >= http.StatusBadRequest {
			event.Kind = "request_failed"
			event.Level = "error"
		}
		if writeErr := s.write(event); writeErr != nil {
			return resp, writeErr
		}
		return resp, err
	}
}

func machineRoute(path string) (route, machineID string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" || parts[1] != "machines" {
		return "", ""
	}
	if len(parts) == 2 {
		return "/v1/machines", ""
	}
	machineID = parts[2]
	parts[2] = "{machine_id}"
	identifiers := map[string]string{
		"artifacts":  "{artifact_id}",
		"executions": "{execution_id}",
		"previews":   "{preview_id}",
		"ssh":        "{session_id}",
		"terminals":  "{terminal_id}",
	}
	for index := 3; index+1 < len(parts); index++ {
		if placeholder, ok := identifiers[parts[index]]; ok {
			parts[index+1] = placeholder
			index++
		}
	}
	return "/" + strings.Join(parts, "/"), machineID
}

func (s *Session) write(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows >= maxRows || s.bytes >= maxFileBytes {
		return nil
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode feedback log event: %w", err)
	}
	line = append(line, '\n')
	if s.bytes+int64(len(line)) > maxFileBytes {
		return nil
	}
	n, err := s.file.Write(line)
	if err != nil {
		return fmt.Errorf("write feedback log event: %w", err)
	}
	s.rows++
	s.bytes += int64(n)
	return nil
}

var defaultSession struct {
	sync.Once
	session *Session
	err     error
}

// Middleware records receipts in the process-default diagnostics partition.
func Middleware() option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		defaultSession.Do(func() {
			dir, err := Directory()
			if err != nil {
				defaultSession.err = err
				return
			}
			defaultSession.session, defaultSession.err = Open(dir)
		})
		if defaultSession.err != nil {
			return nil, fmt.Errorf("initialize feedback diagnostics: %w", defaultSession.err)
		}
		return defaultSession.session.Middleware()(req, next)
	}
}

// LatestFailure returns the newest failed request from the last 24 hours.
func LatestFailure(dir string, now time.Time) (*Failure, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read debug directory: %w", err)
	}
	var latest *Failure
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		failure, readErr := latestFailureInFile(path, now.Add(-receiptAge))
		if readErr != nil {
			return nil, readErr
		}
		if failure != nil && (latest == nil || failure.Time.After(latest.Time)) {
			latest = failure
		}
	}
	return latest, nil
}

func latestFailureInFile(path string, cutoff time.Time) (*Failure, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feedback log: %w", err)
	}
	defer file.Close()

	var latest *Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse feedback log %s: %w", path, err)
		}
		if event.Time.Before(cutoff) || (event.Kind != "request_failed" && event.Kind != "request_completed") {
			continue
		}
		if latest == nil || event.Time.After(latest.Time) {
			latest = &event
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan feedback log %s: %w", path, err)
	}
	if latest == nil || latest.Kind != "request_failed" {
		return nil, nil
	}
	return &Failure{Event: *latest, Path: path}, nil
}

func sweep(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read debug directory: %w", err)
	}
	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	files := make([]fileInfo, 0, len(entries))
	var total int64
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect debug artifact: %w", infoErr)
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() || now.Sub(info.ModTime()) > retention {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove expired debug artifact: %w", err)
			}
			continue
		}
		files = append(files, fileInfo{path: path, modTime: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= maxDirBytes {
			break
		}
		if err := os.Remove(file.path); err != nil {
			return fmt.Errorf("evict debug artifact: %w", err)
		}
		total -= file.size
	}
	return nil
}

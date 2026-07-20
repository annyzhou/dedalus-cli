package cmd

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dedalus-labs/dedalus-cli/internal/feedbackdiag"
)

const (
	feedbackUploadCap       = 1 << 20
	feedbackDecompressedCap = 10 << 20
	feedbackCandidateAge    = 24 * time.Hour
)

var feedbackArtifactFormats = map[string]string{
	"dedalus-feedback.log":                 "text/plain",
	"dedalus-doctor-report.json":           "application/json",
	"dedalus-connectivity-diagnostics.txt": "text/plain",
	"dedalus-operation.jsonl.gz":           "application/gzip",
}

var feedbackArtifactOrder = []string{
	"dedalus-operation.jsonl.gz",
	"dedalus-doctor-report.json",
	"dedalus-connectivity-diagnostics.txt",
}

type feedbackAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

type feedbackManifestFile struct {
	Filename  string `json:"filename"`
	Format    string `json:"format"`
	SHA256    string `json:"sha256"`
	SizeBytes int    `json:"size_bytes"`
}

func (a feedbackAttachment) manifest() feedbackManifestFile {
	return feedbackManifestFile{
		Filename: a.Filename, Format: a.ContentType,
		SHA256: fmt.Sprintf("%x", sha256.Sum256(a.Data)), SizeBytes: len(a.Data),
	}
}

func selectFeedbackBundle(mode string, now time.Time) ([]feedbackAttachment, *feedbackdiag.Failure, error) {
	dir, err := feedbackdiag.Directory()
	if err != nil {
		return nil, nil, err
	}
	failure, err := feedbackdiag.LatestFailure(dir, now)
	if err != nil {
		return nil, nil, err
	}
	if mode == "false" {
		return nil, failure, nil
	}

	attachments := make([]feedbackAttachment, 0, 4)
	remaining := feedbackUploadCap
	logPath := ""
	if failure != nil {
		logPath = failure.Path
	} else if mode == "true" {
		logPath, err = newestInvocationLog(dir)
		if err != nil {
			return nil, nil, err
		}
	}
	if logPath != "" {
		data, readErr := readLogTail(logPath, remaining)
		if readErr != nil {
			return nil, nil, readErr
		}
		if len(data) > 0 {
			attachments = append(attachments, feedbackAttachment{
				Filename: "dedalus-feedback.log", ContentType: "text/plain", Data: data,
			})
			remaining -= len(data)
		}
	}

	cutoff := now.Add(-feedbackCandidateAge)
	for _, name := range feedbackArtifactOrder {
		attachment, readErr := readExplicitArtifact(dir, name, remaining, cutoff, mode == "true")
		if readErr != nil {
			return nil, nil, readErr
		}
		if attachment == nil {
			continue
		}
		attachments = append(attachments, *attachment)
		remaining -= len(attachment.Data)
	}
	if mode == "true" && len(attachments) == 0 {
		return nil, failure, fmt.Errorf("no first-party debug artifacts are available; rerun the failed command, then retry")
	}
	return attachments, failure, nil
}

func newestInvocationLog(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read debug directory: %w", err)
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	logs := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return "", fmt.Errorf("inspect feedback log: %w", infoErr)
		}
		logs = append(logs, candidate{filepath.Join(dir, entry.Name()), info.ModTime()})
	}
	if len(logs) == 0 {
		return "", nil
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mod.After(logs[j].mod) })
	return logs[0].path, nil
}

func readLogTail(path string, budget int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feedback log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect feedback log: %w", err)
	}
	offset := max(int64(0), info.Size()-int64(budget))
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek feedback log: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(budget)))
	if err != nil {
		return nil, fmt.Errorf("read feedback log: %w", err)
	}
	if offset > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	return data, nil
}

func readExplicitArtifact(dir, name string, budget int, cutoff time.Time, includeOld bool) (*feedbackAttachment, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular first-party debug artifact", name)
	}
	if !includeOld && info.ModTime().Before(cutoff) {
		return nil, nil
	}
	if info.Size() <= 0 || info.Size() > int64(budget) {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if name == "dedalus-operation.jsonl.gz" {
		if err := validateSingleGZIPMember(data); err != nil {
			return nil, err
		}
	}
	return &feedbackAttachment{Filename: name, ContentType: feedbackArtifactFormats[name], Data: data}, nil
}

func validateSingleGZIPMember(data []byte) error {
	reader := bytes.NewReader(data)
	compressed, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("dedalus-operation.jsonl.gz is invalid: %w", err)
	}
	compressed.Multistream(false)
	written, copyErr := io.CopyN(io.Discard, compressed, feedbackDecompressedCap+1)
	closeErr := compressed.Close()
	if copyErr != nil && copyErr != io.EOF {
		return fmt.Errorf("read dedalus-operation.jsonl.gz: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close dedalus-operation.jsonl.gz: %w", closeErr)
	}
	if written > feedbackDecompressedCap {
		return fmt.Errorf("dedalus-operation.jsonl.gz exceeds the 10 MB decompressed cap")
	}
	if reader.Len() != 0 {
		return fmt.Errorf("dedalus-operation.jsonl.gz must contain exactly one gzip member")
	}
	return nil
}

package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestFeedbackCommandSendsJSONWithoutLogs(t *testing.T) {
	t.Setenv("DEDALUS_DEBUG_DIR", t.TempDir())
	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/feedback", r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Equal(t, "dedalus feedback", r.Header.Get("X-Dedalus-CLI-Command"))
		idempotencyKey, err := uuid.Parse(r.Header.Get("Idempotency-Key"))
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), idempotencyKey.Version())

		var metadata feedbackMetadata
		require.NoError(t, json.NewDecoder(r.Body).Decode(&metadata))
		require.Equal(t, "please add machine names", metadata.Message)
		require.Equal(t, "cli", metadata.Source)
		require.False(t, metadata.Debug.Included)
		require.Empty(t, metadata.Debug.Files)

		return feedbackResponse(`{"id":"feedback_123"}`)
	})

	var output bytes.Buffer
	root := testFeedbackCommand(&output)
	err := root.Run(context.Background(), []string{
		"dedalus", "--base-url", "https://api.test", "--api-key", "test-key",
		"feedback", "--include-logs=false", "please", "add", "machine", "names",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"feedback_123"}`, output.String())
}

func TestFeedbackCommandRetriesWithSameIdempotencyKey(t *testing.T) {
	t.Setenv("DEDALUS_DEBUG_DIR", t.TempDir())
	originalDelay := feedbackRetryDelay
	feedbackRetryDelay = 0
	t.Cleanup(func() { feedbackRetryDelay = originalDelay })

	var keys []string
	attempt := 0
	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		attempt++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if attempt == 1 {
			return feedbackStatusResponse(
				http.StatusServiceUnavailable,
				`{"detail":{"error":{"retryable":true}}}`,
			)
		}
		return feedbackResponse(`{"id":"feedback_retry"}`)
	})

	err := testFeedbackCommand(io.Discard).Run(
		context.Background(),
		[]string{"dedalus", "--base-url", "https://api.test", "feedback", "retry me"},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempt)
	require.NotEmpty(t, keys[0])
	require.Equal(t, keys[0], keys[1])
}

func TestFeedbackCommandDoesNotRetryNonRetryableFailure(t *testing.T) {
	t.Setenv("DEDALUS_DEBUG_DIR", t.TempDir())
	attempts := 0
	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		attempts++
		return feedbackStatusResponse(
			http.StatusBadGateway,
			`{"detail":{"error":{"retryable":false}}}`,
		)
	})

	err := testFeedbackCommand(io.Discard).Run(
		context.Background(),
		[]string{"dedalus", "--base-url", "https://api.test", "feedback", "bad config"},
	)
	require.ErrorContains(t, err, "502 Bad Gateway")
	require.Equal(t, 1, attempts)
}

func TestFeedbackCommandRetriesUnreadableResponse(t *testing.T) {
	t.Setenv("DEDALUS_DEBUG_DIR", t.TempDir())
	originalDelay := feedbackRetryDelay
	feedbackRetryDelay = 0
	t.Cleanup(func() { feedbackRetryDelay = originalDelay })

	attempts := 0
	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		attempts++
		if attempts == 1 {
			response := feedbackResponse("")
			response.Body = unreadableFeedbackBody{}
			return response
		}
		return feedbackResponse(`{"id":"feedback_retry"}`)
	})

	err := testFeedbackCommand(io.Discard).Run(
		context.Background(),
		[]string{"dedalus", "--base-url", "https://api.test", "feedback", "retry me"},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

func TestFeedbackCommandAutoSendsFailedInvocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEDALUS_DEBUG_DIR", dir)
	requestID := "01973f7b7cf6726a9a9f4f37d4b47a21"
	log := fmt.Sprintf(
		`{"ts":%q,"kind":"request_failed","level":"error","target":"http","command":"dedalus machines create","route":"/v1/machines/{machine_id}","machine_id":"machine_123","request_id":%q,"process_uuid":"process","status_code":500,"duration_ms":42}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), requestID,
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "invocation.jsonl"), []byte(log), 0o600))

	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.Equal(t, "multipart/form-data", mediaType)
		reader := multipart.NewReader(r.Body, params["boundary"])

		metadataPart, err := reader.NextPart()
		require.NoError(t, err)
		require.Equal(t, "metadata", metadataPart.FormName())
		var metadata feedbackMetadata
		require.NoError(t, json.NewDecoder(metadataPart).Decode(&metadata))

		logPart, err := reader.NextPart()
		require.NoError(t, err)
		require.Equal(t, "debug_files", logPart.FormName())
		require.Equal(t, "dedalus-feedback.log", logPart.FileName())
		uploaded, err := io.ReadAll(logPart)
		require.NoError(t, err)
		require.Equal(t, log, string(uploaded))
		_, err = reader.NextPart()
		require.ErrorIs(t, err, io.EOF)

		require.True(t, metadata.Debug.Included)
		require.Equal(t, requestID, metadata.ReportedRequestID)
		require.Equal(t, "machine_123", metadata.MachineID)
		require.Equal(t, "dedalus machines create", metadata.Command)
		require.Equal(t, &feedbackLastAPIFailure{StatusCode: 500, DurationMS: 42, Route: "/v1/machines/{machine_id}"}, metadata.Diagnostics.LastAPIFailure)
		require.Equal(t, []feedbackManifestFile{{
			Filename: "dedalus-feedback.log", Format: "text/plain",
			SHA256: fmt.Sprintf("%x", sha256.Sum256(uploaded)), SizeBytes: len(uploaded),
		}}, metadata.Debug.Files)

		return feedbackResponse(`{"id":"feedback_456"}`)
	})

	root := testFeedbackCommand(io.Discard)
	err := root.Run(context.Background(), []string{"dedalus", "--base-url", "https://api.test", "feedback", "machine create failed"})
	require.NoError(t, err)
}

func TestFeedbackCommandOmitsInvalidRequestReceipt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEDALUS_DEBUG_DIR", dir)
	log := fmt.Sprintf(
		`{"ts":%q,"kind":"request_failed","level":"error","target":"http","request_id":"not-a-platform-id","process_uuid":"process"}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "invocation.jsonl"), []byte(log), 0o600))

	setFeedbackTransport(t, func(r *http.Request) *http.Response {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.Equal(t, "multipart/form-data", mediaType)
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		require.NoError(t, err)
		var metadata feedbackMetadata
		require.NoError(t, json.NewDecoder(part).Decode(&metadata))
		require.Empty(t, metadata.ReportedRequestID)
		return feedbackResponse(`{"id":"feedback_789"}`)
	})

	err := testFeedbackCommand(io.Discard).Run(
		context.Background(),
		[]string{"dedalus", "--base-url", "https://api.test", "feedback", "failed"},
	)
	require.NoError(t, err)
}

func TestFeedbackBundleModes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEDALUS_DEBUG_DIR", dir)

	attachments, failure, err := selectFeedbackBundle("false", time.Now())
	require.NoError(t, err)
	require.Nil(t, failure)
	require.Empty(t, attachments)

	_, _, err = selectFeedbackBundle("true", time.Now())
	require.EqualError(t, err, "no first-party debug artifacts are available; rerun the failed command, then retry")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "dedalus-doctor-report.json"), []byte(`{"status":"failed"}`), 0o600))
	attachments, _, err = selectFeedbackBundle("auto", time.Now())
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Equal(t, "dedalus-doctor-report.json", attachments[0].Filename)
}

func TestFeedbackBundleRejectsSymlinkedArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEDALUS_DEBUG_DIR", dir)
	outside := filepath.Join(t.TempDir(), "user-document.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"secret":true}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "dedalus-doctor-report.json")))

	_, _, err := selectFeedbackBundle("true", time.Now())
	require.EqualError(t, err, "dedalus-doctor-report.json is not a regular first-party debug artifact")
}

func TestFeedbackBundleIncludesEveryAllowedArtifact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEDALUS_DEBUG_DIR", dir)
	completed := fmt.Sprintf(
		`{"ts":%q,"kind":"request_completed","level":"info","target":"http","process_uuid":"p","status_code":200}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "invocation.jsonl"), []byte(completed), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dedalus-doctor-report.json"), []byte(`{"status":"failed"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dedalus-connectivity-diagnostics.txt"), []byte("dns: ok\n"), 0o600))

	var trace bytes.Buffer
	writer := gzip.NewWriter(&trace)
	_, err := io.WriteString(writer, `{"step":"dial"}`+"\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dedalus-operation.jsonl.gz"), trace.Bytes(), 0o600))

	attachments, failure, err := selectFeedbackBundle("true", time.Now())
	require.NoError(t, err)
	require.Nil(t, failure)
	require.Equal(t, []string{
		"dedalus-feedback.log",
		"dedalus-operation.jsonl.gz",
		"dedalus-doctor-report.json",
		"dedalus-connectivity-diagnostics.txt",
	}, attachmentNames(attachments))
	for _, attachment := range attachments {
		require.Equal(t, feedbackArtifactFormats[attachment.Filename], attachment.ContentType)
		require.Equal(t, attachment.manifest().SizeBytes, len(attachment.Data))
	}
}

func TestValidateSingleGZIPMemberRejectsMultipleMembers(t *testing.T) {
	var data bytes.Buffer
	for _, value := range []string{"one", "two"} {
		writer := gzip.NewWriter(&data)
		_, err := io.WriteString(writer, value)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
	}
	require.EqualError(t, validateSingleGZIPMember(data.Bytes()), "dedalus-operation.jsonl.gz must contain exactly one gzip member")
}

func testFeedbackCommand(output io.Writer) *cli.Command {
	feedback := newFeedbackCommand()
	return &cli.Command{
		Name:   "dedalus",
		Writer: output,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "base-url"},
			&cli.StringFlag{Name: "api-key"},
			&cli.StringFlag{Name: "x-api-key"},
			&cli.StringFlag{Name: "dedalus-org-id"},
		},
		Commands: []*cli.Command{&feedback},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func setFeedbackTransport(t *testing.T, handler func(*http.Request) *http.Response) {
	t.Helper()
	original := feedbackHTTPClient
	feedbackHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return handler(request), nil
	})}
	t.Cleanup(func() { feedbackHTTPClient = original })
}

func feedbackResponse(body string) *http.Response {
	return feedbackStatusResponse(http.StatusCreated, body)
}

func feedbackStatusResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type unreadableFeedbackBody struct{}

func (unreadableFeedbackBody) Read([]byte) (int, error) {
	return 0, errors.New("truncated response")
}

func (unreadableFeedbackBody) Close() error { return nil }

func attachmentNames(attachments []feedbackAttachment) []string {
	names := make([]string, len(attachments))
	for index, attachment := range attachments {
		names[index] = attachment.Filename
	}
	return names
}

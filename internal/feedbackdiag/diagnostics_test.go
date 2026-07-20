package feedbackdiag

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMiddlewareRecordsServerReceiptWithoutSensitiveData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	session, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/machines/machine_123/ssh", strings.NewReader("secret body"))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Dedalus-CLI-Command", "dedalus machines create")
	responseHeader := make(http.Header)
	responseHeader.Set("X-Request-ID", "server-receipt")
	resp, err := session.Middleware()(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     responseHeader,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	contents, err := os.ReadFile(session.path)
	require.NoError(t, err)
	require.Contains(t, string(contents), `"request_id":"server-receipt"`)
	require.Contains(t, string(contents), `"kind":"request_failed"`)
	require.Contains(t, string(contents), `"route":"/v1/machines/{machine_id}/ssh"`)
	require.Contains(t, string(contents), `"machine_id":"machine_123"`)
	require.NotContains(t, string(contents), "/v1/machines/machine_123")
	require.NotContains(t, string(contents), "secret")

	failure, err := LatestFailure(dir, time.Now())
	require.NoError(t, err)
	require.NotNil(t, failure)
	require.Equal(t, "server-receipt", failure.RequestID)
	require.Equal(t, session.path, failure.Path)
}

func TestMachineRouteRedactsNestedResourceIDs(t *testing.T) {
	t.Parallel()

	route, machineID := machineRoute("/v1/machines/machine_123/executions/execution_456/output")
	require.Equal(t, "/v1/machines/{machine_id}/executions/{execution_id}/output", route)
	require.Equal(t, "machine_123", machineID)

	route, machineID = machineRoute("/v1/unknown/private-value")
	require.Empty(t, route)
	require.Empty(t, machineID)

	route, machineID = machineRoute("/v1/machines")
	require.Equal(t, "/v1/machines", route)
	require.Empty(t, machineID)
}

func TestLatestFailureIgnoresExpiredReceipts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/old.jsonl"
	old := `{"ts":"2026-01-01T00:00:00Z","kind":"request_failed","level":"error","target":"http","process_uuid":"p","status_code":500}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(old), 0o600))

	failure, err := LatestFailure(dir, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Nil(t, failure)
}

func TestLatestFailureIgnoresAnAttemptRecoveredByRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/retried.jsonl"
	contents := strings.Join([]string{
		`{"ts":"2026-01-02T00:00:00Z","kind":"request_failed","level":"error","target":"http","request_id":"failed","process_uuid":"p","status_code":500}`,
		`{"ts":"2026-01-02T00:00:01Z","kind":"request_completed","level":"info","target":"http","request_id":"succeeded","process_uuid":"p","status_code":200}`,
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	failure, err := LatestFailure(dir, time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Nil(t, failure)
}

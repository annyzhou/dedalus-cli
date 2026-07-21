package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v3"
)

const feedbackProductionURL = "https://api.dedaluslabs.ai"

var feedbackHTTPClient = &http.Client{Timeout: 30 * time.Second}
var feedbackRequestID = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
var feedbackRetryDelay = time.Second

func init() {
	Command.Commands = append(Command.Commands, &feedbackCommand)
}

type feedbackDebug struct {
	Included bool                   `json:"included"`
	Files    []feedbackManifestFile `json:"files,omitempty"`
}

type feedbackLastAPIFailure struct {
	StatusCode int    `json:"status_code"`
	DurationMS int64  `json:"duration_ms"`
	Route      string `json:"route"`
}

type feedbackDiagnostics struct {
	LastAPIFailure *feedbackLastAPIFailure `json:"last_api_failure,omitempty"`
}

type feedbackClient struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type feedbackMetadata struct {
	Message           string               `json:"message"`
	Source            string               `json:"source"`
	MachineID         string               `json:"machine_id,omitempty"`
	ReportedRequestID string               `json:"reported_request_id,omitempty"`
	Command           string               `json:"command,omitempty"`
	Client            feedbackClient       `json:"client"`
	Diagnostics       *feedbackDiagnostics `json:"diagnostics,omitempty"`
	Debug             feedbackDebug        `json:"debug"`
}

var feedbackCommand = newFeedbackCommand()

func newFeedbackCommand() cli.Command {
	return cli.Command{
		Name:      "feedback",
		Usage:     "Send feedback to Dedalus",
		UsageText: "dedalus feedback [--include-logs <auto|true|false>] <message>",
		ArgsUsage: "<message>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "include-logs",
				Usage: "Include first-party debug logs: auto, true, or false",
				Value: "auto",
				Validator: func(value string) error {
					if value != "auto" && value != "true" && value != "false" {
						return fmt.Errorf("include-logs must be one of: auto, true, false")
					}
					return nil
				},
			},
		},
		Action:          handleFeedback,
		HideHelpCommand: true,
	}
}

func handleFeedback(ctx context.Context, command *cli.Command) error {
	message := strings.TrimSpace(strings.Join(command.Args().Slice(), " "))
	if message == "" {
		return fmt.Errorf("message is required\nRun 'dedalus feedback --help' for usage information")
	}
	attachments, failure, err := selectFeedbackBundle(command.String("include-logs"), time.Now())
	if err != nil {
		return err
	}
	metadata := feedbackMetadata{
		Message: message,
		Source:  "cli",
		Client:  feedbackClient{Name: "dedalus-cli", Version: Version},
		Debug:   feedbackDebug{Included: len(attachments) > 0},
	}
	for _, attachment := range attachments {
		metadata.Debug.Files = append(metadata.Debug.Files, attachment.manifest())
	}
	if failure != nil {
		metadata.MachineID = failure.MachineID
		if feedbackRequestID.MatchString(failure.RequestID) {
			metadata.ReportedRequestID = failure.RequestID
		}
		metadata.Command = failure.Command
		if failure.StatusCode >= 100 {
			metadata.Diagnostics = &feedbackDiagnostics{LastAPIFailure: &feedbackLastAPIFailure{
				StatusCode: failure.StatusCode, DurationMS: failure.DurationMS, Route: failure.Route,
			}}
		}
	}

	request, err := feedbackRequest(ctx, command, metadata, attachments)
	if err != nil {
		return err
	}
	body, err := submitFeedback(request)
	if err != nil {
		return err
	}
	if !json.Valid(body) {
		return fmt.Errorf("feedback response is not valid JSON")
	}
	writer := command.Root().Writer
	if writer == nil {
		writer = os.Stdout
	}
	_, err = fmt.Fprintln(writer, string(body))
	return err
}

func feedbackRequest(ctx context.Context, command *cli.Command, metadata feedbackMetadata, attachments []feedbackAttachment) (*http.Request, error) {
	var body bytes.Buffer
	contentType := "application/json"
	if len(attachments) == 0 {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return nil, fmt.Errorf("encode feedback: %w", err)
		}
		body.Write(encoded)
	} else {
		writer := multipart.NewWriter(&body)
		if err := writeFeedbackMultipart(writer, metadata, attachments); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("close feedback multipart body: %w", err)
		}
		contentType = writer.FormDataContentType()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, feedbackEndpoint(command), &body)
	if err != nil {
		return nil, fmt.Errorf("create feedback request: %w", err)
	}
	request.Header.Set("Content-Type", contentType)
	idempotencyKey, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create feedback idempotency key: %w", err)
	}
	request.Header.Set("Idempotency-Key", strings.ReplaceAll(idempotencyKey.String(), "-", ""))
	request.Header.Set("User-Agent", fmt.Sprintf("Dedalus/CLI %s", Version))
	request.Header.Set("X-Dedalus-CLI-Command", "dedalus feedback")
	root := command.Root()
	if key := root.String("api-key"); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if key := root.String("x-api-key"); key != "" {
		request.Header.Set("X-API-Key", key)
	}
	if orgID := root.String("dedalus-org-id"); orgID != "" {
		request.Header.Set("X-Dedalus-Org-Id", orgID)
	}
	return request, nil
}

func submitFeedback(request *http.Request) ([]byte, error) {
	for attempt := range 2 {
		current := request
		if attempt > 0 {
			body, err := request.GetBody()
			if err != nil {
				return nil, fmt.Errorf("rebuild feedback request: %w", err)
			}
			current = request.Clone(request.Context())
			current.Body = body
		}
		response, err := feedbackHTTPClient.Do(current)
		if err != nil {
			if attempt == 0 {
				if waitErr := waitFeedbackRetry(request.Context()); waitErr != nil {
					return nil, fmt.Errorf("submit feedback: %w", waitErr)
				}
				continue
			}
			return nil, fmt.Errorf("submit feedback: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil {
			if attempt == 0 {
				if waitErr := waitFeedbackRetry(request.Context()); waitErr != nil {
					return nil, fmt.Errorf("read feedback response: %w", waitErr)
				}
				continue
			}
			return nil, fmt.Errorf("read feedback response: %w", readErr)
		}
		if response.StatusCode == http.StatusCreated {
			return body, nil
		}
		if attempt == 0 && retryableFeedbackResponse(response.StatusCode, body) {
			if waitErr := waitFeedbackRetry(request.Context()); waitErr != nil {
				return nil, fmt.Errorf("submit feedback: %w", waitErr)
			}
			continue
		}
		return nil, fmt.Errorf("feedback request failed: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	panic("unreachable")
}

func waitFeedbackRetry(ctx context.Context) error {
	timer := time.NewTimer(feedbackRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryableFeedbackResponse(statusCode int, body []byte) bool {
	var response struct {
		Detail struct {
			Error struct {
				Retryable *bool `json:"retryable"`
			} `json:"error"`
		} `json:"detail"`
	}
	parsed := json.Unmarshal(body, &response) == nil && response.Detail.Error.Retryable != nil
	if parsed {
		return *response.Detail.Error.Retryable
	}
	return statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= 500
}

func writeFeedbackMultipart(writer *multipart.Writer, metadata feedbackMetadata, attachments []feedbackAttachment) error {
	metadataHeader := make(textproto.MIMEHeader)
	metadataHeader.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "metadata"}))
	metadataHeader.Set("Content-Type", "application/json")
	part, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return fmt.Errorf("create feedback metadata part: %w", err)
	}
	if err := json.NewEncoder(part).Encode(metadata); err != nil {
		return fmt.Errorf("encode feedback metadata: %w", err)
	}
	for _, attachment := range attachments {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name": "debug_files", "filename": attachment.Filename,
		}))
		header.Set("Content-Type", attachment.ContentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return fmt.Errorf("create %s part: %w", attachment.Filename, err)
		}
		if _, err := part.Write(attachment.Data); err != nil {
			return fmt.Errorf("write %s part: %w", attachment.Filename, err)
		}
	}
	return nil
}

func feedbackEndpoint(command *cli.Command) string {
	baseURL := command.Root().String("base-url")
	if baseURL == "" {
		baseURL = os.Getenv("DEDALUS_BASE_URL")
	}
	if baseURL == "" {
		baseURL = feedbackProductionURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/feedback"
	}
	return baseURL + "/v1/feedback"
}

package simpleforce

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIUsage counts HTTP attempts, including failed and retried calls.
// Retries counts transient retries, not session refresh replays.
type APIUsage struct {
	RESTQueryCalls  int
	DescribeCalls   int
	BulkJobsCreated int
	BulkPollCalls   int
	BulkResultCalls int
	Retries         int
	LimitInfo       string
}

// TotalCalls returns the total number of HTTP attempts.
func (u APIUsage) TotalCalls() int {
	return u.RESTQueryCalls + u.DescribeCalls + u.BulkJobsCreated + u.BulkPollCalls + u.BulkResultCalls
}

// FieldMetadata describes a field, including numeric precision and scale.
type FieldMetadata struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Precision int    `json:"precision"`
	Scale     int    `json:"scale"`
}

// QueryOptions configures query operations. Zero values select three transient
// retries, five-second polling and 50,000 records per Bulk result page.
// Sleep, when supplied, must honor context cancellation. Set a context deadline
// to bound the duration of requests and job polling.
type QueryOptions struct {
	MaxRetries   int
	PollInterval time.Duration
	MaxRecords   int
	Sleep        func(context.Context, time.Duration) error
}

// QueryClient streams queries through an existing authenticated Client.
// The provider receives an empty rejectedSID normally and the rejected session
// ID on a 401, when it must bypass cached credentials and refresh authentication.
// The provider must be concurrency-safe and return Clients that remain immutable
// while in use. Refresh must return a new Client, not call SetSidLoc (or other
// setters/login methods) on a Client already returned to a query.
type QueryClient struct {
	getClient func(context.Context, string) (*Client, error)
	options   QueryOptions
	mu        sync.Mutex
	usage     APIUsage
}

// NewQueryClient creates an opt-in query helper without changing legacy APIs.
// Configure HTTP transport through Client.SetHttpClient before using the client.
func NewQueryClient(getClient func(context.Context, string) (*Client, error), options QueryOptions) (*QueryClient, error) {
	if getClient == nil {
		return nil, fmt.Errorf("client provider is required")
	}
	if options.MaxRetries < 0 || options.PollInterval < 0 || options.MaxRecords < 0 {
		return nil, fmt.Errorf("query options must not be negative")
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = 3
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = 50000
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	return &QueryClient{getClient: getClient, options: options}, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Usage returns a concurrency-safe snapshot of cumulative query usage.
func (q *QueryClient) Usage() APIUsage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.usage
}

func (q *QueryClient) observe(kind string, retry bool, response *http.Response) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch kind {
	case "rest":
		q.usage.RESTQueryCalls++
	case "describe":
		q.usage.DescribeCalls++
	case "bulk_create":
		q.usage.BulkJobsCreated++
	case "bulk_poll":
		q.usage.BulkPollCalls++
	case "bulk_result":
		q.usage.BulkResultCalls++
	}
	if retry {
		q.usage.Retries++
	}
	if response != nil && response.Header.Get("Sforce-Limit-Info") != "" {
		q.usage.LimitInfo = response.Header.Get("Sforce-Limit-Info")
	}
}

// Only idempotent reads retry ambiguous failures. A job creation can be
// replayed after an explicit 401, but never after a transport failure or 5xx.
func (q *QueryClient) request(ctx context.Context, kind, method, path string, body []byte) (*http.Response, error) {
	var client *Client
	var err error
	refreshed, retry := false, false
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if client == nil {
			client, err = q.getClient(ctx, "")
			if err != nil {
				return nil, err
			}
		}
		if client == nil || client.sessionID == "" {
			return nil, ErrAuthentication
		}
		sentSID := client.sessionID
		relative, err := url.Parse(path)
		if err != nil || relative.IsAbs() || relative.Host != "" || strings.HasPrefix(path, "//") {
			return nil, fmt.Errorf("invalid Salesforce request path %q", path)
		}
		requestPath := path
		if !strings.HasPrefix(path, "/services/data/") {
			requestPath = "/services/data/v" + strings.TrimPrefix(client.apiVersion, "v") + "/" + path
		}
		endpoint := strings.TrimRight(client.instanceURL, "/") + requestPath
		accept := "application/json"
		if kind == "bulk_result" {
			accept = "text/csv"
		}
		response, requestErr := client.requestContext(ctx, method, endpoint, bytes.NewReader(body), accept)
		q.observe(kind, retry, response)
		retry = false
		if requestErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		if response != nil && requestErr == nil && response.StatusCode == http.StatusUnauthorized && !refreshed {
			response.Body.Close()
			client, err = q.getClient(ctx, sentSID)
			if err != nil {
				return nil, err
			}
			if client == nil {
				return nil, ErrAuthentication
			}
			refreshed = true
			continue
		}
		retryable := requestErr != nil
		var failure error = requestErr
		delay := time.Second << uint(minInt(retries, 5))
		if response != nil && requestErr == nil {
			retryable = response.StatusCode == 429 || response.StatusCode >= 500
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
			response.Body.Close()
			if readErr != nil {
				failure = fmt.Errorf("Salesforce HTTP %d: reading error body: %w", response.StatusCode, readErr)
			} else {
				failure = ParseSalesforceError(response.StatusCode, data)
			}
			if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds >= 0 {
				delay = time.Duration(minInt(seconds, 60)) * time.Second
			} else if deadline, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
				delay = time.Until(deadline)
				if delay < 0 {
					delay = 0
				}
				if delay > time.Minute {
					delay = time.Minute
				}
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if method != http.MethodGet || !retryable || retries >= q.options.MaxRetries {
			return nil, failure
		}
		if err := q.options.Sleep(ctx, delay); err != nil {
			return nil, err
		}
		retries++
		retry = true
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (q *QueryClient) json(ctx context.Context, kind, method, path string, body []byte, target interface{}) error {
	response, err := q.request(ctx, kind, method, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode Salesforce %s response: %w", kind, err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode Salesforce %s response: unexpected trailing JSON document", kind)
		}
		return fmt.Errorf("finish Salesforce %s response: %w", kind, err)
	}
	return nil
}

var countOnlyQuery = regexp.MustCompile(`(?is)^\s*select\s+count\s*\(\s*\)\s+from\b`)

// Query streams records page by page, preserving JSON numbers as json.Number
// and relationships as nested maps. Callback errors stop pagination immediately.
// A successful query validates the number of records against totalSize.
// Count-only SELECT COUNT() queries are unsupported; use Client.Query instead.
func (q *QueryClient) Query(ctx context.Context, soql string, emit func(map[string]interface{}) error) error {
	if emit == nil || strings.TrimSpace(soql) == "" {
		return fmt.Errorf("query and record callback are required")
	}
	if countOnlyQuery.MatchString(soql) {
		return fmt.Errorf("count-only SELECT COUNT() queries are unsupported by streaming Query; use Client.Query")
	}
	path := "query?q=" + url.QueryEscape(soql)
	seen := make(map[string]bool)
	var total, expected int64
	first := true
	for {
		if seen[path] {
			return fmt.Errorf("repeated REST query locator")
		}
		seen[path] = true
		var page struct {
			TotalSize *int64                   `json:"totalSize"`
			Done      *bool                    `json:"done"`
			Next      string                   `json:"nextRecordsUrl"`
			Records   []map[string]interface{} `json:"records"`
		}
		if err := q.json(ctx, "rest", http.MethodGet, path, nil, &page); err != nil {
			return err
		}
		if page.Done == nil || page.TotalSize == nil || *page.TotalSize < 0 {
			return fmt.Errorf("missing or invalid REST query done/totalSize")
		}
		if first {
			expected, first = *page.TotalSize, false
		} else if *page.TotalSize != expected {
			return fmt.Errorf("REST query totalSize changed during pagination")
		}
		for _, record := range page.Records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := emit(record); err != nil {
				return err
			}
			total++
		}
		if total > expected || (*page.Done && total != expected) {
			return fmt.Errorf("REST query expected totalSize %d, received %d", expected, total)
		}
		if *page.Done {
			return nil
		}
		if !strings.HasPrefix(page.Next, "/services/data/") {
			return fmt.Errorf("missing or invalid nextRecordsUrl")
		}
		path = page.Next
	}
}

// Describe returns typed field metadata and propagates HTTP and decoding errors.
func (q *QueryClient) Describe(ctx context.Context, object string) ([]FieldMetadata, error) {
	if !validIdentifier(object) {
		return nil, fmt.Errorf("invalid object name %q", object)
	}
	var result struct {
		Fields []FieldMetadata `json:"fields"`
	}
	err := q.json(ctx, "describe", http.MethodGet, "sobjects/"+object+"/describe", nil, &result)
	return result.Fields, err
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char != '_' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

type bulkQueryJob struct {
	ID                     string `json:"id"`
	State                  string `json:"state"`
	ErrorMessage           string `json:"errorMessage"`
	NumberRecordsProcessed *int64 `json:"numberRecordsProcessed"`
}

// BulkQuery creates a Bulk API 2.0 query job, waits for completion and streams
// CSV rows. Keys use the caller's requested field spelling. Headers are matched
// case-insensitively; missing/duplicate headers and result count mismatches fail.
// Like encoding/csv, embedded CRLF values are normalized to LF; use Query when
// exact multiline text preservation is required.
// Cancellation stops local work; it does not send a separate abort request.
func (q *QueryClient) BulkQuery(ctx context.Context, soql string, fields []string, emit func(map[string]string) error) error {
	if strings.TrimSpace(soql) == "" || len(fields) == 0 || emit == nil {
		return fmt.Errorf("query, fields and record callback are required")
	}
	requested := make(map[string]bool)
	for _, field := range fields {
		key := strings.ToLower(field)
		if key == "" || requested[key] {
			return fmt.Errorf("empty or duplicate requested field %q", field)
		}
		requested[key] = true
	}
	body, err := json.Marshal(map[string]string{
		"operation": "query", "query": soql, "contentType": "CSV",
		"columnDelimiter": "COMMA", "lineEnding": "LF",
	})
	if err != nil {
		return err
	}
	var job bulkQueryJob
	if err := q.json(ctx, "bulk_create", http.MethodPost, "jobs/query", body, &job); err != nil {
		return err
	}
	if !validIdentifier(job.ID) {
		return fmt.Errorf("missing or invalid Bulk query job ID")
	}
	path := "jobs/query/" + job.ID
	for {
		var status bulkQueryJob
		if err := q.json(ctx, "bulk_poll", http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		switch status.State {
		case "JobComplete":
			if status.NumberRecordsProcessed == nil || *status.NumberRecordsProcessed < 0 {
				return fmt.Errorf("missing or invalid Bulk query total record count")
			}
			return q.bulkResults(ctx, path, fields, *status.NumberRecordsProcessed, emit)
		case "Failed", "Aborted":
			return fmt.Errorf("Bulk query job %s %s: %s", job.ID, status.State, status.ErrorMessage)
		case "UploadComplete", "InProgress", "Open":
			if err := q.options.Sleep(ctx, q.options.PollInterval); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown Bulk query state %q", status.State)
		}
	}
}

func (q *QueryClient) bulkResults(ctx context.Context, path string, fields []string, expected int64, emit func(map[string]string) error) error {
	locator := ""
	seen := make(map[string]bool)
	var total int64
	for {
		params := url.Values{"maxRecords": {strconv.Itoa(q.options.MaxRecords)}}
		if locator != "" {
			params.Set("locator", locator)
		}
		response, err := q.request(ctx, "bulk_result", http.MethodGet, path+"/results?"+params.Encode(), nil)
		if err != nil {
			return err
		}
		count, err := readBulkPage(ctx, response.Body, fields, emit)
		response.Body.Close()
		if err != nil {
			return err
		}
		if value := response.Header.Get("Sforce-NumberOfRecords"); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n != count {
				return fmt.Errorf("Bulk page count mismatch: header %q, received %d", value, count)
			}
		}
		total += count
		if total > expected {
			return fmt.Errorf("Bulk result count mismatch: expected %d, received at least %d", expected, total)
		}
		locator = response.Header.Get("Sforce-Locator")
		if strings.EqualFold(locator, "null") {
			if total != expected {
				return fmt.Errorf("Bulk result count mismatch: expected %d, received %d", expected, total)
			}
			return nil
		}
		if locator == "" || seen[locator] {
			return fmt.Errorf("missing or repeated Bulk result locator")
		}
		seen[locator] = true
	}
}

func readBulkPage(ctx context.Context, body io.Reader, fields []string, emit func(map[string]string) error) (int64, error) {
	reader := csv.NewReader(body)
	headers, err := reader.Read()
	if err != nil {
		return 0, fmt.Errorf("read Bulk CSV header: %w", err)
	}
	positions := make(map[string]int, len(headers))
	for i, header := range headers {
		key := strings.ToLower(header)
		if _, exists := positions[key]; exists {
			return 0, fmt.Errorf("duplicate Bulk CSV header %q", header)
		}
		positions[key] = i
	}
	indices := make([]int, len(fields))
	for i, field := range fields {
		position, exists := positions[strings.ToLower(field)]
		if !exists {
			return 0, fmt.Errorf("Bulk CSV missing field %s", field)
		}
		indices[i] = position
	}
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		values, err := reader.Read()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("read Bulk CSV record: %w", err)
		}
		record := make(map[string]string, len(fields))
		for i, field := range fields {
			record[field] = values[indices[i]]
		}
		if err := emit(record); err != nil {
			return count, err
		}
		count++
	}
}

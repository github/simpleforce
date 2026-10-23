package simpleforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testQueries(t *testing.T, handler http.HandlerFunc, options QueryOptions) (*QueryClient, *Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(server.URL, "test", "60.0")
	client.SetSidLoc("token", server.URL)
	client.SetHttpClient(server.Client())
	if options.Sleep == nil {
		options.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	queries, err := NewQueryClient(func(context.Context, string) (*Client, error) {
		return client, nil
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	return queries, client
}

func TestQueryStreamingAndMetadata(t *testing.T) {
	calls := 0
	queries, client := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("shared session not used")
		}
		w.Header().Set("Sforce-Limit-Info", "api-usage=42/5000")
		switch r.URL.Path {
		case "/services/data/v60.0/query":
			if r.URL.Query().Get("q") != "SELECT Amount,Owner.Name FROM Opportunity" {
				t.Error("SOQL was not encoded correctly")
			}
			fmt.Fprint(w, `{"totalSize":2,"done":false,"nextRecordsUrl":"/services/data/v60.0/query/next","records":[{"Amount":9999999999999999.99,"Owner":{"Name":"Mona","Number":1.0E7}}]}`)
		case "/services/data/v60.0/query/next":
			fmt.Fprint(w, `{"totalSize":2,"done":true,"records":[{"Amount":null,"Owner":null}]}`)
		case "/services/data/v60.0/sobjects/Product2/describe":
			fmt.Fprint(w, `{"fields":[{"name":"Price","type":"currency","precision":18,"scale":2}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}, QueryOptions{})
	var records []map[string]interface{}
	if err := queries.Query(context.Background(), "SELECT Amount,Owner.Name FROM Opportunity", func(row map[string]interface{}) error {
		if len(records) == 0 && calls != 1 {
			t.Error("first page must be emitted before requesting the next")
		}
		records = append(records, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0]["Amount"] != json.Number("9999999999999999.99") {
		t.Fatalf("records: %#v", records)
	}
	owner := records[0]["Owner"].(map[string]interface{})
	if owner["Number"] != json.Number("1.0E7") || records[1]["Owner"] != nil {
		t.Fatalf("relationships: %#v", records)
	}
	fields, err := queries.Describe(context.Background(), "Product2")
	if err != nil || len(fields) != 1 || fields[0] != (FieldMetadata{"Price", "currency", 18, 2}) {
		t.Fatalf("describe: %+v, %v", fields, err)
	}
	if usage := queries.Usage(); usage.RESTQueryCalls != 2 || usage.DescribeCalls != 1 || usage.TotalCalls() != 3 || usage.LimitInfo != "api-usage=42/5000" {
		t.Fatalf("usage: %+v", usage)
	}
	// Legacy Query keeps its float64 decoding behavior and shared transport.
	result, err := client.Query("SELECT Amount,Owner.Name FROM Opportunity")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Records[0]["Amount"].(float64); !ok {
		t.Fatal("legacy Query changed number types")
	}
}

func TestQueryRejectsIncompleteAndRepeatedPages(t *testing.T) {
	for _, test := range []struct {
		name, response, want string
	}{
		{"short", `{"totalSize":3,"done":true,"records":[{}]}`, "expected totalSize 3"},
		{"long", `{"totalSize":0,"done":true,"records":[{}]}`, "expected totalSize 0"},
		{"missing total", `{"done":true,"records":[]}`, "done/totalSize"},
		{"missing done", `{"totalSize":0,"records":[]}`, "done/totalSize"},
		{"missing locator", `{"totalSize":1,"done":false,"records":[]}`, "nextRecordsUrl"},
		{"external locator", `{"totalSize":1,"done":false,"nextRecordsUrl":"https://evil.example/","records":[]}`, "nextRecordsUrl"},
		{"repeated locator", `{"totalSize":1,"done":false,"nextRecordsUrl":"/services/data/v60.0/query/next","records":[]}`, "repeated"},
		{"malformed", `{"totalSize":`, "decode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, test.response)
			}, QueryOptions{})
			err := q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestQueryCallbackAndCancellation(t *testing.T) {
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"totalSize":2,"done":false,"nextRecordsUrl":"/services/data/v60.0/query/next","records":[{}]}`)
	}, QueryOptions{})
	want := errors.New("writer failed")
	err := q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return want })
	if !errors.Is(err, want) || q.Usage().TotalCalls() != 1 {
		t.Fatalf("callback failure: %v %+v", err, q.Usage())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = q.Query(ctx, "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
	if !errors.Is(err, context.Canceled) || q.Usage().TotalCalls() != 1 {
		t.Fatalf("canceled query: %v %+v", err, q.Usage())
	}
}

func TestQueryRefreshAndRetryUsage(t *testing.T) {
	calls := 0
	var delays []time.Duration
	q, client := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(401)
		case 2:
			if r.Header.Get("Authorization") != "Bearer fresh" {
				t.Error("refreshed SID not used")
			}
			w.Header().Set("Sforce-Limit-Info", "api-usage=9/100")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
		case 3:
			w.WriteHeader(503)
		default:
			fmt.Fprint(w, `{"totalSize":0,"done":true,"records":[]}`)
		}
	}, QueryOptions{Sleep: func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}})
	refreshes := 0
	q.getClient = func(_ context.Context, rejected string) (*Client, error) {
		if rejected != "" {
			refreshes++
			if rejected != "token" {
				t.Errorf("rejected SID = %s", rejected)
			}
			client.SetSidLoc("fresh", client.GetLoc())
		}
		return client, nil
	}
	if err := q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return nil }); err != nil {
		t.Fatal(err)
	}
	u := q.Usage()
	if refreshes != 1 || u.RESTQueryCalls != 4 || u.Retries != 2 || u.LimitInfo != "api-usage=9/100" || len(delays) != 2 || delays[0] != 2*time.Second || delays[1] != 2*time.Second {
		t.Fatalf("refreshes=%d usage=%+v delays=%v", refreshes, u, delays)
	}
}

func TestQueryRetryBoundsAndAuthenticationErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `[{"errorCode":"TEST_FAILURE"}]`)
			}, QueryOptions{MaxRetries: 2})
			err := q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
			wantCalls, wantRetries := 1, 0
			if status == 401 {
				wantCalls = 2
			}
			if status >= 429 {
				wantCalls, wantRetries = 3, 2
			}
			if err == nil || !strings.Contains(err.Error(), "TEST_FAILURE") || q.Usage().TotalCalls() != wantCalls || q.Usage().Retries != wantRetries {
				t.Fatalf("error=%v usage=%+v", err, q.Usage())
			}
		})
	}
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }, QueryOptions{})
	want := errors.New("refresh failed")
	q.getClient = func(_ context.Context, rejected string) (*Client, error) {
		if rejected != "" {
			return nil, want
		}
		c := NewClient("unused", "test", "60.0")
		c.SetSidLoc("token", "http://unused")
		c.SetHttpClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})})
		return c, nil
	}
	if err := q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("refresh error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQueryTransportRetriesAndCreationSafety(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprint(bulk), func(t *testing.T) {
			q, client := testQueries(t, func(http.ResponseWriter, *http.Request) {}, QueryOptions{MaxRetries: 2})
			want := errors.New("connection lost")
			client.SetHttpClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, want
			})})
			var err error
			calls := 3
			if bulk {
				err = q.BulkQuery(context.Background(), "SELECT Id FROM Account", []string{"Id"}, func(map[string]string) error { return nil })
				calls = 1
			} else {
				err = q.Query(context.Background(), "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
			}
			if !errors.Is(err, want) || q.Usage().TotalCalls() != calls {
				t.Fatalf("error=%v usage=%+v", err, q.Usage())
			}
		})
	}
}

func TestBulkQueryStreamingPagination(t *testing.T) {
	polls, results, emitted := 0, 0, 0
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["operation"] != "query" || body["query"] != "SELECT Id,Name FROM Account" || body["contentType"] != "CSV" {
				t.Errorf("job body: %+v", body)
			}
			fmt.Fprint(w, `{"id":"750JOB","state":"UploadComplete"}`)
		} else if strings.HasSuffix(r.URL.Path, "/results") {
			results++
			if r.Header.Get("Accept") != "text/csv" || r.URL.Query().Get("maxRecords") != "50000" {
				t.Error("incorrect result request")
			}
			if results == 1 {
				w.Header().Set("Sforce-Locator", "next+/=")
				w.Header().Set("Sforce-NumberOfRecords", "1")
				fmt.Fprint(w, "ID,NAME\n001,\"Contoso, Inc.\"\n")
			} else {
				if emitted != 1 || r.URL.Query().Get("locator") != "next+/=" {
					t.Error("page was buffered or locator was not encoded")
				}
				w.Header().Set("Sforce-Locator", "null")
				fmt.Fprint(w, "Id,Name\n002,\"Line\nBreak\"\n")
			}
		} else {
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"id":"750JOB","state":"InProgress"}`)
			} else {
				fmt.Fprint(w, `{"id":"750JOB","state":"JobComplete","numberRecordsProcessed":2}`)
			}
		}
	}, QueryOptions{Sleep: func(_ context.Context, delay time.Duration) error {
		if delay != 5*time.Second {
			t.Errorf("poll delay = %s", delay)
		}
		return nil
	}})
	var rows []map[string]string
	err := q.BulkQuery(context.Background(), "SELECT Id,Name FROM Account", []string{"Id", "Name"}, func(row map[string]string) error {
		emitted++
		rows = append(rows, row)
		return nil
	})
	if err != nil || len(rows) != 2 || rows[0]["Name"] != "Contoso, Inc." || rows[1]["Name"] != "Line\nBreak" {
		t.Fatalf("rows=%v error=%v", rows, err)
	}
	u := q.Usage()
	if u.BulkJobsCreated != 1 || u.BulkPollCalls != 2 || u.BulkResultCalls != 2 || u.TotalCalls() != 5 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestBulkQueryErrors(t *testing.T) {
	for _, test := range []struct {
		name, status, csv, locator, pageCount, want string
	}{
		{"failed", `{"state":"Failed","errorMessage":"INVALID_FIELD"}`, "", "", "", "INVALID_FIELD"},
		{"aborted", `{"state":"Aborted"}`, "", "", "", "Aborted"},
		{"unknown", `{"state":"Unexpected"}`, "", "", "", "unknown"},
		{"missing total", `{"state":"JobComplete"}`, "", "", "", "total record count"},
		{"missing field", `{"state":"JobComplete","numberRecordsProcessed":1}`, "Id\n001\n", "null", "", "missing field Name"},
		{"duplicate header", `{"state":"JobComplete","numberRecordsProcessed":1}`, "Id,id,Name\n001,001,Mona\n", "null", "", "duplicate"},
		{"bad CSV", `{"state":"JobComplete","numberRecordsProcessed":1}`, "Id,Name\n001\n", "null", "", "read Bulk CSV record"},
		{"short total", `{"state":"JobComplete","numberRecordsProcessed":2}`, "Id,Name\n001,Mona\n", "null", "", "count mismatch"},
		{"long total", `{"state":"JobComplete","numberRecordsProcessed":0}`, "Id,Name\n001,Mona\n", "null", "", "count mismatch"},
		{"bad page count", `{"state":"JobComplete","numberRecordsProcessed":1}`, "Id,Name\n001,Mona\n", "null", "2", "page count mismatch"},
		{"missing locator", `{"state":"JobComplete","numberRecordsProcessed":1}`, "Id,Name\n001,Mona\n", "", "", "locator"},
		{"repeated locator", `{"state":"JobComplete","numberRecordsProcessed":3}`, "Id,Name\n001,Mona\n", "next", "", "repeated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					fmt.Fprint(w, `{"id":"750JOB"}`)
				case strings.HasSuffix(r.URL.Path, "/results"):
					w.Header().Set("Sforce-Locator", test.locator)
					w.Header().Set("Sforce-NumberOfRecords", test.pageCount)
					fmt.Fprint(w, test.csv)
				default:
					fmt.Fprint(w, test.status)
				}
			}, QueryOptions{})
			err := q.BulkQuery(context.Background(), "SELECT Id,Name FROM Account", []string{"Id", "Name"}, func(map[string]string) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%s", err, test.want)
			}
		})
	}
}

func TestBulkQueryDoesNotRetryCreationHTTPFailure(t *testing.T) {
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, QueryOptions{})
	err := q.BulkQuery(context.Background(), "SELECT Id FROM Account", []string{"Id"}, func(map[string]string) error { return nil })
	if err == nil || q.Usage().BulkJobsCreated != 1 || q.Usage().Retries != 0 {
		t.Fatalf("error=%v usage=%+v", err, q.Usage())
	}
}

func TestBulkCancellationAndCallback(t *testing.T) {
	t.Run("poll canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				fmt.Fprint(w, `{"id":"750JOB"}`)
			} else {
				fmt.Fprint(w, `{"state":"InProgress"}`)
			}
		}, QueryOptions{Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return sleepContext(ctx, d)
		}})
		err := q.BulkQuery(ctx, "SELECT Id FROM Account", []string{"Id"}, func(map[string]string) error { return nil })
		if !errors.Is(err, context.Canceled) || q.Usage().BulkPollCalls != 1 || q.Usage().BulkResultCalls != 0 {
			t.Fatalf("error=%v usage=%+v", err, q.Usage())
		}
	})
	t.Run("callback stops CSV before EOF", func(t *testing.T) {
		reader, writer := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fmt.Fprint(writer, "Id\n001\n")
			// Deliberately do not close the pipe until the callback returns.
		}()
		want := errors.New("sink failed")
		_, err := readBulkPage(context.Background(), reader, []string{"Id"}, func(map[string]string) error { return want })
		reader.Close()
		writer.Close()
		<-done
		if !errors.Is(err, want) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestQueryValidationAndBackoffCancellation(t *testing.T) {
	if _, err := NewQueryClient(nil, QueryOptions{}); err == nil {
		t.Fatal("nil provider accepted")
	}
	for _, options := range []QueryOptions{{MaxRetries: -1}, {PollInterval: -1}, {MaxRecords: -1}} {
		if _, err := NewQueryClient(func(context.Context, string) (*Client, error) { return nil, nil }, options); err == nil {
			t.Fatal("negative options accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, QueryOptions{Sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepContext(ctx, d)
	}})
	err := q.Query(ctx, "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
	if !errors.Is(err, context.Canceled) || q.Usage().TotalCalls() != 1 {
		t.Fatalf("error=%v usage=%+v", err, q.Usage())
	}
	if _, err := q.Describe(context.Background(), "../Account"); err == nil {
		t.Fatal("invalid object accepted")
	}
}

func TestBulkQueryRefreshAndReadRetries(t *testing.T) {
	creates, polls, results, refreshes := 0, 0, 0, 0
	q, client := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			creates++
			if creates == 1 {
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer fresh" {
				t.Error("creation did not use refreshed SID")
			}
			fmt.Fprint(w, `{"id":"750JOB"}`)
		case strings.HasSuffix(r.URL.Path, "/results"):
			results++
			if results == 1 {
				w.WriteHeader(503)
				return
			}
			if r.URL.Query().Get("maxRecords") != "100" {
				t.Error("MaxRecords override not used")
			}
			w.Header().Set("Sforce-Locator", "null")
			fmt.Fprint(w, "Id\n001\n")
		default:
			polls++
			if polls == 1 {
				w.WriteHeader(429)
				return
			}
			fmt.Fprint(w, `{"state":"JobComplete","numberRecordsProcessed":1}`)
		}
	}, QueryOptions{MaxRecords: 100})
	q.getClient = func(_ context.Context, rejected string) (*Client, error) {
		if rejected != "" {
			refreshes++
			client.SetSidLoc("fresh", client.GetLoc())
		}
		return client, nil
	}
	err := q.BulkQuery(context.Background(), "SELECT Id FROM Account", []string{"Id"}, func(map[string]string) error { return nil })
	u := q.Usage()
	if err != nil || refreshes != 1 || u.BulkJobsCreated != 2 || u.BulkPollCalls != 2 || u.BulkResultCalls != 2 || u.Retries != 2 || u.TotalCalls() != 6 {
		t.Fatalf("error=%v refreshes=%d usage=%+v", err, refreshes, u)
	}
}

func TestQueryInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}, QueryOptions{})
	err := q.Query(ctx, "SELECT Id FROM Account", func(map[string]interface{}) error { return nil })
	if !errors.Is(err, context.Canceled) || q.Usage().TotalCalls() != 1 || q.Usage().Retries != 0 {
		t.Fatalf("error=%v usage=%+v", err, q.Usage())
	}
}

func TestBulkEmptyResultsAndCallbackFailure(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					fmt.Fprint(w, `{"id":"750JOB"}`)
				case strings.HasSuffix(r.URL.Path, "/results"):
					w.Header().Set("Sforce-Locator", "null")
					fmt.Fprint(w, "Id\n")
					if count > 0 {
						fmt.Fprint(w, "001\n")
					}
				default:
					fmt.Fprintf(w, `{"state":"JobComplete","numberRecordsProcessed":%d}`, count)
				}
			}, QueryOptions{})
			want := errors.New("callback failed")
			err := q.BulkQuery(context.Background(), "SELECT Id FROM Account", []string{"Id"}, func(map[string]string) error { return want })
			if count == 0 && err != nil || count == 1 && !errors.Is(err, want) {
				t.Fatalf("error=%v", err)
			}
			if q.Usage().BulkResultCalls != 1 {
				t.Fatal("callback failure replayed a result")
			}
		})
	}
}

func TestDescribeAndProviderErrors(t *testing.T) {
	q, _ := testQueries(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `[{"errorCode":"INVALID_TYPE"}]`)
	}, QueryOptions{})
	_, err := q.Describe(context.Background(), "Product2")
	var sfErr SalesforceError
	if !errors.As(err, &sfErr) || sfErr.HttpCode != 400 || q.Usage().DescribeCalls != 1 {
		t.Fatalf("error=%v usage=%+v", err, q.Usage())
	}
	want := errors.New("get client failed")
	q.getClient = func(context.Context, string) (*Client, error) { return nil, want }
	if _, err := q.Describe(context.Background(), "Product2"); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
	q.getClient = func(context.Context, string) (*Client, error) { return nil, nil }
	if _, err := q.Describe(context.Background(), "Product2"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("error=%v", err)
	}
}

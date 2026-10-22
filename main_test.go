package main

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gaugeVecValue returns the current value of a GaugeVec for the given labels.
func gaugeVecValue(gv *prometheus.GaugeVec, labels prometheus.Labels) float64 {
	registry := prometheus.NewRegistry()
	if err := registry.Register(gv); err != nil {
		return 0
	}
	families, err := registry.Gather()
	if err != nil {
		return 0
	}
	for _, family := range families {
		for _, m := range family.GetMetric() {
			got := labelsFromMetric(m)
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestStringOrBool_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected StringOrBool
		wantErr  bool
	}{
		{
			name:     "null value",
			input:    `null`,
			expected: "unknown",
			wantErr:  false,
		},
		{
			name:     "string value",
			input:    `"batch_123"`,
			expected: "batch_123",
			wantErr:  false,
		},
		{
			name:     "bool true",
			input:    `true`,
			expected: "true",
			wantErr:  false,
		},
		{
			name:     "bool false",
			input:    `false`,
			expected: "false",
			wantErr:  false,
		},
		{
			name:     "invalid type",
			input:    `123`,
			expected: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s StringOrBool
			err := json.Unmarshal([]byte(tt.input), &s)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, s)
			}
		})
	}
}

func TestFloatOrString_UnmarshalJSON_String(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected FloatOrString
		wantErr  bool
	}{
		{
			name:     "string value",
			input:    "123.45",
			expected: 123.45,
			wantErr:  false,
		},
		{
			name:     "string value many zeros",
			input:    "0.1739150000000000000000000000",
			expected: 0.173915,
			wantErr:  false,
		},
		{
			name:     "int as string",
			input:    "73",
			expected: 73.0,
			wantErr:  false,
		},
		{
			name:     "invalid string value",
			input:    "foo",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "invalid type",
			input:    `[]`,
			expected: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FloatOrString
			err := json.Unmarshal([]byte(tt.input), &f)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, f)
			}
		})
	}
}

func TestFloatOrString_UnmarshalJSON_Float(t *testing.T) {
	tests := []struct {
		name     string
		input    float64
		expected FloatOrString
		wantErr  bool
	}{
		{
			name:     "2 decimals",
			input:    123.45,
			expected: 123.45,
			wantErr:  false,
		},
		{
			name:     "6 decimals",
			input:    0.173915,
			expected: 0.173915,
			wantErr:  false,
		},
		{
			name:     "int",
			input:    73,
			expected: 73.0,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FloatOrString
			b, errMarshal := json.Marshal(tt.input)
			assert.NoError(t, errMarshal)
			err := json.Unmarshal(b, &f)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, f)
			}
		})
	}
}

func TestMoney_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected Money
		wantErr  bool
	}{
		{
			name:  "string value",
			input: `{"currency": "usd", "value": "0.1739150000000000000000000000"}`,
			expected: Money{
				Value:    0.173915,
				Currency: "usd",
			},
			wantErr: false,
		},
		{
			name:  "float64 value",
			input: `{"currency": "usd", "value": 0.173915}`,
			expected: Money{
				Value:    0.173915,
				Currency: "usd",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Money
			err := json.Unmarshal([]byte(tt.input), &m)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, m)
			}
		})
	}
}

func TestDeref(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{
			name:     "nil pointer",
			input:    nil,
			expected: "unknown",
		},
		{
			name:     "valid string",
			input:    strPtr("test-value"),
			expected: "test-value",
		},
		{
			name:     "empty string",
			input:    strPtr(""),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deref(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMergeLabels(t *testing.T) {
	base := prometheus.Labels{
		"model":     "gpt-4",
		"operation": "completions",
	}

	result := mergeLabels(base, "token_type", "input")

	assert.Len(t, result, 3)
	assert.Equal(t, "gpt-4", result["model"])
	assert.Equal(t, "completions", result["operation"])
	assert.Equal(t, "input", result["token_type"])

	assert.Len(t, base, 2)
}

func TestUpdateMetric(t *testing.T) {
	usageState = make(map[string]float64)
	lastScrape = 0

	labels := prometheus.Labels{
		"model":        "gpt-4",
		"operation":    "completions",
		"project_id":   "proj-123",
		"project_name": "test-project",
		"user_id":      "user-456",
		"api_key_id":   "key-789",
		"api_key_name": "key-name",
		"batch":        "false",
	}

	now := time.Now().Unix()
	bucketStart := now - 120
	bucketEnd := now - 60

	t.Run("processes completed bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		updateMetric(labels, "input", bucketStart, bucketEnd, 100.0)
		assert.Len(t, usageState, 1)
	})

	t.Run("skips incomplete bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		futureEnd := now + 60
		updateMetric(labels, "input", bucketStart, futureEnd, 100.0)
		assert.Len(t, usageState, 0)
	})

	t.Run("skips already processed bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		updateMetric(labels, "input", bucketStart, bucketEnd, 100.0)
		initialLen := len(usageState)
		updateMetric(labels, "input", bucketStart, bucketEnd, 200.0)
		assert.Len(t, usageState, initialLen)
	})
}

func TestNewExporter(t *testing.T) {
	t.Run("missing OPENAI_SECRET_KEY", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "")
		t.Setenv("OPENAI_ORG_ID", "org-123")

		_, err := NewExporter()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "OPENAI_SECRET_KEY")
	})

	t.Run("missing OPENAI_ORG_ID", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "sk-test")
		t.Setenv("OPENAI_ORG_ID", "")

		_, err := NewExporter()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "OPENAI_ORG_ID")
	})

	t.Run("valid environment", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "sk-test")
		t.Setenv("OPENAI_ORG_ID", "org-123")

		exporter, err := NewExporter()
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NotNil(t, exporter.client)
		assert.Equal(t, "sk-test", exporter.apiKey)
		assert.Equal(t, "org-123", exporter.orgID)
	})
}

func TestEnsureProjectName(t *testing.T) {
	projectNames = make(map[string]string)

	t.Run("empty project id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName("")
		assert.Equal(t, "unknown", result)
	})

	t.Run("unknown project id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName("unknown")
		assert.Equal(t, "unknown", result)
	})

	t.Run("cached project name", func(t *testing.T) {
		projectNames = make(map[string]string)
		projectNames["proj-123"] = "cached-project"

		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName("proj-123")
		assert.Equal(t, "cached-project", result)
	})

	t.Run("fetch project name from API", func(t *testing.T) {
		projectNames = make(map[string]string)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Project{Name: "fetched-project"})
		}))
		defer server.Close()

		client := &http.Client{}

		req, _ := http.NewRequest("GET", server.URL, nil)
		req.Header.Set("Authorization", "Bearer test-key")

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var proj Project
		err = json.NewDecoder(resp.Body).Decode(&proj)
		require.NoError(t, err)

		assert.Equal(t, "fetched-project", proj.Name)
	})

	t.Run("API error returns unknown", func(t *testing.T) {
		projectNames = make(map[string]string)

		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		result := e.ensureProjectName("proj-timeout")
		assert.Equal(t, "unknown", result)
	})
}

func TestEnsureAPIKeyName(t *testing.T) {
	apiKeyNames = make(map[string]string)

	t.Run("empty api key id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName("proj-123", "")
		assert.Equal(t, "unknown", result)
	})

	t.Run("unknown api key id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName("proj-123", "unknown")
		assert.Equal(t, "unknown", result)
	})

	t.Run("cached api key name", func(t *testing.T) {
		apiKeyNames = make(map[string]string)
		apiKeyNames["key-123"] = "cached-key"

		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName("proj-123", "key-123")
		assert.Equal(t, "cached-key", result)
	})

	t.Run("fetch api key name from API (parsing only)", func(t *testing.T) {
		apiKeyNames = make(map[string]string)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(APIKey{Name: "fetched-key"})
		}))
		defer server.Close()

		client := &http.Client{}
		req, _ := http.NewRequest("GET", server.URL, nil)
		req.Header.Set("Authorization", "Bearer test-key")

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var k APIKey
		err = json.NewDecoder(resp.Body).Decode(&k)
		require.NoError(t, err)

		assert.Equal(t, "fetched-key", k.Name)
	})

	t.Run("API error returns unknown", func(t *testing.T) {
		apiKeyNames = make(map[string]string)

		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		result := e.ensureAPIKeyName("proj-any", "key-timeout")
		assert.Equal(t, "unknown", result)
	})
}

func TestFetchUsageData_ErrorCases(t *testing.T) {
	t.Run("HTTP request error", func(t *testing.T) {
		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		endpoint := UsageEndpoint{Path: "completions", Name: "completions"}
		err := e.fetchUsageData(endpoint, 1000, 2000)
		assert.Error(t, err)
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					return url.Parse(server.URL)
				},
			},
		}

		e := &Exporter{
			client: client,
			apiKey: "test-key",
		}

		endpoint := UsageEndpoint{Path: "completions", Name: "completions"}
		err := e.fetchUsageData(endpoint, 1000, 2000)
		assert.Error(t, err)
	})
}

func TestFetchCostData_ErrorCases(t *testing.T) {
	t.Run("HTTP request error", func(t *testing.T) {
		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		_, err := e.fetchCostData(1000, 2000)
		assert.Error(t, err)
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					return url.Parse(server.URL)
				},
			},
		}

		e := &Exporter{
			client: client,
			apiKey: "test-key",
		}

		_, err := e.fetchCostData(1000, 2000)
		assert.Error(t, err)
	})
}

func TestUpdateTodayCost_SetsTodayCostMetric(t *testing.T) {
	projectNames = map[string]string{"proj-123": "demo-project"}

	fixedNow := time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)
	originalNow := utcNow
	utcNow = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		utcNow = originalNow
	})

	t.Run("sets today cost for today's bucket", func(t *testing.T) {
		resetCostState(t)

		bucketStart := fixedNow.Unix()
		response := CostsList{
			Object: "list",
			Data: []CostBucket{{
				StartTime: bucketStart,
				EndTime:   bucketStart + 60,
				Results: []CostResult{{
					Amount: Money{
						Value:    FloatOrString(12.34),
						Currency: "usd",
					},
					LineItem:       strPtr("GPT-4 Turbo"),
					ProjectID:      strPtr("proj-123"),
					OrganizationID: "org-123",
				}},
			}},
		}

		e := newCostTestExporter(t, response)
		require.NoError(t, e.updateTodayCost())

		val := gaugeVecValue(todayCostUSD, prometheus.Labels{
			"project_id":      "proj-123",
			"project_name":    "demo-project",
			"line_item":       "GPT-4 Turbo",
			"organization_id": "org-123",
			"currency":        "usd",
		})
		assert.InDelta(t, 12.34, val, 0.001)
	})

	t.Run("skips non-today buckets", func(t *testing.T) {
		resetCostState(t)

		yesterdayStart := fixedNow.Add(-24 * time.Hour).Unix()
		response := CostsList{
			Object: "list",
			Data: []CostBucket{{
				StartTime: yesterdayStart,
				EndTime:   yesterdayStart + 60,
				Results: []CostResult{{
					Amount: Money{
						Value:    FloatOrString(1.23),
						Currency: "usd",
					},
					LineItem:       strPtr("GPT-4 Turbo"),
					ProjectID:      strPtr("proj-123"),
					OrganizationID: "org-123",
				}},
			}},
		}

		e := newCostTestExporter(t, response)
		// Only yesterday's bucket: an unusable response, reported as an error, gauge untouched.
		require.Error(t, e.updateTodayCost())

		assert.Empty(t, gaugeVecSeries(todayCostUSD))
	})
}

func TestTodayCostGaugeVec(t *testing.T) {
	todayCostUSD.Reset()

	labels := prometheus.Labels{
		"project_id":      "proj-123",
		"project_name":    "demo-project",
		"line_item":       "GPT-4 Turbo",
		"organization_id": "org-123",
		"currency":        "usd",
	}
	todayCostUSD.With(labels).Set(42.5)

	val := gaugeVecValue(todayCostUSD, labels)
	assert.InDelta(t, 42.5, val, 0.001)
}

func newCostTestExporter(t *testing.T, response CostsList) *Exporter {
	t.Helper()
	return newTestExporter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(response))
	}))
}

// newTestExporter returns an Exporter whose requests to api.openai.com are routed to handler.
func newTestExporter(t *testing.T, handler http.Handler) *Exporter {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	// Keep enough idle connections for the concurrent usage endpoints so loop tests reuse
	// sockets instead of opening (and leaving in TIME_WAIT) new ones for every window.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2 * len(usageEndpoints)
	t.Cleanup(transport.CloseIdleConnections)

	return &Exporter{
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				cloned.URL.Scheme = serverURL.Scheme
				cloned.URL.Host = serverURL.Host
				return transport.RoundTrip(cloned)
			}),
		},
		apiKey: "test-key",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func labelsFromMetric(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}

func strPtr(s string) *string {
	return &s
}

// ---- Collection loop and cost refresh tests ----

// fakeClock is a manually advanced clock; Sleep advances it instantly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	// onSleep, if set, runs after every Sleep with the new time (used to stop loops).
	onSleep func(now time.Time)
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	if c.onSleep != nil {
		c.onSleep(c.Now())
	}
	return ctx.Err()
}

// installFakeClock replaces utcNow and sleepCtx with a fake clock for the test.
func installFakeClock(t *testing.T, start time.Time) *fakeClock {
	t.Helper()
	clock := &fakeClock{now: start}
	origNow, origSleep := utcNow, sleepCtx
	utcNow = clock.Now
	sleepCtx = clock.Sleep
	t.Cleanup(func() {
		utcNow, sleepCtx = origNow, origSleep
	})
	return clock
}

// setFlag overrides a flag value for the duration of the test.
func setFlag[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func setLastScrape(t *testing.T, v int64) {
	t.Helper()
	old := lastScrape
	lastScrape = v
	t.Cleanup(func() { lastScrape = old })
}

func resetCostState(t *testing.T) {
	t.Helper()
	costMu.Lock()
	costDay = ""
	costMu.Unlock()
	todayCostUSD.Reset()
	t.Cleanup(func() {
		costMu.Lock()
		costDay = ""
		costMu.Unlock()
		todayCostUSD.Reset()
	})
}

// gaugeVecSeries returns the label sets of all series currently in gv.
func gaugeVecSeries(gv *prometheus.GaugeVec) []map[string]string {
	registry := prometheus.NewRegistry()
	if err := registry.Register(gv); err != nil {
		return nil
	}
	families, err := registry.Gather()
	if err != nil {
		return nil
	}
	var out []map[string]string
	for _, family := range families {
		for _, m := range family.GetMetric() {
			out = append(out, labelsFromMetric(m))
		}
	}
	return out
}

type usageCall struct {
	endpoint   string
	start, end int64
	at         time.Time
}

// usageRecorder is a fake usage API that records every call with the fake time it arrived at.
// Each request advances the fake clock by the duration returned from latency, simulating slow calls.
type usageRecorder struct {
	mu      sync.Mutex
	clock   *fakeClock
	calls   []usageCall
	latency func(call usageCall) time.Duration
}

func (u *usageRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, _ := strconv.ParseInt(q.Get("start_time"), 10, 64)
	end, _ := strconv.ParseInt(q.Get("end_time"), 10, 64)
	call := usageCall{
		endpoint: strings.TrimPrefix(r.URL.Path, "/v1/organization/usage/"),
		start:    start,
		end:      end,
	}
	u.mu.Lock()
	call.at = u.clock.Now()
	u.calls = append(u.calls, call)
	u.mu.Unlock()
	if u.latency != nil {
		u.clock.Advance(u.latency(call))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"object":"page","data":[],"has_more":false}`))
}

func (u *usageRecorder) snapshot() []usageCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]usageCall(nil), u.calls...)
}

type usageWindow struct {
	start, end int64
	firstAt    time.Time
	endpoints  map[string]int
}

// windows groups calls by window, in the order windows were first requested.
func windowsOf(calls []usageCall) []usageWindow {
	var out []usageWindow
	idx := map[int64]int{}
	for _, c := range calls {
		i, ok := idx[c.start]
		if !ok {
			i = len(out)
			idx[c.start] = i
			out = append(out, usageWindow{start: c.start, end: c.end, firstAt: c.at, endpoints: map[string]int{}})
		}
		if c.at.Before(out[i].firstAt) {
			out[i].firstAt = c.at
		}
		out[i].endpoints[c.endpoint]++
	}
	return out
}

// assertContiguousWindows checks windows are fetched oldest first, step-sized, with no gaps or repeats,
// and that every endpoint was queried exactly once per window.
func assertContiguousWindows(t *testing.T, windows []usageWindow, firstStart, step int64) {
	t.Helper()
	require.NotEmpty(t, windows)
	for i, w := range windows {
		assert.Equal(t, firstStart+int64(i)*step, w.start, "window %d start", i)
		assert.Equal(t, w.start+step, w.end, "window %d end", i)
		assert.Len(t, w.endpoints, len(usageEndpoints), "window %d endpoints", i)
		for ep, n := range w.endpoints {
			assert.Equal(t, 1, n, "window %d endpoint %s calls", i, ep)
		}
	}
}

// maxRequestsPerMinute returns the largest number of calls in any sliding 60s window.
func maxRequestsPerMinute(calls []usageCall) int {
	times := make([]time.Time, len(calls))
	for i, c := range calls {
		times[i] = c.at
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	best, j := 0, 0
	for i := range times {
		for times[j].Add(time.Minute).Compare(times[i]) <= 0 {
			j++
		}
		if n := i - j + 1; n > best {
			best = n
		}
	}
	return best
}

func setDefaultCollectFlags(t *testing.T) {
	t.Helper()
	setFlag(t, scrapeInterval, time.Minute)
	setFlag(t, scrapeSettleDelay, time.Minute)
	setFlag(t, scrapeMaxCatchup, 60)
	setFlag(t, scrapeCatchupSpacing, 20*time.Second)
}

func TestCollectUsage_NoDriftWhenFetchesTakeTime(t *testing.T) {
	setDefaultCollectFlags(t)
	start := time.Date(2026, time.June, 17, 12, 0, 30, 0, time.UTC)
	clock := installFakeClock(t, start)
	setLastScrape(t, initialWindowStart(start))
	firstStart := lastScrape

	// Every request takes 1s of fake time, so each window fetch takes ~7s. The old
	// "fetch then sleep(interval)" loop drifted by exactly that much every cycle.
	rec := &usageRecorder{clock: clock, latency: func(usageCall) time.Duration { return time.Second }}
	e := newTestExporter(t, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopAt := start.Add(6 * time.Hour)
	clock.onSleep = func(now time.Time) {
		if !now.Before(stopAt) {
			cancel()
		}
	}
	e.collectUsage(ctx)

	windows := windowsOf(rec.snapshot())
	assertContiguousWindows(t, windows, firstStart, 60)
	assert.GreaterOrEqual(t, len(windows), 6*60)

	// Window 0 is the startup window, fetched immediately on start.
	for i, w := range windows[1:] {
		i++
		due := time.Unix(w.end, 0).Add(*scrapeSettleDelay)
		// Each window is fetched exactly when it becomes due: never early, never late.
		assert.True(t, w.firstAt.Equal(due), "window %d fetched at %s, due %s", i, w.firstAt, due)
	}
	// After 6h the collector is still within one interval of real time.
	lag := clock.Now().Add(-*scrapeSettleDelay).Sub(time.Unix(lastScrape, 0))
	assert.LessOrEqual(t, lag, 2*time.Minute)
}

func TestCollectUsage_RecoversWhenCyclesTakeLongerThanInterval(t *testing.T) {
	setDefaultCollectFlags(t)
	start := time.Date(2026, time.June, 17, 12, 0, 30, 0, time.UTC)
	clock := installFakeClock(t, start)
	setLastScrape(t, initialWindowStart(start))
	firstStart := lastScrape
	slowWindow := firstStart + 10*60

	// One window's completions call hangs for 5 minutes, so that cycle takes far longer than the interval.
	rec := &usageRecorder{clock: clock, latency: func(c usageCall) time.Duration {
		if c.start == slowWindow && c.endpoint == "completions" {
			return 5 * time.Minute
		}
		return 500 * time.Millisecond
	}}
	e := newTestExporter(t, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopAt := start.Add(1 * time.Hour)
	clock.onSleep = func(now time.Time) {
		if !now.Before(stopAt) {
			cancel()
		}
	}
	e.collectUsage(ctx)

	calls := rec.snapshot()
	windows := windowsOf(calls)
	assertContiguousWindows(t, windows, firstStart, 60)

	// Windows that piled up during the slow cycle are fetched without waiting for their own
	// interval, only spaced by the catch-up throttle.
	var catchUp []usageWindow
	for _, w := range windows {
		if w.start > slowWindow && w.firstAt.After(time.Unix(w.end, 0).Add(*scrapeSettleDelay)) {
			catchUp = append(catchUp, w)
		}
	}
	require.GreaterOrEqual(t, len(catchUp), 4)
	for i := 1; i < len(catchUp); i++ {
		assert.GreaterOrEqual(t, catchUp[i].firstAt.Sub(catchUp[i-1].firstAt), *scrapeCatchupSpacing)
	}

	// Once caught up, windows are fetched exactly when due again and the lag is gone.
	last := windows[len(windows)-1]
	assert.True(t, last.firstAt.Equal(time.Unix(last.end, 0).Add(*scrapeSettleDelay)))
	lag := clock.Now().Add(-*scrapeSettleDelay).Sub(time.Unix(lastScrape, 0))
	assert.LessOrEqual(t, lag, 2*time.Minute)
	assert.LessOrEqual(t, maxRequestsPerMinute(calls), orgRateLimitPerMinute)
}

func TestRunUsageCycle_CatchUpRespectsCapAndThrottle(t *testing.T) {
	setDefaultCollectFlags(t)
	setFlag(t, scrapeMaxCatchup, 10)
	now := time.Date(2026, time.June, 17, 12, 0, 30, 0, time.UTC)
	clock := installFakeClock(t, now)
	// 100 settled windows are pending (e.g. after an outage).
	firstStart := initialWindowStart(now) - 99*60
	setLastScrape(t, firstStart)

	rec := &usageRecorder{clock: clock}
	e := newTestExporter(t, rec)

	caughtUp := e.runUsageCycle(context.Background())
	assert.False(t, caughtUp)

	calls := rec.snapshot()
	windows := windowsOf(calls)
	require.Len(t, windows, 10, "cycle must stop at scrape.max-catchup-windows")
	assertContiguousWindows(t, windows, firstStart, 60)
	assert.Equal(t, firstStart+10*60, lastScrape)

	// The first window starts immediately; later ones are spaced by the throttle.
	assert.True(t, windows[0].firstAt.Equal(now))
	for i := 1; i < len(windows); i++ {
		assert.Equal(t, *scrapeCatchupSpacing, windows[i].firstAt.Sub(windows[i-1].firstAt), "window %d spacing", i)
	}
	// 20s spacing * 7 endpoints = at most 21 requests in any minute, under the 30/min limit.
	assert.LessOrEqual(t, maxRequestsPerMinute(calls), 21)

	// Continue for a bounded amount of fake time: the backlog is drained at ~2 windows/min net
	// (3 fetched, 1 produced), so 100 windows are caught up well within 2h.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopAt := now.Add(2 * time.Hour)
	clock.onSleep = func(t time.Time) {
		if !t.Before(stopAt) {
			cancel()
		}
	}
	e.collectUsage(ctx)

	calls = rec.snapshot()
	windows = windowsOf(calls)
	assertContiguousWindows(t, windows, firstStart, 60)
	assert.GreaterOrEqual(t, len(windows), 100)
	assert.LessOrEqual(t, maxRequestsPerMinute(calls), 21)
	lag := clock.Now().Add(-*scrapeSettleDelay).Sub(time.Unix(lastScrape, 0))
	assert.LessOrEqual(t, lag, 2*time.Minute, "backlog must be drained")
}

func TestRunUsageCycle_WaitsForSettleDelay(t *testing.T) {
	setDefaultCollectFlags(t)
	windowStart := time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)
	// The window [12:00, 12:01) ends at 12:01 and settles at 12:02.
	clock := installFakeClock(t, windowStart.Add(2*time.Minute-time.Second))
	setLastScrape(t, windowStart.Unix())

	rec := &usageRecorder{clock: clock}
	e := newTestExporter(t, rec)

	assert.True(t, e.runUsageCycle(context.Background()))
	assert.Empty(t, rec.snapshot(), "window must not be fetched before it settles")

	clock.Advance(time.Second)
	assert.True(t, e.runUsageCycle(context.Background()))
	windows := windowsOf(rec.snapshot())
	assertContiguousWindows(t, windows, windowStart.Unix(), 60)
	assert.Len(t, windows, 1)
}

func TestInitialWindowStart(t *testing.T) {
	setDefaultCollectFlags(t)
	now := time.Date(2026, time.June, 17, 12, 0, 30, 0, time.UTC)
	got := initialWindowStart(now)
	assert.Equal(t, time.Date(2026, time.June, 17, 11, 58, 0, 0, time.UTC).Unix(), got)
	// The first window is already settled; the one after it is not.
	settled := now.Add(-*scrapeSettleDelay).Unix()
	assert.LessOrEqual(t, got+60, settled)
	assert.Greater(t, got+120, settled)
}

func TestValidateFlags(t *testing.T) {
	setDefaultCollectFlags(t)
	setFlag(t, costInterval, 5*time.Minute)
	setFlag(t, httpTimeout, 30*time.Second)
	require.NoError(t, validateFlags())

	tests := []struct {
		name  string
		apply func(t *testing.T)
	}{
		{"sub-second interval", func(t *testing.T) { setFlag(t, scrapeInterval, 1500*time.Millisecond) }},
		{"negative settle delay", func(t *testing.T) { setFlag(t, scrapeSettleDelay, -time.Second) }},
		{"zero catch-up cap", func(t *testing.T) { setFlag(t, scrapeMaxCatchup, 0) }},
		{"zero spacing", func(t *testing.T) { setFlag(t, scrapeCatchupSpacing, 0) }},
		{"spacing above interval", func(t *testing.T) { setFlag(t, scrapeCatchupSpacing, 2*time.Minute) }},
		{"zero cost interval", func(t *testing.T) { setFlag(t, costInterval, 0) }},
		{"zero http timeout", func(t *testing.T) { setFlag(t, httpTimeout, 0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.apply(t)
			assert.Error(t, validateFlags())
		})
	}
}

func TestCatchupRequestsPerMinute_DefaultsStayUnderRateLimit(t *testing.T) {
	spacing := 20 * time.Second
	if f := flag.Lookup("scrape.catchup-spacing"); f != nil {
		d, err := time.ParseDuration(f.DefValue)
		require.NoError(t, err)
		spacing = d
	}
	rpm := catchupRequestsPerMinute(spacing)
	assert.InDelta(t, 21.0, rpm, 0.001)
	// Leave headroom for the cost request and project/API key name lookups.
	assert.LessOrEqual(t, rpm+1+2, float64(orgRateLimitPerMinute))
}

func TestNewExporter_UsesHTTPTimeout(t *testing.T) {
	t.Setenv("OPENAI_SECRET_KEY", "sk-test")
	t.Setenv("OPENAI_ORG_ID", "org-123")
	assert.Equal(t, "30s", flag.Lookup("http.timeout").DefValue)

	e, err := NewExporter()
	require.NoError(t, err)
	assert.Equal(t, *httpTimeout, e.client.Timeout)
}

func TestFetchUsageData_NonOKStatusIsError(t *testing.T) {
	e := newTestExporter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached"}}`))
	}))
	err := e.fetchUsageData(UsageEndpoint{Path: "completions", Name: "completions"}, 1000, 2000)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

func costBucket(day time.Time, results ...CostResult) CostBucket {
	return CostBucket{Object: "bucket", StartTime: day.Unix(), EndTime: day.Add(24 * time.Hour).Unix(), Results: results}
}

func costResult(project string, value float64) CostResult {
	return CostResult{
		Object:         "organization.costs.result",
		Amount:         Money{Value: FloatOrString(value), Currency: "usd"},
		ProjectID:      strPtr(project),
		OrganizationID: "org-123",
	}
}

func costLabels(project string) prometheus.Labels {
	return prometheus.Labels{
		"project_id":      project,
		"project_name":    project + "-name",
		"line_item":       "unknown",
		"organization_id": "org-123",
		"currency":        "usd",
	}
}

// costServer serves whatever response/status the test currently sets and records request queries.
type costServer struct {
	mu       sync.Mutex
	status   int
	response CostsList
	queries  []url.Values
	paths    []string
}

func (c *costServer) set(status int, response CostsList) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.response = status, response
}

func (c *costServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, r.URL.Query())
	c.paths = append(c.paths, r.URL.Path)
	if c.status != http.StatusOK {
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c.response)
}

func setupCostTest(t *testing.T, now time.Time) (*fakeClock, *costServer, *Exporter) {
	t.Helper()
	resetCostState(t)
	oldNames := projectNames
	projectNames = map[string]string{"proj-a": "proj-a-name", "proj-b": "proj-b-name"}
	t.Cleanup(func() { projectNames = oldNames })
	clock := installFakeClock(t, now)
	srv := &costServer{status: http.StatusOK}
	return clock, srv, newTestExporter(t, srv)
}

func TestUpdateTodayCost_QueryParams(t *testing.T) {
	now := time.Date(2026, time.June, 17, 13, 45, 10, 0, time.UTC)
	today := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	_, srv, e := setupCostTest(t, now)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{costBucket(today, costResult("proj-a", 1))}})

	require.NoError(t, e.updateTodayCost())
	require.Len(t, srv.queries, 1)
	q := srv.queries[0]
	assert.Equal(t, "/v1/organization/costs", srv.paths[0])
	assert.Equal(t, strconv.FormatInt(today.Unix(), 10), q.Get("start_time"))
	// end_time = end of the day bucket; end_time = now makes the real API return no bucket.
	assert.Equal(t, strconv.FormatInt(today.Add(24*time.Hour).Unix(), 10), q.Get("end_time"))
	assert.Equal(t, "1d", q.Get("bucket_width"))
	assert.Equal(t, "project_id", q.Get("group_by"))
}

func TestUpdateTodayCost_AtMidnightQueriesWholeDay(t *testing.T) {
	midnight := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	_, srv, e := setupCostTest(t, midnight)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{costBucket(midnight)}})

	require.NoError(t, e.updateTodayCost())
	q := srv.queries[0]
	assert.Equal(t, strconv.FormatInt(midnight.Unix(), 10), q.Get("start_time"))
	assert.Equal(t, strconv.FormatInt(midnight.Add(24*time.Hour).Unix(), 10), q.Get("end_time"))
}

func TestUpdateTodayCost_NoTodayBucketKeepsPreviousValues(t *testing.T) {
	now := time.Date(2026, time.June, 17, 13, 0, 0, 0, time.UTC)
	today := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	_, srv, e := setupCostTest(t, now)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{costBucket(today, costResult("proj-a", 4))}})
	require.NoError(t, e.updateTodayCost())

	for _, resp := range []CostsList{
		{Object: "page"}, // what the API returned for end_time = now
		{Object: "page", Data: []CostBucket{costBucket(today.Add(-24*time.Hour), costResult("proj-a", 9))}},
	} {
		srv.set(http.StatusOK, resp)
		err := e.updateTodayCost()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no bucket for 2026-06-17")
		assert.InDelta(t, 4.0, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)
	}
}

func TestUpdateTodayCost_UsesOnlyTodaysBucket(t *testing.T) {
	now := time.Date(2026, time.June, 17, 13, 0, 0, 0, time.UTC)
	today := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	_, srv, e := setupCostTest(t, now)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{
		costBucket(today.Add(-24*time.Hour), costResult("proj-a", 9), costResult("proj-b", 8)),
		costBucket(today, costResult("proj-a", 1.5)),
	}})

	require.NoError(t, e.updateTodayCost())
	assert.Len(t, gaugeVecSeries(todayCostUSD), 1)
	assert.InDelta(t, 1.5, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)
}

func TestUpdateTodayCost_DayRolloverResetsGauge(t *testing.T) {
	day1 := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	clock, srv, e := setupCostTest(t, day1.Add(23*time.Hour+58*time.Minute))
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{
		costBucket(day1, costResult("proj-a", 3), costResult("proj-b", 4)),
	}})
	require.NoError(t, e.updateTodayCost())
	require.Len(t, gaugeVecSeries(todayCostUSD), 2)

	// Same day, proj-b missing from the response: no reset, its series stays.
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{
		costBucket(day1, costResult("proj-a", 3.5)),
	}})
	require.NoError(t, e.updateTodayCost())
	assert.Len(t, gaugeVecSeries(todayCostUSD), 2)
	assert.InDelta(t, 3.5, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)

	// Next UTC day: only proj-a has spend, proj-b (yesterday only) must disappear.
	clock.Advance(5 * time.Minute)
	day2 := day1.Add(24 * time.Hour)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{
		costBucket(day2, costResult("proj-a", 0.25)),
	}})
	require.NoError(t, e.updateTodayCost())

	series := gaugeVecSeries(todayCostUSD)
	require.Len(t, series, 1)
	assert.Equal(t, "proj-a", series[0]["project_id"])
	assert.InDelta(t, 0.25, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)
	assert.Equal(t, strconv.FormatInt(day2.Unix(), 10), srv.queries[len(srv.queries)-1].Get("start_time"))
}

func TestUpdateTodayCost_ErrorKeepsPreviousValues(t *testing.T) {
	day1 := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	clock, srv, e := setupCostTest(t, day1.Add(12*time.Hour))
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{
		costBucket(day1, costResult("proj-a", 7)),
	}})
	require.NoError(t, e.updateTodayCost())

	srv.set(http.StatusInternalServerError, CostsList{})
	err := e.updateTodayCost()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.InDelta(t, 7.0, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)

	// A rate-limited response must not be mistaken for "no spend today" either.
	srv.set(http.StatusTooManyRequests, CostsList{})
	require.Error(t, e.updateTodayCost())
	assert.Len(t, gaugeVecSeries(todayCostUSD), 1)

	// Errors across the day boundary keep the previous values until a fetch succeeds.
	clock.Advance(13 * time.Hour)
	srv.set(http.StatusInternalServerError, CostsList{})
	require.Error(t, e.updateTodayCost())
	assert.InDelta(t, 7.0, gaugeVecValue(todayCostUSD, costLabels("proj-a")), 0.001)
}

func TestCollectCost_RefreshesOnInterval(t *testing.T) {
	setFlag(t, costInterval, 5*time.Minute)
	start := time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)
	clock, srv, e := setupCostTest(t, start)
	day := time.Date(2026, time.June, 17, 0, 0, 0, 0, time.UTC)
	srv.set(http.StatusOK, CostsList{Object: "page", Data: []CostBucket{costBucket(day, costResult("proj-a", 1))}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.onSleep = func(now time.Time) {
		if !now.Before(start.Add(20 * time.Minute)) {
			cancel()
		}
	}
	e.collectCost(ctx)

	// Fetches at 12:00, 12:05, 12:10, 12:15, each for the whole current UTC day bucket.
	require.Len(t, srv.queries, 4)
	for _, q := range srv.queries {
		assert.Equal(t, strconv.FormatInt(day.Unix(), 10), q.Get("start_time"))
		assert.Equal(t, strconv.FormatInt(day.Add(24*time.Hour).Unix(), 10), q.Get("end_time"))
	}
}

func TestCollectUsage_StuckClockDoesNotSpin(t *testing.T) {
	setDefaultCollectFlags(t)
	now := time.Date(2026, time.June, 17, 12, 0, 30, 0, time.UTC)
	clock := installFakeClock(t, now)
	// Far behind, and the clock never moves: neither sleeps nor requests advance it.
	setLastScrape(t, initialWindowStart(now)-500*60)

	rec := &usageRecorder{clock: clock}
	e := newTestExporter(t, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sleeps []time.Duration
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) >= 200 {
			cancel()
		}
		return ctx.Err()
	}
	e.collectUsage(ctx)

	// Every fetched window advances lastScrape, and requests are bounded by the number of sleeps
	// (one window per throttle sleep, plus at most one per cycle without a throttle wait).
	windows := windowsOf(rec.snapshot())
	assertContiguousWindows(t, windows, initialWindowStart(now)-500*60, 60)
	assert.LessOrEqual(t, len(windows), len(sleeps)+1)
	for _, d := range sleeps {
		assert.Greater(t, d, time.Duration(0))
	}

	// Once caught up with a stuck clock, each cycle still sleeps at least minUsageLoopSleep.
	setLastScrape(t, initialWindowStart(now)+60)
	sleeps = nil
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) >= 50 {
			cancel2()
		}
		return ctx.Err()
	}
	before := len(rec.snapshot())
	e.collectUsage(ctx2)
	assert.Equal(t, before, len(rec.snapshot()), "no window is due, so nothing is fetched")
	for _, d := range sleeps {
		assert.GreaterOrEqual(t, d, minUsageLoopSleep)
	}
}

func TestUsageEndpoints_GroupByMatchesAPI(t *testing.T) {
	got := map[string]string{}
	for _, ep := range usageEndpoints {
		got[ep.Path] = ep.GroupBy
	}
	// The usage API returns 400 "Invalid group_by value: batch" for every endpoint but completions.
	assert.Equal(t, "project_id,user_id,api_key_id,model,batch", got["completions"])
	assert.Equal(t, "project_id", got["vector_stores"])
	for _, p := range []string{"embeddings", "moderations", "images", "audio_speeches", "audio_transcriptions"} {
		assert.Equal(t, defaultUsageGroupBy, got[p], p)
		assert.NotContains(t, got[p], "batch", p)
	}
}

func TestFetchUsageData_SendsEndpointGroupBy(t *testing.T) {
	var seen string
	e := newTestExporter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("group_by")
		_, _ = w.Write([]byte(`{"object":"page","data":[],"has_more":false}`))
	}))
	require.NoError(t, e.fetchUsageData(UsageEndpoint{Path: "vector_stores", Name: "vector_stores", GroupBy: "project_id"}, 1000, 2000))
	assert.Equal(t, "project_id", seen)
	require.NoError(t, e.fetchUsageData(UsageEndpoint{Path: "embeddings", Name: "embeddings"}, 1000, 2000))
	assert.Equal(t, defaultUsageGroupBy, seen, "empty GroupBy falls back to the default")
}

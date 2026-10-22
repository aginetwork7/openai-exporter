package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// Custom Type for Batch Field

// StringOrBool is a custom type that unmarshals a JSON value that may be either a string, a bool, or null.
type StringOrBool string

// UnmarshalJSON implements the json.Unmarshaler interface for StringOrBool.
func (s *StringOrBool) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = "unknown"
		return nil
	}
	if b[0] == '"' {
		var tmp string
		if err := json.Unmarshal(b, &tmp); err != nil {
			return err
		}
		*s = StringOrBool(tmp)
		return nil
	}
	var tmp bool
	if err := json.Unmarshal(b, &tmp); err == nil {
		if tmp {
			*s = "true"
		} else {
			*s = "false"
		}
		return nil
	}
	return fmt.Errorf("unsupported type for StringOrBool: %s", string(b))
}

type FloatOrString float64

func (f *FloatOrString) UnmarshalJSON(data []byte) error {
	var floatVal float64
	if err := json.Unmarshal(data, &floatVal); err == nil {
		*f = FloatOrString(floatVal)
		return nil
	}

	var strVal string
	if err := json.Unmarshal(data, &strVal); err != nil {
		return err
	}

	floatVal, err := strconv.ParseFloat(strVal, 64)
	if err != nil {
		return err
	}

	*f = FloatOrString(floatVal)
	return nil
}

// Global Variables and State

var (
	stateMu sync.RWMutex
	// usageState stores already processed buckets to avoid double counting.
	usageState   = make(map[string]float64)
	lastScrape   = int64(0)
	projectNames = make(map[string]string) // mapping project_id -> project_name
	apiKeyNames  = make(map[string]string) // mapping api_key_id -> api_key_name
	utcNow       = time.Now                // overridable for testing
	// sleepCtx waits for d or until ctx is cancelled; overridable for testing with a fake clock.
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}

	costMu sync.Mutex
	// costDay is the UTC date (YYYY-MM-DD) whose spend todayCostUSD currently holds.
	costDay string
)

// orgRateLimitPerMinute is the request limit observed on OpenAI organization endpoints
// ("30 requests per 1 minute"). It is used to validate the catch-up throttle.
const orgRateLimitPerMinute = 30

// minUsageLoopSleep is the minimum pause between usage collection cycles. Each cycle either
// advances lastScrape (bounded by scrape.max-catchup-windows) or sleeps, so even with a stuck or
// jumping clock the loop can never spin on the API.
const minUsageLoopSleep = time.Second

// Prometheus Metric and CLI Flags

type UsageEndpoint struct {
	Path string // API endpoint path (e.g. "completions")
	Name string // Name of the operation (e.g. "completions")
}

var (
	listenAddress = flag.String("web.listen-address", ":9185", "Address to listen on for web interface and telemetry")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	// API polling interval; also the size of each usage time window.
	scrapeInterval = flag.Duration("scrape.interval", 1*time.Minute, "Interval for API calls and usage data window")
	// Usage buckets can show up in the API a little after their minute has ended, so a window
	// is only fetched once it has been closed for at least this long.
	scrapeSettleDelay = flag.Duration("scrape.settle-delay", 1*time.Minute, "How long after a usage window has ended before it is fetched")
	scrapeMaxCatchup  = flag.Int("scrape.max-catchup-windows", 60, "Maximum number of usage windows fetched in one collection cycle when behind")
	// Every usage window costs one request per usage endpoint, so the spacing bounds the request rate while catching up.
	scrapeCatchupSpacing = flag.Duration("scrape.catchup-spacing", 20*time.Second, "Minimum time between the starts of consecutive usage window fetches (rate-limit guard while catching up)")
	costInterval         = flag.Duration("cost.interval", 5*time.Minute, "Interval for fetching today's cost")
	httpTimeout          = flag.Duration("http.timeout", 30*time.Second, "Timeout for each OpenAI API request")
	logLevel             = flag.String("log.level", "info", "Log level")

	usageEndpoints = []UsageEndpoint{
		{Path: "completions", Name: "completions"},
		{Path: "embeddings", Name: "embeddings"},
		{Path: "moderations", Name: "moderations"},
		{Path: "images", Name: "images"},
		{Path: "audio_speeches", Name: "audio_speeches"},
		{Path: "audio_transcriptions", Name: "audio_transcriptions"},
		{Path: "vector_stores", Name: "vector_stores"},
	}

	tokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_api_tokens_total",
			Help: "Total number of tokens used per model, operation, project, user, API key, batch and token type",
		},
		[]string{"model", "operation", "project_id", "project_name", "user_id", "api_key_id", "api_key_name", "batch", "token_type"},
	)
	todayCostUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openai_api_today_cost",
			Help: "Today's spend by project/line_item/organization (currency indicated by label).",
		},
		[]string{"project_id", "project_name", "line_item", "organization_id", "currency"},
	)
)

func init() {
	prometheus.MustRegister(tokensTotal)
	prometheus.MustRegister(todayCostUSD)
}

func setupLogging() {
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	logrus.Infof("Log level set to %s", level)
}

// Exporter and API Structures

type Exporter struct {
	client *http.Client
	apiKey string
	orgID  string
	// lastWindowAt is when the most recent usage window fetch started; used to throttle catch-up.
	lastWindowAt time.Time
}

type APIResponse struct {
	Object   string   `json:"object"`
	Data     []Bucket `json:"data"`
	HasMore  bool     `json:"has_more"`
	NextPage string   `json:"next_page"`
}

type Bucket struct {
	Object    string        `json:"object"`
	StartTime int64         `json:"start_time"`
	EndTime   int64         `json:"end_time"`
	Results   []UsageResult `json:"results"`
}

type UsageResult struct {
	Object            string       `json:"object"`
	InputTokens       int64        `json:"input_tokens"`
	OutputTokens      int64        `json:"output_tokens"`
	InputCachedTokens int64        `json:"input_cached_tokens"`
	InputAudioTokens  int64        `json:"input_audio_tokens"`
	OutputAudioTokens int64        `json:"output_audio_tokens"`
	NumModelRequests  int64        `json:"num_model_requests"`
	ProjectID         *string      `json:"project_id"`
	UserID            *string      `json:"user_id"`
	APIKeyID          *string      `json:"api_key_id"`
	Model             *string      `json:"model"`
	Batch             StringOrBool `json:"batch"`
}

type Project struct {
	Name string `json:"name"`
}

type APIKey struct {
	Name string `json:"name"`
}

type CostsList struct {
	Object   string       `json:"object"`
	Data     []CostBucket `json:"data"`
	HasMore  bool         `json:"has_more"`
	NextPage string       `json:"next_page"`
}

type CostBucket struct {
	Object    string       `json:"object"`
	StartTime int64        `json:"start_time"`
	EndTime   int64        `json:"end_time"`
	Results   []CostResult `json:"results"`
}

type Money struct {
	Value    FloatOrString `json:"value"`
	Currency string        `json:"currency"`
}

type CostResult struct {
	Object         string  `json:"object"`
	Amount         Money   `json:"amount"`
	LineItem       *string `json:"line_item"`
	ProjectID      *string `json:"project_id"`
	OrganizationID string  `json:"organization_id"`
}

func NewExporter() (*Exporter, error) {
	apiKey := os.Getenv("OPENAI_SECRET_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_SECRET_KEY environment variable is not set")
	}
	orgID := os.Getenv("OPENAI_ORG_ID")
	if orgID == "" {
		return nil, fmt.Errorf("OPENAI_ORG_ID environment variable is not set")
	}
	return &Exporter{
		client: &http.Client{Timeout: *httpTimeout},
		apiKey: apiKey,
		orgID:  orgID,
	}, nil
}

// Helper Functions for State and Metrics

func mergeLabels(base prometheus.Labels, key, value string) prometheus.Labels {
	newLabels := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		newLabels[k] = v
	}
	newLabels[key] = value
	return newLabels
}

// updateMetric updates the metric for a given token type.
// If the bucket is completed (bucketEnd <= current time) and has not been processed yet,
// its value is added to the counter, and the bucket information is saved in usageState.
func updateMetric(labels prometheus.Labels, tokenType string, bucketStart, bucketEnd int64, newValue float64) {
	compositeKey := strings.Join([]string{
		labels["operation"],
		fmt.Sprintf("%d", bucketStart),
		labels["project_id"],
		labels["user_id"],
		labels["api_key_id"],
		labels["model"],
		labels["batch"],
		tokenType,
	}, "|")

	now := utcNow().Unix()
	// Update the metric only if the bucket is completed.
	if bucketEnd > now {
		logrus.Debugf("Bucket %s is not yet completed (bucketEnd: %d, now: %d), skipping", compositeKey, bucketEnd, now)
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	// If the bucket has already been processed, it is not updated again.
	if _, exists := usageState[compositeKey]; exists {
		logrus.Debugf("Bucket %s has already been processed, skipping", compositeKey)
		return
	}

	tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(newValue)
	usageState[compositeKey] = newValue
}

// Data Collection

func (e *Exporter) fetchUsageData(endpoint UsageEndpoint, startTime, endTime int64) error {
	baseURL := fmt.Sprintf("https://api.openai.com/v1/organization/usage/%s", endpoint.Path)
	nextPage := ""

	allResults := []UsageResult{}

	for {
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1m&limit=1440&group_by=project_id,user_id,api_key_id,model,batch",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching usage data: %s", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("error fetching usage data: %w", err)
		}
		// Error bodies (e.g. 429 rate limit) decode into an empty page, so reject them explicitly.
		if err := checkStatus(resp); err != nil {
			return fmt.Errorf("error fetching usage data from %s: %w", endpoint.Path, err)
		}

		var response APIResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&response)
		closeErr := resp.Body.Close()

		if decodeErr != nil {
			return fmt.Errorf("error decoding response: %w", decodeErr)
		}
		if closeErr != nil {
			logrus.WithError(closeErr).Warn("failed to close response body")
		}
		logrus.Debugf("Received response: %+v", response)

		for _, bucket := range response.Data {
			if len(bucket.Results) > 0 {
				logrus.Debugf("Results %+v", bucket.Results)
			}
			for _, result := range bucket.Results {
				allResults = append(allResults, result)

				labels := prometheus.Labels{
					"model":        deref(result.Model),
					"operation":    endpoint.Name,
					"project_id":   deref(result.ProjectID),
					"project_name": e.ensureProjectName(deref(result.ProjectID)),
					"user_id":      deref(result.UserID),
					"api_key_id":   deref(result.APIKeyID),
					"api_key_name": e.ensureAPIKeyName(deref(result.ProjectID), deref(result.APIKeyID)),
					"batch":        string(result.Batch),
				}

				updateMetric(labels, "input", bucket.StartTime, bucket.EndTime, float64(result.InputTokens))
				updateMetric(labels, "output", bucket.StartTime, bucket.EndTime, float64(result.OutputTokens))
				updateMetric(labels, "input_cached", bucket.StartTime, bucket.EndTime, float64(result.InputCachedTokens))
				updateMetric(labels, "input_audio", bucket.StartTime, bucket.EndTime, float64(result.InputAudioTokens))
				updateMetric(labels, "output_audio", bucket.StartTime, bucket.EndTime, float64(result.OutputAudioTokens))

				logrus.Debugf("Processed result - Model: %s, Operation: %s, ProjectID: %s, UserID: %s, APIKeyID: %s, Batch: %s, BucketStart: %d, BucketEnd: %d, InputTokens: %d, OutputTokens: %d, InputCached: %d, InputAudio: %d, OutputAudio: %d, Requests: %d",
					deref(result.Model), endpoint.Name, deref(result.ProjectID), deref(result.UserID), deref(result.APIKeyID),
					string(result.Batch), bucket.StartTime, bucket.EndTime,
					result.InputTokens, result.OutputTokens, result.InputCachedTokens, result.InputAudioTokens, result.OutputAudioTokens, result.NumModelRequests)
			}
		}

		if !response.HasMore {
			break
		}
		nextPage = response.NextPage
	}

	logrus.Infof("Total records fetched from %s: %d", endpoint.Path, len(allResults))
	return nil
}

// ensureProjectName returns the name for already known projects and exports the name for new ones
func (e *Exporter) ensureProjectName(projectId string) string {
	if projectId == "" || projectId == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := projectNames[projectId]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	url := fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s", projectId)
	logrus.Debugf("Fetching project name: %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = resp.Body.Close() }()

	var obj Project
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil || obj.Name == "" {
		return "unknown"
	}
	logrus.Debugf("Received response: %+v", resp)

	stateMu.Lock()
	projectNames[projectId] = obj.Name
	stateMu.Unlock()
	return obj.Name
}

// ensureProjectName returns the name for already known API keys and exports the name for new ones
func (e *Exporter) ensureAPIKeyName(projectID, apiKeyID string) string {
	if apiKeyID == "" || apiKeyID == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := apiKeyNames[apiKeyID]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	var urls []string
	if projectID != "" && projectID != "unknown" {
		urls = append(urls,
			fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s/api_keys/%s", projectID, apiKeyID))
	}
	urls = append(urls,
		fmt.Sprintf("https://api.openai.com/v1/organization/api_keys/%s", apiKeyID))

	for _, u := range urls {
		logrus.Debugf("Fetching api key name: %s", u)
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		if name, ok := func() (string, bool) {
			resp, err := e.client.Do(req)
			if err != nil {
				return "", false
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				_, _ = io.Copy(io.Discard, resp.Body) // дренируем
				return "", false
			}

			var obj APIKey
			if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
				return "", false
			}
			if obj.Name == "" {
				return "", false
			}
			return obj.Name, true
		}(); ok {
			stateMu.Lock()
			apiKeyNames[apiKeyID] = name
			stateMu.Unlock()
			return name
		}
	}

	return "unknown"
}

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

// checkStatus returns an error (and drains the body) for non-2xx responses.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// fetchCostData downloads daily (bucket_width=1d) cost buckets grouped by project for [startTime, endTime).
func (e *Exporter) fetchCostData(startTime, endTime int64) ([]CostBucket, error) {
	baseURL := "https://api.openai.com/v1/organization/costs"
	nextPage := ""
	var buckets []CostBucket

	for {
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1d&group_by=project_id",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching cost data: %s", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("error fetching cost data: %w", err)
		}
		if err := checkStatus(resp); err != nil {
			return nil, fmt.Errorf("error fetching cost data: %w", err)
		}

		var out CostsList
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		closeErr := resp.Body.Close()

		if decodeErr != nil {
			return nil, fmt.Errorf("error decoding response: %w", decodeErr)
		}
		if closeErr != nil {
			logrus.WithError(closeErr).Warn("failed to close response body")
		}
		logrus.Debugf("Received response: %+v", out)
		buckets = append(buckets, out.Data...)

		if !out.HasMore {
			break
		}
		nextPage = out.NextPage
	}

	return buckets, nil
}

// updateTodayCost fetches the current UTC day's cost bucket and publishes it in todayCostUSD.
// The query window is always [today 00:00 UTC, now), independent of the usage collection window.
// On error the gauge keeps its previous values. When the UTC day changes, the gauge vec is
// reset before writing so projects that spent yesterday but not today disappear.
func (e *Exporter) updateTodayCost() error {
	now := utcNow().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	endTime := now.Unix()
	if endTime <= dayStart.Unix() {
		// Exactly at midnight: the API needs a non-empty range.
		endTime = dayStart.Unix() + 1
	}

	buckets, err := e.fetchCostData(dayStart.Unix(), endTime)
	if err != nil {
		return err
	}

	type costPoint struct {
		labels prometheus.Labels
		value  float64
	}
	today := dayStart.Format("2006-01-02")
	var points []costPoint
	for _, bucket := range buckets {
		date := time.Unix(bucket.StartTime, 0).UTC().Format("2006-01-02")
		if date != today {
			continue
		}
		for _, res := range bucket.Results {
			projectId := deref(res.ProjectID)
			lineName := "unknown"
			if res.LineItem != nil && *res.LineItem != "" {
				lineName = *res.LineItem
			}
			// Resolve names before touching the gauge so a Reset is not followed by slow lookups.
			labels := prometheus.Labels{
				"project_id":      projectId,
				"project_name":    e.ensureProjectName(projectId),
				"line_item":       lineName,
				"organization_id": res.OrganizationID,
				"currency":        res.Amount.Currency,
			}
			points = append(points, costPoint{labels: labels, value: float64(res.Amount.Value)})
			logrus.Debugf("Processed result - Date: %s, ProjectID: %s, ProjectName: %s, LineItem: %s, OrganizationID: %s, Cost: %f, Currency: %s",
				date, projectId, labels["project_name"], lineName, res.OrganizationID, res.Amount.Value, res.Amount.Currency)
		}
	}

	costMu.Lock()
	defer costMu.Unlock()
	if costDay != today {
		if costDay != "" {
			logrus.Infof("UTC day changed from %s to %s, resetting %s", costDay, today, "openai_api_today_cost")
		}
		todayCostUSD.Reset()
		costDay = today
	}
	for _, p := range points {
		todayCostUSD.With(p.labels).Set(p.value)
	}
	return nil
}

// collectCost refreshes today's cost every cost.interval until ctx is cancelled.
func (e *Exporter) collectCost(ctx context.Context) {
	for {
		if err := e.updateTodayCost(); err != nil {
			logrus.WithError(err).Warn("Error fetching cost data, keeping previous values")
		}
		if err := sleepCtx(ctx, *costInterval); err != nil {
			return
		}
	}
}

// initialWindowStart returns the start of the first usage window to fetch after startup:
// the most recent window that has already settled, so it is due immediately.
func initialWindowStart(now time.Time) int64 {
	return now.Add(-*scrapeSettleDelay).Truncate(*scrapeInterval).Add(-*scrapeInterval).Unix()
}

// catchupRequestsPerMinute is the worst-case usage request rate while catching up:
// at most one window per scrape.catchup-spacing, each window hitting every usage endpoint once.
func catchupRequestsPerMinute(spacing time.Duration) float64 {
	return float64(len(usageEndpoints)) * float64(time.Minute) / float64(spacing)
}

// validateFlags checks the collection flags for values that would break the loops or the rate limit.
func validateFlags() error {
	if *scrapeInterval < time.Second || *scrapeInterval%time.Second != 0 {
		return fmt.Errorf("-scrape.interval must be a whole number of seconds >= 1s, got %s", *scrapeInterval)
	}
	if *scrapeSettleDelay < 0 {
		return fmt.Errorf("-scrape.settle-delay must not be negative, got %s", *scrapeSettleDelay)
	}
	if *scrapeMaxCatchup < 1 {
		return fmt.Errorf("-scrape.max-catchup-windows must be >= 1, got %d", *scrapeMaxCatchup)
	}
	if *scrapeCatchupSpacing <= 0 || *scrapeCatchupSpacing > *scrapeInterval {
		// A spacing above the interval would fetch windows slower than they are produced.
		return fmt.Errorf("-scrape.catchup-spacing must be in (0, scrape.interval], got %s", *scrapeCatchupSpacing)
	}
	if *costInterval <= 0 {
		return fmt.Errorf("-cost.interval must be positive, got %s", *costInterval)
	}
	if *httpTimeout <= 0 {
		return fmt.Errorf("-http.timeout must be positive, got %s", *httpTimeout)
	}
	if rpm := catchupRequestsPerMinute(*scrapeCatchupSpacing); rpm > orgRateLimitPerMinute {
		logrus.Warnf("-scrape.catchup-spacing=%s allows %.1f usage requests/min while catching up, above the %d/min organization rate limit",
			*scrapeCatchupSpacing, rpm, orgRateLimitPerMinute)
	}
	return nil
}

// fetchUsageWindow fetches all usage endpoints concurrently for [startTime, endTime).
// Per-endpoint errors are logged and the window is not retried.
func (e *Exporter) fetchUsageWindow(startTime, endTime int64) {
	logrus.Infof("Starting collection cycle: startTime=%d, endTime=%d", startTime, endTime)

	var wg sync.WaitGroup
	for _, endpoint := range usageEndpoints {
		wg.Add(1)
		go func(ep UsageEndpoint) {
			defer wg.Done()
			if err := e.fetchUsageData(ep, startTime, endTime); err != nil {
				logrus.WithError(err).Errorf("Error fetching data from %s", ep.Path)
			}
		}(endpoint)
	}
	wg.Wait()
}

// runUsageCycle fetches every usage window [lastScrape, lastScrape+step) that has settled
// (window end <= now - scrape.settle-delay), oldest first, advancing lastScrape after each one.
// At most scrape.max-catchup-windows windows are fetched, and window starts are spaced at
// least scrape.catchup-spacing apart. Returns true when no settled window is left.
func (e *Exporter) runUsageCycle(ctx context.Context) bool {
	step := int64(*scrapeInterval / time.Second)
	for n := 0; n < *scrapeMaxCatchup; n++ {
		target := utcNow().Add(-*scrapeSettleDelay).Unix()
		if lastScrape+step > target {
			return true
		}
		if behind := (target - lastScrape) / step; behind > 1 && n == 0 {
			logrus.Warnf("Usage collection is %d windows behind, catching up", behind)
		}

		// Throttle: keep window starts spaced so catch-up stays under the rate limit.
		if !e.lastWindowAt.IsZero() {
			if wait := e.lastWindowAt.Add(*scrapeCatchupSpacing).Sub(utcNow()); wait > 0 {
				if err := sleepCtx(ctx, wait); err != nil {
					return false
				}
			}
		}
		e.lastWindowAt = utcNow()

		e.fetchUsageWindow(lastScrape, lastScrape+step)
		lastScrape += step
	}
	return lastScrape+step > utcNow().Add(-*scrapeSettleDelay).Unix()
}

// collectUsage keeps the usage window aligned to the wall clock. Instead of sleeping a fixed
// interval after each fetch (which drifts by the fetch duration every cycle), it sleeps until the
// next window becomes due, so lag never accumulates regardless of how long API calls take.
func (e *Exporter) collectUsage(ctx context.Context) {
	for ctx.Err() == nil {
		caughtUp := e.runUsageCycle(ctx)
		if ctx.Err() != nil {
			return
		}

		now := utcNow()
		var wake time.Time
		if caughtUp {
			// The next window [lastScrape, lastScrape+step) is due once it has ended and settled.
			wake = time.Unix(lastScrape, 0).Add(*scrapeInterval + *scrapeSettleDelay)
		} else {
			// Hit the per-cycle cap: pause until the next interval boundary before continuing.
			wake = now.Truncate(*scrapeInterval).Add(*scrapeInterval)
		}
		d := wake.Sub(now)
		if d < minUsageLoopSleep {
			d = minUsageLoopSleep
		}
		if err := sleepCtx(ctx, d); err != nil {
			return
		}
	}
}

// Main Function

func main() {
	flag.Parse()
	setupLogging()
	if err := validateFlags(); err != nil {
		logrus.Fatal(err)
	}

	lastScrape = initialWindowStart(utcNow())
	exporter, err := NewExporter()
	if err != nil {
		logrus.Fatal(err)
	}

	ctx := context.Background()
	go exporter.collectUsage(ctx)
	go exporter.collectCost(ctx)

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("<html><head><title>OpenAI Exporter</title></head><body><h1>OpenAI Exporter</h1><p><a href='" + *metricsPath + "'>Metrics</a></p></body></html>"))
		if err != nil {
			logrus.WithError(err).Error("Failed to write response")
		}
	})

	logrus.Infof("Starting server on %s", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		logrus.Fatal(err)
	}
}

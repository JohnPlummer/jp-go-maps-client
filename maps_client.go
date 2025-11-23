// Package mapsclient provides a Google Maps geocoding client with circuit breaker
// protection, thread-safe caching, and structured error handling.
//
// The client wraps the Google Maps Go library and integrates with jp-go-resilience
// for retry and circuit breaker patterns. Results are cached with configurable TTL
// to reduce API calls and costs.
//
// Basic usage:
//
//	client, err := mapsclient.New("your-api-key")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	result, err := client.Geocode(ctx, "1600 Amphitheatre Parkway, Mountain View, CA")
//	if err != nil {
//	    var mapsErr *mapsclient.APIError
//	    if errors.As(err, &mapsErr) {
//	        // Handle Maps API error with type-safe status code
//	        log.Printf("Maps API error: %s", mapsErr.StatusCode)
//	    }
//	    log.Fatal(err)
//	}
//
//	fmt.Printf("Location: %f, %f\n", result.Latitude, result.Longitude)
package mapsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	resilience "github.com/JohnPlummer/jp-go-resilience"
	"googlemaps.github.io/maps"
)

// Common errors for geocoding operations.
var (
	ErrEmptyLocation = errors.New("location cannot be empty")
	ErrNoResults     = errors.New("no geocoding results found")
	ErrInvalidAPIKey = errors.New("invalid Google Maps API key")
	ErrQuotaExceeded = errors.New("API quota exceeded")
)

// APIError wraps Google Maps API errors with parsed status information.
// The Google Maps Go library returns errors as plain strings in the format:
// "maps: STATUS_CODE - error_message"
// This type parses that format to enable type-safe error detection.
//
// Example usage:
//
//	result, err := client.Geocode(ctx, address)
//	if err != nil {
//	    var mapsErr *APIError
//	    if errors.As(err, &mapsErr) {
//	        switch mapsErr.StatusCode {
//	        case "OVER_QUERY_LIMIT":
//	            // Handle quota exceeded
//	        case "REQUEST_DENIED":
//	            // Handle auth error
//	        }
//	    }
//	}
type APIError struct {
	// StatusCode is the Maps API status (e.g., "OVER_QUERY_LIMIT", "REQUEST_DENIED")
	StatusCode string

	// Message is the error message from the API
	Message string

	// Err is the original error from the Maps library
	Err error
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.StatusCode != "" {
		return fmt.Sprintf("Maps API error [status: %s]: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("Maps API error: %s", e.Message)
}

// Unwrap returns the underlying error.
func (e *APIError) Unwrap() error {
	return e.Err
}

// ParseAPIError attempts to parse a Maps API error into an APIError.
// Returns nil if the error is not a Maps API error.
func ParseAPIError(err error) *APIError {
	if err == nil {
		return nil
	}

	errMsg := err.Error()

	// Check if this is a Maps API error (format: "maps: STATUS_CODE - message")
	if !strings.HasPrefix(errMsg, "maps: ") {
		return nil
	}

	// Remove "maps: " prefix
	content := strings.TrimPrefix(errMsg, "maps: ")

	// Split on " - " to separate status code and message
	parts := strings.SplitN(content, " - ", 2)

	mapsErr := &APIError{Err: err}

	if len(parts) == 2 {
		mapsErr.StatusCode = parts[0]
		mapsErr.Message = parts[1]
	} else {
		// No separator found, treat entire content as message
		mapsErr.Message = content
	}

	return mapsErr
}

// GeocodeResult represents a geocoding result with confidence scoring.
type GeocodeResult struct {
	// Address is the formatted address from Google Maps
	Address string

	// Latitude is the geographic latitude
	Latitude float64

	// Longitude is the geographic longitude
	Longitude float64

	// Confidence is a score from 0.0 to 1.0 indicating result quality
	// 1.0 = ROOFTOP accuracy, 0.75 = RANGE_INTERPOLATED, 0.5 = GEOMETRIC_CENTER, 0.25 = APPROXIMATE
	Confidence float64

	// PlaceID is the Google Maps Place ID
	PlaceID string

	// Types are the address component types (e.g., "street_address", "locality")
	Types []string
}

// GeocodeOptions configures geocoding behavior.
type GeocodeOptions struct {
	// Region biases results to a specific region (e.g., "uk", "us")
	Region string

	// Bounds restricts results to a geographic area
	Bounds *maps.LatLngBounds

	// Language specifies the language for results
	Language string
}

// Client provides geocoding services with circuit breaker protection.
type Client interface {
	// Geocode converts an address to geographic coordinates.
	Geocode(ctx context.Context, address string) (*GeocodeResult, error)

	// GeocodeWithOptions converts an address with additional options.
	GeocodeWithOptions(ctx context.Context, address string, opts *GeocodeOptions) (*GeocodeResult, error)

	// State returns the current circuit breaker state.
	State() resilience.CircuitBreakerState

	// Counts returns the current circuit breaker counts.
	Counts() resilience.CircuitBreakerCounts

	// Close stops the background cleanup goroutine.
	Close() error
}

// geocodeRequest wraps the geocoding request parameters.
type geocodeRequest struct {
	address string
	opts    *GeocodeOptions
}

// geocodeClient implements ResilientClient for geocoding operations.
type geocodeClient struct {
	client *maps.Client
}

// Execute implements the ResilientClient interface for geocoding.
func (gc *geocodeClient) Execute(ctx context.Context, req geocodeRequest) ([]maps.GeocodingResult, error) {
	geocodeReq := &maps.GeocodingRequest{
		Address: req.address,
	}

	if req.opts != nil {
		if req.opts.Region != "" {
			geocodeReq.Region = req.opts.Region
		}
		if req.opts.Bounds != nil {
			geocodeReq.Bounds = req.opts.Bounds
		}
		if req.opts.Language != "" {
			geocodeReq.Language = req.opts.Language
		}
	}

	return gc.client.Geocode(ctx, geocodeReq)
}

// mapsClient implements Client with caching and circuit breaker.
type mapsClient struct {
	client          *maps.Client
	cb              *resilience.CircuitBreakerWrapper[geocodeRequest, []maps.GeocodingResult]
	cache           *geocodeCache
	timeout         time.Duration
	cleanupInterval time.Duration
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *slog.Logger
}

// geocodeCache provides thread-safe caching with TTL.
type geocodeCache struct {
	entries map[string]*cacheEntry
	ttl     time.Duration
	mu      sync.RWMutex
}

type cacheEntry struct {
	result    *GeocodeResult
	expiresAt time.Time
}

// Option configures a Client.
type Option func(*mapsClient)

// WithTimeout sets the API call timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(mc *mapsClient) {
		mc.timeout = timeout
	}
}

// WithCacheTTL sets the cache time-to-live.
func WithCacheTTL(ttl time.Duration) Option {
	return func(mc *mapsClient) {
		mc.cache.ttl = ttl
	}
}

// WithCleanupInterval sets the interval for periodic cache cleanup.
func WithCleanupInterval(interval time.Duration) Option {
	return func(mc *mapsClient) {
		mc.cleanupInterval = interval
	}
}

// WithCircuitBreakerThreshold sets the failure threshold for the circuit breaker.
func WithCircuitBreakerThreshold(threshold uint32) Option {
	return func(mc *mapsClient) {
		// Recreate circuit breaker with new threshold
		baseClient := &geocodeClient{client: mc.client}
		mc.cb = resilience.NewCircuitBreakerWrapper(
			baseClient,
			resilience.WithMaxRequests(10),
			resilience.WithInterval(60*time.Second),
			resilience.WithTimeout(60*time.Second),
			resilience.WithReadyToTrip(func(counts resilience.CircuitBreakerCounts) bool {
				return counts.ConsecutiveFailures >= threshold
			}),
			resilience.WithCircuitBreakerErrorClassifier(&errorClassifier{logger: mc.logger}),
			resilience.WithCircuitBreakerLogger(mc.logger),
		)
	}
}

// WithLogger sets a custom logger for the Maps client.
func WithLogger(logger *slog.Logger) Option {
	return func(mc *mapsClient) {
		mc.logger = logger
	}
}

// New creates a new Client with the specified API key.
func New(apiKey string, opts ...Option) (Client, error) {
	if apiKey == "" {
		return nil, ErrInvalidAPIKey
	}

	client, err := maps.NewClient(maps.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create Google Maps client: %w", err)
	}

	// Create context for lifecycle management
	ctx, cancel := context.WithCancel(context.Background())

	// Default configuration
	mc := &mapsClient{
		client:          client,
		timeout:         10 * time.Second,
		cleanupInterval: 1 * time.Hour,
		ctx:             ctx,
		cancel:          cancel,
		logger:          slog.Default(),
		cache: &geocodeCache{
			entries: make(map[string]*cacheEntry),
			ttl:     24 * time.Hour,
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(mc)
	}

	// Create circuit breaker if not already created by options
	if mc.cb == nil {
		baseClient := &geocodeClient{client: client}
		mc.cb = resilience.NewCircuitBreakerWrapper(
			baseClient,
			resilience.WithMaxRequests(10),
			resilience.WithInterval(60*time.Second),
			resilience.WithTimeout(60*time.Second),
			resilience.WithReadyToTrip(func(counts resilience.CircuitBreakerCounts) bool {
				return counts.ConsecutiveFailures >= 5
			}),
			resilience.WithCircuitBreakerErrorClassifier(&errorClassifier{logger: mc.logger}),
			resilience.WithCircuitBreakerLogger(mc.logger),
		)
	}

	// Start background cleanup goroutine
	go mc.startCleanup()

	return mc, nil
}

// Geocode converts an address to geographic coordinates.
func (mc *mapsClient) Geocode(ctx context.Context, address string) (*GeocodeResult, error) {
	return mc.GeocodeWithOptions(ctx, address, nil)
}

// GeocodeWithOptions converts an address with additional options.
func (mc *mapsClient) GeocodeWithOptions(ctx context.Context, address string, opts *GeocodeOptions) (*GeocodeResult, error) {
	// Validate input
	if strings.TrimSpace(address) == "" {
		return nil, ErrEmptyLocation
	}

	// Normalize address for caching
	cacheKey := mc.buildCacheKey(address, opts)

	// Check cache first
	if result := mc.cache.get(cacheKey); result != nil {
		return result, nil
	}

	// Create request context with timeout
	reqCtx, cancel := context.WithTimeout(ctx, mc.timeout)
	defer cancel()

	// Execute through circuit breaker
	req := geocodeRequest{
		address: address,
		opts:    opts,
	}
	results, err := mc.cb.Execute(reqCtx, req)
	if err != nil {
		// Try parsing as Maps API error for type-safe error detection
		if mapsErr := ParseAPIError(err); mapsErr != nil {
			isContextDeadline := errors.Is(err, context.DeadlineExceeded)

			// Log structured diagnostic information
			logFields := []any{
				"address", address,
				"status_code", mapsErr.StatusCode,
				"message", mapsErr.Message,
			}
			if isContextDeadline {
				logFields = append(logFields, "context_deadline_exceeded", true)
			}
			mc.logger.Error("Maps API error details", logFields...)

			// Return APIError directly for type-safe error checking with errors.As()
			return nil, mapsErr
		}

		// Handle context errors
		if errors.Is(err, context.DeadlineExceeded) {
			mc.logger.Error("Geocoding request timeout",
				"error", err,
				"address", address)
			return nil, fmt.Errorf("geocoding request timeout: %w", err)
		}

		// Generic network or other error
		return nil, fmt.Errorf("geocoding failed: %w", err)
	}

	// Check if we got results
	if len(results) == 0 {
		return nil, ErrNoResults
	}

	// Convert first result (best match) to our format
	result := convertToGeocodeResult(&results[0])

	// Cache the result
	mc.cache.set(cacheKey, result)

	return result, nil
}

// State returns the current circuit breaker state.
func (mc *mapsClient) State() resilience.CircuitBreakerState {
	return mc.cb.State()
}

// Counts returns the current circuit breaker counts.
func (mc *mapsClient) Counts() resilience.CircuitBreakerCounts {
	return mc.cb.Counts()
}

// buildCacheKey creates a normalized cache key.
func (mc *mapsClient) buildCacheKey(address string, opts *GeocodeOptions) string {
	key := strings.ToLower(strings.TrimSpace(address))
	if opts != nil {
		if opts.Region != "" {
			key += "|" + opts.Region
		}
		if opts.Language != "" {
			key += "|" + opts.Language
		}
	}
	return key
}

// convertToGeocodeResult converts Google Maps result to our format.
func convertToGeocodeResult(result *maps.GeocodingResult) *GeocodeResult {
	confidence := calculateConfidence(result.Geometry.LocationType)

	return &GeocodeResult{
		Address:    result.FormattedAddress,
		Latitude:   result.Geometry.Location.Lat,
		Longitude:  result.Geometry.Location.Lng,
		Confidence: confidence,
		PlaceID:    result.PlaceID,
		Types:      result.Types,
	}
}

// calculateConfidence converts location type to confidence score.
func calculateConfidence(locationType string) float64 {
	switch locationType {
	case "ROOFTOP":
		return 1.0
	case "RANGE_INTERPOLATED":
		return 0.75
	case "GEOMETRIC_CENTER":
		return 0.5
	case "APPROXIMATE":
		return 0.25
	default:
		return 0.5
	}
}

// errorClassifier implements CircuitBreakerErrorClassifier for Maps API errors.
type errorClassifier struct {
	logger *slog.Logger
}

// ShouldTripCircuit determines if an error should cause the circuit to trip.
// Uses type-safe error detection by parsing Maps API error format.
func (m *errorClassifier) ShouldTripCircuit(err error) bool {
	if err == nil {
		return false
	}

	// Check for context errors - don't trip on timeouts
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	// Try to parse as Maps API error for type-safe detection
	if mapsErr := ParseAPIError(err); mapsErr != nil {
		switch mapsErr.StatusCode {
		case "OVER_QUERY_LIMIT":
			// Quota exceeded - trip circuit to avoid hammering API
			m.logger.Warn("Circuit breaker tripping due to quota exceeded",
				"status_code", mapsErr.StatusCode,
				"message", mapsErr.Message)
			return true

		case "REQUEST_DENIED", "INVALID_REQUEST":
			// Authentication/authorization errors - trip circuit
			m.logger.Warn("Circuit breaker tripping due to authentication error",
				"status_code", mapsErr.StatusCode,
				"message", mapsErr.Message)
			return true

		case "ZERO_RESULTS":
			// Not a failure - valid API response indicating no results
			return false

		default:
			// Unknown Maps API error - trip circuit to be safe
			m.logger.Warn("Circuit breaker tripping due to unknown Maps API error",
				"status_code", mapsErr.StatusCode,
				"message", mapsErr.Message)
			return true
		}
	}

	// Non-Maps API error (network error, etc.) - trip circuit
	m.logger.Warn("Circuit breaker tripping due to non-Maps API error",
		"error", err.Error())
	return true
}

// get retrieves a cached result if it exists and hasn't expired.
// Automatically removes expired entries to prevent memory leak.
func (gc *geocodeCache) get(key string) *GeocodeResult {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	entry, exists := gc.entries[key]
	if !exists {
		return nil
	}

	// Check if expired and remove if so
	if time.Now().After(entry.expiresAt) {
		delete(gc.entries, key)
		return nil
	}

	return entry.result
}

// set stores a result in the cache with TTL.
func (gc *geocodeCache) set(key string, result *GeocodeResult) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	gc.entries[key] = &cacheEntry{
		result:    result,
		expiresAt: time.Now().Add(gc.ttl),
	}
}

// cleanup removes all expired entries from the cache.
// This prevents memory leaks from entries that are never accessed again after expiring.
func (gc *geocodeCache) cleanup() int {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	now := time.Now()
	removed := 0

	for key, entry := range gc.entries {
		if now.After(entry.expiresAt) {
			delete(gc.entries, key)
			removed++
		}
	}

	return removed
}

// startCleanup runs the periodic cache cleanup in a background goroutine.
// Stops when the client's context is cancelled.
func (mc *mapsClient) startCleanup() {
	ticker := time.NewTicker(mc.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mc.ctx.Done():
			return
		case <-ticker.C:
			mc.cache.cleanup()
		}
	}
}

// Close stops the background cleanup goroutine and releases resources.
// Should be called when the client is no longer needed.
func (mc *mapsClient) Close() error {
	mc.cancel()
	return nil
}

package upgradediags

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/blang/semver/v4"
	"github.com/google/uuid"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-version-operator/pkg/cincinnati"
	"github.com/openshift/cluster-version-operator/pkg/clusterconditions"
	"github.com/openshift/managed-upgrade-operator/config"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
)

const (
	defaultUpstreamServer = "https://api.openshift.com/api/upgrades_info/v1/graph"
	cacheTTL              = 1 * time.Hour
)

// AvailableVersionsResponse is returned by GET /api/v1/upgrade-diags/available-versions
type AvailableVersionsResponse struct {
	Channel            string               `json:"channel"`
	CurrentVersion     string               `json:"currentVersion"`
	AvailableUpdates   []VersionInfo        `json:"availableUpdates"`
	ConditionalUpdates []ConditionalVersion `json:"conditionalUpdates"`
	CachedAt           time.Time            `json:"cachedAt"`
	CacheExpiresAt     time.Time            `json:"cacheExpiresAt"`
}

// VersionInfo represents an available version
type VersionInfo struct {
	Version string `json:"version"`
}

// ConditionalVersion represents a conditional update with risks
type ConditionalVersion struct {
	Version string   `json:"version"`
	Risks   []string `json:"risks,omitempty"`
}

// versionsCacheEntry stores Cincinnati query results with expiry
type versionsCacheEntry struct {
	channel            string
	currentVersion     string
	availableUpdates   []VersionInfo
	conditionalUpdates []ConditionalVersion
	cachedAt           time.Time
	expiresAt          time.Time
}

// versionsCache provides thread-safe caching of Cincinnati queries
type versionsCache struct {
	mu      sync.RWMutex
	entries map[string]*versionsCacheEntry // keyed by "channel:currentVersion"
}

var (
	globalVersionsCache = &versionsCache{
		entries: make(map[string]*versionsCacheEntry),
	}
)

// get retrieves a cached entry if valid
func (c *versionsCache) get(channel, currentVersion string) (*versionsCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := fmt.Sprintf("%s:%s", channel, currentVersion)
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		return nil, false
	}

	return entry, true
}

// set stores a cache entry
func (c *versionsCache) set(entry *versionsCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := fmt.Sprintf("%s:%s", entry.channel, entry.currentVersion)
	c.entries[key] = entry
}

// handleAvailableVersions returns available upgrade versions for a given channel.
// It queries Cincinnati and caches results for 1 hour.
// Query params:
//   - channel (required): target upgrade channel (e.g., "stable-4.22", "fast-4.23")
//   - refresh (optional): if "true", bypass cache and force fresh query
func (s *UpgradeDiagsHTTPServer) handleAvailableVersions(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()

	// Authorization: requires 'list' permission on upgradediagschecks
	// (same as catalog endpoint - this is cluster metadata, not creating resources)
	if err := s.authorizeToken(ctx, token, "list"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	channel := r.URL.Query().Get("channel")
	if channel == "" {
		http.Error(w, "Bad request: 'channel' query parameter required (e.g., ?channel=stable-4.22)", http.StatusBadRequest)
		return
	}

	refresh := r.URL.Query().Get("refresh") == "true"

	// Get current ClusterVersion
	clusterVersion, err := cv.NewCVClient(s.Client).GetClusterVersion()
	if err != nil {
		s.Logger.Error(err, "failed to get ClusterVersion for available versions query")
		http.Error(w, "Failed to retrieve cluster version: "+err.Error(), http.StatusInternalServerError)
		return
	}

	currentVersion := clusterVersion.Status.Desired.Version

	// Check cache unless refresh=true
	if !refresh {
		if entry, ok := globalVersionsCache.get(channel, currentVersion); ok {
			s.Logger.V(1).Info("serving available versions from cache",
				"channel", channel, "currentVersion", currentVersion,
				"cachedAt", entry.cachedAt, "expiresAt", entry.expiresAt)
			writeJSON(w, http.StatusOK, AvailableVersionsResponse{
				Channel:            entry.channel,
				CurrentVersion:     entry.currentVersion,
				AvailableUpdates:   entry.availableUpdates,
				ConditionalUpdates: entry.conditionalUpdates,
				CachedAt:           entry.cachedAt,
				CacheExpiresAt:     entry.expiresAt,
			})
			return
		}
	}

	// Query Cincinnati
	s.Logger.Info("querying Cincinnati for available versions",
		"channel", channel, "currentVersion", currentVersion, "refresh", refresh)

	available, conditional, err := s.queryCincinnati(ctx, clusterVersion, channel)
	if err != nil {
		s.Logger.Error(err, "Cincinnati query failed", "channel", channel)
		http.Error(w, "Failed to query available versions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Convert to response format
	now := time.Now()
	entry := &versionsCacheEntry{
		channel:            channel,
		currentVersion:     currentVersion,
		availableUpdates:   make([]VersionInfo, len(available)),
		conditionalUpdates: make([]ConditionalVersion, len(conditional)),
		cachedAt:           now,
		expiresAt:          now.Add(cacheTTL),
	}

	for i, v := range available {
		entry.availableUpdates[i] = VersionInfo{Version: v.Version}
	}

	for i, c := range conditional {
		risks := make([]string, len(c.Risks))
		for j, r := range c.Risks {
			risks[j] = r.Name
		}
		entry.conditionalUpdates[i] = ConditionalVersion{
			Version: c.Release.Version,
			Risks:   risks,
		}
	}

	// Cache the result
	globalVersionsCache.set(entry)

	writeJSON(w, http.StatusOK, AvailableVersionsResponse{
		Channel:            entry.channel,
		CurrentVersion:     entry.currentVersion,
		AvailableUpdates:   entry.availableUpdates,
		ConditionalUpdates: entry.conditionalUpdates,
		CachedAt:           entry.cachedAt,
		CacheExpiresAt:     entry.expiresAt,
	})
}

// queryCincinnati queries the Cincinnati update graph for available versions.
// Returns available updates and conditional updates for the given channel.
func (s *UpgradeDiagsHTTPServer) queryCincinnati(ctx context.Context, clusterVersion *configv1.ClusterVersion, channel string) ([]configv1.Release, []configv1.ConditionalUpdate, error) {
	// Get cluster ID from ClusterVersion
	clusterIDStr := string(clusterVersion.Spec.ClusterID)
	if clusterIDStr == "" {
		return nil, nil, fmt.Errorf("ClusterVersion.spec.clusterID is empty")
	}

	clusterID, err := uuid.Parse(clusterIDStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse cluster ID %s: %w", clusterIDStr, err)
	}

	// Parse current version
	currentVersion := clusterVersion.Status.Desired.Version
	parsedVersion, err := semver.Parse(currentVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse current version %s: %w", currentVersion, err)
	}

	// Use default Cincinnati upstream server
	upstreamURI, err := url.Parse(defaultUpstreamServer)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse upstream URI: %w", err)
	}

	// Create HTTP transport (with potential proxy support from environment)
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, nil, fmt.Errorf("failed to cast http.DefaultTransport to *http.Transport")
	}
	transportClone := transport.Clone()

	// Query Cincinnati
	_, available, conditional, err := cincinnati.NewClient(
		clusterID,
		transportClone,
		config.SetUserAgent(),
		clusterconditions.NewConditionRegistry(),
	).GetUpdates(ctx, upstreamURI, runtime.GOARCH, runtime.GOARCH, channel, parsedVersion)

	if err != nil {
		return nil, nil, fmt.Errorf("cincinnati GetUpdates failed: %w", err)
	}

	return available, conditional, nil
}

package net

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/opentracing/opentracing-go"
	"github.com/redis/go-redis/v9"
	"github.com/zalando/skipper/logging"
	"github.com/zalando/skipper/metrics"

	xxhash "github.com/cespare/xxhash/v2"
	rendezvous "github.com/dgryski/go-rendezvous"

	jump "github.com/dgryski/go-jump"

	"github.com/dchest/siphash"
	mpchash "github.com/dgryski/go-mpchash"
)

// RedisOptions is used to configure the redis client (Ring or Cluster)
type RedisOptions struct {
	// Addrs are the list of redis shards (for Ring) or seed nodes (for Cluster).
	Addrs []string

	// AddrUpdater is called periodically to update Redis addresses in Ring mode.
	AddrUpdater func() ([]string, error)

	// RemoteURL specifies a URL to fetch Redis addresses from for Ring mode.
	RemoteURL string

	// UpdateInterval for AddrUpdater in Ring mode.
	UpdateInterval time.Duration

	// ClusterMode enables Redis Cluster mode instead of Ring mode.
	ClusterMode bool

	// TLSConfig for Redis connection.
	TLSConfig *tls.Config

	// Password for Redis authentication.
	Password string

	// ReadTimeout for redis socket reads
	ReadTimeout time.Duration
	// WriteTimeout for redis socket writes
	WriteTimeout time.Duration
	// DialTimeout is the max time.Duration to dial a new connection (also used for RemoteURL fetch)
	DialTimeout time.Duration

	// PoolTimeout is the max time.Duration to get a connection from pool
	PoolTimeout time.Duration
	// IdleTimeout is the maximum amount of time a connection may be idle.
	IdleTimeout time.Duration
	// IdleCheckFrequency - frequency for removing idle connections. If <= 0, IdleTimeout is ignored.
	IdleCheckFrequency time.Duration
	// MaxConnAge is the maximum age of a connection.
	MaxConnAge time.Duration
	// MinIdleConns is the minimum number of socket connections to redis
	MinIdleConns int
	// MaxIdleConns is the maximum number of socket connections to redis.
	MaxIdleConns int

	// HeartbeatFrequency for PING commands in Ring mode.
	HeartbeatFrequency time.Duration

	// ConnMetricsInterval for updating connection metrics.
	ConnMetricsInterval time.Duration
	// MetricsPrefix for redis client metrics.
	MetricsPrefix string
	// Tracer for OpenTracing support.
	Tracer opentracing.Tracer
	// Log is the logger.
	Log logging.Logger

	// HashAlgorithm for Ring mode (rendezvous, rendezvousVnodes, jump, mpchash).
	HashAlgorithm string

	// ClusterAddressMapper maps cluster node addresses (e.g., Docker internal IPs to localhost ports).
	// Used in Cluster mode for testing with Docker containers.
	ClusterAddressMapper func(addr string) string
}

// RedisClient is a redis client that supports both Ring and Cluster modes.
// computing a ring hash. It logs to the logging.Logger interface,
// that you can pass. It adds metrics and operations are traced with
// opentracing. You can set timeouts and the defaults are set to be ok
// to be in the hot path of low latency production requests.
type RedisClient struct {
	mu            sync.RWMutex
	wg            sync.WaitGroup
	client        interface{}
	clusterMode   bool
	log           logging.Logger
	metrics       metrics.Metrics
	metricsPrefix string
	options       *RedisOptions
	tracer        opentracing.Tracer
	quit          chan struct{}
	once          sync.Once
	closed        bool
	cancel        context.CancelFunc
}

// RedisScript wraps redis.Script.
type RedisScript struct {
	script *redis.Script
}

// Define defaults within this package
const (
	DefaultRedisReadTimeout         = 25 * time.Millisecond
	DefaultRedisWriteTimeout        = 25 * time.Millisecond
	DefaultRedisPoolTimeout         = 25 * time.Millisecond
	DefaultRedisDialTimeout         = 25 * time.Millisecond
	DefaultRedisMinIdleConns        = 100
	DefaultRedisMaxIdleConns        = 100
	DefaultRedisUpdateInterval      = 10 * time.Second
	DefaultRedisConnMetricsInterval = 60 * time.Second
	DefaultRedisMetricsPrefix       = "swarm.redis."
)

// --- Hashing implementations (used only for Ring mode) ---
// https://arxiv.org/pdf/1406.2294.pdf
type jumpHash struct {
	shards []string
}

func NewJumpHash(shards []string) redis.ConsistentHash {
	return &jumpHash{shards: shards}
}

func (j *jumpHash) Get(k string) string {
	key := xxhash.Sum64String(k)
	h := jump.Hash(key, len(j.shards))
	if len(j.shards) == 0 {
		return ""
	}
	return j.shards[int(h)]
}

// Multi-probe consistent hashing - mpchash
// https://arxiv.org/pdf/1505.00062.pdf
type multiprobe struct {
	hash *mpchash.Multi
}

func NewMultiprobe(shards []string) redis.ConsistentHash {
	return &multiprobe{
		// 2 seeds and k=21 got from library
		hash: mpchash.New(shards, siphash64seed, [2]uint64{1, 2}, 21),
	}
}
func (mc *multiprobe) Get(k string) string {
	return mc.hash.Hash(k)
}

func siphash64seed(b []byte, s uint64) uint64 {
	return siphash.Hash(s, 0, b)
}

// rendezvous copied from github.com/go-redis/redis/v8@v8.3.3/ring.go
type rendezvousWrapper struct{ *rendezvous.Rendezvous }

func (w rendezvousWrapper) Get(key string) string { return w.Lookup(key) }
func NewRendezvous(shards []string) redis.ConsistentHash {
	return rendezvousWrapper{rendezvous.New(shards, xxhash.Sum64String)}
}

// rendezvous vnodes
type rendezvousVnodes struct {
	*rendezvous.Rendezvous
	table map[string]string
}

const vnodePerShard = 100

func (w rendezvousVnodes) Get(key string) string {
	k := w.Lookup(key)
	v, ok := w.table[k]
	if !ok {
		log.Printf("rendezvousVnodes: vnode key '%s' not found in table for input key '%s'. Returning vnode key.", k, key)
		return k
	}
	return v
}

func NewRendezvousVnodes(shards []string) redis.ConsistentHash {
	vshards := make([]string, vnodePerShard*len(shards))
	table := make(map[string]string, vnodePerShard*len(shards))
	for i := 0; i < vnodePerShard; i++ {
		for j, shard := range shards {
			vshard := fmt.Sprintf("%s%d", shard, i) // suffix
			table[vshard] = shard
			vshards[i*len(shards)+j] = vshard
		}
	}
	return rendezvousVnodes{rendezvous.New(vshards, xxhash.Sum64String), table}
}

func createRemoteUpdater(url string, timeout time.Duration, logger logging.Logger) func() ([]string, error) {
	if url == "" {
		return nil
	}
	logger.Infof("Creating remote address updater for Redis Ring from URL: %s (timeout: %v)", url, timeout)
	client := &http.Client{Timeout: timeout}

	return func() ([]string, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			logger.Errorf("Failed to create request for remote redis endpoints %s: %v", url, err)
			return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
		}

		req.Header.Set("User-Agent", "skipper-redis-updater/1.0")

		resp, err := client.Do(req)
		if err != nil {
			logger.Errorf("Failed to fetch remote redis endpoints from %s: %v", url, err)
			return nil, fmt.Errorf("failed to fetch %s: %w", url, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			logger.Errorf("Failed to fetch remote redis endpoints from %s: status %d", url, resp.StatusCode)
			// Read body for error message from server
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) // Limit read size
			logger.Errorf("Response body (limited): %s", string(bodyBytes))
			return nil, fmt.Errorf("failed to fetch %s: status %d", url, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logger.Errorf("Failed to read response body from %s: %v", url, err)
			return nil, fmt.Errorf("failed to read response body from %s: %w", url, err)
		}

		rawAddrs := strings.Split(string(body), ",")
		cleanedAddrs := make([]string, 0, len(rawAddrs))
		for _, addr := range rawAddrs {
			trimmed := strings.TrimSpace(addr)
			if trimmed != "" {
				host, _, err := net.SplitHostPort(trimmed)
				if err != nil {
					logger.Warnf("Ignoring invalid address format from remote URL %s: '%s' (%v)", url, trimmed, err)
					continue
				}

				if host == "" {
					logger.Warnf("Ignoring invalid address format from remote URL %s: '%s' (host part is empty)", url, trimmed)
					continue
				}

				cleanedAddrs = append(cleanedAddrs, trimmed)
			}
		}

		if len(cleanedAddrs) == 0 {
			logger.Errorf("No valid addresses found in response from %s. Body: %s", url, string(body))
			return nil, fmt.Errorf("no valid addresses found in response from %s", url)
		}
		logger.Debugf("Successfully fetched %d addresses from %s", len(cleanedAddrs), url)
		return cleanedAddrs, nil
	}
}

// NewRedisClient creates a new RedisClient which can operate in Ring or Cluster mode.
func NewRedisClient(ro *RedisOptions) *RedisClient {
	const backOffTime = 2 * time.Second
	const retryCount = 5

	if ro == nil {
		ro = &RedisOptions{}
	}
	if ro.Log == nil {
		ro.Log = &logging.DefaultLog{}
	}
	if ro.Tracer == nil {
		ro.Tracer = &opentracing.NoopTracer{}
	}
	if ro.ConnMetricsInterval <= 0 {
		ro.ConnMetricsInterval = DefaultRedisConnMetricsInterval
	}
	if ro.MetricsPrefix == "" {
		ro.MetricsPrefix = DefaultRedisMetricsPrefix
	}
	if ro.ReadTimeout == 0 {
		ro.ReadTimeout = DefaultRedisReadTimeout
	}
	if ro.WriteTimeout == 0 {
		ro.WriteTimeout = DefaultRedisWriteTimeout
	}
	if ro.PoolTimeout == 0 {
		ro.PoolTimeout = DefaultRedisPoolTimeout
	}
	if ro.DialTimeout == 0 {
		ro.DialTimeout = DefaultRedisDialTimeout
	}
	if ro.MinIdleConns == 0 {
		ro.MinIdleConns = DefaultRedisMinIdleConns
	}
	if ro.MaxIdleConns == 0 {
		ro.MaxIdleConns = DefaultRedisMaxIdleConns
	}

	r := &RedisClient{
		once:          sync.Once{},
		quit:          make(chan struct{}),
		metrics:       metrics.Default,
		tracer:        ro.Tracer,
		log:           ro.Log,
		options:       ro,
		metricsPrefix: ro.MetricsPrefix,
		clusterMode:   ro.ClusterMode,
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	if ro.ClusterMode {
		r.log.Infof("Creating Redis client in Cluster mode")
		if ro.AddrUpdater != nil || ro.RemoteURL != "" {
			r.log.Warnf("AddrUpdater/RemoteURL provided but ignored in Cluster mode (cluster handles node discovery)")
		}
		if len(ro.Addrs) == 0 {
			r.log.Errorf("No seed addresses provided for Redis Cluster mode.")
			r.closed = true
			r.cancel()
			return r
		}

		clusterOptions := &redis.ClusterOptions{
			Addrs:           ro.Addrs,
			Password:        ro.Password,
			ReadTimeout:     ro.ReadTimeout,
			WriteTimeout:    ro.WriteTimeout,
			PoolTimeout:     ro.PoolTimeout,
			DialTimeout:     ro.DialTimeout,
			MinIdleConns:    ro.MinIdleConns,
			PoolSize:        ro.MaxIdleConns,
			MaxIdleConns:    ro.MaxIdleConns,
			ConnMaxLifetime: ro.MaxConnAge,
			ConnMaxIdleTime: ro.IdleTimeout,
			TLSConfig:       ro.TLSConfig,
		}

		// Add address mapper if provided (for Docker testing)
		if ro.ClusterAddressMapper != nil {
			clusterOptions.NewClient = func(opt *redis.Options) *redis.Client {
				// Remap the address using the provided mapper
				opt.Addr = ro.ClusterAddressMapper(opt.Addr)
				return redis.NewClient(opt)
			}
		}

		r.client = redis.NewClusterClient(clusterOptions)
		r.log.Infof("Created Redis Cluster client with seed addresses: %v", ro.Addrs)

	} else {
		r.log.Infof("Creating Redis client in Ring mode")

		if ro.AddrUpdater == nil && ro.RemoteURL != "" {
			r.log.Infof("No AddrUpdater provided, creating from RemoteURL: %s", ro.RemoteURL)
			ro.AddrUpdater = createRemoteUpdater(ro.RemoteURL, ro.DialTimeout, r.log)
			if ro.UpdateInterval == 0 {
				ro.UpdateInterval = DefaultRedisUpdateInterval
			}
		}

		var initialAddrs []string
		if ro.AddrUpdater != nil {
			r.log.Info("Fetching initial addresses using AddrUpdater...")
			var err error
			for i := 0; i < retryCount; i++ {
				initialAddrs, err = ro.AddrUpdater()
				if err == nil {
					break
				}
				r.log.Warnf("Failed to get initial addresses from AddrUpdater (attempt %d/%d), retrying in %v: %v", i+1, retryCount, backOffTime, err)
				time.Sleep(backOffTime)
			}
			if err != nil {
				r.log.Warnf("Failed to get initial addresses from AddrUpdater after %d retries: %v. Falling back to statically configured addresses.", retryCount, err)
				initialAddrs = ro.Addrs
			} else {
				r.log.Infof("Successfully fetched initial addresses: %v", initialAddrs)
			}
		} else if len(ro.Addrs) > 0 {
			r.log.Info("Using provided static addresses.")
			initialAddrs = ro.Addrs
		} else {
			r.log.Errorf("No AddrUpdater, RemoteURL, or static Addrs provided for Redis Ring mode.")
			r.closed = true
			r.cancel()
			return r
		}
		r.options.Addrs = initialAddrs

		ringOptions := &redis.RingOptions{
			Addrs:              createAddressMap(initialAddrs),
			Password:           ro.Password,
			HeartbeatFrequency: ro.HeartbeatFrequency,
			ReadTimeout:        ro.ReadTimeout,
			WriteTimeout:       ro.WriteTimeout,
			PoolTimeout:        ro.PoolTimeout,
			DialTimeout:        ro.DialTimeout,
			MinIdleConns:       ro.MinIdleConns,
			PoolSize:           ro.MaxIdleConns,
			MaxIdleConns:       ro.MaxIdleConns,
			ConnMaxLifetime:    ro.MaxConnAge,
			ConnMaxIdleTime:    ro.IdleTimeout,
			TLSConfig:          ro.TLSConfig,
		}

		switch ro.HashAlgorithm {
		case "rendezvous":
			ringOptions.NewConsistentHash = NewRendezvous
		case "rendezvousVnodes":
			ringOptions.NewConsistentHash = NewRendezvousVnodes
		case "jump":
			ringOptions.NewConsistentHash = NewJumpHash
		case "mpchash":
			ringOptions.NewConsistentHash = NewMultiprobe
		default:
			if ro.HashAlgorithm != "" {
				r.log.Warnf("Unknown HashAlgorithm '%s', using default (rendezvous).", ro.HashAlgorithm)
			}
			ringOptions.NewConsistentHash = NewRendezvous
		}

		r.client = redis.NewRing(ringOptions)
		r.log.Infof("Created initial Ring with addresses: %v", initialAddrs)

		if ro.AddrUpdater != nil {
			if ro.UpdateInterval <= 0 {
				ro.UpdateInterval = DefaultRedisUpdateInterval
				r.log.Warnf("UpdateInterval was zero, defaulting to %v", ro.UpdateInterval)
			}
			r.wg.Add(1)
			go r.startUpdater(ctx)
		} else {
			r.log.Info("No AddrUpdater configured, Ring addresses will remain static.")
		}
	}

	if r.client == nil && !r.closed {
		r.log.Error("Redis client initialization failed unexpectedly.")
		r.closed = true
		r.cancel()
		return r
	}

	r.StartMetricsCollection(ctx)

	return r
}

func createAddressMap(addrs []string) map[string]string {
	res := make(map[string]string, len(addrs))
	for _, addr := range addrs {
		res[addr] = addr
	}
	return res
}

func hasAll(a []string, set map[string]struct{}) bool {
	if len(a) != len(set) {
		return false
	}
	tempSet := make(map[string]struct{}, len(set))
	for k, v := range set {
		tempSet[k] = v
	}
	for _, w := range a {
		if _, ok := tempSet[w]; !ok {
			return false
		}
		delete(tempSet, w)
	}
	return len(tempSet) == 0
}

func (r *RedisClient) startUpdater(ctx context.Context) {
	defer r.wg.Done()

	if r.clusterMode || r.options.AddrUpdater == nil {
		r.log.Warn("startUpdater called unexpectedly.")
		return
	}

	r.log.Infof("Starting goroutine to update Redis Ring instances every %s", r.options.UpdateInterval)
	defer r.log.Info("Stopped goroutine to update Redis Ring")

	r.mu.RLock()
	initialOptionsAddrs := make([]string, len(r.options.Addrs))
	copy(initialOptionsAddrs, r.options.Addrs)
	r.mu.RUnlock()

	currentAddrsSet := make(map[string]struct{})
	for _, addr := range initialOptionsAddrs {
		currentAddrsSet[addr] = struct{}{}
	}

	ticker := time.NewTicker(r.options.UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.quit:
			r.log.Info("Redis Ring updater received quit signal.")
			return
		case <-ctx.Done():
			r.log.Infof("Redis Ring updater stopping due to context cancellation: %v", ctx.Err())
			return
		case <-ticker.C:
			type updateResult struct {
				addrs []string
				err   error
			}
			updateChan := make(chan updateResult, 1)
			go func() {
				addrs, err := r.options.AddrUpdater()
				updateChan <- updateResult{addrs: addrs, err: err}
			}()

			var newAddrsSlice []string
			var err error

			select {
			case <-r.quit:
				r.log.Info("Redis Ring updater received quit signal while updating.")
				return
			case <-ctx.Done():
				r.log.Infof("Redis Ring updater stopping due to context cancellation while updating: %v", ctx.Err())
				return
			case res := <-updateChan:
				newAddrsSlice, err = res.addrs, res.err
			}

			if err != nil {
				r.log.Errorf("Failed to get updated addresses from AddrUpdater: %v", err)
				continue
			}

			if len(newAddrsSlice) == 0 {
				r.log.Warn("Updater returned empty address list, not updating. Keeping previous addresses.")
				continue
			}

			r.mu.RLock()
			currentAddresses := make([]string, len(r.options.Addrs))
			copy(currentAddresses, r.options.Addrs)
			r.mu.RUnlock()

			needsUpdate := !hasAll(newAddrsSlice, currentAddrsSet)

			if needsUpdate {
				r.log.Infof("Redis Ring updater detected address change. Old count: %d, New count: %d. Updating...", len(currentAddresses), len(newAddrsSlice))

				r.SetAddrs(ctx, newAddrsSlice)

				newSet := make(map[string]struct{}, len(newAddrsSlice))
				for _, addr := range newAddrsSlice {
					newSet[addr] = struct{}{}
				}
				currentAddrsSet = newSet

				r.log.Infof("Redis Ring addresses updated to: %v", newAddrsSlice)

			} else {
				r.log.Debugf("Redis Ring addresses unchanged (%d).", len(currentAddrsSet))
			}
		}
	}
}

// IsAvailable checks if the Redis client (Ring or Cluster) can be reached.
func (r *RedisClient) IsAvailable() bool {
	r.mu.RLock()
	isClosed := r.closed
	localClient := r.client
	r.mu.RUnlock()

	if isClosed {
		r.log.Warnf("Checking availability on a closed client.")
		return false
	}
	if localClient == nil {
		r.log.Warnf("Checking availability, but client is not initialized.")
		return false
	}

	cmdable, ok := localClient.(redis.Cmdable)
	if !ok {
		r.log.Errorf("Internal error: client does not implement redis.Cmdable.")
		return false
	}
	if cmdable == nil {
		r.log.Errorf("Internal error: client instance is nil, but client field was not.")
		return false
	}

	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b := backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(500*time.Millisecond), 3), ctx)
	err = backoff.Retry(func() error {
		pingCtx, pingCancel := context.WithTimeout(ctx, 1*time.Second)
		defer pingCancel()
		pingErr := cmdable.Ping(pingCtx).Err()
		if pingErr != nil {
			mode := "Ring"
			if r.clusterMode {
				mode = "Cluster"
			}
			r.log.Debugf("Failed to ping redis (%s mode), retry with backoff: %v", mode, pingErr)
		}
		return pingErr
	}, b)

	if err != nil {
		mode := "Ring"
		if r.clusterMode {
			mode = "Cluster"
		}
		r.log.Warnf("Redis client (%s mode) is unavailable after retries: %v", mode, err)
		return false
	}

	return true
}

// StartMetricsCollection starts collecting connection pool statistics.
func (r *RedisClient) StartMetricsCollection(ctx context.Context) {
	r.mu.RLock()
	localClient := r.client
	optionsIdleTimeout := r.options.IdleTimeout
	optionsIdleCheckFreq := r.options.IdleCheckFrequency
	optionsConnMetricsInterval := r.options.ConnMetricsInterval
	r.mu.RUnlock()

	if localClient == nil {
		r.log.Warnf("Cannot start metrics collection, client is not initialized.")
		return
	}
	if optionsIdleCheckFreq > 0 && optionsIdleTimeout <= 0 {
		r.log.Warnf("RedisOptions.IdleCheckFrequency is set (%v) but IdleTimeout is not (> 0), so idle connections may not be reaped as expected.", optionsIdleCheckFreq)
	} else if optionsIdleCheckFreq <= 0 && optionsIdleTimeout > 0 {
		r.log.Debugf("RedisOptions.IdleTimeout is set (%v), idle connections will be checked internally by go-redis.", optionsIdleTimeout)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		ticker := time.NewTicker(optionsConnMetricsInterval)
		defer ticker.Stop()

		r.log.Infof("Starting Redis metrics collection every %s", optionsConnMetricsInterval)
		defer r.log.Info("Stopped Redis metrics collection")

		for {
			select {
			case <-ticker.C:
				select {
				case <-r.quit:
					return
				case <-ctx.Done():
					return
				default:
				}

				r.mu.RLock()
				localClientForTick := r.client
				localIsClosed := r.closed
				localClusterMode := r.clusterMode
				localOptionsAddrs := make([]string, len(r.options.Addrs))
				copy(localOptionsAddrs, r.options.Addrs)
				r.mu.RUnlock()

				if localIsClosed {
					return
				}
				if localClientForTick == nil {
					r.log.Warn("Metrics collection: client became nil.")
					continue
				}

				var stats *redis.PoolStats
				var ok bool

				if localClusterMode {
					var clusterClient *redis.ClusterClient
					clusterClient, ok = localClientForTick.(*redis.ClusterClient)
					if ok && clusterClient != nil {
						stats = clusterClient.PoolStats()
					} else if !ok {
						r.log.Error("Metrics collection: client is not a *redis.ClusterClient in cluster mode.")
						continue
					} else {
						r.log.Warn("Metrics collection: cluster client instance is nil.")
						continue
					}
				} else {
					var ringClient *redis.Ring
					ringClient, ok = localClientForTick.(*redis.Ring)
					if ok && ringClient != nil {
						stats = ringClient.PoolStats()
						liveShards := ringClient.Len()
						r.metrics.UpdateGauge(r.metricsPrefix+"shards.live", float64(liveShards))
						r.metrics.UpdateGauge(r.metricsPrefix+"shards.configured", float64(len(localOptionsAddrs)))
					} else if !ok {
						r.log.Error("Metrics collection: client is not a *redis.Ring in ring mode.")
						continue
					} else {
						r.log.Warn("Metrics collection: ring client instance is nil.")
						continue
					}
				}

				if stats != nil {
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.hits", float64(stats.Hits))
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.misses", float64(stats.Misses))
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.timeouts", float64(stats.Timeouts))
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.staleconns", float64(stats.StaleConns))
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.idleconns", float64(stats.IdleConns))
					r.metrics.UpdateGauge(r.metricsPrefix+"pool.totalconns", float64(stats.TotalConns))
				}

			case <-r.quit:
				return
			case <-ctx.Done():
				r.log.Info("Redis metrics collection stopping due to context cancellation.")
				return
			}
		}
	}()
}

// StartSpan starts an OpenTracing span.
func (r *RedisClient) StartSpan(operationName string, opts ...opentracing.StartSpanOption) opentracing.Span {
	if r.tracer == nil {
		return opentracing.NoopTracer{}.StartSpan(operationName)
	}
	return r.tracer.StartSpan(operationName, opts...)
}

// Close shuts down the Redis client (Ring or Cluster).
func (r *RedisClient) Close() {
	r.once.Do(func() {
		r.log.Info("Closing Redis client...")

		if r.cancel != nil {
			r.cancel()
		}
		close(r.quit)

		r.log.Debug("Waiting for background goroutines to exit...")
		r.wg.Wait()
		r.log.Debug("Background goroutines finished.")

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			r.log.Warn("Close called again after already closed.")
			return
		}
		r.closed = true
		clientToClose := r.client
		r.client = nil
		r.mu.Unlock()

		if clientToClose == nil {
			r.log.Warn("Attempted to close an uninitialized or already nil Redis client instance.")
			return
		}

		var err error
		if r.clusterMode {
			if clusterClient, ok := clientToClose.(*redis.ClusterClient); ok && clusterClient != nil {
				err = clusterClient.Close()
			} else if !ok {
				r.log.Error("Close: clientToClose is not a *redis.ClusterClient in cluster mode.")
			} else {
				r.log.Warn("Close: cluster client instance was nil during close.")
			}
		} else {
			if ringClient, ok := clientToClose.(*redis.Ring); ok && ringClient != nil {
				err = ringClient.Close()
			} else if !ok {
				r.log.Error("Close: clientToClose is not a *redis.Ring in ring mode.")
			} else {
				r.log.Warn("Close: ring client instance was nil during close.")
			}
		}

		if err != nil {
			r.log.Errorf("Error closing Redis client: %v", err)
		} else {
			r.log.Info("Redis client closed successfully.")
		}
	})
}

// SetAddrs updates the addresses for a Ring client. No-op in Cluster mode.
func (r *RedisClient) SetAddrs(ctx context.Context, addrs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	select {
	case <-ctx.Done():
		r.log.Warnf("SetAddrs not executed due to context cancellation: %v", ctx.Err())
		return
	default:
	}

	if r.closed {
		r.log.Warn("SetAddrs called on a closed client.")
		return
	}
	if r.clusterMode {
		r.log.Debugf("SetAddrs called in Cluster mode, has no effect.")
		return
	}

	localClient := r.client
	if localClient == nil {
		r.log.Error("SetAddrs called but client is not initialized.")
		return
	}

	ringClient, ok := localClient.(*redis.Ring)
	if !ok {
		r.log.Errorf("SetAddrs called but client is not a Ring.")
		return
	}
	if ringClient == nil {
		r.log.Error("SetAddrs called but ring client instance is nil.")
		return
	}

	addrMap := createAddressMap(addrs)
	if len(addrs) == 0 {
		r.log.Warn("SetAddrs called with empty address list. Ring might become unusable.")
	}

	r.log.Infof("Updating Ring addresses via SetAddrs: %v", addrs)
	ringClient.SetAddrs(addrMap)

	r.options.Addrs = make([]string, len(addrs))
	copy(r.options.Addrs, addrs)
}

// getCmdable safely retrieves the underlying Cmdable interface.
func (r *RedisClient) getCmdable() (redis.Cmdable, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, fmt.Errorf("redis client is closed")
	}
	localClient := r.client
	if localClient == nil {
		return nil, fmt.Errorf("redis client is not initialized")
	}
	cmdable, ok := localClient.(redis.Cmdable)
	if !ok {
		r.log.Errorf("Internal error: client type %T is not redis.Cmdable", localClient)
		return nil, fmt.Errorf("internal error: redis client is not Cmdable")
	}
	if cmdable == nil {
		return nil, fmt.Errorf("redis client internal instance is nil")
	}
	return cmdable, nil
}

// Get executes the GET command.
func (r *RedisClient) Get(ctx context.Context, key string) (string, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return "", err
	}
	res := cmdable.Get(ctx, key)
	val, err := res.Result()
	if err == redis.Nil {
		return "", err
	}
	return val, err
}

// Set executes the SET command.
func (r *RedisClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) (string, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return "", err
	}
	res := cmdable.Set(ctx, key, value, expiration)
	return res.Result()
}

// ZAdd executes the ZADD command.
func (r *RedisClient) ZAdd(ctx context.Context, key string, val int64, score float64) (int64, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return 0, err
	}
	res := cmdable.ZAdd(ctx, key, redis.Z{Member: fmt.Sprint(val), Score: score})
	return res.Val(), res.Err()
}

// ZRem executes the ZREM command.
func (r *RedisClient) ZRem(ctx context.Context, key string, members ...interface{}) (int64, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return 0, err
	}
	res := cmdable.ZRem(ctx, key, members...)
	return res.Val(), res.Err()
}

// Expire executes the EXPIRE command.
func (r *RedisClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	if expiration <= 0 {
		return true, nil
	}
	cmdable, err := r.getCmdable()
	if err != nil {
		return false, err
	}
	res := cmdable.Expire(ctx, key, expiration)
	return res.Val(), res.Err()
}

// ZRemRangeByScore executes the ZREMRANGEBYSCORE command.
func (r *RedisClient) ZRemRangeByScore(ctx context.Context, key string, min, max float64) (int64, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return 0, err
	}
	res := cmdable.ZRemRangeByScore(ctx, key, fmt.Sprint(min), fmt.Sprint(max))
	return res.Val(), res.Err()
}

// ZRangeWithScores executes the ZRANGE WITHSCORES command.
func (r *RedisClient) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]redis.Z, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return nil, err
	}
	return cmdable.ZRangeWithScores(ctx, key, start, stop).Result()
}

// ZCard executes the ZCARD command.
func (r *RedisClient) ZCard(ctx context.Context, key string) (int64, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return 0, err
	}
	res := cmdable.ZCard(ctx, key)
	return res.Val(), res.Err()
}

// ZRangeByScoreWithScoresFirst gets the first element within a score range.
func (r *RedisClient) ZRangeByScoreWithScoresFirst(ctx context.Context, key string, min, max float64, offset, count int64) (interface{}, error) {
	cmdable, err := r.getCmdable()
	if err != nil {
		return nil, err
	}
	opt := redis.ZRangeBy{
		Min:    fmt.Sprint(min),
		Max:    fmt.Sprint(max),
		Offset: offset,
		Count:  count,
	}
	if count <= 0 {
		opt.Count = 1
	}

	res := cmdable.ZRangeByScoreWithScores(ctx, key, &opt)
	zs, err := res.Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	if len(zs) == 0 {
		return nil, nil
	}

	return zs[0].Member, nil
}

// NewScript creates a new RedisScript instance.
func (r *RedisClient) NewScript(source string) *RedisScript {
	return &RedisScript{redis.NewScript(source)}
}

// RunScript executes a pre-loaded Lua script.
func (r *RedisClient) RunScript(ctx context.Context, s *RedisScript, keys []string, args ...interface{}) (interface{}, error) {
	if s == nil || s.script == nil {
		return nil, errors.New("invalid RedisScript provided")
	}
	cmdable, err := r.getCmdable()
	if err != nil {
		return nil, err
	}
	return s.script.Run(ctx, cmdable, keys, args...).Result()
}

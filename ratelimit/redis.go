package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/metrics"
	"github.com/zalando/skipper/net"
	"golang.org/x/time/rate"
)

// clusterLimitRedis stores all data required for the cluster ratelimit.
type clusterLimitRedis struct {
	failClosed  bool
	typ         string
	group       string
	maxHits     int64
	window      time.Duration
	redisClient *net.RedisClient
	metrics     metrics.Metrics
	sometimes   rate.Sometimes
}

const (
	redisMetricsPrefix               = "swarm.redis."
	allowMetricsFormat               = redisMetricsPrefix + "query.allow.%s"
	retryAfterMetricsFormat          = redisMetricsPrefix + "query.retryafter.%s"
	allowMetricsFormatWithGroup      = redisMetricsPrefix + "query.allow.%s.%s"
	retryAfterMetricsFormatWithGroup = redisMetricsPrefix + "query.retryafter.%s.%s"

	allowSpanName       = "redis_allow"
	oldestScoreSpanName = "redis_oldest_score"
)

// newClusterRateLimiterRedis creates a new clusterLimitRedis for given
// Settings. Group is used to identify the ratelimit instance, is used
// in log messages and has to be the same in all skipper instances.
func newClusterRateLimiterRedis(s Settings, r *net.RedisClient, group string) *clusterLimitRedis {
	if r == nil {
		log.Warnf("newClusterRateLimiterRedis called with nil RedisClient for group '%s'. Redis-based limiting will be disabled.", group)
		return nil
	}

	rl := &clusterLimitRedis{
		failClosed:  s.FailClosed,
		typ:         s.Type.String(),
		group:       group,
		maxHits:     int64(s.MaxHits),
		window:      s.TimeWindow,
		redisClient: r,
		metrics:     metrics.Default,
		sometimes:   rate.Sometimes{First: 3, Interval: 1 * time.Second},
	}

	log.Infof("Created Redis-based cluster rate limiter for group '%s', type '%s', maxHits %d, window %v", group, s.Type, s.MaxHits, s.TimeWindow)
	return rl
}

func (c *clusterLimitRedis) prefixKey(clearText string) string {
	groupName := c.group
	if groupName == "" {
		groupName = "default_ratelimit_group"
	}
	return fmt.Sprintf(swarmKeyFormat, groupName, clearText)
}

func (c *clusterLimitRedis) measureQuery(format, groupFormat string, fail *bool, start time.Time) {
	if c.metrics == nil {
		return
	}

	result := "success"
	if fail != nil && *fail {
		result = "failure"
	}

	var key string
	if c.group == "" {
		key = fmt.Sprintf(format, result)
	} else {
		key = fmt.Sprintf(groupFormat, result, c.group)
	}

	c.metrics.MeasureSince(key, start)
}

func parentSpan(ctx context.Context) opentracing.Span {
	return opentracing.SpanFromContext(ctx)
}

func (c *clusterLimitRedis) commonTags() opentracing.Tags {
	return opentracing.Tags{
		string(ext.Component): "skipper",
		string(ext.DBType):    "redis",
		string(ext.SpanKind):  ext.SpanKindRPCClientEnum,
		"ratelimit_type":      c.typ,
		"ratelimit_group":     c.group,
		"ratelimit_max_hits":  c.maxHits,
		"ratelimit_window":    c.window.String(),
	}
}

// Allow returns true if the request calculated across the cluster of
// skippers should be allowed else false. It will share it's own data
// and use the current cluster information to calculate global rates
// to decide to allow or not.
//
// Performance considerations:
//
// In case of deny it will use ZREMRANGEBYSCORE and ZCARD commands in
// one pipeline to remove old items in the list of hits.
// In case of allow it will additionally use ZADD with a second
// roundtrip.
//
// Uses provided context for creating an OpenTracing span.
func (c *clusterLimitRedis) Allow(ctx context.Context, clearText string) bool {
	if c.redisClient == nil {
		log.Warnf("Allow called for group '%s' but Redis client is nil. Failing open/closed based on config: %v", c.group, !c.failClosed)
		return !c.failClosed
	}

	c.metrics.IncCounter(redisMetricsPrefix + "total")
	now := time.Now()

	var span opentracing.Span
	if parent := parentSpan(ctx); parent != nil {
		span = c.redisClient.StartSpan(allowSpanName, opentracing.ChildOf(parent.Context()), c.commonTags())
		defer span.Finish()
	}

	allow, err := c.allow(ctx, clearText)
	failed := err != nil
	if failed {
		allow = !c.failClosed
		msgFmt := "Failed to determine if operation is allowed using Redis: %v"
		setError(span, fmt.Sprintf(msgFmt, err))
		c.logError(msgFmt, err)
	}
	if span != nil {
		span.SetTag("allowed", allow)
		if failed {
			span.SetTag("fail_mode", map[bool]string{true: "closed", false: "open"}[c.failClosed])
		}
	}

	c.measureQuery(allowMetricsFormat, allowMetricsFormatWithGroup, &failed, now)

	if allow {
		c.metrics.IncCounter(redisMetricsPrefix + "allows")
	} else {
		c.metrics.IncCounter(redisMetricsPrefix + "forbids")
	}
	return allow
}

func (c *clusterLimitRedis) allow(ctx context.Context, clearText string) (bool, error) {
	if c.redisClient == nil {
		return !c.failClosed, errors.New("redis client is not initialized")
	}

	s := getHashedKey(clearText)
	key := c.prefixKey(s)

	now := time.Now()
	nowNanos := now.UnixNano()
	clearBefore := now.Add(-c.window).UnixNano()

	if _, err := c.redisClient.ZRemRangeByScore(ctx, key, 0.0, float64(clearBefore)); err != nil {
		return false, fmt.Errorf("ZRemRangeByScore failed for key '%s' (group '%s'): %w", key, c.group, err)
	}

	count, err := c.redisClient.ZCard(ctx, key)
	if err != nil {
		return false, fmt.Errorf("ZCard failed for key '%s' (group '%s'): %w", key, c.group, err)
	}

	if count >= c.maxHits {
		return false, nil
	}

	_, err = c.redisClient.ZAdd(ctx, key, nowNanos, float64(nowNanos))
	if err != nil {
		log.Warnf("Redis ZAdd failed for key '%s' (group '%s') after allow check: %v", key, c.group, err)
	}

	expireDuration := c.window + 5*time.Second
	if _, err := c.redisClient.Expire(ctx, key, expireDuration); err != nil {
		log.Warnf("Redis Expire failed for key %s: %v", key, err)
	}

	return true, nil
}

func (c *clusterLimitRedis) Close() {}

func (c *clusterLimitRedis) deltaFrom(ctx context.Context, clearText string, from time.Time) (time.Duration, error) {
	oldest, err := c.oldest(ctx, clearText)
	if err != nil {
		return 0, fmt.Errorf("failed to get oldest entry: %w", err)
	}
	if oldest.IsZero() {
		return -1 * time.Hour, nil
	}

	expiryTime := oldest.Add(c.window)
	delta := expiryTime.Sub(from)

	return delta, nil
}

// Delta returns the time.Duration until the next call is allowed,
// negative means immediate calls are allowed
func (c *clusterLimitRedis) Delta(clearText string) time.Duration {
	if c.redisClient == nil {
		log.Warnf("Delta called for group '%s' but Redis client is nil. Returning large negative duration.", c.group)
		return -1 * time.Hour
	}

	now := time.Now()
	d, err := c.deltaFrom(context.Background(), clearText, now)
	if err != nil {
		c.logError("Failed to get the duration until the next call is allowed (Delta): %v", err)
		// Earlier, we returned duration since time=0 in these error cases. It is more graceful to the
		// client applications to return 0.
		return 0
	}

	return d
}

func setError(span opentracing.Span, msg string) {
	if span != nil {
		ext.Error.Set(span, true)
		span.LogKV("error.message", msg)
	}
}

func (c *clusterLimitRedis) logError(format string, err error) {
	c.sometimes.Do(func() {
		log.Errorf("Redis Rate Limiter (group: %s, type: %s): "+format, c.group, c.typ, err)
	})
}

func (c *clusterLimitRedis) oldest(ctx context.Context, clearText string) (time.Time, error) {
	if c.redisClient == nil {
		return time.Time{}, errors.New("redis client is not initialized for oldest")
	}

	s := getHashedKey(clearText)
	key := c.prefixKey(s)

	var span opentracing.Span
	if parent := parentSpan(ctx); parent != nil {
		span = c.redisClient.StartSpan(oldestScoreSpanName, opentracing.ChildOf(parent.Context()), c.commonTags())
		defer span.Finish()
	}

	results, err := c.redisClient.ZRangeWithScores(ctx, key, 0, 0)
	if err != nil {
		setError(span, fmt.Sprintf("Failed to execute ZRangeWithScores: %v", err))
		return time.Time{}, fmt.Errorf("ZRangeWithScores failed: %w", err)
	}

	if len(results) == 0 {
		return time.Time{}, nil
	}

	oldestScore := results[0].Score
	oldestNanos := int64(oldestScore)

	if span != nil {
		span.LogKV("oldest_timestamp_ns", oldestNanos)
	}

	return time.Unix(0, oldestNanos), nil
}

// Oldest returns the oldest known request time.
//
// Performance considerations:
//
// It will use ZRANGEBYSCORE with offset 0 and count 1 to get the
// oldest item stored in redis.
func (c *clusterLimitRedis) Oldest(clearText string) time.Time {
	if c.redisClient == nil {
		log.Warnf("Oldest called for group '%s' but Redis client is nil. Returning zero time.", c.group)
		return time.Time{}
	}

	t, err := c.oldest(context.Background(), clearText)
	if err != nil {
		c.logError("Failed to get the oldest known request time (Oldest): %v", err)
		return time.Time{}
	}

	return t
}

func (*clusterLimitRedis) Resize(string, int) {}

// RetryAfterContext returns seconds until next call is allowed similar to
// Delta(), but returns at least one 1 in all cases. That is being
// done, because if not the ratelimit would be too few ratelimits,
// because of how it's used in the proxy and the nature of cluster
// ratelimits being not strongly consistent across calls to Allow()
// and RetryAfter() (or Allow and RetryAfterContext accordingly).
//
// Uses context for creating an OpenTracing span.
func (c *clusterLimitRedis) RetryAfterContext(ctx context.Context, clearText string) int {
	// If less than 1s to wait -> so set to 1
	const minWait = 1

	if c.redisClient == nil {
		log.Warnf("RetryAfterContext called for group '%s' but Redis client is nil. Returning %d.", c.group, minWait)
		return minWait
	}

	now := time.Now()
	var queryFailure bool
	defer c.measureQuery(retryAfterMetricsFormat, retryAfterMetricsFormatWithGroup, &queryFailure, now)

	retr, err := c.deltaFrom(ctx, clearText, now)
	if err != nil {
		c.logError("Failed to get the duration to wait until the next request (RetryAfterContext): %v", err)
		queryFailure = true
		return minWait
	}

	if retr <= 0 {
		return 0
	}

	res := int(retr.Seconds() + 0.999)
	if res < minWait {
		return minWait
	}

	return res
}

// RetryAfter is like RetryAfterContext, but not using a context.
func (c *clusterLimitRedis) RetryAfter(clearText string) int {
	return c.RetryAfterContext(context.Background(), clearText)
}

package ratelimit

import (
	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/net"
)

const (
	swarmPrefix    = `ratelimit.`
	swarmKeyFormat = swarmPrefix + "%s.%s"
)

// newClusterRateLimiter will return a limiter instance, that has a
// cluster wide knowledge of ongoing requests. Settings are the normal
// ratelimit settings, Swarmer is an instance satisfying the Swarmer
// interface, which is one of swarm.Swarm or noopSwarmer,
// swarm.Options to configure a swarm.Swarm, redisClient is a net.RedisClient
// (supporting both Ring and Cluster modes) and group is the ratelimit group
// that can span one or multiple routes.
func newClusterRateLimiter(s Settings, sw Swarmer, redisClient *net.RedisClient, group string) limiter {
	if sw != nil {
		if l := newClusterRateLimiterSwim(s, sw, group); l != nil {
			log.Infof("Using Swim-based cluster rate limiter for group '%s'", group)
			return l
		}
		log.Warnf("Swarmer provided for group '%s', but Swim limiter initialization failed. Checking for Redis.", group)
	}

	if redisClient != nil {
		if l := newClusterRateLimiterRedis(s, redisClient, group); l != nil {
			return l
		}
		log.Warnf("Redis client provided for group '%s', but Redis limiter initialization failed.", group)
	}

	log.Warnf("Neither Swim nor Redis cluster rate limiter could be initialized for group '%s'. Using voidRatelimit (no limiting).", group)
	return voidRatelimit{}
}

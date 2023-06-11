package backoff

import (
	"context"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	lru "github.com/hashicorp/golang-lru/v2"
)

// BackoffConnector is a utility to connect to peers, but only if we have not recently tried connecting to them already
// 是一个实用工具，用于连接到节点，但仅在最近没有尝试连接到它们时才进行连接
type BackoffConnector struct {
	// LRU (Least Recently Used) 算法是一种缓存淘汰策略，用于管理有限容量的缓存。它的目标是在缓存空间不足时，淘汰最近最少使用的项目，以便为新的项目腾出空间。
	cache      *lru.TwoQueueCache[peer.ID, *connCacheData] // 用于存储对等节点连接的缓存，使用 LRU (Least Recently Used) 算法进行管理
	host       host.Host
	connTryDur time.Duration  // 连接尝试的持续时间，即在给定时间段内只允许尝试连接一次
	backoff    BackoffFactory // 用于计算指数退避间隔的策略
	mux        sync.Mutex
}

// NewBackoffConnector creates a utility to connect to peers, but only if we have not recently tried connecting to them already
// cacheSize is the size of a TwoQueueCache
// connectionTryDuration is how long we attempt to connect to a peer before giving up
// backoff describes the strategy used to decide how long to backoff after previously attempting to connect to a peer
func NewBackoffConnector(h host.Host, cacheSize int, connectionTryDuration time.Duration, backoff BackoffFactory) (*BackoffConnector, error) {
	cache, err := lru.New2Q[peer.ID, *connCacheData](cacheSize)
	if err != nil {
		return nil, err
	}

	return &BackoffConnector{
		cache:      cache,
		host:       h,
		connTryDur: connectionTryDuration,
		backoff:    backoff,
	}, nil
}

type connCacheData struct {
	nextTry time.Time
	strat   BackoffStrategy
}

// Connect attempts to connect to the peers passed in by peerCh. Will not connect to peers if they are within the backoff period.
// As Connect will attempt to dial peers as soon as it learns about them, the caller should try to keep the number,
// and rate, of inbound peers manageable.
func (c *BackoffConnector) Connect(ctx context.Context, peerCh <-chan peer.AddrInfo) {
	for {
		select {
		case pi, ok := <-peerCh:
			if !ok {
				return
			}

			if pi.ID == c.host.ID() || pi.ID == "" {
				continue
			}

			c.mux.Lock()
			var cachedPeer *connCacheData
			if tv, ok := c.cache.Get(pi.ID); ok {
				now := time.Now()
				if now.Before(tv.nextTry) {
					c.mux.Unlock()
					continue
				}

				tv.nextTry = now.Add(tv.strat.Delay())
			} else {
				cachedPeer = &connCacheData{strat: c.backoff()}
				cachedPeer.nextTry = time.Now().Add(cachedPeer.strat.Delay())
				c.cache.Add(pi.ID, cachedPeer)
			}
			c.mux.Unlock()

			go func(pi peer.AddrInfo) {
				ctx, cancel := context.WithTimeout(ctx, c.connTryDur)
				defer cancel()

				err := c.host.Connect(ctx, pi)
				if err != nil {
					log.Debugf("Error connecting to pubsub peer %s: %s", pi.ID, err.Error())
					return
				}
			}(pi)

		case <-ctx.Done():
			log.Infof("discovery: backoff connector context error %v", ctx.Err())
			return
		}
	}
}

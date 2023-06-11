package routing

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/discovery"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// 提供了基于路由表的服务发现功能，主要用于在 libp2p 网络中维护和查询节点的路由表信息。

// RoutingDiscovery is an implementation of discovery using ContentRouting.
// Namespaces are translated to Cids using the SHA256 hash.
// 它将命名空间（Namespaces）转换为使用 SHA256 哈希算法生成的 CID
type RoutingDiscovery struct {
	routing.ContentRouting
}

func NewRoutingDiscovery(router routing.ContentRouting) *RoutingDiscovery {
	return &RoutingDiscovery{router}
}

// 将本节点的路由表信息发布到网络中
// 命名空间是一种人类可读的标识符，用于表示特定内容或资源
// 在网络中，节点之间通常使用唯一的标识符来识别和定位内容，这些标识符通常是基于内容的哈希值
// CID（Content Identifier）是一种用于表示内容的哈希值的标识符，它可以唯一标识内容，无论其位置在哪里。
func (d *RoutingDiscovery) Advertise(ctx context.Context, ns string, opts ...discovery.Option) (time.Duration, error) {
	var options discovery.Options
	err := options.Apply(opts...)
	if err != nil {
		return 0, err
	}

	ttl := options.Ttl
	if ttl == 0 || ttl > 3*time.Hour {
		// the DHT provider record validity is 24hrs, but it is recommnded to republish at least every 6hrs
		// we go one step further and republish every 3hrs
		ttl = 3 * time.Hour
	}

	cid, err := nsToCid(ns)
	if err != nil {
		return 0, err
	}

	// this context requires a timeout; it determines how long the DHT looks for
	// closest peers to the key/CID before it goes on to provide the record to them.
	// Not setting a timeout here will make the DHT wander forever.
	// 创建一个具有超时时间的子上下文 pctx
	// 用于在一定时间内寻找最近的节点来提供广告记录。如果超过超时时间仍未找到节点，则广告发布过程终止。
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	//将 CID 提供给路由系统进行广播
	// 如果传递 true，它还会将其进行广播（announce），否则仅在本地的账本中保留提供的对象信息。
	err = d.Provide(pctx, cid, true)
	if err != nil {
		return 0, err
	}

	return ttl, nil
}

// 基于路由系统的发现中查找提供特定命名空间（ns）的节点
// 并使用路由系统的 FindProvidersAsync 方法进行异步查找
func (d *RoutingDiscovery) FindPeers(ctx context.Context, ns string, opts ...discovery.Option) (<-chan peer.AddrInfo, error) {
	var options discovery.Options
	err := options.Apply(opts...)
	if err != nil {
		return nil, err
	}

	limit := options.Limit
	if limit == 0 {
		limit = 100 // that's just arbitrary, but FindProvidersAsync needs a count
	}

	cid, err := nsToCid(ns)
	if err != nil {
		return nil, err
	}

	return d.FindProvidersAsync(ctx, cid, limit), nil
}

func nsToCid(ns string) (cid.Cid, error) {
	h, err := mh.Sum([]byte(ns), mh.SHA2_256, -1)
	if err != nil {
		return cid.Undef, err
	}

	return cid.NewCidV1(cid.Raw, h), nil
}

func NewDiscoveryRouting(disc discovery.Discovery, opts ...discovery.Option) *DiscoveryRouting {
	return &DiscoveryRouting{disc, opts}
}

// RoutingDiscovery 是基于内容路由的发现机制，而 DiscoveryRouting 是基于路由表的发现机制
type DiscoveryRouting struct {
	discovery.Discovery
	opts []discovery.Option
}

func (r *DiscoveryRouting) Provide(ctx context.Context, c cid.Cid, bcast bool) error {
	if !bcast {
		return nil
	}

	_, err := r.Advertise(ctx, cidToNs(c), r.opts...)
	return err
}

func (r *DiscoveryRouting) FindProvidersAsync(ctx context.Context, c cid.Cid, limit int) <-chan peer.AddrInfo {
	ch, _ := r.FindPeers(ctx, cidToNs(c), append([]discovery.Option{discovery.Limit(limit)}, r.opts...)...)
	return ch
}

func cidToNs(c cid.Cid) string {
	return "/provider/" + c.String()
}

package autonat

import (
	"context"
	"io"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
)

// AutoNAT is the interface for NAT autodiscovery
type AutoNAT interface {
	// Status returns the current NAT status
	// 表示节点的可达性状态，可以是公共网络、私有网络、未知状态等。
	Status() network.Reachability
	// 该接口继承了 io.Closer 接口，表示可以关闭 AutoNAT 实例，释放相关资源。
	io.Closer
}

// 向提供 AutoNAT 服务的节点发起请求，测试拨号回连并报告成功连接的地址。通过调用这个方法，可以让提供 AutoNAT 服务的节点回拨连接客户端，并返回连接成功的结果。
// 客户端是用于发起请求并获取自己的可达性结果的一端。客户端通过调用 DialBack 方法向提供 AutoNAT 服务的节点发送请求，请求节点回拨连接并返回连接的结果，从而确定自己的可达性状态。
// Client is a stateless client interface to AutoNAT peers
type Client interface {
	// DialBack requests from a peer providing AutoNAT services to test dial back
	// and report the address on a successful connection.
	DialBack(ctx context.Context, p peer.ID) error
}

// AddrFunc is a function returning the candidate addresses for the local host.
// 用于返回本地主机的候选地址
// 这些候选地址将被用于拨号、探测和通信等功能
type AddrFunc func() []ma.Multiaddr

// Option is an Autonat option for configuration
type Option func(*config) error

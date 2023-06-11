package autonat

import (
	"net"

	"github.com/libp2p/go-libp2p/core/host"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

type dialPolicy struct {
	allowSelfDials bool
	host           host.Host
}

// skipDial indicates that a multiaddress isn't worth attempted dialing.
// The same logic is used when the autonat client is considering if
// a remote peer is worth using as a server, and when the server is
// considering if a requested client is worth dialing back.

// 用于判断一个多地址（multiaddress）是否值得尝试进行拨号
// 对节点多地址分析，是否跳过拨号
func (d *dialPolicy) skipDial(addr ma.Multiaddr) bool {
	// skip relay addresses
	// 跳过中继地址
	// 通过判断多地址中是否包含 Circuit Relay 协议（P_CIRCUIT）
	_, err := addr.ValueForProtocol(ma.P_CIRCUIT)
	// 跳过中继地址的目的是为了避免尝试与中继节点建立直接的拨号连接
	// 为了优化拨号操作的效率和性能，会跳过中继地址，直接尝试与目标节点建立直接的通信链路，而不是通过中继节点进行转发
	if err == nil {
		return true
	}
	// 如果允许节点自己拨号，则不跳过
	// 在 AutoNAT 中，当节点在拨号请求中尝试与自身建立连接时，可以通过设置 allowSelfDials 参数来决定是否跳过该拨号操作。
	if d.allowSelfDials {
		return false
	}

	// skip private network (unroutable) addresses
	// 判断多地址是否属于私有网络，如果是私有网络地址，可以跳过拨号
	if !manet.IsPublicAddr(addr) {
		return true
	}
	// 如果多地址不属于私有网络，则获取候选 IP 地址
	candidateIP, err := manet.ToIP(addr)
	if err != nil {
		return true
	}

	// Skip dialing addresses we believe are the local node's
	for _, localAddr := range d.host.Addrs() {
		localIP, err := manet.ToIP(localAddr)
		if err != nil {
			continue
		}
		// 如果存在与候选 IP 地址相等的本地地址
		// 排除掉与本地节点相同的地址，从而避免节点与自身进行拨号连接。
		if localIP.Equal(candidateIP) {
			return true
		}
	}

	return false
}

// skipPeer indicates that the collection of multiaddresses representing a peer
// isn't worth attempted dialing. If one of the addresses matches an address
// we believe is ours, we exclude the peer, even if there are other valid
// public addresses in the list.

// 判断一组表示某个节点的多地址集合是否值得进行拨号连接
// 如果其中一个地址与我们认为属于本地节点的地址匹配，即使列表中存在其他有效的公共地址，我们也会排除该节点。
// 这样做的目的是确保不会与本地节点发生拨号冲突。如果远程节点的某个地址与本地节点的地址相同，即表示该节点与本地节点可能是同一个节点，因此不需要进行拨号连接
func (d *dialPolicy) skipPeer(addrs []ma.Multiaddr) bool {
	localAddrs := d.host.Addrs() // 获取本地地址
	localHosts := make([]net.IP, 0)
	for _, lAddr := range localAddrs {
		// 如果节点不是中继节点，且地址是公网地址
		if _, err := lAddr.ValueForProtocol(ma.P_CIRCUIT); err != nil && manet.IsPublicAddr(lAddr) {
			lIP, err := manet.ToIP(lAddr)
			if err != nil {
				continue
			}
			localHosts = append(localHosts, lIP)
		}
	}

	// if a public IP of the peer is one of ours: skip the peer.
	goodPublic := false
	for _, addr := range addrs {
		// 判断多地址如果不是中继地址，并且是一个公网地址
		if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err != nil && manet.IsPublicAddr(addr) {
			aIP, err := manet.ToIP(addr)
			if err != nil {
				continue
			}

			for _, lIP := range localHosts {
				if lIP.Equal(aIP) {
					return true
				}
			}
			goodPublic = true
		}
	}

	if d.allowSelfDials {
		return false
	}

	return !goodPublic
}

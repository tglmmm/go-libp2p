package mdns

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/libp2p/zeroconf/v2"

	logging "github.com/ipfs/go-log/v2"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

const (
	ServiceName   = "_p2p._udp" // MDNS的服务名称
	mdnsDomain    = "local"     // 范围，表示在本地局域网中进行MDNS发现
	dnsaddrPrefix = "dnsaddr="
)

var log = logging.Logger("mdns")

type Service interface {
	Start() error
	io.Closer
}

type Notifee interface {
	HandlePeerFound(peer.AddrInfo)
}

type mdnsService struct {
	host        host.Host
	serviceName string
	peerName    string

	// The context is canceled when Close() is called.
	ctx       context.Context
	ctxCancel context.CancelFunc

	resolverWG sync.WaitGroup
	server     *zeroconf.Server

	notifee Notifee
}

func NewMdnsService(host host.Host, serviceName string, notifee Notifee) *mdnsService {
	if serviceName == "" {
		serviceName = ServiceName
	}
	s := &mdnsService{
		host:        host,
		serviceName: serviceName,
		peerName:    randomString(32 + rand.Intn(32)), // generate a random string between 32 and 63 characters long
		notifee:     notifee,
	}
	s.ctx, s.ctxCancel = context.WithCancel(context.Background())
	return s
}

func (s *mdnsService) Start() error {
	// 开启注册服务用于广播数据
	if err := s.startServer(); err != nil {
		return err
	}
	//
	s.startResolver(s.ctx)
	return nil
}

func (s *mdnsService) Close() error {
	s.ctxCancel()
	if s.server != nil {
		s.server.Shutdown()
	}
	s.resolverWG.Wait()
	return nil
}

// We don't really care about the IP addresses, but the spec (and various routers / firewalls) require us
// to send A and AAAA records.
func (s *mdnsService) getIPs(addrs []ma.Multiaddr) ([]string, error) {
	var ip4, ip6 string
	for _, addr := range addrs {
		first, _ := ma.SplitFirst(addr)
		if first == nil {
			continue
		}
		if ip4 == "" && first.Protocol().Code == ma.P_IP4 {
			ip4 = first.Value()
		} else if ip6 == "" && first.Protocol().Code == ma.P_IP6 {
			ip6 = first.Value()
		}
	}
	ips := make([]string, 0, 2)
	if ip4 != "" {
		ips = append(ips, ip4)
	}
	if ip6 != "" {
		ips = append(ips, ip6)
	}
	if len(ips) == 0 {
		return nil, errors.New("didn't find any IP addresses")
	}
	return ips, nil
}

func (s *mdnsService) startServer() error {
	// [/p2p-circuit /ip4/26.26.26.1/tcp/4001 /ip4/169.254.75.5/tcp/4001 /ip4/198.19.255.253/tcp/4001 /ip4/169.254.200.249/tcp/4001 /ip4/169.254.180.126/tcp/4001 /ip4/192.168.243.1/tcp/4001 /ip4/192.168.204.1/tcp/4001 /ip4/192.168.0. 107/tcp/4001 /ip4/169.254.126.189/tcp/4001 /ip4/127.0.0.1/tcp/4001]
	interfaceAddrs, err := s.host.Network().InterfaceListenAddresses()
	if err != nil {
		return err
	}
	// [/p2p-circuit/p2p/QmbSLDP4MBNMZe9HHVcwyVrpJBCgCobasUrw28hRkQJ8K9 /ip4/26.26.26.1/tcp/4001/p2p/QmbSLDP4MBNMZe9HHVcwyVrpJBCgCobasUrw28hRkQJ8K9 /ip4/169.254.75.5/tcp/4001/p2p/QmbSLDP4MBNMZe9HHVcwyVrpJBCgCobasUrw28hRkQJ8K9 /ip4/198.19.255.253/tc
	addrs, err := peer.AddrInfoToP2pAddrs(&peer.AddrInfo{
		ID:    s.host.ID(),
		Addrs: interfaceAddrs,
	})
	if err != nil {
		return err
	}
	var txts []string
	// 过滤出不是Circuit的地址
	// dnsaddr=/ip4/26.26.26.1/tcp/4001/p2p/QmUy7WjZZvLd3y6zjAHg28Y3HJfxejN1NqkMDUDru2QseR
	for _, addr := range addrs {
		if manet.IsThinWaist(addr) { // don't announce circuit addresses
			txts = append(txts, dnsaddrPrefix+addr.String())
		}
	}
	// 获取到第一个ipv4,ipv6地址
	ips, err := s.getIPs(addrs)
	if err != nil {
		return err
	}

	//	注册: 在局域网中注册一个服务,是其他设备能够发现和访问该服务,
	//	发现: 他会创建一个MDNS服务条目,并通过UDP向局域网中广播该服务信息
	//	监听局域网中的mDNS服务的广播
	//  解析: 当需要连接到某个服务时,可以使用解析方法获取该服务的网络地址
	server, err := zeroconf.RegisterProxy(
		s.peerName,
		s.serviceName,
		mdnsDomain,
		4001, // we have to pass in a port number here, but libp2p only uses the TXT records
		s.peerName,
		ips,
		txts, // txt 记录
		nil,
	)
	if err != nil {
		return err
	}
	s.server = server
	return nil
}

// 用于发现局域网中的其他节点，并提取TXT记录
func (s *mdnsService) startResolver(ctx context.Context) {
	// 这里将会开启两个携程
	s.resolverWG.Add(2)
	// 创建了一个缓冲通道 entryChan 来接收 mDNS 发现的服务条目
	entryChan := make(chan *zeroconf.ServiceEntry, 1000)
	go func() {
		defer s.resolverWG.Done()
		// 遍历接收到的TXT条目
		for entry := range entryChan {
			// We only care about the TXT records.
			// Ignore A, AAAA and PTR.
			addrs := make([]ma.Multiaddr, 0, len(entry.Text)) // assume that all TXT records are dnsaddrs
			for _, s := range entry.Text {
				// 如果没有包含dnsaddr前缀则跳过
				if !strings.HasPrefix(s, dnsaddrPrefix) {
					log.Debug("missing dnsaddr prefix")
					continue
				}
				// 根据txt条目创建一个多地址
				addr, err := ma.NewMultiaddr(s[len(dnsaddrPrefix):])
				if err != nil {
					log.Debugf("failed to parse multiaddr: %s", err)
					continue
				}
				addrs = append(addrs, addr)
			}
			// 从多地址中转换peer.AddrInfo
			infos, err := peer.AddrInfosFromP2pAddrs(addrs...)
			if err != nil {
				log.Debugf("failed to get peer info: %s", err)
				continue
			}
			// 当前循环迭代的节点是否是自身节点，来判断是否跳过对该节点的处理。
			for _, info := range infos {
				if info.ID == s.host.ID() {
					continue
				}
				// 通知上层，发现新的节点
				go s.notifee.HandlePeerFound(info)
			}
		}
	}()
	go func() {
		// 监听指定的服务名和域名，并将发现的服务条目发送到 entryChan 中。
		defer s.resolverWG.Done()
		// 这里指定服务名称，域名，和一个条目接收通道
		if err := zeroconf.Browse(ctx, s.serviceName, mdnsDomain, entryChan); err != nil {
			log.Debugf("zeroconf browsing failed: %s", err)
		}
	}()
}

func randomString(l int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	s := make([]byte, 0, l)
	for i := 0; i < l; i++ {
		s = append(s, alphabet[rand.Intn(len(alphabet))])
	}
	return string(s)
}

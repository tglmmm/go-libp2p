package autonat

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/host/autonat/pb"

	"github.com/libp2p/go-msgio/pbio"

	ma "github.com/multiformats/go-multiaddr"
)

var streamTimeout = 60 * time.Second

const (
	ServiceName = "libp2p.autonat"

	maxMsgSize = 4096
)

// AutoNATService provides NAT autodetection services to other peers
type autoNATService struct {
	instanceLock      sync.Mutex
	instance          context.CancelFunc
	backgroundRunning chan struct{} // closed when background exits

	config *config

	// rate limiter
	mx         sync.Mutex      // 用于保护请求计数器的并发访问
	reqs       map[peer.ID]int // 一个映射表，用于记录每个对等节点的请求计数
	globalReqs int             // 全局请求计数器
}

// NewAutoNATService creates a new AutoNATService instance attached to a host
// 用于方便地创建一个与主机关联的 AutoNAT 服务实例，并提供必要的配置信息
func newAutoNATService(c *config) (*autoNATService, error) {
	if c.dialer == nil {
		return nil, errors.New("cannot create NAT service without a network")
	}
	return &autoNATService{
		config: c,
		reqs:   make(map[peer.ID]int),
	}, nil
}

// 作为autoNAT服务端，处理传入的网络流
func (as *autoNATService) handleStream(s network.Stream) {
	// 它将流附加到 AutoNAT 服务的作用域（scope）上，以便进行服务标识
	if err := s.Scope().SetService(ServiceName); err != nil {
		log.Debugf("error attaching stream to autonat service: %s", err)
		s.Reset()
		return
	}
	// 它为流分配足够的内存空间，以便进行消息读写。
	if err := s.Scope().ReserveMemory(maxMsgSize, network.ReservationPriorityAlways); err != nil {
		log.Debugf("error reserving memory for autonat stream: %s", err)
		s.Reset()
		return
	}
	// 函数退出时候释放资源
	defer s.Scope().ReleaseMemory(maxMsgSize)
	// 流的截止时间，以确保在超时后关闭流。
	s.SetDeadline(time.Now().Add(streamTimeout))
	defer s.Close()

	pid := s.Conn().RemotePeer()
	// 获取远程对等节点的标识，并记录日志。
	log.Debugf("New stream from %s", pid.Pretty())

	r := pbio.NewDelimitedReader(s, maxMsgSize)
	w := pbio.NewDelimitedWriter(s)

	var req pb.Message
	var res pb.Message

	// 从流中读取数据
	err := r.ReadMsg(&req)
	if err != nil {
		log.Debugf("Error reading message from %s: %s", pid.Pretty(), err.Error())
		s.Reset()
		return
	}
	// 检查读取的消息类型，如果不是 DIAL 类型，则记录错误日志并关闭流。
	t := req.GetType()
	if t != pb.Message_DIAL {
		log.Debugf("Unexpected message from %s: %s (%d)", pid.Pretty(), t.String(), t)
		s.Reset()
		return
	}
	// 调用 handleDial 方法处理 DIAL 请求，并将结果存储在 dr 变量中（dialResponse）
	dr := as.handleDial(pid, s.Conn().RemoteMultiaddr(), req.GetDial().GetPeer())
	res.Type = pb.Message_DIAL_RESPONSE.Enum()
	res.DialResponse = dr
	// 响应消息写入到流
	err = w.WriteMsg(&res)
	if err != nil {
		log.Debugf("Error writing response to %s: %s", pid.Pretty(), err.Error())
		s.Reset()
		return
	}
	// 记录传出的 DIAL_RESPONSE 的状态
	if as.config.metricsTracer != nil {
		as.config.metricsTracer.OutgoingDialResponse(res.GetDialResponse().GetStatus())
	}
}

// 处理拨号请求
func (as *autoNATService) handleDial(p peer.ID, obsaddr ma.Multiaddr, mpi *pb.Message_PeerInfo) *pb.Message_DialResponse {
	// mpi 请求消息中包含了客户端 peerid ,addresses
	if mpi == nil {
		return newDialResponseError(pb.Message_E_BAD_REQUEST, "missing peer info")
	}
	//
	mpid := mpi.GetId()
	if mpid != nil {
		// 从bytes获取peer ID
		mp, err := peer.IDFromBytes(mpid)
		if err != nil {
			return newDialResponseError(pb.Message_E_BAD_REQUEST, "bad peer id")
		}
		// 检查消息中的ID和连接中获取到的peer ID是否一致
		if mp != p {
			return newDialResponseError(pb.Message_E_BAD_REQUEST, "peer id mismatch")
		}
	}
	//
	addrs := make([]ma.Multiaddr, 0, as.config.maxPeerAddresses)
	seen := make(map[string]struct{})

	// Don't even try to dial peers with blocked remote addresses. In order to dial a peer, we
	// need to know their public IP address, and it needs to be different from our public IP
	// address.
	// 检查配置中的拨号策略，如果观察到的地址 obsaddr 被跳过拨号，则返回一个表示拒绝拨号的错误响应。
	if as.config.dialPolicy.skipDial(obsaddr) {
		if as.config.metricsTracer != nil {
			as.config.metricsTracer.OutgoingDialRefused(dial_blocked)
		}
		// Note: versions < v0.20.0 return Message_E_DIAL_ERROR here, thus we can not rely on this error code.
		return newDialResponseError(pb.Message_E_DIAL_REFUSED, "refusing to dial peer with blocked observed address")
	}

	// Determine the peer's IP address.
	// 提取观察到的地址 obsaddr 中的主机 IP 地址。
	hostIP, _ := ma.SplitFirst(obsaddr)
	switch hostIP.Protocol().Code {
	case ma.P_IP4, ma.P_IP6:
	default:
		// This shouldn't be possible as we should skip all addresses that don't include
		// public IP addresses.
		return newDialResponseError(pb.Message_E_INTERNAL_ERROR, "expected an IP address")
	}

	// add observed addr to the list of addresses to dial
	// 将观察到的地址添加到 addrs中
	addrs = append(addrs, obsaddr)
	// 并且添加到seen映射
	seen[obsaddr.String()] = struct{}{}
	// 从客户端请求的信息中获取到地址客户端汇报的地址切片，并且遍历
	for _, maddr := range mpi.GetAddrs() {
		// 从bytes获取一个多地址（将地址解析为 ma.Multiaddr 类型。）
		addr, err := ma.NewMultiaddrBytes(maddr)
		if err != nil {
			log.Debugf("Error parsing multiaddr: %s", err.Error())
			continue
		}

		// For security reasons, we _only_ dial the observed IP address.
		// Replace other IP addresses with the observed one so we can still try the
		// requested ports/transports.
		// 检查地址中的主机 IP 是否与观察到的主机 IP 相同，如果不同，则将主机 IP 替换为观察到的主机 IP
		if ip, rest := ma.SplitFirst(addr); !ip.Equal(hostIP) {
			// Make sure it's an IP address
			switch ip.Protocol().Code {
			case ma.P_IP4, ma.P_IP6:
			default:
				continue
			}
			addr = hostIP
			if rest != nil {
				//  /ip4/1.2.3.4 encapsulate /tcp/80 = /ip4/1.2.3.4/tcp/80
				addr = addr.Encapsulate(rest)
			}
		}

		// Make sure we're willing to dial the rest of the address (e.g., not a circuit
		// address).
		// 检查是否应该跳过拨号该地址，如果是，则继续下一个地址。
		if as.config.dialPolicy.skipDial(addr) {
			continue
		}

		// 将地址转换为字符串，并检查地址是否已经在地址映射 seen 中，如果是，则继续下一个地址。
		str := addr.String()
		_, ok := seen[str]
		if ok {
			continue
		}
		// 继续添加到addrs 和seen中
		addrs = append(addrs, addr)
		seen[str] = struct{}{}
		// 如果地址大于最大限制，则退出循环
		if len(addrs) >= as.config.maxPeerAddresses {
			break
		}
	}
	// 返回一个表示没有可拨号地址的错误响应
	if len(addrs) == 0 {
		if as.config.metricsTracer != nil {
			as.config.metricsTracer.OutgoingDialRefused(no_valid_address)
		}
		// Note: versions < v0.20.0 return Message_E_DIAL_ERROR here, thus we can not rely on this error code.
		return newDialResponseError(pb.Message_E_DIAL_REFUSED, "no dialable addresses")
	}
	// 向对等节点拨号，并将拨号结果作为响应返回。
	return as.doDial(peer.AddrInfo{ID: p, Addrs: addrs})
}

// 执行拨号操作
func (as *autoNATService) doDial(pi peer.AddrInfo) *pb.Message_DialResponse {
	// rate limit check
	// 检查拨号数量是否超过了每个节点的最大拨号数，或者全局拨号数
	as.mx.Lock()
	count := as.reqs[pi.ID]
	// 如果当前节点拨号数量大于配置的最大阈值，或者当前全局最大请求数大于配置中全局请求
	if count >= as.config.throttlePeerMax || (as.config.throttleGlobalMax > 0 &&
		as.globalReqs >= as.config.throttleGlobalMax) {
		as.mx.Unlock()
		if as.config.metricsTracer != nil {
			// 记录已达拨号请求上限
			as.config.metricsTracer.OutgoingDialRefused(rate_limited)
		}
		// 返回错误
		return newDialResponseError(pb.Message_E_DIAL_REFUSED, "too many dials")
	}
	// as.reqs 是对应节点和对应请求的数量
	as.reqs[pi.ID] = count + 1
	// 全局计数 +1
	as.globalReqs++
	as.mx.Unlock()
	// 根据配置的拨号超时时间，创建一个ctx
	ctx, cancel := context.WithTimeout(context.Background(), as.config.dialTimeout)
	defer cancel()

	// 先删除，在重新添加，为了确保使用最新的地址信息进行拨号
	as.config.dialer.Peerstore().ClearAddrs(pi.ID)
	as.config.dialer.Peerstore().AddAddrs(pi.ID, pi.Addrs, peerstore.TempAddrTTL)

	defer func() {
		as.config.dialer.Peerstore().ClearAddrs(pi.ID)
		as.config.dialer.Peerstore().RemovePeer(pi.ID)
	}()

	// 执行拨号
	conn, err := as.config.dialer.DialPeer(ctx, pi.ID)
	if err != nil {
		log.Debugf("error dialing %s: %s", pi.ID.Pretty(), err.Error())
		// wait for the context to timeout to avoid leaking timing information
		// this renders the service ineffective as a port scanner

		// 拨号失败后，等待上下文超时结束，而不是立即返回错误
		// 这样不管是什么错误，都会以相同的超时时间返回，避免被作为探测端口的工具利用
		<-ctx.Done()
		return newDialResponseError(pb.Message_E_DIAL_ERROR, "dial failed")
	}

	ra := conn.RemoteMultiaddr()
	// 关闭连接
	// 调用底层的拨号器（dialer）关闭与指定对等节点（peer）的连接。这个操作会关闭与对等节点的连接并清除与该对等节点相关的所有状态信息。
	as.config.dialer.ClosePeer(pi.ID)
	return newDialResponseOK(ra)
}

// Enable the autoNAT service if it is not running.
// 如果 AutoNAT 服务尚未运行，它会启动服务并开始后台处理。
func (as *autoNATService) Enable() {
	as.instanceLock.Lock()
	defer as.instanceLock.Unlock()
	if as.instance != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	as.instance = cancel
	as.backgroundRunning = make(chan struct{})
	as.config.host.SetStreamHandler(AutoNATProto, as.handleStream)

	go as.background(ctx)
}

// Disable the autoNAT service if it is running.
func (as *autoNATService) Disable() {
	as.instanceLock.Lock()
	defer as.instanceLock.Unlock()
	if as.instance != nil {
		as.config.host.RemoveStreamHandler(AutoNATProto)
		as.instance()
		as.instance = nil
		<-as.backgroundRunning // 等待资源释放后关闭通道，在这里相当于一个关闭服务的信号接收器
	}
}

// 主要作用是定期重置请求计数器和全局请求数
func (as *autoNATService) background(ctx context.Context) {
	// 函数退出时候关闭通道
	defer close(as.backgroundRunning)
	// 用于定期重置请求计数器和全局请求数的值
	timer := time.NewTimer(as.config.throttleResetPeriod)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			as.mx.Lock()
			as.reqs = make(map[peer.ID]int)
			as.globalReqs = 0
			as.mx.Unlock()
			// 重置请求计数的随机因子
			jitter := rand.Float32() * float32(as.config.throttleResetJitter)
			timer.Reset(as.config.throttleResetPeriod + time.Duration(int64(jitter)))
		case <-ctx.Done(): // 表示上下文被取消，退出循环并返回。
			return
		}
	}
}

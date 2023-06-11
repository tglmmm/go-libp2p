package autonat

import (
	"context"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"

	logging "github.com/ipfs/go-log/v2"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

var log = logging.Logger("autonat")

// 是用于衡量对NAT状态的信心程度的指标
// 小于3，系统可能会尝试多次自动NAT探测以提高置信度
const maxConfidence = 3

// AmbientAutoNAT is the implementation of ambient NAT autodiscovery
type AmbientAutoNAT struct {
	host host.Host

	*config

	ctx               context.Context
	ctxCancel         context.CancelFunc // is closed when Close is called
	backgroundRunning chan struct{}      // is closed when the background go routine exits 一个通道，当后台goroutine退出时关闭。
	inboundConn       chan network.Conn  // 一个通道用于传输，传入的连接
	dialResponses     chan error         // 一个通道用于传输拨号的相应错误
	// status is an autoNATResult reflecting current status.
	status atomic.Pointer[network.Reachability] // 一个原子指针表示当前状态的结果
	// Reflects the confidence on of the NATStatus being private, as a single
	// dialback may fail for reasons unrelated to NAT.
	// If it is <3, then multiple autoNAT peers may be contacted for dialback
	// If only a single autoNAT peer is known, then the confidence increases
	// for each failure until it reaches 3.
	confidence   int
	lastInbound  time.Time             // 上次接收传入连接的时间。
	lastProbeTry time.Time             // 上次重试的探测时间
	lastProbe    time.Time             // 上一次探测时间
	recentProbes map[peer.ID]time.Time // 最近探测的peer ID的时间和映射
	service      *autoNATService       // autoNATService实例

	emitReachabilityChanged event.Emitter      // 用于发出可达性更改事件的事件发射器
	subscriber              event.Subscription // 事件订阅对象
}

// StaticAutoNAT is a simple AutoNAT implementation when a single NAT status is desired.
// 是一个简单的AutoNAT（自动NAT）实现，用于在只需要一个NAT状态时使用。
type StaticAutoNAT struct {
	host         host.Host
	reachability network.Reachability
	service      *autoNATService
}

// New creates a new NAT autodiscovery system attached to a host
func New(h host.Host, options ...Option) (AutoNAT, error) {
	var err error
	conf := new(config) // 创建一个config
	conf.host = h
	conf.dialPolicy.host = h
	// 设置默认参数
	if err = defaults(conf); err != nil {
		return nil, err
	}
	// ru过本地节点的科大地址是nil,则使用host监听的地址
	if conf.addressFunc == nil {
		conf.addressFunc = h.Addrs
	}
	// 遍历传入的可选项
	for _, o := range options {
		if err = o(conf); err != nil {
			return nil, err
		}
	}
	// 创建一个事件发射器，用于发出本地可达性改变的事件
	emitReachabilityChanged, _ := h.EventBus().Emitter(new(event.EvtLocalReachabilityChanged), eventbus.Stateful)

	var service *autoNATService
	// 没有强制使用特定的可达性，则创建一个AmbientAutoNAT实例。
	if (!conf.forceReachability || conf.reachability == network.ReachabilityPublic) && conf.dialer != nil {
		service, err = newAutoNATService(conf)
		if err != nil {
			return nil, err
		}
		service.Enable()
	}
	// 如果配置强制设置可达性状态
	if conf.forceReachability {
		// 发送一个可达性改变的事件
		emitReachabilityChanged.Emit(event.EvtLocalReachabilityChanged{Reachability: conf.reachability})
		// 并创建一个StaticAutoNAT实例返回
		return &StaticAutoNAT{
			host:         h,
			reachability: conf.reachability,
			service:      service,
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	as := &AmbientAutoNAT{
		ctx:               ctx,
		ctxCancel:         cancel,
		backgroundRunning: make(chan struct{}),
		host:              h,
		config:            conf,
		inboundConn:       make(chan network.Conn, 5),
		dialResponses:     make(chan error, 1),

		emitReachabilityChanged: emitReachabilityChanged,
		service:                 service,
		recentProbes:            make(map[peer.ID]time.Time),
	}
	reachability := network.ReachabilityUnknown
	as.status.Store(&reachability)
	// 订阅与地址更新和节点识别完成事件相关的事件
	subscriber, err := as.host.EventBus().Subscribe(
		[]any{new(event.EvtLocalAddressesUpdated), new(event.EvtPeerIdentificationCompleted)},
		eventbus.Name("autonat"),
	)
	if err != nil {
		return nil, err
	}
	as.subscriber = subscriber
	// 例如新的传入连接建立时，主机会将连接的信息发送到 as.inboundConn 通道中，as 可以从该通道中读取连接并进行处理
	h.Network().Notify(as)
	// 启动一个后台goroutine，用于处理自动NAT发现的逻辑。
	go as.background()

	return as, nil
}

// Status returns the AutoNAT observed reachability status.
func (as *AmbientAutoNAT) Status() network.Reachability {
	s := as.status.Load()
	return *s
}

func (as *AmbientAutoNAT) emitStatus() {
	status := *as.status.Load()
	as.emitReachabilityChanged.Emit(event.EvtLocalReachabilityChanged{Reachability: status})
	if as.metricsTracer != nil {
		as.metricsTracer.ReachabilityStatus(status)
	}
}

func ipInList(candidate ma.Multiaddr, list []ma.Multiaddr) bool {
	candidateIP, _ := manet.ToIP(candidate)
	for _, i := range list {
		if ip, err := manet.ToIP(i); err == nil && ip.Equal(candidateIP) {
			return true
		}
	}
	return false
}

func (as *AmbientAutoNAT) background() {
	defer close(as.backgroundRunning)
	// wait a bit for the node to come online and establish some connections
	// before starting autodetection
	delay := as.config.bootDelay
	// 监听主机事件总线的事件
	// 根据不同类型的事件执行相应的操作，例如在本地地址更新时减少可信度 (confidence)，在对等体标识完成时进行探测。
	subChan := as.subscriber.Out()
	defer as.subscriber.Close()
	defer as.emitReachabilityChanged.Close()
	// 计时器
	timer := time.NewTimer(delay)
	defer timer.Stop()
	timerRunning := true
	retryProbe := false
	for {
		select {
		// new inbound connection.
		//	当有新的传入连接建立时，从该通道中接收连接，并根据连接的属性更新 as 的状态。
		case conn := <-as.inboundConn:
			localAddrs := as.host.Addrs()
			// 远程地址是公网地址
			if manet.IsPublicAddr(conn.RemoteMultiaddr()) &&
				// 并且IP不在本地地址当中
				!ipInList(conn.RemoteMultiaddr(), localAddrs) {
				// 更新最后入站时间
				as.lastInbound = time.Now()
			}

		case e := <-subChan:
			// 根据事件类型执行相应的操作
			switch e := e.(type) {
			// 本地地址更新
			case event.EvtLocalAddressesUpdated:
				// On local address update, reduce confidence from maximum so that we schedule
				// the next probe sooner
				// 将可信度降低， 以便更早地安排下一次探测
				if as.confidence == maxConfidence {
					as.confidence--
				}
			// 对等体标识完成
			case event.EvtPeerIdentificationCompleted:
				// 如果对等体支持 autoNAT,
				if s, err := as.host.Peerstore().SupportsProtocols(e.Peer, AutoNATProto); err == nil && len(s) > 0 {
					currentStatus := *as.status.Load()
					// 如果当前状态是未知，则开始探测
					if currentStatus == network.ReachabilityUnknown {
						as.tryProbe(e.Peer)
					}
				}
			default:
				log.Errorf("unknown event type: %T", e)
			}

		// probe finished.
		// 监听拨号响应的结果
		//
		case err, ok := <-as.dialResponses:
			if !ok {
				return
			}
			// 如果拨号被拒绝则标识，需要重新探测
			if IsDialRefused(err) {
				retryProbe = true
			} else {
				//	否则处理响应
				as.handleDialResponse(err)
			}
		case <-timer.C:
			// 根据当前状态选择要进行探测的对等体
			peer := as.getPeerToProbe()
			as.tryProbe(peer)
			timerRunning = false
			retryProbe = false
		case <-as.ctx.Done():
			return
		}

		// Drain the timer channel if it hasn't fired in preparation for Resetting it.
		if timerRunning && !timer.Stop() {
			<-timer.C
		}
		// scheduleProbe 函数用于计算下一次探测（probe）应该安排的时间。
		timer.Reset(as.scheduleProbe(retryProbe))
		timerRunning = true
	}
}

func (as *AmbientAutoNAT) cleanupRecentProbes() {
	fixedNow := time.Now()
	for k, v := range as.recentProbes {
		if fixedNow.Sub(v) > as.throttlePeerPeriod {
			delete(as.recentProbes, k)
		}
	}
}

// scheduleProbe calculates when the next probe should be scheduled for.
// 用于计算下一次探测（probe）应该安排的时间
func (as *AmbientAutoNAT) scheduleProbe(retryProbe bool) time.Duration {
	// Our baseline is a probe every 'AutoNATRefreshInterval'
	// This is modulated by:
	// * if we are in an unknown state, have low confidence, or we want to retry because a probe was refused that
	//   should drop to 'AutoNATRetryInterval'
	// * recent inbound connections (implying continued connectivity) should decrease the retry when public
	// * recent inbound connections when not public mean we should try more actively to see if we're public.
	fixedNow := time.Now()
	currentStatus := *as.status.Load()

	nextProbe := fixedNow
	// Don't look for peers in the peer store more than once per second.
	// 如果之前的探测尝试时间不为空（!as.lastProbeTry.IsZero()），则需要在上次尝试时间的基础上增加一秒的时间间隔，以避免频繁地查询节点存储库中的对等节点
	if !as.lastProbeTry.IsZero() {
		// 上次探测基础上+ 1s
		backoff := as.lastProbeTry.Add(time.Second)
		if backoff.After(nextProbe) {
			nextProbe = backoff
		}
	}
	if !as.lastProbe.IsZero() {
		untilNext := as.config.refreshInterval
		if retryProbe {
			untilNext = as.config.retryInterval
		} else if currentStatus == network.ReachabilityUnknown {
			untilNext = as.config.retryInterval
		} else if as.confidence < maxConfidence {
			untilNext = as.config.retryInterval
		} else if currentStatus == network.ReachabilityPublic && as.lastInbound.After(as.lastProbe) {
			untilNext *= 2
		} else if currentStatus != network.ReachabilityPublic && as.lastInbound.After(as.lastProbe) {
			untilNext /= 5
		}

		if as.lastProbe.Add(untilNext).After(nextProbe) {
			nextProbe = as.lastProbe.Add(untilNext)
		}
	}
	if as.metricsTracer != nil {
		as.metricsTracer.NextProbeTime(nextProbe)
	}
	return nextProbe.Sub(fixedNow)
}

// handleDialResponse updates the current status based on dial response.
// 根据拨号响应更新当前的 NAT 状态。
func (as *AmbientAutoNAT) handleDialResponse(dialErr error) {
	var observation network.Reachability
	switch {
	case dialErr == nil:
		// 可达，说明是公网IP
		observation = network.ReachabilityPublic
	//	私有IP
	case IsDialError(dialErr):
		observation = network.ReachabilityPrivate
	default:
		observation = network.ReachabilityUnknown
	}

	as.recordObservation(observation)
}

// recordObservation updates NAT status and confidence
// 用于更新 NAT 状态和可信度。
func (as *AmbientAutoNAT) recordObservation(observation network.Reachability) {
	// 载入当前状态
	currentStatus := *as.status.Load()
	// 如果探测到的当前是一个公网地址
	if observation == network.ReachabilityPublic {
		changed := false
		// 如果当前节点不是公网，则表示发生了变化
		if currentStatus != network.ReachabilityPublic {
			// Aggressively switch to public from other states ignoring confidence
			log.Debugf("NAT status is public")

			// we are flipping our NATStatus, so confidence drops to 0
			as.confidence = 0
			if as.service != nil {
				as.service.Enable()
			}
			changed = true
			// 如果当前
		} else if as.confidence < maxConfidence {
			// 没有变化，confidence可以递增
			as.confidence++
		}
		// 存储当前状态
		as.status.Store(&observation)
		if changed {
			// 发送状态变更事件
			as.emitStatus()
		}
		//	如果探测到当前节点是一个私有地址
	} else if observation == network.ReachabilityPrivate {
		// 载入的当前节点不是一个私有地址,说明地址发生了变化
		if currentStatus != network.ReachabilityPrivate {
			// confidence递减
			if as.confidence > 0 {
				as.confidence--
			} else {
				log.Debugf("NAT status is private")

				// we are flipping our NATStatus, so confidence drops to 0
				as.confidence = 0
				// 存储当前状态
				as.status.Store(&observation)
				// 当节点切换到私网状态时，它已经确定了自己的NAT状态为私网，不再需要发送探测请求
				// 因为私网状态下节点已经确认了自己的NAT状态，不需要继续进行探测。
				if as.service != nil {
					as.service.Disable()
				}
				as.emitStatus()
			}
			//	表示从私有网络转变成了公网
		} else if as.confidence < maxConfidence {
			as.confidence++
			as.status.Store(&observation)
		}
	} else if as.confidence > 0 {
		// don't just flip to unknown, reduce confidence first
		as.confidence--
	} else {
		log.Debugf("NAT status is unknown")
		as.status.Store(&observation)
		if currentStatus != network.ReachabilityUnknown {
			if as.service != nil {
				as.service.Enable()
			}
			as.emitStatus()
		}
	}
	if as.metricsTracer != nil {
		as.metricsTracer.ReachabilityStatusConfidence(as.confidence)
	}
}

func (as *AmbientAutoNAT) tryProbe(p peer.ID) bool {
	// 设置当前时间为最后一次探测时间
	as.lastProbeTry = time.Now()
	// 验证对等体 p 的有效性
	if p.Validate() != nil {
		return false
	}
	// 检查最近时间内是否对节点进行探测
	if lastTime, ok := as.recentProbes[p]; ok {
		// 判断是否在探测间隔内
		if time.Since(lastTime) < as.throttlePeerPeriod {
			return false
		}
	}
	// 将已经超过时间间隔的节点从recentProbes中删除
	as.cleanupRecentProbes()
	// 从peerstore 中获取指定Peer信息
	info := as.host.Peerstore().PeerInfo(p)
	// 判断配置中的策略是否要跳过对该地址的探测
	if !as.config.dialPolicy.skipPeer(info.Addrs) {
		// 如果不跳过，则记录该节点最近一次的探测时间
		as.recentProbes[p] = time.Now()
		as.lastProbe = time.Now()
		go as.probe(&info)
		return true
	}
	return false
}

// 用于向指定的对等体发送探测请求。
func (as *AmbientAutoNAT) probe(pi *peer.AddrInfo) {
	// 创建一个 AutoNATClient 客户端，该客户端用于与对等体进行自动 NAT 探测
	cli := NewAutoNATClient(as.host, as.config.addressFunc, as.metricsTracer)
	ctx, cancel := context.WithTimeout(as.ctx, as.config.requestTimeout)
	defer cancel()
	// 发起与指定对等体 pi.ID 的 DialBack 连接，即发送探测请求。
	err := cli.DialBack(ctx, pi.ID)
	log.Debugf("Dialback through peer %s completed: err: %s", pi.ID, err)

	select {
	// 将错误信息通过通道 as.dialResponses 发送出去，以便其他地方可以处理该结果。
	case as.dialResponses <- err:
	case <-as.ctx.Done():
		return
	}
}

func (as *AmbientAutoNAT) getPeerToProbe() peer.ID {
	// 获取当前主机的所有对等体列表
	peers := as.host.Network().Peers()
	if len(peers) == 0 {
		return ""
	}

	candidates := make([]peer.ID, 0, len(peers))

	for _, p := range peers {
		info := as.host.Peerstore().PeerInfo(p)
		// Exclude peers which don't support the autonat protocol.
		// 判断对等体是否支持autoNAT 协议
		if proto, err := as.host.Peerstore().SupportsProtocols(p, AutoNATProto); len(proto) == 0 || err != nil {
			continue
		}

		// Exclude peers in backoff.
		// recentProbes 记录了最近一次然测时间
		if lastTime, ok := as.recentProbes[p]; ok {
			// 如果对等节点在探测期内
			if time.Since(lastTime) < as.throttlePeerPeriod {
				continue
			}
		}
		// 判断是否应跳过该对等体 ， 这可能基于某些策略
		if as.config.dialPolicy.skipPeer(info.Addrs) {
			continue
		}
		candidates = append(candidates, p)
	}

	if len(candidates) == 0 {
		return ""
	}
	// 从 candidates 列表中随机选择一个对等体作为探测目标
	return candidates[rand.Intn(len(candidates))]
}

func (as *AmbientAutoNAT) Close() error {
	as.ctxCancel()
	if as.service != nil {
		as.service.Disable()
	}
	<-as.backgroundRunning
	return nil
}

// Status returns the AutoNAT observed reachability status.
func (s *StaticAutoNAT) Status() network.Reachability {
	return s.reachability
}

func (s *StaticAutoNAT) Close() error {
	if s.service != nil {
		s.service.Disable()
	}
	return nil
}

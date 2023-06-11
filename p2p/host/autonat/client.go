package autonat

import (
	"context"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autonat/pb"

	"github.com/libp2p/go-msgio/pbio"
)

// NewAutoNATClient creates a fresh instance of an AutoNATClient
// If addrFunc is nil, h.Addrs will be used
// 当前节点的主机对象。
// mt 用于跟踪度量指标
func NewAutoNATClient(h host.Host, addrFunc AddrFunc, mt MetricsTracer) Client {
	if addrFunc == nil {
		// 如果 addrFunc 参数为 nil，则会使用 h.Addrs 函数来获取地址信息
		addrFunc = h.Addrs
	}
	return &client{h: h, addrFunc: addrFunc, mt: mt}
}

type client struct {
	h        host.Host
	addrFunc AddrFunc
	mt       MetricsTracer
}

// DialBack asks peer p to dial us back on all addresses returned by the addrFunc.
// It blocks until we've received a response from the peer.
//
// Note: A returned error Message_E_DIAL_ERROR does not imply that the server
// actually performed a dial attempt. Servers that run a version < v0.20.0 also
// return Message_E_DIAL_ERROR if the dial was skipped due to the dialPolicy.
// 需要注意的是，返回的错误 Message_E_DIAL_ERROR 并不意味着服务器实际执行了回拨尝试。
//运行版本低于 v0.20.0 的服务器也会在由于 dialPolicy 跳过了回拨时返回 Message_E_DIAL_ERROR。
//换句话说，即使返回错误，也不能确定服务器是否尝试进行回拨。

// 用于请求对等节点通过 addrFunc 返回的所有地址拨打回我们
// 他会阻塞直到我们收到对等节点的响应
func (c *client) DialBack(ctx context.Context, p peer.ID) error {
	s, err := c.h.NewStream(ctx, p, AutoNATProto)
	if err != nil {
		return err
	}
	// 设置流服务名称
	if err := s.Scope().SetService(ServiceName); err != nil {
		log.Debugf("error attaching stream to autonat service: %s", err)
		s.Reset()
		return err
	}
	// 预留流的内存空间。
	if err := s.Scope().ReserveMemory(maxMsgSize, network.ReservationPriorityAlways); err != nil {
		log.Debugf("error reserving memory for autonat stream: %s", err)
		s.Reset()
		return err
	}
	// 释放内存空间
	defer s.Scope().ReleaseMemory(maxMsgSize)
	// 流的截止时间是指在该时间之前，流的操作（如读取、写入）必须完成
	// 如果在截止时间之前无法完成操作，流将被关闭或取消
	s.SetDeadline(time.Now().Add(streamTimeout))
	// Might as well just reset the stream. Once we get to this point, we
	// don't care about being nice.
	defer s.Close()
	// 用于读取和写入消息
	r := pbio.NewDelimitedReader(s, maxMsgSize)
	w := pbio.NewDelimitedWriter(s)
	// 创建一个DialMessage消息
	req := newDialMessage(peer.AddrInfo{ID: c.h.ID(), Addrs: c.addrFunc()})
	// 并通过 w.WriteMsg 将其写入流中
	if err := w.WriteMsg(req); err != nil {
		s.Reset()
		return err
	}

	var res pb.Message
	// 从流中读取响应消息，并将其存储在 res 变量中。
	if err := r.ReadMsg(&res); err != nil {
		s.Reset()
		return err
	}
	// 判断响应的消息类型
	if res.GetType() != pb.Message_DIAL_RESPONSE {
		s.Reset()
		return fmt.Errorf("unexpected response: %s", res.GetType().String())
	}
	// 如果状态为 pb.Message_OK，表示拨打回成功
	status := res.GetDialResponse().GetStatus()
	if c.mt != nil {
		c.mt.ReceivedDialResponse(status)
	}
	switch status {
	case pb.Message_OK:
		return nil
	default:
		return Error{Status: status, Text: res.GetDialResponse().GetStatusText()}
	}
}

// Error wraps errors signalled by AutoNAT services
type Error struct {
	Status pb.Message_ResponseStatus
	Text   string
}

func (e Error) Error() string {
	return fmt.Sprintf("AutoNAT error: %s (%s)", e.Text, e.Status.String())
}

// IsDialError returns true if the error was due to a dial back failure
func (e Error) IsDialError() bool {
	return e.Status == pb.Message_E_DIAL_ERROR
}

// IsDialRefused returns true if the error was due to a refusal to dial back
func (e Error) IsDialRefused() bool {
	return e.Status == pb.Message_E_DIAL_REFUSED
}

// IsDialError returns true if the AutoNAT peer signalled an error dialing back
func IsDialError(e error) bool {
	ae, ok := e.(Error)
	return ok && ae.IsDialError()
}

// IsDialRefused returns true if the AutoNAT peer signalled refusal to dial back
func IsDialRefused(e error) bool {
	ae, ok := e.(Error)
	return ok && ae.IsDialRefused()
}

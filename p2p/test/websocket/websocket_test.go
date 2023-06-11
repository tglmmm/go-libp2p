package websocket_test

import (
	"context"
	"io"
	"testing"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func TestReadLimit(t *testing.T) {
	h1, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0/ws"))
	require.NoError(t, err)
	defer h1.Close()

	ctx := context.Background()
	h2, err := libp2p.New(libp2p.NoListenAddrs)
	require.NoError(t, err)
	defer h2.Close()

	err = h2.Connect(ctx, peer.AddrInfo{ID: h1.ID(), Addrs: h1.Addrs()})
	require.NoError(t, err)

	buf := make([]byte, 256<<10)
	buf2 := make([]byte, 256<<10)
	copyBuf := make([]byte, 8<<10)

	errCh := make(chan error, 1)
	// TODO perf would be perfect here, but not yet merged.
	h1.SetStreamHandler("/big-blocks", func(s network.Stream) {
		defer s.Close()
		_, err := io.CopyBuffer(io.Discard, s, copyBuf)
		if err != nil {
			errCh <- err
			return
		}
		_, err = s.Write(buf)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	})

	allocs := testing.AllocsPerRun(100, func() {
		s, err := h2.NewStream(ctx, h1.ID(), "/big-blocks")
		require.NoError(t, err)
		defer s.Close()
		_, err = s.Write(buf2)
		require.NoError(t, err)
		require.NoError(t, s.CloseWrite())

		_, err = io.ReadFull(s, buf2)
		require.NoError(t, err)

		_, err = s.Read([]byte{0})
		require.ErrorIs(t, err, io.EOF)
		require.NoError(t, <-errCh)
	})

	// Make sure we aren't doing some crazy allocs when transferring big blocks
	require.Less(t, allocs, 8*1024.0)
}

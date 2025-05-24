package main

import (
	"log"
	"math/rand/v2"
	"net"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/jitterbuffer"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	port     = 9000
	ssrc     = 1234
	mtu      = 1500
	lossRate = 0.2 // 丢包概率
)

func main() {
	go sender()
	receiver()
}

func receiver() {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		panic(err)
	}

	defer conn.Close()

	// NACK 生成器
	nackGenFactory, _ := nack.NewGeneratorInterceptor()
	nackGen, _ := nackGenFactory.NewInterceptor("")

	// JitterBuffer + Pop 控制
	jb := jitterbuffer.New(jitterbuffer.WithMinimumPacketCount(3))
	// jbInterceptorFactory, _ := jitterbuffer.NewInterceptor()
	// jbInterceptor, _ := jbInterceptorFactory.NewInterceptor("")

	// chain := interceptor.NewChain([]interceptor.Interceptor{jbInterceptor, nackGen})
	chain := interceptor.NewChain([]interceptor.Interceptor{nackGen})

	rtcpWriterSet := false
	remote := chain.BindRemoteStream(&interceptor.StreamInfo{
		SSRC:         ssrc,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, interceptor.RTPReaderFunc(func(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		p := &rtp.Packet{}
		_ = p.Unmarshal(b)
		jb.Push(p)

		return len(b), nil, nil
	}))

	go func() {
		// time.Sleep(30 * time.Millisecond)
		for {
			pkt, err := jb.Pop()
			if err != nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			log.Printf("✅ Pop sorted RTP Seq=%d", pkt.SequenceNumber)
		}

	}()

	buf := make([]byte, mtu)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			log.Println("read error:", err)
			continue
		}

		_, _, _ = remote.Read(buf[:n], nil)

		if !rtcpWriterSet {
			rtcpWriterSet = true
			chain.BindRTCPWriter(interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, _ interceptor.Attributes) (int, error) {
				raw, err := rtcp.Marshal(pkts)
				if err != nil {
					return 0, err
				}
				for _, pkt := range pkts {
					if _, ok := pkt.(*rtcp.TransportLayerNack); ok {
						log.Printf("📨 Send NACK: %+v", pkt)
					}
				}
				return conn.WriteTo(raw, addr)
			}))
		}
	}
}

func sender() {
	time.Sleep(1 * time.Second) // 等待接收端准备好

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	nackResponderFactory, _ := nack.NewResponderInterceptor()
	nackResponder, _ := nackResponderFactory.NewInterceptor("")
	chain := interceptor.NewChain([]interceptor.Interceptor{nackResponder})
	var seq uint16 = 0
	sentPkts := make(map[uint16][]byte)
	// RTCP Reader 处理 NACK
	rtcpReader := chain.BindRTCPReader(interceptor.RTCPReaderFunc(func(in []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		pkts, err := rtcp.Unmarshal(in)
		if err != nil {
			return 0, nil, err
		}
		for _, pkt := range pkts {
			if nackPkt, ok := pkt.(*rtcp.TransportLayerNack); ok {
				for _, pair := range nackPkt.Nacks {
					log.Printf("🔁 Got NACK for Seq=%d", pair.PacketID)

					// 根据 NACK 请求重传丢失的包
					if lostPkt, exists := sentPkts[pair.PacketID]; exists {
						_, err := conn.Write(lostPkt)
						if err != nil {
							log.Printf("failed to resend packet with Seq=%d: %v", pair.PacketID, err)
						} else {
							log.Printf("📤 Resent RTP Seq=%d", pair.PacketID)
						}
					}
				}
			}
		}
		return len(in), nil, nil
	}))

	go func() {
		rtcpBuf := make([]byte, mtu)
		for {
			n, _ := conn.Read(rtcpBuf)
			rtcpReader.Read(rtcpBuf[:n], nil)
		}
	}()

	stream := chain.BindLocalStream(&interceptor.StreamInfo{
		SSRC:         ssrc,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, interceptor.RTPWriterFunc(func(hdr *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		hb, _ := hdr.Marshal()
		return conn.Write(append(hb, payload...))
	}))

	for {
		h := &rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq}
		payload := []byte{0x01, 0x02}

		hb, err := h.Marshal()
		if err != nil {
			log.Printf("failed to marshal RTP header: %v", err)
			continue
		}
		fullPkt := append(hb, payload...)
		sentPkts[seq] = fullPkt

		if rand.Float64() > lossRate {
			_, _ = stream.Write(h, payload, nil)
			log.Printf("📤 Sent RTP Seq=%d", seq)
		} else {
			log.Printf("❌ Drop RTP Seq=%d", seq)
		}

		seq++
		time.Sleep(50 * time.Millisecond)
	}
}

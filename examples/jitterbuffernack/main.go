package main

import (
	"encoding/binary"
	"fmt"
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
	rtxSSRC  = 4321
	mtu      = 1500
	lossRate = 0.2

	originalPayloadType = 96
	rtxPayloadType      = 98
)

func main() {
	go sender()
	receiver()
}

func unwrapRTX(pkt *rtp.Packet) (*rtp.Packet, error) {
	if len(pkt.Payload) < 2 {
		return nil, fmt.Errorf("invalid RTX payload")
	}
	origSeq := binary.BigEndian.Uint16(pkt.Payload[:2])
	pkt.Payload = pkt.Payload[2:]
	pkt.PayloadType = originalPayloadType
	pkt.SSRC = ssrc
	pkt.SequenceNumber = origSeq
	return pkt, nil
}

func receiver() {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	nackGenFactory, _ := nack.NewGeneratorInterceptor()
	nackGen, _ := nackGenFactory.NewInterceptor("")

	jbInterceptorFactory, _ := jitterbuffer.NewInterceptor()
	jbInterceptor, _ := jbInterceptorFactory.NewInterceptor("")

	chain := interceptor.NewChain([]interceptor.Interceptor{nackGen, jbInterceptor})
	rtcpWriterSet := false

	remote := chain.BindRemoteStream(&interceptor.StreamInfo{
		SSRC:         ssrc,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, interceptor.RTPReaderFunc(func(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		p := &rtp.Packet{}
		if err := p.Unmarshal(b); err != nil {
			log.Printf("❌ Failed to unmarshal RTP: %v", err)
			return 0, nil, err
		}

		if p.PayloadType == rtxPayloadType {
			log.Printf("📥 Received RTX packet Seq=%d", p.SequenceNumber)
			p, err = unwrapRTX(p)
			if err != nil {
				log.Printf("❌ unwrap RTX failed: %v", err)
				return 0, nil, err
			}
			log.Printf("✅ Unwrapped to original RTP Seq=%d", p.SequenceNumber)
		} else {
			log.Printf("📨 Received RTP Seq=%d", p.SequenceNumber)
		}
		return len(b), nil, nil
	}))

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
					if nackPkt, ok := pkt.(*rtcp.TransportLayerNack); ok {
						log.Printf("📨 Send NACK: %+v", nackPkt)
					}
				}
				return conn.WriteTo(raw, addr)
			}))
		}
	}
}

func buildRTXPacket(orig []byte, origSeq uint16, origHeader *rtp.Header) ([]byte, error) {
	headerBytes, err := origHeader.Marshal()
	if err != nil {
		return nil, err
	}
	rtxHeader := &rtp.Header{
		Version:        2,
		PayloadType:    rtxPayloadType,
		SequenceNumber: origHeader.SequenceNumber,
		SSRC:           rtxSSRC,
	}
	rtxHeaderBytes, err := rtxHeader.Marshal()
	if err != nil {
		return nil, err
	}

	// 添加原始序列号 + 原始 payload（跳过 RTP Header 部分）
	payload := append([]byte{byte(origSeq >> 8), byte(origSeq & 0xFF)}, orig[len(headerBytes):]...)
	return append(rtxHeaderBytes, payload...), nil
}

func sender() {
	time.Sleep(1 * time.Second)

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	nackResponderFactory, _ := nack.NewResponderInterceptor()
	nackResponder, _ := nackResponderFactory.NewInterceptor("")
	chain := interceptor.NewChain([]interceptor.Interceptor{nackResponder})

	var seq uint16 = 100
	sentPkts := make(map[uint16][]byte)

	rtcpReader := chain.BindRTCPReader(interceptor.RTCPReaderFunc(func(in []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		pkts, err := rtcp.Unmarshal(in)
		if err != nil {
			return 0, nil, err
		}
		for _, pkt := range pkts {
			if nackPkt, ok := pkt.(*rtcp.TransportLayerNack); ok {
				for _, pair := range nackPkt.Nacks {
					log.Printf("🔁 Got NACK for Seq=%d", pair.PacketID)
					if origPkt, exists := sentPkts[pair.PacketID]; exists {
						origHeader := &rtp.Header{}
						_, err := origHeader.Unmarshal(origPkt)
						if err != nil {
							log.Printf("❌ Failed to parse RTP header: %v", err)
							continue
						}
						rtxPkt, err := buildRTXPacket(origPkt, pair.PacketID, origHeader)
						if err != nil {
							log.Printf("❌ Failed to build RTX: %v", err)
							continue
						}
						conn.Write(rtxPkt)
						log.Printf("📤 Resent RTX packet for Seq=%d", pair.PacketID)
					}
				}
			}
		}
		return len(in), nil, nil
	}))

	go func() {
		buf := make([]byte, mtu)
		for {
			n, _ := conn.Read(buf)
			rtcpReader.Read(buf[:n], nil)
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
		h := &rtp.Header{
			Version:        2,
			SSRC:           ssrc,
			PayloadType:    originalPayloadType,
			SequenceNumber: seq,
		}
		payload := []byte{0x01, 0x02}
		hb, err := h.Marshal()
		if err != nil {
			continue
		}
		fullPkt := append(hb, payload...)
		sentPkts[seq] = fullPkt

		if rand.Float64() > lossRate {
			stream.Write(h, payload, nil)
			log.Printf("📤 Sent RTP Seq=%d", seq)
		} else {
			log.Printf("❌ Drop RTP Seq=%d", seq)
		}
		seq++
		time.Sleep(50 * time.Millisecond)
	}
}

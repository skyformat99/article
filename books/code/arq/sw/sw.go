package sw

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	_ = iota
	cmdPush
	cmdAck
)

var (
	cmdSize    = 1
	seqSize    = 4
	overHead   = cmdSize + seqSize
	defaultRto = time.Millisecond * 100
	errAgain   = fmt.Errorf("EAGAIN")
)

type segment struct {
	cmd  uint8
	seq  uint32
	data []byte

	raddr *net.UDPAddr
}

type Sw struct {
	conn *net.UDPConn

	// 接收缓冲区
	rcvMu  sync.Mutex
	rcvbuf *list.List

	// 收到ack事件
	recvAck chan struct{}

	// 收到push事件
	recvPush chan struct{}

	// 下一个发送序列号
	nextSeq uint32

	// 下一个收到序列号
	// 如果收到的序列号和expectSeq不相等，则丢弃
	expectSeq uint32

	// mss，超过mss则进行分片
	mss int

	// rtt采样，微妙级别
	rtt    int64
	minrtt int64
	maxrtt int64

	// 超时计时器
	timer *time.Timer
}

func NewSw(conn *net.UDPConn) *Sw {
	sw := &Sw{
		conn:     conn,
		rcvbuf:   list.New(),
		recvAck:  make(chan struct{}),
		recvPush: make(chan struct{}),
		timer:    time.NewTimer(defaultRto),
		mss:      1400,
		rtt:      0,
		minrtt:   math.MaxInt64,
		maxrtt:   math.MinInt64,
	}
	go sw.read()
	return sw
}

func (sw *Sw) Peek() ([]byte, error) {
	sw.rcvMu.Lock()

	if sw.rcvbuf.Len() > 0 {
		ele := sw.rcvbuf.Front()
		seg := ele.Value.(segment)
		sw.rcvbuf.Remove(ele)
		sw.rcvMu.Unlock()
		return seg.data, nil
	}
	sw.rcvMu.Unlock()

	select {
	case <-sw.recvPush:
		sw.rcvMu.Lock()
		if sw.rcvbuf.Len() > 0 {
			ele := sw.rcvbuf.Front()
			seg := ele.Value.(segment)
			sw.rcvbuf.Remove(ele)
			sw.rcvMu.Unlock()
			return seg.data, nil
		}
		sw.rcvMu.Unlock()
	}

	return nil, errAgain
}

func (sw *Sw) Send(data []byte, raddr *net.UDPAddr) {
	// 分片
	if len(data) > sw.mss {
		log.Println("[D] fragment")
		for c := 0; c < len(data); {
			pos := c + sw.mss
			if pos > len(data) {
				pos = len(data)
			}
			sw.sendFragment(data[c:pos], raddr)
			c = pos
		}
	} else {
		sw.sendFragment(data, raddr)
	}
}

func (sw *Sw) sendFragment(data []byte, raddr *net.UDPAddr) {
	seg := segment{
		cmd:   cmdPush,
		seq:   sw.nextSeq,
		data:  data,
		raddr: raddr,
	}
	sw.tx(seg)

	beg := time.Now()
	sw.timer.Reset(defaultRto)
	for {
		select {
		// 死等ack
		case <-sw.recvAck:
			rtt := time.Now().Sub(beg).Microseconds()
			sw.rtt = rtt
			if rtt < sw.minrtt {
				sw.minrtt = rtt
			}

			if rtt > sw.maxrtt {
				sw.maxrtt = rtt
			}

			log.Printf("[D] rtt %d minrtt: %d maxrtt: %d\n",
				sw.rtt, sw.minrtt, sw.maxrtt)

			sw.timer.Stop()
			atomic.AddUint32(&sw.nextSeq, 1)
			return

		// 超时重传
		case <-sw.timer.C:
			log.Println("resend ", seg.seq)
			sw.tx(seg)
			sw.timer.Reset(defaultRto)
		}
	}
}

func (sw *Sw) read() error {
	for {
		seg, err := sw.rx()
		if err != nil {
			fmt.Println(err)
			return err
		}

		if seg.cmd == cmdAck { // ack包
			log.Println("[D] receive ack")
			sw.recvAck <- struct{}{}
		} else {
			if seg.seq > sw.expectSeq {
				fmt.Printf("expected %d,got %d\n", sw.expectSeq, seg.seq)
				continue
			}

			// 延迟包
			// 延迟包依旧需要确认
			// ack不设置确认机制，ack丢失时，会永远收不到数据包
			if seg.seq < sw.expectSeq {
				log.Println("[D] receive delay seg")
				// 响应ack
				sw.tx(segment{
					cmd:   cmdAck,
					raddr: seg.raddr,
				})
				continue
			}

			sw.rcvMu.Lock()
			sw.rcvbuf.PushBack(seg)
			sw.rcvMu.Unlock()
			// 响应ack
			sw.tx(segment{
				cmd:   cmdAck,
				raddr: seg.raddr,
			})

			// 通知上层收包
			sw.recvPush <- struct{}{}

			sw.expectSeq += 1
		}
	}
}

func (sw *Sw) tx(buf segment) {
	data := make([]byte, 5)
	data[0] = buf.cmd
	binary.BigEndian.PutUint32(data[1:], buf.seq)
	data = append(data, buf.data...)
	if sw.conn.RemoteAddr() != nil {
		_, err := sw.conn.Write(data)
		if err != nil {
			fmt.Println(err)
		}
	} else {
		_, err := sw.conn.WriteTo(data, buf.raddr)
		if err != nil {
			fmt.Println(err)
		}
	}
}

func (sw *Sw) rx() (segment, error) {
	buf := make([]byte, sw.mss)
	nr, raddr, err := sw.conn.ReadFromUDP(buf)
	if err != nil {
		return segment{}, err
	}

	// ack不携带数据
	if nr == 1 && buf[0] == cmdAck {
		return segment{cmd: cmdAck}, nil
	}

	if nr < overHead {
		return segment{}, fmt.Errorf("invalid overhead")
	}

	seq := binary.BigEndian.Uint32(buf[cmdSize:])
	seg := segment{
		cmd:   buf[0],
		seq:   seq,
		data:  buf[cmdSize+seqSize : nr],
		raddr: raddr,
	}
	fmt.Println(buf[:nr])
	return seg, nil
}
package tunnel

import (
	"bytes"
	"encoding/binary"
	"io"
	"time"
)

func (t *Tunnel) on_packet (pkt *Packet) (recycly bool, err error) {
	var p *Packet

	t.logger.Debug("recv", pkt)
	t.stat.recvpkt += 1
	t.timer.keep = TM_KEEPALIVE

	if (pkt.flag & RST) != 0 {
		t.c_event <- EV_END
		return true, nil
	}

	diff := (pkt.seq - t.recvseq)
	size := len(pkt.content)
	if t.status == EST && pkt.flag == 0 && diff == 0 && size != 0 &&
		(len(t.recvbuf) == 0 || t.recvbuf[0].seq != pkt.seq + int32(size)) {
		t.recv_data(pkt)
		if OPT_DELAYACK {
			if t.timer.dack == 0 { t.timer.dack = 1 }
		}else{ t.send(ACK, nil) }
		return true, nil
	}

	if diff >= 0 {
		if (pkt.flag & ACK) != 0 { t.recv_ack(pkt) }
		if (pkt.flag & SACK) != 0 {
			err = t.recv_sack(pkt)
			if err != nil { panic(err) }
			return true, nil
		}
	}

	switch t.status {
	case TIMEWAIT:
		if (pkt.flag & SYN) != 0 { t.c_event <- EV_END }
		return true, nil
	case FINWAIT1:
		if pkt.flag == ACK && pkt.ack == t.sendseq {
			t.status = FINWAIT2
			t.send(FIN, nil)
			return true, nil
		}
	case CLOSING:
		if pkt.flag == ACK && pkt.ack == t.sendseq {
			t.status = TIMEWAIT
			t.timer.set_close()
			t.close_nowait()
			return true, nil
		}
	case LASTACK:
		if pkt.flag == ACK && pkt.ack == t.sendseq {
			t.status = CLOSED
			t.c_event <- EV_END
			return true, nil
		}
	case SYNRCVD:
		t.timer.conn = 0
		t.status = EST
		t.c_wrout = t.c_wrin
	}

	switch {
	case diff < 0:
		if pkt.flag != ACK { t.send(ACK, nil) }
		return true, nil
	case diff == 0:
		var ok bool
		ackneed := false
		for p = pkt; ; p = t.recvbuf.Pop() {
			ok, err = t.proc_current(p)
			if err != nil { panic(err) }
			ackneed = ackneed || ok
			put_packet(p)

			if len(t.recvbuf) == 0 || (t.recvbuf[0].seq != t.recvseq) {
				break
			}
		}

		if ackneed || (t.recvack != t.recvseq) {
			if OPT_DELAYACK {
				if t.timer.dack == 0 { t.timer.dack = 1 }
			}else{ t.send(ACK, nil) }
		}
	case diff > 0:
		if (len(pkt.content) > 0) || (pkt.flag != ACK) {
			if !t.recvbuf.Push(pkt) { recycly = true }
		}else{ recycly = true }
		err = t.send_sack()
		// if err != nil { return }
		if err != nil { panic(err) }
	}

	return
}

func (t *Tunnel) recv_data (pkt *Packet) {
	t.readlck.Lock()
	defer t.readlck.Unlock()
	_, err := t.readbuf.Write(pkt.content)
	if err != nil { panic(err) }
	select {
	case t.c_read <- 1:
	default:
	}
	size := len(pkt.content)
	t.recvseq += int32(size)
	t.stat.recvsize += uint64(size)
}

func (t *Tunnel) proc_current (pkt *Packet) (ackneed bool, err error) {
	t.sendwnd = int32(pkt.window)

	switch {
	case len(pkt.content) > 0:
		t.recv_data(pkt)
		return
	case pkt.flag == 0: return true, nil
	case pkt.flag != ACK: t.recvseq += 1
	default: return
	}

	switch pkt.flag & ^ACK {
	case SYN:
		if (pkt.flag & ACK) != 0 {
			if t.status != SYNSENT {
				t.drop()
				t.logger.Err("SYN ACK status wrong,", t)
				return
			}
			t.timer.conn = 0
			t.status = EST
			t.send(ACK, nil)
			t.c_wrout = t.c_wrin
			t.c_connect <- EV_CONNECTED
		}else{
			if t.status != CLOSED {
				t.drop()
				t.logger.Err("SYN status wrong,", t)
				return
			}
			t.status = SYNRCVD
			t.send(SYN | ACK, nil)
		}
	case FIN:
		switch t.status {
		case TIMEWAIT: t.send(ACK, nil)
		case EST:
			t.status = LASTACK
			t.send(FIN | ACK, nil)
			t.c_wrout = nil
			return
		case FINWAIT1:
			if len(t.sendbuf) == 0 {
				t.status = TIMEWAIT
				t.send(ACK, nil)
				t.timer.set_close()
				t.close_nowait()
			}else{
				t.status = CLOSING
				t.send(ACK, nil)
			}
		case FINWAIT2:
			t.status = TIMEWAIT
			t.send(ACK, nil)
			t.timer.set_close()
			t.close_nowait()
		default:
			t.drop()
			t.logger.Err("FIN status wrong,", t)
		}
	}
	return
}

func (t *Tunnel) recv_ack(pkt *Packet) {
	var p *Packet
	ti := time.Now()

	resend_flag := t.sack_count >= 2 || t.retrans_count != 0
	for len(t.sendbuf) != 0 && (t.sendbuf[0].seq - pkt.ack) < 0 {
		p = t.sendbuf.Pop()

		if !resend_flag {
			if t.rtt == 0 {
				t.rtt = uint32(ti.Sub(p.t).Nanoseconds() / 1000) + 1
				t.rttvar = t.rtt / 2
			}else{
				delta := int32(ti.Sub(p.t).Nanoseconds() / 1000 - int64(t.rtt))
				t.rtt = uint32(int32(t.rtt) + delta >> 3)
				t.rttvar = uint32(int32(t.rttvar) + (abs(delta) - int32(t.rttvar)) >> 2)
			}
		}

		t.stat.sendpkt += 1
		t.stat.sendsize += uint64(len(p.content))
		t.stat.senderr -= 1

		put_packet(p)
	}
	t.rto = int32((t.rtt + t.rttvar << 2 + 999)/1000)
	// if t.rto < TM_TICK { t.rto = TM_TICK }
	t.logger.Info("rtt info,", t.rtt, t.rttvar, t.rto)

	switch {
	case resend_flag: t.cwnd = t.ssthresh
	case t.cwnd <= t.ssthresh: t.cwnd += MSS
	case t.cwnd < MSS*MSS: t.cwnd += MSS*MSS/t.cwnd
	default: t.cwnd += 1
	}
	t.sack_count = 0
	t.sack_sent = nil
	t.retrans_count = 0
	t.logger.Info("congestion adjust, ack,", t.cwnd, t.ssthresh)

	if t.timer.rexmt != 0 {
		if len(t.sendbuf) != 0 {
			t.timer.rexmt = t.rto
			t.timer.rexmt -= int32(ti.Sub(t.sendbuf[0].t) / 1000000)
		}else{ t.timer.rexmt = 0 }
	}
	return 
}

func (t *Tunnel) recv_sack(pkt *Packet) (err error) {
	var i int
	var id int32

	t.logger.Debug("sack proc", t.sendbuf)
	t.stat.recverr += 1
	buf := bytes.NewBuffer(pkt.content)

	err = binary.Read(buf, binary.BigEndian, &id)
	t.logger.Debug("sack id", id)
	switch err {
	case io.EOF: err = nil
	case nil:
		var sendbuf PacketQueue
LOOP:
		for i = 0; i < len(t.sendbuf); {
			p := t.sendbuf[i]
			df := p.seq - id
			switch {
			case df == 0:
				put_packet(p)
				i += 1
			case df < 0:
				sendbuf = append(sendbuf, p)
				i += 1
			}

			if df >= 0 {
				err = binary.Read(buf, binary.BigEndian, &id)
				switch err {
				case io.EOF:
					err = nil
					break LOOP
				case nil:
				default:
					panic(err)
					// return
				}
				t.logger.Debug("sack id", id)
			}
		}
		if i < len(t.sendbuf) { sendbuf = append(sendbuf, t.sendbuf[i:]...) }
		t.sendbuf = sendbuf
	}
	t.logger.Debug("sack proc end", t.sendbuf)

	t.sack_count += 1
	switch {
	case t.sack_count < RETRANS_SACKCOUNT: return
	case t.sack_count == RETRANS_SACKCOUNT:
		inairlen := int32(0)
		if len(t.sendbuf) > 0 { inairlen = t.sendseq - t.sendbuf[0].seq }
		t.ssthresh = max32(int32(float32(inairlen)*BACKRATE), 2*MSS)
		t.cwnd = t.ssthresh + 3*MSS
		t.logger.Info("congestion adjust, first sack,", t.cwnd, t.ssthresh)
	case t.sack_count > RETRANS_SACKCOUNT:
		t.cwnd += MSS
		t.logger.Info("congestion adjust, sack,", t.cwnd, t.ssthresh)
	}

	var ok bool
	if t.sack_sent == nil { t.sack_sent = make(map[int32]uint8) }
	for _, p := range t.sendbuf {
		if (p.seq - id) >= 0 { break }
		if t.sack_sent != nil {
			_, ok = t.sack_sent[p.seq]
			if ok { continue }
		}
		t.send_packet(p)
		t.sack_sent[p.seq] = 1
	}
	t.timer.rexmt = t.rto * (1 << t.retrans_count)
	return
}
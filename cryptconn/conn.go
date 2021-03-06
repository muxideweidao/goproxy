package cryptconn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"time"

	logging "github.com/op/go-logging"
)

var logger = logging.MustGetLogger("")

const (
	KEYSIZE           = 16
	DEBUGOUTPUT       = false
	HANDSHAKE_TIMEOUT = 30 * time.Second
)

func NewBlock(method string, key string) (c cipher.Block, err error) {
	logger.Debugf("Crypt Wrapper with %s preparing.", method)
	byteKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return
	}

	switch method {
	default:
		c, err = aes.NewCipher(byteKey)
	case "aes":
		c, err = aes.NewCipher(byteKey)
	case "des":
		c, err = des.NewCipher(byteKey)
	case "tripledes":
		c, err = des.NewTripleDESCipher(byteKey)
	}
	return
}

type CryptConn struct {
	net.Conn
	block cipher.Block
	in    cipher.Stream
	out   cipher.Stream
}

func SentIV(conn net.Conn, n int) (iv []byte, err error) {
	iv = make([]byte, n)
	_, err = rand.Read(iv)
	if err != nil {
		return
	}

	w, err := conn.Write(iv)
	if err != nil {
		return
	}
	if n != w {
		err = io.ErrShortWrite
		return
	}

	logger.Debugf("sent iv: %x", iv)
	return
}

func RecvIV(conn net.Conn, n int) (iv []byte, err error) {
	iv = make([]byte, n)
	t := time.Now().Add(HANDSHAKE_TIMEOUT)
	conn.SetReadDeadline(t)

	_, err = io.ReadFull(conn, iv)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	logger.Debugf("recv iv: %x", iv)
	return
}

func XOR(n int, a []byte, b []byte) (r []byte) {
	r = make([]byte, n)
	for i := 0; i < n; i++ {
		r[i] = a[i] ^ b[i]
	}
	logger.Debugf("xor iv: %x", r)
	return
}

// It is not safe to do like this. Each time session's security key should be
// generated and used just for one time. So we can make sure that attacker
// who recorded everything will never recover data back even he cracked key.

func ExchangeIV(conn net.Conn, n int) (iv []byte, err error) {
	ivs, err := SentIV(conn, n)
	if err != nil {
		return
	}

	ivr, err := RecvIV(conn, n)
	if err != nil {
		return
	}

	iv = XOR(n, ivs, ivr)
	logger.Noticef("Exchange IV for %s: %x", conn.RemoteAddr().String(), iv)
	return
}

func NewClient(conn net.Conn, block cipher.Block) (sc CryptConn, err error) {
	iv, err := ExchangeIV(conn, block.BlockSize())
	if err != nil {
		return
	}

	sc = CryptConn{
		Conn:  conn,
		block: block,
		in:    cipher.NewCFBDecrypter(block, iv),
		out:   cipher.NewCFBEncrypter(block, iv),
	}
	return
}

func NewServer(conn net.Conn, block cipher.Block) (sc *CryptConn, err error) {
	iv, err := ExchangeIV(conn, block.BlockSize())
	if err != nil {
		return
	}

	sc = &CryptConn{
		Conn:  conn,
		block: block,
		in:    cipher.NewCFBDecrypter(block, iv),
		out:   cipher.NewCFBEncrypter(block, iv),
	}
	return
}

func (sc CryptConn) Read(b []byte) (n int, err error) {
	n, err = sc.Conn.Read(b)
	if err != nil {
		return
	}
	sc.in.XORKeyStream(b[:n], b[:n])
	if DEBUGOUTPUT {
		logger.Debugf("recv\n", hex.Dump(b[:n]))
	}
	return
}

func (sc CryptConn) Write(b []byte) (n int, err error) {
	if DEBUGOUTPUT {
		logger.Debugf("send\n", hex.Dump(b))
	}
	sc.out.XORKeyStream(b[:], b[:])
	return sc.Conn.Write(b)
}

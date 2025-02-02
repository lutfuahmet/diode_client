// Diode Network Client
// Copyright 2019 IoT Blockchain Technology Corporation LLC (IBTC)
// Licensed under the Diode License, Version 1.0
package rpc

import (
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/diodechain/diode_go_client/config"
	"github.com/diodechain/diode_go_client/crypto"
	"github.com/diodechain/diode_go_client/db"
	"github.com/diodechain/diode_go_client/edge"
	"github.com/diodechain/diode_go_client/util"
	"github.com/diodechain/openssl"
)

type SSL struct {
	conn              *openssl.Conn
	ctx               *openssl.Ctx
	tcpConn           *net.TCPConn
	addr              string
	mode              openssl.DialFlags
	enableKeepAlive   bool
	keepAliveInterval time.Duration
	totalConnections  uint64
	totalBytes        uint64
	counter           uint64
	clientPrivKey     *ecdsa.PrivateKey
	rm                sync.RWMutex
	cd                sync.Once
	closeCh           chan struct{}
	serverID          util.Address
}

// Host returns the non-resolved addr name of the host
func (s *SSL) Host() string {
	return s.addr
}

// DialContext connect to address with openssl context
func DialContext(ctx *openssl.Ctx, addr string, mode openssl.DialFlags) (*SSL, error) {
	conn, err := openssl.Dial("tcp", addr, ctx, mode)
	if err != nil {
		return nil, err
	}
	s := &SSL{
		conn:    conn,
		ctx:     ctx,
		addr:    addr,
		mode:    mode,
		closeCh: make(chan struct{}),
	}
	return s, nil
}

// LocalAddr returns local network address of ssl connection
func (s *SSL) LocalAddr() net.Addr {
	conn := s.UnderlyingConn()
	return conn.LocalAddr()
}

// RemoteAddr returns remote network address of ssl connection
func (s *SSL) RemoteAddr() net.Addr {
	conn := s.UnderlyingConn()
	return conn.RemoteAddr()
}

// TotalConnections returns total connections of device
func (s *SSL) TotalConnections() uint64 {
	s.rm.RLock()
	defer s.rm.RUnlock()
	return s.totalConnections
}

// TotalBytes returns total bytes that sent from device
func (s *SSL) TotalBytes() uint64 {
	s.rm.RLock()
	defer s.rm.RUnlock()
	return s.totalBytes
}

// Counter returns counter in ssl
func (s *SSL) UpdateCounter(c uint64) {
	s.rm.Lock()
	defer s.rm.Unlock()
	s.counter = c
}

// Counter returns counter in ssl
func (s *SSL) Counter() uint64 {
	s.rm.RLock()
	defer s.rm.RUnlock()
	return s.counter
}

// UnderlyingConn returns connection of ssl
func (s *SSL) UnderlyingConn() net.Conn {
	s.rm.RLock()
	defer s.rm.RUnlock()
	return s.conn.UnderlyingConn()
}

// Closed returns connection is closed
func (s *SSL) Closed() bool {
	return isClosed(s.closeCh)
}

// Close the ssl connection
func (s *SSL) Close() {
	s.cd.Do(func() {
		close(s.closeCh)
		s.conn.Close()
	})
}

// EnableKeepAlive enable the tcp keepalive package in os level, could use ping instead
func (s *SSL) EnableKeepAlive() error {
	netConn := s.conn.UnderlyingConn()
	tcpConn, ok := netConn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("wrong conn type for keepalive")
	}
	err := tcpConn.SetKeepAlive(true)
	if err != nil {
		return err
	}
	s.tcpConn = tcpConn
	s.enableKeepAlive = true
	return nil
}

// SetKeepAliveInterval sets the time (in seconds) between individual keepalive
// probes.
func (s *SSL) SetKeepAliveInterval(d time.Duration) error {
	if !s.enableKeepAlive {
		return fmt.Errorf("should enable keepalive first")
	}
	s.keepAliveInterval = d
	return s.tcpConn.SetKeepAlivePeriod(d)
}

// GetServerID returns server address
func (s *SSL) GetServerID() ([20]byte, error) {
	if s.serverID != util.EmptyAddress {
		return s.serverID, nil
	}
	serverID, err := GetConnectionID(s.conn)
	if err != nil {
		return util.EmptyAddress, err
	}
	copy(s.serverID[:], serverID[:])
	return serverID, nil
}

// GetConnectionID returns address from an openssl connection
func GetConnectionID(conn *openssl.Conn) ([20]byte, error) {
	pubKey, err := getConnectionPubkey(conn)
	if err != nil {
		return [20]byte{}, err
	}
	hashPubKey := util.PubkeyToAddress(pubKey)
	return hashPubKey, nil
}

// GetCertificatePubKey returns server uncompressed public key
func getConnectionPubkey(conn *openssl.Conn) ([]byte, error) {
	cert, err := conn.PeerCertificate()
	if err != nil {
		return nil, err
	}
	rawPubKey, err := cert.PublicKey()
	if err != nil {
		return nil, err
	}
	derPubKey, err := rawPubKey.MarshalPKIXPublicKeyDER()
	if err != nil {
		return nil, err
	}
	return crypto.DerToPublicKey(derPubKey)
}

// GetClientPrivateKey returns client private key
// It can fetch from openssl, the way is like GetServerPublicKey doing:
// 1. get key from openssl
// 2. change format from openssl key to DER
// 3. change format from DER to ecdsa
// Maybe compare both
func (s *SSL) GetClientPrivateKey() (*ecdsa.PrivateKey, error) {
	if s.clientPrivKey != nil {
		return s.clientPrivKey, nil
	}
	kd := EnsurePrivatePEM()
	block, _ := pem.Decode(kd)
	if block == nil {
		return nil, fmt.Errorf("invalid pem private key format")
	}
	clientPrivKey, err := crypto.DerToECDSA(block.Bytes)
	if err != nil {
		return nil, err
	}
	s.clientPrivKey = clientPrivKey
	return clientPrivKey, nil
}

// LoadClientPubKey loads the clients public key from the database
func LoadClientPubKey() []byte {
	kd := EnsurePrivatePEM()
	block, _ := pem.Decode(kd)
	privKey, err := crypto.DerToECDSA(block.Bytes)
	if err != nil {
		return []byte{}
	}
	clientPubKey := crypto.MarshalPubkey(&privKey.PublicKey)
	return clientPubKey
}

func (s *SSL) incrementTotalConnections(n int) {
	s.rm.Lock()
	defer s.rm.Unlock()
	s.totalConnections += uint64(n)
}

func (s *SSL) incrementTotalBytes(n int) {
	s.rm.Lock()
	defer s.rm.Unlock()
	s.totalBytes += uint64(n)
}

func (s *SSL) getOpensslConn() *openssl.Conn {
	s.rm.Lock()
	defer s.rm.Unlock()
	return s.conn
}

func (s *SSL) readMessage() (msg edge.Message, err error) {
	// read length of response
	var n int
	lenByt := make([]byte, 2)
	conn := s.getOpensslConn()
	_, err = conn.Read(lenByt)
	if err != nil {
		return
	}
	lenr := binary.BigEndian.Uint16(lenByt)
	if lenr <= 0 {
		return msg, fmt.Errorf("read 0 byte from connection")
	}
	// read response
	res := make([]byte, lenr)
	read := 0
	for read < int(lenr) && err == nil {
		n, err = conn.Read(res[read:])
		read += n
	}
	if err != nil {
		return
	}
	read += 2
	s.incrementTotalBytes(read)
	msg = edge.Message{
		Len:    read,
		Buffer: res,
	}
	return msg, nil
}

func (s *SSL) sendMessage(buf []byte) (n int, err error) {
	conn := s.getOpensslConn()
	// write message length
	byteLen := make([]byte, 2)
	binary.BigEndian.PutUint16(byteLen, uint16(len(buf)))
	n, err = conn.Write(byteLen)
	if err != nil {
		return
	}
	if n != len(byteLen) {
		err = fmt.Errorf("data was truncated")
		return
	}
	n, err = conn.Write(buf)
	if err != nil {
		return
	}
	if n != len(buf) {
		err = fmt.Errorf("data was truncated")
	}
	s.incrementTotalBytes(n + len(byteLen))
	return
}

func (s *SSL) reconnect() error {
	s.rm.Lock()
	conn, err := openssl.Dial("tcp", s.addr, s.ctx, s.mode)
	if err != nil {
		s.rm.Unlock()
		return err
	}
	s.conn = conn
	s.rm.Unlock()
	if s.enableKeepAlive {
		s.EnableKeepAlive()
		err = s.SetKeepAliveInterval(s.keepAliveInterval)
		if err != nil {
			return err
		}
	}
	s.incrementTotalConnections(1)
	return nil
}

func EnsurePrivatePEM() []byte {
	key, _ := db.DB.Get("private")
	if key == nil {
		privKey, err := openssl.GenerateECKey(openssl.Secp256k1)
		if err != nil {
			config.AppConfig.Logger.Error(fmt.Sprintf("Failed to generate ec key: %s", err.Error()))
			os.Exit(129)
		}
		bytes, err := privKey.MarshalPKCS1PrivateKeyPEM()
		if err != nil {
			config.AppConfig.Logger.Error(fmt.Sprintf("Failed to marshal ec key: %s", err.Error()))
			os.Exit(129)
		}
		err = db.DB.Put("private", bytes)
		if err != nil {
			config.AppConfig.Logger.Error(fmt.Sprintf("Failed to save ec key to file: %s", err.Error()))
			os.Exit(129)
		}
		return bytes
	}
	return key
}

func DoConnect(host string, config *config.Config, pool *DataPool) (*Client, error) {
	ctx := initSSLCtx(config)
	client, err := DialContext(ctx, host, openssl.InsecureSkipHostVerification)
	if err != nil {
		config.Logger.Error(fmt.Sprintf("Failed to connect to: %s (%s)", host, err.Error()))
		// Retry to connect
		isOk := false
		backoff := Backoff{
			Min:    config.RetryWait,
			Max:    10 * config.RetryWait,
			Factor: 2,
			Jitter: true,
		}
		for i := 1; i <= config.RetryTimes; i++ {
			dur := backoff.Duration()
			config.Logger.Info(fmt.Sprintf("Retry to connect to %s (%d/%d), waiting %s", host, i, config.RetryTimes, dur.String()))
			time.Sleep(dur)
			client, err = DialContext(ctx, host, openssl.InsecureSkipHostVerification)
			if err == nil {
				isOk = true
				break
			}
			if config.Debug {
				config.Logger.Debug(fmt.Sprintf("Failed to connect to host: %s (%s)", host, err.Error()))
			}
		}
		if !isOk {
			return nil, fmt.Errorf("failed to connect to host: %s", host)
		}
	}
	// enable keepalive
	if config.EnableKeepAlive {
		err = client.EnableKeepAlive()
		if err != nil {
			return nil, err
		}
		err = client.SetKeepAliveInterval(config.KeepAliveInterval)
		if err != nil {
			return nil, err
		}
	}

	rpcConfig := clientConfig{
		ClientAddr:   config.ClientAddr,
		RegistryAddr: config.RegistryAddr,
		FleetAddr:    config.FleetAddr,
		Blocklists:   config.Blocklists,
		Allowlists:   config.Allowlists,
	}
	rpcClient := NewClient(client, rpcConfig, pool)

	rpcClient.Verbose = config.Debug
	rpcClient.logger = config.Logger

	if config.EnableMetrics {
		rpcClient.enableMetrics = true
		rpcClient.metrics = NewMetrics()
	}
	rpcClient.Start()

	return rpcClient, nil
}

func initSSLCtx(config *config.Config) *openssl.Ctx {
	ctx, err := doInitSSLCtx(config)
	if err != nil {
		config.Logger.Error(fmt.Sprintf("failed to initSSL: %s", err.Error()))
		os.Exit(129)
	}
	return ctx
}

func doInitSSLCtx(config *config.Config) (*openssl.Ctx, error) {
	serial := new(big.Int)
	if _, err := fmt.Sscan("18446744073709551617", serial); err != nil {
		return nil, err
	}
	name, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	info := &openssl.CertificateInfo{
		Serial: serial,
		// The go-openssl library converts these Issued and Expires relative to 'now'
		Issued:       -24 * time.Hour,
		Expires:      24 * time.Hour,
		Country:      "US",
		Organization: "Private",
		CommonName:   name,
	}
	privPEM := EnsurePrivatePEM()
	key, err := openssl.LoadPrivateKeyFromPEM(privPEM)
	if err != nil {
		return nil, err
	}
	cert, err := openssl.NewCertificate(info, key)
	if err != nil {
		return nil, err
	}
	if err = cert.Sign(key, openssl.EVP_SHA256); err != nil {
		return nil, err
	}
	ctx, err := openssl.NewCtxWithVersion(openssl.TLSv1_2)
	if err != nil {
		return nil, err
	}
	if err = ctx.UseCertificate(cert); err != nil {
		return nil, err
	}
	if err = ctx.UsePrivateKey(key); err != nil {
		return nil, err
	}

	// We only use self-signed certificates.
	verifyOption := openssl.VerifyFailIfNoPeerCert | openssl.VerifyPeer
	cb := func(ok bool, store *openssl.CertificateStoreCtx) bool {
		if !ok {
			err := store.Err()
			if err.Error() == "openssl: self signed certificate" {
				return true
			}
			fmt.Printf("Peer verification error: %v\n", err)
			return false
		}
		return ok
	}
	ctx.SetVerify(verifyOption, cb)
	ctx.SetTLSExtServernameCallback(func(ssl *openssl.SSL) openssl.SSLTLSExtErr {
		return openssl.SSLTLSExtErrOK
	})
	if err = ctx.SetEllipticCurve(openssl.Secp256k1); err != nil {
		return nil, err
	}
	curves := []openssl.EllipticCurve{openssl.Secp256k1}
	if err = ctx.SetSupportedEllipticCurves(curves); err != nil {
		return nil, err
	}

	// sets the OpenSSL session lifetime
	ctx.SetTimeout(config.RemoteRPCTimeout)
	return ctx, nil
}

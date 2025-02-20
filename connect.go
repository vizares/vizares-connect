package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/jpillora/backoff"
	"k8s.io/klog"
)

var (
	version                  = "unknown"
	timeout                  = 10 * time.Second
	tlsSkipVerify            = false
	endpointsRefreshInterval = 10 * time.Minute
	backoffFactor            = 2.
	backoffMin               = 5 * time.Second
	backoffMax               = time.Minute
	streamTimeout            = 5 * time.Minute
)

type Tunnel struct {
	address    string
	serverName string
	token      string
	config     []byte
	cancelFn   context.CancelFunc
	gwConn     net.Conn
}

func NewTunnel(address, serverName string, token string, config []byte) *Tunnel {
	t := &Tunnel{
		address:    address,
		serverName: serverName,
		token:      token,
		config:     config,
	}
	var ctx context.Context
	ctx, t.cancelFn = context.WithCancel(context.Background())
	go t.keepConnected(ctx)
	return t
}

func (t *Tunnel) keepConnected(ctx context.Context) {
	b := backoff.Backoff{Factor: backoffFactor, Min: backoffMin, Max: backoffMax}
	var err error
	for {
		select {
		case <-ctx.Done():
			return
		default:
			t.gwConn, err = connect(t.address, t.serverName, t.token, t.config)
			if err == nil {
				start := time.Now()
				err = proxy(ctx, t.gwConn)
				_ = t.gwConn.Close()
				if time.Since(start) > b.Max {
					b.Reset()
				}
			}
			if err != nil {
				klog.Errorln(err)
				d := b.Duration()
				klog.Infof("reconnecting to %s in %.0fs", t.address, d.Seconds())
				time.Sleep(d)
				continue
			}
			b.Reset()
		}
	}
}

func (t *Tunnel) Close() {
	t.cancelFn()
	if t.gwConn != nil {
		_ = t.gwConn.Close()
	}
}

func main() {
	resolverUrl := os.Getenv("RESOLVER_URL")
	if resolverUrl == "" {
		resolverUrl = "https://gw.vizares.com/connect/resolve"
	}
	token := mustEnv("PROJECT_TOKEN")
	if len(token) != 36 {
		klog.Exitln("invalid project token")
	}
	configPath := mustEnv("CONFIG_PATH")

	data, err := os.ReadFile(configPath)
	if err != nil {
		klog.Exitln("failed to read config:", err)
	}
	config := []byte(os.ExpandEnv(string(data)))

	klog.Infof("version: %s", version)

	loop(token, resolverUrl, config)
}

func loop(token, resolverUrl string, config []byte) {
	u, err := url.Parse(resolverUrl)
	if err != nil {
		klog.Exitf("invalid resolver URL %s: %s", resolverUrl, err)
	}
	tlsServerName := u.Hostname()

	tunnels := map[string]*Tunnel{}

	b := backoff.Backoff{Factor: backoffFactor, Min: backoffMin, Max: backoffMax}
	for {
		klog.Infof("updating gateways endpoints from %s", resolverUrl)
		endpoints, err := getEndpoints(resolverUrl, token)
		if err != nil {
			d := b.Duration()
			klog.Errorf("failed to get gateway endpoints: %s, retry in %.0fs", err, d.Seconds())
			time.Sleep(d)
			continue
		}
		b.Reset()
		klog.Infof("desired endpoints: %s", endpoints)
		fresh := map[string]bool{}
		for _, e := range endpoints {
			fresh[e] = true
			if _, ok := tunnels[e]; !ok {
				klog.Infof("starting a tunnel to %s", e)
				tunnels[e] = NewTunnel(e, tlsServerName, token, config)
			}
		}
		for e, t := range tunnels {
			if !fresh[e] {
				klog.Infof("closing tunnel with %s", e)
				t.Close()
				delete(tunnels, e)
			}
		}
		time.Sleep(endpointsRefreshInterval)
	}
}

func getEndpoints(resolverUrl, token string) ([]string, error) {
	req, _ := http.NewRequest("GET", resolverUrl, nil)
	req.Header.Set("X-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(payload))
	}
	return strings.Split(strings.TrimSpace(string(payload)), ";"), nil
}

type RequestHeader struct {
	Token      [36]byte
	Version    [16]byte
	ConfigSize uint32
}

type ResponseHeader struct {
	Status      uint16
	MessageSize uint16
}

func connect(gwAddr, serverName, token string, config []byte) (net.Conn, error) {
	requestHeader := RequestHeader{}
	copy(requestHeader.Token[:], token)
	copy(requestHeader.Version[:], version)
	requestHeader.ConfigSize = uint32(len(config))

	klog.Infof("connecting to %s (%s)", gwAddr, serverName)
	deadline := time.Now().Add(timeout)
	dialer := &net.Dialer{Deadline: deadline}
	tlsCfg := &tls.Config{ServerName: serverName, InsecureSkipVerify: tlsSkipVerify}
	gwConn, err := tls.DialWithDialer(dialer, "tcp", gwAddr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to establish a connection to %s: %s", gwAddr, err)
	}
	klog.Infof("connected to gateway %s", gwAddr)

	_ = gwConn.SetDeadline(deadline)
	if err = binary.Write(gwConn, binary.LittleEndian, requestHeader); err != nil {
		_ = gwConn.Close()
		return nil, fmt.Errorf("failed to send config to %s: %s", gwAddr, err)
	}
	if _, err = gwConn.Write(config); err != nil {
		_ = gwConn.Close()
		return nil, fmt.Errorf("failed to send config to %s: %s", gwAddr, err)
	}
	var responseHeader ResponseHeader
	if err := binary.Read(gwConn, binary.LittleEndian, &responseHeader); err != nil {
		_ = gwConn.Close()
		return nil, fmt.Errorf("failed to read the response from %s: %s", gwAddr, err)
	}
	var responseMessage string
	if responseHeader.MessageSize > 0 {
		buf := make([]byte, responseHeader.MessageSize)
		if _, err := gwConn.Read(buf); err != nil {
			_ = gwConn.Close()
			return nil, fmt.Errorf("failed to read the response from %s: %s", gwAddr, err)
		}
		responseMessage = string(buf)
	}
	_ = gwConn.SetDeadline(time.Time{})

	if responseHeader.Status != 200 {
		_ = gwConn.Close()
		return nil, fmt.Errorf("got %d from %s: %s", responseHeader.Status, gwAddr, responseMessage)
	}
	klog.Infof("ready to proxy requests from %s", gwAddr)
	return gwConn, nil
}

func proxy(ctx context.Context, gwConn net.Conn) error {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = time.Second
	cfg.LogOutput = io.Discard
	session, err := yamux.Server(gwConn, cfg)
	if err != nil {
		return fmt.Errorf("failed to start a TCP multiplexing server: %s", err)
	}
	defer session.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			gwStream, err := session.Accept()
			if err != nil {
				return fmt.Errorf("failed to accept a stream: %s", err)
			}
			go func(c net.Conn) {
				defer c.Close()
				deadline := time.Now().Add(streamTimeout)
				if err := c.SetDeadline(deadline); err != nil {
					klog.Errorf("failed to set a deadline for the stream: %s", err)
					return
				}
				var dstLen uint16
				if err := binary.Read(c, binary.LittleEndian, &dstLen); err != nil {
					klog.Errorf("failed to read the destination size: %s", err)
					return
				}
				dest := make([]byte, int(dstLen))
				if _, err := io.ReadFull(c, dest); err != nil {
					klog.Errorf("failed to read the destination address: %s", err)
					return
				}
				destAddress := string(dest)
				destConn, err := net.DialTimeout("tcp", destAddress, timeout)
				if err != nil {
					klog.Errorf("failed to establish a connection to %s: %s", destAddress, err)
					return
				}
				defer destConn.Close()
				if err = destConn.SetDeadline(deadline); err != nil {
					klog.Errorf("failed to set a deadline for the dest connection: %s", err)
					return
				}
				go func() {
					io.Copy(c, destConn)
				}()
				io.Copy(destConn, c)
			}(gwStream)
		}
	}
}

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Exitln(key, "environment variable is required")
	}
	return value
}

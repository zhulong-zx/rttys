package main

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"rttys/client"
	"rttys/utils"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

type httpResp struct {
	data []byte
	dev  client.Client
}

type httpReq struct {
	devid string
	data  []byte
}

var httpProxyCons sync.Map
var httpProxySessions sync.Map

func handleHttpProxyResp(resp *httpResp) {
	data := resp.data
	addr := data[:18]
	data = data[18:]

	if cons, ok := httpProxyCons.Load(resp.dev.DeviceID()); ok {
		if c, ok := cons.(*sync.Map).Load(string(addr)); ok {
			c := c.(net.Conn)
			if len(data) == 0 {
				c.Close()
			} else {
				c.Write(data)
			}
		}
	}
}

func genDestAddr(addr string) []byte {
	destIP, destPort, err := httpProxyVaildAddr(addr)
	if err != nil {
		return nil
	}

	b := make([]byte, 6)
	copy(b, destIP)

	binary.BigEndian.PutUint16(b[4:], destPort)

	return b
}

func tcpAddr2Bytes(addr *net.TCPAddr) []byte {
	b := make([]byte, 18)

	binary.BigEndian.PutUint16(b[:2], uint16(addr.Port))

	copy(b[2:], addr.IP)

	return b
}

type HttpProxyWriter struct {
	destAddr          []byte
	srcAddr           []byte
	hostHeaderRewrite string
	br                *broker
	dev               *device
	https             bool
}

func (rw *HttpProxyWriter) Write(p []byte) (n int, err error) {
	msg := []byte{}
	dev := rw.dev

	if dev.proto > 3 {
		if rw.https {
			msg = append(msg, 1)
		} else {
			msg = append(msg, 0)
		}
	}

	msg = append(msg, rw.srcAddr...)
	msg = append(msg, rw.destAddr...)
	msg = append(msg, p...)

	rw.br.httpReq <- &httpReq{dev.id, msg}

	return len(p), nil
}

func (rw *HttpProxyWriter) WriteRequest(req *http.Request) {
	req.Host = rw.hostHeaderRewrite
	req.Write(rw)
}

func doHttpProxy(brk *broker, c net.Conn) {
	defer c.Close()

	br := bufio.NewReader(c)

	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	cookie, err := req.Cookie("rtty-http-devid")
	if err != nil {
		return
	}
	devid := cookie.Value

	dev, ok := brk.devices[devid]
	if !ok {
		return
	}

	cookie, err = req.Cookie("rtty-http-sid")
	if err != nil {
		return
	}
	sid := cookie.Value

	https := false
	cookie, _ = req.Cookie("rtty-http-proto")
	if cookie != nil && cookie.Value == "https" {
		https = true
	}

	hostHeaderRewrite := "localhost"
	cookie, err = req.Cookie("rtty-http-destaddr")
	if err == nil {
		hostHeaderRewrite, _ = url.QueryUnescape(cookie.Value)
	}

	destAddr := genDestAddr(hostHeaderRewrite)
	srcAddr := tcpAddr2Bytes(c.RemoteAddr().(*net.TCPAddr))

	if cons, _ := httpProxyCons.LoadOrStore(devid, &sync.Map{}); true {
		cons := cons.(*sync.Map)
		cons.Store(string(srcAddr), c)
	}

	exit := make(chan struct{})

	if v, ok := httpProxySessions.Load(sid); ok {
		go func() {
			select {
			case <-v.(chan struct{}):
				c.Close()
			case <-exit:
			}

			cons, ok := httpProxyCons.Load(devid)
			if ok {
				cons := cons.(*sync.Map)
				cons.Delete(string(srcAddr))
			}
		}()
	} else {
		return
	}

	hpw := &HttpProxyWriter{destAddr, srcAddr, hostHeaderRewrite, brk, dev.(*device), https}

	req.Host = hostHeaderRewrite
	hpw.WriteRequest(req)

	if req.Header.Get("Upgrade") == "websocket" {
		b := make([]byte, 4096)

		for {
			n, err := c.Read(b)
			if err != nil {
				close(exit)
				return
			}

			msg := append([]byte{}, srcAddr...)
			msg = append(msg, destAddr...)
			msg = append(msg, b[:n]...)

			brk.httpReq <- &httpReq{devid, msg}
		}
	} else {
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				close(exit)
				return
			}

			hpw.WriteRequest(req)
		}
	}
}

func listenHttpProxy(brk *broker) {
	cfg := brk.cfg

	if cfg.AddrHttpProxy != "" {
		addr, err := net.ResolveTCPAddr("tcp", cfg.AddrHttpProxy)
		if err != nil {
			log.Warn().Msg("invalid http proxy addr: " + err.Error())
		} else {
			cfg.HttpProxyPort = addr.Port
		}
	}
	ln, err := net.Listen("tcp", cfg.AddrHttpProxy)

	if cfg.SslCert != "" && cfg.SslKey != "" {
		crt, err := tls.LoadX509KeyPair(cfg.SslCert, cfg.SslKey)
		if err != nil {
			log.Fatal().Msg(err.Error())
		}

		tlsConfig := &tls.Config{}
		tlsConfig.Certificates = []tls.Certificate{crt}
		tlsConfig.Time = time.Now
		tlsConfig.Rand = rand.Reader
		tlsConfig.MinVersion = tls.VersionTLS12

		if cfg.SslCacert == "" {
			log.Warn().Msgf("mTLS not enabled")
		} else {
			caCert, err := ioutil.ReadFile(cfg.SslCacert)
			if err != nil {
				log.Error().Msgf("mTLS not enabled: %s", err.Error())
			} else {
				brk.devCertPool = x509.NewCertPool()
				brk.devCertPool.AppendCertsFromPEM(caCert)

				// Create the TLS Config with the CA pool and enable Client certificate validation
				tlsConfig.ClientCAs = brk.devCertPool
				tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
			}
		}

		ln = tls.NewListener(ln, tlsConfig)
		log.Info().Msgf("Listen http proxy on: %s SSL on", ln.Addr().(*net.TCPAddr))
	} else {
		log.Info().Msgf("Listen http proxy on: %s SSL off", ln.Addr().(*net.TCPAddr))
	}

	if cfg.HttpProxyPort == 0 {
		log.Info().Msg("Automatically select an available port for http proxy")
	}

	if err != nil {
		log.Fatal().Msg(err.Error())
	}

	cfg.HttpProxyPort = ln.Addr().(*net.TCPAddr).Port

	log.Info().Msgf("Listen http proxy on: %s", ln.Addr().(*net.TCPAddr))

	go func() {
		defer ln.Close()

		for {
			c, err := ln.Accept()
			if err != nil {
				log.Error().Msg(err.Error())
				continue
			}

			go doHttpProxy(brk, c)
		}
	}()
}

func httpProxyVaildAddr(addr string) (net.IP, uint16, error) {
	ips, ports, err := net.SplitHostPort(addr)
	if err != nil {
		ips = addr
		ports = "80"
	}

	ip := net.ParseIP(ips)
	if ip == nil {
		return nil, 0, errors.New("invalid IPv4 Addr")
	}

	ip = ip.To4()
	if ip == nil {
		return nil, 0, errors.New("invalid IPv4 Addr")
	}

	port, _ := strconv.Atoi(ports)

	return ip, uint16(port), nil
}

func httpProxyRedirect(br *broker, c *gin.Context) {
	cfg := br.cfg
	devid := c.Param("devid")
	proto := c.Param("proto")
	addr := c.Param("addr")
	rawPath := c.Param("path")

	_, _, err := httpProxyVaildAddr(addr)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	path, err := url.Parse(rawPath)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	_, ok := br.devices[devid]
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}

	location := cfg.HttpProxyRedirURL

	if location == "" {
		host, _, err := net.SplitHostPort(c.Request.Host)
		if err != nil {
			host = c.Request.Host
		}
		if cfg.HttpProxyProtocol != "" {
			location = cfg.HttpProxyProtocol + "://" + host
		} else {
			location = "http://" + host
		}

		if cfg.HttpProxyPort != 80 {
			location += fmt.Sprintf(":%d", cfg.HttpProxyPort)
		}
	}

	location += path.Path

	location += fmt.Sprintf("?_=%d", time.Now().Unix())

	if path.RawQuery != "" {
		location += "&" + path.RawQuery
	}

	sid, err := c.Cookie("rtty-http-sid")
	if err == nil {
		if v, ok := httpProxySessions.Load(sid); ok {
			close(v.(chan struct{}))
			httpProxySessions.Delete(sid)
		}
	}

	sid = utils.GenUniqueID("http-proxy")

	httpProxySessions.Store(sid, make(chan struct{}))

	c.SetCookie("rtty-http-sid", sid, 0, "", "", false, true)
	c.SetCookie("rtty-http-devid", devid, 0, "", "", false, true)
	c.SetCookie("rtty-http-proto", proto, 0, "", "", false, true)
	c.SetCookie("rtty-http-destaddr", addr, 0, "", "", false, true)

	c.Redirect(http.StatusFound, location)
}

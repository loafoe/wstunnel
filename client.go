package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/proxy"
	"golang.org/x/net/websocket"
)

var (
	certsDir = flag.String("certs_dir", "", "Directory of certs for TLS connection to AMQP, or empty for non-TLS connection. "+
		"Expected files are: cacert.pem, cert.pem and key.pem.")
	serverName = flag.String("server_name", "", "Name of the server for TLS verification, or empty for default")

	targetHost = flag.String("target_host", "", "The target host:port to tunnel to")
	port       = flag.Int("port", 8080, "The local port to listen on")
	listenAddr = flag.String("listen_addr", "127.0.0.1", "Address to listen on. Empty string for all interfaces.")
)

func getTlsConfig() (*tls.Config, error) {
	if *certsDir == "" {
		return nil, nil
	}

	tlscfg := &tls.Config{
		RootCAs:          x509.NewCertPool(),
		CurvePreferences: []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
		MinVersion:       tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
	}
	if ca, err := ioutil.ReadFile(path.Join(*certsDir, "cacert.pem")); err == nil {
		tlscfg.RootCAs.AppendCertsFromPEM(ca)
	} else {
		return nil, fmt.Errorf("Failed reading CA certificate: %v", err)
	}

	tlscfg.ServerName = strings.Split(*targetHost, ":")[0]
	if *serverName != "" {
		tlscfg.ServerName = *serverName
	}
	return tlscfg, nil
}

func getWsConfig() (*websocket.Config, error) {
	url := url.URL{Scheme: "ws", Host: *targetHost}
	if *certsDir != "" {
		url.Scheme = "wss"
	}

	config, err := websocket.NewConfig(url.String(), "http://localhost/")
	if err != nil {
		return nil, err
	}

	if config.TlsConfig, err = getTlsConfig(); err != nil {
		return nil, err
	}

	return config, nil
}

func iocopy(dst io.Writer, src io.Reader, c chan error) {
	_, err := io.Copy(dst, src)
	c <- err
}

type closeable interface {
	CloseWrite() error
}

func closeWrite(conn net.Conn) {
	if closeme, ok := conn.(closeable); ok {
		closeme.CloseWrite()
	}
}

func getProxiedConn(turl url.URL) (net.Conn, error) {
	// We first try to get a Socks5 proxied conncetion. If that fails, we're moving on to http{s,}_proxy.
	dialer := proxy.FromEnvironment()
	if dialer != proxy.Direct {
		return dialer.Dial("tcp", turl.Host)
	}

	turl.Scheme = strings.Replace(turl.Scheme, "ws", "http", 1)
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: &turl})
	if proxyURL == nil {
		return net.Dial("tcp", turl.Host)
	}

	p, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}

	cc := httputil.NewProxyClientConn(p, nil)
	_, err = cc.Do(&http.Request{
		Method: "CONNECT",
		URL:    &url.URL{},
		Host:   turl.Host,
	})
	if err != nil && err != httputil.ErrPersistEOF {
		return nil, err
	}

	conn, _ := cc.Hijack()

	return conn, nil
}

func handleConnection(wsConfig *websocket.Config, conn net.Conn) {
	defer conn.Close()

	tcp, err := getProxiedConn(*wsConfig.Location)
	if err != nil {
		log.Print("getProxiedConn(): ", err)
		return
	}

	if *certsDir != "" {
		tcp = tls.Client(tcp, wsConfig.TlsConfig)
	}

	ws, err := websocket.NewClient(wsConfig, tcp)
	if err != nil {
		log.Print("websocket.NewClient(): ", err)
		return
	}
	defer ws.Close()

	c := make(chan error, 2)
	go iocopy(ws, conn, c)
	go iocopy(conn, ws, c)

	for i := 0; i < 2; i++ {
		if err := <-c; err != nil {
			fmt.Print("io.Copy(): ", err)
			return
		}
		// If any of the sides closes the connection, we want to close the write channel.
		closeWrite(conn)
		closeWrite(tcp)
	}
}

func main() {
	flag.Parse()

	wsConfig, err := getWsConfig()
	if err != nil {
		panic(err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *listenAddr, *port))
	if err != nil {
		panic(err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print("ln.Accept(): ", err)
			continue
		}
		go handleConnection(wsConfig, conn)
	}
}

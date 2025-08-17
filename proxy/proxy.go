package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/fosrl/gerbil/logger"
	"github.com/patrickmn/go-cache"
)

// RouteRecord represents a routing configuration
type RouteRecord struct {
	Hostname   string
	TargetHost string
	TargetPort int
}

// RouteAPIResponse represents the response from the route API
type RouteAPIResponse struct {
	Endpoints []string `json:"endpoints"`
}

// SNIProxy represents the main proxy server
type SNIProxy struct {
	port            int
	cache           *cache.Cache
	listener        net.Listener
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	exitNodeName    string
	localProxyAddr  string
	localProxyPort  int
	remoteConfigURL string
	publicKey       string

	// New fields for fast local SNI lookup
	localSNIs     map[string]struct{}
	localSNIsLock sync.RWMutex

	// Local overrides for domains that should always use local proxy
	localOverrides map[string]struct{}

	// Track active tunnels by SNI
	activeTunnels     map[string]*activeTunnel
	activeTunnelsLock sync.Mutex
}

type activeTunnel struct {
	conns []net.Conn
}

// readOnlyConn is a wrapper for io.Reader that implements net.Conn
type readOnlyConn struct {
	reader io.Reader
}

func (conn readOnlyConn) Read(p []byte) (int, error)         { return conn.reader.Read(p) }
func (conn readOnlyConn) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (conn readOnlyConn) Close() error                       { return nil }
func (conn readOnlyConn) LocalAddr() net.Addr                { return nil }
func (conn readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (conn readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (conn readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (conn readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

// NewSNIProxy creates a new SNI proxy instance
func NewSNIProxy(port int, remoteConfigURL, publicKey, exitNodeName, localProxyAddr string, localProxyPort int, localOverrides []string) (*SNIProxy, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Create local overrides map
	overridesMap := make(map[string]struct{})
	for _, domain := range localOverrides {
		if domain != "" {
			overridesMap[domain] = struct{}{}
		}
	}

	proxy := &SNIProxy{
		port:            port,
		cache:           cache.New(3*time.Second, 10*time.Minute),
		ctx:             ctx,
		cancel:          cancel,
		exitNodeName:    exitNodeName,
		localProxyAddr:  localProxyAddr,
		localProxyPort:  localProxyPort,
		remoteConfigURL: remoteConfigURL,
		publicKey:       publicKey,
		localSNIs:       make(map[string]struct{}),
		localOverrides:  overridesMap,
		activeTunnels:   make(map[string]*activeTunnel),
	}

	return proxy, nil
}

// Start begins listening for connections
func (p *SNIProxy) Start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", p.port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", p.port, err)
	}

	p.listener = listener
	logger.Debug("SNI Proxy listening on port %d", p.port)

	// Accept connections in a goroutine
	go p.acceptConnections()

	return nil
}

// Stop gracefully shuts down the proxy
func (p *SNIProxy) Stop() error {
	log.Println("Stopping SNI Proxy...")

	p.cancel()

	if p.listener != nil {
		p.listener.Close()
	}

	// Wait for all goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All connections closed gracefully")
	case <-time.After(30 * time.Second):
		log.Println("Timeout waiting for connections to close")
	}

	log.Println("SNI Proxy stopped")
	return nil
}

// acceptConnections handles incoming connections
func (p *SNIProxy) acceptConnections() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			select {
			case <-p.ctx.Done():
				return
			default:
				logger.Debug("Accept error: %v", err)
				continue
			}
		}

		p.wg.Add(1)
		go p.handleConnection(conn)
	}
}

// readClientHello reads and parses the TLS ClientHello message
func (p *SNIProxy) readClientHello(reader io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo
	err := tls.Server(readOnlyConn{reader: reader}, &tls.Config{
		GetConfigForClient: func(argHello *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = new(tls.ClientHelloInfo)
			*hello = *argHello
			return nil, nil
		},
	}).Handshake()
	if hello == nil {
		return nil, err
	}
	return hello, nil
}

// peekClientHello reads the ClientHello while preserving the data for forwarding
func (p *SNIProxy) peekClientHello(reader io.Reader) (*tls.ClientHelloInfo, io.Reader, error) {
	peekedBytes := new(bytes.Buffer)
	hello, err := p.readClientHello(io.TeeReader(reader, peekedBytes))
	if err != nil {
		return nil, nil, err
	}
	return hello, io.MultiReader(peekedBytes, reader), nil
}

// extractSNI extracts the SNI hostname from the TLS ClientHello
func (p *SNIProxy) extractSNI(conn net.Conn) (string, io.Reader, error) {
	clientHello, clientReader, err := p.peekClientHello(conn)
	if err != nil {
		return "", nil, fmt.Errorf("failed to peek ClientHello: %w", err)
	}

	if clientHello.ServerName == "" {
		return "", clientReader, fmt.Errorf("no SNI hostname found in ClientHello")
	}

	return clientHello.ServerName, clientReader, nil
}

// handleConnection processes a single client connection
func (p *SNIProxy) handleConnection(clientConn net.Conn) {
	defer p.wg.Done()
	defer clientConn.Close()

	logger.Debug("Accepted connection from %s", clientConn.RemoteAddr())

	// Set read timeout for SNI extraction
	if err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		logger.Debug("Failed to set read deadline: %v", err)
		return
	}

	// Extract SNI hostname
	hostname, clientReader, err := p.extractSNI(clientConn)
	if err != nil {
		logger.Debug("SNI extraction failed: %v", err)
		return
	}

	if hostname == "" {
		log.Println("No SNI hostname found")
		return
	}

	logger.Debug("SNI hostname detected: %s", hostname)

	// Remove read timeout for normal operation
	if err := clientConn.SetReadDeadline(time.Time{}); err != nil {
		logger.Debug("Failed to clear read deadline: %v", err)
		return
	}

	// Get routing information
	route, err := p.getRoute(hostname)
	if err != nil {
		logger.Debug("Failed to get route for %s: %v", hostname, err)
		return
	}

	if route == nil {
		logger.Debug("No route found for hostname: %s", hostname)
		return
	}

	logger.Debug("Routing %s to %s:%d", hostname, route.TargetHost, route.TargetPort)

	// Connect to target server
	targetConn, err := net.DialTimeout("tcp",
		fmt.Sprintf("%s:%d", route.TargetHost, route.TargetPort),
		10*time.Second)
	if err != nil {
		logger.Debug("Failed to connect to target %s:%d: %v",
			route.TargetHost, route.TargetPort, err)
		return
	}
	defer targetConn.Close()

	logger.Debug("Connected to target: %s:%d", route.TargetHost, route.TargetPort)

	// Track this tunnel by SNI
	p.activeTunnelsLock.Lock()
	tunnel, ok := p.activeTunnels[hostname]
	if !ok {
		tunnel = &activeTunnel{}
		p.activeTunnels[hostname] = tunnel
	}
	tunnel.conns = append(tunnel.conns, clientConn)
	p.activeTunnelsLock.Unlock()

	defer func() {
		// Remove this conn from active tunnels
		p.activeTunnelsLock.Lock()
		if tunnel, ok := p.activeTunnels[hostname]; ok {
			newConns := make([]net.Conn, 0, len(tunnel.conns))
			for _, c := range tunnel.conns {
				if c != clientConn {
					newConns = append(newConns, c)
				}
			}
			if len(newConns) == 0 {
				delete(p.activeTunnels, hostname)
			} else {
				tunnel.conns = newConns
			}
		}
		p.activeTunnelsLock.Unlock()
	}()

	// Start bidirectional data transfer
	p.pipe(clientConn, targetConn, clientReader)
}

// getRoute retrieves routing information for a hostname
func (p *SNIProxy) getRoute(hostname string) (*RouteRecord, error) {
	// Check local overrides first
	if _, isOverride := p.localOverrides[hostname]; isOverride {
		logger.Debug("Local override matched for hostname: %s", hostname)
		return &RouteRecord{
			Hostname:   hostname,
			TargetHost: p.localProxyAddr,
			TargetPort: p.localProxyPort,
		}, nil
	}

	// Fast path: check if hostname is in localSNIs
	p.localSNIsLock.RLock()
	_, isLocal := p.localSNIs[hostname]
	p.localSNIsLock.RUnlock()
	if isLocal {
		return &RouteRecord{
			Hostname:   hostname,
			TargetHost: p.localProxyAddr,
			TargetPort: p.localProxyPort,
		}, nil
	}

	// Check cache first
	if cached, found := p.cache.Get(hostname); found {
		if cached == nil {
			return nil, nil // Cached negative result
		}
		logger.Debug("Cache hit for hostname: %s", hostname)
		return cached.(*RouteRecord), nil
	}

	logger.Debug("Cache miss for hostname: %s, querying API", hostname)

	// Query API with timeout
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Construct API URL (without hostname in path)
	apiURL := fmt.Sprintf("%s/gerbil/get-resolved-hostname", p.remoteConfigURL)

	// Create request body with hostname and public key
	requestBody := map[string]string{
		"hostname":  hostname,
		"publicKey": p.publicKey,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Make HTTP request
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Cache negative result for shorter time (1 minute)
		p.cache.Set(hostname, nil, 1*time.Minute)
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	// Parse response
	var apiResponse RouteAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode API response: %w", err)
	}

	endpoints := apiResponse.Endpoints
	name := apiResponse.Name

	// If the endpoint matches the current exit node, use the local proxy address
	targetHost := endpoint
	targetPort := 443 // Default HTTPS port
	if name == p.exitNodeName {
		targetHost = p.localProxyAddr
		targetPort = p.localProxyPort
	} // THIS IS SAYING TO ROUTE IT LOCALLY IF IT MATCHES - idk HOW TO KEEP THIS

	route := &RouteRecord{
		Hostname:   hostname,
		TargetHost: targetHost,
		TargetPort: targetPort,
	}

	// Cache the result
	p.cache.Set(hostname, route, cache.DefaultExpiration)
	logger.Debug("Cached route for hostname: %s", hostname)

	return route, nil
}

// pipe handles bidirectional data transfer between connections
func (p *SNIProxy) pipe(clientConn, targetConn net.Conn, clientReader io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Copy data from client to target (using the buffered reader)
	go func() {
		defer wg.Done()
		defer func() {
			if tcpConn, ok := targetConn.(*net.TCPConn); ok {
				tcpConn.CloseWrite()
			}
		}()

		// Use a large buffer for better performance
		buf := make([]byte, 32*1024)
		_, err := io.CopyBuffer(targetConn, clientReader, buf)
		if err != nil && err != io.EOF {
			logger.Debug("Copy client->target error: %v", err)
		}
	}()

	// Copy data from target to client
	go func() {
		defer wg.Done()
		defer func() {
			if tcpConn, ok := clientConn.(*net.TCPConn); ok {
				tcpConn.CloseWrite()
			}
		}()

		// Use a large buffer for better performance
		buf := make([]byte, 32*1024)
		_, err := io.CopyBuffer(clientConn, targetConn, buf)
		if err != nil && err != io.EOF {
			logger.Debug("Copy target->client error: %v", err)
		}
	}()

	wg.Wait()
}

// GetCacheStats returns cache statistics
func (p *SNIProxy) GetCacheStats() (int, int) {
	return p.cache.ItemCount(), len(p.cache.Items())
}

// ClearCache clears all cached entries
func (p *SNIProxy) ClearCache() {
	p.cache.Flush()
	log.Println("Cache cleared")
}

// UpdateLocalSNIs updates the local SNIs and invalidates cache for changed domains
func (p *SNIProxy) UpdateLocalSNIs(fullDomains []string) {
	newSNIs := make(map[string]struct{})
	for _, domain := range fullDomains {
		newSNIs[domain] = struct{}{}
		// Invalidate any cached route for this domain
		p.cache.Delete(domain)
	}

	// Update localSNIs
	p.localSNIsLock.Lock()
	removed := make([]string, 0)
	for sni := range p.localSNIs {
		if _, stillLocal := newSNIs[sni]; !stillLocal {
			removed = append(removed, sni)
		}
	}
	p.localSNIs = newSNIs
	p.localSNIsLock.Unlock()

	logger.Debug("Updated local SNIs, added %d, removed %d", len(newSNIs), len(removed))

	// Terminate tunnels for removed SNIs
	if len(removed) > 0 {
		p.activeTunnelsLock.Lock()
		for _, sni := range removed {
			if tunnels, ok := p.activeTunnels[sni]; ok {
				for _, conn := range tunnels.conns {
					conn.Close()
				}
				delete(p.activeTunnels, sni)
				logger.Debug("Closed tunnels for SNI target change: %s", sni)
			}
		}
		p.activeTunnelsLock.Unlock()
	}
}

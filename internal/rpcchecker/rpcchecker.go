package rpcchecker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	// ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"go.uber.org/zap"
)

// RPCNode represents an RPC endpoint
type RPCNode struct {
	Address  string `json:"address"`
	Provider string `json:"provider,omitempty"`
}

// ChainRegistry represents the chain.json format
type ChainRegistry struct {
	APIs struct {
		RPC []RPCNode `json:"rpc"`
	} `json:"apis"`
}

// RPCMonitor dynamically monitors healthy RPC endpoints
type RPCMonitor struct {
	chainRegistryURL string
	customRPCs       []string
	healthy          []RPCNode
	cacheMu          sync.RWMutex
	stopChan         chan struct{}
	logger           *zap.Logger
}

// NewRPCMonitor initializes a new RPC monitor
func NewRPCMonitor(chainRegistryURL string, customRPCs []string, logger *zap.Logger) *RPCMonitor {
	return &RPCMonitor{
		chainRegistryURL: chainRegistryURL,
		customRPCs:       customRPCs,
		stopChan:         make(chan struct{}),
		logger:           logger,
	}
}

// fetchAndUpdateHealthyNodes retrieves RPCs, checks health, and sorts by response time
func (m *RPCMonitor) fetchAndUpdateHealthyNodes() {
	m.logger.Info("Fetching RPC List")

	// Fetch RPCs from registry if URL is provided
	var rpcList []RPCNode
	if m.chainRegistryURL != "" {
		m.logger.Info("Fetching from chain registry", zap.String("url", m.chainRegistryURL))
		rpcs, err := fetchRPCFromRegistry(m.chainRegistryURL)
		if err != nil {
			m.logger.Warn("Failed to fetch RPCs from registry", zap.Error(err))
		} else {
			rpcList = append(rpcList, rpcs...)
		}
	}

	// Add custom RPCs
	for _, rpc := range m.customRPCs {
		rpcList = append(rpcList, RPCNode{Address: rpc})
	}

	// Deduplicate
	rpcMap := make(map[string]RPCNode)
	for _, rpc := range rpcList {
		rpcMap[rpc.Address] = rpc
	}

	// Convert back to slice
	uniqueRPCs := make([]RPCNode, 0, len(rpcMap))
	for _, rpc := range rpcMap {
		uniqueRPCs = append(uniqueRPCs, rpc)
	}

	if len(uniqueRPCs) == 0 {
		m.logger.Warn("No RPC endpoints available after fetching")
		return
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	healthyNodes := []RPCNode{}
	responseTimes := map[string]time.Duration{}
	errorsEncountered := 0

	for _, node := range uniqueRPCs {
		wg.Add(1)

		go func(n RPCNode) {
			defer wg.Done()
			start := time.Now()
			ok := m.checkHealth(n)
			duration := time.Since(start)

			if ok {
				mu.Lock()
				healthyNodes = append(healthyNodes, n)
				responseTimes[n.Address] = duration
				mu.Unlock()
			} else {
				mu.Lock()
				errorsEncountered++
				mu.Unlock()
			}
		}(node)
	}

	wg.Wait()

	// If all RPCs failed, log error and return
	if len(healthyNodes) == 0 {
		m.logger.Error("All RPC health checks failed", zap.Int("failed_count", errorsEncountered))
		return
	}

	// Sort healthy nodes by fastest response time
	sort.Slice(healthyNodes, func(i, j int) bool {
		return responseTimes[healthyNodes[i].Address] < responseTimes[healthyNodes[j].Address]
	})

	m.cacheMu.Lock()
	m.healthy = healthyNodes
	m.cacheMu.Unlock()

	m.logger.Info("Healthy RPC nodes updated",
		zap.Int("healthy_count", len(m.healthy)),
		zap.Int("failed_count", errorsEncountered),
		zap.Any("healthy_nodes_sorted", healthyNodes),
	)
}

// checkHealth verifies if an RPC node is alive and has a working WebSocket connection
func (m *RPCMonitor) checkHealth(node RPCNode) bool {
	client, err := rpchttp.New(node.Address)
	if err != nil {
		m.logger.Warn("Failed to create RPC client", zap.String("url", node.Address), zap.Error(err))
		return false
	}

	// Check status
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		m.logger.Warn("RPC status check failed", zap.String("url", node.Address), zap.Error(err))
		return false
	}

	// Ensure indexing is enabled
	if !status.TxIndexEnabled() {
		m.logger.Warn("Indexing is disabled", zap.String("url", node.Address))
		return false
	}

	// // Check WebSocket connectivity
	if !m.checkWebSocket(node.Address) {
		m.logger.Warn("WebSocket check failed", zap.String("url", node.Address))
		return false
	}

	m.logger.Debug("RPC is healthy", zap.String("url", node.Address))
	return true
}

// JSON-RPC standard request
type jsonRPCRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      int    `json:"id"`
}

// JSON-RPC standard response
type jsonRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// checkWebSocket verifies WebSocket connectivity by sending a JSON-RPC request
func (m *RPCMonitor) checkWebSocket(rpcAddress string) bool {
	wsURL := convertToWebSocket(rpcAddress)
	_, err := url.Parse(wsURL)
	if err != nil {
		m.logger.Warn("Invalid WebSocket URL", zap.String("url", wsURL), zap.Error(err))
		return false
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: false}, // Can be set to `true` for self-signed certs
	}

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		m.logger.Warn("Failed to connect to WebSocket", zap.String("url", wsURL), zap.Error(err))
		return false
	}
	defer conn.Close()

	// Set a short write timeout
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))

	// Send a JSON-RPC status request
	request := jsonRPCRequest{
		Jsonrpc: "2.0",
		Method:  "status",
		ID:      1,
	}
	err = conn.WriteJSON(request)
	if err != nil {
		m.logger.Warn("WebSocket write failed", zap.String("url", wsURL), zap.Error(err))
		return false
	}

	// Set a short read timeout
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Read response
	var response jsonRPCResponse
	err = conn.ReadJSON(&response)
	if err != nil {
		m.logger.Warn("WebSocket did not respond correctly", zap.String("url", wsURL), zap.Error(err))
		return false
	}

	// Check if response contains an error
	if response.Error != nil {
		m.logger.Warn("WebSocket RPC error", zap.String("url", wsURL), zap.String("message", response.Error.Message))
		return false
	}

	m.logger.Info("WebSocket connection successful", zap.String("url", wsURL))
	return true
}

// fetchRPCFromRegistry fetches RPC endpoints from a chain registry JSON
func fetchRPCFromRegistry(url string) ([]RPCNode, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch registry: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned non-200: %d", resp.StatusCode)
	}

	var registry ChainRegistry
	err = json.NewDecoder(resp.Body).Decode(&registry)
	if err != nil {
		return nil, fmt.Errorf("failed to parse registry JSON: %w", err)
	}

	return registry.APIs.RPC, nil
}

// convertToWebSocket converts an HTTP URL to WebSocket (ws:// or wss://)
func convertToWebSocket(httpURL string) string {
	parsed, err := url.Parse(httpURL)
	if err != nil {
		return ""
	}

	if strings.HasPrefix(parsed.Scheme, "https") {
		return "wss://" + parsed.Host + "/websocket"
	}

	return "ws://" + parsed.Host + "/websocket"
}

// StartBackgroundRefresher runs periodic monitoring
func (m *RPCMonitor) StartBackgroundRefresher(interval time.Duration) {
	m.logger.Info("Starting background RPC check")
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		m.fetchAndUpdateHealthyNodes() // Run immediately on startup

		for {
			select {
			case <-ticker.C:
				m.logger.Info("Refreshing RPC list")
				m.fetchAndUpdateHealthyNodes()
				// if err != nil {
				// 	m.logger.Warn("Failed to refresh RPC list", zap.Error(err))
				// }
			case <-m.stopChan:
				m.logger.Info("Stopping RPC monitor refresher")
				return
			}
		}
	}()
}

// StopBackgroundRefresher stops monitoring
func (m *RPCMonitor) StopBackgroundRefresher() {
	close(m.stopChan)
}

// GetHealthyNodes returns the list of healthy RPCs
func (m *RPCMonitor) GetHealthyNodes() []RPCNode {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.healthy
}

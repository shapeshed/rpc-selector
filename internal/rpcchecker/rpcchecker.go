package rpcchecker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	// "strings"
	"sync"
	"time"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/gorilla/websocket"
	// ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"go.uber.org/zap"
)

// RPCNode represents an RPC endpoint.
type RPCNode struct {
	Address  string `json:"address"`
	Provider string `json:"provider,omitempty"`
}

// ChainRegistry represents the chain.json format.
type ChainRegistry struct {
	APIs struct {
		RPC []RPCNode `json:"rpc"`
	} `json:"apis"`
}

// RPCMonitor dynamically monitors healthy RPC endpoints.
type RPCMonitor struct {
	chainRegistryURL string
	customRPCs       []string
	healthy          []RPCNode
	logger           *zap.Logger
	cacheMu          sync.RWMutex
	stopChan         chan struct{}
	// failedNodes maps node addresses to the time until which they are banned.
	failedNodes map[string]time.Time
}

// NewRPCMonitor initializes a new RPC monitor with a registry URL and custom RPCs.
func NewRPCMonitor(chainRegistryURL string, customRPCs []string, logger *zap.Logger) *RPCMonitor {
	return &RPCMonitor{
		chainRegistryURL: chainRegistryURL,
		customRPCs:       customRPCs,
		stopChan:         make(chan struct{}),
		logger:           logger,
		failedNodes:      make(map[string]time.Time),
	}
}

// MarkNodeUnhealthy marks a node as unhealthy for the given duration.
func (m *RPCMonitor) MarkNodeUnhealthy(address string, banDuration time.Duration) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.failedNodes[address] = time.Now().Add(banDuration)
	m.logger.Info("Marking node as unhealthy", zap.String("address", address), zap.Duration("banDuration", banDuration))
}

// GetHealthyNodes returns the healthy RPC nodes, filtering out nodes that are currently banned.
func (m *RPCMonitor) GetHealthyNodes() []RPCNode {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	now := time.Now()
	filtered := []RPCNode{}
	for _, node := range m.healthy {
		if banUntil, exists := m.failedNodes[node.Address]; exists {
			if now.Before(banUntil) {
				continue // Skip this node—it’s still banned.
			}
		}
		filtered = append(filtered, node)
	}
	return filtered
}

// fetchAndUpdateHealthyNodes retrieves RPC endpoints, checks their health, and sorts by response time.
func (m *RPCMonitor) fetchAndUpdateHealthyNodes() error {
	m.logger.Info("Fetching RPC List")

	// Get RPCs from registry if URL is provided.
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

	// Add custom RPCs.
	for _, rpc := range m.customRPCs {
		rpcList = append(rpcList, RPCNode{Address: rpc})
	}

	// Deduplicate.
	rpcMap := make(map[string]RPCNode)
	for _, rpc := range rpcList {
		rpcMap[rpc.Address] = rpc
	}
	uniqueRPCs := make([]RPCNode, 0, len(rpcMap))
	for _, rpc := range rpcMap {
		uniqueRPCs = append(uniqueRPCs, rpc)
	}

	if len(uniqueRPCs) == 0 {
		m.logger.Warn("No RPC endpoints available after fetching")
		return fmt.Errorf("no RPC endpoints available")
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
				// Optionally, mark node as unhealthy.
				m.MarkNodeUnhealthy(n.Address, 2*time.Minute)
				mu.Unlock()
			}
		}(node)
	}

	wg.Wait()

	if len(healthyNodes) == 0 {
		m.logger.Error("All RPC health checks failed", zap.Int("failed_count", errorsEncountered))
		return fmt.Errorf("all RPC health checks failed")
	}

	// Sort healthy nodes by fastest response time (ascending order).
	sort.Slice(healthyNodes, func(i, j int) bool {
		return responseTimes[healthyNodes[i].Address] < responseTimes[healthyNodes[j].Address]
	})

	m.cacheMu.Lock()
	m.healthy = healthyNodes
	m.cacheMu.Unlock()

	m.logger.Info("Healthy RPC nodes updated", zap.Int("healthy_count", len(m.healthy)), zap.Int("failed_count", errorsEncountered))
	return nil
}

// checkHealth verifies if an RPC node is alive, has indexing enabled, and supports WebSocket.
func (m *RPCMonitor) checkHealth(node RPCNode) bool {
	client, err := rpchttp.New(node.Address)
	if err != nil {
		m.logger.Warn("Failed to create RPC client", zap.String("url", node.Address), zap.Error(err))
		return false
	}

	// Check status.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		m.logger.Warn("RPC status check failed", zap.String("url", node.Address), zap.Error(err))
		return false
	}

	if !status.TxIndexEnabled() {
		m.logger.Warn("Indexing is disabled", zap.String("url", node.Address))
		return false
	}

	// Check WebSocket connectivity.
	if !m.checkWebSocket(node.Address) {
		return false
	}

	m.logger.Debug("RPC is healthy", zap.String("url", node.Address))
	return true
}

// checkWebSocket verifies WebSocket connectivity by converting the HTTP URL to a WS URL,
// then performing a simple connection check.
func (m *RPCMonitor) checkWebSocket(rpcAddress string) bool {
	wsURL := convertToWebSocket(rpcAddress)
	// Create a WebSocket dialer.
	dialer := &websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		m.logger.Warn("Failed to establish WebSocket connection", zap.String("url", wsURL), zap.Error(err))
		return false
	}
	conn.Close()
	m.logger.Debug("WebSocket connection successful", zap.String("url", wsURL))
	return true
}

// fetchRPCFromRegistry fetches RPC endpoints from a chain registry JSON.
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

// convertToWebSocket converts an HTTP URL to the appropriate WebSocket URL.
func convertToWebSocket(rpcAddress string) string {
	parsed, err := url.Parse(rpcAddress)
	if err != nil || parsed.Scheme == "" {
		return "ws://" + rpcAddress + "/websocket"
	}
	switch parsed.Scheme {
	case "https":
		return "wss://" + parsed.Host + "/websocket"
	case "http":
		return "ws://" + parsed.Host + "/websocket"
	default:
		return "ws://" + parsed.Host + "/websocket"
	}
}

// StartBackgroundRefresher runs periodic monitoring.
func (m *RPCMonitor) StartBackgroundRefresher(interval time.Duration) {
	m.logger.Info("Starting background RPC check")
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		m.fetchAndUpdateHealthyNodes() // Run immediately on startup.
		for {
			select {
			case <-ticker.C:
				m.fetchAndUpdateHealthyNodes()
			case <-m.stopChan:
				m.logger.Info("Stopping RPC monitor refresher")
				return
			}
		}
	}()
}

// StopBackgroundRefresher stops monitoring.
func (m *RPCMonitor) StopBackgroundRefresher() {
	close(m.stopChan)
}

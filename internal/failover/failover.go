package failover

import (
	"sync"
	"time"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	jsonrpcclient "github.com/cometbft/cometbft/rpc/jsonrpc/client"

	// ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/shapeshed/rpc-selector/internal/rpcchecker"
	"go.uber.org/zap"
)

// FailoverRPCClient wraps RPC requests and automatically handles failover.
type FailoverRPCClient struct {
	monitor        *rpcchecker.RPCMonitor
	mu             sync.Mutex
	logger         *zap.Logger
	rpcClient      *rpchttp.HTTP
	currentAddress string
}

// NewFailoverRPCClient creates a new RPC client with auto-failover and logging.
func NewFailoverRPCClient(monitor *rpcchecker.RPCMonitor, logger *zap.Logger) *FailoverRPCClient {
	client := &FailoverRPCClient{
		monitor: monitor,
		logger:  logger,
	}
	client.selectHealthyNode()
	return client
}

// selectHealthyNode picks a healthy node different from the current one (if possible)
// and creates an RPC client.
func (c *FailoverRPCClient) selectHealthyNode() {
	c.mu.Lock()
	defer c.mu.Unlock()

	healthyNodes := c.monitor.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		c.logger.Error("No healthy RPC nodes available")
		c.rpcClient = nil
		c.currentAddress = ""
		return
	}

	// Try to choose a different node than the current one.
	var newNode *rpcchecker.RPCNode
	for _, node := range healthyNodes {
		if node.Address != c.currentAddress {
			newNode = &node
			break
		}
	}
	if newNode == nil {
		// If all nodes are the same as the current one, fallback to the first node.
		newNode = &healthyNodes[0]
	}

	httpClient, err := jsonrpcclient.DefaultHTTPClient(newNode.Address)
	if err != nil {
		panic(err)
	}
	httpClient.Timeout = 1000 * time.Millisecond

	rpcClient, err := rpchttp.NewWithClient(newNode.Address, httpClient)
	if err != nil {
		c.logger.Warn("Failed to create RPC client", zap.String("url", newNode.Address), zap.Error(err))
		return
	}

	c.currentAddress = newNode.Address
	c.logger.Info("Switching to new RPC node", zap.String("url", newNode.Address))
	c.rpcClient = rpcClient
}

// Failover switches to the next available RPC node.
func (c *FailoverRPCClient) Failover() {
	c.logger.Warn("Failing over to next available RPC node")
	c.selectHealthyNode()
}

// Client returns the current active RPC client.
func (c *FailoverRPCClient) Client() *rpchttp.HTTP {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rpcClient
}

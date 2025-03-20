# RPC Service

This is a POC for a cosmos RPC Service manager.

The service 

- Reads the public chain registry
- Accepts an optional slice of additional URLs
- Checks
  - The RPC `/health` endpoint
  - The RPC has indexing on
  - The RPC is not catching up
  - The Websocket connection is alive
- Sorts healthy nodes by response time
- Bans unhealthy nodes for a certain period of time

Consumers can use it like this, calling `failoverClient.Failover()` within an error to switch the `failoverClient` to another node.

```go
monitor := rpcchecker.NewRPCMonitor(
	"https://raw.githubusercontent.com/cosmos/chain-registry/refs/heads/master/osmosis/chain.json",
	customRPCs,
	logger,
)

monitor.StartBackgroundRefresher(30 * time.Minute)

// Wait for nodes to be refreshed
time.Sleep(5 * time.Second)

// Get healthy nodes
healthyNodes := monitor.GetHealthyNodes()
logger.Info("Healthy RPC Nodes", zap.Any("nodes", healthyNodes))

// Stop background refresher when exiting
defer monitor.StopBackgroundRefresher()

// Initialize Failover Client
rpcClient := failover.NewFailoverRPCClient(monitor, logger)

ctx := context.Background()

status, err := failoverClient.Client().Status(ctx)
if err != nil {
	logger.Warn("Failed to get status", zap.Error(err))
  rpcClient.Failover()
} else {
	logger.Info("Got RPC Status", zap.Any("status", status.SyncInfo))
}

```
## Ideas etc

The cometbft rpc client handles websocket connections as well and does not expose everything so it would be better to create a custom initialiser that does not initialise a websocket connection. This would mean that this library is _only_ concerned with a healthy JSON RPC connection. 

We can set a timeout on the client which would mean if an endpoint is slow to responsd it will switch to another. 

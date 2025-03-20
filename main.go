package main

import (
	"context"
	// "fmt"
	// "sync"
	// "sync/atomic"
	"time"

	"github.com/shapeshed/rpc-selector/internal/failover"
	"github.com/shapeshed/rpc-selector/internal/rpcchecker"
	"go.uber.org/zap"
)

func main() {
	// Create a logger once and pass it through
	cfg := zap.NewProductionConfig()
	logger, _ := cfg.Build()
	defer logger.Sync() // Flush logs before exit

	customRPCs := []string{"https://rpc-osmosis.margined.io"}

	// Initialize RPC Monitor with the same logger
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
	failoverClient := failover.NewFailoverRPCClient(monitor, logger)

	// **Test Rate Limit Scenario**
	ctx := context.Background()
	// Directly call Status()
	status, err := failoverClient.Client().Status(ctx)
	if err != nil {
		logger.Warn("Failed to get status", zap.Error(err))
	} else {
		logger.Info("Got RPC Status", zap.Any("status", status.SyncInfo))
	}

	successCount := 0
	failoverCount := 0

	// Loop until failover happens at least once
	for i := 0; i < 5000; i++ {
		start := time.Now()
		status, err := failoverClient.Client().Status(ctx)
		elapsed := time.Since(start)

		logger.Info("RPC", zap.String("remote", failoverClient.Client().Remote()))

		if err != nil {
			logger.Error("RPC Request Failed",
				zap.Int("iterator", i+1),
				zap.Duration("elapsed", elapsed),
				zap.Error(err),
			)
			failoverClient.Failover()
			failoverCount++
			// Short sleep to avoid hammering
			// time.Sleep(10 * time.Millisecond)
		} else {
			logger.Error("RPC Request Succeed",
				zap.Int("iterator", i+1),
				zap.Duration("elapsed", elapsed),
				zap.Int64("block height", status.SyncInfo.LatestBlockHeight),
			)
			successCount++
		}

		// Exit after a failover occurs
		if failoverCount > 3 {
			break
		}
	}
	logger.Info("test results", zap.Int("success count", successCount), zap.Int("failoverCount", failoverCount))

	// var successCount int32
	// var failoverCount int32

	// numGoroutines := 10 // Number of concurrent workers.
	// iterationsPerWorker := 1000

	// var wg sync.WaitGroup

	// logger.Info("Starting concurrent RPC stress test to trigger failover")

	// for i := 0; i < numGoroutines; i++ {
	// 	wg.Add(1)
	// 	go func(workerID int) {
	// 		defer wg.Done()
	// 		for j := 0; j < iterationsPerWorker; j++ {
	// 			start := time.Now()
	// 			status, err := failoverClient.Client().Status(ctx)
	// 			elapsed := time.Since(start)

	// 			logger.Info("RPC Request", zap.String("worker", fmt.Sprintf("%d", workerID)),
	// 				zap.Int("iteration", j),
	// 				zap.String("remote", failoverClient.Client().Remote()),
	// 			)

	// 			if err != nil {
	// 				logger.Error("RPC Request Failed",
	// 					zap.Int("worker", workerID),
	// 					zap.Int("iteration", j),
	// 					zap.Duration("elapsed", elapsed),
	// 					zap.Error(err),
	// 				)
	// 				failoverClient.Failover()
	// 				atomic.AddInt32(&failoverCount, 1)
	// 				// Short sleep to avoid hammering the same node immediately.
	// 				time.Sleep(10 * time.Millisecond)
	// 			} else {
	// 				logger.Info("RPC Request Succeeded",
	// 					zap.Int("worker", workerID),
	// 					zap.Int("iteration", j),
	// 					zap.Duration("elapsed", elapsed),
	// 					zap.Int64("block height", status.SyncInfo.LatestBlockHeight),
	// 				)
	// 				atomic.AddInt32(&successCount, 1)
	// 			}
	// 		}
	// 	}(i)
	// }

	// wg.Wait()

	// logger.Info("Test results", zap.Int32("success count", successCount), zap.Int32("failover count", failoverCount))
	// logger.Info("Test complete.")
}

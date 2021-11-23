package main

import (
	"os"
	"os/signal"
	"time"

	consul "github.com/letsencrypt/attache/src/consul/client"
	lock "github.com/letsencrypt/attache/src/consul/lock"
	rediscli "github.com/letsencrypt/attache/src/redis/cli"
	redisclient "github.com/letsencrypt/attache/src/redis/client"
	logger "github.com/sirupsen/logrus"
)

func setLogLevel(level string) {
	parsedLevel, err := logger.ParseLevel(level)
	if err != nil {
		logger.Fatalf("initializing: %s is not a valid log-level: %s", level, err)
	}
	logger.SetLevel(parsedLevel)
}

func main() {
	start := time.Now()
	conf := ParseFlags()
	ValidateConfig(conf)
	setLogLevel(conf.LogLevel)
	logger.Infof("starting %s", os.Args[0])

	logger.Info("consul: setting up a consul client")
	consulClient, err := conf.ConsulOpts.MakeConsulClient()
	if err != nil {
		logger.Fatalf("consul: %s", err)
	}

	redisClient := redisclient.New(conf.RedisNodeAddr, "")

	var nodesInDest []string
	var nodesInAwait []string

	var attemptCount int
	var ticks = time.Tick(conf.AttemptInterval)
	for range ticks {
		attemptCount++

		nodeIsNew, err := redisClient.StateNewCheck()
		if err != nil {
			logger.Fatalf("redis: %s", err)
		}

		if !nodeIsNew {
			logger.Info("redis: this node has already joined a cluster")
			break
		}

		lock := lock.New(consulClient, conf.LockPath, "10s")
		err = lock.CreateSession()
		if err != nil {
			logger.Fatalf("consul: %s", err)
		}

		nodeHasLock, err := lock.Acquire()
		if err != nil {
			logger.Fatalf("consul: %s", err)
		}

		// If forced to exit early, cleanup our session.
		catchSignals := make(chan os.Signal, 1)
		signal.Notify(catchSignals, os.Interrupt)
		go func() {
			<-catchSignals
			logger.Error("consul: operation interrupted, cleaning up session and exiting")

			lock.Cleanup()
			os.Exit(1)
		}()

		if nodeHasLock {
			logger.Info("consul: acquired leader lock")

			// Spin-off a goroutine to periodically renew our leader lock until
			// our work is complete.
			doneChan := make(chan struct{})
			go lock.Renew(doneChan)

			cleanup := func() {
				// Stop renewing the lock session.
				close(doneChan)
				lock.Cleanup()

			}

			// Check the Consul service catalog for an existing Redis Cluster
			// that we can join. We're limiting the scope of our search to nodes
			// in the destService Consul service that Consul considers healthy.
			destService := consul.New(consulClient, conf.DestServiceName, "", true)
			nodesInDest, err = destService.GetNodeAddresses()
			if err != nil {
				cleanup()
				logger.Fatal(err)
			}
			logger.Infof("consul: found nodes %q in service %q", nodesInDest, conf.DestServiceName)

			// If 0 existing nodes can be found with this criteria, we know that
			// we need to initialize a new cluster.
			if len(nodesInDest) == 0 {
				// Check the Consul service catalog for other nodes that are
				// waiting to form a cluster. We're limiting the scope of our
				// search to nodes in the awaitService Consul service that
				// Consul considers healthy.
				awaitService := consul.New(consulClient, conf.AwaitServiceName, "", true)
				nodesInAwait, err = awaitService.GetNodeAddresses()
				if err != nil {
					cleanup()
					logger.Fatalf("consul: %s", err)
				}
				logger.Infof("consul: found nodes %q in service %q", nodesInAwait, conf.AwaitServiceName)

				// We should only attempt to initialize a new cluster if all of
				// the nodes that we expect in said cluster have finished
				// starting up and reside in the awaitService Consul service.
				nodesMissing := (conf.RedisPrimaryCount + conf.RedisReplicaCount) - len(nodesInAwait)
				if nodesMissing <= 0 {
					replicasPerPrimary := conf.RedisReplicaCount / conf.RedisPrimaryCount

					var nodesToCluster []string
					if replicasPerPrimary == 0 {
						// This handles a special case for clusters that are
						// started with less than enough replicas to give at
						// lease one to each primary. Once the first primary
						// only cluster is started and the lock is released our
						// remaining replica nodes will be able to add
						// themselves to the newly created cluster.
						nodesToCluster = nodesInAwait[:conf.RedisPrimaryCount]
					} else {
						nodesToCluster = nodesInAwait
					}

					logger.Infof("attempting to create a new cluster with nodes %q", nodesToCluster)
					err := rediscli.CreateCluster(nodesToCluster, replicasPerPrimary)
					if err != nil {
						cleanup()
						logger.Fatalf("redis: %s", err)
					}
					logger.Info("redis: suceeded")
					cleanup()
					break
				} else {
					logger.Infof("still waiting for %d nodes to startup, releasing lock", nodesMissing)
					cleanup()
					continue
				}
			}

			logger.Infof("redis: gathering info from the cluster that %q belongs to", nodesInDest[0])
			clusterClient := redisclient.New(nodesInDest[0], "")
			primaryNodesInCluster, err := clusterClient.GetPrimaryNodes()
			if err != nil {
				cleanup()
				logger.Fatalf("redis: %s", err)
			}
			replicaNodesInCluster, err := clusterClient.GetReplicaNodes()
			if err != nil {
				cleanup()
				logger.Fatalf("redis: %s", err)
			}

			if len(primaryNodesInCluster) < conf.RedisPrimaryCount {
				// The current cluster has less than `shardPrimaryCount` shard
				// primary nodes. This node should be added as a new primary and
				// the existing cluster shardslots should be rebalanced.
				logger.Infof("redis: node %q should be added as a new shard primary", conf.RedisNodeAddr)
				logger.Infof("redis: attempting to join %q to the cluster that %q belongs to", conf.RedisNodeAddr, nodesInDest[0])

				err := rediscli.AddNewShardPrimary(conf.RedisNodeAddr, nodesInDest[0])
				if err != nil {
					cleanup()
					logger.Fatalf("redis: %s", err)
				}
				logger.Info("redis: suceeded")
				cleanup()
				break

			} else if len(replicaNodesInCluster) < conf.RedisReplicaCount {
				// All `shardPrimaryCount` shard primary nodes exist in the
				// current cluster. This node should be added as a replica to
				// the primary node with the least number of replicas.
				logger.Infof("redis: node %q should be added as a new shard replica", conf.RedisNodeAddr)
				logger.Infof("redis: attempting to join %q to the cluster that %q belongs to", conf.RedisNodeAddr, nodesInDest[0])

				err := rediscli.AddNewShardReplica(conf.RedisNodeAddr, nodesInDest[0])
				if err != nil {
					cleanup()
					logger.Fatalf("redis: %s", err)
				}
				logger.Info("redis: suceeded")
				cleanup()
				break
			}
		} else {
			if attemptCount == conf.AttemptLimit {
				logger.Fatal("failed to join or initialize a cluster during the time permitted")
			}
			logger.Info("another node currently has the lock")
			logger.Infof("continuing to wait, %d attempts remain", (conf.AttemptLimit - attemptCount))
		}
	}

	// TODO: Remove once https://github.com/hashicorp/nomad/issues/10058 has
	// been solved. Nomad Post-Start tasks need to stay healthy for at least 10s
	// after the Main Tasks are marked healthy. Attache is a Post-Start Task, so
	// just sleeping for a really long time will ensure that we don't
	// accidentally trigger this behavior during a deployment.
	duration := time.Since(start)
	if duration < time.Minute*10 {
		timeToWait := time.Minute*10 - duration
		logger.Infof("waiting %s to exit", timeToWait.String())
		time.Sleep(timeToWait)
	}
	logger.Info("exiting...")
}
package distributedrediscluster

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1alpha1 "github.com/ucloud/redis-cluster-operator/pkg/apis/redis/v1alpha1"
	"github.com/ucloud/redis-cluster-operator/pkg/config"
	"github.com/ucloud/redis-cluster-operator/pkg/redisutil"
	"github.com/ucloud/redis-cluster-operator/pkg/utils"
)

var (
	defaultLabels = map[string]string{
		redisv1alpha1.LabelManagedByKey: redisv1alpha1.OperatorName,
	}
)

const passwordKey = "password"

func getLabels(cluster *redisv1alpha1.DistributedRedisCluster) map[string]string {
	dynLabels := map[string]string{
		redisv1alpha1.LabelClusterName: cluster.Name,
	}
	return utils.MergeLabels(defaultLabels, dynLabels, cluster.Labels)
}

func getClusterPassword(client client.Client, cluster *redisv1alpha1.DistributedRedisCluster) (string, error) {
	if cluster.Spec.PasswordSecret == nil {
		return "", nil
	}
	secret := &corev1.Secret{}
	err := client.Get(context.TODO(), types.NamespacedName{
		Name:      cluster.Spec.PasswordSecret.Name,
		Namespace: cluster.Namespace,
	}, secret)
	if err != nil {
		return "", err
	}
	return string(secret.Data[passwordKey]), nil
}

// newRedisAdmin builds and returns new redis.Admin from the list of pods
func newRedisAdmin(pods []*corev1.Pod, password string, cfg *config.Redis) (redisutil.IAdmin, error) {
	nodesAddrs := []string{}
	for _, pod := range pods {
		redisPort := redisutil.DefaultRedisPort
		for _, container := range pod.Spec.Containers {
			if container.Name == "redis" {
				for _, port := range container.Ports {
					if port.Name == "client" {
						redisPort = fmt.Sprintf("%d", port.ContainerPort)
					}
				}
			}
		}
		log.V(4).Info("append redis admin addr", "addr", pod.Status.PodIP, "port", redisPort)
		nodesAddrs = append(nodesAddrs, net.JoinHostPort(pod.Status.PodIP, redisPort))
	}
	adminConfig := redisutil.AdminOptions{
		ConnectionTimeout:  time.Duration(cfg.DialTimeout) * time.Millisecond,
		RenameCommandsFile: cfg.GetRenameCommandsFile(),
		Password:           password,
	}

	return redisutil.NewAdmin(nodesAddrs, &adminConfig), nil
}

func makeCluster(cluster *redisv1alpha1.DistributedRedisCluster, clusterInfos *redisutil.ClusterInfos) error {
	logger := log.WithValues("namespace", cluster.Namespace, "name", cluster.Name)
	mastersCount := int(cluster.Spec.MasterSize)
	clusterReplicas := cluster.Spec.ClusterReplicas
	expectPodNum := mastersCount * int(clusterReplicas+1)

	if len(clusterInfos.Infos) != expectPodNum {
		return fmt.Errorf("node num different from expectation")
	}

	logger.Info(fmt.Sprintf(">>> Performing hash slots allocation on %d nodes...", expectPodNum))

	masterNodes := make(redisutil.Nodes, mastersCount)
	i := 0
	k := 0
	slotsPerNode := redisutil.DefaultHashMaxSlots / mastersCount
	first := 0
	cursor := 0
	for _, nodeInfo := range clusterInfos.Infos {
		if i < mastersCount {
			nodeInfo.Node.Role = redisutil.RedisMasterRole
			last := cursor + slotsPerNode - 1
			if last > redisutil.DefaultHashMaxSlots+1 || i == mastersCount-1 {
				last = redisutil.DefaultHashMaxSlots
			}
			logger.Info(fmt.Sprintf("Master[%d] -> Slots %d - %d", i, first, last))
			nodeInfo.Node.Slots = redisutil.BuildSlotSlice(redisutil.Slot(first), redisutil.Slot(last))
			first = last + 1
			cursor += slotsPerNode
			masterNodes[i] = nodeInfo.Node
		} else {
			if k > mastersCount {
				k = 0
			}
			logger.Info(fmt.Sprintf("Adding replica %s:%s to %s:%s", nodeInfo.Node.IP, nodeInfo.Node.Port,
				masterNodes[k].IP, masterNodes[k].Port))
			nodeInfo.Node.Role = redisutil.RedisSlaveRole
			nodeInfo.Node.MasterReferent = masterNodes[k].ID
			k++
		}
		i++
	}

	log.Info(clusterInfos.GetNodes().String())

	return nil
}

func newRedisCluster(infos *redisutil.ClusterInfos, cluster *redisv1alpha1.DistributedRedisCluster) (*redisutil.Cluster, redisutil.Nodes, error) {
	// now we can trigger the rebalance
	nodes := infos.GetNodes()

	// build redis cluster vision
	rCluster := &redisutil.Cluster{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
		Nodes:     make(map[string]*redisutil.Node),
	}

	for _, node := range nodes {
		rCluster.Nodes[node.ID] = node
	}

	for _, node := range cluster.Status.Nodes {
		if rNode, ok := rCluster.Nodes[node.ID]; ok {
			rNode.PodName = node.PodName
			rNode.NodeName = node.NodeName
		}
	}

	return rCluster, nodes, nil
}

func clusterPods(pods []corev1.Pod) []*corev1.Pod {
	var podSlice []*corev1.Pod
	for _, pod := range pods {
		podPointer := pod
		podSlice = append(podSlice, &podPointer)
	}
	return podSlice
}

func needClusterOperation(cluster *redisv1alpha1.DistributedRedisCluster, reqLogger logr.Logger) bool {
	if compareIntValue("NumberOfMaster", &cluster.Status.NumberOfMaster, &cluster.Spec.MasterSize) {
		reqLogger.V(4).Info("needClusterOperation---NumberOfMaster")
		return true
	}

	if compareIntValue("MinReplicationFactor", &cluster.Status.MinReplicationFactor, &cluster.Spec.ClusterReplicas) {
		reqLogger.V(4).Info("needClusterOperation---MinReplicationFactor")
		return true
	}

	if compareIntValue("MaxReplicationFactor", &cluster.Status.MaxReplicationFactor, &cluster.Spec.ClusterReplicas) {
		reqLogger.V(4).Info("needClusterOperation---MaxReplicationFactor")
		return true
	}

	return false
}

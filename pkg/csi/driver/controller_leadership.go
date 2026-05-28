package driver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

const serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

type ControllerLeadership struct {
	enabled        bool
	isLeader       atomic.Bool
	identity       string
	leaseName      string
	leaseNamespace string
}

func NewControllerLeadership(ctx context.Context, cfg config.CSIPluginConfig) (*ControllerLeadership, error) {
	enabled, ok := cfg.GetBool(config.ControllerLeaderElectionEnabledVar)
	if !ok || !enabled {
		return &ControllerLeadership{}, nil
	}

	leaseName, ok := cfg.GetString(config.ControllerLeaderElectionLeaseNameVar)
	if !ok || strings.TrimSpace(leaseName) == "" {
		return nil, fmt.Errorf("%s must be set when controller leader election is enabled", config.ControllerLeaderElectionLeaseNameVar)
	}

	leaseNamespace, err := controllerLeaseNamespace(cfg)
	if err != nil {
		return nil, err
	}

	identity, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to determine controller leader election identity: %w", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize in-cluster config for controller leader election: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize kubernetes client for controller leader election: %w", err)
	}

	leadership := &ControllerLeadership{
		enabled:        true,
		identity:       identity,
		leaseName:      leaseName,
		leaseNamespace: leaseNamespace,
	}

	leaseDuration := configDuration(cfg, config.ControllerLeaderElectionLeaseDurationVar, 45)
	renewDeadline := configDuration(cfg, config.ControllerLeaderElectionRenewDeadlineVar, 30)
	retryPeriod := configDuration(cfg, config.ControllerLeaderElectionRetryPeriodVar, 10)

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: leaseNamespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	leaderElector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Name:            DefaultDriverName + "-controller",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {
				leadership.isLeader.Store(true)
				klog.InfoS("Controller leader election acquired",
					"leaseName", leaseName,
					"leaseNamespace", leaseNamespace,
					"identity", identity)
			},
			OnStoppedLeading: func() {
				leadership.isLeader.Store(false)
				klog.InfoS("Controller leader election lost",
					"leaseName", leaseName,
					"leaseNamespace", leaseNamespace,
					"identity", identity)
			},
			OnNewLeader: func(current string) {
				klog.V(2).InfoS("Observed controller leader election holder",
					"leaseName", leaseName,
					"leaseNamespace", leaseNamespace,
					"leaderIdentity", current)
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize controller leader election: %w", err)
	}

	go runControllerLeaderElectionLoop(ctx, retryPeriod, leaderElector.Run)

	return leadership, nil
}

func (l *ControllerLeadership) Enabled() bool {
	return l != nil && l.enabled
}

func (l *ControllerLeadership) IsLeader() bool {
	return l == nil || !l.enabled || l.isLeader.Load()
}

func (l *ControllerLeadership) UnavailableMessage() string {
	if l == nil || !l.enabled {
		return ""
	}
	return fmt.Sprintf(
		"controller replica is not leader for lease %s/%s; retry later",
		l.leaseNamespace,
		l.leaseName,
	)
}

func controllerLeaseNamespace(cfg config.CSIPluginConfig) (string, error) {
	if namespace, ok := cfg.GetString(config.ControllerLeaderElectionLeaseNamespaceVar); ok && strings.TrimSpace(namespace) != "" {
		return strings.TrimSpace(namespace), nil
	}

	if raw, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		namespace := strings.TrimSpace(string(raw))
		if namespace != "" {
			return namespace, nil
		}
	}

	return "", fmt.Errorf("%s must be set when controller leader election is enabled", config.ControllerLeaderElectionLeaseNamespaceVar)
}

func configDuration(cfg config.CSIPluginConfig, key string, fallback int) time.Duration {
	value, ok := cfg.GetInt(key)
	if !ok || value <= 0 {
		value = fallback
	}
	return time.Duration(value) * time.Second
}

func runControllerLeaderElectionLoop(ctx context.Context, retryPeriod time.Duration, run func(context.Context)) {
	if retryPeriod <= 0 {
		retryPeriod = time.Second
	}

	for {
		if ctx.Err() != nil {
			return
		}

		run(ctx)
		if ctx.Err() != nil {
			return
		}

		klog.InfoS("Controller leader election runner exited; restarting",
			"retryPeriod", retryPeriod)

		timer := time.NewTimer(retryPeriod)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

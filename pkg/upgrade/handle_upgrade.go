package upgrade

import (
	"context"
	"time"

	upgradeapiv1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	upgradectlv1 "github.com/rancher/system-upgrade-controller/pkg/generated/controllers/upgrade.cattle.io/v1"
	upgradejob "github.com/rancher/system-upgrade-controller/pkg/upgrade/job"
	upgradeplan "github.com/rancher/system-upgrade-controller/pkg/upgrade/plan"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
)

func (ctl *Controller) handlePlans(ctx context.Context) error {
	jobs := ctl.batchFactory.Batch().V1().Job()
	nodes := ctl.coreFactory.Core().V1().Node()
	plans := ctl.upgradeFactory.Upgrade().V1().Plan()
	secrets := ctl.coreFactory.Core().V1().Secret()
	secretsCache := secrets.Cache()

	// process plan events, mutating status accordingly
	upgradectlv1.RegisterPlanStatusHandler(ctx, plans, "", ctl.Name,
		func(obj *upgradeapiv1.Plan, status upgradeapiv1.PlanStatus) (upgradeapiv1.PlanStatus, error) {
			logrus.Debugf("PLAN STATUS HANDLER: plan=%s/%s@%s, status=%+v", obj.Namespace, obj.Name, obj.ResourceVersion, status)
			resolved := upgradeapiv1.PlanLatestResolved
			resolved.CreateUnknownIfNotExists(obj)
			if obj.Spec.Version == "" && obj.Spec.Channel == "" {
				resolved.SetError(obj, "Error", upgradeapiv1.ErrPlanUnresolvable)
				return upgradeplan.DigestStatus(obj, secretsCache)
			}
			if obj.Spec.Version != "" {
				resolved.False(obj)
				resolved.SetError(obj, "Version", nil)
				obj.Status.LatestVersion = upgradeplan.MungeVersion(obj.Spec.Version)
				return upgradeplan.DigestStatus(obj, secretsCache)
			}
			if resolved.IsTrue(obj) {
				if lastUpdated, err := time.Parse(time.RFC3339, resolved.GetLastUpdated(obj)); err == nil {
					if interval := time.Now().Sub(lastUpdated); interval < upgradeplan.PollingInterval {
						plans.EnqueueAfter(obj.Namespace, obj.Name, upgradeplan.PollingInterval-interval)
						return status, nil
					}
				}
			}
			latest, err := upgradeplan.ResolveChannel(ctx, obj.Spec.Channel, obj.Status.LatestVersion, ctl.clusterID)
			if err != nil {
				return status, err
			}
			resolved.False(obj)
			resolved.SetError(obj, "Channel", nil)
			obj.Status.LatestVersion = upgradeplan.MungeVersion(latest)
			return upgradeplan.DigestStatus(obj, secretsCache)
		},
	)

	// process plan events by creating jobs to apply the plan
	upgradectlv1.RegisterPlanGeneratingHandler(ctx, plans, ctl.apply.WithCacheTypes(jobs, nodes, secrets).WithNoDelete(), "", ctl.Name,
		func(obj *upgradeapiv1.Plan, status upgradeapiv1.PlanStatus) (objects []runtime.Object, _ upgradeapiv1.PlanStatus, _ error) {
			logrus.Debugf("PLAN GENERATING HANDLER: plan=%s/%s@%s, status=%+v", obj.Namespace, obj.Name, obj.ResourceVersion, status)
			concurrentNodeNames, err := upgradeplan.SelectConcurrentNodeNames(obj, nodes.Cache())
			if err != nil {
				return objects, status, err
			}
			logrus.Debugf("concurrentNodeNames = %q", concurrentNodeNames)
			for _, nodeName := range concurrentNodeNames {
				objects = append(objects, upgradejob.New(obj, nodeName, ctl.Name))
			}
			obj.Status.Applying = concurrentNodeNames
			return objects, obj.Status, nil
		},
		&generic.GeneratingHandlerOptions{
			AllowClusterScoped: true,
		},
	)

	return nil
}

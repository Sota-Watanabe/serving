/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metric

import (
	"context"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/autoscaler"

	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	listers "knative.dev/serving/pkg/client/listers/autoscaling/v1alpha1"
	rbase "knative.dev/serving/pkg/reconciler"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
)

const reconcilerName = "Metrics"

// reconciler implements controller.Reconciler for Metric resources.
type reconciler struct {
	*rbase.Base
	collector    autoscaler.Collector
	metricLister listers.MetricLister
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two.
func (r *reconciler) Reconcile(ctx context.Context, key string) error {
	logger := logging.FromContext(ctx)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Errorw("Invalid resource key", zap.Error(err))
		return nil
	}

	original, err := r.metricLister.Metrics(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		// The metric object is gone, so delete the collection.
		return r.collector.Delete(namespace, name)
	} else if err != nil {
		return errors.Wrap(err, "failed to fetch metric "+key)
	}

	// Don't mess with informer's copy.
	metric := original.DeepCopy()
	metric.SetDefaults(ctx)
	metric.Status.InitializeConditions()

	if err = r.reconcileCollection(ctx, metric); err != nil {
		logger.Errorw("Error reconciling metric collection", zap.Error(err))
		r.Recorder.Event(metric, corev1.EventTypeWarning, "InternalError", err.Error())
	} else {
		metric.Status.MarkMetricReady()
	}

	if !equality.Semantic.DeepEqual(original.Status, metric.Status) {
		// Change of status, need to update the object.
		if uErr := r.updateStatus(metric); uErr != nil {
			logger.Warnw("Failed to update metric status", zap.Error(uErr))
			r.Recorder.Eventf(metric, corev1.EventTypeWarning, "UpdateFailed",
				"Failed to update metric status: %v", uErr)
			return uErr
		}
		r.Recorder.Eventf(metric, corev1.EventTypeNormal, "Updated", "Successfully updated metric status %s", key)
	}
	return err
}

func (r *reconciler) reconcileCollection(ctx context.Context, metric *v1alpha1.Metric) error {
	err := r.collector.CreateOrUpdate(metric)
	if err != nil {
		// If create or update failes, we won't be able to collect at all.
		metric.Status.MarkMetricFailed("CollectionFailed", "Failed to reconcile metric collection")
		return errors.Wrap(err, "failed to initiate or update scraping")
	}
	return nil
}

func (r *reconciler) updateStatus(m *v1alpha1.Metric) error {
	ex, err := r.metricLister.Metrics(m.Namespace).Get(m.Name)
	if err != nil {
		// If something deleted metric while we were reconciling ¯\(°_o)/¯.
		return err
	}
	if equality.Semantic.DeepEqual(ex.Status, m.Status) {
		// no-op
		return nil
	}
	ex = ex.DeepCopy()
	ex.Status = m.Status
	_, err = r.ServingClientSet.AutoscalingV1alpha1().Metrics(ex.Namespace).UpdateStatus(ex)
	return err
}
